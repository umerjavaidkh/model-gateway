#!/usr/bin/env bash
#
# Prove the Python control plane and the Go data plane agree.
#
# The snapshot is a contract between two languages. Unit tests on either side
# can both pass while the two disagree about what a field means, what order map
# entries serialize in, or how a key lookup is derived — and the failure only
# appears when a real worker rejects every real key.
#
# So: Python compiles a snapshot and issues a key; Go loads that exact file and
# authenticates that exact key. Nothing is mocked and no fixture is committed,
# because a committed fixture stops testing the producer.
set -euo pipefail

readonly ROOT="$(cd "$(dirname "$0")/.." && pwd)"
readonly PEPPER="cross-language-check-pepper-32-bytes"
readonly PORT="${CROSSCHECK_PORT:-18099}"

WORK="$(mktemp -d)"
GATEWAY_PID=""
cleanup() {
  [ -n "$GATEWAY_PID" ] && kill "$GATEWAY_PID" 2>/dev/null || true
  rm -rf "$WORK"
}
trap cleanup EXIT

echo "==> Python builds the snapshot"
issued=$(cd "$ROOT/controlplane" && uv run gatewayctl build-snapshot \
  --config "$ROOT/examples/demo-snapshot.json" \
  --out "$WORK/snapshot.pb" \
  --pepper "$PEPPER")
echo "$issued"

key=$(echo "$issued" | awk '/demo-key:/ {print $2}')
if [ -z "$key" ]; then
  echo "FAIL: the control plane issued no key" >&2
  exit 1
fi

echo "==> Go loads it and serves"
(cd "$ROOT/dataplane" && go build -o "$WORK/gateway" ./cmd/gateway)
GATEWAY_SNAPSHOT_FILE="$WORK/snapshot.pb" \
GATEWAY_KEY_PEPPER="$PEPPER" \
GATEWAY_LISTEN_ADDR="127.0.0.1:$PORT" \
  "$WORK/gateway" >"$WORK/gateway.log" 2>&1 &
GATEWAY_PID=$!

if ! curl -sf --retry 40 --retry-delay 1 --retry-connrefused \
  -o /dev/null "http://127.0.0.1:$PORT/healthz"; then
  echo "FAIL: the worker did not start" >&2
  cat "$WORK/gateway.log" >&2
  exit 1
fi

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

# The key the control plane just issued must authenticate, which is only true
# if both languages derive the same HMAC lookup from the same pepper.
check "python-issued key is accepted" 200 "$(curl -s -o /dev/null -w '%{http_code}' \
  -X POST "http://127.0.0.1:$PORT/v1/chat/completions" \
  -H "Authorization: Bearer $key" \
  -d '{"model":"fast","messages":[{"role":"user","content":"hi"}]}')"

check "a forged key is rejected" 401 "$(curl -s -o /dev/null -w '%{http_code}' \
  -X POST "http://127.0.0.1:$PORT/v1/chat/completions" \
  -H "Authorization: Bearer gw_demo_forged" -d '{"model":"fast"}')"

# The alias only resolves if the global layer decoded correctly; the budget
# check only passes if the tenant layer did.
check "an unknown model is a 404" 404 "$(curl -s -o /dev/null -w '%{http_code}' \
  -X POST "http://127.0.0.1:$PORT/v1/chat/completions" \
  -H "Authorization: Bearer $key" -d '{"model":"no-such-model"}')"

# Go verifies the digest Python computed. A mismatch here means the two sides
# serialize the same message differently.
version=$(curl -s "http://127.0.0.1:$PORT/readyz")
if ! echo "$version" | grep -q '"snapshot_digest":"sha256:'; then
  echo "  FAIL the worker did not verify a digest: $version" >&2
  fail=1
else
  echo "  ok   go verified the digest python computed"
fi

if [ "$fail" -ne 0 ]; then
  echo "cross-language check FAILED" >&2
  exit 1
fi
echo "cross-language check passed"
