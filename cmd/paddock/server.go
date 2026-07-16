package main

import (
	"encoding/json"
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

// location is where a session's sandbox pod lives, as reported by the server.
type location struct {
	Namespace string `json:"namespace"`
	Pod       string `json:"pod"`
	Status    string `json:"status"`
}

// sessionLocation asks the server where a session's pod is. The layout is the
// server's to decide, so the CLI reads it rather than reconstructing it.
func sessionLocation(sessionID string) (location, error) {
	resp, err := http.Get(serverURL() + "/v1/sessions/" + sessionID)
	if err != nil {
		return location{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return location{}, fmt.Errorf("no session %s (paddock ls shows the live ones)", sessionID)
	}
	if resp.StatusCode != http.StatusOK {
		return location{}, fmt.Errorf("look up session %s: %s", sessionID, resp.Status)
	}
	var loc location
	if err := json.NewDecoder(resp.Body).Decode(&loc); err != nil {
		return location{}, err
	}
	if loc.Status != "running" {
		return location{}, fmt.Errorf("session %s is %s", sessionID, loc.Status)
	}
	if loc.Namespace == "" || loc.Pod == "" {
		return location{}, fmt.Errorf("session %s has no sandbox: the server is running without a cluster", sessionID)
	}
	return loc, nil
}

func healthy(base string) bool {
	client := &http.Client{Timeout: 500 * time.Millisecond}
	resp, err := client.Get(base + "/healthz")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
