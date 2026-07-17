// Package auth answers "who is calling" for the control-plane API.
//
// It matters more here than the size of the package suggests. Paddock's
// claim is that it can tell you who did what: which developer ran which
// agent, what it spent, where it tried to connect. Every part of that rests
// on the identity attached to a session — and until this package existed,
// that identity was a string the client typed into a JSON body. The audit
// trail attributed actions; it did not establish them.
//
// The Authenticator seam is the point: an operator with an IdP wants OIDC,
// an operator without one wants a file of tokens, and a laptop wants
// neither. They differ only in how a request becomes an Identity.
package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
)

// GroupAdmin is the group that may see and act on other people's sessions.
// Operators, not agents: nothing inside a sandbox can reach this API.
const GroupAdmin = "paddock-admin"

// Identity is the authenticated caller. Subject is what lands in the audit
// trail and owns the sessions it creates, so it must come from the
// authenticator — never from the request body.
type Identity struct {
	Subject string
	Groups  []string
}

// IsAdmin reports whether this identity may act on sessions it does not own.
func (i Identity) IsAdmin() bool { return slices.Contains(i.Groups, GroupAdmin) }

// ErrUnauthenticated is returned by an Authenticator that could not
// establish who is calling.
var ErrUnauthenticated = errors.New("unauthenticated")

// Authenticator turns a request into an Identity.
type Authenticator interface {
	Authenticate(r *http.Request) (Identity, error)
	// Describe is what the server logs at startup, so the deployment's auth
	// posture is visible without reading its flags.
	Describe() string
}

// Anonymous authenticates every caller as the same identity. It is how
// paddock ran before this package existed, and it remains the right answer
// for a laptop or a k3d loop — but it is now a deliberate choice with a name
// rather than the unexamined default, and the server says so on every start.
type Anonymous struct{ As string }

func (a Anonymous) Authenticate(*http.Request) (Identity, error) {
	subject := a.As
	if subject == "" {
		subject = "anonymous"
	}
	return Identity{Subject: subject}, nil
}

func (a Anonymous) Describe() string {
	return fmt.Sprintf("DISABLED — every caller is %q and may act on every session", a.As)
}

// Tokens authenticates static bearer tokens from a file, which is how
// self-hosted software is configured when there is no IdP to defer to: a
// Secret, mounted, holding a token per human.
//
// Tokens are held as digests. The file is the only place the plaintext
// lives, and a lookup by digest keeps the comparison off the timing side
// channel that a byte-by-byte match against a stored secret would open.
type Tokens struct {
	byDigest map[string]Identity
}

// tokenFile is the on-disk format.
type tokenFile struct {
	Users []struct {
		Token   string   `json:"token"`
		Subject string   `json:"subject"`
		Groups  []string `json:"groups,omitempty"`
	} `json:"users"`
}

// LoadTokens reads a token file. An empty file is an error rather than a
// deny-all: an operator who asked for token auth and got a typo'd path
// should be told at startup, not by every developer at once.
func LoadTokens(path string) (*Tokens, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read token file: %w", err)
	}
	var f tokenFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("parse token file %s: %w", path, err)
	}
	t := &Tokens{byDigest: map[string]Identity{}}
	for i, u := range f.Users {
		if u.Token == "" || u.Subject == "" {
			return nil, fmt.Errorf("%s: users[%d] needs both a token and a subject", path, i)
		}
		t.byDigest[digest(u.Token)] = Identity{Subject: u.Subject, Groups: u.Groups}
	}
	if len(t.byDigest) == 0 {
		return nil, fmt.Errorf("%s: no users configured", path)
	}
	return t, nil
}

func (t *Tokens) Authenticate(r *http.Request) (Identity, error) {
	token := bearer(r.Header.Get("Authorization"))
	if token == "" {
		return Identity{}, ErrUnauthenticated
	}
	id, ok := t.byDigest[digest(token)]
	if !ok {
		return Identity{}, ErrUnauthenticated
	}
	return id, nil
}

func (t *Tokens) Describe() string {
	return fmt.Sprintf("bearer tokens (%d configured)", len(t.byDigest))
}

func digest(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func bearer(header string) string {
	token, ok := strings.CutPrefix(header, "Bearer ")
	if !ok {
		return ""
	}
	return strings.TrimSpace(token)
}

type contextKey struct{}

// Middleware authenticates every request and puts the Identity in the
// context. Handlers read it with FromContext and never trust the body.
//
// public paths skip authentication: health checks have no identity to carry,
// and the dashboard is markup — every byte of data it shows comes back
// through the authenticated API.
func Middleware(a Authenticator, public []string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if slices.Contains(public, r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		id, err := a.Authenticate(r)
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="paddock"`)
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), contextKey{}, id)))
	})
}

// FromContext returns the authenticated caller. The second result is false
// only for requests that never passed through Middleware — handlers should
// treat that as a bug, not as anonymous access.
func FromContext(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(contextKey{}).(Identity)
	return id, ok
}
