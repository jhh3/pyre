#!/bin/bash
# A16 mobile protocol check: the Android client (Robolectric/JVM, no device)
# against a REAL pyre-hub that this script swaps underneath it:
#   A protocol 1..1  ->  B protocol 2..2 (incompatible)  ->  C 1..1 again  ->  D 1..2
# Every hub runs on the same data directory and port, like an in-place hub
# upgrade and rollback. The protocol 2 builds use `go build -overlay` on a
# scratch copy of internal/contracts/doc.go (nothing is committed), the way
# tests/e2e/release_update.sh builds its protocol-2 release.
#
#   bash android/scripts/protocol-it.sh
# Env: PYRE_E2E_DIR (default $TMPDIR/pyre-android-protocol), PYRE_E2E_PORT
# (17644), GO (go), PYRE_KEEP=1 keeps the scratch directory.
# Isolated: own hub data, CLI config and pairing files under PYRE_E2E_DIR;
# never touches ~/.config/pyre or real credentials.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ANDROID="$(dirname "$HERE")"
REPO="$(dirname "$ANDROID")"
DIR="${PYRE_E2E_DIR:-${TMPDIR:-/tmp}/pyre-android-protocol}"
PORT="${PYRE_E2E_PORT:-17644}"
GO="${GO:-go}"
HUB="https://127.0.0.1:$PORT"
HS="$DIR/hs"
DATA="$DIR/hub"
export PYRE_CONFIG="$DIR/client.json"
EVIDENCE="$ANDROID/app/build/protocol-it.txt"

rm -rf "$DIR"
mkdir -p "$DIR/overlay" "$HS"
chmod 700 "$DIR"

HUBPID="" GPID=""
cleanup() {
  [ -n "$HUBPID" ] && kill "$HUBPID" 2>/dev/null && wait "$HUBPID" 2>/dev/null
  [ -n "$GPID" ] && kill "$GPID" 2>/dev/null
  if [ "${PYRE_KEEP:-0}" != 1 ]; then rm -rf "$DIR"; fi
}
trap cleanup EXIT

# ---- builds: p1 (1..1), p2 (2..2), p12 (1..2) ----
DOC="$REPO/internal/contracts/doc.go"
grep -q '^const ProtocolVersion = 1$' "$DOC"
grep -q '^const MinProtocolVersion = 1$' "$DOC"
sed -e 's/^const ProtocolVersion = 1$/const ProtocolVersion = 2/' -e 's/^const MinProtocolVersion = 1$/const MinProtocolVersion = 2/' "$DOC" >"$DIR/overlay/doc_p2.go"
sed -e 's/^const ProtocolVersion = 1$/const ProtocolVersion = 2/' "$DOC" >"$DIR/overlay/doc_p12.go"
printf '{"Replace":{"%s":"%s"}}\n' "$DOC" "$DIR/overlay/doc_p2.go" >"$DIR/overlay/p2.json"
printf '{"Replace":{"%s":"%s"}}\n' "$DOC" "$DIR/overlay/doc_p12.go" >"$DIR/overlay/p12.json"
build() { # name overlay-or-empty
  local name=$1 ov=$2 args=()
  [ -n "$ov" ] && args=("-overlay=$ov")
  for b in pyre pyre-hub; do
    CGO_ENABLED=0 "$GO" -C "$REPO" build -trimpath ${args[@]+"${args[@]}"} \
      -ldflags "-X github.com/jhh3/pyre/internal/buildinfo.Version=0.0.0-protoit-$name" \
      -o "$DIR/bin-$name/$b" "./cmd/$b"
  done
  echo "built $name: $("$DIR/bin-$name/pyre-hub" version --json)"
}
build p1 ""
build p2 "$DIR/overlay/p2.json"
build p12 "$DIR/overlay/p12.json"

serve() { # build-name
  if [ -n "$HUBPID" ]; then kill "$HUBPID"; wait "$HUBPID" 2>/dev/null || true; HUBPID=""; fi
  "$DIR/bin-$1/pyre-hub" serve --data "$DATA" --listen "127.0.0.1:$PORT" --hub-url "$HUB" >>"$DIR/hub-$1.log" 2>&1 &
  HUBPID=$!
  for _ in $(seq 1 100); do
    if curl -sk --max-time 1 "$HUB/healthz" >/dev/null 2>&1; then return 0; fi
    sleep 0.1
  done
  echo "hub $1 did not come up"; cat "$DIR/hub-$1.log"; exit 1
}
pairlink() { # build-name file
  (umask 077; "$DIR/bin-$1/pyre" pair-client --name "android-protocol-it-$1" --kind android --json 2>/dev/null |
    python3 -c 'import json,sys;print(json.load(sys.stdin)["uri"])' >"$HS/$2")
  [ -s "$HS/$2" ] || { echo "pair-client ($1) produced no URI"; exit 1; }
}
ready() { # phase build-name
  local v; v=$("$DIR/bin-$2/pyre-hub" version --json)
  echo "hub $2 pid $HUBPID $v" >"$HS/$1.ready"
  echo "== phase $1: hub $2 ($v)"
}
waitdone() { # phase
  for _ in $(seq 1 3000); do
    [ -f "$HS/$1.done" ] && return 0
    kill -0 "$GPID" 2>/dev/null || { echo "test JVM exited before phase $1 finished"; exit 1; }
    sleep 0.2
  done
  echo "timed out waiting for phase $1"; exit 1
}

# ---- hub data, owner CLI (protocol 1) ----
"$DIR/bin-p1/pyre-hub" init --data "$DATA" --hub-url "$HUB" --tls-host 127.0.0.1 >/dev/null
serve p1
python3 -c "import json;print(json.load(open('$DATA/owner-credential.json'))['token'])" |
  "$DIR/bin-p1/pyre" login --hub "$HUB" --pin "$(python3 -c "import json;print(json.load(open('$DATA/owner-credential.json'))['hub_cert_sha256'])")" --token - >/dev/null
pairlink p1 link-a

"$ANDROID/gradlew" -p "$ANDROID" --console=plain testDebugUnitTest --rerun \
  --tests 'dev.jhh3.pyre.integration.ProtocolRefusalIntegrationTest' \
  -Ppyre.realhub.protodir="$HS" -Ppyre.realhub.evidence="$EVIDENCE" >"$DIR/gradle.log" 2>&1 &
GPID=$!

ready A p1
waitdone A
serve p2
pairlink p2 link-b # minted by the protocol 2 CLI on the protocol 2 hub
ready B p2
waitdone B
serve p1
ready C p1
waitdone C
serve p12
ready D p12
waitdone D

set +e
wait "$GPID"
rc=$?
GPID=""
set -e
grep -E "ProtocolRefusalIntegrationTest|tests completed|BUILD" "$DIR/gradle.log" || true
echo "--- hub refusals (protocol 2 hub log) ---"
grep -c '"status":426\|status=426' "$DIR/hub-p2.log" || true
echo "--- evidence ($EVIDENCE) ---"
cat "$EVIDENCE"
exit $rc
