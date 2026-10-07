#!/bin/bash
# E1: two concurrent Claude sessions bound to synthetic profiles A and B.
# Proves: each request carries only its own profile's credential; keychain service is
# per-CLAUDE_CONFIG_DIR; default login files untouched (mtime/size only).
set -u
export SPIKE_ROOT="${SPIKE_ROOT:-${TMPDIR:-/tmp}/pyre-wp02-e1}"; rm -rf "$SPIKE_ROOT"
source "$(dirname "$0")/../lib.sh"
DEFAULTS=("$HOME/.claude/.credentials.json" "$HOME/.claude.json" "$HOME/.claude/settings.json")
stat_defaults "${DEFAULTS[@]}" > "$SPIKE_ROOT/defaults.before"
mk_claude_home "$SPIKE_ROOT/A" A
mk_claude_home "$SPIKE_ROOT/B" B
start_fake "$SPIKE_ROOT/fake.jsonl" --sleep 3
mkdir -p "$SPIKE_ROOT/proj" && cd "$SPIKE_ROOT/proj"
for L in A B; do
  $TIMEOUT 90 $(claude_env "$SPIKE_ROOT/$L") ANTHROPIC_CUSTOM_HEADERS="x-pyre-run: run-$L" \
    "$CLAUDE_BIN" -p "hello from $L" --output-format json </dev/null \
    > "$SPIKE_ROOT/out$L.json" 2> "$SPIKE_ROOT/err$L.txt" &
done
wait $(jobs -p | grep -v "^$FAKE_PID$")
for L in A B; do
  echo "== auth status $L"; $(claude_env "$SPIKE_ROOT/$L") "$CLAUDE_BIN" auth status --json </dev/null 2>&1 | head -30
  echo "== result $L: $(python3 -c 'import json,sys;d=json.load(open(sys.argv[1]));print(d.get("result"),d.get("session_id"))' "$SPIKE_ROOT/out$L.json")"
done
stop_fake
stat_defaults "${DEFAULTS[@]}" > "$SPIKE_ROOT/defaults.after"
echo "== requests (path, label, x-pyre-run)"
python3 - "$SPIKE_ROOT/fake.jsonl" <<'PY'
import json,sys
for l in open(sys.argv[1]):
    r=json.loads(l)
    print(r.get("via"), r.get("method"), r.get("host"), (r.get("path") or "")[:40], "label="+str(r.get("label")), "run="+str(r.get("pid_hint")), r.get("note",""))
PY
echo "== keychain shim calls"; sort -u -t, -k3,4 "$SPIKE_ROOT/security-shim.jsonl" | sed 's/"ts":[0-9]*,//; s/,"ppid":[0-9]*//' | sort | uniq -c
for L in A B; do printf "sha256(%s)[:8]=" "$SPIKE_ROOT/$L"; printf %s "$SPIKE_ROOT/$L" | shasum -a 256 2>/dev/null | cut -c1-8 || printf %s "$SPIKE_ROOT/$L" | sha256sum | cut -c1-8; done
echo "== defaults diff (empty = unchanged)"; diff "$SPIKE_ROOT/defaults.before" "$SPIKE_ROOT/defaults.after"
