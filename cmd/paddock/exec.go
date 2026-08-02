package main

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"strings"

	"github.com/urfave/cli/v3"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
	utilexec "k8s.io/client-go/util/exec"
)

// execCmd runs a single command inside a session's sandbox and streams its
// output back — the non-interactive sibling of `attach`. It is the primitive
// behind the local-harness mode: a local coding agent's Bash tool calls are
// rewritten (by `hook-bash`) to `paddock exec <id> …`, so the shell command
// runs in the governed sandbox instead of on the developer's machine.
func execCmd() *cli.Command {
	return &cli.Command{
		Name:      "exec",
		Usage:     "run a command inside a session's sandbox (non-interactive)",
		ArgsUsage: "<id> <command>...",
		Description: "Runs the command in the sandbox's /workspace and streams stdout/stderr;\n" +
			"the command's exit code is propagated. Used by the local-harness Bash hook\n" +
			"to send an agent's shell commands into the sandbox instead of running them\n" +
			"locally.",
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:  "b64",
				Usage: "the single argument is a base64-encoded command string (avoids quoting)",
			},
		},
		Action: func(_ context.Context, c *cli.Command) error {
			id := c.Args().First()
			if id == "" {
				return cli.Exit("which session? paddock ls shows them", 2)
			}
			rest := c.Args().Tail()
			command, err := commandFromArgs(rest, c.Bool("b64"))
			if err != nil {
				return cli.Exit(err.Error(), 2)
			}
			return execInSandbox(id, command)
		},
	}
}

// commandFromArgs assembles the shell command from CLI args. With --b64 the
// hook hands us exactly one base64 blob so arbitrary shell survives the local
// shell that invokes `paddock exec`; without it, args are joined as-is.
func commandFromArgs(args []string, b64 bool) (string, error) {
	if b64 {
		if len(args) != 1 {
			return "", errors.New("--b64 expects exactly one base64 argument")
		}
		raw, err := base64.StdEncoding.DecodeString(args[0])
		if err != nil {
			return "", errors.New("bad --b64 argument: " + err.Error())
		}
		return string(raw), nil
	}
	if len(args) == 0 {
		return "", errors.New("nothing to run")
	}
	return strings.Join(args, " "), nil
}

// execInSandbox execs `sh -c <command>` in the session's pod. The container's
// WorkingDir is /workspace, so the command runs there. A non-zero exit is
// surfaced as this process's own exit code, so the calling agent sees the same
// success/failure it would running the command locally.
func execInSandbox(sessionID, command string) error {
	loc, err := sessionLocation(sessionID)
	if err != nil {
		return err
	}
	// Running a command is activity (keeps the session off the idle reaper) and
	// worth recording (what the agent ran). Both async, so neither adds latency.
	go sendHeartbeat(sessionID)
	go logExec(sessionID, command)
	cfg, err := kubeConfig()
	if err != nil {
		return err
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return err
	}
	req := client.CoreV1().RESTClient().Post().
		Resource("pods").Namespace(loc.Namespace).Name(loc.Pod).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: "agent",
			Command:   []string{"sh", "-c", command},
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	executor, err := remotecommand.NewSPDYExecutor(cfg, "POST", req.URL())
	if err != nil {
		return err
	}
	err = executor.StreamWithContext(context.Background(), remotecommand.StreamOptions{
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	})
	if err != nil {
		var codeErr utilexec.CodeExitError
		if errors.As(err, &codeErr) {
			os.Exit(codeErr.Code)
		}
		return err
	}
	return nil
}
