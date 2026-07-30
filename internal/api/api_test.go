package api

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/viktorwelbers/paddock/internal/audit"
	"github.com/viktorwelbers/paddock/internal/auth"
	"github.com/viktorwelbers/paddock/internal/budget"
	"github.com/viktorwelbers/paddock/internal/sandbox"
)

func testHandler(t *testing.T, cfg Config) *Handler {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	ledger, err := budget.NewLedger(db, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.Create(budget.Budget{ID: "b1", Name: "b1", LimitUSD: 10}); err != nil {
		t.Fatal(err)
	}
	auditStore, err := audit.NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	return &Handler{Sessions: sessions, Ledger: ledger, Audit: auditStore,
		Provisioner: sandbox.Noop{}, Config: cfg}
}

// stubAuth authenticates every request as whatever identity is registered
// under the bearer token — enough to exercise per-caller authorization
// without pulling in auth.Tokens' file-and-digest machinery.
type stubAuth struct {
	byToken map[string]auth.Identity
}

func (s stubAuth) Authenticate(r *http.Request) (auth.Identity, error) {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	id, ok := s.byToken[tok]
	if !ok {
		return auth.Identity{}, auth.ErrUnauthenticated
	}
	return id, nil
}

func (s stubAuth) Describe() string { return "stub" }

// budgetAuthHandler builds a handler with a small budget hierarchy —
// org → team → user (Alice's chain) and org → team2 → user2 (Bob's, a
// sibling) — plus a stub authenticator so tests can act as distinct callers.
func budgetAuthHandler(t *testing.T) *Handler {
	t.Helper()
	h := testHandler(t, Config{AgentImages: map[string]string{"claude": "img"}, GatewayURL: "http://gw"})
	for _, b := range []budget.Budget{
		{ID: "org", Name: "org", LimitUSD: 1000},
		{ID: "team", ParentID: "org", Name: "team", LimitUSD: 500},
		{ID: "user", ParentID: "team", Name: "user", LimitUSD: 100},
		{ID: "team2", ParentID: "org", Name: "team2", LimitUSD: 500},
		{ID: "user2", ParentID: "team2", Name: "user2", LimitUSD: 100},
	} {
		if err := h.Ledger.Create(b); err != nil {
			t.Fatal(err)
		}
	}
	h.Auth = stubAuth{byToken: map[string]auth.Identity{
		"alice": {Subject: "alice"},
		"bob":   {Subject: "bob"},
		"root":  {Subject: "root", Groups: []string{auth.GroupAdmin}},
	}}
	return h
}

// createSessionAs creates a session authenticated as the given stub token,
// referencing budgetID.
func createSessionAs(t *testing.T, h *Handler, token, budgetID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/v1/sessions",
		strings.NewReader(`{"agent":"claude","budget_id":"`+budgetID+`"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, req)
	return rec
}

// budgetsReq issues an authenticated GET against a budgets endpoint.
func budgetsReq(t *testing.T, h *Handler, token, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, req)
	return rec
}

func createSessionReq(t *testing.T, h *Handler, agent string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/v1/sessions",
		strings.NewReader(`{"user":"viktor","agent":"`+agent+`","budget_id":"b1"}`)))
	return rec
}

// An agent nobody configured has no image and no provider env contract. It
// used to fall through to the default image, so `paddock run claud` spawned a
// Claude sandbox labelled with the typo — billed, audited, and wrong.
func TestCreateSessionRejectsUnconfiguredAgent(t *testing.T) {
	h := testHandler(t, Config{
		AgentImages: map[string]string{"claude": "img"},
		GatewayURL:  "http://gw",
	})

	if rec := createSessionReq(t, h, "claude"); rec.Code != http.StatusCreated {
		t.Fatalf("configured agent: %d %s", rec.Code, rec.Body.String())
	}

	rec := createSessionReq(t, h, "definitely-not-an-agent")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("unconfigured agent = %d, want 400 (got body %q)", rec.Code, rec.Body.String())
	}
}

// The pi agent needs an OpenAI upstream and a pinned model; without them the
// sandbox would come up unable to reach any model at all.
func TestCreateSessionRejectsPiWithoutOpenAIConfig(t *testing.T) {
	h := testHandler(t, Config{
		AgentImages: map[string]string{"claude": "img", "pi": "pi-img"},
		GatewayURL:  "http://gw",
	})
	if rec := createSessionReq(t, h, "pi"); rec.Code != http.StatusBadRequest {
		t.Errorf("pi without openai config = %d, want 400", rec.Code)
	}
}

// listedIDs returns the session IDs `paddock ls` would show for the given path.
func listedIDs(t *testing.T, h *Handler, path string) map[string]bool {
	t.Helper()
	w := httptest.NewRecorder()
	h.Handler().ServeHTTP(w, httptest.NewRequest("GET", path, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("list %s: %d %s", path, w.Code, w.Body)
	}
	var ss []Session
	if err := json.Unmarshal(w.Body.Bytes(), &ss); err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, s := range ss {
		ids[s.ID] = true
	}
	return ids
}

// The per-session ResourceQuota went away with the per-session namespace, so
// this ceiling is the only thing standing between a runaway `POST /v1/sessions`
// loop and every node in the cluster. Past the cap, create is a 429; ending a
// session frees the slot.
func TestSessionCeilingPerUser(t *testing.T) {
	h := testHandler(t, Config{
		AgentImages:        map[string]string{"claude": "img"},
		GatewayURL:         "http://gw",
		MaxSessionsPerUser: 2,
	})

	first := sessionID(t, createSessionReq(t, h, "claude"))
	sessionID(t, createSessionReq(t, h, "claude"))
	if rec := createSessionReq(t, h, "claude"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("over-cap create = %d, want 429 (body %q)", rec.Code, rec.Body.String())
	}

	// A terminal session no longer holds a sandbox, so the slot comes back.
	if err := h.Sessions.setStatus(first, statusDeleted); err != nil {
		t.Fatal(err)
	}
	if rec := createSessionReq(t, h, "claude"); rec.Code != http.StatusCreated {
		t.Fatalf("after freeing a slot = %d, want 201 (body %q)", rec.Code, rec.Body.String())
	}
}

// The total cap is a whole-server ceiling; unlike the per-user one it isn't
// reset by spreading load across users (the test handler owns everything as one
// anonymous subject anyway, so it exercises the count path directly).
func TestSessionCeilingTotal(t *testing.T) {
	h := testHandler(t, Config{
		AgentImages:      map[string]string{"claude": "img"},
		GatewayURL:       "http://gw",
		MaxSessionsTotal: 1,
	})
	sessionID(t, createSessionReq(t, h, "claude"))
	if rec := createSessionReq(t, h, "claude"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("over-total-cap create = %d, want 429 (body %q)", rec.Code, rec.Body.String())
	}
}

// Docker-like default: `paddock ls` shows only running sessions; `--all`
// (?all=1) includes the stopped ones so the default view stays uncluttered.
func TestListSessionsHidesStoppedByDefault(t *testing.T) {
	h := testHandler(t, Config{AgentImages: map[string]string{"claude": "img"}, GatewayURL: "http://gw"})

	running := sessionID(t, createSessionReq(t, h, "claude"))
	stopped := sessionID(t, createSessionReq(t, h, "claude"))
	if err := h.Sessions.setStatus(stopped, statusDeleted); err != nil {
		t.Fatal(err)
	}

	def := listedIDs(t, h, "/v1/sessions")
	if !def[running] || def[stopped] {
		t.Errorf("default ls must show running and hide stopped; got running=%v stopped=%v", def[running], def[stopped])
	}

	all := listedIDs(t, h, "/v1/sessions?all=1")
	if !all[running] || !all[stopped] {
		t.Errorf("ls --all must show both; got running=%v stopped=%v", all[running], all[stopped])
	}
}

// A non-admin's budget list is exactly their own spend chain — their
// session's budget plus every ancestor — never a sibling team's, and never
// the unrelated flat "b1" budget testHandler always seeds.
func TestListBudgetsNonAdminSeesOnlyOwnChain(t *testing.T) {
	h := budgetAuthHandler(t)
	if rec := createSessionAs(t, h, "alice", "user"); rec.Code != http.StatusCreated {
		t.Fatalf("create session: %d %s", rec.Code, rec.Body)
	}

	rec := budgetsReq(t, h, "alice", "/v1/budgets")
	if rec.Code != http.StatusOK {
		t.Fatalf("list budgets: %d %s", rec.Code, rec.Body)
	}
	var out []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, b := range out {
		got[b["id"].(string)] = true
	}
	want := map[string]bool{"user": true, "team": true, "org": true}
	if len(got) != len(want) {
		t.Fatalf("got budgets %v, want exactly %v", got, want)
	}
	for id := range want {
		if !got[id] {
			t.Errorf("missing %s in non-admin's list", id)
		}
	}
}

// An admin sees every budget, including ones no session references.
func TestListBudgetsAdminSeesAll(t *testing.T) {
	h := budgetAuthHandler(t)
	if rec := createSessionAs(t, h, "alice", "user"); rec.Code != http.StatusCreated {
		t.Fatalf("create session: %d %s", rec.Code, rec.Body)
	}

	rec := budgetsReq(t, h, "root", "/v1/budgets")
	var out []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out) != 6 { // b1, org, team, user, team2, user2
		t.Fatalf("admin should see all 6 budgets, got %d: %v", len(out), out)
	}
}

// getBudget returns 404 — not 403 — for a budget outside the caller's
// visible set, exercising a sibling leaf, a sibling's own ancestor path, a
// 2-hop-away ancestor that IS visible, and the caller's own leaf.
func TestGetBudgetOutOfScopeIs404(t *testing.T) {
	h := budgetAuthHandler(t)
	if rec := createSessionAs(t, h, "alice", "user"); rec.Code != http.StatusCreated {
		t.Fatalf("create session: %d %s", rec.Code, rec.Body)
	}

	for _, tc := range []struct {
		id   string
		want int
	}{
		{"user", http.StatusOK},        // own leaf
		{"team", http.StatusOK},        // own parent
		{"org", http.StatusOK},         // 2-hop ancestor, verifies multi-hop expansion
		{"team2", http.StatusNotFound}, // sibling team, not an ancestor of alice's chain
		{"user2", http.StatusNotFound}, // sibling leaf
		{"b1", http.StatusNotFound},    // unrelated flat budget
	} {
		if rec := budgetsReq(t, h, "alice", "/v1/budgets/"+tc.id); rec.Code != tc.want {
			t.Errorf("GET /v1/budgets/%s = %d, want %d (body %q)", tc.id, rec.Code, tc.want, rec.Body)
		}
	}
}

// A user with no sessions has an empty visible set: listBudgets is empty and
// every getBudget is a 404, even for the org root.
func TestBudgetVisibilityEmptyForUserWithNoSessions(t *testing.T) {
	h := budgetAuthHandler(t) // bob never creates a session

	rec := budgetsReq(t, h, "bob", "/v1/budgets")
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Errorf("list = %d %q, want 200 []", rec.Code, rec.Body.String())
	}
	if rec := budgetsReq(t, h, "bob", "/v1/budgets/org"); rec.Code != http.StatusNotFound {
		t.Errorf("get org = %d, want 404 for a user with no sessions", rec.Code)
	}
}
