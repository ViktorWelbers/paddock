package gateway

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/viktorwelbers/paddock/internal/api"
	"github.com/viktorwelbers/paddock/internal/audit"
	"github.com/viktorwelbers/paddock/internal/budget"
	"github.com/viktorwelbers/paddock/internal/sandbox"
)

type fix struct {
	srv       *httptest.Server
	ledger    *budget.Ledger
	audit     *audit.Store
	token     string
	sessionID string
}

// newBackends spins up SQLite stores, one budget, and one session created
// through the public API.
func newBackends(t *testing.T, limitUSD float64) (Backends, api.Session) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "gw.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	ledger, err := budget.NewLedger(db, budget.PriceTable{
		"test-model": {InputPerMTok: 10, OutputPerMTok: 20},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.Create(budget.Budget{ID: "b1", Name: "b1", LimitUSD: limitUSD}); err != nil {
		t.Fatal(err)
	}
	auditStore, err := audit.NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := api.NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	h := &api.Handler{Sessions: sessions, Ledger: ledger, Audit: auditStore,
		Provisioner: sandbox.Noop{}, Config: api.Config{AgentImage: "img", GatewayURL: "http://gw"}}
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, httptest.NewRequest("POST", "/v1/sessions",
		strings.NewReader(`{"user":"viktor","agent":"claude","budget_id":"b1"}`)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create session: %d %s", rec.Code, rec.Body.String())
	}
	var sess api.Session
	if err := json.Unmarshal(rec.Body.Bytes(), &sess); err != nil {
		t.Fatal(err)
	}
	return Backends{Sessions: sessions, Ledger: ledger, Audit: auditStore}, sess
}

// fixture wires the backends to a fake Anthropic upstream and the proxy
// under test.
func fixture(t *testing.T, upstream http.HandlerFunc, limitUSD float64) fix {
	t.Helper()
	b, sess := newBackends(t, limitUSD)

	up := httptest.NewServer(upstream)
	t.Cleanup(up.Close)
	upURL, _ := url.Parse(up.URL)

	proxy := &AnthropicProxy{Backends: b, Upstream: upURL, APIKey: "real-key"}
	srv := httptest.NewServer(proxy)
	t.Cleanup(srv.Close)
	return fix{srv: srv, ledger: b.Ledger, audit: b.Audit, token: sess.Token, sessionID: sess.ID}
}

func call(t *testing.T, srv *httptest.Server, token string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", srv.URL+"/anthropic/v1/messages", strings.NewReader(`{}`))
	req.Header.Set("x-api-key", token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func drain(t *testing.T, resp *http.Response) {
	t.Helper()
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

func TestJSONUsageIsMeteredAndKeySwapped(t *testing.T) {
	var upstreamSawKey string
	f := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		upstreamSawKey = r.Header.Get("x-api-key")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"model":"test-model","usage":{"input_tokens":100000,"output_tokens":100000}}`)
	}, 100)

	resp := call(t, f.srv, f.token)
	drain(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if upstreamSawKey != "real-key" {
		t.Fatalf("upstream saw key %q, want the real provider key", upstreamSawKey)
	}

	b, _ := f.ledger.Get("b1")
	if b.SpentUSD != 3 { // 0.1M*10 + 0.1M*20
		t.Fatalf("spent = %v, want 3", b.SpentUSD)
	}
	events, err := f.audit.BySession(f.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if !hasKind(events, audit.KindModelCall) {
		t.Fatalf("no model.call audit event; got %+v", events)
	}
}

func TestSSEUsageIsMetered(t *testing.T) {
	f := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: message_start\n")
		fmt.Fprint(w, `data: {"type":"message_start","message":{"model":"test-model","usage":{"input_tokens":200000}}}`+"\n\n")
		fmt.Fprint(w, "event: message_delta\n")
		fmt.Fprint(w, `data: {"type":"message_delta","usage":{"output_tokens":50000}}`+"\n\n")
		fmt.Fprint(w, "event: message_delta\n")
		fmt.Fprint(w, `data: {"type":"message_delta","usage":{"output_tokens":100000}}`+"\n\n")
	}, 100)

	resp := call(t, f.srv, f.token)
	drain(t, resp) // reach EOF so the meter settles

	b, _ := f.ledger.Get("b1")
	// input 0.2M*10 = 2; output is cumulative, last delta wins: 0.1M*20 = 2
	if b.SpentUSD != 4 {
		t.Fatalf("spent = %v, want 4", b.SpentUSD)
	}
}

func TestHardStopReturns402(t *testing.T) {
	f := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"model":"test-model","usage":{"input_tokens":0,"output_tokens":400000}}`) // 8 USD
	}, 10)

	drain(t, call(t, f.srv, f.token)) // 8 of 10 spent
	drain(t, call(t, f.srv, f.token)) // post-paid: goes through, breach recorded

	resp := call(t, f.srv, f.token) // remaining <= 0 → hard stop
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402", resp.StatusCode)
	}
	events, err := f.audit.BySession(f.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if !hasKind(events, audit.KindBudgetExhausted) {
		t.Fatal("expected a budget.exhausted audit event")
	}
}

func TestBadTokenIsRejected(t *testing.T) {
	f := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream must not be reached with a bad token")
	}, 100)
	resp := call(t, f.srv, "pdk_not_a_real_token")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func hasKind(events []audit.Event, kind string) bool {
	for _, e := range events {
		if e.Kind == kind {
			return true
		}
	}
	return false
}
