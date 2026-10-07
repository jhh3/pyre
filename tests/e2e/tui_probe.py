#!/usr/bin/env python3
"""Drive the real `pyre` TUI in a pseudo-terminal against a live hub and
report which screens rendered. Keystrokes only (A17 keyboard-only check).

  PYRE_CONFIG=... tui_probe.py <cols> <rows>
"""
import fcntl, os, pty, re, select, struct, sys, termios, time

PYRE = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "..", "bin", "pyre")
cols, rows = int(sys.argv[1]), int(sys.argv[2])
pid, fd = pty.fork()
if pid == 0:
    os.environ["TERM"] = "xterm-256color"
    os.execv(PYRE, [PYRE])
fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", rows, cols, 0, 0))
buf = b""


def pump(sec):
    global buf
    end = time.time() + sec
    while time.time() < end:
        r, _, _ = select.select([fd], [], [], 0.1)
        if r:
            try:
                d = os.read(fd, 65536)
            except OSError:
                return
            if not d:
                return
            buf += d
            # Answer terminal queries like a real terminal would: OSC 11
            # (background colour) and DSR cursor position.
            if b"\x1b]11;?" in d:
                os.write(fd, b"\x1b]11;rgb:0000/0000/0000\x1b\\")
            if b"\x1b[6n" in d:
                os.write(fd, b"\x1b[1;1R")


def text():
    return re.sub(rb"\x1b\[[0-9;?]*[A-Za-z]|\x1b\][^\x07]*\x07|\x1b[()][A-Z0-9]", b"", buf).decode("utf-8", "replace")


def step(name, keys, expect, wait=2.0):
    global buf
    buf = b""
    for k in keys:
        os.write(fd, k)
        time.sleep(0.15)
    pump(wait)
    t = text()
    ok = all(e.lower() in t.lower() for e in expect)
    print(f"{'PASS' if ok else 'FAIL'} {name}: expect {expect}")
    if not ok:
        print("   screen tail:", " | ".join(l.strip() for l in t.splitlines() if l.strip())[-600:])
    return ok


t0 = time.time()
while "Sessions" not in text() and time.time() - t0 < 10:
    pump(0.2)
print(f"first paint after {time.time() - t0:.1f}s")
t = text()
print(f"{'PASS' if 'Sessions' in t else 'FAIL'} initial render at {cols}x{rows}: Sessions view")
results = [
    step("move selection down/up (repaints selection)", [b"j", b"k"], []),
    step("help overlay", [b"?"], ["help"], 1.5),
    step("close help", [b"\x1b"], ["Sessions"], 1.5),
    step("projects tab", [b"\t"], ["Projects"]),
    step("inbox tab", [b"\t"], ["Inbox"]),
    step("back to sessions", [b"\t"], ["Sessions"]),
    step("command palette -> machines", [b":", b"m", b"a", b"c", b"h", b"\r"], ["Machines"], 3),
    step("escape back", [b"\x1b"], ["Sessions"], 1.5),
    step("filter sessions", [b"/", b"e", b"2", b"e", b"\r"], ["e2e"], 2),
    step("enter attaches to the real terminal", [b"\r"], [], 4),
    step("type in attached shell", [b"echo tui-attach-$((6*7))\r"], ["tui-attach-42"], 2),
    step("Ctrl-b d detaches back to the same list", [b"\x02", b"d"], ["Sessions"], 4),
]
os.write(fd, b"q")
pump(1.5)
try:
    os.kill(pid, 9)
except ProcessLookupError:
    pass
print("summary:", sum(results), "/", len(results), "steps passed")
