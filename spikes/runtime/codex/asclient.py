#!/usr/bin/env python3
"""Minimal stdlib-only JSON-RPC client for `codex app-server` (Codex CLI 0.160.0).

Transports (all carry JSON-RPC 2.0 messages, one JSON object per message):
  stdio  -> spawn `codex app-server` (default --listen stdio://); newline-delimited JSON on stdin/stdout.
  unix   -> connect to `codex app-server --listen unix://PATH`; the socket speaks HTTP/1.1 WebSocket
            upgrade (GET /), then one JSON-RPC message per WebSocket text frame.
  proxy  -> spawn `codex app-server proxy --sock PATH`; a raw byte bridge from stdio to the socket, so the
            same WebSocket upgrade + framing is spoken over the proxy's stdin/stdout (useful over ssh).

Usage as a library: see experiments.py. Usage as CLI:
  asclient.py --transport unix --sock /path/as.sock probe      # initialize + account/read + rateLimits (redacted)
"""
import argparse
import base64
import json
import os
import queue
import shutil
import re
import socket
import struct
import subprocess
import sys
import threading
import time

CODEX_BIN_DIR = os.environ.get("CODEX_BIN_DIR", os.path.dirname(shutil.which("codex") or "/usr/local/bin/codex"))


# --------------------------------------------------------------------------- transports
class StdioTransport:
    def __init__(self, argv, env=None):
        e = dict(os.environ)
        e["PATH"] = CODEX_BIN_DIR + ":" + e.get("PATH", "")
        if env:
            e.update(env)
        self.proc = subprocess.Popen(argv, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                     stderr=open(os.devnull, "w") if not os.environ.get("ASCLIENT_STDERR") else None,
                                     env=e, bufsize=0)

    def send(self, obj):
        self.proc.stdin.write((json.dumps(obj) + "\n").encode())
        self.proc.stdin.flush()

    def recv_loop(self, cb):
        for line in self.proc.stdout:
            line = line.strip()
            if line:
                cb(json.loads(line))
        cb(None)

    def close(self):
        try:
            self.proc.stdin.close()
        except Exception:
            pass
        try:
            self.proc.terminate()
            self.proc.wait(3)
        except Exception:
            self.proc.kill()


class _PipeSock:
    """Socket-like adaptor over a subprocess's stdin/stdout (used for `codex app-server proxy`)."""

    def __init__(self, argv):
        e = dict(os.environ)
        e["PATH"] = CODEX_BIN_DIR + ":" + e.get("PATH", "")
        self.proc = subprocess.Popen(argv, stdin=subprocess.PIPE, stdout=subprocess.PIPE, bufsize=0, env=e)

    def sendall(self, b):
        self.proc.stdin.write(b)
        self.proc.stdin.flush()

    def recv(self, n):
        return os.read(self.proc.stdout.fileno(), n)

    def close(self):
        try:
            self.proc.stdin.close()
            self.proc.terminate()
            self.proc.wait(3)
        except Exception:
            self.proc.kill()


class UnixWebSocketTransport:
    def __init__(self, path=None, pipe_argv=None):
        if pipe_argv:
            self.sock = _PipeSock(pipe_argv)
        else:
            self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
            self.sock.connect(path)
        key = base64.b64encode(os.urandom(16)).decode()
        req = ("GET / HTTP/1.1\r\nHost: localhost\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"
               f"Sec-WebSocket-Key: {key}\r\nSec-WebSocket-Version: 13\r\n\r\n")
        self.sock.sendall(req.encode())
        buf = b""
        while b"\r\n\r\n" not in buf:
            chunk = self.sock.recv(4096)
            if not chunk:
                raise ConnectionError("socket closed during websocket handshake")
            buf += chunk
        head, self._rest = buf.split(b"\r\n\r\n", 1)
        if b" 101 " not in head.split(b"\r\n")[0]:
            raise ConnectionError("websocket upgrade failed: " + head.decode(errors="replace"))
        self.handshake_headers = head.decode(errors="replace")
        self._lock = threading.Lock()

    def _send_frame(self, opcode, payload):
        hdr = bytes([0x80 | opcode])
        n = len(payload)
        if n < 126:
            hdr += bytes([0x80 | n])
        elif n < 65536:
            hdr += bytes([0x80 | 126]) + struct.pack(">H", n)
        else:
            hdr += bytes([0x80 | 127]) + struct.pack(">Q", n)
        mask = os.urandom(4)
        masked = bytes(b ^ mask[i % 4] for i, b in enumerate(payload))
        with self._lock:
            self.sock.sendall(hdr + mask + masked)

    def send(self, obj):
        self._send_frame(0x1, json.dumps(obj).encode())

    def _read_exact(self, n):
        while len(self._rest) < n:
            chunk = self.sock.recv(65536)
            if not chunk:
                raise ConnectionError("eof")
            self._rest += chunk
        out, self._rest = self._rest[:n], self._rest[n:]
        return out

    def recv_loop(self, cb):
        msg = b""
        try:
            while True:
                b0, b1 = self._read_exact(2)
                fin, op = b0 & 0x80, b0 & 0x0F
                n = b1 & 0x7F
                if n == 126:
                    n = struct.unpack(">H", self._read_exact(2))[0]
                elif n == 127:
                    n = struct.unpack(">Q", self._read_exact(8))[0]
                if b1 & 0x80:
                    mask = self._read_exact(4)
                    data = bytes(b ^ mask[i % 4] for i, b in enumerate(self._read_exact(n)))
                else:
                    data = self._read_exact(n)
                if op == 0x9:  # ping
                    self._send_frame(0xA, data)
                    continue
                if op == 0x8:  # close
                    break
                if op in (0x0, 0x1, 0x2):
                    msg += data
                    if fin:
                        cb(json.loads(msg.decode()))
                        msg = b""
        except (ConnectionError, OSError):
            pass
        cb(None)

    def close(self):
        try:
            self._send_frame(0x8, b"")
        except Exception:
            pass
        try:
            self.sock.close()
        except Exception:
            pass


# --------------------------------------------------------------------------- client
class RpcError(Exception):
    def __init__(self, err):
        super().__init__(json.dumps(err))
        self.err = err


class Client:
    """JSON-RPC client. Notifications and server->client requests go to self.events (a Queue) and self.log."""

    def __init__(self, transport, log_path=None, server_request_handler=None):
        self.t = transport
        self.next_id = 1
        self.pending = {}
        self.events = queue.Queue()
        self.log = []
        self.closed = threading.Event()
        self.server_request_handler = server_request_handler
        self.log_f = open(log_path, "a") if log_path else None
        self._lock = threading.Lock()
        self.reader = threading.Thread(target=self.t.recv_loop, args=(self._on_msg,), daemon=True)
        self.reader.start()

    def _record(self, direction, msg):
        rec = {"ts": round(time.time(), 3), "dir": direction, "msg": msg}
        self.log.append(rec)
        if self.log_f:
            self.log_f.write(json.dumps(rec) + "\n")
            self.log_f.flush()

    def _on_msg(self, msg):
        if msg is None:
            self.closed.set()
            self.events.put(None)
            for q in list(self.pending.values()):
                q.put({"error": {"code": -1, "message": "connection closed"}})
            return
        self._record("in", msg)
        if "id" in msg and "method" not in msg:  # response
            q = self.pending.pop(msg["id"], None)
            if q:
                q.put(msg)
            return
        if "id" in msg and "method" in msg:  # server -> client request
            if self.server_request_handler:
                threading.Thread(target=self._handle_server_request, args=(msg,), daemon=True).start()
            self.events.put(msg)
            return
        self.events.put(msg)  # notification

    def _handle_server_request(self, msg):
        res = self.server_request_handler(self, msg)
        if res is not None:
            self.respond(msg["id"], res)

    def respond(self, req_id, result=None, error=None):
        m = {"jsonrpc": "2.0", "id": req_id}
        if error is not None:
            m["error"] = error
        else:
            m["result"] = result
        self._record("out", m)
        self.t.send(m)

    def send_request(self, method, params=None):
        with self._lock:
            rid = self.next_id
            self.next_id += 1
        q = queue.Queue()
        self.pending[rid] = q
        m = {"jsonrpc": "2.0", "id": rid, "method": method}
        if params is not None:
            m["params"] = params
        self._record("out", m)
        self.t.send(m)
        return rid, q

    def request(self, method, params=None, timeout=60):
        rid, q = self.send_request(method, params)
        try:
            r = q.get(timeout=timeout)
        except queue.Empty:
            raise TimeoutError(f"{method} timed out")
        if "error" in r:
            raise RpcError(r["error"])
        return r.get("result")

    def notify(self, method, params=None):
        m = {"jsonrpc": "2.0", "method": method}
        if params is not None:
            m["params"] = params
        self._record("out", m)
        self.t.send(m)

    def initialize(self, name="pyre-spike", experimental=False, opt_out=None):
        caps = {"experimentalApi": experimental}
        if opt_out:
            caps["optOutNotificationMethods"] = opt_out
        r = self.request("initialize", {"clientInfo": {"name": name, "version": "0.0.1"}, "capabilities": caps})
        self.notify("initialized")
        return r

    def wait_for(self, pred, timeout=120):
        """Consume events until pred(msg) is true; returns (match, consumed_list)."""
        seen = []
        deadline = time.time() + timeout
        while time.time() < deadline:
            try:
                m = self.events.get(timeout=max(0.05, deadline - time.time()))
            except queue.Empty:
                break
            if m is None:
                return None, seen
            seen.append(m)
            if pred(m):
                return m, seen
        return None, seen

    def drain(self, secs=0.5):
        out = []
        end = time.time() + secs
        while time.time() < end:
            try:
                m = self.events.get(timeout=0.05)
            except queue.Empty:
                continue
            if m is None:
                break
            out.append(m)
        return out

    def close(self):
        self.t.close()


def connect(transport, sock=None, log_path=None, handler=None, extra_args=None):
    if transport == "stdio":
        t = StdioTransport(["codex", "app-server"] + (extra_args or []))
    elif transport == "unix":
        t = UnixWebSocketTransport(sock)
    elif transport == "proxy":
        # `proxy` is a raw byte bridge to the control socket: still WebSocket framing on stdio.
        t = UnixWebSocketTransport(pipe_argv=["codex", "app-server", "proxy", "--sock", sock])
    else:
        raise ValueError(transport)
    return Client(t, log_path=log_path, server_request_handler=handler)


# --------------------------------------------------------------------------- redaction
EMAIL_RE = re.compile(r"[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}")
SENSITIVE_KEYS = re.compile(r"(email|accountid|account_id|userid|user_id|token|secret|orgid|org_id|workspaceid|"
                            r"organization|chatgptuserid|name$|login|planid)", re.I)


def redact(o, key=None):
    if isinstance(o, dict):
        return {k: redact(v, k) for k, v in o.items()}
    if isinstance(o, list):
        return [redact(v) for v in o]
    if isinstance(o, str):
        if key and SENSITIVE_KEYS.search(key) and key not in ("planType", "type", "method"):
            return "<redacted>"
        return EMAIL_RE.sub("<email>", o)
    return o


def short(m, n=400):
    s = json.dumps(m)
    return s if len(s) <= n else s[:n] + "...(%d bytes)" % len(s)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--transport", default="unix", choices=["stdio", "unix", "proxy"])
    ap.add_argument("--sock")
    ap.add_argument("cmd", choices=["probe"])
    a = ap.parse_args()
    c = connect(a.transport, a.sock)
    print("initialize ->", json.dumps(redact(c.initialize())))
    for m in ("account/read", "account/rateLimits/read"):
        try:
            print(m, "->", json.dumps(redact(c.request(m, {} if m == "account/read" else None)), indent=1))
        except Exception as e:
            print(m, "ERR", e)
    c.close()


if __name__ == "__main__":
    main()
