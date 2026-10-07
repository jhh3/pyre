#!/usr/bin/env python3
"""Ask `codex app-server` (in the current CODEX_HOME) for account/read and
account/rateLimits/read, exactly as rota does, and print the redacted results.
Usage: appserver_probe.py CODEX_BIN [codex -c overrides...]"""
import json
import select
import subprocess
import sys
import time

cmd = [sys.argv[1], *sys.argv[2:], "app-server"]
p = subprocess.Popen(cmd, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE)


def send(m):
    p.stdin.write((json.dumps(m) + "\n").encode())
    p.stdin.flush()


def wait_for(i, timeout=15):
    buf = b""
    end = time.time() + timeout
    while time.time() < end:
        r, _, _ = select.select([p.stdout], [], [], 0.5)
        if not r:
            continue
        line = p.stdout.readline()
        if not line:
            break
        try:
            m = json.loads(line)
        except Exception:
            continue
        if m.get("id") == i:
            return m
    return {"timeout": i}


try:
    send({"id": 1, "method": "initialize", "params": {"clientInfo": {"name": "pyre-wp02", "version": "0"}}})
    wait_for(1)
    send({"method": "initialized", "params": {}})
    send({"id": 2, "method": "account/read", "params": {"refreshToken": False}})
    r = wait_for(2)
    print("account/read:", json.dumps(r.get("result") or r.get("error") or r))
    send({"id": 3, "method": "account/rateLimits/read", "params": {}})
    r = wait_for(3)
    print("account/rateLimits/read:", json.dumps(r.get("result") or r.get("error") or r)[:900])
finally:
    p.terminate()
    try:
        p.wait(3)
    except Exception:
        p.kill()
