#!/usr/bin/env bash
# Reproduces the Claude channel experiment (originally e3): a stdlib MCP server
# declaring `claude/channel`, loaded with --dangerously-load-development-channels.
# Shows: per-launch consent dialog, idle delivery, busy delivery (queued natively),
# delivery while a permission dialog is open (dialog unaffected), and model trust behaviour.
set -u
HERE=$(cd "$(dirname "$0")" && pwd)
export PATH=$HOME/.local/bin:/opt/homebrew/bin:$PATH
D=${1:-/tmp/pyre-wp01/claude/repro-channel}
rm -rf "$D"; mkdir -p "$D/work"
exec > >(tee "$D.log") 2>&1
T="tmux -S $D/tmux.sock"
C="python3 $HERE/ctl.py --dir $D"
cat > "$D/mcp.json" <<EOF
{"mcpServers": {"pyre": {"command": "python3", "args": ["$HERE/channel_server.py"], "env": {"PYRE_SPIKE_DIR": "$D"}}}}
EOF
wait_idle() { for _ in $(seq 1 40); do sleep 1.5; $C state | grep -q '"waiting_for_input"' && return 0; done; echo "(timeout)"; }
$C ensure --extra "--model sonnet --effort low --permission-mode manual --mcp-config $D/mcp.json --allowedTools mcp__pyre__pyre_ack --dangerously-load-development-channels server:pyre" | cut -c1-80
$C accept-dialogs --wait 40
echo "-- idle channel delivery"; $C deliver c-1 "Channel probe: call pyre_ack with this message_id, then reply with the single word ok." --mode channel --timeout 25 | cut -c1-260; wait_idle
grep '"ack"' "$D/channel.log" | cut -c1-120
echo "-- busy channel delivery"; $C deliver c-2 "Write the integers from 1 to 150 separated by spaces, no other text." | cut -c1-40
$C deliver c-3 "Busy channel probe: reply with the single word chan-busy." --mode channel --timeout 25 | cut -c1-260; wait_idle
echo "-- channel delivery while a permission dialog is open"
$C deliver c-4 "Run this exact shell command: touch pyre-chan-approval.txt" --mode channel --timeout 20 | cut -c1-40
for _ in $(seq 1 20); do sleep 1; $C state | grep -q waiting_for_approval && break; done
$C deliver c-5 "While-dialog channel probe: reply with the single word chan-dialog." --mode channel --timeout 8 | cut -c1-200
$T capture-pane -p -t pyre | grep -E 'Do you want to proceed' | head -1
$T send-keys -t pyre 1; sleep 0.3; $T send-keys -t pyre Enter; wait_idle
python3 -c "
import json
for l in open('$D/events.jsonl'):
    e=json.loads(l); print(e['t'], e['event'], e.get('tool_name',''), (e.get('prompt') or '')[:70].replace(chr(10),' '))"
$T capture-pane -p -t pyre | grep -v '^\s*$' | tail -6 | cut -c1-150
$T kill-server
