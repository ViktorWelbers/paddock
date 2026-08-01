## Why

Today the coding harness (Claude Code) runs *inside* the sandbox pod, which forces the entire
subscription-auth dance (OAuth injection, onboarding pre-seed, TTY login) and ties the developer to
an in-pod TUI. A second, first-class mode is wanted: run the harness **locally** — with the
developer's own model credentials and their native tooling — while its **tool execution happens in
the governed sandbox**. paddock becomes a "governed execution sandbox": isolation, governed egress,
and audit for the agent's *actions*, without owning model spend.

## What Changes

- Add a **local-harness mode**: the harness runs on the developer's machine; its shell commands
  execute in the session's sandbox and its workspace stays consistent with the sandbox tree.
- `paddock exec <id> [--b64] <cmd>` — run a command in a session's sandbox, streaming output and
  propagating the exit code (already prototyped).
- `paddock hook-bash` — a Claude Code `PreToolUse` adapter that rewrites Bash tool calls to
  `paddock exec …` so they run in the sandbox (already prototyped).
- `paddock init-local <id>` — bind a directory to a session and install the hook (already
  prototyped).
- `paddock sync <id>` — **bidirectional** workspace synchronization between the local directory and
  the sandbox `/workspace`, using the industry-standard **DevSpace sync** engine (tar-over-exec, no
  SSH, client-only), so it is irrelevant that the harness's native file tools run locally.
- `paddock dev <agent>` — a convenience that creates a detached session, binds the directory, and
  starts sync, so setup is one command.
- Docs: a mode comparison and honest per-mode governance labeling.
- **Not BREAKING**: the existing in-pod mode (`paddock run`, `paddock attach`) is unchanged and
  stays supported.

## Capabilities

### New Capabilities
- `local-harness`: run the coding harness locally against a governed sandbox — remote command
  execution, Bash-tool redirection, bidirectional workspace sync, one-command setup, and the
  mode's governance posture (unmetered; egress + audit for tool actions preserved).

### Modified Capabilities
<!-- None: openspec/specs/ has no existing capability specs; the in-pod behavior is untouched. -->

## Impact

- **New code (client only):** `cmd/paddock/{exec,hookbash,initlocal,sync,dev}.go`; new commands
  registered in `cmd/paddock/main.go`.
- **New dependency:** the `devspace` CLI on the developer's machine (`brew install devspace`);
  shelled out to by `paddock sync`. No library vendoring in v1.
- **No control-plane or agent-image change:** DevSpace injects its own helper via tar-over-exec;
  the agent image already ships `tar`. The mode is entirely client-side and uses the developer's
  kubeconfig, exactly as `paddock attach` does today.
- **Governance:** model-call metering is **not** applied in this mode (documented); governed egress
  and the audit trail for tool actions are unchanged because execution still happens in the sandbox.
- **Transport caveat:** like `attach`, v1 talks to the Kubernetes API from the developer's machine;
  routing through a server-side relay is future work (see ROADMAP "server-side attach relay").
