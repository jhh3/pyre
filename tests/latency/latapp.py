#!/usr/bin/env python3
"""Synthetic echo targets for the keystroke-to-echo latency harness.

  latapp.py echo   raw mode; each key redraws one line "> X" (X = the key).
  latapp.py ink    an Ink-like (Claude-Code-like) full-screen app: a box
                   redrawn on every key and by a spinner every 80 ms, the
                   way a busy agent TUI keeps repainting while you type.

Only the echoed key is drawn with ASCII letters after start-up; the
spinner and counters use braille, box drawing and digits, so the harness
can find the echoed letter in the output stream. Synthetic content only.
"""
import os
import sys
import termios
import threading
import time
import tty

mode = sys.argv[1] if len(sys.argv) > 1 else "echo"
fd = sys.stdin.fileno()
old = termios.tcgetattr(fd)
tty.setraw(fd)
lock = threading.Lock()
last = " "
spin = "⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏"
tick = 0


def out(s):
    os.write(1, s.encode())


def frame():
    # Ink rewrites its whole output block: cursor home, then every line.
    rows = []
    rows.append("╭" + "─" * 58 + "╮")
    for i in range(6):
        rows.append("│ " + ("░" * ((tick + i) % 40)).ljust(56) + " │")
    rows.append("│ " + spin[tick % len(spin)] + " " + str(tick).rjust(8) + " ".ljust(45) + " │")
    rows.append("│ > " + last + " " * 53 + " │")
    rows.append("╰" + "─" * 58 + "╯")
    return "\x1b[H" + "".join(r + "\x1b[K\r\n" for r in rows)


def spinner():
    global tick
    while True:
        time.sleep(0.08)
        with lock:
            tick += 1
            out(frame())


try:
    out("\x1b[2J\x1b[H")
    if mode == "ink":
        out(frame())
        out("\x1b[12;1Hlat-ready")
        threading.Thread(target=spinner, daemon=True).start()
    else:
        out("\r\nlat-ready\r\n> ")
    while True:
        b = os.read(fd, 64)
        if not b or b"\x03" in b:
            break
        ch = b.decode(errors="replace")[-1:]
        with lock:
            last = ch if ch.isprintable() else " "
            if mode == "ink":
                out(frame())
            else:
                out("\r\x1b[K> " + last)
finally:
    termios.tcsetattr(fd, termios.TCSADRAIN, old)
