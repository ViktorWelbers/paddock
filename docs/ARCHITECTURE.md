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
5. `DELETE /v1/sessions/{id}` deletes the pod and its policy. Audit events outlive the session.

A session ends one of four ways, and every one of them is terminal in the same place: the status column. `deleted` (the user asked), `failed` (the sandbox vanished under it — see reconciliation), or `expired` (it outlived its TTL). `ByToken` only serves `running` sessions, so *whichever* way a session ends, its sandbox token stops working at the gateway the instant the row changes — there is no separate revocation step to forget. The **TTL reaper** (`--max-session-age`, default 24h in the chart) sweeps on a ticker and, for any session past its age, tears the pod down and marks it `expired`. It keys off wall-clock age rather than the presence of a pod, so unlike the drift reconciliation it is safe to run continuously. This is what stops an idle sandbox from being both standing compute the operator pays for and a standing credential — the exposure a credential store should not leave open. (Absolute, not idle-based: idle detection needs per-session activity tracking that does not exist yet; on the roadmap.)

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

**Two ways to give the claude agent a model.** The path above is the **API-key** mode: the sandbox holds only a session token, the gateway swaps in the org's Anthropic key and meters every call against a budget — the governed default (`gateway.existingSecret`). The second mode is a personal **Claude subscription**, which has no API key: `sandbox.claudeOAuthSecret` points at a Secret holding `CLAUDE_CODE_OAUTH_TOKEN` (from `claude setup-token`), and `agentEnv` then injects that token by `secretKeyRef` (so it never enters the pod spec or etcd) and omits `ANTHROPIC_BASE_URL`/`ANTHROPIC_API_KEY`, so Claude Code authenticates straight to `api.anthropic.com`. The gateway can't sit in this path — a subscription OAuth token is `Authorization: Bearer` + an oauth beta header, not the `x-api-key` scheme the gateway proxies, and a flat-rate subscription has nothing per-token to meter. So these calls are **not** budget-metered, but they still leave only through the governed egress proxy (below), so `api.anthropic.com` must be on the egress allowlist and every call is audited `egress.allowed`/`egress.denied`. Governance minus metering — the most a subscription allows. One wrinkle: Claude Code's interactive TUI shows its "Select login method" wizard on first launch even with a valid `CLAUDE_CODE_OAUTH_TOKEN` (it auto-skips that screen only for `ANTHROPIC_API_KEY`), so the agent image bakes a minimal `~/.claude.json` with `hasCompletedOnboarding: true` and a pre-accepted folder-trust for `/workspace`; the token then authenticates transparently and `paddock run claude` drops straight into a session (see `Dockerfile.agent`).

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

## Git credentials (encrypted handoff)

An agent that can read the code but not `git push` is half a tool, so `paddock run` installs the repo's credentials into the sandbox. It reads them from **git's own helper chain** (`git credential fill`) — the macOS keychain, libsecret, `~/.git-credentials`, whatever already makes `git push` work on the laptop — for the https hosts the repo has remotes on, including enterprise hosts like `github.axa.com`. ssh remotes are skipped: a key and an agent are a different mechanism (ssh-agent forwarding is on the roadmap).

The credential is the developer's own and the agent works as the developer, so it is not a secret *from the agent*. It must, however, be a secret *from the control plane*: paddock operates the cluster, so an injection streamed in the clear would sit in server memory and in whatever records the kube API server's exec channel. So the handoff is encrypted end to end, and the plaintext never transits paddock:

```
pod: age-keygen ─▶ private key stays in the pod           (never leaves)
CLI: GET  /git-recipient  ◀── age1… public recipient ──── server ──exec: age-keygen -y ──▶ pod
CLI: encrypt cred file to age1…, POST /git-credentials {ciphertext, hosts}
     ── ciphertext ──▶ server ── exec: age -d -i key > ~/.git-credentials ──▶ pod
```

The pod generates an `age` (X25519) keypair on first ask and keeps the private half; the server only ever handles the public recipient and the opaque ciphertext. Only the pod can open it. The credential lands `0600` in the agent's home under git's own `store` helper — not in the pod spec, not in etcd, not in the workspace the agent might commit. Every injection is audited by host and username; **never the secret** (which the server cannot see anyway). The boundary this draws is passive exposure — capture in transit or at rest in the control plane — not paddock itself, which can already exec into the pod; repo-scoped, short-lived tokens remain the answer to "the agent can use what it can read". `--no-git-credentials` opts out. One caveat: a token baked directly into a remote URL in `.git/config` still rides along in the workspace tar in the clear, so prefer a credential helper — which is the source the harvest reads.

### Signed commits

Many orgs require signed commits, so an agent that commits *unsigned* is as blocked as one that cannot push. `paddock run` therefore also installs the developer's **signing key**, over the exact same encrypted channel — the key material is sealed to the pod's `age` recipient and only the pod opens it. The CLI reads how the developer signs from git's own config (`gpg.format`, `user.signingkey`, `commit.gpgsign`) and takes the right branch:

- **ssh** (`gpg.format=ssh`): the private signing key is sealed, decrypted to `~/.ssh/paddock_signing` (`0600`), and git is pointed at it. A literal or agent/hardware key has no file to send and is skipped. Needs `openssh-client` in the image.
- **openpgp**: only the secret **subkeys** are exported (`gpg --export-secret-subkeys`) — the master secret stays on the laptop — and imported into the pod's keyring. Needs `gnupg` in the image.

A signing identity is a heavier loan than a token (an OpenPGP key signs releases and mail, often with no expiry), so this is opt-out (`--no-git-signing`), the gpg path warns that a subkey left the machine, and every setup is audited by method and key id — **never the key material**. Two honest limits: a passphrase-protected key cannot be used non-interactively (prefer a dedicated passphraseless signing subkey, the usual CI practice), and dynamic values (key id, the sign flags) reach the pod as shell *positional arguments*, never interpolated into the install script, so a hostile value is data, not code.

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

Append-only `events` table: `ts, session_id, actor, kind, payload(JSON)`. Kinds: `session.created`, `session.deleted`, `session.expired`, `session.orphaned`, `sandbox.reaped`, `model.call`, `budget.warn`, `budget.exhausted`, `mcp.call`, `policy.denied`, `egress.allowed`, `egress.denied`, `egress.closed`, `workspace.push`, `workspace.pull`, `git.credentials.injected`. The enterprise tier adds hash-chaining (tamper evidence) and SIEM/OTLP export; OSS keeps the same schema so evidence is portable.

## Observability

Two views, deliberately separate. The **audit store** is the compliance record — who did what, kept forever, per session. The **operator view** is `/metrics` and the access log — how the fleet is doing right now, for an SRE, not an auditor.

`GET /metrics` is Prometheus text format, computed from the stores at scrape time rather than accumulated in memory, so nothing resets on a restart:

- `paddock_sessions{status}` — gauge; running/failed/expired/deleted, the terminal states emitted even at zero so a panel does not blank out.
- `paddock_budget_limit_usd{budget}` / `paddock_budget_spent_usd{budget}` — gauges from the ledger.
- `paddock_events_total{kind}` — counter per audit kind. The audit table is append-only, so these are genuinely monotonic and survive restarts, which in-memory counters would not.

It is **authenticated** — the one metrics endpoint that is not public — because the chart's ingress is a bare `/` prefix, so an open metrics path would put budget spend on the internet. Scrape it in-cluster with a bearer token. The access log is one structured line per request (method, path, status, duration, subject), emitted inside the auth middleware so the subject is already resolved.

Health is split so the kubelet does the right thing: `/healthz` is shallow (the process is up) and backs **liveness**; `/readyz` pings the database and backs **readiness**, so a database blip drains traffic instead of restarting the pod into a crashloop that cannot fix a database. Both are public — they carry no credential and reveal only up or down.

## Open-core boundary (technical)

OSS: everything above. Enterprise: SSO/SAML (auth middleware), chargeback exporters, report-pack generators (DORA/AI-Act templates over the audit store), signed audit log, vetted-MCP registry feed (external data service the gateway can subscribe to). The boundary is additive modules, not forked internals.

## Who is calling

Two different populations reach paddock, and they authenticate differently because they are asking different questions.

**Sandboxes** present a session token (`internal/api.Store.ByToken`). It is minted per session, injected as the agent's API key, and it is all the gateway needs: the session identifies the budget to debit, the policy input, and the audit subject. A sandbox can only reach the gateway's ports, so it never touches the control-plane API at all.

**Humans and CI** present a bearer token to the control-plane API (`internal/auth`). The identity behind it — never the request body — is what owns a session. This matters more than a login screen usually does: paddock's product is the sentence "this user ran this agent, which spent this much and connected here", and every noun in it comes from the authenticated caller.

- `auth.existingSecret` — a Secret holding `tokens.json`: a token, a subject, and optional groups per human. Tokens are compared by digest.
- `auth.disabled: true` — no authentication. Correct on a laptop, and it must be asked for by name; the server logs the posture on every start.
- Membership of `paddock-admin` sees and acts on everyone's sessions. Everyone else gets their own — and a **404**, not a 403, for anyone else's, because a 403 confirms that a session id exists.

OIDC is the next step and slots in behind the same `auth.Authenticator` interface: it only changes how a request becomes an `Identity`.

## Isolation roadmap

MVP: pods + a port-scoped NetworkPolicy + container limits + no mounted secrets + no service-account token (threat model: careless/compromised agent, not hostile kernel exploits). Next: optional `runtimeClassName: gvisor`, then Kata/microVMs for customers who demand hardware isolation. The pod spec is rendered in one place (`internal/sandbox`), so isolation upgrades are config, not rearchitecture.

### Pod Security Admission

Both the sandbox pods and paddock's own control plane satisfy the **`restricted`** Pod Security Standard: `runAsNonRoot`, `seccompProfile: RuntimeDefault`, `allowPrivilegeEscalation: false`, all capabilities dropped, no host namespaces, no hostPath. The control-plane containers additionally run with a read-only root filesystem (`/tmp` is an emptyDir for SQLite's temp files).

This is meant to be used, not admired — label the namespace and the cluster enforces it:

```sh
kubectl label namespace paddock \
  pod-security.kubernetes.io/enforce=restricted \
  pod-security.kubernetes.io/enforce-version=latest
```

The controls are asserted in `internal/sandbox`'s unit tests because the failure mode is unhelpful: a sandbox missing `seccompProfile` isn't degraded, it's `Forbidden` at admission, and every `paddock run` fails on a cluster the developer can't debug.
