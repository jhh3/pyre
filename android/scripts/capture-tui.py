#!/usr/bin/env python3
"""Capture the byte stream a Pyre web terminal relays for a TUI.

Starts PROGRAM inside an isolated tmux server (own socket) with an isolated
HOME / CLAUDE_CONFIG_DIR / CODEX_HOME (so no real credentials or config are
read), attaches a tmux client in a PTY of COLSxROWS exactly like pyred's
relayed attach does, records everything the client writes for SECONDS, then
tears the server down.

  capture-tui.py OUT.bin COLS ROWS SECONDS PROGRAM [ARGS...]
"""
import os
import pty
import select
import shutil
import signal
import struct
import subprocess
import sys
import tempfile
import termios
import time
import fcntl

out_path, cols, rows, seconds = sys.argv[1], int(sys.argv[2]), int(sys.argv[3]), float(sys.argv[4])
program = sys.argv[5:]
work = tempfile.mkdtemp(prefix="pyre-tui-capture.")
home = os.path.join(work, "home")
os.makedirs(home)
sock = os.path.join(work, "tmux.sock")
env = {
    "PATH": os.environ["PATH"],
    "HOME": home,
    "CLAUDE_CONFIG_DIR": os.path.join(home, ".claude-isolated"),
    "CODEX_HOME": os.path.join(home, ".codex-isolated"),
    "TERM": "xterm-256color",
    "COLORTERM": "truecolor",
    "LANG": "en_US.UTF-8",
    "LC_ALL": "en_US.UTF-8",
}
os.makedirs(env["CLAUDE_CONFIG_DIR"])
os.makedirs(env["CODEX_HOME"])
tmux = shutil.which("tmux")
conf = os.path.join(work, "tmux.conf")
with open(conf, "w") as f:
    f.write("set -g status off\nset -g default-terminal tmux-256color\nset -ga terminal-features ',xterm-256color:RGB'\n")
cmd = " ".join("'" + a.replace("'", "'\\''") + "'" for a in program)
subprocess.run([tmux, "-S", sock, "-f", conf, "new-session", "-d", "-x", str(cols), "-y", str(rows), "-c", work, cmd], env=env, check=True)
pid, fd = pty.fork()
if pid == 0:
    os.execve(tmux, [tmux, "-u", "-S", sock, "attach-session"], env)
fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", rows, cols, 0, 0))
data = bytearray()
deadline = time.time() + seconds
while time.time() < deadline:
    r, _, _ = select.select([fd], [], [], 0.2)
    if r:
        try:
            chunk = os.read(fd, 65536)
        except OSError:
            break
        if not chunk:
            break
        data += chunk
subprocess.run([tmux, "-S", sock, "kill-server"], env=env)
try:
    os.kill(pid, signal.SIGKILL)
except ProcessLookupError:
    pass
shutil.rmtree(work, ignore_errors=True)
with open(out_path, "wb") as f:
    f.write(data)
print(f"captured {len(data)} bytes -> {out_path}")
