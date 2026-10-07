#!/usr/bin/env bash
# Does a long bracketed paste (collapsed by the TUI into "[Pasted text #N]") reach
# UserPromptSubmit/transcript expanded, with the trailing Pyre marker intact? One turn.
set -u
HERE=$(cd "$(dirname "$0")" && pwd)
export PATH=$HOME/.local/bin:/opt/homebrew/bin:$PATH
D=/tmp/pyre-wp01/claude/longpaste
rm -rf "$D"; mkdir -p "$D/work"
T="tmux -S $D/tmux.sock"
C="python3 $HERE/ctl.py --dir $D"
$C ensure --extra "--model sonnet --effort low --permission-mode manual" | cut -c1-60
$C accept-dialogs --wait 30
BODY=$(python3 -c "print('\n'.join('synthetic line %d of a long Pyre message' % i for i in range(1,61)) + '\nReply with the single word ok.')")
python3 - "$D" "$BODY" <<'EOF'
import subprocess, sys
d, body = sys.argv[1], sys.argv[2]
# Paste without Enter first to observe how the input box renders it.
text = body + " [pyre-msg:lp-1]"
subprocess.run(["tmux", "-S", d + "/tmux.sock", "load-buffer", "-b", "lp", "-"], input=text, text=True)
subprocess.run(["tmux", "-S", d + "/tmux.sock", "paste-buffer", "-p", "-d", "-b", "lp", "-t", "pyre"])
EOF
sleep 1
echo "-- input box after paste:"; $T capture-pane -p -t pyre | grep -E 'Pasted|pyre-msg' | head -3 | cut -c1-120
$T send-keys -t pyre Enter; sleep 6
python3 - "$D" <<'EOF'
import json, sys
d = sys.argv[1]
for l in open(d + "/events.jsonl"):
    e = json.loads(l)
    if e["event"] == "UserPromptSubmit":
        p = e.get("prompt", "")
        print("hook prompt: len=%d (hook.py truncates at 300) startswith=%r" % (len(p), p[:40]))
tp = [json.loads(l).get("transcript_path") for l in open(d + "/events.jsonl")][-1]
for l in open(tp):
    r = json.loads(l)
    if r.get("type") == "user" and "lp-1" in l:
        c = r["message"]["content"]
        c = c if isinstance(c, str) else json.dumps(c)
        print("transcript user content: lines=%d has_marker=%s has_line60=%s placeholder=%s" % (
            c.count("\n") + 1, "[pyre-msg:lp-1]" in c, "line 60 of" in c, "Pasted text" in c))
EOF
$T kill-server
