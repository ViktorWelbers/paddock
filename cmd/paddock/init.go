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
		Description: "Installs the coding harness's lifecycle integration so the sandbox is\n" +
			"created/reused and synced automatically when you start it here — no\n" +
			"`paddock dev`/`down`. Pick the harness with --agent (claude, opencode); the\n" +
			"default is auto-detected. Denies native web tools by default (they run locally\n" +
			"and bypass the governed sandbox egress); pass --allow-web-tools to keep them.",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "agent", Usage: "harness to set up: claude, opencode (default: auto-detect)"},
			&cli.BoolFlag{Name: "allow-web-tools", Usage: "keep the harness's native web tools enabled (ungoverned, unaudited network access)"},
		},
		Action: func(_ context.Context, c *cli.Command) error {
			a, err := selectAdapter(c.String("agent"))
			if err != nil {
				return err
			}
			allowWeb := c.Bool("allow-web-tools")
			if err := a.install(installOpts{allowWeb: allowWeb}); err != nil {
				return err
			}
			fmt.Printf("paddock local-harness mode is set up for %s in this directory\n", a.name())
			if allowWeb {
				fmt.Println("native web tools left enabled — note they run locally and are NOT governed or audited")
			} else {
				fmt.Println("native web tools denied (use sandboxed shell for governed, audited network access)")
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
	// agent "claude" is the sandbox toolbox image (git/node/python/…), independent
	// of which local harness is driving — the harness runs on the laptop; the
	// sandbox just executes its commands.
	id, err := ensureSession("claude", true, true, false)
	if err != nil {
		return err
	}
	if err := writeBoundSession(id); err != nil { // record the binding (the adapter installed the redirect at init)
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
