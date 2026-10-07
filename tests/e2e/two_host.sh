#!/bin/bash
# Two-host pilot: hub + node on a Linux test host ($PYRE_E2E_HOST), second node on
# this Mac. Isolated state; nothing persistent installed; tokens never printed.
#   tests/e2e/two_host.sh up | smoke | down
#
# Parameters (environment; PYRE_E2E_HOST and PYRE_E2E_REMOTE_HOME are required):
#   HARNESS                 mock (default), claude, codex, shell
#   PYRE_E2E_HOST           test host ssh alias (required)
#   PYRE_E2E_REMOTE_PORT    hub port on the test host, forwarded to the Mac (17643)
#   PYRE_E2E_LOCAL_DIR      Mac state dir (${TMPDIR:-/tmp}/pyre-e2e-2h)
#   PYRE_E2E_REMOTE_ROOT    test-host state dir relative to $HOME (.cache/pyre-e2e)
#   PYRE_E2E_REMOTE_HOME    test host $HOME (required, e.g. /home/alice)
#   PYRE_E2E_NAME_PREFIX    node name prefix (e2e -> e2e-linux, e2e-mac)
#   PYRE_E2E_REMOTE_FLAGS   extra pyred flags on the test host (e.g.
#                           "--rota-checkout --accounts-test-mode")
#   PYRE_E2E_REMOTE_ENV     lines written to <remote root>/node.env and sourced
#                           by remote_node.sh before pyred starts (e.g.
#                           PYRE_PROFILE_ROOT, PYRE_HIVE_BIN, PYRE_E2E_INTEGRATIONS=1)
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
HOSTALIAS=${PYRE_E2E_HOST:?set PYRE_E2E_HOST to the test host ssh alias}
HOSTIP=${PYRE_E2E_HOST_IP:-$(ssh -G "$HOSTALIAS" | awk '/^hostname /{print $2}')}
PORT=${PYRE_E2E_REMOTE_PORT:-17643}
# The hub listens on the test host; the Mac reaches it through an ssh local
# forward over the existing ssh route (no firewall/network changes).
HUB=https://127.0.0.1:$PORT
L=${PYRE_E2E_LOCAL_DIR:-${TMPDIR:-/tmp}/pyre-e2e-2h}
RR=${PYRE_E2E_REMOTE_ROOT:-.cache/pyre-e2e}
RHOME=${PYRE_E2E_REMOTE_HOME:?set PYRE_E2E_REMOTE_HOME to the test host \$HOME}
NP=${PYRE_E2E_NAME_PREFIX:-e2e}
HARNESS=${HARNESS:-mock}
export PYRE_CONFIG=$L/client.json
P=$ROOT/bin/pyre
MOCKFLAG=""
if [ "$HARNESS" = mock ]; then MOCKFLAG=--dev-mock; fi
# Real harnesses: trust only isolated work dirs on each node.
REMOTE_WORK=${PYRE_E2E_REMOTE_WORK:-$RHOME/$RR/work}
LOCAL_WORK=$L/work
RFLAGS="$MOCKFLAG --trust-dir $REMOTE_WORK ${PYRE_E2E_REMOTE_FLAGS:-}"
LFLAGS="$MOCKFLAG --trust-dir $LOCAL_WORK"
RNODE="bash -l $RHOME/$RR/bin/remote_node.sh"

jget() { python3 -c "import json,sys;print(json.load(open('$1'))$2)"; }

up() {
  mkdir -p "$L/dist"
  make -C "$ROOT" build >/dev/null
  for b in pyre pyred pyre-hub; do
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 ${GO:-go} -C "$ROOT" build -trimpath -o "$L/dist/$b" "./cmd/$b"
  done
  # Upload beside, then rename: a running binary (e.g. an agent's `pyre mcp`)
  # cannot be overwritten in place on Linux (ETXTBSY), but can be replaced.
  ssh -o BatchMode=yes "$HOSTALIAS" "mkdir -p $RR/bin $RR/upload $REMOTE_WORK"
  scp -q "$L/dist/pyre" "$L/dist/pyred" "$L/dist/pyre-hub" "$ROOT/tests/e2e/remote_node.sh" "$HOSTALIAS:$RR/upload/"
  ssh -o BatchMode=yes "$HOSTALIAS" "cd $RR && for f in pyre pyred pyre-hub remote_node.sh; do mv -f upload/\$f bin/\$f; done"
  printf '%s\n' "${PYRE_E2E_REMOTE_ENV:-}" | ssh -o BatchMode=yes "$HOSTALIAS" "umask 077; cat > $RR/node.env"
  ssh -o BatchMode=yes "$HOSTALIAS" "$RNODE hub-up $HUB $PORT $HOSTIP" >"$L/owner-credential.json"
  ssh -o BatchMode=yes -o ExitOnForwardFailure=yes -N -L "127.0.0.1:$PORT:127.0.0.1:$PORT" "$HOSTALIAS" >/dev/null 2>&1 &
  echo $! >"$L/tunnel.pid"
  sleep 1.5
  chmod 600 "$L/owner-credential.json"
  PIN=$(jget "$L/owner-credential.json" "['hub_cert_sha256']")
  if [ ! -f "$PYRE_CONFIG" ]; then
    jget "$L/owner-credential.json" "['token']" | "$P" login --hub "$HUB" --pin "$PIN" --token - --force >/dev/null
  fi
  # Linux node (on the hub host)
  if ! ssh -o BatchMode=yes "$HOSTALIAS" "test -f $RR/node/node-credential.json"; then
    "$P" enroll-node --name "$NP-linux" --ssh "$HOSTALIAS" --json >"$L/t1.json"
    jget "$L/t1.json" "['secret']" | ssh -o BatchMode=yes "$HOSTALIAS" "$RNODE node-up $HUB $PIN $(jget "$L/t1.json" "['node_id']") $NP-linux '$RFLAGS' $HOSTALIAS"
    rm -f "$L/t1.json"
  else
    ssh -o BatchMode=yes "$HOSTALIAS" "$RNODE node-up $HUB $PIN x $NP-linux '$RFLAGS' $HOSTALIAS" </dev/null
  fi
  # Mac node
  mkdir -p "$L/node" "$LOCAL_WORK"
  if [ ! -f "$L/node/node-credential.json" ]; then
    "$P" enroll-node --name "$NP-mac" --json >"$L/t2.json"
    jget "$L/t2.json" "['secret']" | "$ROOT/bin/pyred" enroll --hub "$HUB" --pin "$PIN" --node-id "$(jget "$L/t2.json" "['node_id']")" --credential-file "$L/node/node-credential.json" --secret-stdin >/dev/null
    rm -f "$L/t2.json"
  fi
  "$ROOT/bin/pyred" run --state "$L/node" --credential "$L/node/node-credential.json" --name "$NP-mac" --no-integrations $LFLAGS >"$L/node.log" 2>&1 &
  echo $! >"$L/node.pid"
  sleep 3
  "$P" nodes
}

smoke() {
  "$P" start --node "$NP-linux" --harness "$HARNESS" --cwd /tmp --title linux-worker --json >"$L/s1.json"
  "$P" start --node "$NP-mac" --harness "$HARNESS" --cwd /tmp --title mac-coordinator --json >"$L/s2.json"
  S1=$(jget "$L/s1.json" "['session']['session']['session_id']"); S2=$(jget "$L/s2.json" "['session']['session']['session_id']")
  sleep 4
  "$P" sessions
  "$P" send "$S1" "two-host synthetic ping" --json >"$L/m1.json"
  sleep 5
  "$P" msg show "$(jget "$L/m1.json" "['envelope']['message_id']")" | grep -E "stored|runtime|receipt"
}

down() {
  for f in node tunnel; do
    if [ -f "$L/$f.pid" ]; then kill "$(cat "$L/$f.pid")" 2>/dev/null || true; rm -f "$L/$f.pid"; fi
  done
  ssh -o BatchMode=yes "$HOSTALIAS" "$RNODE down" || true
}

# add-mac <alias>: enroll another macOS host as a node. It reaches the hub via
# an ssh reverse forward from this machine (chained through the local tunnel).
add_mac() {
  A=$1
  PIN=$(jget "$L/owner-credential.json" "['hub_cert_sha256']")
  ssh -o BatchMode=yes "$A" "mkdir -p $RR/bin"
  scp -q "$ROOT/bin/pyre" "$ROOT/bin/pyred" "$ROOT/tests/e2e/remote_node.sh" "$A:$RR/bin/"
  ssh -o BatchMode=yes -o ExitOnForwardFailure=yes -N -R "127.0.0.1:$PORT:127.0.0.1:$PORT" "$A" >/dev/null 2>&1 &
  echo $! >"$L/rtunnel-$A.pid"
  sleep 1.5
  if ! ssh -o BatchMode=yes "$A" "test -f $RR/node/node-credential.json"; then
    "$P" enroll-node --name "$NP-$A" --ssh "$A" --json >"$L/t3.json"
    jget "$L/t3.json" "['secret']" | ssh -o BatchMode=yes "$A" "bash -l $RR/bin/remote_node.sh node-up $HUB $PIN $(jget "$L/t3.json" "['node_id']") $NP-$A '$MOCKFLAG' $A"
    rm -f "$L/t3.json"
  else
    ssh -o BatchMode=yes "$A" "bash -l $RR/bin/remote_node.sh node-up $HUB $PIN x $NP-$A '$MOCKFLAG' $A" </dev/null
  fi
  sleep 2
  "$P" nodes
}

down_mac() {
  A=$1
  if [ -f "$L/rtunnel-$A.pid" ]; then kill "$(cat "$L/rtunnel-$A.pid")" 2>/dev/null || true; rm -f "$L/rtunnel-$A.pid"; fi
  ssh -o BatchMode=yes "$A" "bash -l $RR/bin/remote_node.sh down" || true
}

case "${1:-}" in
  up) up ;; smoke) smoke ;; down) down ;;
  add-mac) add_mac "$2" ;; down-mac) down_mac "$2" ;;
  *) echo "usage: $0 up|smoke|down|add-mac ALIAS|down-mac ALIAS"; exit 2 ;;
esac
