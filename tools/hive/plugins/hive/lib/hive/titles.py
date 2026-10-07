"""Title freshness: tmux windows/sessions follow what the session is doing.

The stale-title mechanism (docs/PLAN.md §5): tmux has automatic-rename off,
and every naming authority stamps names only at creation. Hive derives titles
from the session's newest captured entry and pushes them server-side
(`tmux rename-window` works despite `allow-rename off`, which only blocks
escape-sequence renames).

Guards: throttle (config titles.throttle_s, default 300s), freeze after two
identical derivations (title_locked), never touch sessions without a
recorded tmux target, fail-soft everywhere.
"""

from __future__ import annotations

import os
from pathlib import Path

from . import config, db, util

# Overridable for tests (fake /proc tree under tmp).
PROC_ROOT = "/proc"

_DISCOVER_MAX_DEPTH = 6      # /proc descendant walk depth per pane
_DISCOVER_MAX_PROCS = 200    # processes visited per pane walk


def _project_dir(cwd: str) -> str:
    """cwd -> claude project dir name: every char outside [A-Za-z0-9]
    becomes '-' (an absolute path therefore starts with '-'), e.g.
    /home/user/code/x -> -home-user-code-x."""
    return "".join(
        c if ("a" <= c <= "z" or "A" <= c <= "Z" or "0" <= c <= "9") else "-"
        for c in cwd)


def _descendants(root_pid: str) -> list[str]:
    """root_pid plus its /proc descendants (children files), bounded."""
    seen = {root_pid}
    out = [root_pid]
    frontier = [(root_pid, 0)]
    while frontier and len(out) < _DISCOVER_MAX_PROCS:
        pid, depth = frontier.pop(0)
        if depth >= _DISCOVER_MAX_DEPTH:
            continue
        try:
            kids = (Path(PROC_ROOT) / pid / "task" / pid /
                    "children").read_text().split()
        except OSError:
            continue
        for k in kids:
            if k not in seen:
                seen.add(k)
                out.append(k)
                frontier.append((k, depth + 1))
    return out


def _pane_harness_cwd(pane_pid: str) -> str:
    """cwd of the first claude/codex process under a pane's pid, or ''."""
    for pid in _descendants(pane_pid):
        try:
            comm = (Path(PROC_ROOT) / pid / "comm").read_text().strip()
        except OSError:
            continue
        if comm == "claude" or "codex" in comm:
            try:
                return os.readlink(str(Path(PROC_ROOT) / pid / "cwd"))
            except OSError:
                continue
    return ""


def discover_panes() -> int:
    """Backfill tmux targets for ledger rows seeded without them.

    Sweep-seeded rows (sessions started before hive, or whose hooks didn't
    record tmux) have empty tmux_session/tmux_pane, so the title phase has no
    targets. Walk live tmux panes, find the harness process under each, map
    its cwd to the claude project dir name, and attach the pane to the most
    recently active matching row. Idempotent; never overwrites a non-empty
    value; every failure is silent-logged. Returns rows updated.
    """
    updated = 0
    try:
        with db.connect() as conn:
            db.ensure_hive_schema(conn)
            candidates = conn.execute(
                """SELECT COUNT(*) FROM hive_sessions
                   WHERE ended_at IS NULL AND machine=?
                     AND (tmux_pane='' OR tmux_session='')""",
                (config.machine_id(),)).fetchone()[0]
            if not candidates:
                return 0
            r = util.run(["tmux", "list-panes", "-a", "-F",
                          "#{pane_id}\t#{session_name}\t#{pane_pid}"],
                         timeout=5)
            if r.returncode != 0:
                return 0
            for line in (r.stdout or "").splitlines():
                parts = line.split("\t")
                if len(parts) != 3 or not all(parts[:2]):
                    continue
                pane_id, sess, pane_pid = parts
                # A pane already claimed by a live row never re-attaches
                # (idempotence: one pane, one session).
                claimed = conn.execute(
                    """SELECT 1 FROM hive_sessions
                       WHERE ended_at IS NULL AND machine=? AND tmux_pane=?
                       LIMIT 1""",
                    (config.machine_id(), pane_id)).fetchone()
                if claimed:
                    continue
                cwd = _pane_harness_cwd(pane_pid.strip())
                if not cwd:
                    continue
                row = conn.execute(
                    """SELECT harness, session_id FROM hive_sessions
                       WHERE ended_at IS NULL AND machine=?
                         AND (tmux_pane='' OR tmux_session='')
                         AND transcript_path LIKE ?
                       ORDER BY last_activity DESC LIMIT 1""",
                    (config.machine_id(),
                     f"%/{_project_dir(cwd)}/%")).fetchone()
                if not row:
                    continue
                conn.execute(
                    """UPDATE hive_sessions SET
                         tmux_session=CASE WHEN tmux_session=''
                                           THEN ? ELSE tmux_session END,
                         tmux_pane=CASE WHEN tmux_pane=''
                                        THEN ? ELSE tmux_pane END
                       WHERE harness=? AND session_id=? AND machine=?""",
                    (sess, pane_id, row["harness"], row["session_id"],
                     config.machine_id()))
                updated += 1
            conn.commit()
    except Exception as exc:
        util.log(config.TICK_LOG, f"titles: discover_panes failed: {exc!r}")
    return updated


def _derive(conn, session_id: str) -> str:
    row = conn.execute(
        "SELECT title FROM entries WHERE uid=? AND superseded=0 "
        "ORDER BY source_bytes DESC LIMIT 1", (session_id,)).fetchone()
    if not row or not row["title"]:
        return ""
    title = row["title"].strip()
    if title == "(untitled session)":
        return ""
    max_len = config.load()["titles"]["max_len"]
    return title[:max_len].rstrip()


def _tmux_target(row) -> str:
    """Prefer the pane id (targets its window exactly); else the session's
    active window."""
    if row["tmux_pane"]:
        return row["tmux_pane"]
    if row["tmux_session"]:
        return row["tmux_session"] + ":"
    return ""


def tick_phase() -> int:
    """Refresh titles for live sessions. Returns number renamed."""
    discover_panes()
    throttle = config.load()["titles"]["throttle_s"]
    now = db.now()
    renamed = 0
    with db.connect() as conn:
        db.ensure_hive_schema(conn)
        rows = conn.execute(
            """SELECT session_id, harness, tmux_session, tmux_pane, title,
                      title_locked, title_derivations, last_title_at
               FROM hive_sessions
               WHERE ended_at IS NULL AND machine=?
                 AND (tmux_session != '' OR tmux_pane != '')
                 AND title_locked = 0""",
            (config.machine_id(),)).fetchall()
        for row in rows:
            if now - row["last_title_at"] < throttle:
                continue
            title = _derive(conn, row["session_id"])
            if not title:
                continue
            if title == row["title"]:
                # Second identical derivation → freeze (stable work topic).
                if row["title_derivations"] >= 1:
                    conn.execute(
                        "UPDATE hive_sessions SET title_locked=1 "
                        "WHERE harness=? AND session_id=? AND machine=?",
                        (row["harness"], row["session_id"],
                         config.machine_id()))
                continue
            target = _tmux_target(row)
            if not target:
                continue
            try:
                r = util.run(["tmux", "rename-window", "-t", target, title],
                             timeout=3)
                if r.returncode != 0:
                    continue
            except Exception:
                continue
            conn.execute(
                """UPDATE hive_sessions SET title=?, last_title_at=?,
                       title_derivations=title_derivations+1
                   WHERE harness=? AND session_id=? AND machine=?""",
                (title, now, row["harness"], row["session_id"],
                 config.machine_id()))
            renamed += 1
        conn.commit()
    return renamed
