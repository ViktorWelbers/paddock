package main

import (
	"context"

	"github.com/urfave/cli/v3"
)

// heartbeatCmd marks the bound session active, keeping it off the idle reaper
// while the harness is alive. It is driven by the harness itself (a Claude Code
// Stop hook after each response; an opencode plugin interval while open) so that
// when the harness closes or crashes, heartbeats stop and the server reclaims
// the sandbox on its own — no `paddock down` required.
func heartbeatCmd() *cli.Command {
	return &cli.Command{
		Name:      "heartbeat",
		Hidden:    true,
		Usage:     "mark the bound session active (keeps it off the idle reaper)",
		ArgsUsage: "[id]",
		Action: func(_ context.Context, c *cli.Command) error {
			id := c.Args().First()
			if id == "" {
				id = resolveLocalSession()
			}
			if id != "" {
				sendHeartbeat(id)
			}
			return nil
		},
	}
}
