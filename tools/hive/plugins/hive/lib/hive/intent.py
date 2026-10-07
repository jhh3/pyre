"""Manual intent verbs: done / park / pin (PLAN §5 — intent, not safety).

These are the human's channel for stating intent about work; nothing here is
load-bearing for capture or sync. `done`/`park` route through the lifecycle
close pipeline when it exists (built in parallel — M2), degrading to a plain
vendored `ws update` so the verb always works. `pin` promotes an entry so
decay can never demote it out of injection.
"""

from __future__ import annotations

import subprocess

from . import db, util, vendored

_STATUS = {"done": "done", "park": "parked"}
_PIN_FLOOR = 1.5


def run(verb: str, rest: list[str]) -> int:
    if verb in ("done", "park"):
        return _close(verb, rest)
    if verb == "pin":
        return _pin(rest)
    util.eprint(f"hive: unknown intent verb '{verb}'")
    return 2


def _close(verb: str, rest: list[str]) -> int:
    if not rest:
        util.eprint(f"usage: hive {verb} <slug>")
        return 2
    slug = rest[0]
    status = _STATUS[verb]
    try:
        from . import lifecycle
        lifecycle.close_stream(slug, reason=verb, status=status)
        print(f"{slug}: {status} (close pipeline ran)")
        return 0
    except (ImportError, AttributeError, TypeError):
        pass  # lifecycle lands in parallel; fall back to the vendored update
    try:
        res = vendored.ws(["update", slug, "--status", status], capture=False)
    except (OSError, subprocess.TimeoutExpired) as e:
        util.eprint(f"hive {verb}: ws update failed: {e}")
        return 1
    if res.returncode == 0:
        print(f"{slug}: {status}")
    return res.returncode


def _pin(rest: list[str]) -> int:
    if not rest:
        util.eprint("usage: hive pin <uid>")
        return 2
    uid = rest[0]
    conn = db.connect()
    db.ensure_hive_schema(conn)
    n = 0
    has_entries = conn.execute(
        "SELECT 1 FROM sqlite_master WHERE type='table' AND name='entries'").fetchone()
    if has_entries:
        cur = conn.execute(
            "UPDATE entries SET durability=MAX(durability, ?), injectable=1 "
            "WHERE uid=?", (_PIN_FLOOR, uid))
        n = cur.rowcount
    conn.execute("INSERT INTO hive_feedback (uid, kind, ts) VALUES (?, 'pin', ?)",
                 (uid, db.now()))
    conn.commit()
    conn.close()
    if n:
        print(f"pinned: {uid} — durability floored at {_PIN_FLOOR}, injectable")
    else:
        print(f"pinned intent recorded, but no entry row matched uid {uid}")
    return 0
