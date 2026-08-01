## Why

Local-harness mode only supports Claude Code — the redirection, lifecycle hooks, and web-tool deny
are hard-wired to Claude Code's settings format. But paddock's *core* (sessions, sandbox, gateway,
`paddock exec`, `paddock sync`, reuse, heartbeat, idle reaper) is already harness-neutral, and every
target harness offers the same interception primitives, just via a different config surface. RTK
ships a per-agent installer for exactly this reason. paddock should too.

## What Changes

- Introduce a **harness-adapter seam**: an `Adapter` interface with one built-in implementation per
  harness, selected by `paddock init --agent <name>` (with auto-detection). Each adapter installs
  its harness's config to (1) redirect the shell tool → `paddock exec`, (2) wire session lifecycle →
  the neutral `paddock hook-session-start`/`-end`, and (3) deny native web tools unless
  `--allow-web-tools`.
- **Refactor** today's Claude-specific setup behind a `claudeAdapter` (no behavior change): writes
  `.claude/settings.local.json` hooks + `permissions.deny`.
- Add an **`opencodeAdapter`**: writes `.opencode/plugins/paddock.js` — a plugin whose
  `tool.execute.before` rewrites the `bash` command to `paddock exec <id> --b64 <cmd>` and throws on
  `webfetch` (unless web tools allowed), and which runs `paddock hook-session-start` on load.
- Make `hook-session-start` harness-neutral (ensure session + sync; no longer installs Claude's hook,
  which is now the adapter's job at `init`).
- Not BREAKING: in-pod mode, and the existing `dev`/`down`/`exec`/`sync`/`init-local` commands, are
  unchanged. `paddock init` defaults to the detected/Claude adapter, matching current behavior.

## Capabilities

### Modified Capabilities
- `local-harness`: add requirements for the harness-adapter seam and opencode support. (Existing
  requirements unchanged; the Claude behavior they describe now runs via `claudeAdapter`.)

## Impact

- **Client only:** new `cmd/paddock/adapter*.go` (interface + registry + claude + opencode adapters);
  `paddock init` gains `--agent`; `hook-session-start` refactored to be harness-neutral. No
  control-plane or agent-image change.
- **New generated artifact:** `.opencode/plugins/paddock.js` for opencode projects.
- **Follow-up (noted, not in scope):** a pi adapter (pi has an equivalent `tool_call` extension
  hook); cleaner opencode teardown (opencode lacks a reliable "harness exited" event, so on close it
  relies on `paddock down` or the idle/absolute reaper rather than a SessionEnd-style hook).
