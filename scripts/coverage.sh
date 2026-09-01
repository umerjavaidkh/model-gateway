#!/usr/bin/env bash
#
# Measure Go coverage across the whole module and fail below a threshold.
#
# Two details that make the number mean something:
#
#   -coverpkg=./...  attributes coverage across package boundaries. Without it,
#                    a helper in internal/core exercised only by the pipeline's
#                    tests reads as 0%, which pushes people to write shallow
#                    tests that inflate a per-package figure without testing
#                    anything new.
#
#   exclusions       generated protobuf code is not ours to test, and cmd/ is
#                    thin wiring around dependencies that are tested where they
#                    live. Including either measures the wrong thing.
set -euo pipefail

THRESHOLD="${COVERAGE_THRESHOLD:-80}"
cd "$(dirname "$0")/../dataplane"

go test -coverpkg=./... -coverprofile=coverage.raw ./... >/dev/null

# Keep the mode: header, drop excluded files.
head -1 coverage.raw >coverage.out
grep -v -e '/cmd/' -e '\.pb\.go' coverage.raw | tail -n +2 >>coverage.out || true
rm -f coverage.raw

go tool cover -func=coverage.out | sed 's|github.com/umerjavaidkh/model-gateway/dataplane/||'
total=$(go tool cover -func=coverage.out | awk '/^total:/ {print $3}' | tr -d '%')

echo
awk -v t="$total" -v min="$THRESHOLD" 'BEGIN {
  if (t + 0 < min + 0) {
    printf "FAIL: coverage %.1f%% is below the %s%% threshold\n", t, min
    exit 1
  }
  printf "OK: coverage %.1f%% (threshold %s%%)\n", t, min
}'
