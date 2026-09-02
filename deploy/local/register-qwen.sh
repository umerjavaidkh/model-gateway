#!/usr/bin/env bash
#
# Register a Qwen deployment.
#
# The gateway does not run models — ProviderPort is a client. So this points it
# at a server, and which server is a deployment decision the gateway is
# deliberately indifferent to:
#
#   ./register-qwen.sh                                  # local Ollama
#   ./register-qwen.sh https://api.together.xyz/v1 \
#       Qwen/Qwen2.5-7B-Instruct-Turbo env:TOGETHER_API_KEY
#
# Both are one row with provider "openai-compatible". Swapping between them is
# an UPDATE, with no code change and no restart — which is the whole point of
# the port, and worth doing once to see it happen.
#
# SQL rather than an API call because there is no endpoint to create a
# deployment yet. That gap is what M12 exists to close, and it is better seen
# than hidden behind a helper.
set -euo pipefail

readonly HERE="$(cd "$(dirname "$0")" && pwd)"

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

compose() { docker compose -f "$HERE/compose.yaml" "$@"; }

compose exec -T postgres psql -U gateway -d gateway -v ON_ERROR_STOP=1 \
  -v endpoint="$ENDPOINT" -v model="$MODEL" -v credential="$CREDENTIAL" -v tier="$TIER" <<'SQL'
-- Every column is NOT NULL with no default, so all of them are named. The
-- tedium here is the argument for a provisioning API: this is what onboarding
-- a model costs today, and it is one typo away from a constraint error.
insert into deployments
  (id, base_model, adapter_id, provider, endpoint, region, trust_tier,
   credential_ref, weight,
   input_cost_micro_usd, output_cost_micro_usd,
   cached_input_cost_micro_usd, cache_write_cost_micro_usd)
values
  ('qwen-1', :'model', '', 'openai-compatible', :'endpoint', 'local', :tier,
   :'credential', 100,
   100, 200, 0, 0)
on conflict (id) do update
  set endpoint       = excluded.endpoint,
      base_model     = excluded.base_model,
      credential_ref = excluded.credential_ref,
      trust_tier     = excluded.trust_tier;
SQL

echo "registered $MODEL at $ENDPOINT (trust tier $TIER)"
if [ "$TIER" = 1 ]; then
  echo "  external, so the PII chain will transform payloads on the way out"
fi
echo "  workers pick it up within one snapshot poll (5s); nothing restarts"
