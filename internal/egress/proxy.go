package egress

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/viktorwelbers/paddock/internal/audit"
	"github.com/viktorwelbers/paddock/internal/policy"
)

// Session is the minimal view the proxy needs of an authenticated session.
type Session struct {
	ID    string
	User  string
	Agent string
}

// SessionAuth resolves a proxy credential (the session token) to a session.
// It returns an error for unknown/expired tokens.
type SessionAuth func(token string) (Session, error)

// PolicyFunc decouples the proxy from the OPA engine for testing.
type PolicyFunc func(ctx context.Context, in policy.Input) (policy.Decision, error)

// Resolver is the DNS surface the proxy uses; injectable for tests.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// Proxy is the governed egress HTTP proxy. It handles CONNECT (the common
// case: TLS tunnels for https registries and git) and, when enabled, plain
// absolute-URI http proxying.
type Proxy struct {
	Auth        SessionAuth
	Policy      PolicyFunc
	Audit       *audit.Store
	Allowlist   *Allowlist
	Resolver    Resolver      // nil = net.DefaultResolver
	DialTimeout time.Duration // default 10s
	IdleTimeout time.Duration // default 5m; tunnel torn down after this idle

	// allowLoopback relaxes the rebinding filter for loopback upstreams.
	// Only tests set it (they dial httptest servers on 127.0.0.1).
	allowLoopback bool
}

func (p *Proxy) resolver() Resolver {
	if p.Resolver != nil {
		return p.Resolver
	}
	return net.DefaultResolver
}

func (p *Proxy) dialTimeout() time.Duration {
	if p.DialTimeout > 0 {
		return p.DialTimeout
	}
	return 10 * time.Second
}

func (p *Proxy) idleTimeout() time.Duration {
	if p.IdleTimeout > 0 {
		return p.IdleTimeout
	}
	return 5 * time.Minute
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.handleConnect(w, r)
		return
	}
	// Plain HTTP proxying: only absolute-URI requests, only when enabled.
	if p.Allowlist.PlainHTTP && r.URL.IsAbs() {
		p.handlePlainHTTP(w, r)
		return
	}
	http.Error(w, "this proxy only serves CONNECT", http.StatusMethodNotAllowed)
}

// authenticate reads the session token from Proxy-Authorization (Basic
// password or Bearer). Returns the session, or ok=false having already
// audited+responded.
func (p *Proxy) authenticate(w http.ResponseWriter, r *http.Request, host string, port int) (Session, bool) {
	token := proxyToken(r.Header.Get("Proxy-Authorization"))
	if token == "" {
		p.denyUnauthenticated(w, host, port)
		return Session{}, false
	}
	sess, err := p.Auth(token)
	if err != nil {
		p.denyUnauthenticated(w, host, port)
		return Session{}, false
	}
	return sess, true
}

// decide runs allowlist → rebinding filter → OPA. On allow it returns the
// vetted upstream address (host resolved to a safe IP) to dial. On deny it
// has already audited and written the response.
func (p *Proxy) decide(w http.ResponseWriter, r *http.Request, sess Session, host string, port int) (netip.Addr, []string, bool) {
	// Strip credentials before anything downstream can log/forward them.
	r.Header.Del("Proxy-Authorization")

	groups := p.Allowlist.MatchGroups(host)
	if len(groups) == 0 {
		reason := "not_in_allowlist"
		if _, err := netip.ParseAddr(canonicalHost(host)); err == nil {
			reason = "ip_literal"
		}
		p.denyForbidden(w, sess, host, port, reason)
		return netip.Addr{}, nil, false
	}
	if !p.Allowlist.PortAllowed(port) {
		p.denyForbidden(w, sess, host, port, "port_not_allowed")
		return netip.Addr{}, nil, false
	}

	dec, err := p.Policy(r.Context(), policy.Input{
		Kind: "egress", User: sess.User, Session: sess.ID, Agent: sess.Agent,
		Host: host, Port: port, Groups: groups,
	})
	if err != nil {
		// Fail closed.
		p.denyForbidden(w, sess, host, port, "policy_error")
		return netip.Addr{}, nil, false
	}
	if !dec.Allow {
		p.denyForbidden(w, sess, host, port, "policy_denied: "+strings.Join(dec.Reasons, "; "))
		return netip.Addr{}, nil, false
	}

	addr, err := p.resolveSafe(r.Context(), host)
	if err != nil {
		reason := "resolve_failed"
		if errors.Is(err, errPrivateAddr) {
			reason = "private_address"
		}
		p.denyForbidden(w, sess, host, port, reason)
		return netip.Addr{}, nil, false
	}
	return addr, groups, true
}

func (p *Proxy) handleConnect(w http.ResponseWriter, r *http.Request) {
	host, port, err := splitHostPort(r.Host)
	if err != nil {
		http.Error(w, "bad CONNECT target", http.StatusBadRequest)
		return
	}
	sess, ok := p.authenticate(w, r, host, port)
	if !ok {
		return
	}
	addr, groups, ok := p.decide(w, r, sess, host, port)
	if !ok {
		return
	}

	// Dial the vetted IP (not the hostname) so no second, unchecked
	// resolution can happen between decision and connection.
	upstream, err := (&net.Dialer{Timeout: p.dialTimeout()}).DialContext(
		r.Context(), "tcp", net.JoinHostPort(addr.String(), strconv.Itoa(port)))
	if err != nil {
		p.denyForbidden(w, sess, host, port, "dial_failed")
		return
	}
	defer upstream.Close()

	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "proxy requires a hijackable connection", http.StatusInternalServerError)
		return
	}
	client, bufrw, err := hj.Hijack()
	if err != nil {
		return
	}
	defer client.Close()

	if _, err := bufrw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	if err := bufrw.Flush(); err != nil {
		return
	}
	// The tunnel manages its own idle deadlines from here.
	_ = client.SetDeadline(time.Time{})

	_ = p.Audit.Append(audit.Event{
		SessionID: sess.ID, Actor: sess.User, Kind: audit.KindEgressAllowed,
		Payload: map[string]any{"host": host, "port": port, "groups": groups, "scheme": "connect"},
	})

	sent, received := p.tunnel(client, bufrw, upstream)

	_ = p.Audit.Append(audit.Event{
		SessionID: sess.ID, Actor: sess.User, Kind: audit.KindEgressClosed,
		Payload: map[string]any{
			"host": host, "port": port,
			"bytes_sent": sent, "bytes_received": received,
		},
	})
}

// tunnel splices the hijacked client and the upstream, returning bytes sent
// (client→upstream) and received (upstream→client). It reads the client
// side from bufrw.Reader — an eager client may have already buffered the TLS
// ClientHello there.
func (p *Proxy) tunnel(client net.Conn, bufrw *bufio.ReadWriter, upstream net.Conn) (sent, received int64) {
	idle := p.idleTimeout()
	var sentN, recvN atomic.Int64
	done := make(chan struct{}, 2)

	go func() {
		n, _ := copyIdle(upstream, bufrw.Reader, idle, client, upstream)
		sentN.Store(n)
		if c, ok := upstream.(interface{ CloseWrite() error }); ok {
			_ = c.CloseWrite()
		}
		done <- struct{}{}
	}()
	go func() {
		n, _ := copyIdle(client, upstream, idle, client, upstream)
		recvN.Store(n)
		if c, ok := client.(interface{ CloseWrite() error }); ok {
			_ = c.CloseWrite()
		}
		done <- struct{}{}
	}()
	<-done
	<-done
	return sentN.Load(), recvN.Load()
}

// copyIdle copies src→dst, resetting an idle deadline on refresh conns before
// each read so a stalled tunnel is torn down instead of leaking a goroutine.
func copyIdle(dst io.Writer, src io.Reader, idle time.Duration, refresh ...net.Conn) (int64, error) {
	buf := make([]byte, 32*1024)
	var total int64
	for {
		deadline := time.Now().Add(idle)
		for _, c := range refresh {
			_ = c.SetDeadline(deadline)
		}
		nr, er := src.Read(buf)
		if nr > 0 {
			nw, ew := dst.Write(buf[:nr])
			total += int64(nw)
			if ew != nil {
				return total, ew
			}
			if nw < nr {
				return total, io.ErrShortWrite
			}
		}
		if er != nil {
			if er == io.EOF {
				return total, nil
			}
			return total, er
		}
	}
}

func (p *Proxy) handlePlainHTTP(w http.ResponseWriter, r *http.Request) {
	host, port, err := splitHostPort(r.Host)
	if err != nil {
		host = r.URL.Hostname()
		port = 80
		if r.URL.Port() != "" {
			port, _ = strconv.Atoi(r.URL.Port())
		}
	}
	sess, ok := p.authenticate(w, r, host, port)
	if !ok {
		return
	}
	addr, groups, ok := p.decide(w, r, sess, host, port)
	if !ok {
		return
	}

	dialer := &net.Dialer{Timeout: p.dialTimeout()}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, net.JoinHostPort(addr.String(), strconv.Itoa(port)))
		},
	}
	defer transport.CloseIdleConnections()

	outReq := r.Clone(r.Context())
	outReq.RequestURI = ""
	outReq.Header.Del("Proxy-Authorization")
	outReq.Header.Del("Proxy-Connection")

	resp, err := transport.RoundTrip(outReq)
	if err != nil {
		p.denyForbidden(w, sess, host, port, "upstream_error")
		return
	}
	defer resp.Body.Close()

	_ = p.Audit.Append(audit.Event{
		SessionID: sess.ID, Actor: sess.User, Kind: audit.KindEgressAllowed,
		Payload: map[string]any{"host": host, "port": port, "groups": groups, "scheme": "http", "url": r.URL.String()},
	})

	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	n, _ := io.Copy(w, resp.Body)

	_ = p.Audit.Append(audit.Event{
		SessionID: sess.ID, Actor: sess.User, Kind: audit.KindEgressClosed,
		Payload: map[string]any{"host": host, "port": port, "bytes_received": n},
	})
}

var errPrivateAddr = errors.New("resolved to a blocked address")

// resolveSafe resolves host and returns the first address that survives the
// DNS-rebinding filter, or an error. It denies loopback/link-local/multicast
// unconditionally (covers cloud metadata at 169.254.169.254) and private
// ranges unless explicitly allowlisted (covers the kube API and neighbour
// namespaces).
func (p *Proxy) resolveSafe(ctx context.Context, host string) (netip.Addr, error) {
	addrs, err := p.resolver().LookupNetIP(ctx, "ip", host)
	if err != nil {
		return netip.Addr{}, err
	}
	var sawBlocked bool
	for _, a := range addrs {
		a = a.Unmap()
		if isHardBlocked(a) && !(p.allowLoopback && a.IsLoopback()) {
			sawBlocked = true
			continue
		}
		if isPrivate(a) && !p.Allowlist.PrivateAllowed(a) {
			sawBlocked = true
			continue
		}
		return a, nil
	}
	if sawBlocked {
		return netip.Addr{}, errPrivateAddr
	}
	return netip.Addr{}, errors.New("no addresses")
}

// isHardBlocked are addresses no allowlist can open: loopback, unspecified,
// link-local (incl. 169.254.169.254 cloud metadata), and multicast.
func isHardBlocked(a netip.Addr) bool {
	return a.IsLoopback() || a.IsUnspecified() ||
		a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() || a.IsMulticast()
}

// isPrivate are addresses blocked unless allowed_private_cidrs opts them in:
// RFC1918, CGNAT 100.64/10, and ULA fc00::/7.
func isPrivate(a netip.Addr) bool {
	if a.IsPrivate() {
		return true
	}
	if a.Is4() {
		return cgnat.Contains(a)
	}
	return false
}

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

func (p *Proxy) denyUnauthenticated(w http.ResponseWriter, host string, port int) {
	_ = p.Audit.Append(audit.Event{
		Actor: "unknown", Kind: audit.KindEgressDenied,
		Payload: map[string]any{"host": host, "port": port, "reason": "unauthenticated"},
	})
	w.Header().Set("Proxy-Authenticate", `Basic realm="paddock"`)
	http.Error(w, "proxy authentication required", http.StatusProxyAuthRequired)
}

func (p *Proxy) denyForbidden(w http.ResponseWriter, sess Session, host string, port int, reason string) {
	_ = p.Audit.Append(audit.Event{
		SessionID: sess.ID, Actor: orUnknown(sess.User), Kind: audit.KindEgressDenied,
		Payload: map[string]any{"host": host, "port": port, "reason": reason},
	})
	http.Error(w, "egress denied: "+reason, http.StatusForbidden)
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// proxyToken extracts the session token from a Proxy-Authorization header:
// Basic (password field) or Bearer.
func proxyToken(header string) string {
	if header == "" {
		return ""
	}
	scheme, rest, ok := strings.Cut(header, " ")
	if !ok {
		return ""
	}
	switch strings.ToLower(scheme) {
	case "basic":
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(rest))
		if err != nil {
			return ""
		}
		_, pass, ok := strings.Cut(string(raw), ":")
		if !ok {
			return ""
		}
		return pass
	case "bearer":
		return strings.TrimSpace(rest)
	}
	return ""
}

func splitHostPort(hostport string) (string, int, error) {
	host, portStr, err := net.SplitHostPort(hostport)
	if err != nil {
		return "", 0, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		return "", 0, fmt.Errorf("bad port %q", portStr)
	}
	return host, port, nil
}

// ListenerLog is a tiny helper so the gateway can log denied startups.
func (p *Proxy) LogConfig(l *log.Logger) {
	groups := make([]string, 0, len(p.Allowlist.Groups))
	for g := range p.Allowlist.Groups {
		groups = append(groups, g)
	}
	l.Printf("egress proxy: allowlist groups=%v ports=%v plainHTTP=%v", groups, p.Allowlist.AllowedPorts, p.Allowlist.PlainHTTP)
}
