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
- [x] Deploy server + gateway via helm (k3d dev loop: `make dev-up`; homelab: `values-homelab.yaml`)
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

- [ ] Web dashboard (sessions, spend, audit trail) — read-only is enough
- [ ] Hierarchical budgets exposed in config (org/team/user)
- [ ] Policy decision input schema pinned + documented; `opa test` examples
- [ ] Docs site, quickstart that works on kind/k3d in <10 min
- [ ] Launch: HN post + deep-dive blog ("your coding agent should never see an API key")

## M3 — First design partner

- [ ] 3–5 discovery calls with EU fintech/bank platform leads
- [ ] DORA/AI-Act control matrix drafted with a real compliance officer
- [ ] First report pack prototype (evidence export over the audit store)
- [ ] Postgres backend option

## M4 — Enterprise tier / revenue

- [ ] SSO/SAML, RBAC
- [ ] Signed (hash-chained) audit log, SIEM export
- [ ] Report packs productized
- [ ] License + pricing page; first paid deployment

## M5 — Data moat

- [ ] MCP scanner pipeline (static + behavioral) feeding a vetted-registry dataset
- [ ] Registry feed as enterprise subscription
- [ ] gVisor runtimeClass option
