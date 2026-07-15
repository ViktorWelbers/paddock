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
//  1. PADDOCK_SERVER — per-shell override (CI, one-off against another
//     deployment).
//  2. the saved config (`paddock config set server <url>`) — the normal
//     path: platform teams expose the server behind an ingress and
//     developers save that URL once.
//  3. a paddock-server already reachable on localhost:8080 — the k3d dev
//     loop maps the cluster ingress there (`make dev-up`).
//
// Deliberately nothing else: a production CLI shouldn't tunnel into the
// cluster. Resolution failures are fatal — no command can do anything
// useful without the server.
var serverURL = sync.OnceValue(func() string {
	if v := os.Getenv("PADDOCK_SERVER"); v != "" {
		return v
	}
	if c := loadConfig(); c.Server != "" {
		return c.Server
	}
	local := "http://localhost:8080"
	if healthy(local) {
		return local
	}
	fmt.Fprintln(os.Stderr, "error: no paddock server reachable")
	fmt.Fprintln(os.Stderr, "save your deployment's URL once with: paddock config set server <url>")
	fmt.Fprintln(os.Stderr, "(PADDOCK_SERVER overrides it per shell)")
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
