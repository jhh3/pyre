#!/usr/bin/env python3
"""Pyre WP01 Codex app-server experiments (stdlib only). Uses asclient.py.

Each subcommand prints a compact, redacted event trace. Full raw logs go to $SCRATCH/logs/<cmd>-<ts>.jsonl
(scratch only; they may contain account data and must not be committed).

  experiments.py basic     [--transport unix|stdio|proxy] [--sock PATH] [--thread ID]
  experiments.py busy      ...   # turn/start + turn/steer while a turn is running
  experiments.py interrupt ...   # turn/interrupt during a running command
  experiments.py approval  ... [--decision accept|decline] [--double]
  experiments.py crash     ...   # send turn/start, then os._exit before reading the response
  experiments.py reconcile --thread ID --marker M   # new controller finds the marker via thread/read
  experiments.py resume    --thread ID              # thread/resume (+ thread/fork) in this app-server
  experiments.py subagent  ...
  experiments.py watch     --thread ID [--secs N]   # resume/subscribe and print notifications (for TUI interop)
  experiments.py say       --thread ID --text T     # resume and send a turn, then exit after completion
"""
import argparse
import json
import os
import sys
import time
import uuid

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from asclient import connect, redact, short, RpcError  # noqa: E402

SCRATCH = os.environ.get("PYRE_CODEX_SCRATCH", "/tmp/pyre-wp01/codex")
WORK = os.path.join(SCRATCH, "work")
NOISY = {"item/agentMessage/delta", "item/reasoning/summaryTextDelta", "item/reasoning/textDelta",
         "item/reasoning/summaryPartAdded", "item/commandExecution/outputDelta", "thread/tokenUsage/updated",
         "account/rateLimits/updated", "mcpServer/startupStatus/updated", "item/plan/delta"}


def t0():
    return time.time()


START = t0()


def p(*a):
    print("[%6.2fs]" % (time.time() - START), *a, flush=True)


def summarize(m):
    """One-line summary of a notification / server request focusing on ids."""
    meth = m.get("method")
    prm = m.get("params") or {}
    out = {"method": meth}
    if "id" in m:
        out["rpcId"] = m["id"]
    for k in ("threadId", "turnId", "itemId", "requestId", "approvalId"):
        if k in prm:
            out[k] = prm[k]
    if "turn" in prm:
        tr = prm["turn"]
        out["turn"] = {"id": tr.get("id"), "status": tr.get("status"), "error": tr.get("error")}
    if "item" in prm:
        it = prm["item"]
        d = {"type": it.get("type"), "id": it.get("id")}
        for k in ("clientId", "status", "command", "exitCode", "tool", "receiverThreadIds", "senderThreadId",
                  "agentThreadId", "kind", "prompt"):
            if k in it:
                d[k] = it[k]
        if it.get("type") == "userMessage":
            d["text"] = " ".join(c.get("text", "") for c in it.get("content", []))[:120]
        if it.get("type") == "agentMessage":
            d["text"] = it.get("text", "")[:120]
        out["item"] = d
    if "status" in prm and meth == "thread/status/changed":
        out["status"] = prm["status"]
    if meth in ("item/commandExecution/requestApproval", "item/fileChange/requestApproval",
                "item/permissions/requestApproval"):
        out["params"] = redact(prm)
    if meth in ("error", "warning"):
        out["params"] = redact(prm)
    if meth == "thread/started":
        th = prm.get("thread", {})
        out["thread"] = {"id": th.get("id"), "parentThreadId": th.get("parentThreadId"),
                         "agentNickname": th.get("agentNickname"), "source": th.get("source")}
    return json.dumps(out)


def pump(c, until, timeout=180, quiet=False):
    """Print events until until(msg) true. Returns (match, events)."""
    evs = []
    end = time.time() + timeout
    while time.time() < end:
        m, seen = c.wait_for(lambda _m: True, timeout=end - time.time())
        if m is None:
            break
        evs.append(m)
        if not quiet and m.get("method") not in NOISY:
            p("EVT", summarize(m))
        if until(m):
            return m, evs
    return None, evs


def is_turn_completed(m, turn_id=None):
    return m.get("method") == "turn/completed" and (turn_id is None or m["params"]["turn"]["id"] == turn_id)


def mk(args, handler=None, tag="x"):
    os.makedirs(os.path.join(SCRATCH, "logs"), exist_ok=True)
    log = os.path.join(SCRATCH, "logs", f"{tag}-{int(time.time())}.jsonl")
    c = connect(args.transport, args.sock, log_path=log, handler=handler)
    c.initialize(name="pyre-spike-" + tag, experimental=True)
    p("connected via", args.transport, "log:", log)
    return c


def new_thread(c, approval="never", sandbox="read-only"):
    r = c.request("thread/start", {"cwd": WORK, "approvalPolicy": approval, "sandbox": sandbox,
                                   "developerInstructions": "This is an automated protocol test. Be terse."})
    th = r["thread"]
    p("thread/start ->", json.dumps({"thread.id": th["id"], "sessionId": th.get("sessionId"),
                                     "status": th.get("status"), "source": th.get("source"),
                                     "path": th.get("path"), "approvalPolicy": r.get("approvalPolicy"),
                                     "sandbox": r.get("sandbox"), "model": r.get("model")}))
    return th["id"]


def text_input(t):
    return [{"type": "text", "text": t}]


def turn_start(c, tid, text, cmid=None, **kw):
    params = {"threadId": tid, "input": text_input(text)}
    if cmid:
        params["clientUserMessageId"] = cmid
    params.update(kw)
    return c.request("turn/start", params)


# ------------------------------------------------------------------ experiments
def exp_basic(a):
    c = mk(a, tag="basic")
    tid = a.thread or new_thread(c)
    if a.thread:
        r = c.request("thread/resume", {"threadId": tid, "excludeTurns": True})
        p("thread/resume ->", r["thread"]["id"], r["thread"]["status"])
    marker = "PYRE-MSG-" + uuid.uuid4().hex[:12]
    cmid = "pyre-cmid-" + uuid.uuid4().hex[:8]
    r = turn_start(c, tid, f"Reply with the single word ok. ({marker})", cmid)
    p("turn/start response ->", short(r, 600))
    turn_id = r["turn"]["id"]
    pump(c, lambda m: is_turn_completed(m, turn_id))
    rr = c.request("thread/read", {"threadId": tid, "includeTurns": True})
    for t in rr["thread"]["turns"]:
        for it in t["items"]:
            if it["type"] == "userMessage":
                p("thread/read userMessage", t["id"], it["id"], "clientId=", it.get("clientId"),
                  "marker_found=", marker in json.dumps(it))
    p("RESULT thread", tid, "turn", turn_id, "marker", marker, "cmid", cmid)
    c.close()


def exp_busy(a):
    c = mk(a, tag="busy")
    tid = a.thread or new_thread(c, approval="never", sandbox="read-only")
    r = turn_start(c, tid, "Run exactly this shell command: `sleep 8 && echo pyre-busy-done`. "
                           "Then reply with the single word done.", "cmid-A")
    turn_a = r["turn"]["id"]
    p("turn A started", turn_a)
    pump(c, lambda m: m.get("method") == "item/started" and m["params"]["item"]["type"] == "commandExecution",
         timeout=90)
    p("command running; now sending second turn/start (cmid-B) while busy")
    try:
        r2 = turn_start(c, tid, "PYRE-SECOND-START reply with the word second.", "cmid-B")
        p("2nd turn/start ->", short(r2, 800))
    except RpcError as e:
        p("2nd turn/start ERROR ->", e)
    try:
        r3 = c.request("turn/steer", {"threadId": tid, "expectedTurnId": "not-the-turn",
                                      "input": text_input("PYRE-STEER-WRONG"), "clientUserMessageId": "cmid-C"})
        p("turn/steer wrong expectedTurnId ->", short(r3))
    except RpcError as e:
        p("turn/steer wrong expectedTurnId ERROR ->", e)
    try:
        r4 = c.request("turn/steer", {"threadId": tid, "expectedTurnId": turn_a,
                                      "input": text_input("PYRE-STEER-OK also say steered."),
                                      "clientUserMessageId": "cmid-D"})
        p("turn/steer correct ->", short(r4))
    except RpcError as e:
        p("turn/steer correct ERROR ->", e)
    m, evs = pump(c, lambda m: is_turn_completed(m), timeout=180)
    p("first turn/completed seen:", summarize(m) if m else None)
    more = c.drain(15)
    for e in more:
        if e.get("method") not in NOISY:
            p("EVT(after)", summarize(e))
    # turn/steer when idle
    try:
        r5 = c.request("turn/steer", {"threadId": tid, "expectedTurnId": turn_a, "input": text_input("x")})
        p("turn/steer when idle ->", short(r5))
    except RpcError as e:
        p("turn/steer when idle ERROR ->", e)
    rr = c.request("thread/turns/list", {"threadId": tid, "itemsView": "full", "sortDirection": "asc"})
    for t in rr.get("data", []):
        p("turn", t["id"], t["status"], [(i["type"], i.get("clientId"),
                                          " ".join(x.get("text", "") for x in i.get("content", []))[:60])
                                         for i in t["items"] if i["type"] == "userMessage"])
    p("RESULT thread", tid)
    c.close()


def exp_interrupt(a):
    c = mk(a, tag="interrupt")
    tid = a.thread or new_thread(c, approval="never", sandbox="read-only")
    r = turn_start(c, tid, "Run exactly this shell command: `sleep 30 && echo pyre-int`. Then reply done.")
    turn = r["turn"]["id"]
    pump(c, lambda m: m.get("method") == "item/started" and m["params"]["item"]["type"] == "commandExecution",
         timeout=90)
    t1 = time.time()
    p("turn/interrupt ->", short(c.request("turn/interrupt", {"threadId": tid, "turnId": turn})))
    m, _ = pump(c, lambda m: is_turn_completed(m, turn), timeout=60)
    p("turn completed after interrupt in %.2fs:" % (time.time() - t1), summarize(m) if m else None)
    try:
        p("turn/interrupt again ->", short(c.request("turn/interrupt", {"threadId": tid, "turnId": turn})))
    except RpcError as e:
        p("turn/interrupt again ERROR ->", e)
    p("RESULT thread", tid, "turn", turn)
    c.close()


def exp_approval(a):
    captured = []

    def handler(cl, msg):
        captured.append(msg)
        p("SERVER REQUEST", summarize(msg))
        if msg["method"] == "item/commandExecution/requestApproval":
            res = {"decision": a.decision}
            cl.respond(msg["id"], res)
            p("responded", res, "to rpc id", msg["id"])
            if a.double:
                time.sleep(0.5)
                cl.respond(msg["id"], {"decision": "decline"})
                p("responded AGAIN (decline) to rpc id", msg["id"])
            return None
        return None  # leave others unanswered

    c = mk(a, handler=handler, tag="approval")
    tid = a.thread or new_thread(c, approval="untrusted", sandbox="read-only")
    r = turn_start(c, tid, "Run exactly this shell command: `echo pyre-approval-test > pyre-approval.txt && "
                           "cat pyre-approval.txt`. Then reply with its output.",
                   approvalPolicy="untrusted")
    turn = r["turn"]["id"]
    pump(c, lambda m: is_turn_completed(m, turn), timeout=180)
    if captured:
        rid = captured[0]["id"]
        p("responding to stale approval rpc id", rid, "after turn completed")
        c.respond(rid, {"decision": "accept"})
        for e in c.drain(3):
            p("EVT(after stale)", summarize(e))
    p("RESULT thread", tid, "turn", turn, "approvals", len(captured))
    c.close()


def exp_crash(a):
    c = mk(a, tag="crash")
    tid = a.thread or new_thread(c)
    marker = "PYRE-MSG-" + uuid.uuid4().hex[:12]
    p("CRASH thread", tid, "marker", marker)
    c.send_request("turn/start", {"threadId": tid, "clientUserMessageId": "cmid-" + marker,
                                  "input": text_input(f"Reply with the single word ok. ({marker})")})
    p("sent turn/start; exiting hard before reading the response")
    sys.stdout.flush()
    os._exit(0)


def exp_reconcile(a):
    c = mk(a, tag="reconcile")
    lst = c.request("thread/loaded/list", {})
    p("thread/loaded/list ->", short(lst, 500))
    rr = c.request("thread/read", {"threadId": a.thread, "includeTurns": True})
    th = rr["thread"]
    p("thread/read status", th["status"], "turns", len(th["turns"]))
    for t in th["turns"]:
        for it in t["items"]:
            if it["type"] == "userMessage" and a.marker in json.dumps(it):
                p("FOUND marker in turn", t["id"], "status", t["status"], "item", it["id"], "clientId", it.get("clientId"))
    tl = c.request("thread/list", {"searchTerm": a.marker[:20], "limit": 5, "sourceKinds": []})
    p("thread/list searchTerm ->", [(x["id"], x.get("preview", "")[:60]) for x in tl.get("data", [])])
    c.close()


def exp_resume(a):
    c = mk(a, tag="resume")
    r = c.request("thread/resume", {"threadId": a.thread})
    th = r["thread"]
    p("thread/resume -> id", th["id"], "same=", th["id"] == a.thread, "status", th["status"], "turns", len(th["turns"]),
      "path", th.get("path"))
    marker = "PYRE-MSG-" + uuid.uuid4().hex[:12]
    r2 = turn_start(c, a.thread, f"Reply with the single word ok. ({marker})")
    pump(c, lambda m: is_turn_completed(m, r2["turn"]["id"]))
    f = c.request("thread/fork", {"threadId": a.thread, "excludeTurns": True})
    p("thread/fork -> id", f["thread"]["id"], "forkedFromId", f["thread"].get("forkedFromId"))
    try:
        r3 = c.request("thread/resume", {"threadId": "019a0000-0000-7000-8000-000000000000"})
        p("resume bogus ->", short(r3))
    except RpcError as e:
        p("resume bogus ERROR ->", e)
    c.close()


def exp_subagent(a):
    c = mk(a, tag="subagent")
    tid = a.thread or new_thread(c, approval="never", sandbox="read-only")
    r = turn_start(c, tid, "Use your sub-agent / spawn_agent tool to delegate this tiny task to one sub-agent: "
                           "'reply with the single word ok'. Wait for it, then reply with what it said. "
                           "Do not run shell commands.")
    turn = r["turn"]["id"]
    pump(c, lambda m: is_turn_completed(m, turn), timeout=300)
    lst = c.request("thread/loaded/list", {})
    p("thread/loaded/list ->", short(lst, 800))
    p("RESULT thread", tid)
    c.close()


def exp_watch(a):
    c = mk(a, tag="watch")
    r = c.request("thread/resume", {"threadId": a.thread, "excludeTurns": True})
    p("subscribed via thread/resume", r["thread"]["id"], r["thread"]["status"])
    pump(c, lambda m: False, timeout=a.secs)
    c.close()


def exp_say(a):
    c = mk(a, tag="say")
    r = c.request("thread/resume", {"threadId": a.thread, "excludeTurns": True})
    p("resumed", r["thread"]["id"], r["thread"]["status"])
    r2 = turn_start(c, a.thread, a.text, "cmid-say-" + uuid.uuid4().hex[:6])
    p("turn/start ->", r2["turn"]["id"])
    pump(c, lambda m: is_turn_completed(m, r2["turn"]["id"]))
    c.close()


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("cmd")
    ap.add_argument("--transport", default="unix", choices=["unix", "stdio", "proxy"])
    ap.add_argument("--sock", default=os.path.join(SCRATCH, "as.sock"))
    ap.add_argument("--thread")
    ap.add_argument("--marker")
    ap.add_argument("--text")
    ap.add_argument("--secs", type=int, default=60)
    ap.add_argument("--decision", default="accept")
    ap.add_argument("--double", action="store_true")
    a = ap.parse_args()
    globals()["exp_" + a.cmd](a)


if __name__ == "__main__":
    main()
