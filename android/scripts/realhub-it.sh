#!/bin/bash
# JVM-side integration check of the Android client against a REAL local Pyre
# hub + pyred (tests/e2e/local_stack.sh pattern, isolated scratch state).
# Runtime adapters: the node's shell adapter (isolated tmux socket) and the
# deterministic mock adapter; no device/emulator involved.
#
#   bash android/scripts/realhub-it.sh
# Env: PYRE_E2E_DIR (default $TMPDIR/pyre-android-realhub), PYRE_E2E_PORT (17643).
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ANDROID="$(dirname "$HERE")"
REPO="$(dirname "$ANDROID")"
export PYRE_E2E_DIR="${PYRE_E2E_DIR:-${TMPDIR:-/tmp}/pyre-android-realhub}"
export PYRE_E2E_PORT="${PYRE_E2E_PORT:-17643}"
export PYRE_CONFIG="$PYRE_E2E_DIR/client.json"
export HARNESS=mock
STACK="$REPO/tests/e2e/local_stack.sh"
P="$REPO/bin/pyre"

trap 'bash "$STACK" down || true' EXIT
bash "$STACK" up

mkdir -p "$PYRE_E2E_DIR/work"
HTML="$PYRE_E2E_DIR/work/android-it-preview.html"
echo "<h1>Android IT $(date +%s)</h1><script>document.title='js-ran'</script>" >"$HTML"
"$P" publish "$HTML" --title android-it-html >/dev/null

# The pairing link is a one-time secret: keep it in a 0600 file, never on a
# command line (gradle and the forked test JVM).
URIFILE="$PYRE_E2E_DIR/work/pair-uri"
(umask 077; "$P" pair-client --name android-it --kind android --json 2>/dev/null | python3 -c 'import json,sys;print(json.load(sys.stdin)["uri"])' >"$URIFILE")
[ -s "$URIFILE" ] || { echo "pair-client produced no URI"; exit 1; }

EVIDENCE="$ANDROID/app/build/realhub-it.txt"
"$ANDROID/gradlew" -p "$ANDROID" --console=plain testDebugUnitTest --rerun \
  --tests 'dev.jhh3.pyre.integration.RealHubIntegrationTest' \
  -Ppyre.realhub.urifile="$URIFILE" -Ppyre.realhub.cwd="$PYRE_E2E_DIR/work" \
  -Ppyre.realhub.evidence="$EVIDENCE"
echo "--- evidence ($EVIDENCE) ---"
cat "$EVIDENCE"
