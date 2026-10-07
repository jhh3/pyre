#!/usr/bin/env python3
"""Raw terminal input recorder for the tmux extended-keys test.

keydump.py OUT SECONDS MODES   (MODES: comma list of bp, mok, kitty)
Puts the pane tty in raw mode, requests the given modes the way an
application such as Claude Code does (bp = bracketed paste ?2004h, mok =
xterm modifyOtherKeys level 2 >4;2m, kitty = kitty keyboard push >1u),
creates OUT.ready, records every input byte for SECONDS, writes repr(bytes)
to OUT.
"""
import os, select, sys, termios, time, tty

out, secs, modes = sys.argv[1], float(sys.argv[2]), sys.argv[3].split(",")
fd = sys.stdin.fileno()
old = termios.tcgetattr(fd)
tty.setraw(fd)
req = ""
if "bp" in modes:
    req += "\x1b[?2004h"
if "mok" in modes:
    req += "\x1b[>4;2m"
if "kitty" in modes:
    req += "\x1b[>1u"
os.write(1, req.encode())
open(out + ".ready", "w").close()
data = b""
end = time.time() + secs
while time.time() < end:
    r, _, _ = select.select([fd], [], [], 0.1)
    if r:
        data += os.read(fd, 4096)
termios.tcsetattr(fd, termios.TCSADRAIN, old)
with open(out, "w") as f:
    f.write(repr(data))
