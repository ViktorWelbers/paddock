package api

import (
	"database/sql"
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
