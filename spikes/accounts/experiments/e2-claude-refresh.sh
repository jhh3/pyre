#!/bin/bash
# E2: Claude refresh behaviour with synthetic expired tokens, observed through a local
# TLS-terminating proxy (throwaway CA trusted only by the child via NODE_EXTRA_CA_CERTS).
#  e2a: one process, expired token          -> who refreshes, where, what is written
#  e2b: two processes sharing ONE home      -> does Claude's refresh lock serialise them?
#  e2c: two homes holding a COPY of one grant -> rotation makes the slower copy fail
set -u
export SPIKE_ROOT="${SPIKE_ROOT:-${TMPDIR:-/tmp}/pyre-wp02-e2}"; rm -rf "$SPIKE_ROOT"
source "$(dirname "$0")/../lib.sh"
mk_ca
start_fake "$SPIKE_ROOT/fake.jsonl" --mitm-ca "$MITM_CA" --sleep-refresh "${SLEEP_REFRESH:-2}"
mkdir -p "$SPIKE_ROOT/proj" && cd "$SPIKE_ROOT/proj"
run() { # run HOME TAG
  $TIMEOUT 90 $(claude_env "$1") NODE_EXTRA_CA_CERTS="$MITM_CA" ANTHROPIC_CUSTOM_HEADERS="x-pyre-run: $2" \
    "$CLAUDE_BIN" -p "hi $2" --output-format json </dev/null > "$SPIKE_ROOT/out-$2.json" 2> "$SPIKE_ROOT/err-$2.txt"
  echo "$2 rc=$? result=$(python3 -c 'import json,sys;d=json.load(open(sys.argv[1]));print(d.get("result"))' "$SPIKE_ROOT/out-$2.json" 2>/dev/null)"
}
tok() { python3 -c 'import json,sys;o=json.load(open(sys.argv[1]))["claudeAiOauth"];print(o["accessToken"],o["refreshToken"])' "$1/.credentials.json" 2>/dev/null || echo "(no plaintext creds file)"; }
echo "### e2a single process, expired"
mk_claude_home "$SPIKE_ROOT/A" A expired
run "$SPIKE_ROOT/A" e2a
echo "A creds after: $(tok "$SPIKE_ROOT/A")"
echo "### e2b two processes, one shared home, expired"
mk_claude_home "$SPIKE_ROOT/S" S expired
run "$SPIKE_ROOT/S" e2b-1 & run "$SPIKE_ROOT/S" e2b-2 & wait $(jobs -p | grep -v "^$FAKE_PID$")
echo "S creds after: $(tok "$SPIKE_ROOT/S")"
echo "### e2c two homes with a copy of the same grant, expired"
mk_claude_home "$SPIKE_ROOT/C1" C expired; mkdir -p "$SPIKE_ROOT/C2"; cp -p "$SPIKE_ROOT/C1/.credentials.json" "$SPIKE_ROOT/C1/.claude.json" "$SPIKE_ROOT/C2/"
run "$SPIKE_ROOT/C1" e2c-1 & run "$SPIKE_ROOT/C2" e2c-2 & wait $(jobs -p | grep -v "^$FAKE_PID$")
echo "C1 creds after: $(tok "$SPIKE_ROOT/C1")"; echo "C2 creds after: $(tok "$SPIKE_ROOT/C2")"
stop_fake
echo "### provider log"
python3 - "$SPIKE_ROOT/fake.jsonl" <<'PY'
import json,sys
for l in open(sys.argv[1]):
    r=json.loads(l)
    if r.get("ua")=="node" and r.get("path")=="/": continue  # unrelated local port probe
    print(r.get("via"), r.get("method"), r.get("host"), (r.get("path") or "")[:34], "run="+str(r.get("pid_hint")), "auth="+str(r.get("auth") or r.get("refresh")), r.get("note",""))
PY
echo "### keychain shim"; sed 's/"ts":[0-9]*,//; s/,"ppid":[0-9]*//' "$SPIKE_ROOT/security-shim.jsonl" | sort | uniq -c
echo "### lock files"; find "$SPIKE_ROOT"/{A,S,C1} -maxdepth 1 -name ".*lock*" -o -maxdepth 1 -name "*.lock" 2>/dev/null
grep -h -i "refresh\|oauth\|401\|expired" "$SPIKE_ROOT"/err-*.txt | head -20
