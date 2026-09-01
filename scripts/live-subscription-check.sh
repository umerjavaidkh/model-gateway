#!/usr/bin/env bash
#
# Prove that configuration reaches a running worker.
#
# Everything before this could be verified with a file: Python compiled a
# snapshot, Go loaded it at startup. This checks the property the whole design
# is for — a key issued through the admin API starts working on a worker that is
# already running and is never restarted.
#
# It also checks the failure mode, which matters more: with the control plane
# stopped, the worker keeps serving. "A control-plane outage freezes
# configuration, not traffic" is a claim, and this is the only place it is
# tested.
set -euo pipefail

readonly ROOT="$(cd "$(dirname "$0")/.." && pwd)"
readonly PEPPER="live-subscription-check-pepper-32b!"
readonly ADMIN_TOKEN="live-subscription-check-admin-token"
readonly ADMIN_PORT="${LIVE_ADMIN_PORT:-18201}"
readonly WORKER_PORT="${LIVE_WORKER_PORT:-18202}"

WORK="$(mktemp -d)"
ADMIN_PID=""
WORKER_PID=""
ACCOUNTING_PID=""
cleanup() {
  [ -n "$ACCOUNTING_PID" ] && kill "$ACCOUNTING_PID" 2>/dev/null || true
  [ -n "$WORKER_PID" ] && kill "$WORKER_PID" 2>/dev/null || true
  [ -n "$ADMIN_PID" ] && kill "$ADMIN_PID" 2>/dev/null || true
  rm -rf "$WORK"
}
trap cleanup EXIT

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

admin() { curl -s -H "Authorization: Bearer $ADMIN_TOKEN" "$@"; }
call_gateway() {
  curl -s -o /dev/null -w '%{http_code}' -X POST \
    "http://127.0.0.1:$WORKER_PORT/v1/chat/completions" \
    -H "Authorization: Bearer $1" -d '{"model":"fast","messages":[]}'
}

echo "==> control plane: migrate and seed"
export GATEWAY_DATABASE_URL="sqlite+aiosqlite:///$WORK/gateway.db"
cd "$ROOT/controlplane"
uv run alembic upgrade head >/dev/null
uv run python - <<'PY'
import asyncio, os
from model_gateway_control.db import models
from model_gateway_control.db.session import create_engine, session_factory

async def main():
    engine = create_engine(os.environ["GATEWAY_DATABASE_URL"])
    async with session_factory(engine)() as s:
        s.add(models.Tenant(id="demo", tier="demo", version=1, min_trust_tier=1))
        s.add(models.KeyPrefix(prefix="demo", tenant_id="demo"))
        s.add(models.Org(id="demo-org", tenant_id="demo", name="Demo"))
        s.add(models.Team(id="demo-team", org_id="demo-org", name="Demo"))
        s.add(models.Application(id="demo-app", team_id="demo-team", name="Demo"))
        s.add(models.Budget(
            id="demo-budget", tenant_id="demo", scope=5,
            limit_micro_usd=2000, spent_micro_usd=0, hard=True,
            headroom_basis_points=0))
        s.add(models.Deployment(
            id="echo-1", base_model="echo-model", provider="echo",
            endpoint="in-process", trust_tier=3, weight=100,
            input_cost_micro_usd=1_000_000, output_cost_micro_usd=1_000_000))
        alias = models.Alias(tenant_id=None, name="fast")
        alias.targets = [models.AliasTarget(position=0, base_model="echo-model")]
        s.add(alias)
        await s.commit()
    await engine.dispose()

asyncio.run(main())
PY

echo "==> control plane: start"
GATEWAY_KEY_PEPPER="$PEPPER" GATEWAY_ADMIN_TOKEN="$ADMIN_TOKEN" \
GATEWAY_ADMIN_PORT="$ADMIN_PORT" \
  uv run gateway-admin >"$WORK/admin.log" 2>&1 &
ADMIN_PID=$!
curl -sf --retry 40 --retry-delay 1 --retry-connrefused \
  -o /dev/null "http://127.0.0.1:$ADMIN_PORT/healthz"

echo "==> worker: start, subscribing to the control plane"
(cd "$ROOT/dataplane" && go build -o "$WORK/gateway" ./cmd/gateway)
# GATEWAY_REDIS_URL is passed through when the environment provides one, so
# this check exercises the Redis path in CI and the in-process one locally.
GATEWAY_CONTROL_PLANE_URL="http://127.0.0.1:$ADMIN_PORT" \
GATEWAY_CONTROL_PLANE_TOKEN="$ADMIN_TOKEN" \
GATEWAY_REDIS_URL="${GATEWAY_TEST_REDIS_URL:-}" \
GATEWAY_KEY_PEPPER="$PEPPER" \
GATEWAY_LISTEN_ADDR="127.0.0.1:$WORKER_PORT" \
GATEWAY_SNAPSHOT_INTERVAL="1s" \
  "$WORK/gateway" >"$WORK/worker.log" 2>&1 &
WORKER_PID=$!
if ! curl -sf --retry 40 --retry-delay 1 --retry-connrefused \
  -o /dev/null "http://127.0.0.1:$WORKER_PORT/healthz"; then
  echo "FAIL: the worker did not start" >&2
  cat "$WORK/worker.log" >&2
  exit 1
fi
echo "  ok   worker bootstrapped from the control plane, no snapshot file"

echo "==> issue a key while the worker is running"
issued=$(admin -X POST "http://127.0.0.1:$ADMIN_PORT/v1/tenants/demo/keys" \
  -H 'Content-Type: application/json' \
  -d '{"key_id":"live-1","application_id":"demo-app","models_allow_all":true}')
key=$(printf '%s' "$issued" | sed -n 's/.*"presented": *"\([^"]*\)".*/\1/p')
if [ -z "$key" ]; then
  echo "FAIL: no key issued: $issued" >&2
  exit 1
fi

# Before the next poll the worker has never heard of this key.
check "an unknown key is rejected before propagation" 401 "$(call_gateway "$key")"

echo "==> wait for the worker to pick it up"
accepted=0
for _ in $(seq 1 30); do
  if [ "$(call_gateway "$key")" = "200" ]; then accepted=1; break; fi
  sleep 1
done
check "the key works without restarting the worker" 1 "$accepted"

applied=$(curl -s "http://127.0.0.1:$WORKER_PORT/readyz" |
  sed -n 's/.*"applied":\([0-9]*\).*/\1/p')
if [ "${applied:-0}" -ge 1 ]; then
  echo "  ok   the worker applied $applied snapshot(s) while running"
else
  echo "  FAIL the worker reports no applied snapshots" >&2
  fail=1
fi

echo "==> a rate-limited key is refused once its allowance is spent"
limited=$(admin -X POST "http://127.0.0.1:$ADMIN_PORT/v1/tenants/demo/keys" \
  -H 'Content-Type: application/json' \
  -d '{"key_id":"limited-1","application_id":"demo-app","models_allow_all":true,"requests_per_minute":3}')
limited_key=$(printf '%s' "$limited" | sed -n 's/.*"presented": *"\([^"]*\)".*/\1/p')

for _ in $(seq 1 30); do
  [ "$(call_gateway "$limited_key")" = "200" ] && break
  sleep 1
done

allowed=0
for _ in $(seq 1 12); do
  [ "$(call_gateway "$limited_key")" = "200" ] && allowed=$((allowed + 1))
done
if [ "$allowed" -lt 12 ]; then
  echo "  ok   the limit refused traffic after its allowance ($allowed of 12 admitted)"
else
  echo "  FAIL a key limited to 3/min admitted all 12 requests" >&2
  fail=1
fi

# Everything above proves configuration flows outward. This proves measurement
# flows back: a request costs money, the consumer records it, the builder folds
# it into the next snapshot, and admission refuses the next request. Without
# this the budget arithmetic is only ever asserted in a unit test.
if [ -n "${GATEWAY_TEST_REDIS_URL:-}" ]; then
  echo "==> the budget loop closes"
  (cd "$ROOT/controlplane" && GATEWAY_DATABASE_URL="$GATEWAY_DATABASE_URL" \
    GATEWAY_REDIS_URL="$GATEWAY_TEST_REDIS_URL" \
    uv run gateway-accounting >"$WORK/accounting.log" 2>&1) &
  ACCOUNTING_PID=$!

  budgeted=$(admin -X POST "http://127.0.0.1:$ADMIN_PORT/v1/tenants/demo/keys" \
    -H 'Content-Type: application/json' \
    -d '{"key_id":"budgeted-1","application_id":"demo-app","models_allow_all":true}')
  budgeted_key=$(printf '%s' "$budgeted" | sed -n 's/.*"presented": *"\([^"]*\)".*/\1/p')

  # Attaching the budget to the key is what makes its spend chargeable.
  (cd "$ROOT/controlplane" && uv run python "$ROOT/scripts/attach_demo_budget.py") >/dev/null

  for _ in $(seq 1 30); do
    [ "$(call_gateway "$budgeted_key")" = "200" ] && break
    sleep 1
  done

  # Each request costs far more than the budget allows, so once the spend has
  # been recorded and propagated the next one must be refused.
  refused=0
  for _ in $(seq 1 40); do
    if [ "$(call_gateway "$budgeted_key")" = "402" ]; then refused=1; break; fi
    sleep 1
  done

  kill "$ACCOUNTING_PID" 2>/dev/null || true
  ACCOUNTING_PID=""
  if [ "$refused" -eq 1 ]; then
    echo "  ok   spend was recorded and the budget refused the next request (402)"
  else
    echo "  FAIL the budget never refused; the loop is open" >&2
    tail -20 "$WORK/accounting.log" >&2
    fail=1
  fi
fi

echo "==> stop the control plane; traffic must continue"
kill "$ADMIN_PID" 2>/dev/null || true
ADMIN_PID=""
sleep 3
check "traffic survives a control-plane outage" 200 "$(call_gateway "$key")"

if [ "$fail" -ne 0 ]; then
  echo "live subscription check FAILED" >&2
  exit 1
fi
echo "live subscription check passed"
