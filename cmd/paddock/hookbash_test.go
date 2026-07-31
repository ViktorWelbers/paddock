package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// chdir switches the working directory for a test and restores it after, so
// the .paddock/session lookup can be exercised against a temp tree.
func chdir(t *testing.T, dir string) {
	t.Helper()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
}

// decodeHook parses the hook's stdout and returns the rewritten command.
func decodeHook(t *testing.T, out []byte) hookResponse {
	t.Helper()
	var resp hookResponse
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("hook output is not valid JSON: %v (%s)", err, out)
	}
	return resp
}

// The whole point of the adapter: a Bash command becomes `paddock exec <id>
// --b64 <cmd>`, base64-wrapped so arbitrary shell survives the local shell that
// re-invokes paddock. The session comes from PADDOCK_SESSION here.
func TestHookBashRewritesUsingEnvSession(t *testing.T) {
	t.Setenv("PADDOCK_SESSION", "abc123")

	var out bytes.Buffer
	if err := hookBash(strings.NewReader(`{"tool_input":{"command":"git status"}}`), &out); err != nil {
		t.Fatal(err)
	}

	resp := decodeHook(t, out.Bytes())
	if resp.HookSpecificOutput.HookEventName != "PreToolUse" {
		t.Errorf("hookEventName = %q, want PreToolUse", resp.HookSpecificOutput.HookEventName)
	}
	want := "paddock exec abc123 --b64 " + base64.StdEncoding.EncodeToString([]byte("git status"))
	if got := resp.HookSpecificOutput.UpdatedInput.Command; got != want {
		t.Errorf("rewritten command = %q, want %q", got, want)
	}
}

// With no env var, the session is read from the nearest .paddock/session,
// walking up from the cwd.
func TestHookBashResolvesSessionFromFile(t *testing.T) {
	t.Setenv("PADDOCK_SESSION", "")
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".paddock"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".paddock", "session"), []byte("filesess\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	chdir(t, dir)

	var out bytes.Buffer
	if err := hookBash(strings.NewReader(`{"tool_input":{"command":"ls -la"}}`), &out); err != nil {
		t.Fatal(err)
	}
	got := decodeHook(t, out.Bytes()).HookSpecificOutput.UpdatedInput.Command
	if !strings.HasPrefix(got, "paddock exec filesess --b64 ") {
		t.Errorf("did not resolve session from .paddock/session: %q", got)
	}
}

// No bound session anywhere: the hook is a no-op (empty output), so Claude
// Code runs the command locally rather than the agent wedging.
func TestHookBashNoOpWithoutSession(t *testing.T) {
	t.Setenv("PADDOCK_SESSION", "")
	chdir(t, t.TempDir())

	var out bytes.Buffer
	if err := hookBash(strings.NewReader(`{"tool_input":{"command":"echo hi"}}`), &out); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Errorf("expected no rewrite without a session, got: %s", out.String())
	}
}

// Malformed hook input must not error — just leave the call untouched.
func TestHookBashIgnoresGarbage(t *testing.T) {
	t.Setenv("PADDOCK_SESSION", "abc123")
	var out bytes.Buffer
	if err := hookBash(strings.NewReader(`not json`), &out); err != nil {
		t.Fatalf("garbage input should be a no-op, got error: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("expected no output for garbage input, got: %s", out.String())
	}
}
