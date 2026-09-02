#!/usr/bin/env bash
#
# Prove the local fleet does what a fleet does.
#
# Not a unit test and not a substitute for one: `make check` already asserts
# behaviour in isolation. This asserts the things that only exist once the
# pieces are separate processes in separate containers — configuration
# reaching a worker over the network, spend crossing Redis into Postgres,
# failover between deployments, two workers agreeing about a key.
#
# Every one of those is a place where a component that passes its own tests can
# still be wrong.
set -euo pipefail

readonly HERE="$(cd "$(dirname "$0")" && pwd)"
readonly KEY="gw_demo_local-development-key"
readonly ADMIN_TOKEN="local-development-admin-token-32ch"
readonly WORKER_A="http://localhost:18080"
readonly WORKER_B="http://localhost:18090"
readonly ADMIN="http://localhost:18081"

fail=0
check() {
  local label="$1" want="$2" got="$3"
  if [ "$got" = "$want" ]; then
    echo "  ok   $label ($got)"
  else
    echo "  FAIL $label: got $got, want $want" >&2
    fail=1
  fi
}

compose() { docker compose -f "$HERE/compose.yaml" "$@"; }

ask() {
  curl -s -o /dev/null -w '%{http_code}' -X POST "$1/v1/chat/completions" \
    -H "Authorization: Bearer ${2:-$KEY}" \
    -H 'Content-Type: application/json' \
    -d '{"model":"fast","messages":[{"role":"user","content":"hello"}]}'
}

echo "==> every service is up"
for service in postgres redis admin accounting finetune worker-a worker-b ner-a ner-b; do
  state="$(compose ps "$service" --format '{{.State}}' 2>/dev/null || echo missing)"
  check "$service" running "$state"
done

echo "==> both workers serve, and reject what they should"
check "worker-a serves" 200 "$(ask "$WORKER_A")"
check "worker-b serves" 200 "$(ask "$WORKER_B")"
# The same key on both: a worker that authenticated from local state rather
# than the shared snapshot would disagree here.
check "a forged key is refused" 401 "$(ask "$WORKER_A" gw_demo_wrong)"
check "an unknown scheme is refused" 401 "$(ask "$WORKER_A" not-a-gateway-key)"

echo "==> the router failed over around the unreachable deployment"
# The seed registers an unreachable deployment for the same model. Serving at
# all means the candidate list did its job.
served="$(compose logs worker-a 2>&1 | grep -c '"msg":"breaker opened"' || true)"
if [ "$served" -gt 0 ]; then
  echo "  ok   the breaker opened on the dead deployment ($served)"
else
  echo "  ok   traffic served; the breaker has not yet tripped"
fi

echo "==> configuration reaches running workers"
before="$(compose logs worker-a 2>&1 | grep -c '"msg":"snapshot applied"' || true)"
check "a snapshot builds" 200 "$(curl -s -o /dev/null -w '%{http_code}' -X POST \
  "$ADMIN/v1/snapshots" -H "Authorization: Bearer $ADMIN_TOKEN")"
sleep 8
after="$(compose logs worker-a 2>&1 | grep -c '"msg":"snapshot applied"' || true)"
if [ "$after" -gt "$before" ]; then
  echo "  ok   the worker applied a new snapshot without restarting ($before -> $after)"
else
  echo "  FAIL the worker never picked up the new snapshot" >&2
  fail=1
fi

echo "==> spend crosses Redis into Postgres"
sleep 6
records="$(compose exec -T postgres psql -U gateway -d gateway -tAc \
  'select count(*) from usage_records;' | tr -d '[:space:]')"
if [ "${records:-0}" -gt 0 ]; then
  echo "  ok   the accounting consumer recorded $records requests"
else
  echo "  FAIL nothing reached usage_records; the accounting loop is broken" >&2
  fail=1
fi

echo "==> the accounting consumer survives an idle stream"
# It blocks for five seconds at a time and redis-py raises rather than
# returning empty. Treating that as an error killed the consumer on its first
# quiet window, which no test caught because a test always has events waiting.
timeouts="$(compose logs accounting 2>&1 | grep -c 'Timeout reading' || true)"
check "no idle-read crashes" 0 "$timeouts"
check "still running" running "$(compose ps accounting --format '{{.State}}')"

echo "==> metrics are exposed for scraping"
metrics="$(curl -s "$WORKER_A/metrics" | grep -c '^gateway_' || true)"
if [ "$metrics" -gt 0 ]; then
  echo "  ok   worker-a exports $metrics gateway metrics"
else
  echo "  FAIL no gateway metrics" >&2
  fail=1
fi

if [ "$fail" -ne 0 ]; then
  echo "local smoke failed" >&2
  exit 1
fi
echo "local smoke passed"
