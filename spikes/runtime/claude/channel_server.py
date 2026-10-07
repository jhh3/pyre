#!/usr/bin/env python3
"""Minimal Claude Code channel (MCP stdio server, stdlib only) for the Pyre spike.

Declares the experimental `claude/channel` capability and pushes
`notifications/claude/channel` for every JSON file dropped into
$PYRE_SPIKE_DIR/channel-inbox/ (written atomically by ctl.py deliver --mode channel).
Also exposes one tool, `pyre_ack`, so the model can acknowledge a message ID.
Every protocol exchange is logged to $PYRE_SPIKE_DIR/channel.log.
"""
import json
import os
import sys
import threading
import time

D = os.environ["PYRE_SPIKE_DIR"]
INBOX = os.path.join(D, "channel-inbox")
LOG = os.path.join(D, "channel.log")
os.makedirs(INBOX, exist_ok=True)
lock = threading.Lock()


def log(kind, obj):
    with open(LOG, "a") as f:
        f.write(json.dumps({"t": round(time.time(), 3), "kind": kind, "msg": obj}) + "\n")


def send(obj):
    with lock:
        sys.stdout.write(json.dumps(obj) + "\n")
        sys.stdout.flush()
    log("out", obj)


def pump():
    while True:
        for fn in sorted(os.listdir(INBOX)):
            if not fn.endswith(".json"):
                continue
            p = os.path.join(INBOX, fn)
            try:
                m = json.load(open(p))
            except Exception:
                continue
            send({"jsonrpc": "2.0", "method": "notifications/claude/channel",
                  "params": {"content": m["content"],
                             "meta": {"message_id": m["message_id"], "sender": "pyre-spike"}}})
            os.rename(p, p + ".pushed")
        time.sleep(0.3)


def main():
    started = False
    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        msg = json.loads(line)
        log("in", msg)
        mid, method = msg.get("id"), msg.get("method")
        if method == "initialize":
            send({"jsonrpc": "2.0", "id": mid, "result": {
                "protocolVersion": msg["params"].get("protocolVersion", "2025-06-18"),
                "capabilities": {"experimental": {"claude/channel": {}}, "tools": {}},
                "serverInfo": {"name": "pyre", "version": "0.0.1"},
                "instructions": "Messages from the Pyre fleet arrive as <channel source=\"pyre\"> events. "
                                "Call the pyre_ack tool with the message_id after reading one."}})
        elif method == "notifications/initialized":
            if not started:
                threading.Thread(target=pump, daemon=True).start()
                started = True
        elif method == "tools/list":
            send({"jsonrpc": "2.0", "id": mid, "result": {"tools": [{
                "name": "pyre_ack",
                "description": "Acknowledge a Pyre channel message by id",
                "inputSchema": {"type": "object", "properties": {"message_id": {"type": "string"}},
                                "required": ["message_id"]}}]}})
        elif method == "tools/call":
            args = msg["params"].get("arguments", {})
            log("ack", args)
            send({"jsonrpc": "2.0", "id": mid, "result": {
                "content": [{"type": "text", "text": f"acked {args.get('message_id')}"}]}})
        elif mid is not None:
            send({"jsonrpc": "2.0", "id": mid, "error": {"code": -32601, "message": "not found"}})


if __name__ == "__main__":
    main()
