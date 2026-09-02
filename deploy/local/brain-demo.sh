#!/usr/bin/env bash
#
# Prove the brain seam against a real model.
#
# Three steps, and the interesting part is what does not happen in any of them:
# the gateway never calls the brain. Rules are authored outside, compiled into
# a snapshot, and evaluated in-process — so a policy change reaches every
# worker within one poll and costs nothing per request.
set -euo pipefail

readonly HERE="$(cd "$(dirname "$0")" && pwd)"
readonly ROOT="$(cd "$HERE/../.." && pwd)"
readonly KEY="gw_demo_local-development-key"
#: Whatever was registered. The demo does not care which server runs it.
MODEL="${QWEN_MODEL:-qwen2.5:0.5b}"
readonly MODEL
readonly WORKER="http://localhost:18080"
#: One snapshot poll plus slack. The workers are configured at 5s.
readonly PROPAGATION=8

brain() { (cd "$ROOT/controlplane" && uv run python "$ROOT/examples/brain/brain.py" "$@"); }

ask() {
  curl -s -X POST "$WORKER/v1/chat/completions" \
    -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
    -d "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"$1\"}],\"max_tokens\":40}"
}

status_of() {
  curl -s -o /dev/null -w '%{http_code}' -X POST "$WORKER/v1/chat/completions" \
    -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
    -d "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"hello\"}],\"max_tokens\":8}"
}

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

echo "==> the brain approves the model"
brain approve "$MODEL" >/dev/null
sleep "$PROPAGATION"
check "an approved model is served" 200 "$(status_of)"

echo
echo "  what the model actually said:"
ask "In one short sentence, what is a model gateway?" \
  | python3 -c 'import json,sys; d=json.load(sys.stdin); print("   ", d["choices"][0]["message"]["content"].strip()[:200])' \
  2>/dev/null || echo "    (no content; the model may still be loading)"

echo
echo "==> the brain withdraws approval"
# Nothing is deployed, nothing restarts, and no request is routed through the
# brain to make this happen.
brain revoke "$MODEL" >/dev/null
sleep "$PROPAGATION"
check "a revoked model is refused" 403 "$(status_of)"

echo "  the refusal names the rule that decided it:"
ask "hello" | python3 -c 'import json,sys; print("   ", json.load(sys.stdin)["error"]["message"])' 2>/dev/null || true

echo
echo "==> the brain reads what actually happened"
# Off the request path entirely: it consumes the usage stream, which the
# gateway does not know it has.
brain report | sed 's/^/  /'

echo
echo "==> restore"
brain open >/dev/null
sleep "$PROPAGATION"
check "the model is served again" 200 "$(status_of)"

if [ "$fail" -ne 0 ]; then
  echo "brain demo failed" >&2
  exit 1
fi
echo "brain demo passed"
