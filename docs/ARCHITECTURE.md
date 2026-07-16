# Paddock — Architecture

## Components

| Component | Binary | Role |
|---|---|---|
| Control plane | `paddock-server` | Session CRUD, budget ledger, audit store, sandbox provisioning, workspace transfer |
| Data plane | `paddock-gateway` | LLM API reverse proxy (token metering, budget enforcement), server-side MCP mux, credential broker, egress CONNECT proxy, policy enforcement point |
| CLI | `paddock` | Developer entrypoint: `run`, `push`/`pull`, `ls`, `budget`, `events`, `attach` |

Both server and gateway are single static Go binaries. MVP storage is SQLite (one file, zero-dependency self-hosting); the storage layer is small and Postgres is a straight swap when multi-replica is needed.

## Session lifecycle

1. `paddock run claude` → `POST /v1/sessions` on `paddock-server`.
2. Server checks the user's budget has headroom, writes the session row, and asks the **sandbox provisioner** to create, in the control plane's own namespace:
   - a **Pod** named `paddock-ses-<id>` from an agent image (Claude Code preinstalled), with CPU/memory limits, no service-account token, all capabilities dropped, and env `ANTHROPIC_BASE_URL=<gateway>/anthropic` plus a session-scoped token — *never a real provider key*,
   - a **NetworkPolicy** of the same name, selecting that one pod, allowing egress only to the gateway's ports (+ DNS).
3. The CLI uploads the working directory into the pod's `/workspace` (see below).
4. The agent runs. All model traffic — and all internet traffic — goes through the gateway because the sandbox literally cannot reach anywhere else.
5. `DELETE /v1/sessions/{id}` (or TTL) deletes the pod and its policy. Audit events outlive the session.

The NetworkPolicy's gateway rule is **port-scoped**, which matters more than it looks: the server and gateway share a pod (and therefore an IP), so an unscoped rule would let sandboxes reach the control-plane API. They may reach the gateway's ports (8081, model + MCP; 8082, egress proxy) and nothing else.

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

## Egress data path

The sandbox has no route to the internet; the gateway has the only door. Agents get `HTTP_PROXY`/`HTTPS_PROXY` (and the lowercase forms — curl reads only those) pointing at the gateway's CONNECT proxy, authenticated with the session token. `NO_PROXY` covers the gateway host, or model calls would tunnel through the proxy back into the gateway they came from.

```
sandbox ── CONNECT pypi.org:443 ──▶ gateway :8082
                                     1. Proxy-Authorization → session (407 if not)
                                     2. host → allowlist groups (deny if none)
                                     3. port allowed? (443 by default)
                                     4. OPA: {kind:"egress", user, agent, host, port, groups}
                                     5. resolve DNS *here*, reject loopback/link-local/
                                        RFC1918/CGNAT/ULA — then dial the vetted IP
                                     6. 200, splice bytes, audit allowed/closed(+bytes)
```

Three properties are worth stating plainly:

- **It tunnels, it does not intercept.** TLS is end-to-end between the agent and pypi; the proxy sees a hostname and a byte count, never plaintext. No CA to distribute, no certificate pinning to break, and paddock is not a place your source code can leak from.
- **DNS is resolved by the proxy, not the client**, and the resulting IP is what gets dialled. Otherwise an allowlisted name that resolves to `169.254.169.254` (or the kube API, or another namespace) would sail through. `allowedPrivateCIDRs` punches specific ranges back open; empty is the default, and in-cluster model upstreams are reached via the gateway's `/openai` reverse proxy rather than this path.
- **Default-deny, always audited.** No allowlist means the listener runs and refuses everything. A denial names its reason (`not_in_allowlist`, `port_not_allowed`, `policy_denied`, `private_address`, `ip_literal`, `resolve_failed`, `unauthenticated`), and every closed tunnel records bytes in and out. That trail is the point: it's what turns "the agent had internet" into "here is precisely what it fetched, and what it tried to."

## Workspace transfer

Files move through the **server**, never the CLI's own cluster access:

```
paddock run/push ── tar.gz ──▶ server ── pods/exec: tar -xzf - -C /workspace ──▶ sandbox
paddock pull     ◀── tar.gz ── server ◀── pods/exec: tar -czf - -C /workspace .
```

Streamed end to end — the request body is piped into the pod's stdin, so a large repo never lands in anyone's memory — and both directions are audited with byte counts and a sha256. The developer needs no kubeconfig and no `pods/exec` rights; the server holds them, which is the same trade `attach` will make once the websocket relay lands.

Direction matters for trust. Pushing into the pod needs no path sanitisation: `tar` runs as the agent's own uid inside the agent's own container, so a hostile archive reaches only what the agent already could. **Pulling is the dangerous direction** — the archive was assembled from a directory the agent could write — so the CLI extracts through `os.Root`, which confines every write to the target directory at the kernel level, and additionally refuses `..`/absolute entries up front and caps total bytes.

In a git repo the upload set is `git ls-files -co --exclude-standard` plus `.git`, so `.gitignore` is the contract and `node_modules` stays home.

## Budgets

Hierarchical ledger: `org → team → user → session`. Each node has a limit and accumulated spend; a debit walks up the chain and fails if any ancestor is exhausted. Soft thresholds emit warning events (surface: CLI + audit log). Price table maps model → €/Mtok input/output; overridable in config because list prices drift.

## Policies

Embedded OPA (`open-policy-agent/opa/rego`, no sidecar). Policies are plain `.rego` files loaded from a directory — reviewable in git, testable with `opa test`, compatible with existing Gatekeeper/Conftest workflows. Package `paddock.authz`; `allow` must be true for the call to proceed, and entries in `deny` become the reasons shown to the developer and written to the audit log. Evaluation fails closed.

Decision input:

| Field | Kinds | Notes |
|---|---|---|
| `kind` | all | `tool_call` \| `mcp_call` \| `egress` |
| `user`, `session` | all | |
| `agent` | all | `claude`, `pi`, ... |
| `tool`, `args` | `tool_call` | |
| `server` | `mcp_call` | registry name |
| `host`, `port` | `egress` | CONNECT target |
| `groups` | `egress` | allowlist groups the host matched |

One sharp edge worth knowing when writing rules: optional fields are **omitted** from the input document when empty, not sent as empty values. `count(input.groups) == 0` is therefore not how you catch an ungrouped host — the count is undefined, the rule body fails, and it passes everything while looking correct. Test for absence instead (`not has_groups` with `has_groups if count(input.groups) > 0`), as `policies/egress.rego` does.

## Audit

Append-only `events` table: `ts, session_id, actor, kind, payload(JSON)`. Kinds: `session.created`, `session.deleted`, `model.call`, `budget.warn`, `budget.exhausted`, `mcp.call`, `policy.denied`, `egress.allowed`, `egress.denied`, `egress.closed`, `workspace.push`, `workspace.pull`. The enterprise tier adds hash-chaining (tamper evidence) and SIEM/OTLP export; OSS keeps the same schema so evidence is portable.

## Open-core boundary (technical)

OSS: everything above. Enterprise: SSO/SAML (auth middleware), chargeback exporters, report-pack generators (DORA/AI-Act templates over the audit store), signed audit log, vetted-MCP registry feed (external data service the gateway can subscribe to). The boundary is additive modules, not forked internals.

## Isolation roadmap

MVP: pods + a port-scoped NetworkPolicy + container limits + no mounted secrets + no service-account token (threat model: careless/compromised agent, not hostile kernel exploits). Next: optional `runtimeClassName: gvisor`, then Kata/microVMs for customers who demand hardware isolation. The pod spec is rendered in one place (`internal/sandbox`), so isolation upgrades are config, not rearchitecture.
