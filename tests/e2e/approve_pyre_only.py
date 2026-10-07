#!/usr/bin/env python3
"""Synthetic-test approver: for the listed sessions only, allow permission
requests whose command runs the pyre CLI (pyre / $PYRE_CLI / absolute pyre
path) or calls one of Pyre's own MCP tools, and nothing dangerous; deny
everything else. Logs every decision.

  PYRE_CONFIG=... approve_pyre_only.py <seconds> <session-id>...
"""
import json, os, re, subprocess, sys, time

PYRE = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "..", "bin", "pyre")
deadline = time.time() + float(sys.argv[1])
sessions = set(sys.argv[2:])
SAFE = re.compile(r"(^|[\s;|&('\"])(\$PYRE_CLI|\$\{PYRE_CLI\}|pyre|/[^\s]*/bin/pyre)\s")
# Pyre's own MCP tools (Claude: mcp__pyre__<tool>; Codex: server "pyre").
MCP_SAFE = re.compile(r"\bmcp__pyre__\w+|\bpyre[./:]\s*(send_message|inbox|wait_reply|read_session|list_sessions|list_nodes|publish|ack|note)\b")
DANGER = re.compile(r"\b(rm|curl|wget|ssh|scp|sudo|chmod|chown|mv|dd|git\s+push|kill)\b|>\s*/")
seen = set()


def run(*args):
    return subprocess.run([PYRE, *args], capture_output=True, text=True)


while time.time() < deadline:
    out = run("approvals", "--json")
    items = json.loads(out.stdout or '{"items":[]}').get("items", [])
    for a in items:
        aid = a["approval_id"]
        if aid in seen or a.get("session_id") not in sessions or a.get("state") != "pending":
            continue
        seen.add(aid)
        body = a.get("body", "")
        ok = (bool(SAFE.search(" " + body + " ")) or bool(MCP_SAFE.search(body))) and not DANGER.search(body)
        want = "allow" if ok else "deny"
        # Option ids differ per harness (Claude allow/deny, Codex accept/cancel):
        # pick by kind.
        opts = a.get("options") or []
        opt = next((o["id"] for o in opts if o.get("kind") == want), want)
        r = run("approve", aid, "--option", opt)
        print(f"{time.strftime('%H:%M:%S')} {want.upper():5} ({opt}) {a['session_id'][-6:]} {body[:140]!r} rc={r.returncode}", flush=True)
    time.sleep(3)
