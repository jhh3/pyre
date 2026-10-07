#!/bin/bash
# Print sessions, pending approvals, and recent transcript tails.
#   PYRE_CONFIG=... tests/e2e/observe.sh [session-id ...]
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
P=$ROOT/bin/pyre
sleep "${WAIT:-0}"
"$P" sessions
echo "--- approvals"; "$P" approvals
for s in "$@"; do
  echo "--- read $s"; "$P" read "$s" --tail "${TAIL:-12}" 2>&1 | cut -c1-220
done
