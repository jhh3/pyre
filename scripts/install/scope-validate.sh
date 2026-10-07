#!/bin/bash
# Linux systemd scope validation (S-K; run by scope-validate-remote.sh, last on server): runs in Pyre's tmux server live in a transient scope
# in a test slice and survive `systemctl --user restart` of a transient pyred
# unit; tmux 3.6 extended keys; paste delivery. Fully isolated:
#   - everything under ~/.cache/pyre-sk (hub data, node state, CLI config, work dir)
#   - transient user units only: pyre-test-hub.service, pyre-test-node.service,
#     slice pyre-test-runs.slice, scopes pyre-tmux-*.scope; all removed at exit
#   - never touches the user's tmux server, existing units or installed files
#   - one Claude run with the default login, two tiny turns
set -u
W=$HOME/.cache/pyre-sk
B=$W/bin
PORT=17991
HUB=https://127.0.0.1:$PORT
SLICE=pyre-test-runs.slice
export PYRE_CONFIG=$W/client.json PYRE_STATE_DIR=$W/cli
unset PYRE_HUB PYRE_HUB_PIN PYRE_SOCKET PYRE_TOKEN PYRE_SESSION_TOKEN TMUX
SOCK=$W/state/tmux/pyre.sock
P=$B/pyre
FAILS=0
t0=$(date +%s)
log() { printf '[+%3ss] %s\n' "$(( $(date +%s) - t0 ))" "$*"; }
pass() { log "PASS $*"; }
fail() { log "FAIL $*"; FAILS=$((FAILS + 1)); }
j() { python3 -c "import json,sys; d=json.load(open(sys.argv[1])); print($2)" "$1"; }
section() { echo; echo "===== $* ====="; }

cleanup() {
  section "cleanup"
  for s in ${SESSIONS:-}; do "$P" stop "$s" >/dev/null 2>&1 || true; done
  systemctl --user stop pyre-test-node.service pyre-test-hub.service 2>/dev/null || true
  if [ -S "$SOCK" ]; then tmux -S "$SOCK" kill-server 2>/dev/null || true; fi
  for k in "$W"/keys/*.sock; do [ -S "$k" ] && tmux -S "$k" kill-server 2>/dev/null; done
  sleep 1
  # "-" in slice names is hierarchy: pyre-test-runs.slice implies pyre.slice
  # and pyre-test.slice, which stay loaded (empty) until stopped.
  systemctl --user stop "$SLICE" pyre-test.slice 2>/dev/null || true
  [ -n "$(systemctl --user list-units --no-legend 'pyre-*' 2>/dev/null | grep -v '^pyre.slice')" ] || systemctl --user stop pyre.slice 2>/dev/null || true
  systemctl --user reset-failed pyre-test-node.service pyre-test-hub.service 2>/dev/null || true
  echo "remaining pyre units: $(systemctl --user list-units --all --no-legend 'pyre-test-*' 'pyre-tmux-*' 2>/dev/null | wc -l)"
  find "$W" -mindepth 1 -maxdepth 1 ! -name out -exec rm -rf {} +
  echo "kept: $W/out (logs); removed everything else"
}
trap cleanup EXIT

[ -x "$B/pyred" ] || { echo "missing $B"; exit 2; }
mkdir -p "$W/out" "$W/work" "$W/state" "$W/keys"
chmod 700 "$W" "$W/state"

section "environment"
echo "host: $(uname -srm); $(tmux -V); $(systemd-run --version | head -n 1)"
echo "binaries: $("$B/pyre-hub" version)"
echo "claude: $(claude --version 2>/dev/null | head -n 1)"

section "unit tests (cross-compiled)"
"$B/supervisor.test" -test.run 'Scope|Placement' -test.v 2>&1 | grep -E '^(=== RUN|--- |ok|PASS|FAIL)' | sed 's/^/  /'
"$B/tmux.test" -test.v 2>&1 | grep -E '^(--- |ok|PASS|FAIL|    config_test)' | sed 's/^/  /'
echo "  forced scope outside a service (PYRE_SYSTEMD_SCOPE=on, slice $SLICE):"
PYRE_SYSTEMD_SCOPE=on PYRE_SYSTEMD_SLICE=$SLICE "$B/tmux.test" -test.run 'TestWrittenConfigLoads' -test.v 2>&1 | grep -E '(placement|--- |^ok|FAIL)' | sed 's/^/  /'

section "tmux 3.6 extended keys and paste (raw bytes seen by the application)"
cat >"$W/keys/off.conf" <<'EOF'
set-option -g exit-empty off
set-option -s escape-time 50
EOF
cp "$W/keys/off.conf" "$W/keys/csiu.conf"; cat >>"$W/keys/csiu.conf" <<'EOF'
set-option -s extended-keys on
set-option -s extended-keys-format csi-u
set-option -as terminal-features 'xterm*:extkeys'
EOF
cp "$W/keys/off.conf" "$W/keys/xterm.conf"; cat >>"$W/keys/xterm.conf" <<'EOF'
set-option -s extended-keys on
set-option -s extended-keys-format xterm
set-option -as terminal-features 'xterm*:extkeys'
EOF
printf 'line one\nline two\nline three' >"$W/keys/paste.txt"
for conf in off csiu xterm; do
  for modes in bp bp,mok bp,kitty; do
    k=$W/keys/$conf.sock; o=$W/keys/$conf-$modes.out
    rm -f "$o" "$o.ready"
    tmux -S "$k" -f "$W/keys/$conf.conf" new-session -d -s k -x 120 -y 30 -- python3 "$B/keydump.py" "$o" 4 "$modes"
    for _ in $(seq 1 50); do [ -f "$o.ready" ] && break; sleep 0.1; done
    tmux -S "$k" load-buffer -b pb "$W/keys/paste.txt"
    tmux -S "$k" paste-buffer -p -d -b pb -t =k:
    sleep 0.3
    tmux -S "$k" send-keys -t =k: S-Enter
    sleep 0.2
    tmux -S "$k" send-keys -t =k: Enter
    for _ in $(seq 1 60); do [ -f "$o" ] && break; sleep 0.1; done
    tmux -S "$k" kill-server 2>/dev/null
    printf '  %-6s app requests %-9s -> %s\n' "$conf" "$modes" "$(cat "$o" 2>/dev/null || echo '(no output)')"
  done
done

section "install.sh render into a scratch home + systemd-analyze --user verify (nothing loaded)"
mkdir -p "$W/rel/bin"; cp "$B/pyre" "$B/pyred" "$B/pyre-hub" "$W/rel/bin/"; echo sk-test >"$W/rel/VERSION"
VH=$W/vhome
bash "$W/src/scripts/install/install.sh" --from "$W/rel" --home "$VH" --service node --name sk-render >"$W/out/render-node.txt" 2>&1 && pass "install.sh --service node rendered (scope mode)" || { fail "render node"; cat "$W/out/render-node.txt"; }
bash "$W/src/scripts/install/install.sh" --from "$W/rel" --home "$VH" --service hub --hub-url https://127.0.0.1:7443 >"$W/out/render-hub.txt" 2>&1 && pass "install.sh --service hub rendered" || { fail "render hub"; cat "$W/out/render-hub.txt"; }
grep -E "service-manager, not run" "$W/out/render-node.txt" | sed 's/^/  /'
U=$VH/.config/systemd/user
if systemd-analyze --user verify "$U/pyred.service" "$U/pyre-runs.slice" "$U/pyre-hub.service" >"$W/out/verify.txt" 2>&1; then pass "systemd-analyze --user verify: pyred.service, pyre-runs.slice, pyre-hub.service"; else
  if grep -v -E "^$" "$W/out/verify.txt" | grep -v -E "pyre-hub.*hub.db|Failed to put bus name" | grep -q .; then fail "systemd-analyze verify"; else pass "systemd-analyze --user verify (only expected notes)"; fi
fi
sed 's/^/  verify: /' "$W/out/verify.txt"
cp "$U/pyred.service" "$W/out/rendered-pyred.service"; cp "$U/pyre-runs.slice" "$W/out/rendered-pyre-runs.slice"

section "hub + node as transient user units"
"$B/pyre-hub" init --data "$W/hub" --hub-url "$HUB" --tls-host 127.0.0.1 >/dev/null
systemd-run --user --unit=pyre-test-hub --collect --quiet \
  -p StandardOutput=append:"$W/out/hub.log" -p StandardError=append:"$W/out/hub.log" \
  --setenv=HOME="$HOME" -- "$B/pyre-hub" serve --data "$W/hub" --listen "127.0.0.1:$PORT" --hub-url "$HUB"
for _ in $(seq 1 100); do curl -sk --max-time 1 "$HUB/healthz" | grep -q ok && break; sleep 0.1; done
PIN=$(j "$W/hub/owner-credential.json" "d['hub_cert_sha256']")
j "$W/hub/owner-credential.json" "d['token']" | "$P" login --hub "$HUB" --pin "$PIN" --token - >/dev/null
"$P" enroll-node --name sk-server --json >"$W/t.json" 2>/dev/null
j "$W/t.json" "d['secret']" | "$B/pyred" enroll --hub "$HUB" --pin "$PIN" --node-id "$(j "$W/t.json" "d['node_id']")" --credential-file "$W/state/node-credential.json" --secret-stdin >/dev/null
rm -f "$W/t.json"
start_node() {
  systemd-run --user --unit=pyre-test-node --collect --quiet \
    -p MemoryHigh=512M -p MemoryMax=1G -p CPUQuota=200% -p TasksMax=512 \
    -p StandardOutput=append:"$W/out/node.log" -p StandardError=append:"$W/out/node.log" \
    --working-directory="$W" --setenv=HOME="$HOME" --setenv=PATH="$PATH" \
    --setenv=PYRE_SYSTEMD_SCOPE=require --setenv=PYRE_SYSTEMD_SLICE=$SLICE \
    -- "$B/pyred" run --state "$W/state" --name sk-server --trust-dir "$W/work" --no-integrations
}
start_node
wait_link() {
  for _ in $(seq 1 150); do
    "$B/pyred" status --state "$W/state" --json 2>/dev/null | grep -q '"link":"connected"' && return 0
    sleep 0.2
  done
  return 1
}
if wait_link; then pass "pyred (transient unit pyre-test-node.service) connected"; else fail "pyred did not connect"; tail -n 30 "$W/out/node.log"; exit 1; fi
echo "unit properties: $(systemctl --user show pyre-test-node.service -p KillMode -p MemoryMax -p MemoryHigh -p CPUQuotaPerSecUSec -p TasksMax | tr '\n' ' ')"

section "runs: two shells and one Claude"
SESSIONS=""
start_run() { # harness title -> session id
  "$P" start --node sk-server --harness "$1" --cwd "$W/work" --title "$2" --json >"$W/out/start-$2.json" 2>"$W/out/start-$2.err" || { cat "$W/out/start-$2.err"; return 1; }
  j "$W/out/start-$2.json" "d['session']['session']['session_id']"
}
SH1=$(start_run shell sk-shell-1); SH2=$(start_run shell sk-shell-2); CL=$(start_run claude sk-claude)
SESSIONS="$SH1 $SH2 $CL"
log "sessions: shell $SH1, shell $SH2, claude $CL"
cat "$W/state/tmux/placement" 2>/dev/null | sed 's/^/  placement: /'
grep -q "systemd scope pyre-tmux-.*in $SLICE; server pid [0-9]* cgroup .*/$SLICE/pyre-tmux-[0-9a-z-]*\.scope" "$W/state/tmux/placement" && pass "tmux server placed in a transient scope in $SLICE (placement names the real server cgroup)" || fail "placement"
run_state() { "$P" sessions show "$1" --json 2>/dev/null >"$W/st.json" && j "$W/st.json" "d['run']['state']"; }
run_id() { "$P" sessions show "$1" --json 2>/dev/null >"$W/st.json" && j "$W/st.json" "d['run']['run_id']"; }
sleep 3
# Pyre never types into a shell prompt (shell runs refuse deliveries), so the
# workload is typed into the panes the way an attached human would.
for pair in "1:$SH1" "2:$SH2"; do
  n=${pair%%:*}; sid=${pair#*:}
  r=$(run_id "$sid")
  tmux -S "$SOCK" send-keys -t "=$r:" -l "while :; do date +%s.%N >> $W/work/tick$n; sleep 0.5; done"
  tmux -S "$SOCK" send-keys -t "=$r:" Enter
done
sleep 2
for n in 1 2; do [ -s "$W/work/tick$n" ] && pass "shell $n loop ticking" || fail "shell $n loop not ticking"; done
for _ in $(seq 1 120); do [ "$(run_state "$CL")" = waiting_input ] && break; sleep 1; done
log "claude run state: $(run_state "$CL")"
CLRUN=$(run_id "$CL")

section "Shift-Enter into the Claude pane (tmux send-keys S-Enter)"
tmux -S "$SOCK" send-keys -t "=$CLRUN:" -l "alpha"
tmux -S "$SOCK" send-keys -t "=$CLRUN:" S-Enter
tmux -S "$SOCK" send-keys -t "=$CLRUN:" -l "beta"
sleep 1.5
tmux -S "$SOCK" capture-pane -p -t "=$CLRUN:" >"$W/out/shift-enter.txt"
grep -n -E "alpha|beta" "$W/out/shift-enter.txt" | sed 's/^/  /'
la=$(grep -n alpha "$W/out/shift-enter.txt" | tail -n 1 | cut -d: -f1); lb=$(grep -n beta "$W/out/shift-enter.txt" | tail -n 1 | cut -d: -f1)
if [ -n "$la" ] && [ -n "$lb" ] && [ "$lb" -gt "$la" ] && [ "$(run_state "$CL")" = waiting_input ]; then pass "Shift-Enter inserted a newline in Claude's input (alpha/beta on separate lines, nothing submitted)"; else fail "Shift-Enter (alpha line $la, beta line $lb, state $(run_state "$CL"))"; fi
tmux -S "$SOCK" send-keys -t "=$CLRUN:" BSpace BSpace BSpace BSpace BSpace BSpace BSpace BSpace BSpace BSpace BSpace BSpace
sleep 1

section "multi-line paste delivery to Claude (Pyre bracketed paste, csi-u on)"
"$P" send "$CL" $'Reply with the single word ok.\nsecond line of a multi-line message\nthird line' --json >"$W/out/m1.json"
M1=$(j "$W/out/m1.json" "d['envelope']['message_id']")
for _ in $(seq 1 90); do
  "$P" msg show "$M1" --json >"$W/out/m1-show.json" 2>/dev/null
  st=$(run_state "$CL")
  grep -q '"accepted"' "$W/out/m1-show.json" && { [ "$st" = waiting_input ] || [ "$st" = waiting_approval ]; } && break
  sleep 1
done
log "claude run state after the multi-line message: $(run_state "$CL")"
python3 - "$W/out/m1-show.json" <<'PY'
import json,sys
d=json.load(open(sys.argv[1]))
def find(o,keys):
    if isinstance(o,dict):
        for k,v in o.items():
            if k in keys and isinstance(v,(str,int)): print("  %s: %s" % (k,v))
            find(v,keys)
    elif isinstance(o,list):
        for x in o: find(x,keys)
find(d,{"runtime_state","state","evidence","outcome"})
PY
"$P" read "$CL" --json --tail 30 >"$W/out/read1.json" 2>/dev/null
python3 - "$W/out/read1.json" <<'PY' && pass "Claude's transcript holds the three lines with newlines intact" || fail "multi-line text not intact in the transcript"
import json,sys
d=json.load(open(sys.argv[1]))
txt=json.dumps(d)
ok = "Reply with the single word ok.\\nsecond line of a multi-line message\\nthird line" in txt
print("  transcript contains the exact 3-line text:", ok)
sys.exit(0 if ok else 1)
PY

section "cgroup placement before restart"
SRV=$(tmux -S "$SOCK" display -p '#{pid}')
PANES_BEFORE=$(tmux -S "$SOCK" list-panes -a -F '#{session_name}=#{pane_pid}' | sort | tr '\n' ' ')
PYRED1=$(systemctl --user show pyre-test-node.service -p MainPID --value)
echo "tmux server pid $SRV cgroup: $(cut -d: -f3 /proc/$SRV/cgroup)"
echo "pyred pid $PYRED1 cgroup: $(cut -d: -f3 /proc/$PYRED1/cgroup)"
echo "panes: $PANES_BEFORE"
systemd-cgls --no-pager --user-unit pyre-test-node.service 2>&1 | sed 's/^/  /'
systemd-cgls --no-pager --user-unit $SLICE 2>&1 | sed 's/^/  /' | head -n 40
NODE_CG=$(systemctl --user show pyre-test-node.service -p ControlGroup --value)
echo "pyre-test-node.service cgroup.procs: $(tr '\n' ' ' < /sys/fs/cgroup$NODE_CG/cgroup.procs) memory.current=$(cat /sys/fs/cgroup$NODE_CG/memory.current)"
case "$(cut -d: -f3 /proc/$SRV/cgroup)" in *"/$SLICE/pyre-tmux-"*".scope") pass "tmux server is in $SLICE/pyre-tmux-*.scope, not in pyred's cgroup" ;; *) fail "tmux server cgroup" ;; esac

section "systemctl --user restart pyre-test-node.service"
T1=$(date +%s.%N)
systemctl --user restart pyre-test-node.service
wait_link && pass "pyred restarted and reconnected" || fail "pyred did not come back"
T2=$(date +%s.%N)
PYRED2=$(systemctl --user show pyre-test-node.service -p MainPID --value)
SRV2=$(tmux -S "$SOCK" display -p '#{pid}' 2>/dev/null)
PANES_AFTER=$(tmux -S "$SOCK" list-panes -a -F '#{session_name}=#{pane_pid}' | sort | tr '\n' ' ')
echo "pyred pid $PYRED1 -> $PYRED2; tmux server pid $SRV -> $SRV2"
[ "$PYRED1" != "$PYRED2" ] && pass "pyred main process was replaced" || fail "pyred pid unchanged"
[ "$SRV" = "$SRV2" ] && [ "$PANES_BEFORE" = "$PANES_AFTER" ] && pass "same tmux server and pane processes after restart ($PANES_AFTER)" || fail "panes changed: before $PANES_BEFORE after $PANES_AFTER"
sleep 3
python3 - "$W/work/tick1" "$W/work/tick2" "$T1" "$T2" <<'PY' && pass "shell loops kept ticking through the restart (no gap > 1.5 s)" || fail "tick gap"
import sys
t1, t2 = float(sys.argv[3]), float(sys.argv[4])
ok = True
for f in sys.argv[1:3]:
    ts = [float(x) for x in open(f).read().split()]
    gaps = [b - a for a, b in zip(ts, ts[1:])]
    around = [g for a, g in zip(ts, gaps) if t1 - 2 < a < t2 + 2]
    print("  %s: %d ticks, max gap %.2fs, max gap around restart %.2fs, ticks after restart: %d" % (f.rsplit('/', 1)[1], len(ts), max(gaps), max(around) if around else -1, len([t for t in ts if t > t2])))
    ok = ok and max(gaps) < 1.5 and any(t > t2 for t in ts)
sys.exit(0 if ok else 1)
PY
for s in $SH1 $SH2 $CL; do log "session $s run state after restart: $(run_state "$s")"; done
echo "pyred cgroup.procs after restart: $(tr '\n' ' ' < /sys/fs/cgroup$NODE_CG/cgroup.procs)"
grep -E "recover|Recover" "$W/out/node.log" | tail -n 5 | sed 's/^/  node.log: /'

section "Claude after the restart: answer its pending approval through Pyre, then a second tiny turn"
pending_approval() {
"$P" approvals --json >"$W/out/approvals.json" 2>/dev/null
python3 - "$W/out/approvals.json" "$CL" <<'PY'
import json,sys
d=json.load(open(sys.argv[1])); found=[]
def walk(o):
    if isinstance(o,dict):
        if o.get("approval_id") and o.get("session_id")==sys.argv[2] and o.get("state")=="pending": found.append(o["approval_id"])
        for v in o.values(): walk(v)
    elif isinstance(o,list):
        for v in o: walk(v)
walk(d); print(found[0] if found else "")
PY
}
for round in 1 2 3 4; do
  AP=""
  for _ in $(seq 1 20); do
    [ "$(run_state "$CL")" = waiting_input ] && break
    AP=$(pending_approval); [ -n "$AP" ] && break
    sleep 1
  done
  [ -n "$AP" ] || break
  body=$(python3 -c "import json,sys;d=json.load(open(sys.argv[1]));print([a['body'] for a in d['items'] if a['approval_id']==sys.argv[2]][0][:120])" "$W/out/approvals.json" "$AP")
  log "pending approval $AP: $body"
  if "$P" approve "$AP" --option allow --json >"$W/out/approve-$round.json" 2>&1; then
    pass "approval $round answered through Pyre after the restart (effect $(j "$W/out/approve-$round.json" "d['resolution']['effect']"))"
  else
    fail "approve"; cat "$W/out/approve-$round.json"
  fi
  sleep 2
done
for _ in $(seq 1 60); do [ "$(run_state "$CL")" = waiting_input ] && break; sleep 1; done
log "claude run state after the approvals: $(run_state "$CL")"
"$P" read "$CL" --json --tail 12 >"$W/out/read2.json" 2>/dev/null
python3 -c "import json;d=json.load(open('$W/out/read2.json'));[print('  transcript:', e.get('kind'), '|', str(e.get('text') or '')[:140].replace(chr(10),' / ')) for e in d.get('entries',[])]"
"$P" send "$CL" "Reply with the single word ok." --json >"$W/out/m2.json"
M2=$(j "$W/out/m2.json" "d['envelope']['message_id']")
for _ in $(seq 1 90); do
  "$P" msg show "$M2" --json >"$W/out/m2-show.json" 2>/dev/null
  grep -q '"accepted"' "$W/out/m2-show.json" && break
  sleep 1
done
grep -q '"accepted"' "$W/out/m2-show.json" && pass "message to Claude accepted after the pyred restart (same run $CLRUN)" || fail "delivery after restart"
[ "$(run_id "$CL")" = "$CLRUN" ] && pass "Claude session kept its run id (no relaunch)" || fail "run id changed"

section "stop pyred.service: runs keep living"
systemctl --user stop pyre-test-node.service
sleep 2
n=$(tmux -S "$SOCK" list-panes -a 2>/dev/null | wc -l)
[ "$n" -ge 3 ] && pass "pyred stopped; $n panes still alive in the scope" || fail "panes after stop: $n"
systemctl --user status $SLICE --no-pager 2>&1 | head -n 12 | sed 's/^/  /'
start_node; wait_link && pass "pyred started again" || fail "restart after stop"

section "summary"
log "S-K server validation: $FAILS failure(s)"
cp "$W/out/node.log" "$W/out/node.log.keep" 2>/dev/null
exit "$FAILS"
