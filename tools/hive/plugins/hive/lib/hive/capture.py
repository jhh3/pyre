"""Transcript capture lifecycle: checkpoints, compact saves, finalize, sweep.

All transcript parsing, redaction, sanitization, and the trivial-session skip
rule live in the vendored jhh3-mem capture verb; its UNIQUE(uid, source_bytes)
upsert makes every call here idempotent — a redundant capture is a 0-row
no-op, a grown transcript inserts a new row and supersedes the old ones.
This module only gates (hive_capture_state), invokes the vendored verb, and
maintains state. Never reimplement parsing or redaction here.

Budget: called from hooks — ~150ms of Python plus one bounded subprocess.
Functions fail soft (log + False/None) rather than raise where cheap; the
hook harness guards the rest.
"""

from __future__ import annotations

import os
import time
from pathlib import Path

from . import config, db, util, vendored

CAPTURE_TIMEOUT_S = 60  # vendored capture handles up to a 64MiB tail
SWEEP_MIN_AGE_S = 120   # never sweep a transcript still being written


def _stat_size(path: str | Path) -> int | None:
    try:
        return os.stat(path).st_size
    except OSError:
        return None


def _session_cwd(conn, harness: str, session_id: str) -> str:
    row = conn.execute(
        "SELECT cwd FROM hive_sessions WHERE harness=? AND session_id=? AND machine=?",
        (harness, session_id, config.machine_id()),
    ).fetchone()
    return row["cwd"] if row else ""


def _mem_capture(transcript_path: str, session_id: str = "", cwd: str = "",
                 reason: str = "") -> bool:
    """One vendored capture invocation. True iff the subprocess exited 0."""
    args = ["capture", "--transcript", str(transcript_path)]
    if session_id:
        args += ["--session", session_id]
    if cwd:
        args += ["--cwd", cwd]
    if reason:
        args += ["--reason", reason]
    try:
        return vendored.mem(args, timeout=CAPTURE_TIMEOUT_S).returncode == 0
    except Exception as exc:  # timeout, missing binary — capture is best-effort
        util.log(config.HOOK_LOG, f"capture: vendored capture failed: {exc!r}")
        return False


def _current_entry(conn, uid: str):
    """The live (largest-bytes, non-superseded) entries row for a uid, or None.

    The entries table is vendored-owned and may not exist in a fresh hive DB.
    """
    try:
        return conn.execute(
            "SELECT * FROM entries WHERE uid=? AND superseded=0 "
            "ORDER BY source_bytes DESC LIMIT 1", (uid,)).fetchone()
    except db.sqlite3.OperationalError:
        return None


def _record_offset(harness: str, session_id: str, transcript_path: str,
                   size: int, bump_seq: bool = False) -> int:
    """Upsert hive_capture_state; returns the (possibly bumped) checkpoint_seq.

    bump_seq also mirrors the seq onto hive_sessions.last_checkpoint_seq.
    """
    now = db.now()
    with db.connect() as conn:
        db.ensure_hive_schema(conn)
        if bump_seq:
            conn.execute(
                """INSERT INTO hive_capture_state
                   (harness, session_id, transcript_path, offset_bytes,
                    last_checkpoint_at, entry_uid, checkpoint_seq)
                   VALUES (?,?,?,?,?,?,1)
                   ON CONFLICT(harness, session_id) DO UPDATE SET
                     transcript_path=excluded.transcript_path,
                     offset_bytes=excluded.offset_bytes,
                     last_checkpoint_at=excluded.last_checkpoint_at,
                     entry_uid=excluded.entry_uid,
                     checkpoint_seq=checkpoint_seq+1""",
                (harness, session_id, str(transcript_path), size, now, session_id),
            )
        else:
            conn.execute(
                """INSERT INTO hive_capture_state
                   (harness, session_id, transcript_path, offset_bytes,
                    last_checkpoint_at, entry_uid, checkpoint_seq)
                   VALUES (?,?,?,?,?,?,0)
                   ON CONFLICT(harness, session_id) DO UPDATE SET
                     transcript_path=excluded.transcript_path,
                     offset_bytes=excluded.offset_bytes,
                     last_checkpoint_at=excluded.last_checkpoint_at,
                     entry_uid=excluded.entry_uid""",
                (harness, session_id, str(transcript_path), size, now, session_id),
            )
        seq = conn.execute(
            "SELECT checkpoint_seq FROM hive_capture_state "
            "WHERE harness=? AND session_id=?", (harness, session_id),
        ).fetchone()[0]
        if bump_seq:
            conn.execute(
                "UPDATE hive_sessions SET last_checkpoint_seq=?, last_activity=? "
                "WHERE harness=? AND session_id=? AND machine=?",
                (seq, now, harness, session_id, config.machine_id()),
            )
        conn.commit()
    return seq


def maybe_checkpoint(harness: str, session_id: str, transcript_path: str) -> bool:
    """Stop-hook checkpoint: gated incremental capture upsert.

    Gate (config [checkpoint]): capture when new bytes >= min_new_bytes, or
    when any new bytes exist and min_interval_s has passed since the last
    checkpoint. No new bytes -> never. Passing the gate calls the vendored
    capture (reason "checkpoint"), bumps hive_capture_state + the ledger's
    last_checkpoint_seq, and — when the live entry looks significant
    (config [registration]) — enqueues an enrich_checkpoint job.
    Returns True iff a checkpoint capture ran.
    """
    if not session_id or not transcript_path:
        return False
    size = _stat_size(transcript_path)
    if size is None:
        return False
    cp = config.load()["checkpoint"]
    now = db.now()
    with db.connect() as conn:
        db.ensure_hive_schema(conn)
        state = conn.execute(
            "SELECT offset_bytes, last_checkpoint_at FROM hive_capture_state "
            "WHERE harness=? AND session_id=?", (harness, session_id),
        ).fetchone()
        offset = state["offset_bytes"] if state else 0
        last_at = state["last_checkpoint_at"] if state else 0
        cwd = _session_cwd(conn, harness, session_id)
    new_bytes = size - offset
    if new_bytes <= 0:
        return False
    if new_bytes < cp["min_new_bytes"] and now - last_at < cp["min_interval_s"]:
        return False

    if not _mem_capture(transcript_path, session_id=session_id, cwd=cwd,
                        reason="checkpoint"):
        return False
    _record_offset(harness, session_id, transcript_path, size, bump_seq=True)

    # Significance gate → queue an out-of-band enrichment of the live entry.
    try:
        reg = config.load()["registration"]
        with db.connect() as conn:
            entry = _current_entry(conn, session_id)
        if entry is not None:
            age_min = (now - (entry["started_at"] or entry["ts"] or now)) / 60
            if (entry["n_mod"] >= reg["significant_files"]
                    or age_min >= reg["significant_minutes"]):
                from . import enrich
                enrich.enqueue("enrich_checkpoint", entry_uid=session_id,
                               priority=5)
    except Exception as exc:
        util.log(config.HOOK_LOG, f"capture: significance check failed: {exc!r}")
    return True


def _clip_bytes(text: str, limit: int) -> str:
    """UTF-8-boundary-safe byte truncation (matches the vendored discipline)."""
    raw = text.encode("utf-8")
    if len(raw) <= limit:
        return text
    return raw[:limit].decode("utf-8", "ignore")


def _first_sentence(text: str, limit: int = 300) -> str:
    head = text.strip().split("\n", 1)[0]
    dot = head.find(". ")
    if dot > 0:
        head = head[: dot + 1]
    return _clip_bytes(head, limit)


def compact_save(harness: str, session_id: str, transcript_path: str,
                 trigger: str, snapshot_to: Path) -> None:
    """PreCompact: bank the doomed span, render the reinjection snapshot.

    Runs the vendored capture (reason "compact"), then renders the current
    entry into snapshot_to as markdown (<=2048 bytes — SessionStart(compact)
    reinjects it). Ends with a best-effort hardlink of the pre-compaction
    transcript into SNAPSHOT_DIR — never a copy (PLAN §5).
    """
    if not session_id or not transcript_path:
        return
    with db.connect() as conn:
        db.ensure_hive_schema(conn)
        cwd = _session_cwd(conn, harness, session_id)
    _mem_capture(transcript_path, session_id=session_id, cwd=cwd,
                 reason="compact")
    size = _stat_size(transcript_path)
    if size is not None:
        _record_offset(harness, session_id, transcript_path, size)

    with db.connect() as conn:
        entry = _current_entry(conn, session_id)
    if entry is not None:
        lines = [f"# {entry['title'] or '(untitled session)'}", ""]
        next_steps = entry["next_steps"] or _first_sentence(entry["outcome"])
        if next_steps:
            lines += ["## Next steps", next_steps.strip(), ""]
        files = [f for f in entry["files_touched"].splitlines() if f][:15]
        if files:
            lines += ["## Files touched"] + [f"- {f}" for f in files] + [""]
        if entry["commands"]:
            lines += ["## Commands", entry["commands"], ""]
        lines += [f"trigger: {trigger} · session: {session_id}"]
        try:
            snapshot_to.parent.mkdir(parents=True, exist_ok=True)
            snapshot_to.write_text(_clip_bytes("\n".join(lines), 2048))
        except OSError as exc:
            util.log(config.HOOK_LOG, f"capture: snapshot write failed: {exc!r}")

    # Hardlink the transcript as it stood before compaction (skip silently on
    # cross-device links or an already-banked copy).
    if size is not None:
        try:
            config.SNAPSHOT_DIR.mkdir(parents=True, exist_ok=True)
            os.link(transcript_path,
                    config.SNAPSHOT_DIR / f"precompact-{session_id}-{size}.jsonl")
        except OSError:
            pass


def finalize(harness: str, session_id: str, transcript_path: str,
             end_reason: str) -> str | None:
    """SessionEnd/close: final vendored capture with the payload reason.

    Returns session_id (the entry uid) if an entries row exists for it —
    the caller uses that to enqueue priority enrichment — else None.
    """
    if not session_id:
        return None
    with db.connect() as conn:
        db.ensure_hive_schema(conn)
        cwd = _session_cwd(conn, harness, session_id)
    size = _stat_size(transcript_path) if transcript_path else None
    if size is not None:
        _mem_capture(transcript_path, session_id=session_id, cwd=cwd,
                     reason=end_reason)
        _record_offset(harness, session_id, transcript_path, size)
    with db.connect() as conn:
        try:
            row = conn.execute(
                "SELECT 1 FROM entries WHERE uid=? LIMIT 1", (session_id,),
            ).fetchone()
        except db.sqlite3.OperationalError:
            row = None
    return session_id if row else None


def sweep_transcript(path: Path, harness: str) -> bool:
    """Tick sweep: capture any transcript bytes the hooks missed.

    Skips transcripts modified in the last SWEEP_MIN_AGE_S (a live session's
    own hooks will get them) and transcripts whose size equals the recorded
    offset (nothing new). For claude-code the session id is the basename
    minus .jsonl; codex rollout files get only --transcript (the vendored
    verb derives the uid from the basename). Idempotency makes redundant
    calls safe. Returns True iff a capture ran.
    """
    path = Path(path)
    try:
        st = os.stat(path)
    except OSError:
        return False
    if time.time() - st.st_mtime < SWEEP_MIN_AGE_S:
        return False
    session_id = path.name.removesuffix(".jsonl")
    with db.connect() as conn:
        db.ensure_hive_schema(conn)
        state = conn.execute(
            "SELECT offset_bytes FROM hive_capture_state "
            "WHERE harness=? AND session_id=?", (harness, session_id),
        ).fetchone()
    if state and state["offset_bytes"] == st.st_size:
        return False
    ok = _mem_capture(str(path),
                      session_id=session_id if harness == "claude-code" else "")
    # Record the offset even when capture no-ops (duplicate/trivial): the
    # vendored verb is idempotent, so re-attempting the same bytes forever is
    # pure waste — and it keeps the doctor's sweep-backlog metric honest.
    _record_offset(harness, session_id, str(path), st.st_size)
    if ok:
        _seed_session_row(harness, session_id, path, st)
    return ok


def _seed_session_row(harness: str, session_id: str, path: Path,
                      st: os.stat_result) -> None:
    """Sessions that predate hive (or whose hooks never fired) still need a
    ledger row — the raw-vault mirror and reaper iterate hive_sessions, and a
    days-long session already in flight is exactly the case that must not be
    invisible. Insert-only: never clobbers hook-written rows."""
    try:
        with db.connect() as conn:
            conn.execute(
                """INSERT OR IGNORE INTO hive_sessions
                   (session_id, harness, machine, transcript_path,
                    started_at, last_activity)
                   VALUES (?,?,?,?,?,?)""",
                (session_id, harness, config.machine_id(), str(path),
                 int(st.st_ctime), int(st.st_mtime)))
            conn.execute(
                "UPDATE hive_sessions SET last_activity=?, transcript_path=? "
                "WHERE harness=? AND session_id=? AND machine=? "
                "AND last_activity < ?",
                (int(st.st_mtime), str(path), harness, session_id,
                 config.machine_id(), int(st.st_mtime)))
            conn.commit()
    except Exception:
        pass
