package main

import (
	"context"
	"fmt"

	"github.com/urfave/cli/v3"
)

// devCmd sets up local-harness mode in one step: provision a detached session
// (uploading this directory and installing the developer's git identity /
// credentials / signing, exactly as `run` does), install the Bash redirection
// hook for this directory, then start two-way workspace sync. The developer
// then runs their own local harness here — its Bash tool calls execute in the
// sandbox and its file edits sync to it.
func devCmd() *cli.Command {
	return &cli.Command{
		Name:      "dev",
		Usage:     "local-harness mode: create a sandbox, bind this directory, and start workspace sync",
		ArgsUsage: "<agent>",
		Description: "One-command local-harness setup. By default it holds the sync in the\n" +
			"foreground (Ctrl-C stops everything); with --detach the sync runs in the\n" +
			"background and `paddock down` tears it down. Start your local harness in this\n" +
			"directory: its shell commands run in the sandbox and file edits sync both ways.\n" +
			"Model calls go direct with your own credentials and are not metered in this mode.",
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "detach", Aliases: []string{"d"}, Usage: "run the sync in the background (stop it with `paddock down`)"},
			&cli.BoolFlag{Name: "no-git-credentials", Usage: "do not install the repo's git credentials into the sandbox"},
			&cli.BoolFlag{Name: "no-git-signing", Usage: "do not install the repo's commit-signing key into the sandbox"},
		},
		Action: func(_ context.Context, c *cli.Command) error {
			agent := c.Args().First()
			if agent == "" {
				return cli.Exit("which agent? e.g. paddock dev claude", 2)
			}
			id, err := provisionSession(agent, true, !c.Bool("no-git-credentials"), !c.Bool("no-git-signing"))
			if err != nil {
				return err
			}
			if err := initLocal(id); err != nil {
				return err
			}
			detach := c.Bool("detach")
			if !detach {
				fmt.Println("starting workspace sync — Ctrl-C stops it (or run with --detach)")
			}
			return syncSession(id, nil, detach)
		},
	}
}
