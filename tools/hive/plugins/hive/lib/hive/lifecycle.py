"""Stream close pipeline: archive before anything is destroyed (PLAN §2).

`wt rm`, `hive done`, and `hive park` all route through close_stream: final
capture, ledger close, handoff doc, stream status update — in that order, so
a failure late in the chain never costs the capture. Every step is fail-soft;
the vendored tools own their own timeouts.
"""

from __future__ import annotations

import time
from pathlib import Path

from . import capture, config, db, ledger, util, vendored

HANDOFF_MAX_BYTES = 4096


def _live_session(slug: str):
    with db.connect() as conn:
        db.ensure_hive_schema(conn)
        return conn.execute(
            """SELECT session_id, harness, transcript_path
               FROM hive_sessions
               WHERE stream_slug=? AND ended_at IS NULL AND machine=?
               ORDER BY last_activity DESC LIMIT 1""",
            (slug, config.machine_id())).fetchone()


def _write_handoff(slug: str, uid: str) -> Path | None:
    """Render the closing entry into STREAMS_VAULT/handoffs/<slug>/. The doc
    is the machine-switch payload's raw material — small (<=4KB), masked
    already (the vendored capture redacted the entry fields at write time)."""
    with db.connect() as conn:
        entry = capture._current_entry(conn, uid)
    if entry is None:
        return None
    lines = [f"# {entry['title'] or '(untitled session)'}",
             "", f"closed: {time.strftime('%Y-%m-%d %H:%M')} · "
             f"session: {uid} · machine: {config.machine_id()}", ""]
    for heading, field in (("Learned", "learned"), ("Decided", "decided"),
                           ("Next steps", "next_steps")):
        if entry[field]:
            lines += [f"## {heading}", entry[field].strip(), ""]
    files = [f for f in entry["files_touched"].splitlines() if f][:40]
    if files:
        lines += ["## Files touched"] + [f"- {f}" for f in files] + [""]
    out = config.STREAMS_VAULT / "handoffs" / slug / (
        time.strftime("%Y%m%d-%H%M") + ".md")
    try:
        out.parent.mkdir(parents=True, exist_ok=True)
        out.write_text(capture._clip_bytes("\n".join(lines),
                                           HANDOFF_MAX_BYTES))
        return out
    except OSError as exc:
        util.log(config.TICK_LOG, f"lifecycle: handoff write failed: {exc!r}")
        return None


def close_stream(slug: str, reason: str, status: str = "parked") -> Path | None:
    """Archive-then-close for one stream on this machine.

    Finds the live session for the slug, runs the final capture (payload
    reason), closes the ledger row clean, writes the handoff doc, and
    best-effort updates the vendored stream status. Returns the handoff path
    when one was written.
    """
    handoff = None
    row = _live_session(slug)
    if row is not None:
        sid = row["session_id"]
        try:
            capture.finalize(row["harness"], sid, row["transcript_path"],
                             reason)
        except Exception as exc:
            util.log(config.TICK_LOG, f"lifecycle: finalize {sid}: {exc!r}")
        ledger.close(sid, "clean", row["harness"])
        handoff = _write_handoff(slug, sid)
    try:
        vendored.ws(["update", slug, "--status", status], timeout=30)
    except Exception as exc:
        util.log(config.TICK_LOG, f"lifecycle: ws update {slug}: {exc!r}")
    return handoff
