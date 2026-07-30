# Paddock — Next Steps Plan

This is the **ordered execution plan** for the current push: enterprise-readiness
hardening followed by the bigger M2 items. It complements
[docs/ROADMAP.md](docs/ROADMAP.md) — the roadmap is the milestone list, this file
is what to build next and how. Work top-first; each item stands alone. New
contributors: start with [AGENTS.md](AGENTS.md) for orientation, then
[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for design detail.

Each item below: **Problem · Approach · Tests · Done-when.**

---

## 1. Budget-endpoint authorization *(next)*

**Problem.** `listBudgets` (`internal/api/api.go`) ignores the caller entirely and
`getBudget` has no owner check, so any authenticated user can read every budget's
name, limit, and spend — including other teams'. This is an information-disclosure
gap: sessions are already scoped to their owner, but budgets are wide open.

**Approach.** Make both handlers caller-aware via `caller(r)` (as the session
handlers do). An admin (`id.IsAdmin()`) sees all budgets. A non-admin sees only the
budgets their own sessions reference — `SELECT DISTINCT budget_id FROM sessions
WHERE user = ?` — expanded up the `ParentID` chain so a developer can see their own
spend chain (user → team → org) but not sibling teams'. `getBudget` returns **404**
(not 403) for a budget outside that set, matching the `mayAccess` pattern already in
`internal/api/api.go` (a 403 would confirm the budget exists). Add a small
`budget.Ledger` (or `api.Store`) helper to compute the visible set.

**Tests.** `internal/api/api_test.go`: a non-admin sees only referenced budgets plus
their ancestors; an admin sees all; an unreferenced budget id is a 404 for the
non-admin.

**Done-when.** A non-admin token cannot enumerate another team's budget.

---

## 2. Idle-TTL reaper + last-activity tracking

**Problem.** A detached session holds its pod's CPU and memory until the absolute
`--max-session-age` (default 24h in the chart). There is no idle-based reaping, so a
sandbox abandoned after five minutes still costs for 24 hours.

**Approach.** Add a `sessions.last_active` column (migration via `ALTER TABLE … ADD
COLUMN`, back-filled to `created_at`). The gateway already calls `Store.ByToken` on
every proxied call (`internal/gateway/gateway.go`, `cmd/paddock-gateway/main.go`) —
bump `last_active` there, throttled (only write if the stored value is more than
~60s stale) so a busy session doesn't cause a write per token. Extend
`ReapExpired` (or add a sibling `ReapIdle`) in `internal/api/reconcile.go` to end
sessions whose `last_active` is older than a new `--max-session-idle`, reusing the
existing `reapLoop` ticker in `cmd/paddock-server/main.go`. Distinguish the two in
the audit trail with a `reason` on the `session.expired` payload (`"idle"` vs
`"max_age"`). New chart value `server.maxSessionIdle`.

**Tests.** `internal/api/reconcile_test.go`: an idle session past the idle cap is
reaped; a recently-active one is spared; the absolute-age cap still works.

**Done-when.** An idle sandbox is torn down and its token invalidated on the idle
deadline, independent of absolute age.

---

## 3. Session-token hashing at rest

**Problem.** Session tokens are stored in plaintext in SQLite (`sessions.token`,
with a unique index). The control-plane database is therefore a standing store of
live sandbox credentials.

**Approach.** Store `sha256(token)` instead of the raw token; `ByToken` hashes the
presented token and compares digests — the same shape `auth.Tokens` already uses for
API callers. The plaintext is returned exactly once on session create (already true)
and thereafter lives only in the pod. Keep the unique index, on the hash column.
Migration: hashing can only apply going forward (there is no plaintext to re-hash),
so bump the schema and document that sessions created before the upgrade must be
recreated.

**Tests.** `internal/api/api_test.go`: `ByToken(plaintext)` still authenticates; the
stored column holds a digest, not the token.

**Done-when.** `select token from sessions` shows only digests.

---

## 4. Audit pagination + retention

**Problem.** `GET /v1/sessions/{id}/events` returns every row a session ever produced
(governed egress alone writes dozens per `pip install`), with no `limit`, no cursor,
and no cross-session query. Nothing prunes, so the append-only `audit_events` table
grows forever inside the same SQLite file. Pagination and a bounded store are also
what a future SIEM export will need.

**Approach.** Add a paginated query to `internal/audit/audit.go` —
`BySessionPage(sessionID, afterID, limit)` — using the existing `(session_id, id)`
index; the handler reads `?limit=` and `?after=` and returns a next cursor. Add a
retention sweep `DeleteOlderThan(d)` driven by a new `--audit-retention` flag on the
same `reapLoop` cadence, defaulting to off (keep-forever) so today's behavior is
unchanged unless an operator opts in.

**Tests.** `internal/audit` — paging returns stable, ordered pages with a correct
cursor; retention deletes only rows older than the cutoff.

**Done-when.** The events endpoint is bounded per request and old audit rows can be
pruned on a schedule.

---

## 5. OIDC authentication

**Problem.** Bearer tokens in a Secret are a file an operator hand-edits. An
enterprise wants its IdP to decide who exists, with group membership arriving in the
token rather than in a file.

**Approach.** A new `auth.OIDC` implementing the existing `auth.Authenticator`
interface: validate a bearer JWT against the IdP's JWKS and map claims to
`Identity{Subject, Groups}`. This changes only how a request becomes an `Identity` —
the ownership and admin logic downstream is untouched, which is the whole point of
the seam. Wire an `--auth-oidc-issuer` / `--oidc-audience` path in
`cmd/paddock-server/main.go` alongside `--auth-tokens`, and add a CLI device-code
login so a developer never handles a raw token. Larger than items 1–4; may split
into its own plan.

**Done-when.** A request bearing a valid IdP JWT is authorized with groups from the
token, and `paddock-admin` membership can arrive via a claim.

---

## 6. Server-side attach relay (websocket)

**Problem.** `paddock attach` (`cmd/paddock/attach.go`) is the last thing that talks
to the Kubernetes API from the developer's machine: it needs a kubeconfig,
`pods/exec` rights the developer probably shouldn't have, and it drags client-go
into the CLI (~36MB of the binary). Workspace transfer already proved the shape —
`internal/sandbox.Execer` behind an HTTP endpoint.

**Approach.** `GET /v1/sessions/{id}/attach` upgrades to a websocket, relays the exec
stream through `Execer`, and carries terminal resize. When it lands the CLI is pure
HTTP, `go install` produces a small binary, and `PADDOCK_KUBECONFIG` leaves the
docs.

**Done-when.** `paddock attach` works with no kubeconfig and client-go is gone from
the CLI build.

---

## 7. Published container images + Helm repo

**Problem.** For an outside user, "try paddock" still implies building images first.
GHCR is already the chart's default image; this formalizes the rest.

**Approach.** Ensure `.github/workflows/images.yaml` publishes versioned public GHCR
tags on release (not only `:latest`), and publish the Helm chart to a repo — GitHub
Pages (`gh-pages` `index.yaml`) or a GHCR OCI chart. Document `helm repo add … &&
helm install`.

**Done-when.** A stranger installs paddock from public images plus `helm repo add`,
building nothing.

---

## 8. Docs site / quickstart (<10 min on kind/k3d)

**Problem.** Onboarding is spread across README, ARCHITECTURE, and CONTRIBUTING.

**Approach.** A docs site (e.g. mkdocs-material) assembled from the existing markdown
plus the architecture diagrams under `docs/img/`, and a copy-pasteable kind/k3d
quickstart verified end-to-end by `make e2e`.

**Done-when.** A new user reaches a running `paddock run` in under ten minutes from
the quickstart alone.

---

## Beyond this plan

M3–M5 in [docs/ROADMAP.md](docs/ROADMAP.md) cover the rest: a Postgres backend,
named egress profiles, a signed (hash-chained) audit log with SIEM export,
productized report packs (DORA / EU AI Act), and a gVisor `runtimeClass` option.
