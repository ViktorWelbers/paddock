# AGENTS.md — a fast entry into paddock

Orientation for a coding agent (or human) landing in this repo cold. It's a map,
not a manual: it points at the authoritative docs rather than restating them.

## What paddock is

A self-hosted **governance plane for coding agents**. A developer runs an agent
(Claude Code, `pi`, …) inside a per-session Kubernetes sandbox that holds **no
real provider keys and has no internet except through paddock's gateway** — which
swaps in the real key, **meters spend against a budget**, and **audits every
model, MCP, and egress call**. The pitch is answerable governance: who ran what,
what it cost, where it connected. Go, open-core.

## The three binaries (`cmd/`)

- **`paddock-server`** — control plane: session CRUD (SQLite), budget ledger,
  audit store, and it provisions/reaps the sandboxes.
- **`paddock-gateway`** — data plane: a metering reverse-proxy (`/anthropic`,
  `/openai`) that authenticates session tokens and prices token usage, plus the
  governed-egress CONNECT proxy and the MCP mux.
- **`paddock`** — the developer CLI: `run`, `attach`, `push`, `pull`, `ls`, `rm`,
  `budget`, `events`, `config`.

## Package map (`internal/`)

| Package | Responsibility |
|---|---|
| `api` | Control-plane HTTP surface: session CRUD backed by SQLite, wired to provisioner + ledger + audit. |
| `sandbox` | Renders/provisions the per-session isolation set: the agent Pod + a NetworkPolicy allowing egress only to the gateway (plus DNS). |
| `gateway` | Model-API data plane: authenticates session tokens, injects the real provider key, meters usage from responses (JSON **and** SSE). |
| `egress` | The governed internet door: an HTTP CONNECT proxy, allowlist- + policy-gated, audited per connection. |
| `mcpgw` | Server-side MCP layer: central registry of allowlisted servers + a credential broker so secrets never reach the sandbox. |
| `budget` | Hierarchical spend ledger (org → team → user → session); a debit fails if the node or any ancestor is exhausted. |
| `policy` | Embeds OPA; evaluates Rego on every tool/MCP call. Policies are plain `.rego` files, `opa test`-able. |
| `auth` | Answers "who is calling" for the control-plane API (bearer tokens today; the `Authenticator` seam is where OIDC lands). |
| `audit` | Append-only event store — the compliance spine; schema is shared with the enterprise tier for portable evidence. |

## Read next

- **[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)** — the design in depth, data
  paths, and the pinned policy-decision input schema. Read this before touching
  the gateway, sandbox, or policy packages.
- **[docs/ROADMAP.md](docs/ROADMAP.md)** — what's done and what's next, by
  milestone. Check here before starting anything sizeable.
- **[CONTRIBUTING.md](CONTRIBUTING.md)** — dev setup and ground rules.
- **[SECURITY.md](SECURITY.md)** — reporting vulnerabilities.

## Build / test / run loop

```sh
make build              # binaries into ./bin
make test vet           # unit tests + go vet — no cluster needed
make helm-lint          # lint the Helm chart
make policy-test        # `opa test` the shipped Rego policies

make dev-up             # k3d cluster + images + helm install (local end-to-end)
make e2e                # full smoke test (works without a real Anthropic key)
make e2e-egress         # governed-egress path
make e2e-pi             # second agent via an OpenAI-compatible upstream
```

`go test ./...` needs no cluster and is the fast inner loop. `make e2e-pi` wants an
OpenAI-compatible model server reachable from the cluster (`OPENAI_UPSTREAM=…
OPENAI_MODEL=…`).

## Invariants — non-negotiable

These are the product. Tests assert them; weakening one needs a very good reason:

- **Sandboxes are powerless by construction:** no real provider keys, no
  service-account token, no egress beyond the gateway, no secrets in the session's
  namespace. Credentials live behind the gateway and are injected per-request.
- **Lean dependencies.** The web dashboard is a single embedded HTML file — no JS
  build toolchain. Keep it that way.
- **Behavior change ships with a test**; `go test ./...` and `helm lint` must pass
  (CI enforces both). One logical change per PR, with the "why" in the description.

## Release & deploy model

- **CI builds images on every push to `main`** (`.github/workflows/images.yaml`,
  kaniko) and publishes `paddock`, `agent-claude`, `agent-pi`. **GHCR
  (`ghcr.io/viktorwelbers/*`) is the chart's default image**; operators can mirror
  to their own registry (see the air-gapped path in the README).
- The chart pins `:latest` + `pullPolicy: Always`, so a rollout picks up the newest
  build. The chart **version** is the semver in `deploy/helm/paddock/Chart.yaml`.
- Deployment is GitOps-friendly: the Helm chart is under `deploy/helm/paddock`, and
  `deploy/argocd/` shows the ArgoCD shape.
- **The rule that trips agents:** an **image-only** change just needs a rebuild + a
  pod restart. A **chart *template* change** (new arg, new resource) must **also
  bump `Chart.yaml`** — and, for a pinned GitOps deploy, the release tag /
  `targetRevision`. Forgetting this is the single most common deploy mistake here:
  the new template silently never ships.
- Chart gotcha: the deployment has a `fail` guard that requires either
  `auth.existingSecret` or `auth.disabled=true` — `helm template` errors without
  one, on purpose (paddock refuses to come up quietly unauthenticated).

## Two 30-second gotchas

- The two agent images build **different** agents: `Dockerfile.agent` is Claude
  Code, `Dockerfile.agent-pi` is the `pi` agent. A change to one rarely applies to
  the other.
- Secrets (API auth tokens, provider keys, subscription OAuth token) are created
  **out-of-band** with `kubectl create secret` and referenced by name — they are
  **never committed**. If you need one that isn't there, create it; don't add it to
  values or git.
