# Paddock

[![ci](https://github.com/ViktorWelbers/paddock/actions/workflows/ci.yaml/badge.svg)](https://github.com/ViktorWelbers/paddock/actions/workflows/ci.yaml)
[![license](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)

**A self-hosted governance plane for coding agents.**

Paddock spawns per-user sandboxes for agents like Claude Code and OpenCode on *your own* Kubernetes cluster, and puts a gateway between the agent and the outside world. Every model call is metered against a budget. Every tool and MCP call passes a policy check. Everything is written to an audit log your compliance team can hand to a regulator.

Paddock is **not** a meta-harness or an agent framework. It doesn't orchestrate agents, compose them, or replace your agent of choice. It answers one question for the enterprise: *"Our developers want to run autonomous coding agents — how do we let them without losing control of cost, credentials, and compliance?"*

 ![Gif](docs/img/paddock-selfhosted.gif)

## Why

Coding agents are being adopted faster than platform teams can govern them. Today the typical setup is: an API key in an engineer's shell profile, unbounded spend, tools with unrestricted network and credential access, and no audit trail. That is a non-starter for banks, insurers, and anyone under DORA or the EU AI Act.

Paddock gives the platform team a single control point:

- **Budgets** — hierarchical (org → team → user → session) spend ledgers with soft warnings and hard stops. The agent's model traffic is proxied, token usage is priced, and the ledger is debited in real time.
- **Sandboxes** — each session runs in a locked-down pod: egress allowed only to the Paddock gateway, no secrets mounted, no service-account token, CPU and memory capped. Real provider API keys never enter the sandbox. Sessions are pods in paddock's own namespace, so the server installs with a namespaced Role — no cluster-scoped RBAC.
- **Your code, not a blank page** — `paddock run` uploads your working directory into the sandbox and `paddock pull` brings the agent's edits back. In a git repo, `.gitignore` decides what travels. Files move through the server, so the CLI needs no cluster access.
- **Governed egress** — agents can install dependencies, from the registries you allow and nowhere else. Traffic goes through a CONNECT proxy on the gateway that tunnels TLS end-to-end (paddock never sees your source or your packages) and decides on the domain. Every attempt is audited, allowed or denied, with byte counts.
- **Server-side MCP** — MCP servers are centrally administered by the platform team, run outside the sandbox, and have their credentials injected at the gateway. Developers get capabilities, not secrets.
- **Policies** — OPA/Rego decisions on every tool call, MCP call, and egress connection. Your platform team already speaks Rego; reuse the pipelines and review process you have for Gatekeeper.
- **Audit** — append-only event log of sessions, model calls, tool calls, egress, workspace transfers, and policy decisions, designed to back DORA / EU AI Act evidence requirements.

## Architecture - HLD

![Paddock architecture overview](docs/img/architecture.png)

The developer's `paddock` CLI talks only to `paddock-server` (control plane); the
sandbox pod can reach nothing but `paddock-gateway` (data plane), which meters
model spend, brokers credentials, and governs egress. See
[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for the per-path detail.

## Quickstart (k3d, ~5 minutes)

Requires docker, [k3d](https://k3d.io), kubectl, helm, Go.

```sh
export ANTHROPIC_API_KEY=sk-ant-...   # optional; omit to run with a fake key
make dev-up                           # k3d cluster + images + helm install
make e2e                              # end-to-end smoke test (works without a real key)
make e2e-egress                       # dependency installs allowed, exfiltration refused

make build
cd ~/your-project
paddock run claude                    # uploads this directory, attaches to Claude Code
paddock pull <id>                     # bring the agent's edits back
paddock budget                        # see spend
paddock events <id>                   # what it called, fetched, and was denied
paddock rm <id>                       # tear the sandbox down
```

Developers on a team with a running deployment don't need the repo at all:

```sh
go install github.com/viktorwelbers/paddock/cmd/paddock@latest
paddock config set server https://paddock.internal   # your deployment's URL, once
paddock config set token pdk-...                     # your token, from your platform team
paddock run claude
```

The CLI finds the server once and remembers it: platform teams expose the
server behind an ingress (e.g. `https://paddock.internal`) and developers save
it with `paddock config set server https://paddock.internal`. `PADDOCK_SERVER`
overrides the saved value per shell (CI, one-offs), and with neither set the
CLI falls back to `localhost:8080`, where the k3d dev loop maps the cluster
ingress. The token works the same way (`PADDOCK_TOKEN` overrides it), and the
k3d loop installs with authentication off, so there is nothing to save there. That's the whole story: no port-forwards, no kubeconfig magic. Inside
the sandbox, the agent's only route out is the Paddock gateway: no cluster API,
no real keys, and no internet beyond the domains you allow.

## Two ways to run

You pick a mode per session:

- **In-pod (default).** `paddock run claude` runs the agent *inside* the sandbox and attaches a
  terminal. Model calls are metered through the gateway and the agent is contained by construction —
  the strongest isolation, and the right choice when you want structural, unbypassable governance.
- **Local-harness.** `paddock dev claude` runs *your own local* Claude Code, but redirects its shell
  commands into the sandbox and keeps your files in sync with it. You get native local tooling and
  your own model credentials, and paddock still governs the agent's *actions* — sandboxed execution,
  allowlisted egress, full audit. Model calls are **not** metered in this mode; that trade is what
  buys the local experience.

### Local-harness quickstart

Requires the [DevSpace](https://devspace.sh) CLI for two-way workspace sync:

```sh
brew install devspace
cd ~/your-project
paddock init            # Claude Code (auto-detected). Or: paddock init --agent opencode
claude                  # (or `opencode`) — sandbox created/reused + synced automatically
```

paddock supports multiple harnesses through per-harness adapters (`--agent claude|opencode`, more to
come); each installs that harness's own hooks/plugin on a shared core. After `paddock init`, just run
your harness: it finds-or-creates **one** sandbox for the directory (reused on reopen, so they never
pile up) and starts the two-way sync; shell tool calls run in the sandbox; forgotten sandboxes
self-reap once they go idle — you never start or stop anything by hand. `paddock init` also **denies
the native web tools** (Claude's `WebFetch`/`WebSearch`, opencode's `webfetch`) by default, because
they run in the local harness and would bypass the governed sandbox egress (`--allow-web-tools` keeps
them, ungoverned). **Teardown is automatic for every harness:** while the harness is open it
heartbeats the session; when you close it (or it crashes), the heartbeats stop and the server's idle
reaper reclaims the sandbox on its own, and the local sync process self-terminates. You never run
`paddock down` — though it's still there for an immediate teardown.

Prefer explicit control? `paddock dev claude --detach` does it in one shot and `paddock down` tears
it down; the pieces are `paddock exec <id> <cmd>` (one command in the sandbox), `paddock sync <id>`
(two-way sync), and `paddock init-local <id>` (bind + Bash hook). paddock owns the sync process
either way, so you never manage `devspace` yourself.

## Your workspace in the sandbox

`paddock run` uploads the current directory before attaching, because an agent
with no code can't do anything useful. In a git repo the upload set is git's
own answer — tracked and untracked-but-not-ignored files, plus `.git` so the
agent has real history — which means `.gitignore` already decides what travels
and `node_modules` stays home. Outside a repo, the directory goes up as-is.

```sh
paddock run claude                     # uploads the current directory, then attaches
paddock run claude --no-push           # start with an empty /workspace
paddock run claude --no-git-credentials # don't install git credentials (read, not push)
paddock run claude --no-git-signing    # don't install the commit-signing key
paddock push <id> [dir]                # upload again (--clean to mirror exactly)
paddock pull <id> [dir]                # bring the agent's edits back
```

`pull` overwrites what the archive contains and leaves everything else alone,
like a git checkout. Files travel through the server over `pods/exec`, so the
CLI needs no kubeconfig and no exec rights of its own, and both directions are
audited (`workspace.push` / `workspace.pull`, with byte counts and a sha256).

`paddock run` also hands the repo's **git credentials** to the sandbox, so the
agent can clone and push against private and enterprise hosts (`github.axa.com`
and friends) with nothing to configure per session — they come from git's own
credential helper, the same place `git push` reads. The handoff is encrypted
end to end: the pod holds an `age` private key, the CLI encrypts to its public
half, and paddock moves only ciphertext, so the token never crosses the control
plane in the clear. Audited by host, never the secret. If the developer signs
their commits, the **signing key** rides the same encrypted channel (ssh or
gpg, detected from `gpg.format`) so the agent's commits are signed too — opt out
with `--no-git-signing`. See
[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md#git-credentials-encrypted-handoff).

## Governed egress: dependencies without a blank cheque

An agent that can't run `pip install` is a toy; an agent with open internet is
an exfiltration channel. Paddock's answer is the one [nono](https://github.com/always-further/nono)
uses: the sandbox has no route to the internet at all, and the gateway offers a
single governed door.

The gateway runs an HTTP **CONNECT proxy** that sandboxes reach via injected
`HTTP_PROXY`/`HTTPS_PROXY` (authenticated with the session token). It **tunnels**
rather than intercepts, so TLS stays end-to-end — paddock decides on the
*domain* and never sees your source code or the packages you fetch. There are no
CAs to install and no certificates to trust.

Every connection is authenticated, matched against the allowlist, evaluated by
OPA, and re-checked **after DNS resolution**, so a rebound hostname can't reach
the metadata service, the kube API, or another namespace. Allowed, denied, and
closed (with bytes) all land in the audit trail:

```
$ paddock events <id>
egress.allowed  {"host":"pypi.org","groups":["package_registries"]}
egress.closed   {"host":"pypi.org","bytes_sent":3945,"bytes_received":313909}
egress.denied   {"host":"gitlab.com","reason":"not_in_allowlist"}
```

**Default-deny**: with no allowlist configured the proxy runs and refuses
everything (still audited). Operators opt domains in by group:

```yaml
gateway:
  egress:
    enabled: true
    allowedPorts: [443]        # CONNECT targets; 443 only by default
    plainHTTP: false           # no cleartext proxying
    allowedPrivateCIDRs: []    # keep RFC1918/CGNAT/link-local blocked
    allowlist:
      groups:
        package_registries:
          - pypi.org
          - files.pythonhosted.org
          - registry.npmjs.org
          - proxy.golang.org
          - sum.golang.org
        github:
          - github.com
          - "*.github.com"     # sub-domains only, never the apex
          - codeload.github.com
```

Group names are what the audit trail and your policies see. A pattern is an
exact host or a `*.example.com` wildcard matching sub-domains only. IP-literal
targets are always refused — they'd defeat the rebinding check. (`make dev-up`
seeds pypi and github so the dev loop is useful out of the box; a plain
`helm install` starts closed.)

The static allowlist is the first half of the decision; `policies/egress.rego`
is the second, and it can use the groups:

```rego
package paddock.authz

import rego.v1

# Ships by default: nothing reaches a host that no group claims.
deny contains msg if {
	input.kind == "egress"
	count(input.groups) == 0
	msg := sprintf("host %q matches no allowed egress group", [input.host])
}

# Yours to add: allowlist the group globally, restrict it per team.
deny contains msg if {
	input.kind == "egress"
	"github" in input.groups
	not input.user in {"platform-team"}
	msg := "cloning from github is restricted to the platform team"
}
```

Egress input is `{kind: "egress", user, agent, session, host, port, groups}`.
Policies fail closed: if the engine errors the connection is denied and audited
as `policy_error`. Because the decision is per-connection and carries the user,
"who may reach what" is a Rego question, not a redeploy.

Test your rules the way you already test Gatekeeper policies — `make policy-test`
runs `opa test` over `policies/` (no need to install opa; the Makefile runs the
pinned version through `go run`). Worth doing: a Rego rule that references a
field the input doesn't carry is simply *undefined*, so it never fires, denies
nothing, and reads perfectly well in review. `policies/egress_test.rego` has
that exact case as a regression test.

### Any agent, any model server

Paddock is agent-neutral. The gateway also fronts OpenAI-compatible upstreams
(vLLM, llama.cpp, ...), with the same session-token auth, usage metering
(streaming included — the gateway forces `stream_options.include_usage`, so
clients can't opt out of metering), budgets, and audit trail. The
[pi coding agent](https://github.com/badlogic/pi-mono) is wired in as the second
supported agent:

```sh
# point the gateway at your OpenAI-compatible model server
make k3d-deploy OPENAI_UPSTREAM=https://your-vllm.example OPENAI_MODEL=your/model
make e2e-pi                           # governed completion, metering, netpol — end to end
./bin/paddock run pi                  # interactive pi session in a sandbox
```

### Custom agent images

The default agent images ship node, git, python3 (use `python3 -m venv .venv`
— system site-packages are locked down), make, jq, and ripgrep. For other
toolchains, extend the image and point paddock at it — everything else stays
the same:

```dockerfile
FROM ghcr.io/viktorwelbers/agent-claude:latest
USER root
RUN apt-get update && apt-get install -y --no-install-recommends golang && rm -rf /var/lib/apt/lists/*
USER 10001:10001
# keep the inherited tini entrypoint — it holds the sandbox pod
```

Wire it up via the `agentImage` helm value (or per-agent with the server's
`--agent-images claude=myreg/agent-go:v1` flag).

## Dashboard

The server ships a read-only dashboard at its root URL (`/`): budgets with
spend meters, sessions, and each session's audit trail. It's a single embedded
HTML file — no extra deployment, no JS toolchain, works wherever the API is
reachable.

## Deploying to your own cluster

The images are published to GHCR (`ghcr.io/viktorwelbers/{paddock,agent-claude,agent-pi}`)
and the chart points at them by default, so a basic install builds nothing:

```sh
# 1. Namespace + the real provider key (gateway-side only — never in a sandbox)
kubectl create namespace paddock
kubectl -n paddock create secret generic paddock-anthropic \
  --from-literal=ANTHROPIC_API_KEY=sk-ant-...

# 2. Who may call the API. A subject owns the sessions it creates;
#    paddock-admin sees everyone's.
cat > tokens.json <<'JSON'
{"users": [
  {"token": "pdk-viktor-<random>", "subject": "viktor"},
  {"token": "pdk-ops-<random>", "subject": "ops", "groups": ["paddock-admin"]}
]}
JSON
kubectl -n paddock create secret generic paddock-api-tokens --from-file=tokens.json

# 3. Recommended: hold paddock to the standard it asks of agents
kubectl label namespace paddock \
  pod-security.kubernetes.io/enforce=restricted \
  pod-security.kubernetes.io/enforce-version=latest

# 4. Install (pulls the public GHCR images by default)
helm upgrade --install paddock deploy/helm/paddock -n paddock \
  --set gateway.existingSecret=paddock-anthropic \
  --set auth.existingSecret=paddock-api-tokens
```

The chart will not install without an answer on authentication: either
`auth.existingSecret`, or `auth.disabled=true` to serve the API to anyone who
can reach it. Paddock exists to say who did what, so the choice is yours to
make but not to skip.

**Air-gapped, or want your own registry / a custom agent toolchain?** Build the
three images and push them somewhere your cluster can pull from, then override
the defaults:

```sh
make push REGISTRY=<your-registry> TAG=$(git rev-parse --short HEAD)
helm upgrade --install paddock deploy/helm/paddock -n paddock \
  --set image.repository=<your-registry>/paddock/paddock --set image.tag=<tag> \
  --set agentImage=<your-registry>/paddock/agent-claude:<tag> \
  --set gateway.existingSecret=paddock-anthropic --set auth.existingSecret=paddock-api-tokens
# (if the registry uses a self-signed CA, trust it in Docker before pushing)
```

See `deploy/helm/paddock/values.yaml` for the full surface: ingress (put the
server behind one; developers save the URL with `paddock config set server`),
persistent SQLite, where agent workloads may run (`sandbox.nodeSelector`,
`sandbox.tolerations`, `sandbox.runtimeClassName` for gVisor/Kata), and the
server-side MCP registry. For the model backend you have three choices, none
mutually exclusive: an Anthropic key for metered Claude (`gateway.existingSecret`),
a personal Claude subscription for the claude agent (`sandbox.claudeOAuthSecret`
holding `CLAUDE_CODE_OAUTH_TOKEN` — direct and unmetered, still through the
governed egress proxy), or an OpenAI-compatible upstream for pi
(`gateway.openai.*`, `caConfigMap` for private CAs). Serving only self-hosted
models? Leave `gateway.existingSecret` empty and set `gateway.openai.upstream` —
no Anthropic key required. An ArgoCD `Application` example lives in
[`deploy/argocd/`](deploy/argocd).

## Open core

Everything in this repository is Apache 2.0 and always will be: the gateway, sandbox runner, budgets, OPA integration, and audit log. A commercial self-hosted tier adds what enterprises buy in procurement: SSO/SAML, chargeback exports, DORA / EU AI Act report packs, SIEM export, tamper-evident signed audit logs, and a curated feed of vetted MCP servers.

## Status

Alpha / skeleton. See [docs/ROADMAP.md](docs/ROADMAP.md). Design partners from regulated industries: get in touch.
