## 1. Remote execution + Bash redirection (spike — complete)

- [x] 1.1 `paddock exec <id> [--b64] <cmd>` streams stdout/stderr and propagates the exit code
- [x] 1.2 `paddock hook-bash` PreToolUse adapter rewrites Bash → `paddock exec` (env/file session resolution, safe no-op)
- [x] 1.3 `paddock init-local <id>` binds the directory and merges the hook without clobbering
- [x] 1.4 Unit tests for the hook rewrite; register commands in `cmd/paddock/main.go`
- [x] 1.5 Verified live: commands run in the pod, exit codes propagate, ~110ms/call

## 2. Bidirectional workspace sync

- [x] 2.1 Install `devspace` locally (`brew install devspace`) and confirm the standalone `devspace sync` flags for pod/namespace/container + conflict strategy
- [x] 2.2 `paddock sync <id>`: resolve the pod via `sessionLocation`, shell out to `devspace sync` for `./ ↔ /workspace` (container `agent`), bidirectional, newest-wins; stream status; Ctrl-C stops
- [x] 2.3 Clear error if `devspace` is not installed (point at the install command)
- [x] 2.4 Register `sync` in `cmd/paddock/main.go`

## 3. One-command setup

- [x] 3.1 `paddock dev <agent>`: create a detached session (reuse the `run --detach` path), bind the directory (`init-local`), then start `sync`
- [x] 3.2 Register `dev`; make the manual 3-step flow (`run --detach` → `init-local` → `sync`) also work and be documented

## 4. Docs (public repo, generic only)

- [x] 4.1 README/ARCHITECTURE: add the two-mode comparison (in-pod = structurally-metered; local-harness = governed execution sandbox, unmetered) with honest per-mode labeling
- [x] 4.2 Document the `devspace` dependency and the local-harness quickstart

## 5. Build, test, validate

- [x] 5.1 `go build ./... && go vet ./... && go test ./...` pass
- [x] 5.2 `openspec validate local-harness-mode --strict` passes

## 6. Deploy + install for testing (private tooling stays out of the OSS repo)

- [x] 6.1 Commit + push the CLI + docs; confirm CI is green (server image unchanged — CLI is client-only)
- [ ] 6.2 Ensure the control plane is current via Argo (already v0.5.0; refresh if needed)
- [x] 6.3 Install the local CLI (`go install …/cmd/paddock@main`) and `devspace`
- [ ] 6.4 Add a private `make` target (in the infrastructure repo, NOT the OSS repo) that runs the Argo refresh + local CLI/devspace install in one step
- [ ] 6.5 Smoke test end-to-end: `paddock dev claude` → local Claude Code edits sync + Bash runs in the sandbox
