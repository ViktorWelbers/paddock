package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"

	"golang.org/x/term"

	"github.com/viktorwelbers/paddock/internal/sandbox"
)

// attachSession execs the agent command inside a session's sandbox pod with a
// TTY, over the operator's kubeconfig. The pod itself just holds (tini +
// sleep), so sessions survive the agent exiting and can be re-attached.
func attachSession(sessionID string, command []string) error {
	if len(command) == 0 {
		command = []string{"claude"}
	}

	cfg, err := kubeConfig()
	if err != nil {
		return err
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return err
	}

	ns := sandbox.NamespaceName(sessionID)
	if err := waitPodRunning(client, ns, cfg.Host); err != nil {
		return err
	}

	req := client.CoreV1().RESTClient().Post().
		Resource("pods").Namespace(ns).Name("agent").SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: "agent",
			Command:   command,
			Stdin:     true,
			Stdout:    true,
			Stderr:    false, // stderr is merged into stdout when TTY is on
			TTY:       true,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(cfg, "POST", req.URL())
	if err != nil {
		return err
	}

	stdinFd := int(os.Stdin.Fd())
	if !term.IsTerminal(stdinFd) {
		return fmt.Errorf("attach needs an interactive terminal")
	}
	oldState, err := term.MakeRaw(stdinFd)
	if err != nil {
		return err
	}
	defer term.Restore(stdinFd, oldState)

	fmt.Printf("attaching to session %s (%v); the sandbox can only reach the paddock gateway\r\n", sessionID, command)
	err = exec.StreamWithContext(context.Background(), remotecommand.StreamOptions{
		Stdin:             os.Stdin,
		Stdout:            os.Stdout,
		Tty:               true,
		TerminalSizeQueue: newResizeQueue(stdinFd),
	})
	term.Restore(stdinFd, oldState)
	fmt.Printf("\ndetached from session %s (still running; re-attach with: paddock attach %s)\n", sessionID, sessionID)
	return err
}

// kubeConfig loads the operator's kubeconfig: PADDOCK_KUBECONFIG wins, then
// the usual defaults (KUBECONFIG, ~/.kube/config).
func kubeConfig() (*rest.Config, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if p := os.Getenv("PADDOCK_KUBECONFIG"); p != "" {
		rules.ExplicitPath = p
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, nil).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig (set PADDOCK_KUBECONFIG or KUBECONFIG): %w", err)
	}
	return cfg, nil
}

func waitPodRunning(client kubernetes.Interface, namespace, host string) error {
	deadline := time.Now().Add(2 * time.Minute)
	notified := false
	for {
		pod, err := client.CoreV1().Pods(namespace).Get(context.Background(), "agent", metav1.GetOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			// NotFound just means the pod isn't created yet — keep waiting.
			// Anything else (unreachable API server, auth) won't heal on its
			// own; a stale kubectl context must not look like a slow pod.
			return fmt.Errorf("cannot reach the cluster at %s: %w\nattach talks to the Kubernetes API directly (until the server-side relay lands): point your current kubectl context or PADDOCK_KUBECONFIG at the cluster running paddock", host, err)
		}
		if err == nil {
			switch pod.Status.Phase {
			case corev1.PodRunning:
				return nil
			case corev1.PodSucceeded, corev1.PodFailed:
				return fmt.Errorf("sandbox pod is %s; remove the session and start a new one", pod.Status.Phase)
			}
		}
		if time.Now().After(deadline) {
			if err != nil {
				return fmt.Errorf("sandbox pod never appeared in %s: %w", namespace, err)
			}
			return fmt.Errorf("sandbox pod not running after 2m (phase %s)", pod.Status.Phase)
		}
		if !notified {
			fmt.Println("waiting for the sandbox pod to start...")
			notified = true
		}
		time.Sleep(2 * time.Second)
	}
}

// resizeQueue forwards local terminal size changes to the remote TTY.
type resizeQueue struct {
	fd int
	ch chan remotecommand.TerminalSize
}

func newResizeQueue(fd int) *resizeQueue {
	q := &resizeQueue{fd: fd, ch: make(chan remotecommand.TerminalSize, 1)}
	q.push() // initial size
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGWINCH)
	go func() {
		for range sigs {
			q.push()
		}
	}()
	return q
}

func (q *resizeQueue) push() {
	w, h, err := term.GetSize(q.fd)
	if err != nil {
		return
	}
	select {
	case q.ch <- remotecommand.TerminalSize{Width: uint16(w), Height: uint16(h)}:
	default:
	}
}

func (q *resizeQueue) Next() *remotecommand.TerminalSize {
	size, ok := <-q.ch
	if !ok {
		return nil
	}
	return &size
}
