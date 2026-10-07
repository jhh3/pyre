"""Machine-local enrichment queue and its drain.

Hooks (and the checkpoint significance gate) only enqueue hive_jobs rows;
the tick drains them by invoking the vendored jhh3-mem enrich verb — the one
code path allowed to reach a model, and it already carries the
JHH3_MEM_NO_CAPTURE self-call guard, redaction, and per-row failure handling.
Nothing in this module calls an LLM directly; every subprocess is bounded.
"""

from __future__ import annotations

import sys

from . import config, db, util, vendored

DRAIN_LIMIT = 50  # rows per vendored enrich run (newest-first covers closes)


def enqueue(kind: str, entry_uid: str, session_uid: str = "",
            priority: int = 5) -> None:
    """Queue one enrichment job. Deduped by the ux_hive_jobs_dedupe partial
    unique index — a pending/running (kind, entry_uid) pair enqueues once."""
    now = db.now()
    with db.connect() as conn:
        db.ensure_hive_schema(conn)
        conn.execute(
            """INSERT OR IGNORE INTO hive_jobs
               (kind, session_uid, entry_uid, priority, state,
                created_at, updated_at)
               VALUES (?,?,?,?,'pending',?,?)""",
            (kind, session_uid, entry_uid, priority, now, now),
        )
        conn.commit()


def drain(budget_s: int = 120) -> int:
    """Drain pending jobs with one bounded vendored enrich run.

    The vendored selection enriches newest deterministic rows first, which
    covers just-closed sessions regardless of which job asked; jobs are
    bookkeeping, not a work list. Rows the model fails on stay
    capture_mode='deterministic'/'enrich_failed' and retry naturally via the
    vendored selection (catchup/nightly), so jobs are marked done either way.
    Holds STATE_DIR/enrich.lock; a concurrent drain skips. Returns the number
    of jobs consumed.
    """
    with util.flocked(config.STATE_DIR / "enrich.lock") as acquired:
        if not acquired:
            return 0
        now = db.now()
        with db.connect() as conn:
            db.ensure_hive_schema(conn)
            ids = [r["id"] for r in conn.execute(
                """SELECT id FROM hive_jobs
                   WHERE state='pending' AND not_before <= ?
                   ORDER BY priority, created_at""", (now,))]
            if not ids:
                return 0
            marks = [(now, i) for i in ids]
            conn.executemany(
                "UPDATE hive_jobs SET state='running', attempts=attempts+1, "
                "updated_at=? WHERE id=?", marks)
            conn.commit()
        try:
            proc = vendored.mem(["enrich", "--limit", str(DRAIN_LIMIT)],
                                timeout=budget_s)
            if proc.returncode != 0:
                util.log(config.TICK_LOG,
                         f"enrich: vendored enrich rc={proc.returncode}")
        except Exception as exc:  # timeout etc — vendored selection retries
            util.log(config.TICK_LOG, f"enrich: drain run failed: {exc!r}")
        with db.connect() as conn:
            conn.executemany(
                "UPDATE hive_jobs SET state='done', updated_at=? WHERE id=?",
                [(db.now(), i) for i in ids])
            conn.commit()
        return len(ids)


def run(argv: list[str]) -> int:
    """`hive enrich [--uid U] [--limit N]`.

    --uid: there is no per-row vendored flag, so run a widened enrich pass
    (newest-first reaches recent uids) and report that uid's capture_mode.
    Without --uid: vendored enrich passthrough.
    """
    uid, limit = "", 0
    it = iter(argv)
    for a in it:
        if a == "--uid":
            uid = next(it, "")
        elif a == "--limit":
            try:
                limit = int(next(it, "0"))
            except ValueError:
                print("hive enrich: --limit takes an integer", file=sys.stderr)
                return 2
        else:
            print(f"hive enrich: unknown arg '{a}'", file=sys.stderr)
            return 2

    if uid:
        try:
            vendored.mem(["enrich", "--limit", str(max(limit, 20))],
                         timeout=1800, capture=False)
        except Exception as exc:
            print(f"hive enrich: vendored enrich failed: {exc!r}",
                  file=sys.stderr)
        with db.connect() as conn:
            try:
                row = conn.execute(
                    "SELECT capture_mode FROM entries WHERE uid=? "
                    "ORDER BY source_bytes DESC LIMIT 1", (uid,)).fetchone()
            except db.sqlite3.OperationalError:
                row = None
        if not row:
            print(f"{uid}: no entry")
            return 1
        print(f"{uid}: capture_mode={row['capture_mode']}")
        return 0

    args = ["enrich"]
    if limit:
        args += ["--limit", str(limit)]
    try:
        return vendored.mem(args, timeout=3600, capture=False).returncode
    except Exception as exc:
        print(f"hive enrich: vendored enrich failed: {exc!r}", file=sys.stderr)
        return 1
