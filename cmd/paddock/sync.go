package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/urfave/cli/v3"
)

// syncCmd keeps the local directory and the sandbox /workspace consistent in
// both directions, so a locally-run harness's native file tools and the
// commands it runs in the sandbox see the same tree. It is a thin wrapper over
// DevSpace's sync engine, which does two-way sync (creations, modifications,
// deletions, conflict resolution) over the Kubernetes API using tar-over-exec —
// the same transport paddock's push/pull already use, and it needs no SSH or
// service-account token in the sandbox.
//
// paddock owns the sync process lifecycle: in the foreground, Ctrl-C stops it
// cleanly; with --detach it runs in the background and `paddock down` stops it.
// You never manage the underlying `devspace` process yourself.
func syncCmd() *cli.Command {
	return &cli.Command{
		Name:      "sync",
		Usage:     "bidirectionally sync the current directory with a session's /workspace",
		ArgsUsage: "<id>",
		Description: "Keeps local files and the sandbox /workspace consistent both ways via\n" +
			"`devspace sync` (over the Kubernetes API, no SSH). Foreground by default —\n" +
			"Ctrl-C stops it; with --detach it runs in the background and `paddock down` stops it.",
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:    "detach",
				Aliases: []string{"d"},
				Usage:   "run the sync in the background (stop it with `paddock down`)",
			},
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
			return syncSession(id, c.StringSlice("exclude"), c.Bool("detach"))
		},
	}
}

const syncPidFile = ".paddock/sync.pid"

// syncSession runs `devspace sync` for ./ <-> /workspace against the session's
// pod. preferNewest resolves concrete conflicts by timestamp, which suits a tree
// both sides mutate (local edits + in-sandbox commands, including .git state).
func syncSession(sessionID string, exclude []string, detach bool) error {
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
	// paddock's own control files are not project content; syncing them into the
	// sandbox is noise (and syncing the hook config into a pod that might be
	// attached in-pod could loop), so keep .paddock out by default.
	args = append(args, "--exclude", ".paddock")
	for _, e := range exclude {
		args = append(args, "--exclude", e)
	}
	// paddock's kubeconfig convention (PADDOCK_KUBECONFIG) is not devspace's, so
	// pass it through explicitly to reach the same cluster `attach` uses.
	if kc := os.Getenv("PADDOCK_KUBECONFIG"); kc != "" {
		args = append(args, "--kubeconfig", kc)
	}

	if detach {
		return startSyncDetached(loc.Pod, args)
	}
	return runSyncForeground(loc.Pod, args)
}

// runSyncForeground streams the sync until the user stops it. Ctrl-C in the
// terminal already reaches devspace (same process group); the extra handler
// also forwards a SIGTERM sent straight to paddock, and either way we exit
// cleanly rather than surfacing the signal as an error.
func runSyncForeground(pod string, args []string) error {
	fmt.Printf("syncing . <-> %s:/workspace  (Ctrl-C to stop)\n", pod)
	cmd := exec.Command("devspace", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	stopped := false
	go func() {
		<-sigs
		stopped = true
		if cmd.Process != nil {
			_ = cmd.Process.Signal(syscall.SIGTERM)
		}
	}()
	err := cmd.Wait()
	fmt.Println("\nworkspace sync stopped")
	if stopped {
		return nil // ending the sync on request is not a failure
	}
	return err
}

// startSyncDetached launches devspace in its own session so it survives this
// CLI invocation, records its pid for `paddock down`, and returns. Output goes
// to .paddock/sync.log.
func startSyncDetached(pod string, args []string) error {
	if err := os.MkdirAll(".paddock", 0o755); err != nil {
		return err
	}
	logf, err := os.Create(".paddock/sync.log")
	if err != nil {
		return err
	}
	cmd := exec.Command("devspace", args...)
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // detach from this process group
	if err := cmd.Start(); err != nil {
		return err
	}
	pid := cmd.Process.Pid // capture before Release(), which zeroes it
	if err := os.WriteFile(syncPidFile, []byte(strconv.Itoa(pid)), 0o644); err != nil {
		return err
	}
	_ = cmd.Process.Release()
	fmt.Printf("workspace sync running in the background (. <-> %s:/workspace, pid %d)\n", pod, pid)
	fmt.Println("logs: .paddock/sync.log   ·   stop + tear down: paddock down")
	return nil
}

// stopDetachedSync stops a background sync started with --detach if one is
// tracked for this directory. Returns whether it stopped something.
func stopDetachedSync() bool {
	b, err := os.ReadFile(syncPidFile)
	if err != nil {
		return false
	}
	defer os.Remove(syncPidFile)
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if err := p.Signal(syscall.SIGTERM); err != nil {
		return false // already gone
	}
	fmt.Println("workspace sync stopped")
	return true
}
