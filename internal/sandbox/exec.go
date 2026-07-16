package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
)

// Execer runs a command inside a session's sandbox. It exists so the server
// can stream a workspace in and out of a pod on the developer's behalf —
// the CLI then needs nothing but the paddock API, no kubeconfig and no
// pods/exec rights of its own.
//
// Noop deliberately does not implement it: without a cluster there is no pod
// to exec into, and the workspace endpoints report that rather than pretend.
type Execer interface {
	// WaitRunning blocks until the session's pod is running.
	WaitRunning(ctx context.Context, sessionID string) error
	// Exec runs cmd in the sandbox, wiring the streams. A nil stdin means
	// the command gets no input.
	Exec(ctx context.Context, sessionID string, cmd []string, stdin io.Reader, stdout, stderr io.Writer) error
}

// WaitRunning polls until the session's pod reports Running.
func (k *K8s) WaitRunning(ctx context.Context, sessionID string) error {
	name := ResourceName(sessionID)
	deadline := time.Now().Add(2 * time.Minute)
	for {
		pod, err := k.Client.CoreV1().Pods(k.Namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			// NotFound just means the kubelet hasn't caught up yet; anything
			// else won't heal by waiting.
			return fmt.Errorf("look up sandbox pod: %w", err)
		}
		if err == nil {
			switch pod.Status.Phase {
			case corev1.PodRunning:
				return nil
			case corev1.PodSucceeded, corev1.PodFailed:
				return fmt.Errorf("sandbox pod is %s", pod.Status.Phase)
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("sandbox pod for session %s was not running after 2m", sessionID)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// Exec runs cmd in the session's agent container.
func (k *K8s) Exec(ctx context.Context, sessionID string, cmd []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if k.RESTConfig == nil {
		return fmt.Errorf("sandbox exec is not configured (no REST config)")
	}
	req := k.Client.CoreV1().RESTClient().Post().
		Resource("pods").Namespace(k.Namespace).Name(ResourceName(sessionID)).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: "agent",
			Command:   cmd,
			Stdin:     stdin != nil,
			Stdout:    stdout != nil,
			Stderr:    true,
			TTY:       false, // a TTY would merge stderr into stdout and mangle the tar bytes
		}, scheme.ParameterCodec)

	// WebSockets are the modern transport; older API servers still need
	// SPDY, so fall back when the upgrade is what failed.
	websocket, err := remotecommand.NewWebSocketExecutor(k.RESTConfig, "GET", req.URL().String())
	if err != nil {
		return err
	}
	spdy, err := remotecommand.NewSPDYExecutor(k.RESTConfig, "POST", req.URL())
	if err != nil {
		return err
	}
	exec, err := remotecommand.NewFallbackExecutor(websocket, spdy, httpstream.IsUpgradeFailure)
	if err != nil {
		return err
	}

	// Keep stderr bounded: it only exists to explain a failure, and the
	// remote command is not something we want writing unbounded memory.
	var errBuf bytes.Buffer
	if stderr == nil {
		stderr = &limitedWriter{w: &errBuf, remaining: 8 << 10}
	}
	err = exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:  stdin,
		Stdout: stdout,
		Stderr: stderr,
	})
	if err != nil && errBuf.Len() > 0 {
		return fmt.Errorf("%w: %s", err, bytes.TrimSpace(errBuf.Bytes()))
	}
	return err
}

// limitedWriter keeps the first n bytes and silently drops the rest.
type limitedWriter struct {
	w         io.Writer
	remaining int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.remaining <= 0 {
		return len(p), nil
	}
	keep := p
	if len(keep) > l.remaining {
		keep = keep[:l.remaining]
	}
	n, err := l.w.Write(keep)
	l.remaining -= n
	if err != nil {
		return n, err
	}
	return len(p), nil
}
