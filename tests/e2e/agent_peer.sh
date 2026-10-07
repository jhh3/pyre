#!/bin/bash
# Agent-driven peer messaging with REAL agent sessions through Pyre.
# Tiny synthetic prompts. Defaults: Claude worker and coordinator on the
# node's existing default login (--ambient).
#   PYRE_CONFIG=... tests/e2e/agent_peer.sh <worker-node> <coordinator-node> <worker-cwd> <coordinator-cwd>
# Parameters (environment):
#   WORKER_HARNESS / COORD_HARNESS   claude (default) or codex
#   WORKER_FLAGS / COORD_FLAGS       account flags (default --ambient), e.g.
#                                    WORKER_FLAGS="--account acct_..."
#   PYRE_E2E_OUT                     output dir for ids/json
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
P=$ROOT/bin/pyre
WN=$1; CN=$2; WCWD=$3; CCWD=$4
WH=${WORKER_HARNESS:-claude}; CH=${COORD_HARNESS:-claude}
WF=${WORKER_FLAGS:---ambient}; CF=${COORD_FLAGS:---ambient}
OUT=${PYRE_E2E_OUT:-/tmp/pyre-agent-peer}
mkdir -p "$OUT"
jget() { python3 -c "import json;print(json.load(open('$1'))$2)"; }

# shellcheck disable=SC2086 # flags are word lists
"$P" start --node "$WN" --harness "$WH" $WF --cwd "$WCWD" --title peer-worker --json >"$OUT/w.json"
W=$(jget "$OUT/w.json" "['session']['session']['session_id']")
echo "worker $W ($WH) on $WN"
PROMPT="You are a coordinator in a synthetic Pyre test. Use the pyre tools (MCP tools named pyre_* or the pyre CLI) to: (1) send the session $W the message 'Synthetic test: what is 2+3? Reply with just the number using pyre send --reply-to <the message id> (or the send_message tool with reply_to).'; (2) wait for its reply (pyre wait-reply or the inbox tool, up to 180 seconds); (3) then publish a short note titled 'peer test result' containing the answer you received. Keep it brief."
# shellcheck disable=SC2086
"$P" start --node "$CN" --harness "$CH" $CF --cwd "$CCWD" --title peer-coordinator --prompt "$PROMPT" --json >"$OUT/c.json"
C=$(jget "$OUT/c.json" "['session']['session']['session_id']")
echo "coordinator $C ($CH) on $CN"
echo "$W" >"$OUT/worker.id"; echo "$C" >"$OUT/coordinator.id"
