package api

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

func TestMetricsReportsSessionsSpendAndEvents(t *testing.T) {
	h := testHandler(t, Config{
		AgentImages: map[string]string{"claude": "img"},
		GatewayURL:  "http://gw",
	})
	// One running session, which also lands a session.created audit event.
	if rec := createSessionReq(t, h, "claude"); rec.Code != http.StatusCreated {
		t.Fatalf("create session: %d %s", rec.Code, rec.Body)
	}

	w := httptest.NewRecorder()
	h.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want Prometheus text format", ct)
	}

	body := w.Body.String()
	for _, want := range []string{
		"# TYPE paddock_sessions gauge",
		`paddock_sessions{status="running"} 1`,
		"# TYPE paddock_events_total counter",
		`paddock_events_total{kind="session.created"} 1`,
		"# TYPE paddock_budget_limit_usd gauge",
		`paddock_budget_limit_usd{budget="b1"} 10`,
		`paddock_budget_spent_usd{budget="b1"} 0`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q\n--- got ---\n%s", want, body)
		}
	}
}

// Terminal states are emitted even at zero, so a dashboard panel does not
// vanish the moment nothing is failing.
func TestMetricsEmitsZeroValuedStatuses(t *testing.T) {
	h := testHandler(t, Config{AgentImages: map[string]string{"claude": "img"}, GatewayURL: "http://gw"})
	w := httptest.NewRecorder()
	h.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	for _, want := range []string{
		`paddock_sessions{status="expired"} 0`,
		`paddock_sessions{status="failed"} 0`,
	} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("metrics missing zero series %q", want)
		}
	}
}

func TestReadyzOKWhenDatabaseAnswers(t *testing.T) {
	h := testHandler(t, Config{})
	w := httptest.NewRecorder()
	h.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/readyz", nil))
	if w.Code != http.StatusOK {
		t.Errorf("readyz = %d, want 200 while the db is up", w.Code)
	}
}

// The ingress is a bare `/` prefix, so exposure is decided here: /metrics
// carries budget spend and must stay authenticated; /readyz is up-or-down and
// must stay public for a credential-less kubelet probe.
func TestMetricsIsAuthenticatedAndReadyzIsPublic(t *testing.T) {
	if slices.Contains(publicPaths, "/metrics") {
		t.Error("/metrics must not be public: a bare-/ ingress would put budget spend on the open internet")
	}
	if !slices.Contains(publicPaths, "/readyz") {
		t.Error("/readyz must be public: the kubelet probe sends no bearer token")
	}
}
