#!/bin/bash
# Runs ON a Linux test host (copied there by two_host.sh to <root>/bin, where
# <root> is ~/.cache/pyre-e2e by default). Isolated under <root>; never
# installs services or touches user config/tmux. <root>/node.env, if present,
# is sourced before pyred starts (PYRE_PROFILE_ROOT, PYRE_HIVE_BIN, ...;
# PYRE_E2E_INTEGRATIONS=1 keeps the integrations on).
#   remote_node.sh hub-up <hub_url> <port> <tls_host>
#   remote_node.sh node-up <hub_url> <pin> <node_id> <name> [--dev-mock] [ssh_alias]   (secret on stdin)
#   remote_node.sh down
set -euo pipefail
R=$(cd "$(dirname "$0")/.." && pwd)
BIN=$R/bin
mkdir -p "$R"
case "$1" in
  hub-up)
    HUB=$2; PORT=$3; HOST=$4
    if [ ! -f "$R/hub/owner-credential.json" ]; then
      "$BIN/pyre-hub" init --data "$R/hub" --hub-url "$HUB" --tls-host "$HOST" --tls-host 127.0.0.1 >/dev/null
    fi
    nohup "$BIN/pyre-hub" serve --data "$R/hub" --listen "0.0.0.0:$PORT" --hub-url "$HUB" >"$R/hub.log" 2>&1 &
    echo $! >"$R/hub.pid"
    sleep 1.5
    cat "$R/hub/owner-credential.json"
    ;;
  node-up)
    HUB=$2; PIN=$3; NODE_ID=$4; NAME=$5; MOCK=${6:-}; ALIAS=${7:-}
    mkdir -p "$R/node"
    if [ ! -f "$R/node/node-credential.json" ]; then
      "$BIN/pyred" enroll --hub "$HUB" --pin "$PIN" --node-id "$NODE_ID" --credential-file "$R/node/node-credential.json" --secret-stdin >/dev/null
    fi
    EXTRA=""
    if [ -n "$ALIAS" ]; then EXTRA="--ssh-alias $ALIAS"; fi
    if [ -f "$R/node.env" ]; then set -a; . "$R/node.env"; set +a; fi
    INTEG=--no-integrations
    if [ "${PYRE_E2E_INTEGRATIONS:-0}" = 1 ]; then INTEG=""; fi
    nohup "$BIN/pyred" run --state "$R/node" --credential "$R/node/node-credential.json" --name "$NAME" $INTEG $MOCK $EXTRA >"$R/node.log" 2>&1 &
    echo $! >"$R/node.pid"
    sleep 2
    echo "node up"
    ;;
  down)
    for p in node hub; do
      if [ -f "$R/$p.pid" ]; then kill "$(cat "$R/$p.pid")" 2>/dev/null || true; rm -f "$R/$p.pid"; fi
    done
    echo "remote down"
    ;;
  logs)
    tail -n 20 "$R/hub.log" "$R/node.log" 2>/dev/null || true
    ;;
esac
