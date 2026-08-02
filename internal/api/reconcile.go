package api

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/viktorwelbers/paddock/internal/audit"
)

// Reconcile brings the cluster and the session store back into agreement.
//
// They drift because they are two databases with no transaction between
// them. The control plane can die between creating a session row and
// creating its pod, or between deleting a pod and updating the row; on
// ephemeral storage the store can come back empty while every sandbox it
// created is still running. Both directions of drift cost the operator
// something real, and neither announces itself:
//
//   - A pod with no running session is nobody's: it holds its CPU and memory
//     indefinitely, `paddock ls` does not show it, and `paddock rm` answers
//     404. The only way out was kubectl, which is the tool paddock exists to
//     keep out of the developer's hands. These get reaped.
//
//   - A session marked running with no pod behind it is a lie the API keeps
//     telling: attach hangs, workspace push fails obscurely, and the row sits
//     in `paddock ls` looking healthy. These get marked failed, so the
//     session says what it is.
//
// Run at startup, before serving: afterwards a missing pod is just as likely
// to be a session created a millisecond ago.
func (h *Handler) Reconcile(ctx context.Context) error {
	live, err := h.Provisioner.List(ctx)
	if err != nil {
		return fmt.Errorf("list sandboxes: %w", err)
	}
	sessions, err := h.Sessions.list()
	if err != nil {
		return fmt.Errorf("list sessions: %w", err)
	}

	running := map[string]Session{}
	for _, s := range sessions {
		if s.Status == statusRunning {
			running[s.ID] = s
		}
	}

	var reaped, orphaned int
	for _, id := range live {
		if _, ok := running[id]; ok {
			continue
		}
		if err := h.Provisioner.Delete(ctx, id); err != nil {
			// Keep going: one stuck sandbox should not strand the rest.
			slog.Error("reconcile: could not reap orphaned sandbox", "session", id, "error", err)
			continue
		}
		reaped++
		slog.Warn("reconcile: reaped orphaned sandbox", "session", id)
		h.Audit.Record(audit.Event{
			SessionID: id, Actor: "paddock", Kind: audit.KindSandboxReaped,
			Payload: map[string]any{"reason": "no running session owns this sandbox"},
		})
	}

	for id, sess := range running {
		if slices.Contains(live, id) {
			continue
		}
		if err := h.Sessions.setStatus(id, statusFailed); err != nil {
			slog.Error("reconcile: could not mark session failed", "session", id, "error", err)
			continue
		}
		orphaned++
		slog.Warn("reconcile: session has no sandbox, marked failed", "session", id)
		h.Audit.Record(audit.Event{
			SessionID: id, Actor: "paddock", Kind: audit.KindSessionOrphaned,
			Payload: map[string]any{"user": sess.User, "reason": "sandbox is gone"},
		})
	}

	slog.Info("reconciled sandboxes",
		"live", len(live), "running_sessions", len(running),
		"reaped", reaped, "marked_failed", orphaned)
	return nil
}

// ReconcileGrace is how long a running session is left alone before the live
// reconcile will call its missing sandbox dead — long enough for a pod to be
// scheduled and pull its image, so a seconds-old session is never mistaken for a
// stranded one.
const ReconcileGrace = 3 * time.Minute

// ReconcileLive is the periodic counterpart to Reconcile: it notices a session
// whose sandbox has *died under it* — evicted (workspace over quota), OOMed,
// crashed, or deleted out of band — which the startup Reconcile only catches on
// the next boot. Keyed off Healthy (Running/Pending pods), so a Failed-phase pod
// that still exists as an object does not keep a dead session looking alive. Any
// remnants (the Failed pod, its NetworkPolicy) are torn down and the session is
// marked failed, so it stops claiming to run.
func (h *Handler) ReconcileLive(ctx context.Context, grace time.Duration) (int, error) {
	healthy, err := h.Provisioner.Healthy(ctx)
	if err != nil {
		return 0, fmt.Errorf("list healthy sandboxes: %w", err)
	}
	up := map[string]bool{}
	for _, id := range healthy {
		up[id] = true
	}
	sessions, err := h.Sessions.list()
	if err != nil {
		return 0, fmt.Errorf("list sessions: %w", err)
	}
	cutoff := time.Now().Add(-grace)

	var failed int
	for _, s := range sessions {
		if s.Status != statusRunning || up[s.ID] || !s.CreatedAt.Before(cutoff) {
			continue
		}
		// Best-effort cleanup of whatever remnants are left (a Failed pod, a
		// stray NetworkPolicy), then tell the truth about the session.
		if err := h.Provisioner.Delete(ctx, s.ID); err != nil {
			slog.Error("live-reconcile: could not tear down dead sandbox", "session", s.ID, "error", err)
		}
		if err := h.Sessions.setStatus(s.ID, statusFailed); err != nil {
			slog.Error("live-reconcile: could not mark session failed", "session", s.ID, "error", err)
			continue
		}
		failed++
		slog.Warn("live-reconcile: sandbox died under a running session, marked failed", "session", s.ID, "user", s.User)
		h.Audit.Record(audit.Event{
			SessionID: s.ID, Actor: "paddock", Kind: audit.KindSessionOrphaned,
			Payload: map[string]any{"user": s.User, "reason": "sandbox not running (evicted/failed/gone)"},
		})
	}
	if failed > 0 {
		slog.Info("live-reconcile marked sessions failed", "count", failed)
	}
	return failed, nil
}

// ReapExpired ends sessions older than maxAge: the sandbox is torn down and
// the row moves to `expired`, which also stops its token working, since
// ByToken only serves running sessions. maxAge <= 0 disables it and the
// method is a no-op.
//
// This is the half of the lifecycle the drift reconcile above does not cover.
// A sandbox nobody deletes runs forever — holding the CPU and memory the
// operator pays for, and keeping a working session token alive the whole
// time, which for a table full of credentials is precisely the standing
// exposure paddock argues against. An absolute cap is deliberately blunt: a
// coding sandbox is ephemeral by premise, and an idle-based cap would need
// per-session activity tracking that does not exist yet (see ROADMAP).
//
// Unlike Reconcile, this is safe to run on a ticker: it keys off wall-clock
// age, not the momentary presence of a pod, so a session created a
// millisecond ago is never mistaken for a stranded one.
func (h *Handler) ReapExpired(ctx context.Context, maxAge time.Duration) (int, error) {
	if maxAge <= 0 {
		return 0, nil
	}
	sessions, err := h.Sessions.list()
	if err != nil {
		return 0, fmt.Errorf("list sessions: %w", err)
	}
	cutoff := time.Now().Add(-maxAge)

	var reaped int
	for _, s := range sessions {
		if s.Status != statusRunning || !s.CreatedAt.Before(cutoff) {
			continue
		}
		if err := h.Provisioner.Delete(ctx, s.ID); err != nil {
			// Keep going: one stuck sandbox should not spare the rest their TTL.
			slog.Error("reap: could not tear down expired sandbox", "session", s.ID, "error", err)
			continue
		}
		if err := h.Sessions.setStatus(s.ID, statusExpired); err != nil {
			// The pod is gone but the row still says running: reconcile will
			// catch that on the next restart. Better than dropping the loop.
			slog.Error("reap: sandbox torn down but session not marked expired", "session", s.ID, "error", err)
			continue
		}
		reaped++
		slog.Info("reaped expired session",
			"session", s.ID, "user", s.User, "age", time.Since(s.CreatedAt).Round(time.Second).String())
		h.Audit.Record(audit.Event{
			SessionID: s.ID, Actor: "paddock", Kind: audit.KindSessionExpired,
			Payload: map[string]any{
				"user":            s.User,
				"age_seconds":     int64(time.Since(s.CreatedAt).Seconds()),
				"max_age_seconds": int64(maxAge.Seconds()),
			},
		})
	}
	if reaped > 0 {
		slog.Info("reaped expired sessions", "count", reaped, "max_age", maxAge.String())
	}
	return reaped, nil
}

// ReapIdle ends running sessions with no activity for longer than idle. Where
// ReapExpired caps absolute lifetime, this reclaims sessions that are simply no
// longer in use — the local-harness supervisor stops heartbeating when the
// harness closes, crashes, or the machine sleeps, so a forgotten sandbox is
// cleaned up on its own without the developer removing anything. idle <= 0
// disables it. Like ReapExpired it keys off a stored timestamp, so it is safe
// to run on a ticker.
func (h *Handler) ReapIdle(ctx context.Context, idle time.Duration) (int, error) {
	if idle <= 0 {
		return 0, nil
	}
	sessions, err := h.Sessions.list()
	if err != nil {
		return 0, fmt.Errorf("list sessions: %w", err)
	}
	cutoff := time.Now().Add(-idle)

	var reaped int
	for _, s := range sessions {
		last := s.LastActive
		if last.IsZero() {
			last = s.CreatedAt
		}
		if s.Status != statusRunning || !last.Before(cutoff) {
			continue
		}
		if err := h.Provisioner.Delete(ctx, s.ID); err != nil {
			slog.Error("reap: could not tear down idle sandbox", "session", s.ID, "error", err)
			continue
		}
		if err := h.Sessions.setStatus(s.ID, statusExpired); err != nil {
			slog.Error("reap: idle sandbox torn down but session not marked expired", "session", s.ID, "error", err)
			continue
		}
		reaped++
		slog.Info("reaped idle session",
			"session", s.ID, "user", s.User, "idle", time.Since(last).Round(time.Second).String())
		h.Audit.Record(audit.Event{
			SessionID: s.ID, Actor: "paddock", Kind: audit.KindSessionExpired,
			Payload: map[string]any{
				"user":             s.User,
				"reason":           "idle",
				"idle_seconds":     int64(time.Since(last).Seconds()),
				"max_idle_seconds": int64(idle.Seconds()),
			},
		})
	}
	if reaped > 0 {
		slog.Info("reaped idle sessions", "count", reaped, "max_idle", idle.String())
	}
	return reaped, nil
}
