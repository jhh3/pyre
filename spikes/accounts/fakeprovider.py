#!/usr/bin/env python3
"""Pyre WP02 fake provider: a local stand-in for Anthropic and OpenAI/ChatGPT endpoints.

Purpose: prove per-session account isolation with OBVIOUSLY SYNTHETIC credentials.
It never needs, and refuses to log, real credentials:
  * Any bearer/api-key value that does not start with "pyre-synthetic" (or a JWT whose
    payload carries "pyre_synthetic": true) is logged as "<NON-SYNTHETIC-REDACTED>" and
    the request is answered 401, so a leak shows up as a loud alarm, not a secret.

Roles (one process, one port):
  * Anthropic API:   POST /v1/messages (SSE or JSON), /v1/messages/count_tokens,
                     GET /api/oauth/profile, /api/oauth/usage, /api/oauth/claude_cli/roles,
                     POST /v1/oauth/token (refresh; single-use rotation emulated)
  * ChatGPT/Codex:   POST .../responses (SSE), GET .../wham/usage, GET .../models,
                     POST /oauth/token (refresh; single-use rotation emulated)
  * HTTP proxy:      CONNECT host:443 is logged. With --mitm-ca, hosts listed in
                     --mitm-hosts are TLS-terminated with a throwaway CA and served by the
                     handlers above (lets us observe a harness's fixed refresh URL); all
                     other CONNECTs get 403 so nothing leaves the machine.

Log: JSONL to --log (default stdout). Each line: ts, via, method, host, path, auth, acct
(header), ua, note.
"""
import argparse
import re
import base64
import json
import os
import socket
import ssl
import subprocess
import sys
import threading
import time
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlsplit

LOCK = threading.Lock()
MARKER = re.compile(rb"PYRE-MARKER-[A-Za-z0-9_-]+")
STATE = {"refresh": {}, "log": None, "args": None}


def jwt_payload(tok):
    try:
        p = tok.split(".")[1]
        p += "=" * (-len(p) % 4)
        return json.loads(base64.urlsafe_b64decode(p))
    except Exception:
        return None


def is_synthetic(tok):
    if not tok:
        return True
    if tok.startswith("pyre-synthetic"):
        return True
    pl = jwt_payload(tok)
    return bool(pl and pl.get("pyre_synthetic") is True)


def label_of(tok):
    """Account label from a synthetic token: pyre-synthetic-<LABEL>-..."""
    if not tok:
        return None
    if tok.startswith("pyre-synthetic-"):
        return tok.split("-")[2]
    pl = jwt_payload(tok) or {}
    return pl.get("pyre_label")


def make_jwt(claims):
    enc = lambda d: base64.urlsafe_b64encode(json.dumps(d).encode()).rstrip(b"=").decode()
    return f"{enc({'alg': 'none', 'typ': 'JWT'})}.{enc(claims)}.sig"


def log(rec):
    rec = {"ts": round(time.time(), 3), **rec}
    line = json.dumps(rec, sort_keys=True)
    with LOCK:
        f = STATE["log"]
        f.write(line + "\n")
        f.flush()


def sse(handler, events):
    handler.send_response(200)
    handler.send_header("Content-Type", "text/event-stream")
    handler.send_header("Cache-Control", "no-cache")
    handler.send_header("Connection", "close")
    handler.end_headers()
    for name, data in events:
        chunk = f"event: {name}\ndata: {json.dumps(data)}\n\n"
        handler.wfile.write(chunk.encode())
        handler.wfile.flush()
    handler.close_connection = True


class H(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    server_version = "pyre-fake/1"
    via = "http"
    mitm_host = None

    def log_message(self, fmt, *a):  # silence default stderr logging
        pass

    # ---------- helpers ----------
    def body(self):
        n = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(n) if n else b""
        self.markers = sorted(set(m.decode() for m in MARKER.findall(raw)))
        try:
            return json.loads(raw) if raw else {}
        except Exception:
            try:
                from urllib.parse import parse_qs
                return {k: v[0] for k, v in parse_qs(raw.decode()).items()}
            except Exception:
                return {}

    def auth(self):
        a = self.headers.get("Authorization") or ""
        tok = a.split(" ", 1)[1] if a.lower().startswith("bearer ") else ""
        tok = tok or self.headers.get("x-api-key") or ""
        return tok

    def json(self, code, obj, extra=None):
        data = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        for k, v in (extra or {}).items():
            self.send_header(k, v)
        self.end_headers()
        self.wfile.write(data)

    def record(self, note="", tok=None, extra=None):
        tok = self.auth() if tok is None else tok
        shown = tok if is_synthetic(tok) else "<NON-SYNTHETIC-REDACTED>"
        rec = {
            "via": self.via, "method": self.command,
            "host": self.mitm_host or self.headers.get("Host"),
            "path": self.path, "auth": shown[:80] if shown else None,
            "label": label_of(tok) if is_synthetic(tok) else "ALARM",
            "acct": self.headers.get("chatgpt-account-id"),
            "ua": (self.headers.get("User-Agent") or "")[:60],
            "pid_hint": self.headers.get("x-pyre-run"), "note": note,
        }
        if not tok:
            rec["header_names"] = sorted(k.lower() for k in self.headers.keys())
        if getattr(self, "markers", None):
            rec["markers"] = self.markers
        if extra:
            rec.update(extra)
        log(rec)
        return tok

    # ---------- proxy ----------
    def do_CONNECT(self):
        host, _, port = self.path.partition(":")
        args = STATE["args"]
        if args.mitm_ca and host in args.mitm_hosts:
            log({"via": "proxy", "method": "CONNECT", "host": self.path, "note": "mitm"})
            self.send_response(200, "Connection Established")
            self.end_headers()
            ctx = leaf_context(host)
            try:
                tls = ctx.wrap_socket(self.connection, server_side=True)
            except Exception as e:
                log({"via": "proxy", "host": host, "note": f"tls-fail {e}"})
                return
            # serve HTTP requests inside the tunnel with the same handler logic
            Inner(tls, self.client_address, self.server, host)
            self.close_connection = True
            return
        log({"via": "proxy", "method": "CONNECT", "host": self.path, "note": "blocked"})
        self.send_response(403, "Blocked by pyre fake provider")
        self.send_header("Content-Length", "0")
        self.end_headers()
        self.close_connection = True

    # ---------- routes ----------
    def do_GET(self):
        self.route()

    def do_POST(self):
        self.route()

    def do_PUT(self):
        self.route()

    def do_DELETE(self):
        self.route()

    def route(self):
        if self.path.startswith("http://") or self.path.startswith("https://"):
            # absolute-form request through the proxy: never forward
            self.record("proxy-absolute-blocked")
            return self.json(403, {"error": "blocked"})
        p = urlsplit(self.path).path
        if p.endswith("/v1/oauth/token") or p == "/oauth/token":
            return self.oauth_token()
        tok = self.auth()
        if not is_synthetic(tok):
            self.record("REJECT non-synthetic credential")
            return self.json(401, {"error": {"type": "authentication_error", "message": "non-synthetic"}})
        if STATE["args"].expire_all and tok and tok.startswith("pyre-synthetic") and "-g" not in tok:
            self.record("401 forced (initial token treated as expired)")
            return self.json(401, {"type": "error", "error": {"type": "authentication_error",
                                                             "message": "OAuth token has expired"}})
        if p == "/v1/messages":
            return self.anthropic_messages()
        if p == "/v1/messages/count_tokens":
            self.record()
            return self.json(200, {"input_tokens": 10})
        if p == "/api/oauth/profile":
            self.record()
            lab = label_of(tok) or "X"
            return self.json(200, {
                "account": {"uuid": f"00000000-0000-4000-8000-00000000000{lab[:1]}",
                            "email": f"acct-{lab}@synthetic.invalid", "display_name": f"Synthetic {lab}",
                            "has_claude_max": True, "has_claude_pro": False},
                "organization": {"uuid": f"00000000-0000-4000-9000-00000000000{lab[:1]}",
                                 "name": f"synthetic-org-{lab}", "organization_type": "claude_max",
                                 "rate_limit_tier": "default_claude_max_20x"},
            })
        if p == "/api/oauth/usage":
            self.record()
            return self.json(200, {
                "five_hour": {"utilization": 12.0, "resets_at": "2026-10-05T03:00:00Z"},
                "seven_day": {"utilization": 34.0, "resets_at": "2026-10-09T00:00:00Z"},
                "pyre_synthetic_label": label_of(tok)})
        if p == "/api/oauth/claude_cli/roles":
            self.record()
            return self.json(200, {"organization_role": "admin", "workspace_role": None,
                                   "organization_name": "synthetic"})
        if p.endswith("/responses") and self.command == "GET":
            # websocket upgrade attempt: refuse so the client falls back to HTTPS SSE
            self.record("websocket refused")
            return self.json(404, {"error": "no websocket in pyre fake provider"})
        if p.endswith("/responses"):
            return self.openai_responses()
        if p.endswith("/accounts/check"):
            # Guessed shape; Codex 0.160 workspace routing discovery may need more fields.
            self.record()
            lab = label_of(tok) or "X"
            aid = self.headers.get("chatgpt-account-id") or f"synthetic-acct-{lab}"
            return self.json(200, {"accounts": {aid: {"account": {
                "account_id": aid, "account_user_role": "account-owner", "structure": "personal",
                "plan_type": "pro", "is_default": True, "name": None}, "features": []}},
                "account_ordering": [aid]})
        if p.endswith("/wham/usage") or p.endswith("/codex/usage"):
            self.record()
            lab = label_of(tok) or "X"
            return self.json(200, {
                "plan_type": "pro",
                "rate_limit": {"allowed": True, "limit_reached": False,
                               "primary_window": {"used_percent": 11 if lab == "A" else 77,
                                                  "limit_window_seconds": 18000,
                                                  "reset_after_seconds": 3600, "reset_at": 1791190000},
                               "secondary_window": {"used_percent": 22, "limit_window_seconds": 604800,
                                                    "reset_after_seconds": 86400, "reset_at": 1791270000}},
                "credits": None, "pyre_synthetic_label": lab})
        if p.endswith("/models"):
            self.record()
            return self.json(200, {"models": [], "data": []})
        self.record("unrouted")
        return self.json(404, {"error": "unrouted in pyre fake provider", "path": p})

    def anthropic_messages(self):
        b = self.body()
        tok = self.record("messages", extra={"model": b.get("model"), "stream": b.get("stream")})
        lab = label_of(tok)
        text = f"pyre-fake-reply label={lab}"
        if STATE["args"].sleep:
            time.sleep(STATE["args"].sleep)
        mid = "msg_" + uuid.uuid4().hex[:20]
        usage = {"input_tokens": 5, "output_tokens": 5, "cache_read_input_tokens": 0,
                 "cache_creation_input_tokens": 0}
        dump = json.dumps(b)
        has_bash = any(t.get("name") == "Bash" for t in b.get("tools") or [] if isinstance(t, dict))
        if "PYRE-TOOLTEST" in dump and '"tool_result"' not in dump and has_bash and b.get("stream"):
            # Ask the harness to run a shell command that reports (as a marker, never the value)
            # whether credential env vars / fds are visible to tool subprocesses.
            # the marker prefix is assembled at runtime so only command OUTPUT can match MARKER
            cmd = ("P=PYRE; for v in CLAUDE_CODE_OAUTH_TOKEN CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR CLAUDE_CONFIG_DIR; do "
                   "x=$(printenv $v | cut -c1-14 | tr -c 'a-zA-Z0-9' '_'); echo $P-MARKER-ENV-$v-${x:-unset}; done; "
                   "echo $P-MARKER-FDS-$(ls /dev/fd | tr '\\n' '_')")
            log({"via": "http", "note": "returning tool_use Bash env probe", "label": lab})
            return sse(self, [
                ("message_start", {"type": "message_start", "message": {
                    "id": mid, "type": "message", "role": "assistant", "model": b.get("model", "x"),
                    "content": [], "stop_reason": None, "stop_sequence": None, "usage": usage}}),
                ("content_block_start", {"type": "content_block_start", "index": 0, "content_block": {
                    "type": "tool_use", "id": "toolu_" + uuid.uuid4().hex[:20], "name": "Bash", "input": {}}}),
                ("content_block_delta", {"type": "content_block_delta", "index": 0, "delta": {
                    "type": "input_json_delta",
                    "partial_json": json.dumps({"command": cmd, "description": "pyre env probe"})}}),
                ("content_block_stop", {"type": "content_block_stop", "index": 0}),
                ("message_delta", {"type": "message_delta", "delta": {"stop_reason": "tool_use",
                                                                      "stop_sequence": None},
                                   "usage": {"output_tokens": 5}}),
                ("message_stop", {"type": "message_stop"}),
            ])
        if not b.get("stream"):
            return self.json(200, {"id": mid, "type": "message", "role": "assistant",
                                   "model": b.get("model", "x"), "stop_reason": "end_turn",
                                   "stop_sequence": None, "usage": usage,
                                   "content": [{"type": "text", "text": text}]})
        sse(self, [
            ("message_start", {"type": "message_start", "message": {
                "id": mid, "type": "message", "role": "assistant", "model": b.get("model", "x"),
                "content": [], "stop_reason": None, "stop_sequence": None, "usage": usage}}),
            ("content_block_start", {"type": "content_block_start", "index": 0,
                                     "content_block": {"type": "text", "text": ""}}),
            ("content_block_delta", {"type": "content_block_delta", "index": 0,
                                     "delta": {"type": "text_delta", "text": text}}),
            ("content_block_stop", {"type": "content_block_stop", "index": 0}),
            ("message_delta", {"type": "message_delta", "delta": {"stop_reason": "end_turn",
                                                                  "stop_sequence": None},
                               "usage": {"output_tokens": 5}}),
            ("message_stop", {"type": "message_stop"}),
        ])

    def openai_responses(self):
        b = self.body()
        tok = self.record("responses", extra={"model": b.get("model")})
        lab = label_of(tok)
        text = f"pyre-fake-reply label={lab}"
        if STATE["args"].sleep:
            time.sleep(STATE["args"].sleep)
        rid = "resp_" + uuid.uuid4().hex[:20]
        item = {"type": "message", "role": "assistant", "id": "msg_" + uuid.uuid4().hex[:12],
                "status": "completed",
                "content": [{"type": "output_text", "text": text, "annotations": []}]}
        sse(self, [
            ("response.created", {"type": "response.created", "response": {"id": rid}}),
            ("response.output_item.added", {"type": "response.output_item.added", "output_index": 0,
                                            "item": {**item, "status": "in_progress", "content": []}}),
            ("response.output_text.delta", {"type": "response.output_text.delta", "output_index": 0,
                                            "content_index": 0, "item_id": item["id"], "delta": text}),
            ("response.output_item.done", {"type": "response.output_item.done", "output_index": 0,
                                           "item": item}),
            ("response.completed", {"type": "response.completed", "response": {
                "id": rid, "output": [item],
                "usage": {"input_tokens": 5, "input_tokens_details": {"cached_tokens": 0},
                          "output_tokens": 5, "output_tokens_details": {"reasoning_tokens": 0},
                          "total_tokens": 10}}}),
        ])

    def oauth_token(self):
        b = self.body()
        rt = b.get("refresh_token") or ""
        grant = b.get("grant_type")
        shown = rt if is_synthetic(rt) else "<NON-SYNTHETIC-REDACTED>"
        if not is_synthetic(rt):
            log({"via": self.via, "host": self.mitm_host or self.headers.get("Host"), "path": self.path,
                 "note": "ALARM non-synthetic refresh token presented; rejected", "grant": grant})
            return self.json(400, {"error": "invalid_grant"})
        lab = label_of(rt) or "X"
        if STATE["args"].sleep_refresh:
            time.sleep(STATE["args"].sleep_refresh)
        with LOCK:
            used = STATE["refresh"].get(rt)
            if used is None:
                gen = STATE["refresh"].get(("gen", lab), 0) + 1
                STATE["refresh"][("gen", lab)] = gen
                STATE["refresh"][rt] = gen
            else:
                gen = None
        if gen is None:
            log({"via": self.via, "host": self.mitm_host or self.headers.get("Host"), "path": self.path,
                 "method": "POST", "grant": grant, "refresh": shown, "label": lab,
                 "note": f"REUSED refresh token (first used for gen {used}) -> invalid_grant"})
            return self.json(400, {"error": "invalid_grant",
                                   "error_description": "refresh token already used"})
        log({"via": self.via, "host": self.mitm_host or self.headers.get("Host"), "path": self.path,
             "method": "POST", "grant": grant, "refresh": shown, "label": lab,
             "client_id": b.get("client_id"), "note": f"rotated -> gen {gen}"})
        access = f"pyre-synthetic-{lab}-access-g{gen}"
        resp = {"access_token": access, "refresh_token": f"pyre-synthetic-{lab}-refresh-g{gen}",
                "expires_in": 3600, "token_type": "Bearer",
                "scope": "user:inference user:profile user:sessions:claude_code"}
        if self.path == "/oauth/token" or "openai" in (self.mitm_host or ""):
            claims = {"pyre_synthetic": True, "pyre_label": lab, "exp": int(time.time()) + 3600,
                      "email": f"acct-{lab}@synthetic.invalid",
                      "https://api.openai.com/auth": {"chatgpt_plan_type": "pro",
                                                      "chatgpt_account_id": f"synthetic-acct-{lab}"}}
            resp["id_token"] = make_jwt(claims)
            resp["access_token"] = make_jwt({**claims, "gen": gen})
        return self.json(200, resp)


class Inner(H):
    """Handler for requests inside a MITM'd TLS tunnel."""
    via = "mitm"

    def __init__(self, sock, addr, server, host):
        self.mitm_host = host
        super().__init__(sock, addr, server)


_LEAF = {}


def leaf_context(host):
    if host in _LEAF:
        return _LEAF[host]
    ca = STATE["args"].mitm_ca
    d = os.path.dirname(ca)
    key, csr, crt, ext = (os.path.join(d, f"{host}.{x}") for x in ("key", "csr", "crt", "ext"))
    if not os.path.exists(crt):
        with open(ext, "w") as f:
            f.write(f"subjectAltName=DNS:{host}\nextendedKeyUsage=serverAuth\n")
        subprocess.run(["openssl", "req", "-newkey", "rsa:2048", "-nodes", "-keyout", key, "-out", csr,
                        "-subj", f"/CN={host}"], check=True, capture_output=True)
        subprocess.run(["openssl", "x509", "-req", "-in", csr, "-CA", ca, "-CAkey", ca[:-4] + ".key",
                        "-CAcreateserial", "-out", crt, "-days", "2", "-extfile", ext],
                       check=True, capture_output=True)
    ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    ctx.load_cert_chain(crt, key)
    ctx.set_alpn_protocols(["http/1.1"])
    _LEAF[host] = ctx
    return ctx


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--port", type=int, default=0)
    ap.add_argument("--log", default="-")
    ap.add_argument("--port-file")
    ap.add_argument("--sleep", type=float, default=0, help="delay model replies (seconds)")
    ap.add_argument("--sleep-refresh", type=float, default=0, help="delay token refresh replies")
    ap.add_argument("--expire-all", action="store_true",
                    help="401 any initial synthetic token (no -g<n> generation) to force refresh")
    ap.add_argument("--mitm-ca", help="path to throwaway CA cert (key alongside as .key)")
    ap.add_argument("--mitm-hosts", default="platform.claude.com,console.anthropic.com,auth.openai.com")
    a = ap.parse_args()
    a.mitm_hosts = set(h for h in a.mitm_hosts.split(",") if h)
    STATE["args"] = a
    STATE["log"] = sys.stdout if a.log == "-" else open(a.log, "a")
    srv = ThreadingHTTPServer(("127.0.0.1", a.port), H)
    srv.handle_error = lambda req, addr: None  # client resets are expected noise
    srv.daemon_threads = True
    port = srv.server_address[1]
    if a.port_file:
        with open(a.port_file, "w") as f:
            f.write(str(port))
    log({"via": "server", "note": f"listening 127.0.0.1:{port}"})
    srv.serve_forever()


if __name__ == "__main__":
    main()
