package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/urfave/cli/v3"
)

// hookBashCmd is the Claude Code PreToolUse adapter for the local-harness mode.
// Claude Code runs it before each Bash tool call, feeding the tool input on
// stdin; it responds with a rewritten command that runs the same shell inside
// the session's sandbox (`paddock exec`) instead of on the laptop. This is the
// RTK-style "native binary hook", kept inside the paddock binary so setup is
// one install, not a loose script.
//
// It only rewrites Bash. Claude Code's native Read/Edit/Glob tools bypass this
// hook entirely and still run locally — closing that gap needs a workspace
// mount or mirror, which is deliberately out of this slice.
func hookBashCmd() *cli.Command {
	return &cli.Command{
		Name:   "hook-bash",
		Hidden: true,
		Usage:  "Claude Code PreToolUse hook: redirect Bash tool calls into the session's sandbox",
		Action: func(_ context.Context, _ *cli.Command) error {
			return hookBash(os.Stdin, os.Stdout)
		},
	}
}

// hookResponse is the subset of Claude Code's PreToolUse hook output we emit:
// updatedInput replaces the Bash tool's arguments before it runs.
type hookResponse struct {
	HookSpecificOutput struct {
		HookEventName string `json:"hookEventName"`
		UpdatedInput  struct {
			Command string `json:"command"`
		} `json:"updatedInput"`
	} `json:"hookSpecificOutput"`
}

// hookBash reads a PreToolUse payload and, if this directory is bound to a
// session, rewrites the Bash command to run in that sandbox. Anything it can't
// handle (malformed input, no bound session, empty command) is a no-op: it
// emits nothing and exits cleanly, so the command simply runs locally rather
// than wedging the agent.
func hookBash(in io.Reader, out io.Writer) error {
	var payload struct {
		ToolInput struct {
			Command string `json:"command"`
		} `json:"tool_input"`
	}
	if err := json.NewDecoder(in).Decode(&payload); err != nil {
		return nil // not our shape — leave the call untouched
	}
	command := payload.ToolInput.Command
	session := resolveLocalSession()
	if command == "" || session == "" {
		return nil
	}

	b64 := base64.StdEncoding.EncodeToString([]byte(command))
	var resp hookResponse
	resp.HookSpecificOutput.HookEventName = "PreToolUse"
	resp.HookSpecificOutput.UpdatedInput.Command = fmt.Sprintf("paddock exec %s --b64 %s", session, b64)
	return json.NewEncoder(out).Encode(resp)
}

// resolveLocalSession finds the session this working tree is bound to:
// PADDOCK_SESSION wins, else the nearest .paddock/session walking up from the
// cwd (Claude Code runs the hook at the project root, but a subdir is fine).
func resolveLocalSession() string {
	if s := strings.TrimSpace(os.Getenv("PADDOCK_SESSION")); s != "" {
		return s
	}
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for {
		if b, err := os.ReadFile(filepath.Join(dir, ".paddock", "session")); err == nil {
			return strings.TrimSpace(string(b))
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}
