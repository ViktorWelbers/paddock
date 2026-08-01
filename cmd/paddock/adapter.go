package main

import (
	"fmt"
	"sort"
	"strings"
)

// adapter is paddock's harness-specific seam: everything that differs between
// coding harnesses (Claude Code, opencode, …) when wiring local-harness mode.
// The paddock core — sessions, `exec`, `sync`, reuse, heartbeat, the idle
// reaper, and the neutral `hook-session-start`/`-end` commands — is
// harness-agnostic; an adapter only knows how to install its harness's own
// config to redirect the shell tool into the sandbox, wire session lifecycle to
// those neutral hooks, and deny native web tools.
type adapter interface {
	name() string
	// detect reports whether this harness looks configured in dir, for
	// selecting an adapter when --agent is omitted.
	detect(dir string) bool
	// install writes/merges this harness's configuration. Idempotent and
	// non-clobbering.
	install(o installOpts) error
}

type installOpts struct {
	allowWeb bool // keep the harness's native web tools (ungoverned) instead of denying them
}

// registry of built-in adapters. Adding a harness is one entry + one file.
var adapters = map[string]adapter{
	"claude":   claudeAdapter{},
	"opencode": opencodeAdapter{},
}

// detectOrder fixes a deterministic auto-detect precedence.
var detectOrder = []string{"claude", "opencode"}

// selectAdapter picks the adapter: an explicit name wins; otherwise the first
// harness detected in the current directory; otherwise Claude Code (the
// original default).
func selectAdapter(name string) (adapter, error) {
	if name != "" {
		a, ok := adapters[name]
		if !ok {
			return nil, fmt.Errorf("unknown harness %q; supported: %s", name, strings.Join(adapterNames(), ", "))
		}
		return a, nil
	}
	for _, n := range detectOrder {
		if adapters[n].detect(".") {
			return adapters[n], nil
		}
	}
	return adapters["claude"], nil
}

func adapterNames() []string {
	names := make([]string, 0, len(adapters))
	for n := range adapters {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
