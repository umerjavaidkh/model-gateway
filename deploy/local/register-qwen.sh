#!/usr/bin/env bash
#
# Register a Qwen deployment through the admin API.
#
# This used to be a hand-written INSERT against a table with thirteen NOT NULL
# columns and no defaults — three attempts and two constraint errors before
# anything worked. That is what the provisioning API replaced, and the
# difference between the two versions of this file is the argument for it.
#
#   ./register-qwen.sh                                  # local Ollama
#   ./register-qwen.sh https://api.together.xyz/v1 \
#       Qwen/Qwen2.5-7B-Instruct-Turbo env:TOGETHER_API_KEY
#
# Both are one PUT with provider "openai-compatible". Swapping between them
# changes an endpoint and nothing else — no code, no restart — which is what
# ProviderPort is for.
set -euo pipefail

readonly ADMIN="${GATEWAY_ADMIN_URL:-http://localhost:18081}"
readonly TOKEN="${GATEWAY_ADMIN_TOKEN:-local-development-admin-token-32ch}"

# host.docker.internal reaches Ollama's published port from inside the worker
# containers. A hosted provider is just a public URL.
ENDPOINT="${1:-http://host.docker.internal:11434/v1}"
MODEL="${2:-qwen2.5:0.5b}"
CREDENTIAL="${3:-}"

# Trust tier decides whether the PII chain transforms payloads on the way out.
# A model on this machine is internal and nothing leaves the host; a hosted API
# is external, and redaction is exactly what should happen.
TIER=3
case "$ENDPOINT" in
  http://host.docker.internal:*|http://localhost:*|http://127.0.0.1:*) TIER=3 ;;
  *) TIER=1 ;;
esac

status="$(curl -s -o /tmp/register-qwen.out -w '%{http_code}' \
  -X PUT "$ADMIN/v1/deployments/qwen-1" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d "{\"base_model\":\"$MODEL\",\"provider\":\"openai-compatible\",
       \"endpoint\":\"$ENDPOINT\",\"trust_tier\":$TIER,
       \"credential_ref\":\"$CREDENTIAL\",
       \"input_cost_micro_usd\":100,\"output_cost_micro_usd\":200}")"

if [ "$status" != "200" ]; then
  echo "the gateway refused: $status" >&2
  cat /tmp/register-qwen.out >&2
  exit 1
fi

echo "registered $MODEL at $ENDPOINT (trust tier $TIER)"
if [ "$TIER" = 1 ]; then
  echo "  external, so the PII chain will transform payloads on the way out"
fi
echo "  workers pick it up within one snapshot poll (5s); nothing restarts"
