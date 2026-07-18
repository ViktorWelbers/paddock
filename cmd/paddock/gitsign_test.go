package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestSSHPrivateKeyPathResolvesAndSkips(t *testing.T) {
	dir := t.TempDir()
	priv := filepath.Join(dir, "id_sign")
	if err := os.WriteFile(priv, []byte("PRIVATE"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(priv+".pub", []byte("ssh-ed25519 AAAA"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A .pub path resolves to the private key beside it.
	if got, ok := sshPrivateKeyPath(priv + ".pub"); !ok || got != priv {
		t.Errorf("from .pub = (%q, %v), want (%q, true)", got, ok, priv)
	}
	// A private key path resolves to itself.
	if got, ok := sshPrivateKeyPath(priv); !ok || got != priv {
		t.Errorf("from priv = (%q, %v), want (%q, true)", got, ok, priv)
	}
	// A literal key or agent/hardware key has no file to send.
	for _, literal := range []string{"ssh-ed25519 AAAAC3Nz", "key::ssh-ed25519 AAAA", "sk-ssh-ed25519@openssh.com AAAA"} {
		if _, ok := sshPrivateKeyPath(literal); ok {
			t.Errorf("literal %q should be skipped (agent/hardware, no file)", literal)
		}
	}
	// A path that does not exist is not sendable.
	if _, ok := sshPrivateKeyPath(filepath.Join(dir, "nope")); ok {
		t.Error("a missing key path should be skipped")
	}
}

// A repo with an ssh signing key configured is harvested with the material
// read from disk and the format defaulted correctly.
func TestHarvestGitSigningSSH(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	priv := filepath.Join(dir, "id_sign")
	if err := os.WriteFile(priv, []byte("PRIVATE-KEY-BYTES"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "init", "-q")
	git(t, dir, "config", "gpg.format", "ssh")
	git(t, dir, "config", "user.signingkey", priv)
	git(t, dir, "config", "commit.gpgsign", "true")

	s := harvestGitSigning(dir)
	if s == nil {
		t.Fatal("expected a harvested signing setup")
	}
	if s.Format != "ssh" {
		t.Errorf("format = %q, want ssh", s.Format)
	}
	if !s.CommitSign {
		t.Error("commit_sign should be true")
	}
	if string(s.Material) != "PRIVATE-KEY-BYTES" {
		t.Errorf("material = %q, want the private key bytes", s.Material)
	}
}

// No signing key configured means nothing to carry — run stays unsigned.
func TestHarvestGitSigningSilentWithoutKey(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	git(t, dir, "init", "-q")
	if s := harvestGitSigning(dir); s != nil {
		t.Errorf("expected nil for a repo with no signing key, got %+v", s)
	}
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
