package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/urfave/cli/v3"
)

// initLocalCmd binds the current directory to a session for local-harness mode:
// it records the session id and installs the Claude Code Bash hook, so a local
// Claude Code started here runs its shell commands in the sandbox. Setup, not a
// running agent — the developer keeps using their own local Claude Code.
func initLocalCmd() *cli.Command {
	return &cli.Command{
		Name:      "init-local",
		Usage:     "bind this directory to a session and install the Claude Code Bash hook",
		ArgsUsage: "<id>",
		Description: "Writes .paddock/session and merges a PreToolUse Bash hook into\n" +
			".claude/settings.local.json. Restart Claude Code afterwards; its Bash tool\n" +
			"calls then run inside the sandbox. Native file tools still run locally.",
		Action: func(_ context.Context, c *cli.Command) error {
			id := c.Args().First()
			if id == "" {
				return cli.Exit("which session? e.g. paddock init-local <id> (paddock ls shows them)", 2)
			}
			return initLocal(id)
		},
	}
}

func initLocal(sessionID string) error {
	if err := os.MkdirAll(".paddock", 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(".paddock", "session"), []byte(sessionID+"\n"), 0o644); err != nil {
		return err
	}

	if err := os.MkdirAll(".claude", 0o755); err != nil {
		return err
	}
	path := filepath.Join(".claude", "settings.local.json")
	settings := map[string]any{}
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &settings); err != nil {
			return fmt.Errorf("existing %s is not valid JSON (fix or remove it): %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	added := addBashHook(settings)
	b, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		return err
	}

	fmt.Printf("bound this directory to session %s (.paddock/session)\n", sessionID)
	if added {
		fmt.Printf("installed the Bash hook in %s\n", path)
	} else {
		fmt.Printf("Bash hook already present in %s\n", path)
	}
	fmt.Println("restart Claude Code — its Bash tool calls now run in the sandbox")
	return nil
}

// addBashHook merges our PreToolUse Bash hook into a parsed settings map,
// preserving any hooks the developer already configured. Returns false if an
// equivalent hook was already there (idempotent re-runs). Works on the generic
// map[string]any JSON shape so it never drops unrelated settings.
func addBashHook(settings map[string]any) bool {
	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
		settings["hooks"] = hooks
	}
	pre, _ := hooks["PreToolUse"].([]any)
	for _, e := range pre {
		if entryHasCommand(e, "paddock hook-bash") {
			return false
		}
	}
	hooks["PreToolUse"] = append(pre, map[string]any{
		"matcher": "Bash",
		"hooks": []any{
			map[string]any{"type": "command", "command": "paddock hook-bash"},
		},
	})
	return true
}

// entryHasCommand reports whether a PreToolUse entry already runs command.
func entryHasCommand(entry any, command string) bool {
	m, ok := entry.(map[string]any)
	if !ok {
		return false
	}
	inner, _ := m["hooks"].([]any)
	for _, h := range inner {
		hm, ok := h.(map[string]any)
		if !ok {
			continue
		}
		if cmd, _ := hm["command"].(string); cmd == command {
			return true
		}
	}
	return false
}
