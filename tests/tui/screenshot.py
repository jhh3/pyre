#!/usr/bin/env python3
"""Render real `pyre` TUI screens to PNG for visual review.

Runs a command (default: the TUI demo against the in-process mock) inside a
real terminal emulator — tmux on a private socket (never the user's tmux
server) — replays a key script, and after each step captures the screen
with `tmux capture-pane -e`, renders it as HTML (true colours, bold, faint,
reverse) and has headless Chrome turn it into a PNG.

  python screenshot.py --out DIR [--cols 120 --rows 34] [--theme dark|light]
                       [--colors truecolor|256|16|none] [--emulator tmux|pyte]
                       [--cmd "go run ./internal/tui/demo"] [--scale 2] step=keys ...

Keys use escapes: \\r enter, \\t tab, \\x1b esc, \\x02 ctrl-b, \\x1d ctrl-], space is " ";
↑↓←→ are the arrows and ⌥ sends the next key with alt (⌥s, ⌥↓; tmux emulator only).
A step "name@8=keys" waits 8 s after its keys instead of --settle (live
data); a name starting with "_" sends its keys without capturing.
Against a real hub, pass --cmd "env PYRE_CONFIG=<isolated client.json> pyre"
(see realfleet.sh).
The light theme sets PYRE_THEME=light for the program (tmux does not
answer background-colour queries from a detached client).

--emulator pyte drives the program in a pty and emulates it with pyte
directly; Bubble Tea v2's incremental renderer (LF/BS cursor moves, REP,
ECH) is only partly supported by pyte, so use it for debugging only.
Requires pyte (pip install pyte), tmux and Google Chrome.
"""
import argparse, fcntl, html, math, os, pty, select, shlex, struct, subprocess, termios, time

import pyte

ap = argparse.ArgumentParser()
ap.add_argument("--out", required=True)
ap.add_argument("--cols", type=int, default=120)
ap.add_argument("--rows", type=int, default=34)
ap.add_argument("--theme", default="dark", choices=["dark", "light"])
ap.add_argument("--colors", default="truecolor", choices=["truecolor", "256", "16", "none"],
                help="terminal colour capability advertised to the program (none sets NO_COLOR)")
ap.add_argument("--emulator", default="tmux", choices=["tmux", "pyte"])
ap.add_argument("--cmd", default="go run ./internal/tui/demo")
ap.add_argument("--settle", type=float, default=1.5)
ap.add_argument("--prefix", default="", help="prefix for output file names")
ap.add_argument("--scale", type=float, default=1, help="device pixel ratio for the PNGs (2 = retina-sharp)")
ap.add_argument("steps", nargs="*")
a = ap.parse_args()
os.makedirs(a.out, exist_ok=True)

# Terminal palettes modelled on common modern themes (One Dark / One Light),
# so screenshots show roughly what a typical user sees in 16-colour mode.
DARK = {
    "bg": "#1b1d23", "fg": "#d8dbe2",
    "ansi": {
        "black": "#2e3138", "red": "#e06c75", "green": "#98c379", "brown": "#e5c07b", "yellow": "#e5c07b",
        "blue": "#61afef", "magenta": "#c678dd", "cyan": "#56b6c2", "white": "#d8dbe2",
        "brightblack": "#5c6370", "brightred": "#ff7b86", "brightgreen": "#b5e890", "brightyellow": "#ffd88a",
        "brightbrown": "#ffd88a", "brightblue": "#7cc4ff", "brightmagenta": "#de94f0", "brightcyan": "#73d4e0",
        "brightwhite": "#ffffff",
    },
}
LIGHT = {
    "bg": "#fbfbf8", "fg": "#2a2c33",
    "ansi": {
        "black": "#383a42", "red": "#d1373a", "green": "#3f8f3a", "brown": "#a86b00", "yellow": "#a86b00",
        "blue": "#3165c4", "magenta": "#a626a4", "cyan": "#0f7f8f", "white": "#e5e5e6",
        "brightblack": "#8a8d96", "brightred": "#e45649", "brightgreen": "#50a14f", "brightyellow": "#c18401",
        "brightbrown": "#c18401", "brightblue": "#4078f2", "brightmagenta": "#b94fb7", "brightcyan": "#0184bc",
        "brightwhite": "#ffffff",
    },
}
theme = DARK if a.theme == "dark" else LIGHT
ANSI = theme["ansi"]

# pyte ignores SGR 2 (faint). Map it onto "blink" (never used by the TUI)
# and render blink as faint, so dim text looks dim in screenshots.
_orig_sgr = pyte.Screen.select_graphic_rendition


def _sgr(self, *attrs, **kw):
    out, i = [], 0
    while i < len(attrs):
        v = attrs[i]
        if v in (38, 48, 58) and i + 1 < len(attrs):
            n = 3 if attrs[i + 1] == 5 else 5 if attrs[i + 1] == 2 else 1
            if v != 58:  # underline colour: unsupported, drop
                out.extend(attrs[i:i + n])
            i += n
            continue
        if v == 2:
            out.append(5)
        elif v == 22:
            out.extend([22, 25])
        else:
            out.append(v)
        i += 1
    return _orig_sgr(self, *out, **kw)


pyte.Screen.select_graphic_rendition = _sgr

# For --emulator pyte: REP (CSI n b) and SU/SD (CSI n S / T), which pyte lacks.
_orig_draw = pyte.Screen.draw


def _draw(self, data):
    if data:
        self._last_char = data[-1]
    return _orig_draw(self, data)


def _repeat(self, count=1, *_, **__):
    ch = getattr(self, "_last_char", None)
    if ch:
        _orig_draw(self, ch * max(1, count))


def _scroll(self, count, up):
    top, bottom = (self.margins.top, self.margins.bottom) if self.margins else (0, self.lines - 1)
    y, x = self.cursor.y, self.cursor.x
    self.cursor.y = bottom if up else top
    for _ in range(max(1, count)):
        self.index() if up else self.reverse_index()
    self.cursor.y, self.cursor.x = y, x


pyte.Screen.draw = _draw
pyte.Screen.repeat_last_char = _repeat
pyte.Screen.scroll_up = lambda self, count=1, *_, **__: _scroll(self, count, True)
pyte.Screen.scroll_down = lambda self, count=1, *_, **__: _scroll(self, count, False)
pyte.Stream.csi = dict(pyte.Stream.csi, b="repeat_last_char", S="scroll_up", T="scroll_down")


def child_env():
    env = dict(os.environ)
    env.pop("NO_COLOR", None)
    env.pop("COLORTERM", None)
    env.pop("TMUX", None)
    if a.colors in ("truecolor", "none"):
        env["COLORTERM"] = "truecolor"
    if a.colors == "none":
        env["NO_COLOR"] = "1"
    if a.theme == "light":
        env["PYRE_THEME"] = "light"
    return env


# ---- tmux backend ----

SOCK = f"/tmp/pyre-shots-{os.getpid()}.sock"
TERMS = {"truecolor": "tmux-256color", "256": "tmux-256color", "16": "screen", "none": "tmux-256color"}


def tmux(*args, **kw):
    return subprocess.run(["tmux", "-S", SOCK, "-f", "/dev/null", *args], capture_output=True, text=True, **kw)


def tmux_start():
    env = child_env()
    envargs = []
    for k in ("COLORTERM", "NO_COLOR", "PYRE_THEME"):
        if k in env:
            envargs += ["-e", f"{k}={env[k]}"]
    feats = ["set", "-as", "terminal-features", ",*:RGB", ";"] if a.colors in ("truecolor", "none") else []
    r = tmux("start-server", ";", "set", "-g", "default-terminal", TERMS[a.colors], ";", *feats,
             "set", "-g", "status", "off", ";", "set", "-g", "escape-time", "10", ";",
             "new-session", "-d", "-s", "shots", "-x", str(a.cols), "-y", str(a.rows), *envargs, a.cmd)
    if r.returncode != 0:
        raise SystemExit("tmux failed: " + r.stderr)


KEYNAMES = {"\r": "Enter", "\x1b": "Escape", "\t": "Tab", "\x7f": "BSpace", "\x02": "C-b", "\x1d": "C-]"}


ARROWS = {"↑": "Up", "↓": "Down", "←": "Left", "→": "Right"}


def tmux_send(keys):
    alt = False
    for ch in keys:
        if ch == "⌥":  # ⌥ before a key sends it with alt/meta (⌥s, ⌥↓)
            alt = True
            continue
        if alt:
            tmux("send-keys", "-t", "shots", "M-" + ARROWS.get(ch, ch))
            alt = False
        elif ch in ARROWS:
            tmux("send-keys", "-t", "shots", ARROWS[ch])
        elif ch in KEYNAMES:
            tmux("send-keys", "-t", "shots", KEYNAMES[ch])
        elif ord(ch) < 32:
            tmux("send-keys", "-t", "shots", "C-" + chr(ord(ch) + 96))
        else:
            tmux("send-keys", "-t", "shots", "-l", ch)
        time.sleep(0.06)


def tmux_screen():
    out = tmux("capture-pane", "-p", "-e", "-N", "-t", "shots").stdout
    scr = pyte.Screen(a.cols, a.rows)
    st = pyte.ByteStream(scr)
    lines = out.split("\n")[: a.rows]
    st.feed(("\r\n".join(lines)).encode())
    return scr


# ---- pyte backend ----

pscreen, pstream, fd, pid = None, None, None, None


def pyte_start():
    global pscreen, pstream, fd, pid
    pscreen = pyte.Screen(a.cols, a.rows)
    pstream = pyte.ByteStream(pscreen)
    pid, fd = pty.fork()
    if pid == 0:
        env = child_env()
        env["TERM"] = "xterm" if a.colors == "16" else "xterm-256color"
        os.environ.clear()
        os.environ.update(env)
        argv = shlex.split(a.cmd)
        os.execvp(argv[0], argv)
    fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", a.rows, a.cols, 0, 0))


def osc11_reply():
    h = theme["bg"].lstrip("#")
    r, g, b = h[0:2], h[2:4], h[4:6]
    return f"\x1b]11;rgb:{r}{r}/{g}{g}/{b}{b}\x1b\\".encode()


def pump(sec):
    if a.emulator == "tmux":
        time.sleep(sec)
        return
    end = time.time() + sec
    while time.time() < end:
        r, _, _ = select.select([fd], [], [], 0.05)
        if r:
            try:
                d = os.read(fd, 65536)
            except OSError:
                return
            if not d:
                return
            if b"\x1b]11;?" in d:
                os.write(fd, osc11_reply())
            if b"\x1b[6n" in d:
                os.write(fd, b"\x1b[1;1R")
            pstream.feed(d)


def send(keys):
    if a.emulator == "tmux":
        tmux_send(keys)
        return
    data = keys.encode("utf-8")
    for k in data:
        os.write(fd, bytes([k]))
        time.sleep(0.06)


# ---- rendering ----

def color(c, default):
    if c in ("default", None):
        return default
    if c in ANSI:
        return ANSI[c]
    if len(c) == 6 and all(ch in "0123456789abcdefABCDEF" for ch in c):
        return "#" + c
    return default


def mix(fg, bg, t):
    """Blend fg toward bg by t (faint rendering)."""
    f = [int(fg[i:i + 2], 16) for i in (1, 3, 5)]
    b = [int(bg[i:i + 2], 16) for i in (1, 3, 5)]
    return "#" + "".join(f"{round(x + (y - x) * t):02x}" for x, y in zip(f, b))


CELL_W, FONT = 8.6, 14  # SF Mono at 14px advances ~8.43px; round up generously
LINE_H = 1.25
VERTICAL = set("│┃║╭╮╰╯├┤┌┐└┘")


def snapshot(name):
    screen = tmux_screen() if a.emulator == "tmux" else pscreen
    rows = []
    for y in range(a.rows):
        line = screen.buffer[y]
        spans, cur, buf = [], None, []
        for x in range(a.cols):
            ch = line[x]
            fg, bg = color(ch.fg, theme["fg"]), color(ch.bg, theme["bg"])
            if ch.reverse:
                fg, bg = bg, fg
            if ch.blink:  # faint (see _sgr)
                fg = mix(fg, bg, 0.45)
            style = f"color:{fg};background:{bg};" + ("font-weight:700;" if ch.bold else "") + \
                ("font-style:italic;" if ch.italics else "") + ("text-decoration:underline;" if ch.underscore else "")
            if (ch.data or " ") in VERTICAL:
                # Terminals draw box lines through the whole cell; in HTML the
                # line-height leaves gaps, so stretch these glyphs vertically.
                if buf:
                    spans.append(f'<span style="{cur}">{html.escape("".join(buf))}</span>')
                    buf = []
                spans.append(f'<span style="{style}display:inline-block;transform:scaleY({LINE_H + 0.08})">'
                             f'{html.escape(ch.data)}</span>')
                cur = None
                continue
            if style != cur and buf:
                spans.append(f'<span style="{cur}">{html.escape("".join(buf))}</span>')
                buf = []
            cur = style
            buf.append(ch.data or " ")
        if buf:
            spans.append(f'<span style="{cur}">{html.escape("".join(buf))}</span>')
        rows.append("".join(spans))
    pad = 16
    page = f"""<!doctype html><meta charset="utf-8"><style>
html,body{{margin:0;background:{theme['bg']};}}
pre{{margin:0;padding:{pad}px;font:{FONT}px/{LINE_H} 'SF Mono','JetBrains Mono',Menlo,monospace;color:{theme['fg']};
white-space:pre;width:max-content;}}
</style><pre>{chr(10).join(rows)}</pre>"""
    base = os.path.join(a.out, a.prefix + name)
    with open(base + ".html", "w") as f:
        f.write(page)
    w = math.ceil(a.cols * CELL_W) + 2 * pad + 24
    h = math.ceil(a.rows * FONT * LINE_H) + 2 * pad + 4
    pending.append((base, w, h))
    print("captured", base + ".html", flush=True)


pending = []


def rasterize():
    """Turn captured HTML pages into PNGs with headless Chrome, a few at a
    time, after the session (so slow rasterization never delays capture)."""
    chrome = "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
    todo = list(pending)
    for attempt in range(3):
        procs, failed = [], []
        while todo or procs:
            while todo and len(procs) < 4:
                base, w, h = todo.pop(0)
                p = subprocess.Popen([chrome, "--headless=new", "--disable-gpu", "--no-first-run", "--hide-scrollbars",
                                      f"--window-size={w},{h}", f"--force-device-scale-factor={a.scale:g}",
                                      f"--screenshot={base}.png", "file://" + base + ".html"],
                                     stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
                procs.append((p, (base, w, h), time.time()))
            time.sleep(0.2)
            for item in list(procs):
                p, job, t0 = item
                if p.poll() is not None:
                    procs.remove(item)
                    print("wrote", job[0] + ".png", flush=True)
                elif time.time() - t0 > 60:
                    p.kill()
                    procs.remove(item)
                    failed.append(job)
        if not failed:
            return
        print("retrying", len(failed), "pages", flush=True)
        todo = failed


if a.emulator == "tmux":
    tmux_start()
else:
    pyte_start()
try:
    pump(5)
    snapshot("00-start")
    for i, st in enumerate(a.steps, 1):
        name, _, keys = st.partition("=")
        settle = a.settle
        if "@" in name:  # name@SECONDS=keys waits SECONDS after the keys (live data)
            name, _, secs = name.partition("@")
            settle = float(secs)
        send(keys.encode("latin-1", "backslashreplace").decode("unicode_escape"))
        pump(settle)
        if not name.startswith("_"):  # _name=keys sends keys without a capture
            snapshot(f"{i:02d}-{name}")
finally:
    if a.emulator == "tmux":
        tmux("kill-server")
        try:
            os.unlink(SOCK)
        except OSError:
            pass
    else:
        try:
            os.write(fd, b"q")
            pump(0.5)
            os.kill(pid, 9)
        except Exception:
            pass
rasterize()
