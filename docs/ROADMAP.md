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

The theme: paddock currently assumes a friendly network and an attentive
operator. Both assumptions have to go before anyone else can run it.

**Next up, in order.** The first three are what a stranger hits within an hour
of installing.

- [ ] **Server-side attach relay (websocket).** `paddock attach` is the last
      thing that talks to the Kubernetes API from the developer's machine: it
      needs a kubeconfig pointing at the right cluster, `pods/exec` rights the
      developer probably shouldn't have, and it drags client-go into the CLI
      (36MB of a 40MB binary). Workspace transfer already proved the shape —
      `internal/sandbox.Execer` behind an HTTP endpoint — so this is
      `GET /v1/sessions/{id}/attach` upgrading to a websocket, relaying the
      exec stream, and carrying terminal resize. When it lands, the CLI is
      pure HTTP, `go install` gives a small binary, and `PADDOCK_KUBECONFIG`
      disappears from the docs.
- [ ] **Server API authentication.** Every route is unauthenticated today, which
      is why the honest advice is "keep it cluster-internal or behind ingress
      auth". `GET /v1/sessions/{id}/workspace` makes that worse: it hands a
      session's files to anyone who can reach the API. Sandboxes are already
      fenced off (the netpol is port-scoped), so this is about humans on the
      network. Wanted: OIDC bearer tokens, `user` taken from the token rather
      than the client's claim, and sessions owned by the user who created them.
- [ ] **Sandbox reconciliation/GC.** A control-plane restart with ephemeral
      storage orphans running pods (observed live: the session row vanishes,
      the pod keeps its 4Gi, and `paddock rm` 404s). Reconcile pods labelled
      `paddock.dev/session` against the session store on startup, and adopt or
      reap the difference.

Then:

- [ ] **Idle-TTL reaper** for detached sessions: today a forgotten session holds
      its pod's CPU and memory until someone runs `paddock rm`. Needs a
      last-activity timestamp (the gateway already sees every call) and a
      configurable TTL.
- [ ] **Concurrent-session ceiling.** The per-session `ResourceQuota` went away
      with the per-session namespace, so "how many sandboxes may this cluster
      run" is now a server-side check nobody has written. Per-user and global
      caps at session-create, plus a clear 429.
- [x] `opa test` examples for the shipped policies (`make policy-test`, run in CI):
      Rego that silently matches nothing looks identical to Rego that works, so
      the `omitempty` trap that cost a live bug is now a regression test platform
      teams can copy
- [ ] Hierarchical budgets exposed in config (org/team/user)
- [x] Web dashboard (sessions, spend, audit trail) — read-only, embedded in the server at `/`
- [x] Policy decision input schema pinned + documented (`docs/ARCHITECTURE.md`)
- [ ] Docs site, quickstart that works on kind/k3d in <10 min
- [ ] Published container images + a Helm repo, so "try paddock" isn't "build
      three images first"

## M3 — Production hardening

- [ ] Evidence export over the audit store (DORA / EU AI Act report packs)
- [ ] Postgres backend option
- [ ] **Egress profiles**: ship named bundles (`minimal`, `developer`,
      `enterprise`) over the domain groups, so operators pick a starting point
      instead of assembling registry hostnames by hand and discovering
      `files.pythonhosted.org` the hard way.
- [ ] **Per-agent and per-team egress**: the OPA input already carries `agent`
      and `user`, so the rules are writable today — what's missing is the
      worked examples and a way to bind an allowlist group to a team without
      editing rego.
- [ ] **Workspace ergonomics**: `paddock pull` is a full-tree fetch; a diff
      view (`what did the agent change?`) and selective pull would beat
      "overwrite and read git status".
- [ ] Egress byte budgets, alongside the token budgets — the audit trail
      already counts bytes per tunnel; nothing acts on them yet.

## M4 — Enterprise tier

- [ ] SSO/SAML, RBAC
- [ ] Signed (hash-chained) audit log, SIEM export
- [ ] Report packs productized

## M5 — MCP registry & hardening

- [ ] MCP scanner pipeline (static + behavioral) feeding a vetted-registry dataset
- [ ] gVisor runtimeClass option
