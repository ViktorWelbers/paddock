package main

import (
	"context"
	"fmt"

	"github.com/urfave/cli/v3"
)

// initCmd is the one-time, smooth setup for local-harness mode. After it, the
// developer just runs their harness: a SessionStart hook finds-or-creates the
// directory's sandbox and starts sync, a PreToolUse hook redirects Bash into it,
// and a SessionEnd hook stops the sync. Web tools that would bypass the governed
// sandbox egress are denied by default.
func initCmd() *cli.Command {
	return &cli.Command{
		Name:  "init",
		Usage: "set up automatic local-harness mode for this directory (run once)",
		Description: "Installs Claude Code lifecycle hooks so the sandbox is created/reused and\n" +
			"synced automatically when you start your harness here — no `paddock dev`/`down`.\n" +
			"Denies WebFetch/WebSearch by default (they run locally and bypass the governed\n" +
			"sandbox egress); pass --allow-web-tools to keep them.",
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "allow-web-tools", Usage: "keep WebFetch/WebSearch enabled (ungoverned, unaudited network access)"},
		},
		Action: func(_ context.Context, c *cli.Command) error {
			allowWeb := c.Bool("allow-web-tools")
			err := mergeSettings(func(s map[string]any) {
				addHook(s, "SessionStart", "", "paddock hook-session-start")
				addHook(s, "SessionEnd", "", "paddock hook-session-end")
				addHook(s, "PreToolUse", "Bash", "paddock hook-bash")
				if !allowWeb {
					addToolDeny(s, "WebFetch", "WebSearch")
				}
			})
			if err != nil {
				return err
			}
			fmt.Println("paddock local-harness mode is set up for this directory (.claude/settings.local.json)")
			if allowWeb {
				fmt.Println("WebFetch/WebSearch left enabled — note they run locally and are NOT governed or audited")
			} else {
				fmt.Println("WebFetch/WebSearch denied (use sandboxed shell for governed, audited network access)")
			}
			fmt.Println("just run your harness here — the sandbox is created/reused and synced automatically")
			return nil
		},
	}
}

// hookSessionStartCmd (hidden) is Claude Code's SessionStart hook: ensure the
// directory's sandbox exists (find-or-create, so it never piles up) and its
// workspace sync is running. On first create this provisions a sandbox (a
// one-time wait); on reuse it's fast.
func hookSessionStartCmd() *cli.Command {
	return &cli.Command{
		Name:   "hook-session-start",
		Hidden: true,
		Usage:  "Claude Code SessionStart hook: ensure this directory's sandbox + sync are up",
		Action: func(_ context.Context, _ *cli.Command) error {
			return hookSessionStart()
		},
	}
}

func hookSessionStart() error {
	id, err := ensureSession("claude", true, true, false)
	if err != nil {
		return err
	}
	if err := initLocal(id); err != nil { // record the binding + ensure the Bash hook
		return err
	}
	if syncRunning() {
		return nil // supervisor already up for this directory
	}
	return syncSession(id, nil, true) // start the detached, heartbeating supervisor
}

// hookSessionEndCmd (hidden) is Claude Code's SessionEnd hook: stop the sync
// supervisor. The session itself is left warm for a fast reopen; the server's
// idle reaper reclaims it once the heartbeats stop.
func hookSessionEndCmd() *cli.Command {
	return &cli.Command{
		Name:   "hook-session-end",
		Hidden: true,
		Usage:  "Claude Code SessionEnd hook: stop this directory's workspace sync",
		Action: func(_ context.Context, _ *cli.Command) error {
			stopDetachedSync()
			return nil
		},
	}
}
