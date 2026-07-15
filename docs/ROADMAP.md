# Paddock — Roadmap

## M0 — Skeleton (now)

- [x] Repo, docs, compiling Go skeleton
- [x] Session CRUD API against SQLite
- [x] Budget ledger with price table, hard/soft caps (unit-tested)
- [x] Gateway proxy with token metering against fake upstream (unit-tested)
- [x] Sandbox spec rendering: pod + namespace + NetworkPolicy + quota (unit-tested)
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
- [ ] OpenCode as third agent

## M2 — Launchable OSS

- [x] Web dashboard (sessions, spend, audit trail) — read-only, embedded in the server at `/`
- [ ] Server API authentication (today: keep it cluster-internal or behind ingress auth)
- [ ] Sandbox reconciliation/GC: a control-plane restart with ephemeral storage
      orphans session namespaces (observed live); reconcile namespaces against the
      session store on startup
- [ ] Server-side attach relay (websocket) so developers don't need pods/exec RBAC
- [ ] Hierarchical budgets exposed in config (org/team/user)
- [ ] Policy decision input schema pinned + documented; `opa test` examples
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
