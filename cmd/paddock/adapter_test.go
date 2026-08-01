package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSelectAdapter(t *testing.T) {
	if a, err := selectAdapter("claude"); err != nil || a.name() != "claude" {
		t.Errorf("claude: %v %v", a, err)
	}
	if a, err := selectAdapter("opencode"); err != nil || a.name() != "opencode" {
		t.Errorf("opencode: %v %v", a, err)
	}
	if _, err := selectAdapter("nope"); err == nil {
		t.Error("unknown harness should error")
	}
}

// The opencode plugin must rewrite bash into the sandbox and, by default, block
// the ungoverned webfetch tool.
func TestOpencodePluginContent(t *testing.T) {
	p := opencodePlugin(false)
	for _, want := range []string{
		`"tool.execute.before"`, `input.tool === "bash"`, "paddock exec", "--b64",
		`input.tool === "webfetch"`, "paddock hook-session-start",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("opencode plugin missing %q", want)
		}
	}
	if strings.Contains(opencodePlugin(true), "webfetch") {
		t.Error("--allow-web-tools should omit the webfetch block")
	}
}

func TestClaudeAdapterInstall(t *testing.T) {
	chdir(t, t.TempDir())
	if err := (claudeAdapter{}).install(installOpts{}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(".claude", "settings.local.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s map[string]any
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatalf("settings not valid JSON: %v", err)
	}
	js, _ := json.Marshal(s)
	for _, want := range []string{"hook-session-start", "hook-session-end", "hook-bash", "WebFetch", "WebSearch"} {
		if !strings.Contains(string(js), want) {
			t.Errorf("claude settings missing %q", want)
		}
	}
}

func TestOpencodeAdapterInstall(t *testing.T) {
	chdir(t, t.TempDir())
	if err := (opencodeAdapter{}).install(installOpts{allowWeb: true}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(".opencode", "plugins", "paddock.js"))
	if err != nil {
		t.Fatalf("plugin not written: %v", err)
	}
	if !strings.Contains(string(b), `"tool.execute.before"`) {
		t.Error("generated plugin missing the tool hook")
	}
	if strings.Contains(string(b), "webfetch") {
		t.Error("allowWeb install should not block webfetch")
	}
}
