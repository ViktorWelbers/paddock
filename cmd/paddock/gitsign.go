package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// gitSigning is a harvested commit-signing setup: the non-secret config plus
// the private key material that will be sealed to the pod before it travels.
type gitSigning struct {
	Format     string // "ssh" | "openpgp"
	KeyID      string // gpg key id, or an ssh fingerprint for the audit line
	CommitSign bool
	TagSign    bool
	Material   []byte // secret: the ssh private key, or the armored gpg secret subkeys
}

// harvestGitSigning reads how the developer signs commits and collects the key
// to do it with — from git's own config and the developer's own keyring, the
// same principle as the credential harvest: if `git commit -S` works on the
// laptop, it should work in the sandbox, with nothing new to configure.
//
// It is quiet when there is nothing to do: no signing key configured, an
// ssh key that lives only in an agent, or a gpg key gpg will not export all
// yield nil, and `paddock run` carries on unsigned rather than failing.
func harvestGitSigning(dir string) *gitSigning {
	signingKey := gitConfigGet(dir, "user.signingkey")
	if signingKey == "" {
		return nil // the developer does not sign; nothing to carry
	}
	s := &gitSigning{
		Format:     gitConfigGet(dir, "gpg.format"),
		CommitSign: gitConfigGet(dir, "commit.gpgsign") == "true",
		TagSign:    gitConfigGet(dir, "tag.gpgsign") == "true",
	}
	if s.Format == "" {
		s.Format = "openpgp" // git's own default when gpg.format is unset
	}

	switch s.Format {
	case "ssh":
		path, ok := sshPrivateKeyPath(signingKey)
		if !ok {
			return nil // a literal key or an agent-only key: no file to send
		}
		material, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		s.Material = material
		s.KeyID = sshFingerprint(path) // best-effort, for the audit line only
	case "openpgp":
		material, ok := exportGPGSubkeys(signingKey)
		if !ok {
			return nil // gpg absent, or nothing exportable for this id
		}
		s.Material = material
		s.KeyID = signingKey
	default:
		return nil // x509 and anything else: not handled yet
	}
	return s
}

// sshPrivateKeyPath resolves user.signingkey to a private key file, or reports
// that there is none to send. A literal public key or an `sk-`/`key::` form
// authenticates through an agent or hardware, which is a different mechanism —
// paddock does not forward agents (yet), so those are skipped rather than
// half-supported.
func sshPrivateKeyPath(signingKey string) (string, bool) {
	if strings.HasPrefix(signingKey, "ssh-") ||
		strings.HasPrefix(signingKey, "sk-") ||
		strings.HasPrefix(signingKey, "key::") {
		return "", false
	}
	path := expandHome(signingKey)
	// user.signingkey often points at the .pub; the private key sits beside it.
	path = strings.TrimSuffix(path, ".pub")
	if fi, err := os.Stat(path); err != nil || fi.IsDir() {
		return "", false
	}
	return path, true
}

// sshFingerprint returns the key's SHA256 fingerprint for the audit trail, or
// "" if ssh-keygen cannot read it (e.g. a passphrase-protected key). The
// fingerprint is public; the key never is.
func sshFingerprint(path string) string {
	out, err := exec.Command("ssh-keygen", "-lf", path).Output()
	if err != nil {
		return ""
	}
	fields := strings.Fields(string(out))
	if len(fields) >= 2 {
		return fields[1] // "SHA256:..."
	}
	return ""
}

// exportGPGSubkeys exports the secret *subkeys* for a key id — not the master
// secret, which stays on the laptop. That is the whole point: the sandbox gets
// enough to sign and no more.
func exportGPGSubkeys(keyID string) ([]byte, bool) {
	cmd := exec.Command("gpg", "--batch", "--armor", "--export-secret-subkeys", keyID)
	out, err := cmd.Output()
	if err != nil || len(bytes.TrimSpace(out)) == 0 {
		return nil, false
	}
	return out, true
}

// gitConfigGet reads one config value, returning "" when it is unset or this
// is not a repo. Signing config is read from the repo scope down, so a
// per-repo signing key wins over the global one, exactly as git resolves it.
func gitConfigGet(dir, key string) string {
	out, err := exec.Command("git", "-C", dir, "config", "--get", key).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func expandHome(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(path, "~"))
		}
	}
	return path
}

// sendGitSigning seals the key material to the pod and installs it.
func sendGitSigning(sessionID string, s *gitSigning) error {
	recipient, err := gitRecipient(sessionID)
	if err != nil {
		return err
	}
	ciphertext, err := encryptToRecipient(string(s.Material), recipient)
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]any{
		"format":      s.Format,
		"key_id":      s.KeyID,
		"commit_sign": s.CommitSign,
		"tag_sign":    s.TagSign,
		"ciphertext":  ciphertext,
	})
	if err != nil {
		return err
	}
	req, err := apiRequest(http.MethodPost, "/v1/sessions/"+sessionID+"/git-signing", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := apiDo(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("%s: %s", resp.Status, bytes.TrimSpace(raw))
	}
	return nil
}

// pushGitSigning harvests, seals, and installs the signing setup, reporting
// what it did without ever naming the secret. For gpg it says plainly that a
// signing subkey left the machine, since a signing identity is a heavier loan
// than a token.
func pushGitSigning(sessionID, dir string) error {
	s := harvestGitSigning(dir)
	if s == nil {
		return nil // nothing configured, or nothing sendable
	}
	if err := sendGitSigning(sessionID, s); err != nil {
		return err
	}
	switch s.Format {
	case "ssh":
		fmt.Printf("commit signing configured (ssh %s)\n", s.KeyID)
	case "openpgp":
		fmt.Printf("commit signing configured (openpgp %s)\n", s.KeyID)
		fmt.Fprintf(os.Stderr,
			"note: an encrypted copy of this key's signing subkey now lives in the sandbox; "+
				"use --no-git-signing to skip, and prefer a passphraseless signing subkey\n")
	}
	return nil
}
