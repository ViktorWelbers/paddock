package api

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/viktorwelbers/paddock/internal/auth"
)

// metrics exposes Prometheus text-format gauges and counters, computed from
// the stores at scrape time rather than accumulated in memory. paddock already
// records every number worth watching — how many sandboxes run, what each
// budget has spent, and an append-only trail of every model call, egress
// decision, and reap — so the endpoint just reports what the databases hold,
// and nothing resets on a restart.
//
// It is authenticated, unlike /healthz and /readyz: the chart's ingress is a
// bare `/` prefix, so an unauthenticated metrics path would put budget spend
// on the open internet. Scrape it in-cluster with a bearer token.
func (h *Handler) metrics(w http.ResponseWriter, _ *http.Request) {
	var b strings.Builder

	byStatus, err := h.Sessions.countByStatus()
	if err != nil {
		http.Error(w, "metrics: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Emit the known terminal states even at zero, so a dashboard panel does
	// not blank out the moment nothing is failing or expiring.
	for _, st := range []string{statusRunning, statusFailed, statusExpired, statusDeleted} {
		if _, ok := byStatus[st]; !ok {
			byStatus[st] = 0
		}
	}
	writeMetric(&b, "paddock_sessions", "Sessions by status.", "gauge", "status", byStatus)

	byKind, err := h.Audit.CountByKind()
	if err != nil {
		http.Error(w, "metrics: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeMetric(&b, "paddock_events_total", "Audit events recorded, by kind.", "counter", "kind", byKind)

	budgets, err := h.Ledger.List()
	if err != nil {
		http.Error(w, "metrics: "+err.Error(), http.StatusInternalServerError)
		return
	}
	fmt.Fprint(&b, "# HELP paddock_budget_limit_usd Budget ceiling in USD.\n# TYPE paddock_budget_limit_usd gauge\n")
	for _, bd := range budgets {
		fmt.Fprintf(&b, "paddock_budget_limit_usd{budget=\"%s\"} %g\n", promLabel(bd.ID), bd.LimitUSD)
	}
	fmt.Fprint(&b, "# HELP paddock_budget_spent_usd Budget spent to date in USD.\n# TYPE paddock_budget_spent_usd gauge\n")
	for _, bd := range budgets {
		fmt.Fprintf(&b, "paddock_budget_spent_usd{budget=\"%s\"} %g\n", promLabel(bd.ID), bd.SpentUSD)
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Write([]byte(b.String()))
}

// writeMetric renders one labelled metric family in sorted key order, so a
// scrape is byte-stable and diffable.
func writeMetric[V int | int64](b *strings.Builder, name, help, typ, label string, vals map[string]V) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
	for _, k := range slices.Sorted(maps.Keys(vals)) {
		fmt.Fprintf(b, "%s{%s=\"%s\"} %d\n", name, label, promLabel(k), vals[k])
	}
}

// promLabel escapes the three characters a Prometheus label value may not
// carry raw. Statuses and kinds are controlled constants; budget ids are not,
// so this is the belt to that suspenders.
func promLabel(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
}

// readyz reports whether the server can actually serve, which liveness cannot:
// the process can be up while its database is unreachable, and a readiness
// probe that only checks the former keeps routing traffic into a server that
// will 500 every request. The kubelet sends no credentials, so this stays
// public and reveals only up or down.
func (h *Handler) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := h.Sessions.Ping(ctx); err != nil {
		http.Error(w, "not ready: database unreachable", http.StatusServiceUnavailable)
		return
	}
	fmt.Fprintln(w, "ok")
}

// countByStatus reports how many sessions sit in each status, for /metrics.
func (s *Store) countByStatus() (map[string]int, error) {
	rows, err := s.db.Query(`SELECT status, count(*) FROM sessions GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return nil, err
		}
		out[status] = n
	}
	return out, rows.Err()
}

// Ping reports whether the database answers, for the readiness probe.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// requestLogging writes one structured line per request: what was asked, how
// it ended, how long it took, and who asked. It sits inside the auth
// middleware, so the subject is already resolved by the time a request
// arrives; requests rejected before auth (a missing or bad token) never reach
// it, which is right — those are the auth layer's 401s, not application
// traffic, and it can account for them itself.
func requestLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		subject := "-"
		if id, ok := auth.FromContext(r.Context()); ok && id.Subject != "" {
			subject = id.Subject
		}
		slog.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"dur_ms", time.Since(start).Milliseconds(),
			"subject", subject,
		)
	})
}

// statusRecorder remembers the status code for the access log. It forwards
// Flush so a streaming response — a workspace tar on its way out — is not
// held back by the wrapper.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
