#!/usr/bin/env bash
# End-to-end test for the pi agent: sandbox -> gateway /openai -> the
# OpenAI-compatible upstream (vLLM). Unlike the Anthropic smoke test this
# needs the real upstream reachable from the cluster, because the whole
# point is a live governed completion: pi runs non-interactively inside the
# sandbox, the gateway meters usage, and the budget ledger is debited.
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

SPENT_BEFORE=$(curl -sf "$SERVER/v1/budgets/default" | json "['spent_usd']")

step "create pi session"
RESP=$(curl -sf -X POST "$SERVER/v1/sessions" -H 'content-type: application/json' \
  -d '{"user":"e2e","agent":"pi","budget_id":"default"}') \
  || fail "session create rejected — is the pi agent configured (agentImagePi + gateway.openai.*)?"
SID=$(echo "$RESP" | json "['id']")
echo "session: $SID"
NS="paddock-ses-$SID"

step "wait for sandbox pod"
kubectl -n "$NS" wait --for=condition=Ready pod/agent --timeout=180s

step "sandbox env: openai gateway URL + session token (never a real key)"
kubectl -n "$NS" exec agent -- printenv PADDOCK_OPENAI_BASE_URL | grep -q '/openai/v1' \
  || fail "PADDOCK_OPENAI_BASE_URL not set in sandbox"
kubectl -n "$NS" exec agent -- printenv PI_API_KEY | grep -q '^pdk_' \
  || fail "PI_API_KEY in sandbox is not a pdk_ session token"
kubectl -n "$NS" exec agent -- printenv PADDOCK_MODEL || fail "PADDOCK_MODEL not set"

step "netpol: sandbox cannot reach the upstream directly"
if kubectl -n "$NS" exec agent -- node -e '
  fetch("https://vllm.internal/v1/models", {signal: AbortSignal.timeout(8000)})
    .then(() => process.exit(0)).catch(() => process.exit(1));
' >/dev/null 2>&1; then
  fail "sandbox reached vllm.internal directly — NetworkPolicy is NOT enforced"
fi
echo "ok (direct egress blocked; only the gateway may talk to vLLM)"

step "pi runs a governed completion inside the sandbox"
# stdin must be closed: pi's -p mode waits on an open stdin.
OUT=$(kubectl -n "$NS" exec agent -- sh -c \
  'pi --no-session --no-tools -p "Reply with exactly one word: PONG" </dev/null 2>&1') \
  || { echo "$OUT"; fail "pi exited non-zero inside the sandbox"; }
echo "$OUT" | tail -3
echo "$OUT" | grep -qi PONG || fail "pi did not relay the model's reply"

step "budget debited via the openai metering path"
SPENT_AFTER=$(curl -sf "$SERVER/v1/budgets/default" | json "['spent_usd']")
python3 -c "exit(0 if $SPENT_AFTER > $SPENT_BEFORE else 1)" \
  || fail "budget not debited (before=$SPENT_BEFORE after=$SPENT_AFTER)"
echo "ok (spent_usd $SPENT_BEFORE -> $SPENT_AFTER)"

step "audit trail has the model call"
EVENTS=$(curl -sf "$SERVER/v1/sessions/$SID/events")
echo "$EVENTS" | grep -q 'session.created' || fail "no session.created audit event"
echo "$EVENTS" | grep -q 'model.call' || fail "no model.call audit event"
echo "ok"

step "delete session"
curl -sf -X DELETE "$SERVER/v1/sessions/$SID" -o /dev/null
for _ in $(seq 30); do
  kubectl get ns "$NS" >/dev/null 2>&1 || break
  sleep 2
done
SID=""
echo "ok"

printf '\nE2E PI PASSED\n'
