// Package mcpgw is the server-side MCP layer: a central registry of
// allowlisted MCP servers administered by the platform team, and a
// credential broker that injects secrets at the gateway so they never
// enter a sandbox.
package mcpgw

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"

	"github.com/viktorwelbers/paddock/internal/audit"
	"github.com/viktorwelbers/paddock/internal/policy"
)

// ServerEntry is one centrally administered MCP server.
type ServerEntry struct {
	Name string `json:"name"`
	URL  string `json:"url"`
	// CredentialEnv names the env var (on the gateway host) holding the
	// server's bearer credential. Enterprise tier swaps this for a real
	// secret backend behind the Broker interface.
	CredentialEnv string `json:"credential_env,omitempty"`
}

// Registry is the allowlist. Anything not in it does not exist as far as
// sandboxes are concerned.
type Registry struct {
	Servers map[string]ServerEntry
}

// LoadRegistry reads a JSON file: {"servers": [{"name": ..., "url": ...}]}.
func LoadRegistry(path string) (*Registry, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg struct {
		Servers []ServerEntry `json:"servers"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse MCP registry %q: %w", path, err)
	}
	r := &Registry{Servers: map[string]ServerEntry{}}
	for _, s := range cfg.Servers {
		r.Servers[s.Name] = s
	}
	return r, nil
}

// Broker resolves credentials for MCP servers. Implementations live on the
// gateway; sandboxes only ever hold session tokens.
type Broker interface {
	CredentialFor(server ServerEntry) (string, error)
}

// EnvBroker reads credentials from the gateway's environment.
type EnvBroker struct{}

func (EnvBroker) CredentialFor(s ServerEntry) (string, error) {
	if s.CredentialEnv == "" {
		return "", nil
	}
	v, ok := os.LookupEnv(s.CredentialEnv)
	if !ok {
		return "", fmt.Errorf("credential env %q for MCP server %q is not set", s.CredentialEnv, s.Name)
	}
	return v, nil
}

// PolicyFunc decouples the mux from the policy engine for testing.
type PolicyFunc func(ctx context.Context, in policy.Input) (policy.Decision, error)

// Mux relays sandbox MCP traffic to allowlisted servers: registry check,
// policy decision, credential injection, audit event — in that order.
type Mux struct {
	Registry *Registry
	Broker   Broker
	Policy   PolicyFunc
	Audit    *audit.Store
	// SessionFromRequest authenticates the sandbox's session token and
	// returns (sessionID, user). Wired up by the gateway binary.
	SessionFromRequest func(r *http.Request) (string, string, error)
}

// ServeHTTP handles /mcp/{server}/... paths.
func (m *Mux) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/mcp/")
	name, _, _ := strings.Cut(rest, "/")
	if name == "" {
		http.Error(w, "missing MCP server name", http.StatusBadRequest)
		return
	}

	sessionID, user := "unknown", "unknown"
	if m.SessionFromRequest != nil {
		var err error
		if sessionID, user, err = m.SessionFromRequest(r); err != nil {
			http.Error(w, "invalid session", http.StatusUnauthorized)
			return
		}
	}

	entry, ok := m.Registry.Servers[name]
	if !ok {
		m.deny(w, sessionID, user, name, fmt.Sprintf("MCP server %q is not on the allowlist", name))
		return
	}

	dec, err := m.Policy(r.Context(), policy.Input{
		Kind: "mcp_call", User: user, Session: sessionID, Server: name,
	})
	if err != nil {
		http.Error(w, "policy evaluation failed", http.StatusInternalServerError)
		return
	}
	if !dec.Allow {
		m.deny(w, sessionID, user, name, strings.Join(dec.Reasons, "; "))
		return
	}

	cred, err := m.Broker.CredentialFor(entry)
	if err != nil {
		http.Error(w, "credential broker error", http.StatusBadGateway)
		return
	}

	target, err := url.Parse(entry.URL)
	if err != nil {
		http.Error(w, "bad upstream URL", http.StatusBadGateway)
		return
	}
	_ = m.Audit.Append(audit.Event{
		SessionID: sessionID, Actor: user, Kind: audit.KindMCPCall,
		Payload: map[string]any{"server": name, "path": r.URL.Path},
	})

	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.URL.Path = "/" + strings.TrimPrefix(rest, name+"/")
			pr.Out.Header.Del("Authorization")
			if cred != "" {
				pr.Out.Header.Set("Authorization", "Bearer "+cred)
			}
		},
	}
	proxy.ServeHTTP(w, r)
}

func (m *Mux) deny(w http.ResponseWriter, sessionID, user, server, reason string) {
	_ = m.Audit.Append(audit.Event{
		SessionID: sessionID, Actor: user, Kind: audit.KindPolicyDenied,
		Payload: map[string]any{"server": server, "reason": reason},
	})
	http.Error(w, reason, http.StatusForbidden)
}
