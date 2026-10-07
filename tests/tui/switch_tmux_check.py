#!/usr/bin/env python3
"""W-switch: verify alt+arrow / alt+s reach the Pyre TUI through a real tmux.

Runs the TUI demo (synthetic mock fleet, in-memory fake terminals) inside a
tmux server on a private socket (-L, -f /dev/null: prefix C-b, no M-arrow
binds in the root table, extended-keys off), attaches a tmux client inside a
pty that plays the outer terminal emulator, writes the raw bytes terminals
send for option/alt+arrows, and reads the TUI's screen with capture-pane.

usage: switch_tmux_check.py DEMO_BINARY SOCKET_NAME [demo flags…]

  go build -o /tmp/pyre-demo ./internal/tui/demo
  python3 tests/tui/switch_tmux_check.py /tmp/pyre-demo pyre-switch-check
  # on a host without this checkout, copy tests/fixtures/{contracts,mock}/*.json
  # into one directory and pass: -fixtures DIR
"""
import re, os, pty, select, subprocess, sys, time, fcntl, termios, struct

demo, sock, demo_args = sys.argv[1], sys.argv[2], sys.argv[3:]
T = ["tmux", "-L", sock, "-f", "/dev/null"]
results = []


def tm(*a, check=True):
    return subprocess.run(T + list(a), capture_output=True, text=True, check=check).stdout


def capture():
    return tm("capture-pane", "-p", "-t", "in")


def rows(screen):
    out = []
    for line in screen.splitlines():
        left = re.split("[│┃]", line)[0]
        if len(left) > 3 and left[2] in "●○!◌◔✗?·⊘" and left[3] == " ":
            # status + title (the harness and age columns drop: age ticks)
            out.append((left.startswith("›"), re.sub(r"\s+\S+\s+\S+\s*$", "", left[2:].strip())))
    return out


def selected(screen):
    for sel, text in rows(screen):
        if sel:
            return text
    return None


def check(name, ok, detail=""):
    results.append((name, ok))
    print(("PASS " if ok else "FAIL ") + name + ("" if ok else "  [selected now: %s]" % selected(capture())))


pid = None
fd = None
sockpath = ""
try:
    print(subprocess.run(["tmux", "-V"], capture_output=True, text=True).stdout.strip())
    tm("new-session", "-d", "-s", "in", "-x", "200", "-y", "55", "env", "TERM=xterm-256color", demo, *demo_args)
    tm("set", "-g", "extended-keys", "off")
    tm("set", "-g", "status", "off")
    sockpath = tm("display", "-p", "#{socket_path}").strip()
    print("root-table M-arrow binds:", [l.split()[3] for l in tm("list-keys", "-T", "root").splitlines() if " M-Up" in l or " M-Down" in l or " M-Left" in l or " M-Right" in l] or "none")
    print("extended-keys:", tm("show", "-gv", "extended-keys").strip())
    pid, fd = pty.fork()
    if pid == 0:
        os.environ["TERM"] = "xterm-256color"
        os.environ.pop("TMUX", None)
        os.execvp("tmux", T + ["attach", "-t", "in"])
    fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", 55, 200, 0, 0))

    def drain(t):
        end = time.time() + t
        while time.time() < end:
            r, _, _ = select.select([fd], [], [], 0.05)
            if r:
                try:
                    os.read(fd, 65536)
                except OSError:
                    return

    def send(b, wait=0.9):
        os.write(fd, b)
        drain(wait)

    drain(4)
    s0 = capture()
    order = [t for _, t in rows(s0)]
    print("list rows:", len(order))
    start = selected(s0)
    check("initial selection visible", start is not None, s0)
    i = [t for t in order].index(start)

    send(b"\x1b[1;3B")  # xterm alt+down
    check("CSI 1;3B (xterm alt+down) -> next session", selected(capture()) == order[i + 1], capture())
    send(b"\x1b[1;9B")  # iTerm2-style meta+down
    check("CSI 1;9B (iTerm2-style) -> next session", selected(capture()) == order[i + 2], capture())
    send(b"\x1b\x1b[A")  # ESC-prefixed up
    check("ESC ESC [ A (ESC-prefixed alt+up) -> previous session", selected(capture()) == order[i + 1], capture())
    send(b"\x1b[1;3D")  # alt+left: back to the previously viewed
    check("CSI 1;3D (alt+left) -> previously viewed session", selected(capture()) == order[i + 2], capture())
    send(b"\x1b[1;3D")
    check("alt+left again toggles back", selected(capture()) == order[i + 1], capture())
    send(b"\x1bb")  # Terminal.app option+left default, list focus: acts as alt+left
    check("ESC b (Terminal.app option+left) in the list -> alt+left", selected(capture()) == order[i + 2], capture())

    send(b"\x1b[1;3C")  # alt+right: next session that needs you
    a1 = selected(capture())
    send(b"\x1b[1;3C")
    a2 = selected(capture())
    check("CSI 1;3C (alt+right) cycles sessions that need you", a1 != a2 and a1[0] in "!?⊘" and a2[0] in "!?⊘", "%s / %s" % (a1, a2))

    # Focus the embedded terminal, type, switch while typing.
    send(b"\r", 1.5)
    send(b"abc", 1.2)
    sc = capture()
    check("enter focuses the terminal and typing reaches it", "typing" in sc and "abc" in sc, sc)
    before = selected(sc)
    j = order.index(before)
    send(b"\x1b[1;3B", 1.5)
    sc = capture()
    check("alt+down while typing switches session", selected(sc) == order[j + 1], sc)
    check("typing focus follows (header says typing)", "typing" in sc and "next key takes the input lease" in sc, sc)
    check("no alt bytes in the new session's input", "[1;3B" not in sc and "abc" not in "".join(re.split("[│┃]", l, maxsplit=1)[-1] for l in sc.splitlines()), sc)
    send(b"z", 1.5)
    sc = capture()
    check("next key reaches the new session", any(">" in re.split("[│┃]", l, maxsplit=1)[-1] and "z" in re.split("[│┃]", l, maxsplit=1)[-1] for l in sc.splitlines()), sc)
    send(b"\x1bb", 1.0)  # inside the terminal ESC b stays the harness's (word back)
    check("ESC b inside the terminal does not switch", selected(capture()) == order[j + 1], capture())
    send(b"\x1bs", 1.2)  # alt+s from inside the terminal
    sc = capture()
    check("alt+s from inside the terminal opens the switcher", "Switch session" in sc, sc)
    send(b"\x1b", 1.5)  # esc closes (after tmux's escape-time)
    sc = capture()
    check("esc closes the switcher, focus back in the terminal", "Switch session" not in sc and "typing" in sc, sc)
    send(b"\x1d", 1.0)  # ctrl+] back to the list

finally:
    subprocess.run(T + ["kill-server"], capture_output=True)
    if sockpath:
        try:
            os.unlink(sockpath)
        except OSError:
            pass
    if pid:
        try:
            os.kill(pid, 9)
            os.waitpid(pid, 0)
        except Exception:
            pass
failed = [n for n, ok in results if not ok]
print("RESULT: %d/%d passed" % (len(results) - len(failed), len(results)))
sys.exit(1 if failed else 0)
