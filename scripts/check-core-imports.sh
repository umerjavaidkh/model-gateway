#!/usr/bin/env bash
#
# internal/core must import nothing outside the standard library.
#
# It is the vocabulary every other package speaks, and it is what makes "every
# component is replaceable through a registry" true rather than aspirational. A
# single import of an HTTP client, a database driver or a logging framework
# turns the domain model into that library's domain model, and the coupling is
# invisible until something needs replacing.
#
# The test: a standard-library import path has no dot in its first segment,
# while every module path does — "crypto/hmac" versus "google.golang.org/...".
# Testing the first segment rather than the whole path matters, because the
# standard library does ship packages with dotted later segments, such as
# crypto/internal/entropy/v1.0.0.
set -euo pipefail

readonly SELF=github.com/umerjavaidkh/model-gateway/dataplane/internal/core

cd "$(dirname "$0")/../dataplane"

external=$(go list -deps ./internal/core |
  awk -F/ -v self="$SELF" '$1 ~ /\./ && $0 != self')

if [ -n "$external" ]; then
  echo "FAIL: internal/core depends on packages outside the standard library:" >&2
  echo "$external" | sed 's/^/  /' >&2
  echo >&2
  echo "See CONTRIBUTING.md. If the domain genuinely needs this, it is not domain." >&2
  exit 1
fi

echo "OK: internal/core imports only the standard library"
