package api

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"time"

	"github.com/viktorwelbers/paddock/internal/audit"
	"github.com/viktorwelbers/paddock/internal/sandbox"
)

func contextWithTimeout(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), d)
}

// maxWorkspaceBytes caps an upload. The sandbox volume has its own size
// limit; this stops the server streaming an unbounded body at it first.
const maxWorkspaceBytes = 2 << 30 // 2 GiB, matching the default volume

// workspaceTimeout bounds a transfer. Large repos over a slow link are
// legitimate, so it's generous rather than tight.
const workspaceTimeout = 10 * time.Minute

// pushWorkspace streams a gzipped tar from the developer's machine into the
// session's /workspace. The tar is never buffered: the request body is piped
// straight into `tar -xzf -` inside the pod.
//
// Extraction needs no path sanitisation here — it runs as the agent's own
// uid inside the agent's own container, so a hostile tar can only reach what
// the agent could already reach. (`paddock pull` is the dangerous direction,
// and that one extracts through os.Root.)
func (h *Handler) pushWorkspace(w http.ResponseWriter, r *http.Request) {
	sess, exec, ok := h.workspaceSession(w, r)
	if !ok {
		return
	}
	ctx, cancel := contextWithTimeout(r, workspaceTimeout)
	defer cancel()

	if err := exec.WaitRunning(ctx, sess.ID); err != nil {
		http.Error(w, "sandbox not ready: "+err.Error(), http.StatusConflict)
		return
	}

	if r.URL.Query().Get("clean") == "1" {
		// Replace rather than merge: the developer asked for their tree, not
		// their tree plus whatever the agent left behind.
		if err := exec.Exec(ctx, sess.ID, []string{"find", "/workspace", "-mindepth", "1", "-delete"}, nil, nil, nil); err != nil {
			http.Error(w, "clean workspace: "+err.Error(), http.StatusBadGateway)
			return
		}
	}

	body := http.MaxBytesReader(w, r.Body, maxWorkspaceBytes)
	counter := &countingHash{h: sha256.New()}
	stdin := io.TeeReader(body, counter)

	err := exec.Exec(ctx, sess.ID, []string{"tar", "-xzf", "-", "-C", "/workspace"}, stdin, nil, nil)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			http.Error(w, fmt.Sprintf("workspace exceeds the %d byte limit", maxWorkspaceBytes), http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "extract workspace: "+err.Error(), http.StatusBadGateway)
		return
	}

	sum := counter.sum()
	_ = h.Audit.Append(audit.Event{
		SessionID: sess.ID, Actor: sess.User, Kind: audit.KindWorkspacePush,
		Payload: map[string]any{"bytes": counter.n, "sha256": sum, "clean": r.URL.Query().Get("clean") == "1"},
	})
	writeJSON(w, http.StatusOK, map[string]any{"bytes": counter.n, "sha256": sum})
}

// pullWorkspace streams the session's /workspace back as a gzipped tar.
func (h *Handler) pullWorkspace(w http.ResponseWriter, r *http.Request) {
	sess, exec, ok := h.workspaceSession(w, r)
	if !ok {
		return
	}
	ctx, cancel := contextWithTimeout(r, workspaceTimeout)
	defer cancel()

	if err := exec.WaitRunning(ctx, sess.ID); err != nil {
		http.Error(w, "sandbox not ready: "+err.Error(), http.StatusConflict)
		return
	}

	w.Header().Set("Content-Type", "application/gzip")
	counter := &countingHash{h: sha256.New()}
	// Tee to the hash on the way out so the audit trail records exactly what
	// left the sandbox.
	out := io.MultiWriter(w, counter)

	if err := exec.Exec(ctx, sess.ID, []string{"tar", "-czf", "-", "-C", "/workspace", "."}, nil, out, nil); err != nil {
		// The header is already written by now if any bytes flowed, so
		// there's no status left to set — the truncated body and the audit
		// event are what the client has to go on.
		_ = h.Audit.Append(audit.Event{
			SessionID: sess.ID, Actor: sess.User, Kind: audit.KindWorkspacePull,
			Payload: map[string]any{"bytes": counter.n, "error": err.Error()},
		})
		return
	}
	_ = h.Audit.Append(audit.Event{
		SessionID: sess.ID, Actor: sess.User, Kind: audit.KindWorkspacePull,
		Payload: map[string]any{"bytes": counter.n, "sha256": counter.sum()},
	})
}

// workspaceSession resolves the session and the Execer, writing the error
// response itself when either is unavailable.
func (h *Handler) workspaceSession(w http.ResponseWriter, r *http.Request) (Session, sandbox.Execer, bool) {
	sess, err := h.Sessions.get(r.PathValue("id"))
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return Session{}, nil, false
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return Session{}, nil, false
	}
	if sess.Status != "running" {
		http.Error(w, fmt.Sprintf("session is %s", sess.Status), http.StatusConflict)
		return Session{}, nil, false
	}
	if h.Exec == nil {
		http.Error(w, "workspace transfer needs a cluster; this server runs without one", http.StatusNotImplemented)
		return Session{}, nil, false
	}
	return sess, h.Exec, true
}

// countingHash counts bytes and hashes them in one pass.
type countingHash struct {
	h hash.Hash
	n int64
}

func (c *countingHash) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return c.h.Write(p)
}

func (c *countingHash) sum() string { return hex.EncodeToString(c.h.Sum(nil)) }
