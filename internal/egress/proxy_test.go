package egress

import (
	"context"
	"crypto/tls"
	"database/sql"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/viktorwelbers/paddock/internal/audit"
	"github.com/viktorwelbers/paddock/internal/policy"
)

type stubResolver map[string][]netip.Addr

func (s stubResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	if a, ok := s[host]; ok {
		return a, nil
	}
	return nil, &net.DNSError{Err: "not found", Name: host, IsNotFound: true}
}

func allowAll(context.Context, policy.Input) (policy.Decision, error) {
	return policy.Decision{Allow: true}, nil
}

func newAudit(t *testing.T) *audit.Store {
	t.Helper()
	// Same DSN the server and gateway use: the proxy appends audit rows from
	// its tunnel goroutines while the test reads them back, and without WAL
	// and a busy timeout that contention is SQLITE_BUSY rather than a wait.
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "a.db")+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	st, err := audit.NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// newProxyTo builds a proxy whose allowlist maps "registry.test" to the given
// upstream, running on a real listener; returns the proxy base URL and audit.
func newProxyTo(t *testing.T, upstreamHost string, upstreamAddr netip.Addr, port int) (string, *audit.Store) {
	t.Helper()
	aud := newAudit(t)
	al, err := normalize(&Allowlist{
		Groups:       map[string][]string{"test": {upstreamHost}},
		AllowedPorts: []int{port},
	})
	if err != nil {
		t.Fatal(err)
	}
	p := &Proxy{
		Auth: func(token string) (Session, error) {
			if token == "good-token" {
				return Session{ID: "s1", User: "viktor", Agent: "claude"}, nil
			}
			return Session{}, io.EOF
		},
		Policy:        allowAll,
		Audit:         aud,
		Allowlist:     al,
		Resolver:      stubResolver{upstreamHost: {upstreamAddr}},
		allowLoopback: true,
	}
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	return srv.URL, aud
}

func clientVia(proxyBase, token string) *http.Client {
	u, _ := url.Parse(proxyBase)
	if token != "" {
		u.User = url.UserPassword("paddock", token)
	}
	return &http.Client{Transport: &http.Transport{
		Proxy:           http.ProxyURL(u),
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		// The proxy only writes egress.closed once both halves of the tunnel
		// finish, so tests need the connection to actually end when the
		// response does. With keep-alives on, the transport parks the
		// connection in its idle pool *asynchronously* after EOF, which makes
		// closing it from the test a race rather than an instruction.
		DisableKeepAlives: true,
	}}
}

func kinds(t *testing.T, aud *audit.Store, session string) []string {
	t.Helper()
	events, err := aud.BySession(session)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range events {
		out = append(out, e.Kind)
	}
	return out
}

func has(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func TestConnectTunnelAllowed(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "hello from upstream")
	}))
	defer upstream.Close()
	uurl, _ := url.Parse(upstream.URL)
	port := mustPort(t, uurl.Port())

	proxyBase, aud := newProxyTo(t, "registry.test", netip.MustParseAddr("127.0.0.1"), port)
	client := clientVia(proxyBase, "good-token")

	// The proxy only tunnels — TLS terminates at the upstream, proving no MITM.
	resp, err := client.Get("https://registry.test:" + uurl.Port() + "/")
	if err != nil {
		t.Fatalf("tunneled request failed: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "hello from upstream" {
		t.Errorf("body = %q", body)
	}

	if k := kinds(t, aud, "s1"); !has(k, audit.KindEgressAllowed) {
		t.Errorf("missing egress.allowed event, got %v", k)
	}

	// Ending the tunnel is what produces egress.closed. Closing the body is
	// the deterministic trigger (keep-alives are off, so this closes the
	// connection); the audit row is still appended by the proxy's own
	// goroutine, hence the poll.
	resp.Body.Close()
	client.CloseIdleConnections()
	waitForKind(t, aud, "s1", audit.KindEgressClosed)

	// bytes_sent/received should be non-zero on the close event.
	events, _ := aud.BySession("s1")
	for _, e := range events {
		if e.Kind == audit.KindEgressClosed {
			if sent, _ := e.Payload["bytes_sent"].(float64); sent <= 0 {
				t.Errorf("bytes_sent = %v, want > 0", e.Payload["bytes_sent"])
			}
			if recv, _ := e.Payload["bytes_received"].(float64); recv <= 0 {
				t.Errorf("bytes_received = %v, want > 0", e.Payload["bytes_received"])
			}
		}
	}
}

func TestConnectDeniedHostAudited(t *testing.T) {
	proxyBase, aud := newProxyTo(t, "registry.test", netip.MustParseAddr("127.0.0.1"), 443)
	client := clientVia(proxyBase, "good-token")

	_, err := client.Get("https://evil.test:443/")
	if err == nil {
		t.Fatal("expected the CONNECT to be refused for a non-allowlisted host")
	}
	if k := kinds(t, aud, "s1"); !has(k, audit.KindEgressDenied) {
		t.Errorf("denied host must be audited, got %v", k)
	}
}

func TestConnectRequiresAuth(t *testing.T) {
	proxyBase, _ := newProxyTo(t, "registry.test", netip.MustParseAddr("127.0.0.1"), 443)
	client := clientVia(proxyBase, "") // no credentials

	_, err := client.Get("https://registry.test:443/")
	if err == nil {
		t.Fatal("expected 407 without proxy credentials")
	}
}

func TestConnectPrivateAddressBlocked(t *testing.T) {
	// Host is allowlisted, but it resolves into a blocked range (rebinding).
	for _, ip := range []string{"169.254.169.254", "10.0.0.5", "100.64.0.1"} {
		aud := newAudit(t)
		al, _ := normalize(&Allowlist{Groups: map[string][]string{"test": {"registry.test"}}, AllowedPorts: []int{443}})
		p := &Proxy{
			Auth:      func(string) (Session, error) { return Session{ID: "s1", User: "v"}, nil },
			Policy:    allowAll,
			Audit:     aud,
			Allowlist: al,
			Resolver:  stubResolver{"registry.test": {netip.MustParseAddr(ip)}},
		}
		srv := httptest.NewServer(p)
		client := clientVia(srv.URL, "good-token")
		if _, err := client.Get("https://registry.test:443/"); err == nil {
			t.Errorf("%s: expected the tunnel to be blocked by the rebinding filter", ip)
		}
		events, _ := aud.BySession("s1")
		var reason string
		for _, e := range events {
			if e.Kind == audit.KindEgressDenied {
				reason, _ = e.Payload["reason"].(string)
			}
		}
		if reason != "private_address" {
			t.Errorf("%s: deny reason = %q, want private_address", ip, reason)
		}
		srv.Close()
	}
}

func TestPrivateCIDRPunchThrough(t *testing.T) {
	al, err := normalize(&Allowlist{AllowedPrivateCIDRs: []string{"10.0.0.0/8"}})
	if err != nil {
		t.Fatal(err)
	}
	if !al.PrivateAllowed(netip.MustParseAddr("10.1.2.3")) {
		t.Error("10.1.2.3 should be permitted by the 10.0.0.0/8 punch-through")
	}
	if al.PrivateAllowed(netip.MustParseAddr("192.168.1.1")) {
		t.Error("192.168.1.1 is not in any allowed CIDR and must stay blocked")
	}
}

// waitForKind polls the audit log for an event kind that a background
// goroutine writes when the tunnel tears down.
func waitForKind(t *testing.T, aud *audit.Store, session, kind string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if has(kinds(t, aud, session), kind) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s, got %v", kind, kinds(t, aud, session))
}

func mustPort(t *testing.T, s string) int {
	t.Helper()
	p, err := strconv.Atoi(s)
	if err != nil {
		t.Fatalf("bad port %q: %v", s, err)
	}
	return p
}
