package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/urfave/cli/v3"
)

// downCmd is the teardown counterpart to `paddock dev`: from the project
// directory it stops the background workspace sync (if any) and removes the
// bound session, so a developer never manages the underlying sync process or
// hunts for a session id.
func downCmd() *cli.Command {
	return &cli.Command{
		Name:  "down",
		Usage: "stop this directory's local-harness sync and remove its session",
		Description: "Reads .paddock/ in the current directory: stops a backgrounded workspace\n" +
			"sync and removes the bound session. Use --keep-session to stop only the sync.",
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "keep-session", Usage: "stop the sync but leave the session running"},
		},
		Action: func(_ context.Context, c *cli.Command) error {
			return down(c.Bool("keep-session"))
		},
	}
}

func down(keepSession bool) error {
	stopped := stopDetachedSync()

	id := ""
	if b, err := os.ReadFile(".paddock/session"); err == nil {
		id = strings.TrimSpace(string(b))
	}
	if id == "" {
		if !stopped {
			fmt.Println("nothing to tear down here (no .paddock/session in this directory)")
		}
		return nil
	}
	if keepSession {
		fmt.Printf("session %s left running (unbind/remove later with: paddock rm %s)\n", id, id)
		return nil
	}
	if err := deleteSession(id); err != nil {
		return err
	}
	// Unbind the directory: with no .paddock/session, the Bash hook no-ops and
	// commands run locally again, so a stale session can't keep intercepting.
	_ = os.Remove(".paddock/session")
	fmt.Printf("session %s removed; this directory is unbound\n", id)
	return nil
}
