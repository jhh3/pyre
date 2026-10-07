#!/usr/bin/env bash
# macOS parity for server_phase1 + local native attach from a second terminal.
# Isolated sockets only; cleans up everything it starts.
set -u
HERE=$(cd "$(dirname "$0")" && pwd)
export PATH=$HOME/.local/bin:/opt/homebrew/bin:$PATH
D=/tmp/pyre-wp01/claude/m1
rm -rf "$D"; mkdir -p "$D/work"
T="tmux -S $D/tmux.sock"
C="python3 $HERE/ctl.py --dir $D"
O="tmux -L pyre-spike-outer-mac"

# stop the earlier channel experiment session (e3) if still running
tmux -S /tmp/pyre-wp01/claude/e3/tmux.sock kill-server 2>/dev/null

echo "== controller launches, then is SIGKILLed"
bash -c "python3 $HERE/ctl.py --dir $D ensure --extra '--model sonnet --effort low --permission-mode manual' > $D/controller.out 2>&1; exec sleep 3600" &
CTRL=$!
sleep 4
$C accept-dialogs --wait 30
PANE=$(python3 -c "import json;print(json.load(open('$D/run.json'))['pane_pid'])")
SRV=$($T display-message -p '#{pid}')
ps -o pid,ppid,comm -p "$CTRL","$SRV","$PANE"
kill -9 "$CTRL"; sleep 1
ps -p "$PANE" >/dev/null && echo "harness pane $PANE alive after controller SIGKILL" || echo "harness DEAD"
$C ensure | cut -c1-60
SID=$(python3 -c "import json;print(json.load(open('$D/run.json'))['session_id'])")
echo "claude procs for session: $(ps -axo args | grep -c "^$HOME/.local/bin/claude .*--session-id $SID")"

echo "== local native attach from a second terminal"
$O -f /dev/null new-session -d -s outer -x 150 -y 40 "tmux -S $D/tmux.sock attach -t pyre"
sleep 2
$C clients
$C deliver mq-1 "should queue" --timeout 2
$O send-keys -t outer "Typed in attached terminal: reply with the single word mac-human-ok [pyre-msg:mh-1]"
sleep 0.4; $O send-keys -t outer Enter; sleep 7
$C reconcile mh-1 | cut -c1-140
$O send-keys -t outer C-b d; sleep 1
$C clients
$C state
$T kill-server
$O kill-server 2>/dev/null
echo done
