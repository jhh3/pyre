#!/usr/bin/env python3
"""Minimal "Pyre node controller" for the Claude runtime spike.

Everything lives under one scratch directory ($PYRE_SPIKE_DIR or --dir):
  tmux.sock        isolated tmux server (never the user's default server)
  settings.json    per-session hook settings passed with `claude --settings`
  run.json         run record: session uuid, pane pid, launch argv
  events.jsonl     hook events written by hook.py
  deliveries.jsonl delivery attempt journal (claim before dispatch)

Subcommands:
  ensure  [--extra ARGS]  launch Claude in tmux unless a live run already exists
  state                   print derived run state from hook events
  deliver ID TEXT [--mode paste|stop-inject|channel] [--force]
  reconcile ID            look for runtime evidence of message ID
  capture                 print the visible pane (for evidence; redact before saving)
  clients                 list attached tmux clients (input-lease signal)
  stop                    send /exit-equivalent: C-c twice, then kill the tmux server

The controller holds no long-lived state in memory; kill -9 of this process at
any time leaves the harness running inside the tmux server.
"""
import argparse
import json
import os
import shlex
import shutil
import subprocess
import sys
import time
import uuid

HERE = os.path.dirname(os.path.abspath(__file__))
TMUX = shutil.which("tmux") or "/opt/homebrew/bin/tmux"
CLAUDE = os.path.expanduser("~/.local/bin/claude")
HOOK_EVENTS = ["SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse",
               "PermissionRequest", "Notification", "Stop", "StopFailure",
               "SubagentStart", "SubagentStop", "SessionEnd", "PreCompact"]


def tmux(d, *args, check=True, capture=True, inp=None):
    cmd = [TMUX, "-S", os.path.join(d, "tmux.sock")] + list(args)
    r = subprocess.run(cmd, capture_output=capture, text=True, input=inp)
    if check and r.returncode != 0:
        raise SystemExit(f"tmux {' '.join(args)} failed: {r.stderr.strip()}")
    return r


def write_settings(d):
    hook_cmd = f"python3 {shlex.quote(os.path.join(HERE, 'hook.py'))}"
    hooks = {}
    for ev in HOOK_EVENTS:
        entry = {"hooks": [{"type": "command", "command": hook_cmd, "timeout": 120 if ev == "PermissionRequest" else 10}]}
        if ev in ("PreToolUse", "PostToolUse", "PermissionRequest"):
            entry["matcher"] = "*"
        hooks[ev] = [entry]
    p = os.path.join(d, "settings.json")
    with open(p, "w") as f:
        json.dump({"hooks": hooks}, f, indent=2)
    return p


def load_run(d):
    try:
        with open(os.path.join(d, "run.json")) as f:
            return json.load(f)
    except FileNotFoundError:
        return None


def pid_alive(pid):
    try:
        os.kill(pid, 0)
        return True
    except ProcessLookupError:
        return False
    except PermissionError:
        return True


def live_run(d):
    run = load_run(d)
    if not run:
        return None
    r = tmux(d, "list-panes", "-t", "pyre", "-F", "#{pane_pid} #{pane_dead}", check=False)
    if r.returncode != 0:
        return None
    pid, dead = r.stdout.split()[:2]
    if int(pid) != run["pane_pid"] or dead == "1" or not pid_alive(int(pid)):
        return None
    return run


def cmd_ensure(a):
    d = a.dir
    run = live_run(d)
    if run:
        print(json.dumps({"action": "reattached", **run}))
        return
    settings = write_settings(d)
    sid = a.session_id or str(uuid.uuid4())
    argv = [CLAUDE, "--settings", settings]
    if a.resume:
        argv += ["--resume", a.resume]
    else:
        argv += ["--session-id", sid]
    argv += shlex.split(a.extra or "")
    work = a.cwd or os.path.join(d, "work")
    os.makedirs(work, exist_ok=True)
    env_prefix = f"PYRE_SPIKE_DIR={shlex.quote(d)} "
    shell_cmd = env_prefix + " ".join(shlex.quote(x) for x in argv)
    # remain-on-exit lets us observe exit status instead of losing the pane.
    tmux(d, "-f", "/dev/null", "new-session", "-d", "-s", "pyre", "-x", "160", "-y", "45",
         "-c", work, shell_cmd)
    tmux(d, "set-option", "-w", "-t", "pyre", "remain-on-exit", "on")
    pid = int(tmux(d, "list-panes", "-t", "pyre", "-F", "#{pane_pid}").stdout.split()[0])
    run = {"run_id": str(uuid.uuid4()), "session_id": None if a.resume else sid,
           "resume_from": a.resume, "pane_pid": pid, "cwd": work, "argv": argv,
           "started_at": time.time()}
    with open(os.path.join(d, "run.json"), "w") as f:
        json.dump(run, f, indent=2)
    print(json.dumps({"action": "launched", **run}))


def events(d):
    p = os.path.join(d, "events.jsonl")
    if not os.path.exists(p):
        return []
    with open(p) as f:
        return [json.loads(l) for l in f if l.strip()]


def derive_state(evs, session_id=None):
    state, reason = "starting", "no hook events yet"
    for e in evs:
        if session_id and e.get("session_id") not in (None, session_id):
            continue
        n = e["event"]
        if n == "SessionStart":
            state, reason = "waiting_for_input", f"SessionStart({e.get('source')})"
        elif n == "UserPromptSubmit":
            state, reason = "running", "UserPromptSubmit"
        elif n in ("PreToolUse", "PostToolUse", "SubagentStart", "SubagentStop"):
            if state != "exited":
                state, reason = "running", n
        elif n == "PermissionRequest":
            state, reason = "waiting_for_approval", f"PermissionRequest({e.get('tool_name')})"
        elif n == "Notification":
            nt = e.get("notification_type")
            if nt == "permission_prompt":
                state, reason = "waiting_for_approval", "Notification(permission_prompt)"
            elif nt == "idle_prompt":
                state, reason = "waiting_for_input", "Notification(idle_prompt)"
        elif n in ("Stop", "StopFailure"):
            state, reason = "waiting_for_input", n
        elif n == "SessionEnd":
            state, reason = "exited", f"SessionEnd({e.get('reason')})"
    return state, reason


def cmd_state(a):
    run = load_run(a.dir)
    evs = events(a.dir)
    sid = None
    for e in evs:
        if e["event"] == "SessionStart":
            sid = e.get("session_id")
    state, reason = derive_state(evs, sid)
    alive = live_run(a.dir) is not None
    if not alive and run:
        state, reason = "exited", "pane dead or missing"
    print(json.dumps({"state": state, "reason": reason, "hook_session_id": sid,
                      "pane_alive": alive, "events": len(evs)}))


def journal(d, rec):
    p = os.path.join(d, "deliveries.jsonl")
    with open(p, "a") as f:
        f.write(json.dumps({"t": round(time.time(), 3), **rec}) + "\n")
        f.flush()
        os.fsync(f.fileno())


def marker(mid):
    return f"[pyre-msg:{mid}]"


def find_evidence(d, mid):
    m = marker(mid)
    out = {"hook": None, "transcript": None}
    tpaths = set()
    for e in events(d):
        if e.get("transcript_path"):
            tpaths.add(e["transcript_path"])
        if e["event"] == "UserPromptSubmit" and m in (e.get("prompt") or ""):
            out["hook"] = {"t": e["t"], "session_id": e.get("session_id")}
    for tp in tpaths:
        try:
            with open(tp) as f:
                for line in f:
                    if m in line:
                        rec = json.loads(line)
                        out["transcript"] = {"path_suffix": os.path.basename(tp),
                                             "type": rec.get("type"),
                                             "uuid": rec.get("uuid"),
                                             "origin": rec.get("origin"),
                                             "isMeta": rec.get("isMeta")}
                        break
        except FileNotFoundError:
            pass
    return out


def cmd_deliver(a):
    d, mid = a.dir, a.message_id
    run = live_run(d)
    if not run:
        raise SystemExit("no live run")
    ev = find_evidence(d, mid)
    if ev["hook"] or ev["transcript"]:
        print(json.dumps({"result": "accepted", "dedup": True, "evidence": ev}))
        return
    state, reason = derive_state(events(d))
    attempt = str(uuid.uuid4())
    text = f"{a.text} {marker(mid)}"
    if a.mode == "paste":
        clients = tmux(d, "list-clients", "-t", "pyre", "-F", "#{client_tty}", check=False).stdout.split()
        if state != "waiting_for_input" and not a.force:
            journal(d, {"message_id": mid, "attempt": attempt, "result": "queued", "why": state})
            print(json.dumps({"result": "queued", "state": state, "reason": reason}))
            return
        if clients and not a.force:
            journal(d, {"message_id": mid, "attempt": attempt, "result": "queued", "why": "human attached (input lease)"})
            print(json.dumps({"result": "queued", "why": "attached clients", "clients": len(clients)}))
            return
        journal(d, {"message_id": mid, "attempt": attempt, "run": run["run_id"], "result": "attempting", "mode": "paste"})
        buf = f"pyre-{mid}"
        tmux(d, "load-buffer", "-b", buf, "-", inp=text)
        tmux(d, "paste-buffer", "-p", "-d", "-b", buf, "-t", "pyre")
        time.sleep(0.4)
        if a.crash_before_enter:
            print(json.dumps({"result": "crashed-before-enter (simulated)"}))
            os._exit(9)
        tmux(d, "send-keys", "-t", "pyre", "Enter")
        if a.crash_after_enter:
            os._exit(9)
    elif a.mode == "stop-inject":
        journal(d, {"message_id": mid, "attempt": attempt, "run": run["run_id"], "result": "attempting", "mode": "stop-inject"})
        tmp = os.path.join(d, "stop-inject.tmp")
        with open(tmp, "w") as f:
            f.write(text)
        os.rename(tmp, os.path.join(d, "stop-inject.txt"))
    elif a.mode == "channel":
        journal(d, {"message_id": mid, "attempt": attempt, "run": run["run_id"], "result": "attempting", "mode": "channel"})
        inbox = os.path.join(d, "channel-inbox")
        os.makedirs(inbox, exist_ok=True)
        tmp = os.path.join(inbox, f".{mid}.tmp")
        with open(tmp, "w") as f:
            json.dump({"message_id": mid, "content": text}, f)
        os.rename(tmp, os.path.join(inbox, f"{mid}.json"))
    deadline = time.time() + a.timeout
    while time.time() < deadline:
        ev = find_evidence(d, mid)
        if ev["hook"] or ev["transcript"]:
            journal(d, {"message_id": mid, "attempt": attempt, "result": "accepted", "evidence": ev})
            print(json.dumps({"result": "accepted", "evidence": ev}))
            return
        time.sleep(0.5)
    journal(d, {"message_id": mid, "attempt": attempt, "result": "indeterminate"})
    print(json.dumps({"result": "indeterminate", "state": derive_state(events(d))}))


def cmd_reconcile(a):
    d, mid = a.dir, a.message_id
    attempts = []
    p = os.path.join(d, "deliveries.jsonl")
    if os.path.exists(p):
        attempts = [json.loads(l) for l in open(p) if mid in l]
    ev = find_evidence(d, mid)
    last = attempts[-1]["result"] if attempts else None
    if ev["hook"] or ev["transcript"]:
        verdict = "accepted"
    elif last == "attempting":
        verdict = "indeterminate"  # dispatch may have happened; never auto-replay
    elif last in (None, "queued"):
        verdict = "unattempted"
    else:
        verdict = last
    print(json.dumps({"message_id": mid, "verdict": verdict, "journal": attempts, "evidence": ev}))


def cmd_capture(a):
    print(tmux(a.dir, "capture-pane", "-p", "-t", "pyre").stdout)


def cmd_accept_dialogs(a):
    """Drive the first-run dialogs a managed launch can hit (workspace trust,
    development-channel consent). Evidence that a managed launcher must either
    pre-provision trust or drive these screens deliberately."""
    for _ in range(int(a.wait)):
        screen = tmux(a.dir, "capture-pane", "-p", "-t", "pyre").stdout
        if "trust this folder" in screen:
            tmux(a.dir, "send-keys", "-t", "pyre", "Down")
            time.sleep(0.3)
            tmux(a.dir, "send-keys", "-t", "pyre", "Enter")
            print("accepted workspace trust dialog")
        elif "I am using this for local development" in screen:
            tmux(a.dir, "send-keys", "-t", "pyre", "Enter")
            print("accepted development channel consent")
        elif "? for shortcuts" in screen or "for agents" in screen:
            print("prompt ready")
            return
        time.sleep(1)
    print("timed out waiting for prompt")


def cmd_clients(a):
    print(tmux(a.dir, "list-clients", "-F", "#{client_tty} #{client_termname}", check=False).stdout)


def cmd_pending(a):
    pend = os.path.join(a.dir, "pending")
    ans = os.path.join(a.dir, "answers")
    for fn in sorted(os.listdir(pend)) if os.path.isdir(pend) else []:
        rid = fn[:-5]
        done = os.path.exists(os.path.join(ans, rid + ".json.consumed"))
        print(json.dumps({**json.load(open(os.path.join(pend, fn))), "resolved": done}))


def cmd_answer(a):
    ans = os.path.join(a.dir, "answers")
    os.makedirs(ans, exist_ok=True)
    if not os.path.exists(os.path.join(a.dir, "pending", a.request_id + ".json")):
        raise SystemExit("unknown request id")
    if os.path.exists(os.path.join(ans, a.request_id + ".json.consumed")):
        print(json.dumps({"result": "already_resolved"}))
        return
    tmp = os.path.join(ans, "." + a.request_id)
    with open(tmp, "w") as f:
        json.dump({"allow": a.decision == "allow"}, f)
    os.rename(tmp, os.path.join(ans, a.request_id + ".json"))
    print(json.dumps({"result": "answer_written", "request_id": a.request_id}))


def cmd_stop(a):
    tmux(a.dir, "kill-server", check=False)
    print("killed isolated tmux server")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--dir", default=os.environ.get("PYRE_SPIKE_DIR"))
    sp = ap.add_subparsers(dest="cmd", required=True)
    e = sp.add_parser("ensure")
    e.add_argument("--session-id")
    e.add_argument("--resume")
    e.add_argument("--cwd")
    e.add_argument("--extra", default="")
    sp.add_parser("state")
    dl = sp.add_parser("deliver")
    dl.add_argument("message_id")
    dl.add_argument("text")
    dl.add_argument("--mode", default="paste", choices=["paste", "stop-inject", "channel"])
    dl.add_argument("--force", action="store_true")
    dl.add_argument("--timeout", type=float, default=20)
    dl.add_argument("--crash-before-enter", action="store_true")
    dl.add_argument("--crash-after-enter", action="store_true")
    rc = sp.add_parser("reconcile")
    rc.add_argument("message_id")
    sp.add_parser("capture")
    sp.add_parser("clients")
    sp.add_parser("stop")
    sp.add_parser("pending")
    ad = sp.add_parser("accept-dialogs")
    ad.add_argument("--wait", default=30)
    an = sp.add_parser("answer")
    an.add_argument("request_id")
    an.add_argument("decision", choices=["allow", "deny"])
    a = ap.parse_args()
    if not a.dir:
        raise SystemExit("--dir or PYRE_SPIKE_DIR required")
    a.dir = os.path.abspath(a.dir)
    os.makedirs(a.dir, exist_ok=True)
    {"ensure": cmd_ensure, "state": cmd_state, "deliver": cmd_deliver,
     "reconcile": cmd_reconcile, "capture": cmd_capture, "clients": cmd_clients,
     "stop": cmd_stop, "pending": cmd_pending, "answer": cmd_answer,
     "accept-dialogs": cmd_accept_dialogs}[a.cmd](a)


if __name__ == "__main__":
    main()
