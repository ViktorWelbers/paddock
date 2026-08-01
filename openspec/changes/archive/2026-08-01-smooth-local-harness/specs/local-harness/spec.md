## ADDED Requirements

### Requirement: Directory-scoped session reuse
The system SHALL bind at most one active sandbox to a project directory. When setup runs for a
directory already bound to a still-running session, it SHALL reuse that session rather than create a
new one; it SHALL create (and record) a new session only when there is no binding or the bound
session is no longer running.

#### Scenario: Reopening reuses the existing sandbox
- **WHEN** setup runs in a directory whose `.paddock/session` names a session that is still running
- **THEN** that session is reused and no new sandbox is created

#### Scenario: A reaped binding is replaced
- **WHEN** setup runs and the bound session is gone (reaped or removed)
- **THEN** a new session is created and `.paddock/session` is updated

### Requirement: Automatic hook-driven lifecycle
The system SHALL provide `paddock init` that installs Claude Code lifecycle hooks so the mode runs
without manual start/stop: a `SessionStart` hook that ensures a bound sandbox exists (find-or-create)
and its workspace sync is running, and a `SessionEnd` hook that stops the sync. After `init`, running
the harness in the directory SHALL be sufficient — no explicit `paddock dev`/`down` is required.

#### Scenario: Just running the harness sets everything up
- **WHEN** a developer has run `paddock init` in a project and then starts Claude Code there
- **THEN** a sandbox is ensured, sync is running, and Bash tool calls execute in the sandbox — with no
  other command run by the developer

#### Scenario: Ending the session stops the sync
- **WHEN** the Claude Code session ends
- **THEN** the workspace sync for that directory is stopped

### Requirement: Web-tool governance in local-harness mode
The system SHALL deny the harness's native `WebFetch`/`WebSearch` tools by default when setting up
local-harness mode, because they run in the local harness and bypass the sandbox's governed egress
(an ungoverned exfiltration path); the only network path then is the governed, audited sandbox shell.
An `--allow-web-tools` option SHALL leave those tools enabled for developers who accept ungoverned
native web access.

#### Scenario: Web tools denied by default
- **WHEN** `paddock init` runs without `--allow-web-tools`
- **THEN** the Claude Code settings deny `WebFetch` and `WebSearch`

#### Scenario: Opt back in
- **WHEN** `paddock init --allow-web-tools` runs
- **THEN** `WebFetch`/`WebSearch` are not denied, and the ungoverned exposure is documented

### Requirement: Activity-based idle reaping
Running sessions SHALL record a last-activity time, updated by session use (a client heartbeat while
the sync is running, and existing gateway/egress events). The server SHALL tear down and mark
`expired` any running session whose last activity is older than a configurable idle timeout
(`--max-session-idle`), independently of the absolute session-age cap, so a forgotten, crashed, or
slept session is always reclaimed without the developer removing anything.

#### Scenario: In-use session stays alive
- **WHEN** a session's sync supervisor is running and heartbeating
- **THEN** the session's last-activity stays current and the idle reaper does not reap it

#### Scenario: Idle session is reaped
- **WHEN** a session has had no activity for longer than the idle timeout
- **THEN** its sandbox is torn down and the session is marked expired, invalidating its token

#### Scenario: Crash or sleep is still reclaimed
- **WHEN** the harness crashes or the machine sleeps so heartbeats stop
- **THEN** the session goes idle and is reaped by the timeout, with no developer action
