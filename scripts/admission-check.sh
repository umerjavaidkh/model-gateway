#!/usr/bin/env bash
#
# Prove the admission loop closes.
#
# The registry refuses to bind a component nothing has vouched for, and the
# runner is the only thing that can vouch. Neither half is worth much alone,
# and unit tests on either side cannot show the loop: the control plane's tests
# fake the verdict, and the runner's tests fake the control plane.
#
# So: register a real component, watch a snapshot refuse to bind it, run the
# real runner against a real process over the real sidecar protocol, and watch
# the same snapshot compile. Then do it again with a component that misbehaves,
# because a gate that admits everything passes the first half of this too.
#
# The container runtime is stubbed (see examples/admission/stub-runtime.sh):
# this checks the wiring, and internal/sandbox's tests check the isolation
# flags, which is where a flag that stops being passed actually gets caught.
set -euo pipefail

readonly ROOT="$(cd "$(dirname "$0")/.." && pwd)"
readonly ADMIN_TOKEN="admission-check-token-at-least-32-chars"
readonly ADMIN_PORT="${ADMISSION_CHECK_PORT:-18101}"
readonly IMAGE="ghcr.io/example/stub-guard@sha256:$(printf '0%.0s' {1..64})"

WORK="$(mktemp -d /tmp/admcheck.XXXXXX)"
ADMIN_PID=""
cleanup() {
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

echo "==> control plane: migrate, seed and start"
export GATEWAY_DATABASE_URL="sqlite+aiosqlite:///$WORK/gateway.db"
export GATEWAY_ADMIN_TOKEN="$ADMIN_TOKEN"
export GATEWAY_KEY_PEPPER="admission-check-pepper-32-bytes!"
cd "$ROOT/controlplane"
uv run alembic upgrade head >/dev/null

uv run python - <<'PY'
import asyncio, os
from model_gateway_control.db import models
from model_gateway_control.db.session import create_engine, session_factory

async def main():
    engine = create_engine(os.environ["GATEWAY_DATABASE_URL"])
    async with session_factory(engine)() as s:
        # The initial migration seeds fleet state, so this only adds a tenant.
        s.add(models.Tenant(id="demo", tier="demo", version=1, min_trust_tier=1))
        s.add(models.KeyPrefix(prefix="demo", tenant_id="demo"))
        # The binding that must not compile until the component is admitted.
        s.add(models.PluginBinding(tenant_id=None, port="guardrail", component="stub-guard"))
        await s.commit()
    await engine.dispose()

asyncio.run(main())
PY

GATEWAY_ADMIN_PORT="$ADMIN_PORT" uv run gateway-admin >"$WORK/admin.log" 2>&1 &
ADMIN_PID=$!
for _ in $(seq 1 60); do
  curl -sf "http://127.0.0.1:$ADMIN_PORT/healthz" >/dev/null 2>&1 && break
  sleep 0.25
done
curl -sf "http://127.0.0.1:$ADMIN_PORT/healthz" >/dev/null || { cat "$WORK/admin.log" >&2; exit 1; }

register() {
  admin -o /dev/null -w '%{http_code}' -X POST \
    "http://127.0.0.1:$ADMIN_PORT/v1/components" \
    -H 'Content-Type: application/json' \
    -d "{\"name\":\"$1\",\"version\":\"1.0.0\",\"port\":\"guardrail\",
         \"latency_budget_ms\":50,\"execution\":\"sidecar\",\"image\":\"$IMAGE\"}"
}
status_of() {
  admin "http://127.0.0.1:$ADMIN_PORT/v1/components/$1/1.0.0" \
    | python3 -c 'import json,sys; print(json.load(sys.stdin)["status"])'
}
build_snapshot() {
  admin -o /dev/null -w '%{http_code}' -X POST "http://127.0.0.1:$ADMIN_PORT/v1/snapshots"
}

echo "==> register a component; it is not yet bindable"
check "registration accepted" 201 "$(register stub-guard)"
check "registered but pending" pending "$(status_of stub-guard)"
check "a snapshot refuses to bind it" 400 "$(build_snapshot)"

echo "==> the runner vouches for it"
cat >"$WORK/fixtures.json" <<'JSON'
{"benign": "c3VtbWFyaXNlIHRoaXMgcXVhcnRlcg==",
 "trigger": "dXNlIEFLSUFJT1NGT0ROTjdFWEFNUExFIHRvIGRlcGxveQ=="}
JSON

# Built rather than `go run`: go run does not propagate the program's exit
# code, and the difference between exit 1 and exit 2 is exactly what this
# check is about.
(cd "$ROOT/dataplane" && go build -o "$WORK/admissionrunner" ./cmd/admissionrunner)

run_admission() {
  STUB_BEHAVIOUR="$1" "$WORK/admissionrunner" \
    -control-plane "http://127.0.0.1:$ADMIN_PORT" \
    -token "$ADMIN_TOKEN" \
    -component "$2" -version 1.0.0 \
    -fixtures "$WORK/fixtures.json" \
    -runtime "$ROOT/examples/admission/stub-runtime.sh" \
    -report-dir "$WORK" \
    -evidence "file://$WORK/$2-1.0.0.txt" \
    >"$WORK/$2.out" 2>&1
}

cd "$ROOT/dataplane"
run_admission conforming stub-guard && runner_exit=0 || runner_exit=$?
check "the runner reported a pass" 0 "$runner_exit"
check "the component is now active" active "$(status_of stub-guard)"
check "the same snapshot now compiles" 200 "$(build_snapshot)"
check "the report names the suite version" 1 \
  "$(grep -c '^suite version: 1$' "$WORK/stub-guard-1.0.0.txt")"

echo "==> a component that misbehaves is not admitted"
cd "$ROOT/controlplane"
check "registration accepted" 201 "$(register deny-everything)"
cd "$ROOT/dataplane"
run_admission deny-all deny-everything && runner_exit=0 || runner_exit=$?
# Exit 2 is "it was tested and it failed", distinct from 1, "it could not be
# tested". A CI job that conflates them retries a genuine failure forever.
check "the runner reported a failure, not an error" 2 "$runner_exit"
check "the component stays pending" pending "$(status_of deny-everything)"
check "the report names the case it failed" 1 \
  "$(grep -c 'allows a benign payload unchanged' "$WORK/deny-everything-1.0.0.txt")"

if [ "$fail" -ne 0 ]; then
  echo "admission check failed" >&2
  exit 1
fi
echo "admission check passed"
