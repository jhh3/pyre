#!/usr/bin/env python3
"""Reduce raw app-server experiment logs to a redacted protocol trace:
direction, method/id, and a whitelist of structural fields. Drops account data,
emails, rollout paths and free text other than short synthetic markers.
Usage: summarize_logs.py <raw-log-dir> <out-dir> name1 name2 ..."""
import glob
import json
import os
import re
import sys

KEEP = {"threadId", "turnId", "itemId", "id", "status", "type", "clientId", "kind",
        "requestId", "decision", "code", "message", "expectedTurnId", "agentThreadId",
        "parentThreadId", "receiverThreadIds", "senderThreadId", "tool", "activeFlags",
        "forkedFromId", "planType", "availableDecisions", "command", "clientUserMessageId",
        "approvalPolicy", "exitCode", "willRetry"}
EMAIL = re.compile(r"[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[a-z]{2,}")


def prune(o, depth=0):
    if depth > 6:
        return "…"
    if isinstance(o, dict):
        out = {}
        for k, v in o.items():
            if k in ("email", "accountId", "chatgptAccountId", "path", "codexHome", "userAgent", "cwd"):
                out[k] = "<redacted>"
            elif k in KEEP or isinstance(v, (dict, list)):
                pv = prune(v, depth + 1)
                if pv not in ({}, [], None):
                    out[k] = pv
        return out
    if isinstance(o, list):
        return [prune(x, depth + 1) for x in o[:8]]
    if isinstance(o, str):
        s = EMAIL.sub("<email>", o)
        s = re.sub(r"/Users/[^/\s\"]+", "/Users/USER", s)
        return s[:160]
    return o


def main():
    src, out = sys.argv[1], sys.argv[2]
    os.makedirs(out, exist_ok=True)
    for name in sys.argv[3:]:
        for p in sorted(glob.glob(os.path.join(src, name + "-*.jsonl")))[-1:]:
            lines = []
            for raw in open(p):
                try:
                    r = json.loads(raw)
                except Exception:
                    continue
                msg = r.get("msg", r)
                rec = {"dir": r.get("dir") or r.get("kind")}
                for k in ("method", "id"):
                    if k in msg:
                        rec[k] = msg[k]
                for k in ("params", "result", "error"):
                    if k in msg:
                        rec[k] = prune(msg[k])
                lines.append(json.dumps(rec))
            with open(os.path.join(out, os.path.basename(p)), "w") as f:
                f.write("\n".join(lines) + "\n")
            print(os.path.basename(p), len(lines))


if __name__ == "__main__":
    main()
