package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/viktorwelbers/paddock/internal/audit"
)

// gitSigningRequest carries a commit-signing setup into a sandbox. The private
// key material is inside Ciphertext, sealed to the pod's age key exactly like
// the credential handoff; everything else is non-secret configuration and the
// key id, which for both git.format kinds is public (a gpg long id or an ssh
// fingerprint) and is recorded only for the audit trail.
type gitSigningRequest struct {
	Format     string `json:"format"`      // "ssh" | "openpgp"
	KeyID      string `json:"key_id"`      // gpg key id (set as user.signingkey) or ssh fingerprint (audit only)
	CommitSign bool   `json:"commit_sign"` // git config commit.gpgsign
	TagSign    bool   `json:"tag_sign"`    // git config tag.gpgsign
	Ciphertext string `json:"ciphertext"`  // age-armored signing key material
}

// gpgKeyID is deliberately strict: the value becomes `user.signingkey`, and
// though it is passed to the shell as a positional argument (never
// interpolated into the script), a hex id / fingerprint / email is all a real
// key id ever is, so anything else is a client bug worth rejecting loudly.
var gpgKeyID = regexp.MustCompile(`^[A-Za-z0-9._@+/-]{1,256}$`)

// configureGitSigning installs the developer's own signing key into their
// sandbox so the agent's commits are signed as the developer — the same trust
// model as the credential handoff, and the same encryption: the key material
// crosses the control plane only as ciphertext the pod alone can open.
//
// A signing key is a heavier thing to lend than a git token — an OpenPGP
// identity can sign releases and mail for years — which is why the CLI hands
// over only a signing *subkey* for gpg, why it is opt-out (`--no-git-signing`),
// and why paddock records that it happened. What lands in the pod can sign as
// the developer; it cannot, for gpg, stand in for the master key. Repo policy
// (required signatures, key expiry) remains the real backstop, and the docs
// say so.
func (h *Handler) configureGitSigning(w http.ResponseWriter, r *http.Request) {
	sess, exec, ok := h.workspaceSession(w, r)
	if !ok {
		return
	}

	var req gitSigningRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Ciphertext) == "" {
		http.Error(w, "no ciphertext given", http.StatusBadRequest)
		return
	}

	var script string
	// $0 is a label; the dynamic values follow as positional args so the shell
	// never parses them as anything but data.
	args := []string{"sh", "-c", "", "git-signing"}
	switch req.Format {
	case "ssh":
		script = installSSHSigning
		args = append(args, strconv.FormatBool(req.CommitSign), strconv.FormatBool(req.TagSign))
	case "openpgp":
		if !gpgKeyID.MatchString(req.KeyID) {
			http.Error(w, "openpgp signing needs a valid key_id", http.StatusBadRequest)
			return
		}
		script = installGPGSigning
		args = append(args, req.KeyID, strconv.FormatBool(req.CommitSign), strconv.FormatBool(req.TagSign))
	default:
		http.Error(w, fmt.Sprintf("unsupported signing format %q", req.Format), http.StatusBadRequest)
		return
	}
	args[2] = script

	ctx, cancel := contextWithTimeout(r, gitCredentialTimeout)
	defer cancel()
	if err := exec.WaitRunning(ctx, sess.ID); err != nil {
		http.Error(w, "sandbox not ready: "+err.Error(), http.StatusConflict)
		return
	}
	if err := exec.Exec(ctx, sess.ID, args, strings.NewReader(req.Ciphertext), nil, nil); err != nil {
		http.Error(w, "install signing key: "+err.Error(), http.StatusBadGateway)
		return
	}

	h.Audit.Record(audit.Event{
		SessionID: sess.ID, Actor: sess.User, Kind: audit.KindGitSigning,
		Payload: map[string]any{"format": req.Format, "key_id": req.KeyID, "commit_sign": req.CommitSign},
	})
	writeJSON(w, http.StatusOK, map[string]any{"format": req.Format, "key_id": req.KeyID})
}

// installSSHSigning decrypts the private key to ~/.ssh, derives its public half
// (ssh signing wants both present), and points git at it. $1 commit.gpgsign,
// $2 tag.gpgsign. A passphrase-protected key cannot be derived non-
// interactively — hence the `|| true`; signing then still works if the key is
// passphraseless, which is what a dedicated signing key should be.
const installSSHSigning = `set -eu
umask 077
mkdir -p "$HOME/.ssh"
age -d -i ` + keyPath + ` > "$HOME/.ssh/paddock_signing"
ssh-keygen -y -f "$HOME/.ssh/paddock_signing" > "$HOME/.ssh/paddock_signing.pub" 2>/dev/null || true
git config --global gpg.format ssh
git config --global user.signingkey "$HOME/.ssh/paddock_signing"
git config --global commit.gpgsign "$1"
git config --global tag.gpgsign "$2"`

// installGPGSigning imports the secret subkey and points git at the key id.
// $1 key id, $2 commit.gpgsign, $3 tag.gpgsign.
const installGPGSigning = `set -eu
umask 077
age -d -i ` + keyPath + ` | gpg --batch --import 2>/dev/null
git config --global gpg.format openpgp
git config --global user.signingkey "$1"
git config --global commit.gpgsign "$2"
git config --global tag.gpgsign "$3"`
