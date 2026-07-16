# Paddock — Roadmap

## M0 — Skeleton (now)

- [x] Repo, docs, compiling Go skeleton
- [x] Session CRUD API against SQLite
- [x] Budget ledger with price table, hard/soft caps (unit-tested)
- [x] Gateway proxy with token metering against fake upstream (unit-tested)
- [x] Sandbox spec rendering: pod + NetworkPolicy, in paddock's own namespace (unit-tested)
- [x] OPA policy evaluation with example rego (unit-tested)
- [x] Helm chart scaffold

## M1 — Core loop on a real cluster

- [x] Container images: control plane (`Dockerfile`) + agent with Claude Code (`Dockerfile.agent`)
- [x] Deploy server + gateway via helm (k3d dev loop: `make dev-up`; GitOps: `deploy/argocd/`)
- [x] In-cluster provisioning (`rest.InClusterConfig`), probes, graceful shutdown
- [x] `paddock attach` (exec with TTY over the operator's kubeconfig); `paddock run claude` auto-attaches
- [x] e2e smoke test (`make e2e`): session lifecycle, env injection, NetworkPolicy enforcement, proxy path, audit trail
- [x] MCP registry wired into the chart (ConfigMap + brokered credentials Secret)
- [ ] Real Anthropic passthrough incl. SSE usage metering verified against the live API (needs a real key at demo time)
- [ ] MCP mux relaying one real allowlisted server end-to-end
- [x] Second supported agent proves agent-neutrality: pi against an OpenAI-compatible
      upstream (vLLM) through the gateway's `/openai` metering proxy (`make e2e-pi`,
      verified live: completion relayed, usage metered, budget debited, netpol enforced)
- [x] The sandbox is usable for real work: the developer's working directory is
      uploaded on `paddock run` (git-aware) and `paddock pull` brings the agent's
      edits back, over the server rather than the CLI's kubeconfig; agent images
      carry the expected toolchain (git, node, python3, make, jq)
- [x] Governed egress: a CONNECT proxy on the gateway tunnels TLS end-to-end to
      allowlisted domain groups, decided by allowlist + OPA, defended against DNS
      rebinding, and audited allow/deny/close with byte counts (`make e2e-egress`,
      verified live: `pip install` and github clone through the proxy, gitlab and an
      exfiltration attempt refused and audited)
- [ ] OpenCode as third agent

## M2 — Launchable OSS

- [x] Web dashboard (sessions, spend, audit trail) — read-only, embedded in the server at `/`
- [ ] Server API authentication (today: keep it cluster-internal or behind ingress auth)
- [ ] Sandbox reconciliation/GC: a control-plane restart with ephemeral storage
      orphans session pods (observed live); reconcile pods labelled
      `paddock.dev/session` against the session store on startup
- [ ] Server-side attach relay (websocket) so developers don't need pods/exec RBAC
      (workspace transfer already goes through the server; attach is the last thing
      holding client-go — and 36MB — in the CLI)
- [ ] Idle-TTL reaper for detached sessions: today a forgotten session holds its
      pod's CPU/memory until someone runs `paddock rm`
- [ ] Concurrent-session ceiling: the per-session ResourceQuota went away with the
      per-session namespace, so "how many sandboxes may this cluster run" is now a
      server-side check nobody has written
- [ ] Hierarchical budgets exposed in config (org/team/user)
- [x] Policy decision input schema pinned + documented (`docs/ARCHITECTURE.md`)
- [ ] `opa test` examples for the shipped policies
- [ ] Docs site, quickstart that works on kind/k3d in <10 min

## M3 — Production hardening

- [ ] Evidence export over the audit store (DORA / EU AI Act report packs)
- [ ] Postgres backend option

## M4 — Enterprise tier

- [ ] SSO/SAML, RBAC
- [ ] Signed (hash-chained) audit log, SIEM export
- [ ] Report packs productized

## M5 — MCP registry & hardening

- [ ] MCP scanner pipeline (static + behavioral) feeding a vetted-registry dataset
- [ ] gVisor runtimeClass option
