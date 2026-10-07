#!/usr/bin/env python3
"""Drive `pyre attach <session>` in a pseudo-terminal: wait for the native
screen (Claude TUI, or the Codex TUI on the Pyre app-server), optionally
type a line, detach with Ctrl-b d, and report.

  PYRE_CONFIG=... [ATTACH_PRE_KEYS='\x1b'] attach_probe.py <session-id> [text-to-type]
"""
import os, pty, re, select, subprocess, sys, time

PYRE = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "..", "bin", "pyre")
sess = sys.argv[1]
text = sys.argv[2] if len(sys.argv) > 2 else ""

pid, fd = pty.fork()
if pid == 0:
    os.environ["TERM"] = "xterm-256color"
    os.execv(PYRE, [PYRE, "attach", sess])

buf = b""


def pump(seconds):
    global buf
    end = time.time() + seconds
    while time.time() < end:
        r, _, _ = select.select([fd], [], [], 0.2)
        if r:
            try:
                data = os.read(fd, 65536)
            except OSError:
                return
            if not data:
                return
            buf += data


pump(6)
screen = re.sub(rb"\x1b\[[0-9;?]*[A-Za-z]", b"", buf).decode("utf-8", "replace")
print("attached; native screen contains Claude UI:", ("Claude Code" in screen) or ("❯" in screen))
print("attached; native screen contains Codex UI:", ("OpenAI Codex" in screen) or ("codex" in screen.lower() and "›" in screen))
# ATTACH_PRE_KEYS: keys sent before typing (Python escapes, e.g. "\\x1b" to
# close a startup dialog in the native TUI).
pre = os.environ.get("ATTACH_PRE_KEYS", "")
if pre:
    os.write(fd, pre.encode().decode("unicode_escape").encode())
    pump(2)
if text:
    os.write(fd, text.encode())
    pump(1)
    os.write(fd, b"\r")
    pump(8)
os.write(fd, b"\x02d")  # Ctrl-b d (tmux detach)
pump(4)
try:
    wpid, status = os.waitpid(pid, os.WNOHANG)
except ChildProcessError:
    wpid, status = pid, 0
print("attach process exited after detach:", wpid == pid, "status:", status)
tail = re.sub(rb"\x1b\[[0-9;?]*[A-Za-z]", b"", buf[-3000:]).decode("utf-8", "replace")
print("--- last screen text (truncated) ---")
print("\n".join(l for l in tail.splitlines() if l.strip())[-1500:])
