package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

// serverURL resolves the control-plane endpoint once per invocation:
//
//  1. PADDOCK_SERVER — the normal production path: platform teams expose
//     the server behind an ingress (e.g. https://paddock.internal) and
//     hand developers that one env var.
//  2. a paddock-server already reachable on localhost:8080.
//  3. an automatic port-forward over the kubeconfig the CLI already uses
//     for attach — so on any cluster with paddock installed, `paddock run`
//     just works with zero setup.
//
// Resolution failures are fatal: no command can do anything useful
// without the server.
var serverURL = sync.OnceValue(func() string {
	if v := os.Getenv("PADDOCK_SERVER"); v != "" {
		return v
	}
	local := "http://localhost:8080"
	if healthy(local) {
		return local
	}
	url, err := forwardToServer()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: no paddock server reachable: %v\n", err)
		fmt.Fprintln(os.Stderr, "set PADDOCK_SERVER to your deployment's URL (e.g. https://paddock.internal),")
		fmt.Fprintln(os.Stderr, "or point your kubeconfig at a cluster with paddock installed")
		os.Exit(1)
	}
	return url
})

func healthy(base string) bool {
	client := &http.Client{Timeout: 500 * time.Millisecond}
	resp, err := client.Get(base + "/healthz")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// forwardToServer discovers the paddock-server service by label and opens
// a port-forward to one of its ready pods on an ephemeral local port. The
// forward lives for the rest of the process — fine for a CLI.
func forwardToServer() (string, error) {
	cfg, err := kubeConfig()
	if err != nil {
		return "", err
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	namespace := os.Getenv("PADDOCK_NAMESPACE") // empty = search all namespaces
	svcs, err := client.CoreV1().Services(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "paddock.dev/component=server",
	})
	if err != nil {
		return "", fmt.Errorf("looking for the paddock-server service: %w", err)
	}
	if len(svcs.Items) == 0 {
		return "", fmt.Errorf("no service labeled paddock.dev/component=server found (namespace %q)", namespace)
	}
	svc := svcs.Items[0]

	pod, err := readyPodFor(ctx, client, svc)
	if err != nil {
		return "", err
	}
	port := svc.Spec.Ports[0].TargetPort.IntValue()
	if port == 0 {
		port = int(svc.Spec.Ports[0].Port)
	}

	transport, upgrader, err := spdy.RoundTripperFor(cfg)
	if err != nil {
		return "", err
	}
	req := client.CoreV1().RESTClient().Post().
		Resource("pods").Namespace(pod.Namespace).Name(pod.Name).SubResource("portforward")
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, "POST", req.URL())

	stopCh := make(chan struct{}) // never closed: lives for the process
	readyCh := make(chan struct{})
	fw, err := portforward.New(dialer, []string{fmt.Sprintf("0:%d", port)}, stopCh, readyCh, nil, os.Stderr)
	if err != nil {
		return "", err
	}
	errCh := make(chan error, 1)
	go func() { errCh <- fw.ForwardPorts() }()
	select {
	case <-readyCh:
	case err := <-errCh:
		return "", fmt.Errorf("port-forward to %s/%s: %w", pod.Namespace, pod.Name, err)
	case <-ctx.Done():
		return "", fmt.Errorf("port-forward to %s/%s timed out", pod.Namespace, pod.Name)
	}
	ports, err := fw.GetPorts()
	if err != nil {
		return "", err
	}
	fmt.Fprintf(os.Stderr, "connected to %s/%s via port-forward\n", svc.Namespace, svc.Name)
	return fmt.Sprintf("http://127.0.0.1:%d", ports[0].Local), nil
}

func readyPodFor(ctx context.Context, client kubernetes.Interface, svc corev1.Service) (corev1.Pod, error) {
	pods, err := client.CoreV1().Pods(svc.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labels.SelectorFromSet(svc.Spec.Selector).String(),
	})
	if err != nil {
		return corev1.Pod{}, err
	}
	for _, pod := range pods.Items {
		if pod.Status.Phase != corev1.PodRunning {
			continue
		}
		for _, cond := range pod.Status.Conditions {
			if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
				return pod, nil
			}
		}
	}
	return corev1.Pod{}, fmt.Errorf("no ready pod behind service %s/%s", svc.Namespace, svc.Name)
}
