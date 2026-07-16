#!/usr/bin/env bash
# End-to-end smoke test against a running paddock deployment (k3d or real).
#
# Works with a fake ANTHROPIC_API_KEY: everything except the actual model
# call is fully asserted; with a fake key the model call must come back 401
# from upstream, which still proves the sandbox -> gateway -> upstream path
# and the session-token swap. With a real key it asserts a 200 and that the
# budget was debited.
set -euo pipefail

NAMESPACE=${NAMESPACE:-paddock}
PORT=${PORT:-18080}
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
echo "session: $SID"
# The server reports where the sandbox landed; sessions share its namespace.
NS=$(echo "$RESP" | json "['namespace']")
POD=$(echo "$RESP" | json "['pod']")

step "wait for sandbox pod"
kubectl -n "$NS" wait --for=condition=Ready pod/"$POD" --timeout=180s

step "sandbox env: gateway URL + session token (never a real key)"
kubectl -n "$NS" exec "$POD" -- printenv ANTHROPIC_BASE_URL | grep -q anthropic \
  || fail "ANTHROPIC_BASE_URL not set in sandbox"
kubectl -n "$NS" exec "$POD" -- printenv ANTHROPIC_API_KEY | grep -q '^pdk_' \
  || fail "ANTHROPIC_API_KEY in sandbox is not a pdk_ session token"
echo "ok"

step "netpol: sandbox can reach the gateway"
kubectl -n "$NS" exec "$POD" -- node -e '
  const base = process.env.ANTHROPIC_BASE_URL.replace(/\/anthropic\/?$/, "");
  fetch(base + "/healthz", {signal: AbortSignal.timeout(10000)})
    .then(r => process.exit(r.ok ? 0 : 1)).catch(() => process.exit(1));
' || fail "sandbox cannot reach the gateway"
echo "ok"

step "netpol: sandbox cannot reach the internet"
if kubectl -n "$NS" exec "$POD" -- node -e '
  fetch("https://api.github.com", {signal: AbortSignal.timeout(8000)})
    .then(() => process.exit(0)).catch(() => process.exit(1));
' >/dev/null 2>&1; then
  fail "sandbox reached api.github.com — NetworkPolicy is NOT enforced"
fi
echo "ok (egress blocked)"

step "model call through the gateway"
REAL_KEY=$(kubectl -n "$NAMESPACE" get secret paddock-anthropic \
  -o jsonpath='{.data.ANTHROPIC_API_KEY}' | base64 -d)
STATUS=$(kubectl -n "$NS" exec "$POD" -- node -e '
  fetch(process.env.ANTHROPIC_BASE_URL + "/v1/messages", {
    method: "POST",
    headers: {
      "content-type": "application/json",
      "x-api-key": process.env.ANTHROPIC_API_KEY,
      "anthropic-version": "2023-06-01",
    },
    body: JSON.stringify({model: "claude-haiku-4-5-20251001", max_tokens: 32,
      messages: [{role: "user", content: "Reply with the single word: ok"}]}),
    signal: AbortSignal.timeout(60000),
  }).then(r => console.log(r.status)).catch(e => { console.log("ERR"); });
')
echo "gateway responded: $STATUS"
if [ "$REAL_KEY" = "sk-ant-fake" ]; then
  [ "$STATUS" = "401" ] || fail "expected 401 from upstream with the fake key, got $STATUS"
  echo "ok (401 from upstream proves the proxy path and key swap; set a real ANTHROPIC_API_KEY for the full path)"
else
  [ "$STATUS" = "200" ] || fail "expected 200 with a real key, got $STATUS"
  SPENT=$(curl -sf "$SERVER/v1/budgets/default" | json "['spent_usd']")
  python3 -c "exit(0 if $SPENT > 0 else 1)" || fail "budget was not debited (spent_usd=$SPENT)"
  echo "ok (spent_usd=$SPENT)"
fi

step "audit trail"
curl -sf "$SERVER/v1/sessions/$SID/events" | grep -q 'session.created' \
  || fail "no session.created audit event"
echo "ok"

step "delete session"
curl -sf -X DELETE "$SERVER/v1/sessions/$SID" -o /dev/null
# The namespace is the control plane's and stays put; the session's own pod
# and NetworkPolicy are what must go.
for _ in $(seq 30); do
  kubectl -n "$NS" get pod "$POD" >/dev/null 2>&1 || break
  sleep 2
done
kubectl -n "$NS" get pod "$POD" >/dev/null 2>&1 && fail "sandbox pod $POD survived the session delete"
kubectl -n "$NS" get networkpolicy "$POD" >/dev/null 2>&1 && fail "networkpolicy $POD survived the session delete"
kubectl -n "$NS" get deploy paddock >/dev/null 2>&1 || fail "the control plane was torn down with the session"
SID=""
echo "ok"

printf '\nE2E SMOKE PASSED\n'
