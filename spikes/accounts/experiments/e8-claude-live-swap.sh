#!/bin/bash
# E8: what a RUNNING Claude session does when its credential store is rewritten underneath it
# (what `rota switch` does to the default login). Synthetic: profile P starts with account A's
# tokens; while the interactive session is alive we rewrite P/.credentials.json with B's tokens.
set -u
export SPIKE_ROOT="${SPIKE_ROOT:-${TMPDIR:-/tmp}/pyre-wp02-e8}"; rm -rf "$SPIKE_ROOT"
source "$(dirname "$0")/../lib.sh"
SOCK="pyre-wp02-e8-$$"
start_fake "$SPIKE_ROOT/fake.jsonl"
mkdir -p "$SPIKE_ROOT/proj"; PROJ="$(cd "$SPIKE_ROOT/proj" && pwd -P)"
mk_claude_home "$SPIKE_ROOT/P" A
mk_claude_home "$SPIKE_ROOT/Bsrc" B
python3 - "$SPIKE_ROOT/P/.claude.json" "$PROJ" <<'PY'
import json,sys
p,proj=sys.argv[1:3]; d=json.load(open(p))
d["projects"]={proj:{"hasTrustDialogAccepted":True,"hasCompletedProjectOnboarding":True}}; d["theme"]="dark"
json.dump(d,open(p,"w"))
PY
tmux -L "$SOCK" -f /dev/null new-session -d -s P -x 160 -y 40 -c "$PROJ" "$(claude_env "$SPIKE_ROOT/P") ANTHROPIC_CUSTOM_HEADERS='x-pyre-run: live' $CLAUDE_BIN"
sleep 8
tmux -L "$SOCK" send-keys -t P "PYRE-MARKER-TURN1" Enter; sleep 6
cp "$SPIKE_ROOT/Bsrc/.credentials.json" "$SPIKE_ROOT/P/.credentials.json"   # the "switch"
echo "swapped credentials at $(date +%T)"
tmux -L "$SOCK" send-keys -t P "PYRE-MARKER-TURN2-immediate" Enter; sleep 35
tmux -L "$SOCK" send-keys -t P "PYRE-MARKER-TURN3-after-35s" Enter; sleep 6
tmux -L "$SOCK" kill-server 2>/dev/null
stop_fake
python3 "$SPIKE_DIR/showlog.py" "$SPIKE_ROOT/fake.jsonl" | grep -v CONNECT
