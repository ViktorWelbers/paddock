package main

import (
	"os/exec"
	"testing"
)

func TestIdentityLabel(t *testing.T) {
	cases := []struct {
		name, email, want string
	}{
		{"Ada Lovelace", "ada@example.com", "Ada Lovelace <ada@example.com>"},
		{"Ada Lovelace", "", "Ada Lovelace"},
		{"", "ada@example.com", "ada@example.com"},
	}
	for _, c := range cases {
		if got := identityLabel(c.name, c.email); got != c.want {
			t.Errorf("identityLabel(%q, %q) = %q, want %q", c.name, c.email, got, c.want)
		}
	}
}

// A repo with a local git identity is harvested the same way signing config
// is: straight from git's own config resolution, nothing paddock-specific.
func TestGitConfigGetReadsIdentity(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	git(t, dir, "init", "-q")
	git(t, dir, "config", "user.name", "Ada Lovelace")
	git(t, dir, "config", "user.email", "ada@example.com")

	if got := gitConfigGet(dir, "user.name"); got != "Ada Lovelace" {
		t.Errorf("user.name = %q, want Ada Lovelace", got)
	}
	if got := gitConfigGet(dir, "user.email"); got != "ada@example.com" {
		t.Errorf("user.email = %q, want ada@example.com", got)
	}
}

// A repo with no identity configured yields nothing to send.
func TestGitConfigGetSilentWithoutIdentity(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	git(t, dir, "init", "-q")

	if got := gitConfigGet(dir, "user.name"); got != "" {
		t.Errorf("user.name = %q, want empty for an unconfigured repo", got)
	}
}
