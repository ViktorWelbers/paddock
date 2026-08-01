## Context

Local-harness mode is a manual scaffold: `paddock dev` always creates, and start/stop/sync are
hand-run. Result: orphaned sandboxes and friction. The industry-smooth shape (Codespaces/dev
containers) is "just open the tool; the environment is there and self-manages." We approximate that
with Claude Code lifecycle hooks (client) plus a server-side idle reaper (backstop).

## Goals / Non-Goals

**Goals:** one sandbox per directory (reuse); zero manual start/stop; automatic cleanup that survives
crashes/sleep; close the WebFetch/WebSearch egress hole by default.

**Non-Goals (this change):** routing `paddock exec` through the server (audit of shell + dropping
kubeconfig) — orthogonal to smoothness, larger, and sync still needs kube access; a Claude Code
*marketplace plugin* package (hooks in settings suffice for now); async first-provision (accept a
one-time ~40s on genuine first create, instant on reuse).

## Decisions

- **Reuse via `.paddock/session` + a server liveness check.** Setup reads the bound id and does
  `GET /v1/sessions/{id}`; running → reuse, else create. This is the fix for the pile-up. *Alt:* a
  server-side "one session per (user,dir)" index — rejected: the server doesn't know client dirs.

- **Lifecycle via Claude Code `SessionStart`/`SessionEnd` hooks, installed by `paddock init`
  (per-project, opt-in).** SessionStart = find-or-create + ensure sync; SessionEnd = stop sync. *Alt:*
  global hooks that sandbox every session everywhere — rejected: too aggressive, and first-provision
  latency on every `claude`.

- **Idle signal = client heartbeat from the sync supervisor.** The detached sync becomes a small
  paddock process that runs `devspace` as a child *and* POSTs `/v1/sessions/{id}/heartbeat` every few
  minutes; it dies (killing devspace) on SIGTERM. So "sync running" ⇔ "session in use" ⇔ heartbeats.
  The server also treats existing gateway/egress events as activity. *Alt:* exec-through-server as the
  signal — deferred (bigger); *Alt:* reap purely on SessionEnd — rejected: misses crashes/sleep.

- **Idle reaper extends the existing `reapLoop`/`ReapExpired`.** New `--max-session-idle` (chart
  `server.maxSessionIdle`); reaps running sessions whose `last_active` is older than the timeout,
  reusing the sandbox teardown + `expired` status path (which already invalidates the token via
  `ByToken`). Keeping a session *warm* between opens (not reaping on close) gives instant reuse; the
  idle timeout is the cleanup.

- **Web-tool deny by default** in the settings `permissions.deny` written by `init`/`dev`;
  `--allow-web-tools` omits it. Closes the ungoverned WebFetch exfil path so "all egress governed"
  holds for the mode.

## Risks / Trade-offs

- **Mid-session reap while Claude Code is open but idle (no bash).** → The heartbeat runs off the
  *sync supervisor*, which lives for the whole session regardless of bash activity, so an open session
  keeps heartbeating and won't be reaped. The idle timeout only elapses after SessionEnd/crash.
- **First-provision latency (~40s).** → One-time per directory; reuse makes reopens instant. SessionStart
  prints progress.
- **Hook doesn't fire (crash before SessionEnd).** → The idle reaper is exactly the backstop; heartbeats
  stop, the session ages out.
- **Chart template change** (new `--max-session-idle` arg). → Version bump + `targetRevision` +
  deploy, per the established runbook.
- **Reaping a session whose sync supervisor is still alive but server unreachable** (network blip). →
  Idle timeout is minutes; a brief blip won't cross it. Reuse re-creates if it does.

## Migration Plan

- Additive. Existing `dev`/`down`/`sync`/`exec` keep working. `paddock init` is new and opt-in per
  project. Server gains a nullable `last_active` (back-filled to `created_at`) and one flag.
- Rollout: server first (chart bump + deploy with `maxSessionIdle`), then publish the CLI. Idle
  reaping stays off if `--max-session-idle` is unset (0), so behavior is unchanged until configured.

## Open Questions

- Default idle timeout? (~15–20m proposed.)
- Should `paddock init` also offer a global install later, or stay per-project?
- Fold the exec-through-server relay in next (audit of shell commands + kubeconfig drop)?
