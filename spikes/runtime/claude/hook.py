#!/usr/bin/env python3
"""Pyre spike hook recorder.

Configured per-session through `claude --settings <scratch>/settings.json`; never
installed into ~/.claude. Appends one JSON line per hook invocation to
$PYRE_SPIKE_DIR/events.jsonl. Keeps only the fields Pyre needs (no full tool
payloads) so evidence files contain synthetic prompts only.

Optional behaviour for the Stop-hook injection experiment: if a file
$PYRE_SPIKE_DIR/stop-inject.txt exists when a Stop hook fires, its contents are
returned as {"decision":"block","reason":...} and the file is renamed to
stop-inject.consumed so injection happens at most once (idempotent claim).
"""
import json
import os
import sys
import time

d = os.environ.get("PYRE_SPIKE_DIR")
if not d:
    sys.exit(0)
try:
    ev = json.load(sys.stdin)
except Exception:
    ev = {}

name = ev.get("hook_event_name", "?")
rec = {
    "t": round(time.time(), 3),
    "event": name,
    "session_id": ev.get("session_id"),
    "transcript_path": ev.get("transcript_path"),
    "keys": sorted(ev.keys()),
}
for k in ("prompt", "notification_type", "message", "tool_name", "source",
          "reason", "stop_hook_active", "agent_id", "agent_type",
          "permission_mode", "agent_transcript_path"):
    if k in ev:
        v = ev[k]
        if isinstance(v, str) and len(v) > 300:
            v = v[:300] + "..."
        rec[k] = v
if name in ("PreToolUse", "PermissionRequest") and isinstance(ev.get("tool_input"), dict):
    ti = ev["tool_input"]
    rec["tool_input_keys"] = sorted(ti.keys())
    if "command" in ti:
        rec["command"] = str(ti["command"])[:200]
    if "subagent_type" in ti:
        rec["subagent_type"] = ti["subagent_type"]

out = None
if name == "Stop":
    inj = os.path.join(d, "stop-inject.txt")
    claimed = os.path.join(d, "stop-inject.consumed")
    try:
        os.rename(inj, claimed)  # atomic claim: only one Stop consumes it
        with open(claimed) as f:
            reason = f.read().strip()
        out = {"decision": "block", "reason": reason}
        rec["injected"] = reason
    except FileNotFoundError:
        pass

if name == "PermissionRequest" and os.path.exists(os.path.join(d, "approval-broker")):
    # Structured approval broker: publish the pending request, then wait for an
    # answer file written by the controller. No answer -> fall back to the TUI dialog.
    rid = ev.get("tool_use_id") or f"perm-{int(time.time()*1000)}"
    rec["request_id"] = rid
    pend = os.path.join(d, "pending")
    ans = os.path.join(d, "answers")
    os.makedirs(pend, exist_ok=True)
    os.makedirs(ans, exist_ok=True)
    with open(os.path.join(pend, rid + ".json"), "w") as f:
        json.dump({"request_id": rid, "session_id": ev.get("session_id"),
                   "tool_name": ev.get("tool_name"), "command": rec.get("command")}, f)
    with open(os.path.join(d, "events.jsonl"), "a") as f:
        f.write(json.dumps(rec) + "\n")
    import signal

    def _withdrawn(signum, _frame):
        # Claude kills the pending hook when the dialog is answered in the TUI
        # (or the turn is interrupted). Record that the structured request is gone.
        with open(os.path.join(d, "events.jsonl"), "a") as f:
            f.write(json.dumps({"t": round(time.time(), 3), "event": "PyreApprovalWithdrawn",
                                "request_id": rid, "signal": signum}) + "\n")
        sys.exit(0)

    for sig in (signal.SIGTERM, signal.SIGINT, signal.SIGHUP):
        signal.signal(sig, _withdrawn)
    deadline = time.time() + float(os.environ.get("PYRE_APPROVAL_WAIT", "45"))
    while time.time() < deadline:
        ap = os.path.join(ans, rid + ".json")
        if os.path.exists(ap):
            a = json.load(open(ap))
            os.rename(ap, ap + ".consumed")
            beh = "allow" if a.get("allow") else "deny"
            dec = {"behavior": beh}
            if beh == "deny":
                dec["message"] = a.get("message", "denied by Pyre approval broker")
            with open(os.path.join(d, "events.jsonl"), "a") as f:
                f.write(json.dumps({"t": round(time.time(), 3), "event": "PyreApprovalAnswered",
                                    "request_id": rid, "behavior": beh}) + "\n")
            print(json.dumps({"hookSpecificOutput": {"hookEventName": "PermissionRequest", "decision": dec}}))
            sys.exit(0)
        time.sleep(0.3)
    sys.exit(0)

with open(os.path.join(d, "events.jsonl"), "a") as f:
    f.write(json.dumps(rec) + "\n")
if out:
    print(json.dumps(out))
sys.exit(0)
