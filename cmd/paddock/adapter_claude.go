package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// claudeAdapter wires local-harness mode into Claude Code via its JSON settings:
// SessionStart/SessionEnd hooks call paddock's neutral lifecycle commands, a
// PreToolUse Bash hook rewrites shell commands into the sandbox, and web tools
// are denied through permissions.
type claudeAdapter struct{}

func (claudeAdapter) name() string { return "claude" }

func (claudeAdapter) detect(dir string) bool {
	fi, err := os.Stat(filepath.Join(dir, ".claude"))
	return err == nil && fi.IsDir()
}

func (claudeAdapter) install(o installOpts) error {
	err := mergeSettings(func(s map[string]any) {
		addHook(s, "SessionStart", "", "paddock hook-session-start")
		addHook(s, "SessionEnd", "", "paddock hook-session-end")
		addHook(s, "PreToolUse", "Bash", "paddock hook-bash")
		if !o.allowWeb {
			addToolDeny(s, "WebFetch", "WebSearch")
		}
	})
	if err != nil {
		return err
	}
	fmt.Println("installed Claude Code hooks in .claude/settings.local.json")
	return nil
}
