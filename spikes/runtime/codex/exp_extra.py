#!/usr/bin/env python3
"""Extra probes: clientUserMessageId dedupe; long turn surviving controller exit; approval with no controller."""
import json, os, sys, time, uuid
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from experiments import mk, new_thread, turn_start, pump, is_turn_completed, p, summarize, text_input  # noqa
from asclient import RpcError, short
import argparse

ap = argparse.ArgumentParser(); ap.add_argument("cmd"); ap.add_argument("--transport", default="unix")
ap.add_argument("--sock", default="/tmp/pyre-wp01/codex/as.sock"); ap.add_argument("--thread")
a = ap.parse_args()

if a.cmd == "dedupe":
    c = mk(a, tag="dedupe"); tid = new_thread(c)
    cm = "dup-" + uuid.uuid4().hex[:6]
    r1 = turn_start(c, tid, "Reply with the single word one.", cm)
    pump(c, lambda m: is_turn_completed(m, r1["turn"]["id"]), quiet=True)
    try:
        r2 = turn_start(c, tid, "Reply with the single word two.", cm)
        p("same clientUserMessageId again ->", short(r2), "same turn?", r2["turn"]["id"] == r1["turn"]["id"])
        pump(c, lambda m: is_turn_completed(m), quiet=True)
    except RpcError as e:
        p("same cmid ERROR", e)
    rr = c.request("thread/turns/list", {"threadId": tid, "itemsView": "full", "sortDirection": "asc"})
    for t in rr["data"]:
        p("turn", t["id"], t["status"], [(i.get("clientId"), i["content"][0]["text"]) for i in t["items"] if i["type"] == "userMessage"])
    c.close()
elif a.cmd == "longturn-start":
    c = mk(a, tag="longturn"); tid = new_thread(c)
    r = turn_start(c, tid, "Run exactly this shell command: `sleep 45 && echo pyre-long-done`. Then reply with the single word finished.", "cmid-long")
    pump(c, lambda m: m.get("method") == "item/started" and m["params"]["item"]["type"] == "commandExecution", quiet=True)
    p("LONG thread", tid, "turn", r["turn"]["id"], "- command running; controller exits now")
    c.close()
elif a.cmd == "approval-orphan-start":
    c = mk(a, tag="orphan"); tid = new_thread(c, approval="untrusted")
    r = turn_start(c, tid, "Run exactly this shell command: `echo pyre-orphan > orphan.txt`. Then reply done.")
    m, _ = pump(c, lambda m: m.get("method") == "item/commandExecution/requestApproval", quiet=True)
    p("ORPHAN thread", tid, "turn", r["turn"]["id"], "approval rpc id", m and m["id"], "- controller exits without answering")
    c.close()
elif a.cmd == "status":
    c = mk(a, tag="status")
    rr = c.request("thread/read", {"threadId": a.thread, "includeTurns": True})
    th = rr["thread"]; p("status", th["status"])
    for t in th["turns"]:
        p("turn", t["id"], t["status"], t.get("error"), [(i["type"], i.get("status"), (i.get("text") or i.get("aggregatedOutput") or "")[:60]) for i in t["items"]])
    c.close()
elif a.cmd == "reattach-approval":
    # resume a thread with a pending approval: is the approval request re-sent to the new connection?
    got = []
    def h(cl, msg):
        got.append(msg); p("SERVER REQUEST on new conn", summarize(msg)); return {"decision": "accept"}
    c = mk(a, handler=h, tag="reattach")
    r = c.request("thread/resume", {"threadId": a.thread})
    p("resume status", r["thread"]["status"])
    pump(c, lambda m: is_turn_completed(m), timeout=40)
    p("approval requests received after resume:", len(got))
    c.close()
if a.cmd == "approval-shared":
    got = []
    def h(cl, msg):
        got.append(msg); p("SERVER REQUEST (python, NOT answering)", summarize(msg)); return None
    c = mk(a, handler=h, tag="appshared")
    c.request("thread/resume", {"threadId": a.thread, "excludeTurns": True})
    r = turn_start(c, a.thread, "Run exactly this shell command: `echo pyre-shared-approval > shared.txt`. Then reply done.",
                   "cmid-shared", approvalPolicy="untrusted")
    pump(c, lambda m: is_turn_completed(m, r["turn"]["id"]), timeout=90)
    p("python approval requests seen:", len(got))
    # restore policy for later turns
    c.close()
if a.cmd == "meta":
    c = mk(a, tag="meta")
    th = c.request("thread/read", {"threadId": a.thread})["thread"]
    p("meta", {k: th.get(k) for k in ("id", "parentThreadId", "agentNickname", "agentRole", "sessionId", "forkedFromId", "source", "status", "ephemeral")})
    c.close()
if a.cmd == "queue-bg":
    c = mk(a, tag="queuebg"); tid = new_thread(c)
    r = turn_start(c, tid, "Run exactly this shell command: `sleep 20 && echo pyre-bg-done`. Then reply with the single word first.", "cmid-first")
    turn = r["turn"]["id"]
    pump(c, lambda m: m.get("method") == "item/started" and m["params"]["item"]["type"] == "commandExecution", quiet=True)
    q = c.request("thread/queue/add", {"threadId": tid, "clientUserMessageId": "cmid-queued", "input": text_input("PYRE-QUEUED reply with the single word queued.")})
    p("thread/queue/add ->", short(q))
    p("thread/queue/list ->", short(c.request("thread/queue/list", {"threadId": tid})))
    p("turn/interrupt ->", c.request("turn/interrupt", {"threadId": tid, "turnId": turn}))
    p("backgroundTerminals/list ->", short(c.request("thread/backgroundTerminals/list", {"threadId": tid}), 700))
    try:
        p("backgroundTerminals/clean ->", short(c.request("thread/backgroundTerminals/clean", {"threadId": tid}, timeout=10)))
    except Exception as e:
        p("clean ERR", e)
    p("backgroundTerminals/list after clean ->", short(c.request("thread/backgroundTerminals/list", {"threadId": tid}), 700))
    m, evs = pump(c, lambda m: False, timeout=25)
    p("thread/queue/list end ->", short(c.request("thread/queue/list", {"threadId": tid})))
    rr = c.request("thread/turns/list", {"threadId": tid, "itemsView": "full", "sortDirection": "asc"})
    for t in rr["data"]:
        p("turn", t["id"], t["status"], [(i["type"], i.get("clientId"), i.get("status")) for i in t["items"] if i["type"] in ("userMessage", "commandExecution")])
    c.close()
