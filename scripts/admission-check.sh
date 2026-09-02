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
# Real Docker when it is available, and a stub runtime otherwise. The two
# check different things and both are worth having: the stub proves the wiring
# on a machine with no runtime, and Docker proves the isolation flags the
# sandbox passes actually let a well-behaved component work. A flag that is
# wrong rather than missing — mounting the socket read-only, say — passes every
# argv assertion and fails only here.
RUNTIME="$ROOT/examples/admission/stub-runtime.sh"
IMAGE="ghcr.io/example/stub-guard@sha256:$(printf '0%.0s' {1..64})"
USING_DOCKER=no
if [ "${ADMISSION_CHECK_RUNTIME:-auto}" != "stub" ] && docker info >/dev/null 2>&1; then
  USING_DOCKER=yes
fi

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

# Each behaviour is its own image under Docker, because the sandbox passes no
# caller-controlled environment into the container — a component that
# misbehaves has to be different bytes, which is what it would be in reality.
IMAGE_CONFORMING="$IMAGE"
IMAGE_DENY_ALL="$IMAGE"

# The identity the sandbox will run the component as, mirroring
# internal/sandbox's componentUser: the runner's own uid so it owns the socket
# it has to connect to, and nobody when the runner is root.
#
# The probe has to use the same one. Probing as a different user answers a
# different question and gets a different answer.
component_user() {
  if [ "$(id -u)" = 0 ]; then
    echo "65534:65534"
  else
    echo "$(id -u):$(id -g)"
  fi
}

# Whether the host can reach a socket a container bound on a bind mount.
#
# It cannot on Docker Desktop for macOS: the mount crosses a VM boundary that
# does not proxy Unix socket connections, so the socket file appears on the
# host and connecting to it is refused. That is the environment's limitation
# rather than the sandbox's, and the check says so instead of reporting the
# component as broken.
socket_crosses_the_mount() {
  local dir sock ok=1
  dir="$(mktemp -d /tmp/udsprobe.XXXXXX)"
  chmod 777 "$dir"
  sock="$dir/probe.sock"

  docker run --rm -d --name gw-uds-probe \
    -v "$dir:/s" --user "$(component_user)" \
    -e COMPONENT_SOCKET=/s/probe.sock "$1" >/dev/null 2>&1 || {
    rm -rf "$dir"
    return 1
  }
  for _ in $(seq 1 40); do
    [ -S "$sock" ] && curl -sf --unix-socket "$sock" http://probe/healthz >/dev/null 2>&1 && {
      ok=0
      break
    }
    sleep 0.25
  done

  docker rm -f gw-uds-probe >/dev/null 2>&1 || true
  rm -rf "$dir"
  return "$ok"
}

if [ "$USING_DOCKER" = yes ]; then
  echo "==> building component images"
  for behaviour in conforming deny-all; do
    docker build -q --build-arg "BEHAVIOUR=$behaviour" \
      -t "gw-stub-component:$behaviour" "$ROOT/examples/admission" >/dev/null
  done
  # An image ID rather than a tag: a tag names whatever was built most
  # recently, and admitting an artifact by tag admits a different one tomorrow.
  IMAGE_CONFORMING="$(docker images --no-trunc -q gw-stub-component:conforming)"
  IMAGE_DENY_ALL="$(docker images --no-trunc -q gw-stub-component:deny-all)"

  if socket_crosses_the_mount "$IMAGE_CONFORMING"; then
    RUNTIME="docker"
    echo "  using docker, conforming image $IMAGE_CONFORMING"
  else
    USING_DOCKER=no
    IMAGE_CONFORMING="$IMAGE"
    IMAGE_DENY_ALL="$IMAGE"
    echo "  this host cannot reach a container's socket over a bind mount" \
      "(Docker Desktop crosses a VM boundary that does not proxy them);" \
      "using the stub runtime"
  fi
fi

if [ "$USING_DOCKER" = no ]; then
  echo "==> stub runtime: the wiring is checked, the isolation flags are not"
fi

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
         \"latency_budget_ms\":50,\"execution\":\"sidecar\",\"image\":\"$2\"}"
}
status_of() {
  admin "http://127.0.0.1:$ADMIN_PORT/v1/components/$1/1.0.0" \
    | python3 -c 'import json,sys; print(json.load(sys.stdin)["status"])'
}
build_snapshot() {
  admin -o /dev/null -w '%{http_code}' -X POST "http://127.0.0.1:$ADMIN_PORT/v1/snapshots"
}

echo "==> register a component; it is not yet bindable"
check "registration accepted" 201 "$(register stub-guard "$IMAGE_CONFORMING")"
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

# STUB_BEHAVIOUR only reaches the component under the stub runtime; under
# Docker the behaviour is baked into the image it was registered with.
run_admission() {
  STUB_BEHAVIOUR="$1" "$WORK/admissionrunner" \
    -control-plane "http://127.0.0.1:$ADMIN_PORT" \
    -token "$ADMIN_TOKEN" \
    -component "$2" -version 1.0.0 \
    -fixtures "$WORK/fixtures.json" \
    -runtime "$RUNTIME" \
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
check "registration accepted" 201 "$(register deny-everything "$IMAGE_DENY_ALL")"
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
