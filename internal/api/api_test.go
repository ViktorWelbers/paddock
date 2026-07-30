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
