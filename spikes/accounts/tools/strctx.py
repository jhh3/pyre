#!/usr/bin/env python3
"""Print byte context around literal matches in a binary (code/strings only, no secrets).
Usage: strctx.py BINARY PATTERN [max_hits] [before] [after]"""
import re
import sys

b = open(sys.argv[1], "rb").read()
pat = sys.argv[2].encode()
n = int(sys.argv[3]) if len(sys.argv) > 3 else 5
before = int(sys.argv[4]) if len(sys.argv) > 4 else 200
after = int(sys.argv[5]) if len(sys.argv) > 5 else 300
for m in list(re.finditer(re.escape(pat), b))[:n]:
    s = b[max(0, m.start() - before):m.end() + after]
    print(m.start(), re.sub(rb"[^\x20-\x7e]+", b" | ", s).decode())
    print("-----")
