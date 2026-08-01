## Context

paddock's in-pod mode runs the whole agent (harness + tools) inside the sandbox — the
"remote-first" pattern also used by Codespaces/Gitpod/VS Code Remote, where files live remotely and
there is no sync problem. Moving the harness to the developer's machine (for native tooling and to
retire the in-pod subscription-auth complexity) creates the classic "edit locally, execute remotely"
problem. Two levers are needed: (1) redirect the harness's *command execution* into the sandbox, and
(2) keep *files* consistent between the local tree and the sandbox. Constraints from paddock's threat
model: the sandbox has **no SSH server and no service-account token**, and today the CLI reaches the
cluster with the developer's kubeconfig (as `paddock attach` does).

## Goals / Non-Goals

**Goals:**
- Make it irrelevant that the harness's native file tools run locally, by syncing the workspace both
  ways.
- Reuse proven, industry-standard building blocks instead of hand-rolling sync.
- Keep the mode entirely client-side: no control-plane or agent-image changes.
- Preserve governed egress + audit for the agent's actions.

**Non-Goals:**
- Model-call metering / budget enforcement in this mode (dropped by decision; the harness uses the
  developer's own credentials).
- "Code never on the endpoint" / DLP — sync implies a full local mirror. If that ever matters, a
  FUSE/remote-first branch is the alternative, out of scope here.
- Strict edit-before-exec ordering guarantees in v1 (see Risks).
- A server-side exec/sync relay (kubeconfig-free path) — future work.

## Decisions

- **Command redirection via a Claude Code `PreToolUse` hook that rewrites Bash to `paddock exec`.**
  Verified: the hook can replace a tool's input (`hookSpecificOutput.updatedInput.command`). Native
  Read/Edit/Glob bypass the hook and cannot be swapped for another tool, which is *why* file
  consistency is handled by sync rather than redirection. *Alternative considered:* MCP-only file
  tools (deny native, expose `paddock_edit`) — rejected: clunky UX, model must cooperate.

- **Bidirectional file sync via DevSpace's sync engine.** Research compared Mutagen and DevSpace.
  **Mutagen has no first-class Kubernetes support without SSH** (the `kubectl exec` transport
  workaround fails), and paddock's sandbox deliberately has no SSH — so Mutagen is out. **DevSpace
  sync is bidirectional, runs entirely over the Kubernetes API with no SSH and no server-side
  component or special privileges** (it injects a tiny helper via `kubectl cp` = tar-over-exec,
  which is the exact mechanism paddock's `push`/`pull` already use), handles deletions and conflicts
  with configurable strategies, and is usable standalone (`devspace sync`). It fits paddock's
  constraints almost exactly. *Alternative considered:* hand-rolled hook-driven copy (PostToolUse
  push + post-Bash tree reconciliation) — rejected: re-implements a sync engine and gets deletions,
  renames, `.git`, and ordering wrong.

- **`paddock sync` shells out to `devspace` in v1** rather than vendoring its Go packages. Least
  code, fastest to test. Vendoring to run over paddock's own governed/audited channel (and the
  future websocket relay) is deferred until that relay exists.

- **Conflict policy: two-way with newest-wins (`preferNewest`-style)**, since both sides mutate the
  tree (local edits and in-sandbox commands). The sandbox is treated as authoritative for
  execution-produced state (e.g. `.git`), but timestamps arbitrate concrete conflicts.

- **No control-plane/agent-image change.** DevSpace injects its helper at runtime; the agent image
  already has `tar` and a writable `/workspace` + `/tmp`. This keeps the mode a pure client concern.

## Risks / Trade-offs

- **Edit→exec ordering race** (a local edit may not be synced before a redirected Bash command
  runs). → v1 relies on DevSpace's sub-second sync and accepts eventual consistency; testing will
  show whether a `PreToolUse`-Bash "sync barrier" is needed. Documented as an Open Question, not
  built yet (anti-overengineering).
- **Second-writer conflicts** (developer edits the same tree in another tool/IDE while a session is
  live). → Rule: treat the bound directory as the harness's during a session; DevSpace's two-way
  conflict handling covers accidental overlaps without data loss.
- **New external dependency (`devspace`).** → Documented install (`brew install devspace`); `paddock
  sync` fails with a clear message if it is missing.
- **Kubeconfig on the developer's machine** (v1). → Same posture as `attach` today; removed when the
  server-side relay lands.
- **Full local mirror = code on the laptop.** → Accepted; DLP is a non-goal here.
- **Metering absent.** → Documented per-mode; governed egress + audit still apply to tool actions.

## Migration Plan

- Additive only. In-pod mode (`run`/`attach`) is untouched; no schema or API changes.
- Rollout is client-side: publish the new CLI (`go install`) and document `brew install devspace`.
  The deployed control plane needs no change beyond being current.
- Rollback: users simply stop using `paddock dev`/`sync`; nothing persistent is altered on the
  cluster.

## Open Questions

- Does testing reveal an ordering problem serious enough to justify a `PreToolUse`-Bash sync barrier
  (or a DevSpace "flush" step) before each remote command?
- Should `.git` be synced wholesale, or should git operations be pulled back more surgically to avoid
  churn on large histories?
- When the websocket relay lands, do we vendor DevSpace's sync library to run over paddock's governed
  channel and drop the client kubeconfig requirement?
