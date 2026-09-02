#!/usr/bin/env bash
#
# Prove the Go client and the Python sidecar agree about offsets.
#
# Both sides are unit tested and both pass while disagreeing: Python indexes
# characters, Go indexes bytes, and a payload of pure ASCII hides the
# difference completely. The Go client verifies every span and silently drops
# the ones that do not match, so a regression here does not fail loudly — it
# stops protecting requests.
#
# So: the real sidecar runs on a real socket, and the real client asks it about
# text containing multi-byte characters. Nothing is mocked.
set -euo pipefail

readonly ROOT="$(cd "$(dirname "$0")/.." && pwd)"

WORK="$(mktemp -d /tmp/nercheck.XXXXXX)"
SIDECAR_PID=""
# stop waits for a process to actually exit after asking it to.
#
# Without the wait, the rm below can race a process that has been signalled but
# not yet reaped, and removing a directory holding its running executable fails
# with a permission error. That turns a passing check into a failing one after
# it has already printed that it passed, which is the worst way for a check to
# be flaky.
stop() {
  [ -n "${1:-}" ] || return 0
  kill "$1" 2>/dev/null || return 0
  wait "$1" 2>/dev/null || true
}

cleanup() {
  stop "$SIDECAR_PID"
  rm -rf "$WORK" 2>/dev/null || echo "could not remove $WORK" >&2
}
trap cleanup EXIT

readonly SOCKET="$WORK/s"

echo "==> starting the sidecar"
(cd "$ROOT/sidecars/pii-ner" && PII_NER_SOCKET="$SOCKET" uv run pii-ner >"$WORK/log" 2>&1) &
SIDECAR_PID=$!

for _ in $(seq 1 50); do
  [ -S "$SOCKET" ] && break
  sleep 0.2
done
if [ ! -S "$SOCKET" ]; then
  echo "the sidecar never bound its socket:" >&2
  cat "$WORK/log" >&2
  exit 1
fi

echo "==> the Go client asks it about multi-byte text"
cd "$ROOT/dataplane"
PII_NER_SOCKET="$SOCKET" go run ./cmd/nercheck

echo "==> ok"
