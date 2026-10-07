#!/usr/bin/env python3
"""Measure the wall time a Claude Code hook adds per invocation (A20).

Claude Code runs each hook command through a shell with the event JSON on
stdin and waits for it. This replays real payloads (captured by Pyre's own
hook in a real Claude run's events.jsonl) through the exact hook commands,
the same way: /bin/sh -c <command>, payload on stdin, and reports wall time
and child CPU per event (p50/p95/max).

  pyre mode: hookbench.py pyre --state RUN_STATE_COPY [--n 100]
      Uses RUN_STATE_COPY/pyre-hooks.json (commands rewritten from the original
      state dir to the copy) and payloads from RUN_STATE_COPY/events.jsonl.
      broker.json is removed from the copy so PermissionRequest is measured
      as the plain capture path (an approval wait is user time, not overhead).
      --replace OLD=NEW rewrites the commands (replay on another host).
  once mode: hookbench.py once --n 3 -- CMD...   wall/CPU/peak RSS of a command
  hive mode: hookbench.py hive --hooks HOOKS_JSON --plugin-root DIR --payloads EVENTS_JSONL
             [--transcript FILE] [--n 20]
      Runs each Hive hook command for the events present (Claude runs the
      hooks of one event in parallel, so the event cost is the slowest one).

Output: one JSON object on stdout. Payloads never leave the machine.
"""
import argparse
import json
import os
import resource
import shutil
import statistics
import subprocess
import sys
import time


def pct(vals, q):
    v = sorted(vals)
    if not v:
        return None
    k = (len(v) - 1) * q
    lo, hi = int(k), min(int(k) + 1, len(v) - 1)
    return round(v[lo] + (v[hi] - v[lo]) * (k - lo), 2)


def run_once(cmd, stdin, env, cwd):
    r0 = resource.getrusage(resource.RUSAGE_CHILDREN)
    t0 = time.perf_counter()
    p = subprocess.run(["/bin/sh", "-c", cmd], input=stdin, stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=env, cwd=cwd, timeout=120)
    t1 = time.perf_counter()
    r1 = resource.getrusage(resource.RUSAGE_CHILDREN)
    cpu = (r1.ru_utime - r0.ru_utime + r1.ru_stime - r0.ru_stime) * 1000
    return (t1 - t0) * 1000, cpu, p.returncode, r1.ru_maxrss


def summarize(walls, cpus, rcs):
    return {"n": len(walls), "wall_ms_p50": pct(walls, 0.5), "wall_ms_p95": pct(walls, 0.95), "wall_ms_max": round(max(walls), 2),
            "wall_ms_mean": round(statistics.mean(walls), 2), "cpu_ms_mean": round(statistics.mean(cpus), 2),
            "nonzero_exits": sum(1 for r in rcs if r != 0)}


def latest_payloads(events_path):
    by = {}
    with open(events_path) as f:
        for line in f:
            try:
                rec = json.loads(line)
            except ValueError:
                continue
            if rec.get("payload"):
                by[rec["event"]] = rec["payload"]
    return by


def maxrss_mb(v):
    return round(v / (1024 * 1024) if sys.platform == "darwin" else v / 1024, 1)


def mode_pyre(a):
    st = a.state
    hooks = json.load(open(os.path.join(st, "pyre-hooks.json")))["hooks"]
    try:
        os.remove(os.path.join(st, "broker.json"))
    except FileNotFoundError:
        pass
    payloads = latest_payloads(os.path.join(st, "events.jsonl"))
    out = {"mode": "pyre", "platform": sys.platform, "events": {}}
    env = dict(os.environ)
    peak = 0
    for ev, matchers in hooks.items():
        if ev not in payloads:
            continue
        cmd = matchers[0]["hooks"][0]["command"]
        if a.orig_state:
            cmd = cmd.replace(a.orig_state, st)
        for r in a.replace:
            old, new = r.split("=", 1)
            cmd = cmd.replace(old, new)
        stdin = json.dumps(payloads[ev]).encode()
        walls, cpus, rcs = [], [], []
        run_once(cmd, stdin, env, st)  # warm the page cache
        for _ in range(a.n):
            w, c, rc, peak = run_once(cmd, stdin, env, st)
            walls.append(w)
            cpus.append(c)
            rcs.append(rc)
        out["events"][ev] = summarize(walls, cpus, rcs)
        out["events"][ev]["payload_bytes"] = len(stdin)
    out["children_peak_rss_mb"] = maxrss_mb(peak)
    out["events_jsonl_bytes_after"] = os.path.getsize(os.path.join(st, "events.jsonl"))
    print(json.dumps(out, indent=2))


def mode_hive(a):
    hooks = json.load(open(a.hooks))["hooks"]
    payloads = latest_payloads(a.payloads)
    env = dict(os.environ, CLAUDE_PLUGIN_ROOT=a.plugin_root)
    out = {"mode": "hive", "platform": sys.platform, "events": {}}
    peak = 0
    for ev, matchers in hooks.items():
        if ev not in payloads:
            continue
        p = dict(payloads[ev])
        if a.transcript:
            p["transcript_path"] = a.transcript
        p["cwd"] = os.getcwd()
        stdin = json.dumps(p).encode()
        cmds = [h["command"].replace("${CLAUDE_PLUGIN_ROOT}", a.plugin_root) for m in matchers for h in m["hooks"]]
        per = {}
        event_walls = []
        for _ in range(a.n):
            slowest = 0.0
            for i, cmd in enumerate(cmds):
                w, c, rc, peak = run_once(cmd, stdin, env, os.getcwd())
                d = per.setdefault(i, {"cmd": cmd.split("/")[-1][:60], "w": [], "c": [], "rc": []})
                d["w"].append(w)
                d["c"].append(c)
                d["rc"].append(rc)
                slowest = max(slowest, w)
            event_walls.append(slowest)
        out["events"][ev] = {"parallel_event_wall_ms_p50": pct(event_walls, 0.5), "parallel_event_wall_ms_p95": pct(event_walls, 0.95),
                             "commands": [dict(cmd=d["cmd"], **summarize(d["w"], d["c"], d["rc"])) for d in per.values()]}
    out["children_peak_rss_mb"] = maxrss_mb(peak)
    print(json.dumps(out, indent=2))


def mode_once(a):
    walls, cpus, rcs = [], [], []
    peak = 0
    for _ in range(a.n):
        r0 = resource.getrusage(resource.RUSAGE_CHILDREN)
        t0 = time.perf_counter()
        p = subprocess.run(a.cmd, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=1800)
        t1 = time.perf_counter()
        r1 = resource.getrusage(resource.RUSAGE_CHILDREN)
        walls.append((t1 - t0) * 1000)
        cpus.append((r1.ru_utime - r0.ru_utime + r1.ru_stime - r0.ru_stime) * 1000)
        rcs.append(p.returncode)
        peak = r1.ru_maxrss
        tail = (p.stdout + p.stderr).decode(errors="replace").strip().splitlines()[-3:]
    print(json.dumps(dict(mode="once", cmd=" ".join(a.cmd)[:200], children_peak_rss_mb=maxrss_mb(peak), last_output_tail=tail,
                          **summarize(walls, cpus, rcs)), indent=2))


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("mode", choices=["pyre", "hive", "once"])
    ap.add_argument("--state")
    ap.add_argument("--orig-state", default="")
    ap.add_argument("--hooks")
    ap.add_argument("--plugin-root")
    ap.add_argument("--payloads")
    ap.add_argument("--transcript")
    ap.add_argument("--n", type=int, default=100)
    ap.add_argument("--replace", action="append", default=[], help="OLD=NEW rewrite in hook commands (e.g. another host's pyred path)")
    ap.add_argument("cmd", nargs="*")
    a = ap.parse_args()
    {"pyre": mode_pyre, "hive": mode_hive, "once": mode_once}[a.mode](a)


if __name__ == "__main__":
    main()
