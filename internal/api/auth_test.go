package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/viktorwelbers/paddock/internal/auth"
)

// authedHandler wires the handler behind real token authentication:
// viktor and mallory are ordinary developers, root is an admin.
func authedHandler(t *testing.T) *Handler {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tokens.json")
	if err := os.WriteFile(path, []byte(`{"users":[
		{"token":"tok-viktor","subject":"viktor"},
		{"token":"tok-mallory","subject":"mallory"},
		{"token":"tok-root","subject":"root","groups":["paddock-admin"]}
	]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	tokens, err := auth.LoadTokens(path)
	if err != nil {
		t.Fatal(err)
	}
	h := testHandler(t, Config{AgentImages: map[string]string{"claude": "img"}, GatewayURL: "http://gw"})
	h.Auth = tokens
	return h
}

func as(t *testing.T, h *Handler, token, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, r)
	return rec
}

func createAs(t *testing.T, h *Handler, token string) string {
	t.Helper()
	rec := as(t, h, token, "POST", "/v1/sessions", `{"agent":"claude","budget_id":"b1"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create session: %d %s", rec.Code, rec.Body.String())
	}
	var sess Session
	if err := json.Unmarshal(rec.Body.Bytes(), &sess); err != nil {
		t.Fatal(err)
	}
	return sess.ID
}

// The whole point: a session's owner is the authenticated caller, not a
// string the caller typed. Otherwise the audit trail records intentions.
func TestSessionOwnerComesFromTheTokenNotTheBody(t *testing.T) {
	h := authedHandler(t)
	rec := as(t, h, "tok-viktor", "POST", "/v1/sessions",
		`{"user":"someone-else","agent":"claude","budget_id":"b1"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create session: %d %s", rec.Code, rec.Body.String())
	}
	var sess Session
	if err := json.Unmarshal(rec.Body.Bytes(), &sess); err != nil {
		t.Fatal(err)
	}
	if sess.User != "viktor" {
		t.Errorf("owner = %q, want viktor: the body claimed someone-else and was believed", sess.User)
	}
}

func TestUnauthenticatedRequestsAreRejected(t *testing.T) {
	h := authedHandler(t)
	id := createAs(t, h, "tok-viktor")

	for _, tc := range []struct{ method, path string }{
		{"GET", "/v1/sessions"},
		{"POST", "/v1/sessions"},
		{"GET", "/v1/sessions/" + id},
		{"DELETE", "/v1/sessions/" + id},
		{"GET", "/v1/sessions/" + id + "/events"},
		{"GET", "/v1/sessions/" + id + "/workspace"},
		{"POST", "/v1/sessions/" + id + "/workspace"},
		{"GET", "/v1/budgets"},
	} {
		if rec := as(t, h, "", tc.method, tc.path, ""); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s = %d, want 401", tc.method, tc.path, rec.Code)
		}
	}
}

// GET /workspace hands over every file in a sandbox. Session ids are short
// and appear in logs and terminals, so they are not a secret — they must not
// be the only thing protecting someone's source code.
func TestOneDeveloperCannotReachAnothersSession(t *testing.T) {
	h := authedHandler(t)
	id := createAs(t, h, "tok-viktor")

	for _, tc := range []struct{ method, path string }{
		{"GET", "/v1/sessions/" + id},
		{"GET", "/v1/sessions/" + id + "/events"},
		{"GET", "/v1/sessions/" + id + "/workspace"},
		{"POST", "/v1/sessions/" + id + "/workspace"},
		{"DELETE", "/v1/sessions/" + id},
	} {
		rec := as(t, h, "tok-mallory", tc.method, tc.path, "")
		// 404, not 403: a 403 confirms the session exists.
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s as another developer = %d, want 404", tc.method, tc.path, rec.Code)
		}
	}

	// And the session survived mallory's DELETE.
	sess, err := h.Sessions.get(id)
	if err != nil {
		t.Fatal(err)
	}
	if sess.Status != statusRunning {
		t.Errorf("status = %q: another developer deleted viktor's session", sess.Status)
	}
}

func TestListSessionsShowsOnlyYourOwn(t *testing.T) {
	h := authedHandler(t)
	createAs(t, h, "tok-viktor")
	createAs(t, h, "tok-mallory")

	var mine []Session
	rec := as(t, h, "tok-viktor", "GET", "/v1/sessions", "")
	if err := json.Unmarshal(rec.Body.Bytes(), &mine); err != nil {
		t.Fatal(err)
	}
	if len(mine) != 1 || mine[0].User != "viktor" {
		t.Errorf("ls showed %d sessions %v, want only viktor's", len(mine), mine)
	}

	var all []Session
	rec = as(t, h, "tok-root", "GET", "/v1/sessions", "")
	if err := json.Unmarshal(rec.Body.Bytes(), &all); err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Errorf("admin saw %d sessions, want both", len(all))
	}
}

func TestAdminMayReachAnothersSession(t *testing.T) {
	h := authedHandler(t)
	id := createAs(t, h, "tok-viktor")
	if rec := as(t, h, "tok-root", "GET", "/v1/sessions/"+id, ""); rec.Code != http.StatusOK {
		t.Errorf("admin GET = %d, want 200", rec.Code)
	}
}

// The dashboard is markup; its data arrives through the authenticated API.
// Probes have no credential to offer.
func TestPublicPathsNeedNoToken(t *testing.T) {
	h := authedHandler(t)
	for _, path := range []string{"/healthz", "/"} {
		if rec := as(t, h, "", "GET", path, ""); rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, rec.Code)
		}
	}
}
