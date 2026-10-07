"""hive tick — the single background maintenance pass (PLAN §3).

One cron (*/5), one lock, one log, sequential phases:
sweep → reap → enrich → sync → janitor. Hooks are latency accelerators;
this is the guarantee — losing every hook loses latency, never data. The
tick is the entire Codex capture story.

Discipline: no resident process (runs and exits), self-deprioritized
(nice/ionice; the systemd-run scope wraps the cron line, not this module),
every phase try/except'd — nothing raises out of the phase loop — and every
phase carries a soft wall budget checked between items. No LLM calls except
through the vendored enrich verb (enrich/janitor phases), every subprocess
bounded by a timeout.
"""

from __future__ import annotations

import argparse
import os
import time
from pathlib import Path

from . import config, db, ledger, util, vendored

PHASES = ("sweep", "reap", "enrich", "sync", "janitor")

# Soft wall budgets (seconds), checked between items — a slow single item
# (one bounded subprocess) can overrun; the next check stops the phase.
BUDGET_S = {"sweep": 120, "reap": 90, "enrich": 150, "sync": 180,
            "janitor": 3600}

SWEEP_MAX_CAPTURES = 40       # captures per tick, both harnesses combined
                              # (~200ms each; the 120s budget still governs)
SWEEP_MAX_AGE_S = 7 * 86400   # ignore transcripts older than this
REAP_IDLE_S = 1800            # no activity AND stale transcript → reap


def _lower_priority() -> None:
    """Best-effort: never contend with the interactive session."""
    try:
        os.nice(10)
    except OSError:
        pass
    try:
        util.run(["ionice", "-c3", "-p", str(os.getpid())], timeout=5)
    except Exception:
        pass


# ── sweep ──────────────────────────────────────────────────────────────────

def _iter_transcripts():
    """(path, harness, mtime) for recent transcripts, newest first."""
    cutoff = time.time() - SWEEP_MAX_AGE_S
    found: list[tuple[Path, str, float]] = []
    for root, harness, pattern in (
            (config.CLAUDE_PROJECTS, "claude-code", "*/*.jsonl"),
            (config.CODEX_SESSIONS, "codex", "**/*.jsonl")):
        if not root.is_dir():
            continue
        for p in root.glob(pattern):
            try:
                mtime = p.stat().st_mtime
            except OSError:
                continue
            if mtime >= cutoff:
                found.append((p, harness, mtime))
    found.sort(key=lambda t: t[2], reverse=True)
    return found


def _sweep(deadline: float) -> None:
    from . import capture
    captured = 0
    for path, harness, _ in _iter_transcripts():
        if captured >= SWEEP_MAX_CAPTURES or time.monotonic() > deadline:
            break
        try:
            if capture.sweep_transcript(path, harness):
                captured += 1
        except Exception as exc:
            util.log(config.TICK_LOG, f"tick: sweep {path.name}: {exc!r}")
    if captured:
        util.log(config.TICK_LOG, f"tick: sweep captured {captured}")


# ── reap ───────────────────────────────────────────────────────────────────

def _transcript_fresh(tpath: str, now: float) -> bool:
    if not tpath:
        return False
    try:
        return now - os.stat(tpath).st_mtime < REAP_IDLE_S
    except OSError:
        return False


def _tmux_alive(tmux_session: str) -> bool:
    try:
        r = util.run(["tmux", "has-session", "-t", tmux_session], timeout=5)
        return r.returncode == 0
    except Exception:
        return False


def _reap(deadline: float, boot: bool = False) -> None:
    """Close ledger rows whose sessions are demonstrably gone.

    Normal mode: live rows idle >REAP_IDLE_S — a still-growing transcript
    just gets a heartbeat; a recorded-but-live tmux session is left alone;
    otherwise close(reaped) + final capture. Boot mode (@reboot): everything
    live died with the machine — mark interrupted + finalize, single vault
    pull (sync module, if built), then `ws up` to resume the fleet.
    Reaping marks the ledger; it never kills anything.
    """
    from . import capture
    now = time.time()
    # Boot takes every live row (the machine just restarted; they all died).
    cutoff = int(now) + 1 if boot else int(now) - REAP_IDLE_S
    with db.connect() as conn:
        db.ensure_hive_schema(conn)
        rows = conn.execute(
            """SELECT session_id, harness, transcript_path, tmux_session,
                      tmux_pane
               FROM hive_sessions
               WHERE ended_at IS NULL AND machine=? AND last_activity < ?""",
            (config.machine_id(), cutoff)).fetchall()
    for row in rows:
        if time.monotonic() > deadline:
            break
        sid, harness = row["session_id"], row["harness"]
        try:
            if boot:
                ledger.close(sid, "interrupted", harness)
                capture.finalize(harness, sid, row["transcript_path"],
                                 "interrupted")
                continue
            if _transcript_fresh(row["transcript_path"], now):
                ledger.heartbeat(sid, harness)   # still growing hook-less
                continue
            if row["tmux_pane"] and _tmux_alive(row["tmux_session"]):
                continue                          # idle but alive — never kill
            ledger.close(sid, "reaped", harness)
            capture.finalize(harness, sid, row["transcript_path"], "reaped")
            util.log(config.TICK_LOG, f"tick: reaped {sid}")
        except Exception as exc:
            util.log(config.TICK_LOG, f"tick: reap {sid}: {exc!r}")
    if boot:
        try:
            from . import sync as syncmod
            pull = getattr(syncmod, "pull_once", None) or getattr(
                syncmod, "pull", None)
            if callable(pull):
                pull()
        except (ImportError, AttributeError) as exc:
            util.log(config.TICK_LOG, f"tick: boot pull unavailable: {exc}")
        except Exception as exc:
            util.log(config.TICK_LOG, f"tick: boot pull failed: {exc!r}")
        try:
            vendored.ws(["up"], timeout=300, capture=False)
        except Exception as exc:
            util.log(config.TICK_LOG, f"tick: boot ws up failed: {exc!r}")


# ── enrich / sync ──────────────────────────────────────────────────────────

def _enrich(deadline: float) -> None:
    from . import enrich
    budget = config.load()["enrich"]["job_budget_s"]
    n = enrich.drain(budget_s=budget)
    if n:
        util.log(config.TICK_LOG, f"tick: enrich drained {n} job(s)")


def _sync(deadline: float) -> None:
    """Delegate to the sync + raw + titles modules' tick phases (absent
    modules are logged and skipped, never an error)."""
    for modname in ("sync", "raw", "titles"):
        try:
            import importlib
            mod = importlib.import_module(f".{modname}", __package__)
            mod.tick_phase()
        except (ImportError, AttributeError) as exc:
            util.log(config.TICK_LOG,
                     f"tick: {modname} phase unavailable: {exc}")
        except Exception as exc:
            util.log(config.TICK_LOG, f"tick: {modname} phase failed: {exc!r}")


# ── janitor ────────────────────────────────────────────────────────────────

def _stamp_read(path: Path) -> float:
    try:
        return float(path.read_text().strip())
    except (OSError, ValueError):
        return 0.0


def _stamp_write(path: Path, ts: float) -> None:
    try:
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(f"{int(ts)}\n")
    except OSError:
        pass


def _janitor(deadline: float, now: float | None = None) -> None:
    """State-file-gated maintenance: requeue stuck jobs, hourly vendored
    catchup, nightly (03:00–04:59 local, >20h apart) vendored nightly.
    Stamps are written before the run so a hung pass can't re-trigger."""
    now = time.time() if now is None else now
    with db.connect() as conn:
        db.ensure_hive_schema(conn)
        cur = conn.execute(
            "UPDATE hive_jobs SET state='pending', updated_at=? "
            "WHERE state='running' AND updated_at < ?",
            (int(now), int(now) - 3600))
        conn.commit()
        if cur.rowcount:
            util.log(config.TICK_LOG,
                     f"tick: requeued {cur.rowcount} stuck job(s)")

    # Every ~15 min: ws watchdog (mail nudges / dead-pane revives /
    # escalations) — replaces the retired `*/15 ws watchdog --revive` cron.
    wdog = config.STATE_DIR / "janitor.watchdog"
    if now - _stamp_read(wdog) >= 900:
        _stamp_write(wdog, now)
        try:
            vendored.ws(["watchdog", "--revive", "--quiet"], timeout=180)
        except Exception as exc:
            util.log(config.TICK_LOG, f"tick: ws watchdog failed: {exc!r}")

    hourly = config.STATE_DIR / "janitor.hourly"
    if now - _stamp_read(hourly) >= 3600:
        _stamp_write(hourly, now)
        try:
            vendored.mem(["catchup"], timeout=1500)
        except Exception as exc:
            util.log(config.TICK_LOG, f"tick: catchup failed: {exc!r}")
        # streams/*.md sync: replaces the retired hourly `ws sync` cron.
        # (hive's own push covers only mem/ handoffs/ meta/.)
        try:
            vendored.ws(["sync"], timeout=120)
        except Exception as exc:
            util.log(config.TICK_LOG, f"tick: ws sync failed: {exc!r}")

    nightly = config.STATE_DIR / "janitor.nightly"
    if (time.localtime(now).tm_hour in (3, 4)
            and now - _stamp_read(nightly) > 20 * 3600):
        _stamp_write(nightly, now)
        try:
            vendored.mem(["nightly"], timeout=3500)
        except Exception as exc:
            util.log(config.TICK_LOG, f"tick: nightly failed: {exc!r}")

    # Hourly commit-xref scan (incremental, ≤500 commits/repo) over the
    # configured repo registry — feeds `hive why` / `hive session` and the
    # merged-PR → done suggestions.
    xref = config.STATE_DIR / "janitor.xref"
    if now - _stamp_read(xref) >= 3600:
        _stamp_write(xref, now)
        try:
            from pathlib import Path as _P
            from . import query
            paths = []
            for spec in config.repos().values():
                p = _P(str(spec.get("path", ""))).expanduser()
                if (p / ".git").exists():
                    paths.append(p)
            if paths:
                with db.connect() as conn:
                    db.ensure_hive_schema(conn)
                    n = query.xref_scan(conn, paths)
                if n:
                    util.log(config.TICK_LOG, f"tick: xref indexed {n} commit(s)")
        except Exception as exc:
            util.log(config.TICK_LOG, f"tick: xref scan failed: {exc!r}")

    # Weekly: retry enrich_failed rows (otherwise stranded forever — the
    # vendored hourly selection only takes deterministic rows).
    weekly = config.STATE_DIR / "janitor.weekly"
    if now - _stamp_read(weekly) >= 7 * 86400:
        _stamp_write(weekly, now)
        try:
            vendored.mem(["enrich", "--all", "--limit", "200"], timeout=1800)
        except Exception as exc:
            util.log(config.TICK_LOG, f"tick: weekly enrich retry failed: {exc!r}")

    # Monthly: squash streams-vault history (content is its own history via
    # rev records; the epoch fence makes importers reset cleanly). The vault
    # is dirty most of the time (ws writes constantly), so the stamp is
    # written only when the squash actually runs — a dirty tree just retries
    # on a later tick, shortly after an hourly ws sync leaves it clean.
    monthly = config.STATE_DIR / "janitor.squash"
    if now - _stamp_read(monthly) >= 30 * 86400:
        try:
            status = util.run(["git", "-C", str(config.STREAMS_VAULT),
                               "status", "--porcelain"], timeout=15)
            if status.returncode == 0 and not status.stdout.strip():
                _stamp_write(monthly, now)
                from . import sync as syncmod
                syncmod.squash(config.STREAMS_VAULT)
                util.log(config.TICK_LOG, "tick: monthly vault squash done")
        except Exception as exc:
            _stamp_write(monthly, now)  # errored: retry next month, not next tick
            util.log(config.TICK_LOG, f"tick: vault squash failed: {exc!r}")


# ── entry point ────────────────────────────────────────────────────────────

def run(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(prog="hive tick", add_help=True)
    parser.add_argument("--phases", default=",".join(PHASES),
                        help="comma list of: " + ",".join(PHASES))
    parser.add_argument("--boot", action="store_true",
                        help="@reboot reconcile: interrupted-mark + resume")
    parser.add_argument("--delay", type=int, default=0,
                        help="sleep N seconds before starting")
    args = parser.parse_args(argv)
    wanted = {p.strip() for p in args.phases.split(",") if p.strip()}

    config.ensure_dirs()
    with util.flocked(config.TICK_LOCK) as acquired:
        if not acquired:
            util.log(config.TICK_LOG, "tick: lock held, skipping")
            return 0
        _lower_priority()
        if args.delay > 0:
            time.sleep(min(args.delay, 300))
        fns = {"sweep": _sweep, "reap": lambda d: _reap(d, boot=args.boot),
               "enrich": _enrich, "sync": _sync, "janitor": _janitor}
        for phase in PHASES:                      # canonical order, always
            if phase not in wanted:
                continue
            t0 = time.monotonic()
            try:
                fns[phase](t0 + BUDGET_S[phase])
                util.log(config.TICK_LOG,
                         f"tick: {phase} ok "
                         f"({int((time.monotonic() - t0) * 1000)}ms)")
            except Exception as exc:              # nothing escapes the loop
                util.log(config.TICK_LOG, f"tick: {phase} failed: {exc!r}")
    return 0
