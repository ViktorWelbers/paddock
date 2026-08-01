# local-harness Specification

## Purpose
TBD - created by archiving change local-harness-mode. Update Purpose after archive.
## Requirements
### Requirement: Remote command execution
The system SHALL provide `paddock exec <session-id> [--b64] <command…>` that runs a command inside
the session's sandbox, streams its stdout and stderr to the caller, and exits with the command's
own exit code. With `--b64` the single argument SHALL be a base64-encoded command string, so a
harness hook can pass arbitrary shell without quoting corruption.

#### Scenario: Command runs in the sandbox, not locally
- **WHEN** a developer runs `paddock exec <id> hostname` for a running session
- **THEN** the output is the sandbox pod's hostname, not the local machine's

#### Scenario: Exit code is propagated
- **WHEN** the executed command exits non-zero (e.g. `paddock exec <id> "exit 7"`)
- **THEN** `paddock exec` exits with the same code

#### Scenario: Base64 command survives quoting
- **WHEN** the hook invokes `paddock exec <id> --b64 <base64-of("echo $(hostname)")>`
- **THEN** the command is decoded and executed verbatim in the sandbox

### Requirement: Harness Bash redirection
The system SHALL provide a Claude Code `PreToolUse` adapter (`paddock hook-bash`) that rewrites a
Bash tool call so it runs in the bound session's sandbox via `paddock exec`. The session SHALL be
resolved from the `PADDOCK_SESSION` environment variable or the nearest `.paddock/session` file. If
no session is bound or the input is malformed, the adapter SHALL be a no-op so the harness is never
wedged.

#### Scenario: Bash tool call is redirected
- **WHEN** the harness is about to run a Bash command and the directory is bound to a session
- **THEN** the adapter emits `hookSpecificOutput.updatedInput.command` rewritten to
  `paddock exec <id> --b64 <cmd>`

#### Scenario: Unbound directory is a no-op
- **WHEN** no `PADDOCK_SESSION` is set and no `.paddock/session` exists
- **THEN** the adapter produces no rewrite and the command runs locally

#### Scenario: Native file tools are not redirected
- **WHEN** the harness uses a native Read/Edit/Glob tool
- **THEN** the adapter does not apply (only Bash is redirected); file consistency is provided by
  workspace synchronization

### Requirement: Directory-to-session binding
The system SHALL provide `paddock init-local <session-id>` that records the binding in
`.paddock/session` and installs the Bash redirection hook into `.claude/settings.local.json`
without discarding any settings the developer already configured. Re-running it SHALL be idempotent.

#### Scenario: Hook is installed and directory is bound
- **WHEN** a developer runs `paddock init-local <id>` in a project
- **THEN** `.paddock/session` contains the id and `.claude/settings.local.json` contains a
  `PreToolUse` Bash hook invoking `paddock hook-bash`

#### Scenario: Existing settings are preserved
- **WHEN** `.claude/settings.local.json` already contains unrelated settings or hooks
- **THEN** those are retained and the paddock hook is merged in, not overwritten

### Requirement: Bidirectional workspace synchronization
The system SHALL provide `paddock sync <session-id>` that keeps the local working directory and the
sandbox `/workspace` consistent in **both** directions in real time — propagating creations,
modifications, and deletions — so that native (local) file tools and in-sandbox commands observe the
same tree. Synchronization SHALL run over the Kubernetes API without requiring SSH or a
service-account token in the sandbox.

#### Scenario: Local edit reaches the sandbox
- **WHEN** a file is created or modified locally while `paddock sync` is running
- **THEN** the same change appears in the sandbox `/workspace`

#### Scenario: Sandbox change reaches local
- **WHEN** a command in the sandbox modifies, creates, or deletes files under `/workspace`
  (including `.git` state from a commit or checkout)
- **THEN** the corresponding change appears in the local working directory

#### Scenario: No SSH in the sandbox
- **WHEN** synchronization is established
- **THEN** it succeeds against a sandbox that has no SSH server and no service-account token

### Requirement: One-command setup
The system SHALL provide `paddock dev <agent>` that creates a detached session for the current
directory, binds the directory (installing the hook), and starts workspace synchronization, so a
developer reaches a working local-harness session in a single command.

#### Scenario: Single command prepares the mode
- **WHEN** a developer runs `paddock dev claude` in a project
- **THEN** a session is created, the directory is bound, sync is running, and the developer can
  start their local harness with Bash calls executing in the sandbox

### Requirement: Governance posture of local-harness mode
In local-harness mode the system SHALL continue to govern the agent's actions — commands execute in
the sandbox whose egress is allowlisted and audited — while model-call metering is NOT applied,
because the harness calls the model directly with the developer's own credentials. Documentation
SHALL state this trade-off explicitly so the mode's guarantee is not overstated.

#### Scenario: Tool egress stays governed
- **WHEN** an in-sandbox command reaches the internet (e.g. `pip install`)
- **THEN** it passes through the governed egress proxy and is audited, exactly as in in-pod mode

#### Scenario: Metering is absent by design
- **WHEN** the local harness makes model API calls
- **THEN** those calls are not routed through paddock's metering gateway and are not billed against a
  budget, and this limitation is documented for the mode

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

