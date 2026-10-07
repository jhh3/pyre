#!/bin/bash
# A07 (project session in an isolated git worktree) and A15 (environment
# overlay on a real Claude launch) against the local stack with integrations.
#   INTEGRATIONS=1 tests/e2e/local_stack.sh up ; PYRE_CONFIG=... tests/e2e/worktree_overlay.sh
set -uo pipefail
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
P=$ROOT/bin/pyre
E2E=${PYRE_E2E_DIR:-/tmp/pyre-e2e}
REPO=$E2E/work/synth-repo
rm -rf "$REPO"; mkdir -p "$REPO"
git -C "$REPO" init -q -b main
printf 'synthetic\n' >"$REPO/README.md"
git -C "$REPO" add README.md
git -C "$REPO" -c user.email=synthetic@example.invalid -c user.name=synthetic commit -q -m init
"$P" project new wt-proj --brief "Synthetic worktree project" >/dev/null 2>&1 || true
PID=$("$P" project show wt-proj --json | python3 -c "import json,sys;print(json.load(sys.stdin)['project_id'])")
VER=$("$P" project show wt-proj --json | python3 -c "import json,sys;print(json.load(sys.stdin)['version'])")
"$P" project update wt-proj --repo "synth=$REPO" --json >/dev/null 2>"$E2E/proj-update.err" || cat "$E2E/proj-update.err"
echo "== shell session in an isolated worktree (A07)"
"$P" start --node e2e-mac --harness shell --project wt-proj --cwd "$REPO" --worktree feature-a --title wt-shell --json >"$E2E/wt.json"
S=$(python3 -c "import json;print(json.load(open('$E2E/wt.json'))['session']['session']['session_id'])")
sleep 3
"$P" sessions show "$S" | grep -E "state|cwd|checkout|project"
git -C "$REPO" worktree list
echo "== claude session with integrations: launch args (A15)"
"$P" start --node e2e-mac --harness claude --ambient --cwd "$E2E/work" --title overlay-claude --json >"$E2E/oc.json"
sleep 6
ps -wwax -o command= | grep -E "[c]laude --settings $E2E" | tr ' ' '\n' | grep -E "^--(mcp-config|plugin-dir|settings)" | sed -E "s|$E2E|<e2e>|"
"$P" sessions show "$(python3 -c "import json;print(json.load(open('$E2E/oc.json'))['session']['session']['session_id'])")" | grep -E "state|env|generation"
