package api

import (
	"encoding/json"
	"net/http"

	"github.com/viktorwelbers/paddock/internal/audit"
)

// maxIdentityFieldLen bounds name/email so a malformed client cannot stream
// an unbounded audit payload or argv entry.
const maxIdentityFieldLen = 256

// configureGitIdentity sets the developer's git user.name/user.email inside
// their sandbox, so a commit the agent makes is attributed to the developer
// rather than whatever git falls back to once nothing is configured. Unlike
// the credential and signing handoffs, a name and an email are not secrets —
// the repo's own commit history already shows them — so this arrives as
// plain JSON with no age encryption and no recipient round trip.
func (h *Handler) configureGitIdentity(w http.ResponseWriter, r *http.Request) {
	sess, exec, ok := h.workspaceSession(w, r)
	if !ok {
		return
	}

	var req struct {
		Name  string `json:"name"`
		Email string `json:"email"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.Name == "" && req.Email == "" {
		http.Error(w, "name or email is required", http.StatusBadRequest)
		return
	}
	if len(req.Name) > maxIdentityFieldLen || len(req.Email) > maxIdentityFieldLen {
		http.Error(w, "name/email too long", http.StatusBadRequest)
		return
	}

	ctx, cancel := contextWithTimeout(r, gitCredentialTimeout)
	defer cancel()
	if err := exec.WaitRunning(ctx, sess.ID); err != nil {
		http.Error(w, "sandbox not ready: "+err.Error(), http.StatusConflict)
		return
	}

	// $0 is a label; name and email follow as positional args, never
	// interpolated into the script, so nothing about their content is
	// shell-parsed. An empty value leaves that half of the identity alone
	// rather than clobbering it with "".
	args := []string{"sh", "-c", installGitIdentity, "git-identity", req.Name, req.Email}
	if err := exec.Exec(ctx, sess.ID, args, nil, nil, nil); err != nil {
		http.Error(w, "configure git identity: "+err.Error(), http.StatusBadGateway)
		return
	}

	h.Audit.Record(audit.Event{
		SessionID: sess.ID, Actor: sess.User, Kind: audit.KindGitIdentity,
		Payload: map[string]any{"name": req.Name, "email": req.Email},
	})
	writeJSON(w, http.StatusOK, map[string]any{"name": req.Name, "email": req.Email})
}

// installGitIdentity sets whichever of name/email arrived. $1 name, $2 email.
const installGitIdentity = `set -eu
if [ -n "$1" ]; then git config --global user.name "$1"; fi
if [ -n "$2" ]; then git config --global user.email "$2"; fi`
