## ADDED Requirements

### Requirement: Harness-adapter selection
The system SHALL support multiple coding harnesses through per-harness adapters selected by
`paddock init --agent <name>`, defaulting to the harness detected in the directory. Each adapter
SHALL install its harness's own configuration to redirect the shell tool into the sandbox, wire
session lifecycle to paddock's neutral hooks, and deny native web tools unless `--allow-web-tools` is
given. The paddock core (sessions, `exec`, `sync`, reuse, reaping) SHALL remain harness-neutral.

#### Scenario: Explicit harness selection installs that harness's config
- **WHEN** `paddock init --agent opencode` runs in a project
- **THEN** the opencode adapter's configuration is installed (and not Claude Code's)

#### Scenario: Default detects the harness
- **WHEN** `paddock init` runs with no `--agent` in a directory configured for a known harness
- **THEN** that harness's adapter is used

#### Scenario: An unknown harness is rejected clearly
- **WHEN** `paddock init --agent <unknown>` runs
- **THEN** it fails with a message listing the supported harnesses

### Requirement: opencode support
The system SHALL support opencode as a harness: `paddock init --agent opencode` installs an opencode
plugin whose pre-execution hook rewrites the `bash` tool's command to run in the sandbox via
`paddock exec`, blocks the native `webfetch` tool by default, and ensures the directory's sandbox and
sync are up when the harness starts.

#### Scenario: opencode bash runs in the sandbox
- **WHEN** opencode runs a shell command in a directory set up with the opencode adapter
- **THEN** the command executes in the paddock sandbox, not on the local machine

#### Scenario: opencode web tool is denied by default
- **WHEN** opencode attempts to use its `webfetch` tool and web tools were not allowed at init
- **THEN** the call is blocked with a message pointing to sandboxed shell fetches

#### Scenario: opencode reuses the directory's sandbox
- **WHEN** opencode starts in a directory already bound to a running session
- **THEN** that sandbox is reused rather than a new one created
