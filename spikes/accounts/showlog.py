#!/usr/bin/env python3
"""Summarise a fakeprovider JSONL log: one line per request, unrelated local port probes dropped."""
import json
import sys

for line in open(sys.argv[1]):
    r = json.loads(line)
    if r.get("ua") == "node" and r.get("path") == "/":
        continue  # a desktop app on this Mac probes new localhost ports; not the harness
    if r.get("via") == "server":
        continue
    if r.get("note") == "unrouted" and any(k in (r.get("path") or "") for k in ("/plugins", "/ps/", "/connectors")):
        continue  # Codex plugin/connector catalogue calls; carry the same identity, omitted for brevity
    cred = r.get("auth") or r.get("refresh")
    parts = [r.get("via"), r.get("method"), r.get("host"), (r.get("path") or "")[:40]]
    if r.get("pid_hint"):
        parts.append("run=" + r["pid_hint"])
    if cred:
        parts.append("cred=" + (cred[:44] if cred.startswith("pyre-") else f"<synthetic JWT label={r.get('label')}>"))
    if r.get("acct"):
        parts.append("chatgpt-account-id=" + r["acct"])
    if r.get("markers"):
        parts.append("markers=" + ",".join(r["markers"]))
    if r.get("note"):
        parts.append("[" + r["note"] + "]")
    print(" ".join(str(p) for p in parts if p))
