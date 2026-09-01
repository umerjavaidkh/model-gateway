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
cleanup() {
  [ -n "$SIDECAR_PID" ] && kill "$SIDECAR_PID" 2>/dev/null || true
  rm -rf "$WORK"
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
