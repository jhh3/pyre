#!/usr/bin/env python3
"""Synthetic-test approver for real agent sessions: for the listed sessions
only, allow (a) Bash commands that run the pyre CLI and nothing dangerous and
(b) Pyre's own MCP tools (mcp__pyre__*) and (c) Write/Edit of files under
the directories in PYRE_APPROVE_WRITE_DIRS (colon-separated, e.g. a synthetic
shared checkout); deny everything else. Every decision
is logged with the tool title and a clipped body.

  PYRE_BIN=... PYRE_CONFIG=... approve_pyre_tools.py <seconds> <session-id>...
  (a session ID file may be given as @path; it is re-read every poll so
  sessions launched later by an agent can be added while it runs)
"""
import json, os, re, subprocess, sys, time

PYRE = os.environ.get("PYRE_BIN") or os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "..", "bin", "pyre")
deadline = time.time() + float(sys.argv[1])
SAFE = re.compile(r"(^|[\s;|&(])(\$PYRE_CLI|\$\{PYRE_CLI\}|\"\$PYRE_CLI\"|pyre|/[^\s]*/bin/pyre)\s")
DANGER = re.compile(r"\b(rm|curl|wget|ssh|scp|sudo|chmod|chown|mv|dd|git\s+push|kill)\b|>\s*/")
MCP_TITLE = re.compile(r"^Allow mcp__pyre__[a-z_]+\?$")
WRITE_DIRS = [d.rstrip("/") + "/" for d in os.environ.get("PYRE_APPROVE_WRITE_DIRS", "").split(":") if d.startswith("/")]


def write_ok(path):
    return path.startswith("/") and ".." not in path.split("/") and any(path.startswith(d) for d in WRITE_DIRS)
seen = set()


def sessions():
    out = set()
    for a in sys.argv[2:]:
        if a.startswith("@"):
            try:
                out.update(l.strip() for l in open(a[1:]) if l.strip())
            except OSError:
                pass
        else:
            out.add(a)
    return out


def run(*args):
    return subprocess.run([PYRE, *args], capture_output=True, text=True)


while time.time() < deadline:
    allowed = sessions()
    out = run("approvals", "--json")
    try:
        items = json.loads(out.stdout or '{"items":[]}').get("items", [])
    except json.JSONDecodeError:
        items = []
    for a in items:
        aid = a["approval_id"]
        if aid in seen or a.get("session_id") not in allowed or a.get("state") != "pending":
            continue
        seen.add(aid)
        title, body = a.get("title", ""), a.get("body", "")
        if MCP_TITLE.match(title):
            ok = True
        elif title in ("Allow Write?", "Allow Edit?"):
            ok = write_ok(body.strip())
        elif title == "Allow Bash?":
            ok = bool(SAFE.search(" " + body + " ")) and not DANGER.search(body)
        else:
            ok = False
        opt = "allow" if ok else "deny"
        r = run("approve", aid, "--option", opt)
        print(f"{time.strftime('%H:%M:%S')} {opt.upper():5} {a['session_id'][-6:]} {title} {body[:160]!r} rc={r.returncode}", flush=True)
    time.sleep(2)
