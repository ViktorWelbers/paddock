package main

import (
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"
)

// serverURL resolves the control-plane endpoint once per invocation:
//
//  1. PADDOCK_SERVER — the normal path: platform teams expose the server
//     behind an ingress (e.g. https://paddock.internal) and hand
//     developers that one env var.
//  2. a paddock-server already reachable on localhost:8080 — the k3d dev
//     loop maps the cluster ingress there (`make dev-up`).
//
// Deliberately nothing else: a production CLI shouldn't tunnel into the
// cluster. Resolution failures are fatal — no command can do anything
// useful without the server.
var serverURL = sync.OnceValue(func() string {
	if v := os.Getenv("PADDOCK_SERVER"); v != "" {
		return v
	}
	local := "http://localhost:8080"
	if healthy(local) {
		return local
	}
	fmt.Fprintln(os.Stderr, "error: no paddock server reachable")
	fmt.Fprintln(os.Stderr, "set PADDOCK_SERVER to your deployment's URL (e.g. https://paddock.internal)")
	os.Exit(1)
	return ""
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
