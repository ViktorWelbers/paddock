## 1. Server: activity tracking + idle reaper

- [ ] 1.1 Add `sessions.last_active` (migration: ADD COLUMN, back-fill to `created_at`); helpers to read/set it
- [ ] 1.2 `POST /v1/sessions/{id}/heartbeat` — owner-checked; updates `last_active` to now
- [ ] 1.3 Treat model/egress audit writes as activity too (bump `last_active` where the gateway records them) — or document heartbeat-only for v1
- [ ] 1.4 `ReapIdle(ctx, idle)` in `internal/api/reconcile.go`: reap running sessions with `last_active` older than idle; audit `session.expired` with `reason: "idle"`
- [ ] 1.5 `--max-session-idle` flag + wire into `reapLoop`; chart value `server.maxSessionIdle` + deployment arg; bump `Chart.yaml`
- [ ] 1.6 Tests: heartbeat updates last_active; idle session reaped; active (recent) session spared; absolute cap still works

## 2. Client: session reuse (find-or-create)

- [ ] 2.1 `ensureSession(agent, ...)`: read `.paddock/session`; `GET /v1/sessions/{id}` → reuse if running, else `provisionSession` + rewrite `.paddock/session`
- [ ] 2.2 `paddock dev` uses `ensureSession` (no longer always-create); idempotent re-run

## 3. Client: sync supervisor + heartbeat

- [ ] 3.1 Detached sync becomes a paddock supervisor process (`paddock sync` re-exec'd via Setsid) that runs `devspace` as a child AND heartbeats `/heartbeat` every ~2m; SIGTERM kills devspace + exits
- [ ] 3.2 `.paddock/sync.pid` tracks the supervisor; `paddock down` stops it (already wired)

## 4. Client: automatic lifecycle hooks

- [ ] 4.1 `paddock init [--allow-web-tools]`: install `SessionStart`/`SessionEnd`/`PreToolUse` hooks + `permissions.deny` (WebFetch/WebSearch unless allowed) into `.claude/settings.local.json` without clobbering; record binding
- [ ] 4.2 `paddock hook-session-start` (hidden): ensureSession for cwd + start detached sync if not running
- [ ] 4.3 `paddock hook-session-end` (hidden): stop the sync supervisor for cwd
- [ ] 4.4 Fold web-tool deny into `paddock dev` too (with `--allow-web-tools`)

## 5. Docs

- [ ] 5.1 README/ARCHITECTURE: the smooth flow (`paddock init` once → just run `claude`); web-tool governance note; idle-reaper mention
- [ ] 5.2 Note `--max-session-idle` in the deploy/config docs

## 6. Build, validate, deploy, install

- [ ] 6.1 `go build ./... && go vet ./... && go test ./...` pass; `openspec validate smooth-local-harness --strict`
- [ ] 6.2 Commit + push; CI green
- [ ] 6.3 Chart bump → tag → infra `targetRevision` + `server.maxSessionIdle` → Argo refresh (server change)
- [ ] 6.4 Install CLI; run `paddock init` in the test project; smoke test: open Claude Code twice → same sandbox (reuse); idle → reaped
- [ ] 6.5 Archive the change
