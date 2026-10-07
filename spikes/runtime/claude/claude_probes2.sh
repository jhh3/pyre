#!/usr/bin/env bash
# Follow-up probes: (a) does a client-supplied uuid on a stream-json user message
# survive into the replay/transcript (usable as an idempotency key)? (b) claude --bg in
# an already-trusted scratch dir (e1/work), touching only the session this script creates.
set -u
export PATH=$HOME/.local/bin:$PATH
D=/tmp/pyre-wp01/claude/probes
W=/tmp/pyre-wp01/claude/e1/work
mkdir -p "$D"
cd "$W" || exit 1

echo "== (a) stream-json with client uuid"
SID=$(python3 -c "import uuid;print(uuid.uuid4())")
CU=$(python3 -c "import uuid;print(uuid.uuid4())")
printf '{"type":"user","uuid":"%s","message":{"role":"user","content":"Reply with the single word ok. [pyre-msg:sj-2]"}}\n' "$CU" \
  | claude -p --input-format stream-json --output-format stream-json --verbose --replay-user-messages \
      --model sonnet --effort low --session-id "$SID" > "$D/stream2.jsonl" 2>&1
echo "client uuid: $CU"
grep -o '"type":"user"[^}]*"uuid":"[^"]*"' "$D/stream2.jsonl" | head -2 | cut -c1-120
python3 - "$SID" "$CU" <<'EOF'
import json, os, sys, glob
sid, cu = sys.argv[1], sys.argv[2]
for p in glob.glob(os.path.expanduser("~/.claude/projects/*/%s.jsonl") % sid):
    for l in open(p):
        r = json.loads(l)
        if r.get("type") == "user" and "sj-2" in l:
            print("transcript user uuid:", r.get("uuid"), "matches client uuid:", r.get("uuid") == cu)
EOF

echo "== (b) claude --bg"
BSID=$(python3 -c "import uuid;print(uuid.uuid4())")
claude --bg --session-id "$BSID" --model sonnet --effort low "Reply with the single word ok. [pyre-msg:bg-1]" > "$D/bg.out" 2>&1
echo "exit=$?"; sed -e 's/[[:space:]]*$//' "$D/bg.out" | grep -v '^$' | head -8
sleep 15
SHORT=$(grep -oiE 'claude (attach|logs|stop) [0-9a-z-]+' "$D/bg.out" | head -1 | awk '{print $3}')
[ -z "$SHORT" ] && SHORT=$(grep -oE '\b[0-9a-f]{8}\b' "$D/bg.out" | head -1)
echo "short id parsed: ${SHORT:-none}"
if [ -n "${SHORT:-}" ]; then
  claude logs "$SHORT" 2>&1 | grep -v '^\s*$' | tail -6 | cut -c1-140
  echo "-- processes for bg session (pid ppid comm)"
  for p in $(pgrep -f -- "$BSID"); do ps -o pid,ppid,comm -p "$p" | tail -1; done
  claude stop "$SHORT" 2>&1 | head -3
  sleep 2
  claude rm "$SHORT" 2>&1 | head -3
  echo "remaining procs for bg session: $(pgrep -fc -- "$BSID")"
fi
