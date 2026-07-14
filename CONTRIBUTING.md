# Contributing to Paddock

Thanks for your interest! Paddock is early (alpha) — issues, design feedback
from platform teams, and small focused PRs are all welcome.

## Development setup

Requires Go, Docker, [k3d](https://k3d.io), kubectl, helm.

```sh
make build          # binaries into ./bin
make test vet       # unit tests (no cluster needed)
make helm-lint

make dev-up         # k3d cluster + images + helm install
make e2e            # full smoke test (works without a real Anthropic key)
```

`make e2e-pi` additionally needs an OpenAI-compatible model server reachable
from the cluster (`OPENAI_UPSTREAM=... OPENAI_MODEL=...`).

## Ground rules

- **Isolation invariants are non-negotiable.** Sandboxes get no real provider
  keys, no service-account tokens, no egress beyond the gateway, no secrets in
  session namespaces. Tests assert these; changes that weaken them need a very
  good argument.
- Keep dependencies lean. The dashboard is deliberately a single embedded HTML
  file — no JS build toolchain.
- Add a test with behavior changes; `go test ./...` and `helm lint` must pass
  (CI enforces both).
- One logical change per PR, with the "why" in the description.

## Reporting security issues

Please don't open public issues for vulnerabilities — see [SECURITY.md](SECURITY.md).
