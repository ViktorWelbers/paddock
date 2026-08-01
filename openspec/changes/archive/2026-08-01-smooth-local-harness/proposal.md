## Why

Local-harness mode works but isn't *smooth*: `paddock dev` always creates a new sandbox (so
restarting Claude Code piles up orphaned sandboxes — a developer accumulated three without
noticing), and the lifecycle is manual (`dev` to start, `down` to stop, babysit the sync). A
governance layer you have to remember to boot and kill is one people stop using. paddock should be
ambient: run your harness, and the sandbox appears, is reused, and cleans itself up.

## What Changes

- **Directory-scoped session reuse (find-or-create).** One sandbox per project directory: reopening
  reuses the existing running session instead of creating another, so sandboxes can't pile up.
- **Automatic lifecycle via Claude Code hooks.** `paddock init` installs `SessionStart` (find-or-
  create + start sync) and `SessionEnd` (stop sync) hooks, so a developer just runs `claude` — no
  `paddock dev`/`down`.
- **Server-side idle reaping.** Sessions carry a last-activity time; a client heartbeat (from the
  running sync supervisor) keeps an in-use session warm, and a new idle-TTL reaper tears down
  sessions inactive beyond `--max-session-idle` — so a forgotten/crashed/slept session is always
  cleaned up without the developer removing anything.
- **Web-tool governance.** `paddock init`/`dev` deny `WebFetch`/`WebSearch` by default (they run in
  the local harness, bypassing the governed sandbox egress — an ungoverned exfil path); `--allow-web-tools`
  opts out.
- Not BREAKING: in-pod mode and the existing local-harness commands are unchanged; the new behavior
  is additive.

## Capabilities

### Modified Capabilities
- `local-harness`: add requirements for session reuse, automatic hook-driven lifecycle, web-tool
  governance, and activity-based idle reaping. (Existing requirements unchanged.)

## Impact

- **Client:** new `paddock init` (hook installer) and `SessionStart`/`SessionEnd` hook handlers;
  find-or-create logic in the session path; the detached sync becomes a small paddock supervisor
  that runs devspace + heartbeats the server.
- **Server:** `sessions.last_active` column + a heartbeat endpoint; an idle-TTL reaper extending the
  existing `reapLoop`/`ReapExpired`; new `--max-session-idle` flag and chart value → **chart template
  change (version bump + deploy)**.
- **Config:** Claude Code settings gain `SessionStart`/`SessionEnd` hooks and a `permissions.deny`
  for web tools.
- **Deferred (non-goal here):** routing `paddock exec` through the server (audit of shell commands +
  dropping the client kubeconfig) — orthogonal to smoothness and larger; sync still needs kube
  access anyway, so kubeconfig can't be fully dropped yet.
