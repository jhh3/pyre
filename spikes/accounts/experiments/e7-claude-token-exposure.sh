#!/bin/bash
# E7: is a per-session credential visible to the agent's own tool subprocesses?
#  e7a CLAUDE_CODE_OAUTH_TOKEN env   e7b CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR=3 (token on fd 3)
#  e7c stored profile credential (CLAUDE_CONFIG_DIR only)
# The fake provider replies with a Bash tool_use that prints only a marker derived from the
# first 14 chars (sanitised) of each variable, never a value.
set -u
export SPIKE_ROOT="${SPIKE_ROOT:-${TMPDIR:-/tmp}/pyre-wp02-e7}"; rm -rf "$SPIKE_ROOT"
source "$(dirname "$0")/../lib.sh"
start_fake "$SPIKE_ROOT/fake.jsonl"
mkdir -p "$SPIKE_ROOT/proj" "$SPIKE_ROOT/E" "$SPIKE_ROOT/F" && cd "$SPIKE_ROOT/proj"
mk_claude_home "$SPIKE_ROOT/A" A
printf 'pyre-synthetic-F-fdtoken' > "$SPIKE_ROOT/fdtoken"; chmod 600 "$SPIKE_ROOT/fdtoken"
TOOLS=(--dangerously-skip-permissions --output-format json)  # synthetic home; command only prints markers
$TIMEOUT 60 $(claude_env "$SPIKE_ROOT/E") CLAUDE_CODE_OAUTH_TOKEN=pyre-synthetic-E-envtoken ANTHROPIC_CUSTOM_HEADERS="x-pyre-run: e7a-env" \
  "$CLAUDE_BIN" -p "PYRE-TOOLTEST" "${TOOLS[@]}" </dev/null > "$SPIKE_ROOT/e7a.json" 2>&1
$TIMEOUT 60 $(claude_env "$SPIKE_ROOT/F") CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR=3 ANTHROPIC_CUSTOM_HEADERS="x-pyre-run: e7b-fd" \
  "$CLAUDE_BIN" -p "PYRE-TOOLTEST" "${TOOLS[@]}" </dev/null 3<"$SPIKE_ROOT/fdtoken" > "$SPIKE_ROOT/e7b.json" 2>&1
$TIMEOUT 60 $(claude_env "$SPIKE_ROOT/A") ANTHROPIC_CUSTOM_HEADERS="x-pyre-run: e7c-stored" \
  "$CLAUDE_BIN" -p "PYRE-TOOLTEST" "${TOOLS[@]}" </dev/null > "$SPIKE_ROOT/e7c.json" 2>&1
stop_fake
for f in e7a e7b e7c; do echo "$f result: $(python3 -c 'import json,sys;d=json.load(open(sys.argv[1]));print(d.get("result"))' "$SPIKE_ROOT/$f.json" 2>/dev/null || head -c 200 "$SPIKE_ROOT/$f.json")"; done
echo "fd-token persisted to profile F? $(ls -A "$SPIKE_ROOT/F" | tr '\n' ' ')"
python3 "$SPIKE_DIR/showlog.py" "$SPIKE_ROOT/fake.jsonl" | grep -v CONNECT
