#!/bin/bash
# Local end-to-end stack: real pyre-hub + real pyred + pyre CLI on one machine,
# all state in an isolated scratch directory. Uses the deterministic mock
# runtime adapter unless HARNESS is set (e.g. HARNESS=shell once WP07 lands).
#
#   tests/e2e/local_stack.sh up      # start hub + node, enroll, configure CLI
#   tests/e2e/local_stack.sh env     # print env exports for using the CLI
#   tests/e2e/local_stack.sh smoke   # run the smoke scenario
#   tests/e2e/local_stack.sh down    # stop processes (state kept)
#
# Never touches ~/.config/pyre, the default tmux server, or real credentials.
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
E2E=${PYRE_E2E_DIR:-/tmp/pyre-e2e}
PORT=${PYRE_E2E_PORT:-17543}
HUB=https://127.0.0.1:$PORT
BIN=$ROOT/bin
HARNESS=${HARNESS:-mock}
export PYRE_CONFIG=$E2E/client.json

pin() { python3 -c "import json;print(json.load(open('$E2E/hub/owner-credential.json'))['hub_cert_sha256'])"; }
owner_token() { python3 -c "import json;print(json.load(open('$E2E/hub/owner-credential.json'))['token'])"; }

up() {
  mkdir -p "$E2E"
  make -C "$ROOT" build >/dev/null
  if [ ! -f "$E2E/hub/owner-credential.json" ]; then
    "$BIN/pyre-hub" init --data "$E2E/hub" --hub-url "$HUB" --tls-host 127.0.0.1 >/dev/null
  fi
  "$BIN/pyre-hub" serve --data "$E2E/hub" --listen "127.0.0.1:$PORT" --hub-url "$HUB" >"$E2E/hub.log" 2>&1 &
  echo $! >"$E2E/hub.pid"
  sleep 1.5
  if [ ! -f "$PYRE_CONFIG" ]; then
    owner_token | "$BIN/pyre" login --hub "$HUB" --pin "$(pin)" --token - >/dev/null
  fi
  if [ ! -f "$E2E/node/node-credential.json" ]; then
    mkdir -p "$E2E/node"
    "$BIN/pyre" enroll-node --name e2e-mac --json >"$E2E/ticket.json"
    NODE_ID=$(python3 -c "import json;print(json.load(open('$E2E/ticket.json'))['node_id'])")
    python3 -c "import json;print(json.load(open('$E2E/ticket.json'))['secret'])" |
      "$BIN/pyred" enroll --hub "$HUB" --pin "$(pin)" --node-id "$NODE_ID" --credential-file "$E2E/node/node-credential.json" --secret-stdin >/dev/null
    rm -f "$E2E/ticket.json"
  fi
  DEVMOCK=""
  if [ "$HARNESS" = mock ]; then DEVMOCK=--dev-mock; fi
  # Real Claude needs its workspace trusted; trust only the isolated e2e dir.
  mkdir -p "$E2E/work"
  DEVMOCK="$DEVMOCK --trust-dir $E2E/work"
  "$BIN/pyred" run --state "$E2E/node" --credential "$E2E/node/node-credential.json" --owner-credential "$E2E/hub/owner-credential.json" \
    --name e2e-mac $([ "${INTEGRATIONS:-0}" = 1 ] || echo --no-integrations) $DEVMOCK >"$E2E/node.log" 2>&1 &
  echo $! >"$E2E/node.pid"
  sleep 2
  echo "hub $HUB (log $E2E/hub.log), node log $E2E/node.log"
}

down() {
  for p in node hub; do
    if [ -f "$E2E/$p.pid" ]; then kill "$(cat "$E2E/$p.pid")" 2>/dev/null || true; rm -f "$E2E/$p.pid"; fi
  done
}

smoke() {
  P="$BIN/pyre"
  echo "== hello"; "$P" hello
  echo "== nodes"; "$P" nodes
  echo "== start ($HARNESS)"
  "$P" start --node e2e-mac --harness "$HARNESS" --cwd "$E2E" --title e2e-one --json >"$E2E/s1.json"
  S1=$(python3 -c "import json;print(json.load(open('$E2E/s1.json'))['session']['session']['session_id'])")
  "$P" start --node e2e-mac --harness "$HARNESS" --cwd "$E2E" --title e2e-two --json >"$E2E/s2.json"
  S2=$(python3 -c "import json;print(json.load(open('$E2E/s2.json'))['session']['session']['session_id'])")
  sleep 3
  echo "== sessions"; "$P" sessions
  echo "== send owner -> $S2"; "$P" send "$S2" "e2e synthetic hello" --json >"$E2E/m1.json"
  M1=$(python3 -c "import json;print(json.load(open('$E2E/m1.json'))['envelope']['message_id'])")
  sleep 4
  echo "== message status"; "$P" msg show "$M1"
  echo "== project + doc + publish"
  "$P" project new e2e-project --brief "Synthetic e2e brief" >/dev/null
  echo "synthetic artifact $(date +%s)" >"$E2E/artifact.txt"
  "$P" publish "$E2E/artifact.txt" --project e2e-project --title e2e-artifact
  "$P" artifact ls --project e2e-project
  echo "== attention"; "$P" attention || true
  echo "== events tail"; "$P" sessions show "$S1" | head -20
}

case "${1:-}" in
  up) up ;;
  down) down ;;
  smoke) smoke ;;
  env) echo "export PYRE_CONFIG=$PYRE_CONFIG" ;;
  *) echo "usage: $0 up|smoke|down|env"; exit 2 ;;
esac
