#!/bin/bash
# E6: two INTERACTIVE Claude sessions (profiles A, B) in an isolated tmux server (-L socket).
set -u
export SPIKE_ROOT="${SPIKE_ROOT:-${TMPDIR:-/tmp}/pyre-wp02-e6}"; rm -rf "$SPIKE_ROOT"
source "$(dirname "$0")/../lib.sh"
SOCK="pyre-wp02-e6-$$"
start_fake "$SPIKE_ROOT/fake.jsonl"
mkdir -p "$SPIKE_ROOT/proj"
PROJ="$(cd "$SPIKE_ROOT/proj" && pwd -P)"
for L in A B; do
  mk_claude_home "$SPIKE_ROOT/$L" "$L"
  # pre-accept the folder trust dialog for the synthetic project only
  python3 - "$SPIKE_ROOT/$L/.claude.json" "$PROJ" "$SPIKE_ROOT/proj" <<'PY'
import json,sys
p,proj,proj2=sys.argv[1:4]
d=json.load(open(p))
d["projects"]={k:{"hasTrustDialogAccepted":True,"hasCompletedProjectOnboarding":True} for k in {proj,proj2}}
d["theme"]="dark"
json.dump(d,open(p,"w"))
PY
done
tmux -L "$SOCK" -f /dev/null new-session -d -s A -x 160 -y 40 -c "$PROJ" "$(claude_env "$SPIKE_ROOT/A") ANTHROPIC_CUSTOM_HEADERS='x-pyre-run: tmux-A' $CLAUDE_BIN"
tmux -L "$SOCK" new-session -d -s B -x 160 -y 40 -c "$PROJ" "$(claude_env "$SPIKE_ROOT/B") ANTHROPIC_CUSTOM_HEADERS='x-pyre-run: tmux-B' $CLAUDE_BIN"
sleep 8
for L in A B; do tmux -L "$SOCK" send-keys -t "$L" "PYRE-MARKER-INTERACTIVE-$L say hi" Enter; done
sleep 10
for L in A B; do echo "=== pane $L"; tmux -L "$SOCK" capture-pane -p -t "$L" | grep -v '^\s*$' | tail -12; done
tmux -L "$SOCK" kill-server 2>/dev/null
stop_fake
echo "=== provider log"; python3 "$SPIKE_DIR/showlog.py" "$SPIKE_ROOT/fake.jsonl" | grep -v CONNECT
