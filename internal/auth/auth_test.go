package auth

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func writeTokens(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tokens.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestTokensAuthenticate(t *testing.T) {
	path := writeTokens(t, `{"users":[
		{"token":"tok-viktor","subject":"viktor"},
		{"token":"tok-root","subject":"root","groups":["paddock-admin"]}
	]}`)
	tokens, err := LoadTokens(path)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name, header, wantSubject string
		wantAdmin                 bool
		wantErr                   bool
	}{
		{name: "known token", header: "Bearer tok-viktor", wantSubject: "viktor"},
		{name: "admin group", header: "Bearer tok-root", wantSubject: "root", wantAdmin: true},
		{name: "unknown token", header: "Bearer nope", wantErr: true},
		{name: "no header", header: "", wantErr: true},
		{name: "not a bearer", header: "Basic tok-viktor", wantErr: true},
		{name: "bearer of nothing", header: "Bearer ", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/v1/sessions", nil)
			if tc.header != "" {
				r.Header.Set("Authorization", tc.header)
			}
			id, err := tokens.Authenticate(r)
			if tc.wantErr {
				if !errors.Is(err, ErrUnauthenticated) {
					t.Fatalf("err = %v, want ErrUnauthenticated", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if id.Subject != tc.wantSubject {
				t.Errorf("subject = %q, want %q", id.Subject, tc.wantSubject)
			}
			if id.IsAdmin() != tc.wantAdmin {
				t.Errorf("IsAdmin = %v, want %v", id.IsAdmin(), tc.wantAdmin)
			}
		})
	}
}

// A token file that cannot be understood must stop the server, not degrade
// it: the operator asked for authentication, and the failure mode of
// "carried on without any" is the one thing worse than not starting.
func TestLoadTokensRejectsUnusableFiles(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"missing subject", `{"users":[{"token":"t"}]}`},
		{"missing token", `{"users":[{"subject":"viktor"}]}`},
		{"no users", `{"users":[]}`},
		{"not json", `nope`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := LoadTokens(writeTokens(t, tc.body)); err == nil {
				t.Error("expected an error rather than a server that authenticates nobody")
			}
		})
	}
	if _, err := LoadTokens(filepath.Join(t.TempDir(), "absent.json")); err == nil {
		t.Error("a missing token file must be an error, not an empty allowlist")
	}
}

func TestMiddleware(t *testing.T) {
	path := writeTokens(t, `{"users":[{"token":"tok-viktor","subject":"viktor"}]}`)
	tokens, err := LoadTokens(path)
	if err != nil {
		t.Fatal(err)
	}

	var seen Identity
	var sawIdentity bool
	handler := Middleware(tokens, []string{"/healthz"}, http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			seen, sawIdentity = FromContext(r.Context())
			w.WriteHeader(http.StatusOK)
		}))

	t.Run("rejects anonymous", func(t *testing.T) {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/sessions", nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("code = %d, want 401", rec.Code)
		}
		if rec.Header().Get("WWW-Authenticate") == "" {
			t.Error("a 401 should say how to authenticate")
		}
	})

	t.Run("passes identity through", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/v1/sessions", nil)
		r.Header.Set("Authorization", "Bearer tok-viktor")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, r)
		if rec.Code != http.StatusOK {
			t.Fatalf("code = %d, want 200", rec.Code)
		}
		if !sawIdentity {
			t.Fatal("handler ran without an identity in context")
		}
		if seen.Subject != "viktor" {
			t.Errorf("identity = %q, want viktor", seen.Subject)
		}
	})

	t.Run("health check needs no identity", func(t *testing.T) {
		sawIdentity = true // must be cleared by the handler below
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
		if rec.Code != http.StatusOK {
			t.Errorf("code = %d, want 200: a probe has no credential to offer", rec.Code)
		}
		if sawIdentity {
			t.Error("a public path must not manufacture an identity")
		}
	})
}
