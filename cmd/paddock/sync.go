package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

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
		return startSupervisorDetached(sessionID, exclude)
	}
	return runSyncForeground(sessionID, loc.Pod, args)
}

// runSyncForeground supervises the sync until stopped. It runs devspace and a
// liveness watchdog: if the bound session goes away (the idle reaper reclaimed
// it after the harness closed/crashed, or `paddock down` removed it), the
// watchdog stops devspace and exits, so a supervisor never outlives its session.
// The heartbeat that keeps a session warm comes from the harness itself (see
// `paddock heartbeat`), not from here — that is what lets the reaper reclaim a
// session whose harness is gone. Ctrl-C / SIGTERM also stop it cleanly.
func runSyncForeground(sessionID, pod string, args []string) error {
	fmt.Printf("syncing . <-> %s:/workspace  (Ctrl-C to stop)\n", pod)
	// A detached supervisor recorded its pid; remove it on the way out however we
	// exit (signal, watchdog, or devspace dying), so nothing thinks a dead sync
	// is still running.
	defer removeOwnSyncPid()

	cmd := exec.Command("devspace", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}

	stopped := false
	stop := func() {
		stopped = true
		if cmd.Process != nil {
			_ = cmd.Process.Signal(syscall.SIGTERM)
		}
	}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go func() { <-sigs; stop() }()

	// Watchdog: once the session is gone, there is nothing to sync to.
	done := make(chan struct{})
	defer close(done)
	go func() {
		t := time.NewTicker(90 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if !sessionRunning(sessionID) {
					stop()
					return
				}
			}
		}
	}()

	err := cmd.Wait()
	fmt.Println("\nworkspace sync stopped")
	if stopped {
		return nil // ending the sync on request/reap is not a failure
	}
	return err
}

// removeOwnSyncPid clears .paddock/sync.pid if it points at this process, so a
// supervisor that self-terminates leaves no stale pid behind.
func removeOwnSyncPid() {
	b, err := os.ReadFile(syncPidFile)
	if err != nil {
		return
	}
	if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid == os.Getpid() {
		_ = os.Remove(syncPidFile)
	}
}

// startSupervisorDetached re-execs paddock itself (running the foreground sync,
// which owns both devspace and the heartbeat) in its own session so it survives
// this CLI invocation, records its pid for `paddock down`/SessionEnd, and
// returns. Output goes to .paddock/sync.log.
func startSupervisorDetached(sessionID string, exclude []string) error {
	if err := os.MkdirAll(".paddock", 0o755); err != nil {
		return err
	}
	logf, err := os.Create(".paddock/sync.log")
	if err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	childArgs := []string{"sync", sessionID}
	for _, e := range exclude {
		childArgs = append(childArgs, "--exclude", e)
	}
	cmd := exec.Command(self, childArgs...)
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
	fmt.Printf("workspace sync running in the background (pid %d)\n", pid)
	fmt.Println("logs: .paddock/sync.log   ·   stop + tear down: paddock down")
	return nil
}

func sendHeartbeat(sessionID string) {
	req, err := apiRequest(http.MethodPost, "/v1/sessions/"+sessionID+"/heartbeat", nil)
	if err != nil {
		return
	}
	if resp, err := apiDo(req); err == nil {
		_ = resp.Body.Close()
	}
}

// syncRunning reports whether a background sync supervisor is alive for this
// directory (its recorded pid still exists).
func syncRunning() bool {
	b, err := os.ReadFile(syncPidFile)
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil // signal 0 just probes liveness
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
