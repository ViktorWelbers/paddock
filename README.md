# Paddock

**A self-hosted governance plane for coding agents.**

Paddock spawns per-user sandboxes for agents like Claude Code and OpenCode on *your own* Kubernetes cluster, and puts a gateway between the agent and the outside world. Every model call is metered against a budget. Every tool and MCP call passes a policy check. Everything is written to an audit log your compliance team can hand to a regulator.

Paddock is **not** a meta-harness or an agent framework. It doesn't orchestrate agents, compose them, or replace your agent of choice. It answers one question for the enterprise: *"Our developers want to run autonomous coding agents — how do we let them without losing control of cost, credentials, and compliance?"*

## Why

Coding agents are being adopted faster than platform teams can govern them. Today the typical setup is: an API key in an engineer's shell profile, unbounded spend, tools with unrestricted network and credential access, and no audit trail. That is a non-starter for banks, insurers, and anyone under DORA or the EU AI Act.

Paddock gives the platform team a single control point:

- **Budgets** — hierarchical (org → team → user → session) spend ledgers with soft warnings and hard stops. The agent's model traffic is proxied, token usage is priced, and the ledger is debited in real time.
- **Sandboxes** — each session runs in an isolated pod in a locked-down namespace: egress allowed only to the Paddock gateway, no secrets mounted, resource quotas enforced. Real provider API keys never enter the sandbox.
- **Server-side MCP** — MCP servers are centrally administered by the platform team, run outside the sandbox, and have their credentials injected at the gateway. Developers get capabilities, not secrets.
- **Policies** — OPA/Rego decisions on every tool and MCP call. Your platform team already speaks Rego; reuse the pipelines and review process you have for Gatekeeper.
- **Audit** — append-only event log of sessions, model calls, tool calls, and policy decisions, designed to back DORA / EU AI Act evidence requirements.

## Architecture (30 seconds)

```
 developer                    control plane                    data plane
 ─────────                    ─────────────                    ──────────
 paddock run claude ───────▶  paddock-server ──── spawns ───▶  sandbox pod
                              │  sessions              (Claude Code, egress
                              │  budgets                only to gateway)
                              │  audit log                     │
                              │                                │ ANTHROPIC_BASE_URL
                              │        policy (OPA) ◀──────────┤
                              │        budget check ◀──────────┤
                              ▼                                ▼
                          SQLite/Postgres              paddock-gateway
                                                       │ token metering
                                                       │ MCP mux + credential broker
                                                       ▼
                                              model APIs / MCP servers
```

## Quickstart (k3d, ~5 minutes)

Requires docker, [k3d](https://k3d.io), kubectl, helm, Go.

```sh
export ANTHROPIC_API_KEY=sk-ant-...   # optional; omit to run with a fake key
make dev-up                           # k3d cluster + images + helm install
make e2e                              # end-to-end smoke test (works without a real key)

make build
./bin/paddock run claude              # spawn a governed session and attach to Claude Code
./bin/paddock budget                  # see spend
./bin/paddock rm <id>                 # tear the sandbox down
```

No further setup: the CLI finds the control plane on its own — `PADDOCK_SERVER`
if set (the production path: platform teams expose the server behind an ingress,
e.g. `https://paddock.internal`, and hand developers that one env var), else a
server already on `localhost:8080`, else an automatic port-forward over your
kubeconfig (`PADDOCK_KUBECONFIG` to override, `PADDOCK_NAMESPACE` to narrow the
search). Inside the sandbox, Claude Code can only reach the Paddock gateway:
no internet, no cluster API, no real keys.

### Any agent, any model server

Paddock is agent-neutral. The gateway also fronts OpenAI-compatible upstreams
(vLLM, llama.cpp, ...), with the same session-token auth, usage metering
(streaming included — the gateway forces `stream_options.include_usage`, so
clients can't opt out of metering), budgets, and audit trail. The
[pi coding agent](https://github.com/badlogic/pi-mono) is wired in as the second
supported agent:

```sh
# point the gateway at your model server (defaults target a homelab vLLM)
make k3d-deploy OPENAI_UPSTREAM=https://vllm.internal OPENAI_MODEL=your/model
make e2e-pi                           # governed completion, metering, netpol — end to end
./bin/paddock run pi                  # interactive pi session in a sandbox
```

## Deploying to your own cluster

```sh
# 1. Build and push the images to your registry
make push-harbor REGISTRY=harbor.internal TAG=$(git rev-parse --short HEAD)

# 2. The real provider key lives in one Secret, gateway-side only
kubectl create namespace paddock
kubectl -n paddock create secret generic paddock-anthropic \
  --from-literal=ANTHROPIC_API_KEY=sk-ant-...

# 3. Install
helm upgrade --install paddock deploy/helm/paddock -n paddock \
  -f deploy/helm/paddock/values-homelab.yaml \
  --set image.tag=<tag> --set agentImage=<registry>/paddock/agent-claude:<tag>
```

`values-homelab.yaml` shows a full example (traefik ingress, cert-manager, persistent
SQLite on local-path). An ArgoCD `Application` example lives in
[`deploy/argocd/`](deploy/argocd). On macOS, add your registry to Docker's insecure
registries (or trust its CA) before pushing to a self-signed Harbor.

## Open core

Everything in this repository is Apache 2.0 and always will be: the gateway, sandbox runner, budgets, OPA integration, and audit log. A commercial self-hosted tier adds what enterprises buy in procurement: SSO/SAML, chargeback exports, DORA / EU AI Act report packs, SIEM export, tamper-evident signed audit logs, and a curated feed of vetted MCP servers.

## Status

Alpha / skeleton. See [docs/ROADMAP.md](docs/ROADMAP.md). Design partners from regulated industries: get in touch.
