#!/usr/bin/env python3
"""Print the last N assistant text replies from a spike run's transcript
(transcript path taken from hook events). Usage: last_reply.py <scratch-dir> [N]"""
import json
import sys

d = sys.argv[1]
n = int(sys.argv[2]) if len(sys.argv) > 2 else 2
tp = None
for line in open(d + "/events.jsonl"):
    e = json.loads(line)
    tp = e.get("transcript_path") or tp
texts = []
for line in open(tp):
    r = json.loads(line)
    if r.get("type") == "assistant":
        for c in r["message"].get("content", []):
            if c.get("type") == "text":
                texts.append(c["text"])
for t in texts[-n:]:
    print(repr(t[:300]))
