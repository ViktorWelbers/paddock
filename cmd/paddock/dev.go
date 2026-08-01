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
		Description: "One-command local-harness setup. Leave `dev` running (it holds the sync);\n" +
			"start your local harness in another terminal in this directory. Its shell\n" +
			"commands run in the sandbox; file edits sync both ways. Model calls go direct\n" +
			"with your own credentials and are not metered in this mode.",
		Flags: []cli.Flag{
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
			fmt.Println("starting workspace sync — leave this running; start your harness in another terminal here")
			return syncSession(id, nil)
		},
	}
}
