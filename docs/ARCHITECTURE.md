# Paddock — Architecture

## Components

| Component | Binary | Role |
|---|---|---|
| Control plane | `paddock-server` | Session CRUD, budget ledger, audit store, sandbox provisioning |
| Data plane | `paddock-gateway` | LLM API reverse proxy (token metering, budget enforcement), server-side MCP mux, credential broker, policy enforcement point |
| CLI | `paddock` | Developer entrypoint: `run`, `ls`, `budget`, `attach` |

Both server and gateway are single static Go binaries. MVP storage is SQLite (one file, zero-dependency self-hosting); the storage layer is small and Postgres is a straight swap when multi-replica is needed.

## Session lifecycle

1. `paddock run claude` → `POST /v1/sessions` on `paddock-server`.
2. Server checks the user's budget has headroom, writes the session row, and asks the **sandbox provisioner** to create, in the control plane's own namespace:
   - a **Pod** named `paddock-ses-<id>` from an agent image (Claude Code preinstalled), with CPU/memory limits, no service-account token, all capabilities dropped, and env `ANTHROPIC_BASE_URL=<gateway>/anthropic` plus a session-scoped token — *never a real provider key*,
   - a **NetworkPolicy** of the same name, selecting that one pod, allowing egress only to the gateway's ports (+ DNS).
3. The agent runs. All model traffic goes through the gateway because the sandbox literally cannot reach anywhere else.
4. `DELETE /v1/sessions/{id}` (or TTL) deletes the pod and its policy. Audit events outlive the session.

Sessions are pods, not namespaces. The isolation that does the work is per-pod — the NetworkPolicy, the container limits, the missing service-account token — and none of it came from the namespace boundary. What a namespace *did* cost was cluster-scoped RBAC to create and delete namespaces on demand, which is exactly the permission a platform team is least willing to grant. The server now needs a plain Role in one namespace. The API reports each session's `namespace` and `pod` so clients never reconstruct the layout themselves.

The one thing the shared namespace takes away is the per-session `ResourceQuota` — quotas are namespace-scoped, and in a shared namespace it would meter the control plane too. Its caps survive elsewhere: CPU and memory as container limits, and the pod/service/secret counts by the agent having no API credentials to create anything with.

## Gateway data path

For each model API request:

```
sandbox ── session token ──▶ gateway
                              1. authenticate session token → session, budget
                              2. budget check: hard cap → 402 Payment Required
                              3. swap in real provider key (from broker), proxy upstream
                              4. parse `usage` from response (JSON or SSE message_delta)
                              5. price via model price table → debit ledger
                              6. append audit event (model, tokens, cost, session)
```

Streaming: Anthropic SSE responses carry usage in `message_start` / `message_delta` events; the gateway scans the stream as it relays it. Non-streaming responses carry a top-level `usage` object.

For each MCP call:

```
sandbox ──▶ gateway /mcp/{server}
             1. server on allowlist? (central registry, not developer YAML)
             2. OPA decision: input {user, session, server, tool, args} → allow/deny
             3. inject server credentials (broker) — sandbox never holds them
             4. relay, append audit event with the decision
```

## Budgets

Hierarchical ledger: `org → team → user → session`. Each node has a limit and accumulated spend; a debit walks up the chain and fails if any ancestor is exhausted. Soft thresholds emit warning events (surface: CLI + audit log). Price table maps model → €/Mtok input/output; overridable in config because list prices drift.

## Policies

Embedded OPA (`open-policy-agent/opa/rego`, no sidecar). Policies are plain `.rego` files loaded from a directory — reviewable in git, testable with `opa test`, compatible with existing Gatekeeper/Conftest workflows. Decision input is a stable JSON document (`docs/` will pin the schema); default package `paddock.authz`, rule `allow` with optional `reason`.

## Audit

Append-only `events` table: `ts, session_id, actor, kind, payload(JSON)`. Kinds: `session.created`, `model.call`, `budget.warn`, `budget.exhausted`, `mcp.call`, `policy.denied`, `session.deleted`. The enterprise tier adds hash-chaining (tamper evidence) and SIEM/OTLP export; OSS keeps the same schema so evidence is portable.

## Open-core boundary (technical)

OSS: everything above. Enterprise: SSO/SAML (auth middleware), chargeback exporters, report-pack generators (DORA/AI-Act templates over the audit store), signed audit log, vetted-MCP registry feed (external data service the gateway can subscribe to). The boundary is additive modules, not forked internals.

## Isolation roadmap

MVP: namespaced pods + NetworkPolicy + quotas + no mounted secrets (threat model: careless/compromised agent, not hostile kernel exploits). Next: optional `runtimeClassName: gvisor`, then Kata/microVMs for customers who demand hardware isolation. The pod spec is rendered in one place (`internal/sandbox`), so isolation upgrades are config, not rearchitecture.
