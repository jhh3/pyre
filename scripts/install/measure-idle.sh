#!/bin/bash
# Idle resource measurement of pyre-hub + one connected pyred (mock runtime,
# --no-integrations), isolated in a scratch directory on loopback.
#
#   scripts/install/measure-idle.sh [--bin DIR] [--seconds N] [--dir WORKDIR] [--port N]
#
# Reports RSS and CPU% over the window from the CPU-time delta (ps TIME),
# which is exact to ps resolution, rather than ps's decaying %cpu.
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
BIN="" SECS=60 W=/tmp/pyre-ops-measure PORT=17943
while [ $# -gt 0 ]; do
  case "$1" in
    --bin) BIN=${2:?}; shift 2 ;;
    --seconds) SECS=${2:?}; shift 2 ;;
    --dir) W=${2:?}; shift 2 ;;
    --port) PORT=${2:?}; shift 2 ;;
    *) sed -n '2,9p' "$0"; exit 2 ;;
  esac
done
if [ -e "$W" ]; then [ -f "$W/.pyre-rehearsal" ] || { echo "refusing: $W" >&2; exit 1; }; rm -rf "$W"; fi
mkdir -p "$W"; chmod 700 "$W"; touch "$W/.pyre-rehearsal"
if [ -z "$BIN" ]; then make -C "$ROOT" build >/dev/null; BIN=$ROOT/bin; fi
unset PYRE_HUB PYRE_HUB_PIN PYRE_SOCKET PYRE_TOKEN PYRE_SESSION_TOKEN
export PYRE_CONFIG="$W/client.json" PYRE_STATE_DIR="$W/cli"
U=https://127.0.0.1:$PORT
PIDS=""
trap 'for p in $PIDS; do kill "$p" 2>/dev/null || true; done' EXIT
"$BIN/pyre-hub" init --data "$W/hub" --hub-url "$U" --tls-host 127.0.0.1 >/dev/null
"$BIN/pyre-hub" serve --data "$W/hub" --listen "127.0.0.1:$PORT" --hub-url "$U" >"$W/hub.log" 2>&1 &
HP=$!; PIDS="$PIDS $HP"
for _ in $(seq 1 100); do curl -sk --max-time 1 "$U/healthz" | grep -q ok && break; sleep 0.1; done
jget() { python3 -c "import json,sys;print(json.load(open(sys.argv[1]))[sys.argv[2]])" "$1" "$2"; }
PIN=$(jget "$W/hub/owner-credential.json" hub_cert_sha256)
jget "$W/hub/owner-credential.json" token | "$BIN/pyre" login --hub "$U" --pin "$PIN" --token - >/dev/null
"$BIN/pyre" enroll-node --name measure --json >"$W/t.json" 2>/dev/null
mkdir -p "$W/node"
jget "$W/t.json" secret | "$BIN/pyred" enroll --hub "$U" --pin "$PIN" --node-id "$(jget "$W/t.json" node_id)" --credential-file "$W/node/node-credential.json" --secret-stdin >/dev/null
rm -f "$W/t.json"
"$BIN/pyred" run --state "$W/node" --name measure --no-integrations --dev-mock >"$W/node.log" 2>&1 &
NP=$!; PIDS="$PIDS $NP"
sleep 10  # settle: startup work is excluded from the window
OSN=$(uname -s)
secs() { # ps TIME: macOS [H:]M:SS.cc, Linux [[D-]HH:]MM:SS
  ps -o time= -p "$1" | awk -F'[:.]' -v os="$OSN" '{
    if (os == "Darwin") { if (NF == 3) print $1*60 + $2 + $3/100; else print $1*3600 + $2*60 + $3 + $4/100 }
    else { if (NF == 3) print $1*3600 + $2*60 + $3; else print $1*60 + $2 } }'
}
rss() { ps -o rss= -p "$1" | awk '{printf "%.1f", $1/1024}'; }
h0=$(secs "$HP"); n0=$(secs "$NP"); hr0=$(rss "$HP"); nr0=$(rss "$NP")
sleep "$SECS"
h1=$(secs "$HP"); n1=$(secs "$NP"); hr1=$(rss "$HP"); nr1=$(rss "$NP")
echo "platform: $(uname -sm); $("$BIN/pyre-hub" version); window ${SECS}s after 10s settle; connected, no runs"
awk -v a="$h0" -v b="$h1" -v s="$SECS" -v r0="$hr0" -v r1="$hr1" 'BEGIN { printf "pyre-hub: CPU %.2f%% of one core (%.2fs), RSS %s -> %s MiB\n", (b-a)*100/s, b-a, r0, r1 }'
awk -v a="$n0" -v b="$n1" -v s="$SECS" -v r0="$nr0" -v r1="$nr1" 'BEGIN { printf "pyred:    CPU %.2f%% of one core (%.2fs), RSS %s -> %s MiB\n", (b-a)*100/s, b-a, r0, r1 }'
echo "disk: hub data $(du -sk "$W/hub" | cut -f1) KiB, node state $(du -sk "$W/node" | cut -f1) KiB"
echo "logs: hub $(wc -c <"$W/hub.log" | tr -d ' ') B, node $(wc -c <"$W/node.log" | tr -d ' ') B"
