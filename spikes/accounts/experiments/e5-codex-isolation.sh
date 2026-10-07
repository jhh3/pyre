#!/bin/bash
# E5: Codex per-CODEX_HOME isolation, identity signals, refresh, resume — synthetic only.
set -u
export SPIKE_ROOT="${SPIKE_ROOT:-${TMPDIR:-/tmp}/pyre-wp02-e5}"; rm -rf "$SPIKE_ROOT"
source "$(dirname "$0")/../lib.sh"
SHOW="python3 $SPIKE_DIR/showlog.py"
DEFAULTS=("$HOME/.codex/auth.json" "$HOME/.codex/config.toml")
stat_defaults "${DEFAULTS[@]}" > "$SPIKE_ROOT/defaults.before"
start_fake "$SPIKE_ROOT/fake.jsonl" --sleep "${SLEEP:-2}" --sleep-refresh "${SLEEP_REFRESH:-2}"
mkdir -p "$SPIKE_ROOT/proj" && cd "$SPIKE_ROOT/proj"
CFG=($(codex_cfg) -c 'cli_auth_credentials_store="file"' -m gpt-5.5)
cx() { # cx HOME TAG args...
  local h="$1" tag="$2"; shift 2
  $TIMEOUT 60 $(codex_env "$h") RUST_LOG=codex_login=debug,warn "$CODEX_BIN" "${CFG[@]}" "$@" </dev/null > "$SPIKE_ROOT/$tag.out" 2> "$SPIKE_ROOT/$tag.err"
  echo "$tag rc=$? $(grep -o 'pyre-fake-reply label=[A-Za-z]*' "$SPIKE_ROOT/$tag.out" | head -1) $(grep -o '"thread_id":"[^"]*"' "$SPIKE_ROOT/$tag.out" | head -1)"
}
tok() { python3 -c 'import json,sys;t=json.load(open(sys.argv[1]))["tokens"];print("refresh=",t.get("refresh_token"))' "$1/auth.json" 2>/dev/null || echo "(no auth.json)"; }

echo "### e5a concurrent exec, homes A and B"
mk_codex_home "$SPIKE_ROOT/A" A; mk_codex_home "$SPIKE_ROOT/B" B
cx "$SPIKE_ROOT/A" e5a-A exec --skip-git-repo-check --json "PYRE-MARKER-HISTORY-A hello" &
cx "$SPIKE_ROOT/B" e5a-B exec --skip-git-repo-check --json "hello B" &
wait $(jobs -p | grep -v "^$FAKE_PID$")
for L in A B; do echo "login status $L: $($(codex_env "$SPIKE_ROOT/$L") "$CODEX_BIN" "${CFG[@]}" login status 2>&1 </dev/null | tr '\n' ' ')"; done

echo "### e5b app-server account/read + account/rateLimits/read per home"
for L in A B; do
  echo "-- $L"; $(codex_env "$SPIKE_ROOT/$L") python3 "$SPIKE_DIR/appserver_probe.py" "$CODEX_BIN" "${CFG[@]}" 2>&1 | head -c 1500; echo
done

echo "### e5c refresh: expired single home"
mk_codex_home "$SPIKE_ROOT/X" X expired
cx "$SPIKE_ROOT/X" e5c exec --skip-git-repo-check --json "hi"
echo "X after: $(tok "$SPIKE_ROOT/X")"
echo "### e5d refresh: two processes sharing one expired home"
mk_codex_home "$SPIKE_ROOT/S" S expired
cx "$SPIKE_ROOT/S" e5d-1 exec --skip-git-repo-check --json "hi 1" &
cx "$SPIKE_ROOT/S" e5d-2 exec --skip-git-repo-check --json "hi 2" &
wait $(jobs -p | grep -v "^$FAKE_PID$")
echo "S after: $(tok "$SPIKE_ROOT/S")"
echo "### e5e refresh: copied grant in two homes"
mk_codex_home "$SPIKE_ROOT/C1" C expired; mkdir -p "$SPIKE_ROOT/C2"; cp -p "$SPIKE_ROOT/C1/auth.json" "$SPIKE_ROOT/C2/"
cx "$SPIKE_ROOT/C1" e5e-1 exec --skip-git-repo-check --json "hi" &
cx "$SPIKE_ROOT/C2" e5e-2 exec --skip-git-repo-check --json "hi" &
wait $(jobs -p | grep -v "^$FAKE_PID$")
echo "C1 after: $(tok "$SPIKE_ROOT/C1")"; echo "C2 after: $(tok "$SPIKE_ROOT/C2")"
grep -h -i "refresh\|401\|sign in\|login" "$SPIKE_ROOT"/e5e-*.err | head -6

echo "### e5f cross-home resume"
TID=$(grep -o '"thread_id":"[^"]*"' "$SPIKE_ROOT/e5a-A.out" | head -1 | cut -d'"' -f4)
echo "thread from A: $TID"
cx "$SPIKE_ROOT/B" e5f-missing exec --skip-git-repo-check --json resume "$TID" "PYRE-MARKER-AFTER-SWITCH"
tail -c 400 "$SPIKE_ROOT/e5f-missing.err"; echo
echo "A home layout: $(cd "$SPIKE_ROOT/A" && find . -maxdepth 2 | grep -v '^.$' | sort | tr '\n' ' ')"
cp -R "$SPIKE_ROOT/A/sessions" "$SPIKE_ROOT/B/" 2>/dev/null
cx "$SPIKE_ROOT/B" e5f-sessions exec --skip-git-repo-check --json resume "$TID" "PYRE-MARKER-AFTER-SWITCH"
tail -c 300 "$SPIKE_ROOT/e5f-sessions.err"; echo

echo "### e5g CODEX_ACCESS_TOKEN env in a home with stored creds B"
$TIMEOUT 60 $(codex_env "$SPIKE_ROOT/B") CODEX_ACCESS_TOKEN=pyre-synthetic-E-envaccess "$CODEX_BIN" "${CFG[@]}" exec --skip-git-repo-check --json "hi env" </dev/null > "$SPIKE_ROOT/e5g.out" 2> "$SPIKE_ROOT/e5g.err"
echo "e5g rc=$? $(grep -o 'pyre-fake-reply label=[A-Za-z]*' "$SPIKE_ROOT/e5g.out" | head -1)"; tail -c 300 "$SPIKE_ROOT/e5g.err"; echo
stop_fake
stat_defaults "${DEFAULTS[@]}" > "$SPIKE_ROOT/defaults.after"
echo "--- provider log"; $SHOW "$SPIKE_ROOT/fake.jsonl"
echo "--- defaults diff (empty = unchanged)"; diff "$SPIKE_ROOT/defaults.before" "$SPIKE_ROOT/defaults.after"
