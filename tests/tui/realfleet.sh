#!/bin/bash
# Real fleet for TUI review: real pyre-hub + pyred on this Mac, a pyred on a
# remote Linux host ($PYRE_RF_HOST, required) that reaches the hub through an ssh reverse
# forward, and a third node that is enrolled and then stopped (offline).
# All state is isolated under $PYRE_RF_DIR (default ~/.cache/pyre-wtuireal,
# the same path on each host); never touches ~/.config/pyre, the default tmux
# server or default logins. Real Claude runs on the remote host use rota
# checkout (accounts registered explicitly with `register-rota`).
#
#   realfleet.sh deploy                 copy built binaries (dist/{darwin,linux}) into place
#   realfleet.sh up | down | cleanup    start / stop (state kept) / stop + kill isolated tmux servers
#   realfleet.sh env                    print the PYRE_CONFIG export for the CLI and TUI
#   realfleet.sh register-rota NAME LABEL     register a rota account on the remote node
#   realfleet.sh remote ARGS...         run realfleet_remote.sh on the remote host
#   realfleet.sh tunnel-up|tunnel-down  the remote node's link to the hub
#   realfleet.sh offline-node           enroll + start + stop a third node (shows offline)
set -uo pipefail
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
R=${PYRE_RF_DIR:-$HOME/.cache/pyre-wtuireal}
RR=${PYRE_RF_REMOTE_DIR:-.cache/pyre-wtuireal}
HOST=${PYRE_RF_HOST:?set PYRE_RF_HOST to the remote Linux host ssh alias}
PORT=${PYRE_RF_PORT:-18650}
HUB=https://127.0.0.1:$PORT
MAC_NODE=${PYRE_RF_MAC_NODE:-studio-mac}
REMOTE_NODE=${PYRE_RF_REMOTE_NODE:-lab-linux}
OFF_NODE=${PYRE_RF_OFF_NODE:-edge-pi}
export PYRE_CONFIG=$R/client.json
P=$R/bin/pyre
SSH="ssh -o BatchMode=yes -o ConnectTimeout=8"
mkdir -p "$R/logs" "$R/pids" "$R/work"

jget() { python3 -c "import json,sys;print(json.load(open('$1'))$2)"; }
pin() { jget "$R/hub/owner-credential.json" "['hub_cert_sha256']"; }
alive() { [ -n "${1:-}" ] && kill -0 "$1" 2>/dev/null; }
pidf() { if [ -f "$R/pids/$1" ]; then cat "$R/pids/$1"; fi; }
remote() { $SSH "$HOST" bash -l "$RR/bin/realfleet_remote.sh" "$@"; }

deploy() {
  mkdir -p "$R/bin"
  for b in pyre pyred pyre-hub; do
    cp "$R/dist/darwin/$b" "$R/bin/.$b.new" && mv -f "$R/bin/.$b.new" "$R/bin/$b"
  done
  $SSH "$HOST" mkdir -p "$RR/bin" "$RR/upload"
  scp -q "$R/dist/linux/pyre" "$R/dist/linux/pyred" "$ROOT/tests/tui/realfleet_remote.sh" "$HOST:$RR/upload/"
  for f in pyre pyred realfleet_remote.sh; do $SSH "$HOST" mv -f "$RR/upload/$f" "$RR/bin/$f"; done
  echo "deployed to mac and $HOST"
}

hub_up() {
  if alive "$(pidf hub)"; then return; fi
  if [ ! -f "$R/hub/owner-credential.json" ]; then
    "$R/bin/pyre-hub" init --data "$R/hub" --hub-url "$HUB" --tls-host 127.0.0.1 >/dev/null
  fi
  "$R/bin/pyre-hub" serve --data "$R/hub" --listen "127.0.0.1:$PORT" --hub-url "$HUB" \
    --log-file "$R/logs/hub.log" >>"$R/logs/hub.stderr" 2>&1 </dev/null &
  echo $! >"$R/pids/hub"
  sleep 1.5
  if [ ! -f "$PYRE_CONFIG" ]; then
    jget "$R/hub/owner-credential.json" "['token']" | "$P" login --hub "$HUB" --pin "$(pin)" --token - --force >/dev/null
  fi
}

tunnel_up() {
  if alive "$(pidf tunnel)"; then return; fi
  $SSH -o ExitOnForwardFailure=yes -N -R "127.0.0.1:$PORT:127.0.0.1:$PORT" "$HOST" >/dev/null 2>&1 &
  echo $! >"$R/pids/tunnel"
  sleep 1.5
}

tunnel_down() {
  if alive "$(pidf tunnel)"; then kill "$(pidf tunnel)"; fi
  rm -f "$R/pids/tunnel"
}

enroll_local() { # dir name
  if [ -f "$1/node-credential.json" ]; then return; fi
  mkdir -p "$1"
  "$P" enroll-node --name "$2" --json >"$R/t.json"
  jget "$R/t.json" "['secret']" | "$R/bin/pyred" enroll --hub "$HUB" --pin "$(pin)" --node-id "$(jget "$R/t.json" "['node_id']")" \
    --credential-file "$1/node-credential.json" --secret-stdin >/dev/null
  rm -f "$R/t.json"
}

mac_node_up() {
  if alive "$(pidf node)"; then return; fi
  enroll_local "$R/node" "$MAC_NODE"
  "$R/bin/pyred" run --state "$R/node" --credential "$R/node/node-credential.json" --name "$MAC_NODE" \
    --owner-credential "$R/hub/owner-credential.json" --no-integrations --trust-dir "$R/work" \
    >>"$R/logs/node.stderr" 2>&1 </dev/null &
  echo $! >"$R/pids/node"
}

remote_node_up() {
  if ! $SSH "$HOST" test -f "$RR/node/node-credential.json"; then
    "$P" enroll-node --name "$REMOTE_NODE" --ssh "$HOST" --json >"$R/t.json"
    jget "$R/t.json" "['secret']" | remote node-enroll "$HUB" "$(pin)" "$(jget "$R/t.json" "['node_id']")"
    rm -f "$R/t.json"
  fi
  remote node-start "$REMOTE_NODE" --rota-checkout --ssh-alias "$HOST" --trust-dir "/home/$USER/$RR/work"
}

offline_node() {
  enroll_local "$R/node-off" "$OFF_NODE"
  "$R/bin/pyred" run --state "$R/node-off" --credential "$R/node-off/node-credential.json" --name "$OFF_NODE" \
    --no-integrations >>"$R/logs/node-off.stderr" 2>&1 </dev/null &
  local p=$!
  sleep 4
  kill "$p"
  echo "$OFF_NODE enrolled, reported and stopped"
}

up() {
  hub_up
  tunnel_up
  remote_node_up
  mac_node_up
  sleep 3
  "$P" nodes
}

down() {
  if alive "$(pidf node)"; then kill "$(pidf node)"; fi
  rm -f "$R/pids/node"
  remote node-stop
  tunnel_down
  if alive "$(pidf hub)"; then kill "$(pidf hub)"; fi
  rm -f "$R/pids/hub"
}

cleanup() {
  down
  local s=$R/node/tmux/pyre.sock
  if [ -S "$s" ]; then /opt/homebrew/bin/tmux -S "$s" kill-server 2>/dev/null; echo "killed mac isolated tmux"; fi
  remote kill-tmux
}

case "${1:-}" in
  deploy) deploy ;;
  up) up ;;
  down) down ;;
  cleanup) cleanup ;;
  env) echo "export PYRE_CONFIG=$PYRE_CONFIG" ;;
  hub-up) hub_up ;;
  hub-stop) if alive "$(pidf hub)"; then kill "$(pidf hub)"; fi; rm -f "$R/pids/hub" ;;
  mac-node-up) mac_node_up ;;
  mac-node-stop) if alive "$(pidf node)"; then kill "$(pidf node)"; fi; rm -f "$R/pids/node" ;;
  remote-node-up) remote_node_up ;;
  tunnel-up) tunnel_up ;;
  tunnel-down) tunnel_down ;;
  offline-node) offline_node ;;
  register-rota) "$P" accounts register --node "$REMOTE_NODE" --provider anthropic --rota "$2" --label "$3" ;;
  remote) shift; remote "$@" ;;
  *) echo "usage: $0 deploy|up|down|cleanup|env|register-rota NAME LABEL|remote ARGS|tunnel-up|tunnel-down|offline-node"; exit 2 ;;
esac
