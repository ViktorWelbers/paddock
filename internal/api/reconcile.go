package api

import (
	"context"
	"fmt"
	"log/slog"
	"slices"

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
