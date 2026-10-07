#!/bin/bash
# Runs the Android client integration test against pyre-mockhub over pinned TLS.
# MOCK BACKEND — results are mock evidence only, never runtime evidence.
#
#   bash android/scripts/mockhub-it.sh            (from anywhere)
# Env: GO (default ~/sdk/go1.27.1/bin/go or go), PORT (default 7788).
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ANDROID="$(dirname "$HERE")"
REPO="$(dirname "$ANDROID")"
PORT="${PORT:-7788}"
GO="${GO:-$HOME/sdk/go1.27.1/bin/go}"
[ -x "$GO" ] || GO=go
WORK="$(mktemp -d "${TMPDIR:-/tmp}/pyre-mockhub-it.XXXXXX")"
LOG="$WORK/mockhub.log"

(cd "$REPO" && "$GO" build -o "$WORK/pyre-mockhub" ./cmd/pyre-mockhub)
(cd "$REPO" && exec "$WORK/pyre-mockhub" --listen "127.0.0.1:$PORT" --token dev-token --tls --quiet) >"$LOG" 2>&1 &
HUB_PID=$!
trap 'kill $HUB_PID 2>/dev/null || true; rm -rf "$WORK"' EXIT

for _ in $(seq 1 50); do
  grep -q "TLS pin" "$LOG" 2>/dev/null && break
  sleep 0.2
done
URL="$(awk '/hub URL/ {print $3}' "$LOG")"
PIN="$(awk '/TLS pin/ {print $3}' "$LOG")"
[ -n "$URL" ] && [ -n "$PIN" ] || { echo "mockhub did not start:"; cat "$LOG"; exit 1; }
echo "mockhub: $URL pin $PIN (MOCK)"

EVIDENCE="$ANDROID/app/build/mockhub-it.txt"
"$ANDROID/gradlew" -p "$ANDROID" --console=plain testDebugUnitTest --rerun \
  --tests 'dev.jhh3.pyre.integration.MockHubIntegrationTest' \
  -Ppyre.mockhub.url="$URL" -Ppyre.mockhub.token=dev-token -Ppyre.mockhub.pin="$PIN" \
  -Ppyre.mockhub.evidence="$EVIDENCE"
echo "--- evidence ($EVIDENCE) ---"
cat "$EVIDENCE"
