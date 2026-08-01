package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"

	"github.com/urfave/cli/v3"
)

// syncCmd keeps the local directory and the sandbox /workspace consistent in
// both directions, so a locally-run harness's native file tools and the
// commands it runs in the sandbox see the same tree. It is a thin wrapper over
// DevSpace's sync engine, which does two-way sync (creations, modifications,
// deletions, conflict resolution) over the Kubernetes API using tar-over-exec —
// the same transport paddock's push/pull already use, and it needs no SSH or
// service-account token in the sandbox.
func syncCmd() *cli.Command {
	return &cli.Command{
		Name:      "sync",
		Usage:     "bidirectionally sync the current directory with a session's /workspace",
		ArgsUsage: "<id>",
		Description: "Keeps local files and the sandbox /workspace consistent both ways via\n" +
			"`devspace sync` (over the Kubernetes API, no SSH). Run it alongside a local\n" +
			"harness so its file tools and the sandbox observe the same tree. Ctrl-C stops it.",
		Flags: []cli.Flag{
			&cli.StringSliceFlag{
				Name:  "exclude",
				Usage: "path to exclude from sync (repeatable)",
			},
		},
		Action: func(_ context.Context, c *cli.Command) error {
			id := c.Args().First()
			if id == "" {
				return cli.Exit("which session? paddock ls shows them", 2)
			}
			return syncSession(id, c.StringSlice("exclude"))
		},
	}
}

// syncSession runs `devspace sync` for ./ <-> /workspace against the session's
// pod. preferNewest resolves concrete conflicts by timestamp, which suits a tree
// both sides mutate (local edits + in-sandbox commands, including .git state).
func syncSession(sessionID string, exclude []string) error {
	if _, err := exec.LookPath("devspace"); err != nil {
		return fmt.Errorf("devspace is not installed — `paddock sync` uses it for two-way workspace sync.\n" +
			"install it with `brew install devspace` (or see https://devspace.sh) and retry")
	}
	loc, err := sessionLocation(sessionID)
	if err != nil {
		return err
	}
	args := []string{
		"sync",
		"--path", ".:/workspace",
		"--namespace", loc.Namespace,
		"--pod", loc.Pod,
		"--container", "agent",
		"--initial-sync", "preferNewest",
		"--no-warn",
	}
	for _, e := range exclude {
		args = append(args, "--exclude", e)
	}
	// paddock's kubeconfig convention (PADDOCK_KUBECONFIG) is not devspace's, so
	// pass it through explicitly to reach the same cluster `attach` uses. Without
	// it, devspace falls back to the ambient KUBECONFIG / current context.
	if kc := os.Getenv("PADDOCK_KUBECONFIG"); kc != "" {
		args = append(args, "--kubeconfig", kc)
	}

	fmt.Printf("syncing . <-> %s:/workspace  (Ctrl-C to stop)\n", loc.Pod)
	cmd := exec.Command("devspace", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}
