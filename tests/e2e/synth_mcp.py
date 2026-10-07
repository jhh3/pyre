#!/usr/bin/env python3
"""Synthetic MCP stdio server for the A15 managed-profile test (no network,
no secrets). Usage: synth_mcp.py NAME LOGFILE

On start it appends one JSON line to LOGFILE: server name, pid, parent pid and
the PYRE_ENV_GENERATION it was started under, so the test can tell which
environment generation's --mcp-config a harness loaded. It answers
initialize, tools/list (one no-op tool) and tools/call.
"""
import json
import os
import sys
import time

name = sys.argv[1] if len(sys.argv) > 1 else "synthetic"
log = sys.argv[2] if len(sys.argv) > 2 else None
if log:
    with open(log, "a") as f:
        f.write(json.dumps({
            "server": name,
            "pid": os.getpid(),
            "ppid": os.getppid(),
            "generation": os.environ.get("PYRE_ENV_GENERATION", ""),
            "at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        }) + "\n")


def reply(msg_id, result=None, error=None):
    out = {"jsonrpc": "2.0", "id": msg_id}
    if error is not None:
        out["error"] = error
    else:
        out["result"] = result
    sys.stdout.write(json.dumps(out) + "\n")
    sys.stdout.flush()


for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    try:
        req = json.loads(line)
    except ValueError:
        continue
    method, msg_id = req.get("method"), req.get("id")
    if msg_id is None:
        continue  # notification
    if method == "initialize":
        pv = (req.get("params") or {}).get("protocolVersion") or "2025-06-18"
        reply(msg_id, {"protocolVersion": pv, "capabilities": {"tools": {}},
                       "serverInfo": {"name": "wupd-" + name, "version": "0.0.0-synthetic"}})
    elif method == "tools/list":
        reply(msg_id, {"tools": [{"name": "wupd_%s_ping" % name, "description": "Synthetic no-op tool (test only).",
                                  "inputSchema": {"type": "object", "properties": {}}}]})
    elif method == "tools/call":
        reply(msg_id, {"content": [{"type": "text", "text": "synthetic pong from %s" % name}]})
    elif method == "ping":
        reply(msg_id, {})
    else:
        reply(msg_id, error={"code": -32601, "message": "method not found"})
