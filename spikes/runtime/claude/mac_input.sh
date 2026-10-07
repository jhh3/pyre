#!/usr/bin/env bash
# Reproduces the Claude input/acceptance experiments (originally run as e1/e2):
#   paste on idle, multi-line paste, paste while busy (native queue), Stop-hook injection,
#   crash after Enter / before Enter + reconcile, native subagent, exit + resume by id,
#   permission dialog + structured approval broker + duplicate/late answers.
# Usage: bash mac_input.sh [scratch-dir]   (runs on macOS or Linux; ~14 tiny turns)
set -u
HERE=$(cd "$(dirname "$0")" && pwd)
export PATH=$HOME/.local/bin:/opt/homebrew/bin:$PATH
D=${1:-/tmp/pyre-wp01/claude/repro}
rm -rf "$D"; mkdir -p "$D/work"
exec > >(tee "$D.log") 2>&1
T="tmux -S $D/tmux.sock"
C="python3 $HERE/ctl.py --dir $D"
MODEL="--model sonnet --effort low --permission-mode manual"
wait_idle() { for _ in $(seq 1 40); do sleep 1.5; $C state | grep -q '"waiting_for_input"' && return 0; done; echo "(timeout waiting for idle)"; }
wait_state() { for _ in $(seq 1 30); do sleep 1; $C state | grep -q "\"$1\"" && return 0; done; echo "(timeout waiting for $1)"; }
show() { echo "-- $*"; }

show "launch"; $C ensure --extra "$MODEL" | cut -c1-120; $C accept-dialogs --wait 30; $C state

show "1 paste on idle"; $C deliver p-1 "Reply with the single word ok." | cut -c1-200; wait_idle
show "2 multi-line paste"; $C deliver p-2 "Line one.
Line two: reply with the single word ok." | cut -c1-120; wait_idle
grep '"p-2' "$D/events.jsonl" >/dev/null; python3 -c "import json;[print(repr(json.loads(l)['prompt'])) for l in open('$D/events.jsonl') if 'p-2]' in l and 'UserPromptSubmit' in l]"

show "3 paste while busy (expect native queue-operation enqueue + early UserPromptSubmit)"
$C deliver p-3 "Write the integers from 1 to 150 separated by spaces, no other text." | cut -c1-60
$C deliver p-4 "Busy probe: reply with the single word second." --timeout 2 | cut -c1-120
$C deliver p-4 "Busy probe: reply with the single word second." --force | cut -c1-260
wait_idle

show "4 Stop-hook injection while busy"
$C deliver p-5 "Write the integers from 1 to 100 separated by spaces, no other text." | cut -c1-60
$C deliver p-6 "Stop-hook injection probe: reply with the single word injected." --mode stop-inject | cut -c1-260
wait_idle

show "5 crash after Enter, then reconcile"
$C deliver p-7 "Crash-after-enter probe: reply with the single word ok." --crash-after-enter; sleep 4
$C reconcile p-7 | python3 -c "import json,sys;r=json.load(sys.stdin);print(r['verdict'],[j['result'] for j in r['journal']])"
wait_idle
show "6 crash before Enter, reconcile, inspect draft, clear, redeliver"
$C deliver p-8 "Crash-before-enter probe: reply with the single word ok." --crash-before-enter; sleep 1
$C reconcile p-8 | python3 -c "import json,sys;r=json.load(sys.stdin);print(r['verdict'],[j['result'] for j in r['journal']])"
$T capture-pane -p -t pyre | grep -c 'pyre-msg:p-8' | sed 's/^/draft lines with marker: /'
$T send-keys -t pyre C-u; sleep 0.5
$C deliver p-8 "Crash-before-enter probe: reply with the single word ok." | cut -c1-40; wait_idle
$C reconcile p-8 | python3 -c "import json,sys;r=json.load(sys.stdin);print(r['verdict'],[j['result'] for j in r['journal']])"

show "7 native subagent"
$C deliver p-9 "Use your subagent tool (Agent, general-purpose) to have a subagent reply with the single word sub-ok without other tools, then tell me what it said." | cut -c1-40
for _ in $(seq 1 30); do sleep 2; grep -q SubagentStop "$D/events.jsonl" && $C state | grep -q waiting_for_input && break; done
grep -E 'SubagentStart|SubagentStop' "$D/events.jsonl" | python3 -c "import sys,json;[print(json.loads(l)['event'],json.loads(l).get('agent_type')) for l in sys.stdin]"
wait_idle

show "8 approval: TUI dialog, bracketed paste into dialog is ignored"
$C deliver a-1 "Run this exact shell command: touch pyre-approval-1.txt" | cut -c1-40
wait_state waiting_for_approval; $C state
printf '1 synthetic [pyre-msg:x-1]' | $T load-buffer -b x -; $T paste-buffer -p -d -b x -t pyre; sleep 1
$T capture-pane -p -t pyre | grep -E 'Do you want to proceed|1\. Yes' | head -2
show "8b deny via Esc: note no Stop hook fires; transcript gets turn_duration"
$T send-keys -t pyre Escape; sleep 3; $C state; ls "$D/work"

show "9 structured approval broker (PermissionRequest hook waits for Pyre answer)"
touch "$D/approval-broker"
$C deliver a-2 "Run this exact shell command: touch pyre-approval-2.txt" --force | cut -c1-40
for _ in $(seq 1 20); do sleep 1; [ -n "$($C pending)" ] && break; done
RID=$($C pending | tail -1 | python3 -c "import json,sys;print(json.load(sys.stdin)['request_id'])")
$C answer "$RID" allow; wait_idle; $C answer "$RID" allow; ls "$D/work"

show "10 exit and resume by id"
SID=$(python3 -c "import json;print(json.load(open('$D/run.json'))['session_id'])")
$T send-keys -t pyre "/exit"; sleep 0.5; $T send-keys -t pyre Enter; sleep 4; $C state
$T kill-server; mv "$D/run.json" "$D/run1.json"
$C ensure --resume "$SID" --extra "$MODEL" | cut -c1-80; $C accept-dialogs --wait 20
grep SessionStart "$D/events.jsonl" | python3 -c "import sys,json;[print('SessionStart',json.loads(l).get('source'),json.loads(l)['session_id']) for l in sys.stdin]"
$C deliver p-10 "What single word did the subagent reply earlier? Answer with that word only." | cut -c1-40; wait_idle
$T capture-pane -p -t pyre | grep -v '^\s*$' | grep -A2 'p-10' | cut -c1-100
$T kill-server
echo "events: $D/events.jsonl  deliveries: $D/deliveries.jsonl"
