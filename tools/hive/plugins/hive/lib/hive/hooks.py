"""hive's own hook handlers (the vendored ws/jhh3-mem hooks run as separate
hooks.json entries and are not wrapped here).

Contracts: called via hook-harness.sh (timeouts, breaker, exit-0 owned
there). Handlers stay bounded: no LLM calls, no network, no large copies.
Anything slow is enqueued for the tick or spawned detached.
"""

from __future__ import annotations

import json
import sys

from . import config, db, ledger, util


def _emit_context(text: str) -> None:
    """Inject additionalContext for hooks that support it."""
    if not text:
        return
    print(json.dumps({
        "hookSpecificOutput": {
            "hookEventName": "SessionStart",
            "additionalContext": text,
        }
    }))


def _snapshot_path(session_id: str):
    return config.SNAPSHOT_DIR / f"compact-{session_id}.md"


def _harness(payload: dict) -> str:
    """Same handlers serve both harnesses (codex-hooks.json routes here too);
    the rollout path is the discriminator."""
    if "/.codex/" in payload.get("transcript_path", ""):
        return "codex"
    return "claude-code"


def on_session_start(payload: dict) -> int:
    config.ensure_dirs()
    sid = payload.get("session_id", "")
    source = payload.get("source", "startup")

    ledger.touch_start(payload, harness=_harness(payload))

    # wt birth marker → stamp stream slug onto this session.
    tmux_sess, _ = ledger._tmux_context()
    birth = ledger.claim_birth(tmux_sess)
    if birth and birth.get("slug") and sid:
        ledger.set_stream(sid, birth["slug"])

    inject_parts: list[str] = []

    if source == "compact":
        # Restore the working state the summarizer mangles.
        snap = _snapshot_path(sid)
        try:
            text = snap.read_text()[: config.load()["inject"]["max_bytes"]]
            if text.strip():
                inject_parts.append(
                    "<hive-compact-snapshot>\nWorking state banked just before "
                    "compaction (trust this over the compacted summary for "
                    "in-flight details):\n" + text + "\n</hive-compact-snapshot>"
                )
        except OSError:
            pass
    elif source in ("startup", "resume"):
        brief = _interruption_brief(payload)
        if brief:
            inject_parts.append(brief)

    if inject_parts:
        _emit_context("\n\n".join(inject_parts))
    return 0


def _interruption_brief(payload: dict) -> str:
    """If the previous session in this cwd was interrupted (crash/reboot),
    say so explicitly with its last known state."""
    cwd = payload.get("cwd", "")
    sid = payload.get("session_id", "")
    if not cwd:
        return ""
    try:
        with db.connect() as conn:
            db.ensure_hive_schema(conn)
            row = conn.execute(
                """SELECT session_id, end_reason, last_activity, title,
                          stream_slug, last_checkpoint_seq
                   FROM hive_sessions
                   WHERE machine=? AND cwd=? AND session_id != ?
                     AND end_reason IN ('interrupted','crashed','reaped')
                     AND last_activity > ?
                   ORDER BY last_activity DESC LIMIT 1""",
                (config.machine_id(), cwd, sid, db.now() - 48 * 3600),
            ).fetchone()
            if not row:
                return ""
            # Pull the interrupted session's live entry for next_steps.
            extra = ""
            entry = conn.execute(
                """SELECT title, next_steps, files_touched FROM entries
                   WHERE uid LIKE ? ORDER BY ts DESC LIMIT 1""",
                (f"%{row['session_id'][:8]}%",),
            ).fetchone()
            if entry and (entry["next_steps"] or entry["files_touched"]):
                extra = (f"\nlast checkpoint: {entry['title']}"
                         f"\nnext steps: {entry['next_steps'][:600]}"
                         f"\nfiles in flight: {entry['files_touched'][:400]}")
            import time
            when = time.strftime("%Y-%m-%d %H:%M",
                                 time.localtime(row["last_activity"]))
            # One brief per interrupted session: mark it consumed.
            conn.execute(
                "UPDATE hive_sessions SET end_reason='crashed-briefed' "
                "WHERE machine=? AND session_id=?",
                (config.machine_id(), row["session_id"]),
            )
            conn.commit()
            return (
                "<hive-interruption-brief>\nA previous session in this "
                f"directory was interrupted ({row['end_reason']}) around "
                f"{when} — its native context did NOT survive. Recovered "
                f"state:{extra}\nUse `hive show`/`hive ask` for more."
                "\n</hive-interruption-brief>"
            )
    except Exception:
        return ""


def on_stop(payload: dict) -> int:
    sid = payload.get("session_id", "")
    hz = _harness(payload)
    ledger.heartbeat(sid, harness=hz)
    try:
        from . import capture
        capture.maybe_checkpoint(
            harness=hz, session_id=sid,
            transcript_path=payload.get("transcript_path", ""),
        )
    except Exception as exc:  # capture must never break the turn
        util.log(config.HOOK_LOG, f"stop-checkpoint failed: {exc!r}")
    return 0


def on_pre_compact(payload: dict) -> int:
    """The one by-design context destruction — bank everything first."""
    config.ensure_dirs()
    sid = payload.get("session_id", "")
    tpath = payload.get("transcript_path", "")
    try:
        from . import capture
        capture.compact_save(
            harness="claude-code", session_id=sid, transcript_path=tpath,
            trigger=payload.get("trigger", "auto"),
            snapshot_to=_snapshot_path(sid),
        )
    except Exception as exc:
        util.log(config.HOOK_LOG, f"pre-compact save failed: {exc!r}")
    return 0


def on_session_end(payload: dict) -> int:
    sid = payload.get("session_id", "")
    reason = payload.get("reason", "other")
    ledger.close(sid, end_reason="clean" if reason in ("clear", "logout", "exit",
                 "prompt_input_exit", "other") else reason)
    try:
        from . import capture, enrich
        entry_uid = capture.finalize(
            harness="claude-code", session_id=sid,
            transcript_path=payload.get("transcript_path", ""),
            end_reason=reason,
        )
        if entry_uid:
            enrich.enqueue("enrich_close", entry_uid=entry_uid,
                           session_uid=sid, priority=1)
    except Exception as exc:
        util.log(config.HOOK_LOG, f"session-end pipeline failed: {exc!r}")
    # Accelerator: run enrich+sync phases soon, detached (tick cron is the
    # guarantee if this never runs).
    plugin_root = str(util.Path(__file__).resolve().parent.parent.parent)
    util.spawn_detached(
        ["python3", "-m", "hive.tick_worker", "--phases", "enrich,sync",
         "--delay", "2"],
        logfile=config.TICK_LOG,
        env_extra={"PYTHONPATH": plugin_root + "/lib",
                   config.NO_CAPTURE_ENV: "1"},
    )
    return 0


def on_user_prompt_submit(payload: dict) -> int:
    ledger.heartbeat(payload.get("session_id", ""), harness=_harness(payload))
    return 0


def on_post_tool_use(payload: dict) -> int:
    # Deliberately nothing: the transcript carries tool activity, checkpoints
    # parse it at turn boundaries. Per-tool-call work is the claude-mem tax.
    return 0
