package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/viktorwelbers/paddock/internal/audit"
)

// gitCredentialTimeout bounds the injection. It is a keypair generation or a
// few hundred bytes over an exec stream; if it takes this long, something is
// wrong.
const gitCredentialTimeout = 2 * time.Minute

// maxGitHosts caps how many hosts one injection may name, so a malformed
// client cannot stream an unbounded audit payload.
const maxGitHosts = 64

// maxRecipient bounds the keygen output we read back. An age recipient is one
// short line (~62 bytes); anything larger is a malfunction, not a key.
const maxRecipient = 4 << 10

// keyPath is where the session's age identity lives inside the pod. It is
// generated on demand, readable only by the agent uid, and dies with the
// pod's filesystem — it is never written to the pod spec, etcd, or the image.
const keyPath = `"$HOME/.paddock/git.key"`

// The credential handoff is encrypted end to end, and this is the whole reason
// it exists rather than the server simply writing ~/.git-credentials itself.
//
// paddock runs the cluster, so a workspace, an attach, and this injection all
// pass through the server and the kube API server's exec channel. A git token
// streamed in the clear would therefore sit, however briefly, in server
// memory and in whatever records that channel (API-server audit logs, a
// sidecar, a proxy). For provider keys paddock's answer is that they never
// enter a sandbox at all; a git credential must, because the agent works as
// the developer — but it does not follow that the *control plane* should see
// it too.
//
// So the plaintext never transits paddock. The pod generates an age keypair
// and keeps the private half; the CLI encrypts to the public half; the server
// moves only ciphertext. The boundary this draws is passive exposure: the
// token is not captured in transit or at rest in the control plane. It is not
// a boundary against paddock itself — paddock can exec into the pod and read
// what the agent reads, which is inherent to operating the sandbox and no
// worse than before. Repo-scoped, short-lived tokens remain the answer to
// "the agent can use what it can read", and the docs say so.

// gitRecipient returns the session pod's age recipient, generating the keypair
// on first call. The recipient is public; only it and the later ciphertext
// ever cross the exec channel.
func (h *Handler) gitRecipient(w http.ResponseWriter, r *http.Request) {
	sess, exec, ok := h.workspaceSession(w, r)
	if !ok {
		return
	}

	ctx, cancel := contextWithTimeout(r, gitCredentialTimeout)
	defer cancel()
	if err := exec.WaitRunning(ctx, sess.ID); err != nil {
		http.Error(w, "sandbox not ready: "+err.Error(), http.StatusConflict)
		return
	}

	// Idempotent: keep the identity if it already exists, so a second `run`
	// or a retried request does not rotate the key out from under a
	// credential the developer just encrypted to it. `age-keygen -y` derives
	// the recipient from the identity, so the private key stays put.
	const gen = `set -eu; umask 077; mkdir -p "$HOME/.paddock"; ` +
		`[ -f ` + keyPath + ` ] || age-keygen -o ` + keyPath + ` >/dev/null 2>&1; ` +
		`age-keygen -y ` + keyPath
	var out bytes.Buffer
	if err := exec.Exec(ctx, sess.ID, []string{"sh", "-c", gen},
		nil, &limitedRecipientWriter{buf: &out}, nil); err != nil {
		http.Error(w, "generate session key: "+err.Error(), http.StatusBadGateway)
		return
	}
	recipient := strings.TrimSpace(out.String())
	if !strings.HasPrefix(recipient, "age1") {
		http.Error(w, "sandbox returned no usable key", http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"recipient": recipient})
}

// injectGitCredentials decrypts the developer's git credentials inside their
// sandbox. The body carries ciphertext the pod alone can open, plus the host
// list in the clear for the audit trail — hosts are not the secret, the token
// is, and the token stays inside the ciphertext.
func (h *Handler) injectGitCredentials(w http.ResponseWriter, r *http.Request) {
	sess, exec, ok := h.workspaceSession(w, r)
	if !ok {
		return
	}

	var req struct {
		Hosts      []string `json:"hosts"`
		Ciphertext string   `json:"ciphertext"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Ciphertext) == "" {
		http.Error(w, "no ciphertext given", http.StatusBadRequest)
		return
	}
	if len(req.Hosts) > maxGitHosts {
		http.Error(w, fmt.Sprintf("at most %d hosts per injection", maxGitHosts), http.StatusBadRequest)
		return
	}

	ctx, cancel := contextWithTimeout(r, gitCredentialTimeout)
	defer cancel()
	if err := exec.WaitRunning(ctx, sess.ID); err != nil {
		http.Error(w, "sandbox not ready: "+err.Error(), http.StatusConflict)
		return
	}

	// `age -d` reads the armored ciphertext from stdin and writes the
	// plaintext credential file straight to the agent's home; the server
	// never sees the cleartext. umask 077 so the file is never even briefly
	// world-readable. `store` is git's own helper, so nothing paddock-specific
	// need be present in the agent image beyond age itself.
	const install = `set -eu; umask 077; ` +
		`age -d -i ` + keyPath + ` > "$HOME/.git-credentials"; ` +
		`git config --global credential.helper store`
	if err := exec.Exec(ctx, sess.ID, []string{"sh", "-c", install},
		strings.NewReader(req.Ciphertext), nil, nil); err != nil {
		http.Error(w, "install git credentials: "+err.Error(), http.StatusBadGateway)
		return
	}

	h.Audit.Record(audit.Event{
		SessionID: sess.ID, Actor: sess.User, Kind: audit.KindGitCredentials,
		Payload: map[string]any{"hosts": req.Hosts, "count": len(req.Hosts)},
	})
	writeJSON(w, http.StatusOK, map[string]any{"hosts": req.Hosts})
}

// limitedRecipientWriter captures the first maxRecipient bytes of keygen
// output and drops the rest, so a misbehaving pod cannot make the server
// buffer without bound.
type limitedRecipientWriter struct {
	buf *bytes.Buffer
}

func (l *limitedRecipientWriter) Write(p []byte) (int, error) {
	if room := maxRecipient - l.buf.Len(); room > 0 {
		keep := p
		if len(keep) > room {
			keep = keep[:room]
		}
		l.buf.Write(keep)
	}
	return len(p), nil
}
