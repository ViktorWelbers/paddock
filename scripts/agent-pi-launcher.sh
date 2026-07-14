#!/bin/sh
# Governed launcher for the pi coding agent, installed as `pi` on PATH
# (the real binary lives under /opt/pi). pi reads providers from
# ~/.pi/agent/models.json and cannot interpolate env vars into baseUrl,
# so this renders the file from the sandbox env on every launch and then
# execs the real pi pinned to the gateway-backed provider.
set -eu

: "${PADDOCK_OPENAI_BASE_URL:?not running in a paddock sandbox}"
: "${PADDOCK_MODEL:?not running in a paddock sandbox}"

mkdir -p "$HOME/.pi/agent"
cat > "$HOME/.pi/agent/models.json" <<EOF
{
  "providers": {
    "paddock": {
      "name": "Paddock Gateway",
      "baseUrl": "${PADDOCK_OPENAI_BASE_URL}",
      "api": "openai-completions",
      "apiKey": "\$PI_API_KEY",
      "models": [
        {
          "id": "${PADDOCK_MODEL}",
          "name": "${PADDOCK_MODEL} (governed)",
          "reasoning": false,
          "input": ["text"],
          "contextWindow": ${PADDOCK_MODEL_CONTEXT:-8192},
          "maxTokens": ${PADDOCK_MODEL_MAX_TOKENS:-2048},
          "cost": {"input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0}
        }
      ]
    }
  }
}
EOF

exec /opt/pi/bin/pi --provider paddock --model "$PADDOCK_MODEL" "$@"
