#!/bin/bash
# E3: CLAUDE_CODE_OAUTH_TOKEN precedence + E4: cross-account resume + config composition.
set -u
export SPIKE_ROOT="${SPIKE_ROOT:-${TMPDIR:-/tmp}/pyre-wp02-e3}"; rm -rf "$SPIKE_ROOT"
source "$(dirname "$0")/../lib.sh"
SHOW="python3 $SPIKE_DIR/showlog.py"
mk_ca
start_fake "$SPIKE_ROOT/fake.jsonl" --mitm-ca "$MITM_CA"
mkdir -p "$SPIKE_ROOT/proj" && cd "$SPIKE_ROOT/proj"
res() { python3 -c 'import json,sys;d=json.load(open(sys.argv[1]));print(d.get("result"), d.get("session_id"))' "$1" 2>/dev/null || head -c 300 "$1"; }

echo "### e3a env token, profile dir also holds stored creds for B"
mk_claude_home "$SPIKE_ROOT/B" B
$TIMEOUT 60 $(claude_env "$SPIKE_ROOT/B") NODE_EXTRA_CA_CERTS="$MITM_CA" CLAUDE_CODE_OAUTH_TOKEN=pyre-synthetic-E-envtoken \
  ANTHROPIC_CUSTOM_HEADERS="x-pyre-run: e3a" "$CLAUDE_BIN" -p "hi" --output-format json </dev/null > "$SPIKE_ROOT/e3a.json" 2>&1
echo "e3a: $(res "$SPIKE_ROOT/e3a.json")"
echo "auth status with env token:"; $(claude_env "$SPIKE_ROOT/B") CLAUDE_CODE_OAUTH_TOKEN=pyre-synthetic-E-envtoken "$CLAUDE_BIN" auth status --json </dev/null 2>&1

echo "### e3b env token, empty profile dir (no stored creds, no .claude.json)"
mkdir -p "$SPIKE_ROOT/E"
$TIMEOUT 60 $(claude_env "$SPIKE_ROOT/E") NODE_EXTRA_CA_CERTS="$MITM_CA" CLAUDE_CODE_OAUTH_TOKEN=pyre-synthetic-E-envtoken \
  ANTHROPIC_CUSTOM_HEADERS="x-pyre-run: e3b" "$CLAUDE_BIN" -p "hi" --output-format json </dev/null > "$SPIKE_ROOT/e3b.json" 2>&1
echo "e3b: $(res "$SPIKE_ROOT/e3b.json")"
echo "auth status:"; $(claude_env "$SPIKE_ROOT/E") CLAUDE_CODE_OAUTH_TOKEN=pyre-synthetic-E-envtoken "$CLAUDE_BIN" auth status --json </dev/null 2>&1
echo "files written in E: $(cd "$SPIKE_ROOT/E" && ls -A | tr '\n' ' ')"
[ -f "$SPIKE_ROOT/E/.credentials.json" ] && echo "WARNING: env token persisted to .credentials.json"
stop_fake

echo "### e3c env token rejected with 401 (does Claude try to refresh / fall back to stored creds?)"
start_fake "$SPIKE_ROOT/fake401.jsonl" --mitm-ca "$MITM_CA" --expire-all
$TIMEOUT 60 $(claude_env "$SPIKE_ROOT/B") NODE_EXTRA_CA_CERTS="$MITM_CA" CLAUDE_CODE_OAUTH_TOKEN=pyre-synthetic-E-envtoken \
  ANTHROPIC_CUSTOM_HEADERS="x-pyre-run: e3c" "$CLAUDE_BIN" -p "hi" --output-format json </dev/null > "$SPIKE_ROOT/e3c.json" 2>&1
echo "e3c: $(res "$SPIKE_ROOT/e3c.json")"
echo "B creds after e3c: $(python3 -c 'import json,sys;o=json.load(open(sys.argv[1]))["claudeAiOauth"];print(o["accessToken"])' "$SPIKE_ROOT/B/.credentials.json")"
stop_fake
echo "--- provider log (e3a-c)"; $SHOW "$SPIKE_ROOT/fake.jsonl"; echo "--- 401 run"; $SHOW "$SPIKE_ROOT/fake401.jsonl"

echo "### e4 cross-account resume + composition"
start_fake "$SPIKE_ROOT/fake4.jsonl" --mitm-ca "$MITM_CA"
# A "user config" that Pyre would compose into profiles (synthetic stand-in for ~/.claude)
U="$SPIKE_ROOT/userclaude"; mkdir -p "$U/skills/pyre-marker-skill" "$U/agents"
printf -- '---\nname: pyre-marker-skill\ndescription: PYRE-MARKER-SKILL use when asked about markers\n---\nbody\n' > "$U/skills/pyre-marker-skill/SKILL.md"
printf 'PYRE-MARKER-USER-CLAUDEMD\n' > "$U/CLAUDE.md"
printf '{"env":{"PYRE_SETTINGS_MARKER":"1"}}\n' > "$U/settings.json"
mk_claude_home "$SPIKE_ROOT/A" A
for P in A B; do
  for item in skills agents CLAUDE.md settings.json; do ln -sfn "$U/$item" "$SPIKE_ROOT/$P/$item"; done
done
$TIMEOUT 60 $(claude_env "$SPIKE_ROOT/A") NODE_EXTRA_CA_CERTS="$MITM_CA" ANTHROPIC_CUSTOM_HEADERS="x-pyre-run: e4-create-A" \
  "$CLAUDE_BIN" -p "PYRE-MARKER-HISTORY-1 remember this" --output-format json </dev/null > "$SPIKE_ROOT/e4a.json" 2>&1
SID=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["session_id"])' "$SPIKE_ROOT/e4a.json")
echo "created session $SID under A"
echo "A projects: $(cd "$SPIKE_ROOT/A/projects" && find . -name '*.jsonl' | head)"
echo "-- resume under B without transcript"
$TIMEOUT 60 $(claude_env "$SPIKE_ROOT/B") NODE_EXTRA_CA_CERTS="$MITM_CA" ANTHROPIC_CUSTOM_HEADERS="x-pyre-run: e4-B-missing" \
  "$CLAUDE_BIN" -p --resume "$SID" "continue" --output-format json </dev/null > "$SPIKE_ROOT/e4b0.json" 2>&1
echo "e4b0: $(res "$SPIKE_ROOT/e4b0.json")"
echo "-- import transcript into B (copy of synthetic projects dir), resume under B"
mkdir -p "$SPIKE_ROOT/B/projects"; cp -R "$SPIKE_ROOT/A/projects/." "$SPIKE_ROOT/B/projects/"
$TIMEOUT 60 $(claude_env "$SPIKE_ROOT/B") NODE_EXTRA_CA_CERTS="$MITM_CA" ANTHROPIC_CUSTOM_HEADERS="x-pyre-run: e4-B-resume" \
  "$CLAUDE_BIN" -p --resume "$SID" "PYRE-MARKER-AFTER-SWITCH continue" --output-format json </dev/null > "$SPIKE_ROOT/e4b.json" 2>&1
echo "e4b: $(res "$SPIKE_ROOT/e4b.json")"
echo "B transcript files: $(cd "$SPIKE_ROOT/B/projects" && find . -name '*.jsonl' | head)"
echo "-- shared projects dir via symlink (A and B both point at one history store)"
H="$SPIKE_ROOT/shared-projects"; mv "$SPIKE_ROOT/A/projects" "$H"; ln -s "$H" "$SPIKE_ROOT/A/projects"; rm -rf "$SPIKE_ROOT/B/projects"; ln -s "$H" "$SPIKE_ROOT/B/projects"
$TIMEOUT 60 $(claude_env "$SPIKE_ROOT/A") NODE_EXTRA_CA_CERTS="$MITM_CA" ANTHROPIC_CUSTOM_HEADERS="x-pyre-run: e4-A-again" \
  "$CLAUDE_BIN" -p --resume "$SID" "PYRE-MARKER-BACK-TO-A" --output-format json </dev/null > "$SPIKE_ROOT/e4c.json" 2>&1
echo "e4c: $(res "$SPIKE_ROOT/e4c.json")"
stop_fake
echo "--- provider log (e4)"; $SHOW "$SPIKE_ROOT/fake4.jsonl"
echo "--- keychain shim"; sed 's/"ts":[0-9]*,//; s/,"ppid":[0-9]*//' "$SPIKE_ROOT/security-shim.jsonl" | sort | uniq -c
