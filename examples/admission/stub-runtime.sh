#!/usr/bin/env bash
#
# A stand-in for a container runtime, for the admission check.
#
# It accepts the same command line the sandbox builds and runs the component as
# an ordinary local process. That is emphatically NOT a sandbox — it exists so
# the admission path can be exercised on a machine with no container runtime,
# and the isolation flags themselves are asserted in internal/sandbox's tests,
# which is where they belong: a boundary only checked when Docker happens to be
# installed is one that silently stops being applied.
set -euo pipefail

[ "${1:-}" = "run" ] || exit 0   # "rm -f" and anything else: nothing to do
shift

socket=""
host_dir=""
while [ $# -gt 0 ]; do
  case "$1" in
    --env) case "$2" in COMPONENT_SOCKET=*) socket="${2#COMPONENT_SOCKET=}" ;; esac; shift 2 ;;
    --volume) host_dir="${2%%:*}"; shift 2 ;;
    --name|--memory|--memory-swap|--cpus|--pids-limit|--user|--tmpfs) shift 2 ;;
    --*) shift ;;
    *) shift ;;  # the image, which this runtime has no use for
  esac
done

if [ -z "$socket" ] || [ -z "$host_dir" ]; then
  echo "stub-runtime: the sandbox did not pass a socket and a volume" >&2
  exit 1
fi

# The sandbox mounts the host directory at /run/component; with no mount
# namespace here, the host path is where the socket actually goes.
export COMPONENT_SOCKET="${host_dir}/$(basename "$socket")"
exec python3 "$(dirname "$0")/stub-component.py"
