# Security Policy

Paddock is a security-adjacent product: it exists to keep provider keys out of
sandboxes, bound spend, and produce trustworthy audit trails. Reports that
break any of those guarantees are treated as critical.

## Reporting a vulnerability

Please report vulnerabilities privately via
[GitHub security advisories](https://github.com/ViktorWelbers/paddock/security/advisories/new)
rather than public issues. You should receive a response within a few days.

## Scope — what counts as a vulnerability

- A sandbox obtaining a real provider credential (key swap bypass)
- Sandbox egress beyond the gateway despite the NetworkPolicy set
- Metering bypass (model usage that provably escapes the budget ledger)
- Audit-trail tampering or events that can be suppressed by the sandboxed agent
- Policy-engine bypass for tool/MCP calls

## Current limitations (known, documented)

Alpha software — not yet hardened for hostile multi-tenant use:

- The server API has no authentication yet; deploy it behind your ingress
  auth or keep it cluster-internal. `user` is whatever the client claims,
  so the audit trail attributes actions rather than proves them.
- `paddock attach` uses the operator's kubeconfig (pods/exec RBAC).
- Debits land post-response: a session can overshoot its budget by at most
  one model call by design.
- Session tokens are stored in plaintext and do not expire.

Deployment expectations, in case they help you place paddock in your cluster:

- Sandboxes and the control plane satisfy the `restricted` Pod Security
  Standard; label the namespace and Kubernetes will hold paddock to it (see
  `docs/ARCHITECTURE.md`).
- Sandboxes reach the gateway's ports and DNS, and nothing else. Internet
  access, when enabled, is an allowlisted CONNECT proxy that tunnels TLS
  without intercepting it and audits every allow, deny and close.
- Agent workloads can be pinned to a dedicated node pool (`sandbox.nodeSelector`
  / `sandbox.tolerations`) and given a stronger runtime (`sandbox.runtimeClassName`).
