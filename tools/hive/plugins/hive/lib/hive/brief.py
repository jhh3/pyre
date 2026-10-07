"""hive brief — deterministic cross-machine morning brief. No LLM.

Reads only already-synced data: the hive_sessions ledger (foreign machines
appear via session lane records) and STREAM.md frontmatter in the streams
vault (read-only, tolerant of missing/garbled files). Output is bounded to
roughly one screen (~60 lines); `--all` lifts the caps. Rows from other
machines carry their data age (their last_activity vs now) — no fake
precision about what is happening there right now.
"""

from __future__ import annotations

import argparse
from datetime import datetime

from . import config, db, util

_SESSION_CAP = 20
_STREAM_CAP = 15
_SUGGEST_CAP = 10
_LINE_CAP = 60
_NEXT_MAX = 90
_ENDED_WINDOW_S = 24 * 3600
_PARK_AFTER_S = 7 * 86400
_IDLE_MENTION_S = 48 * 3600
_STREAM_STATUSES = ("active", "blocked", "review")


def run(rest: list[str]) -> int:
    ap = argparse.ArgumentParser(
        prog="hive brief",
        description="cross-machine 'what was I doing everywhere' (no LLM)")
    ap.add_argument("--machine", help="only sessions from this machine")
    ap.add_argument("--all", action="store_true", help="lift output caps")
    try:
        args = ap.parse_args(rest)
    except SystemExit as e:
        return int(e.code or 0)

    now = db.now()
    sessions = _sessions(now, args.machine)
    streams = _streams()

    lines: list[str] = []
    lines += _render_sessions(sessions, now, args.all)
    lines.append("")
    lines += _render_streams(streams, now, args.all)
    lines.append("")
    lines += _render_suggestions(sessions, streams, now, args.all)

    if not args.all and len(lines) > _LINE_CAP:
        extra = len(lines) - (_LINE_CAP - 1)
        lines = lines[:_LINE_CAP - 1]
        lines.append(f"… {extra} more line(s) — hive brief --all")
    print("\n".join(lines))
    return 0


# ── formatting helpers ───────────────────────────────────────────────────────

def _age(now: int, ts: int) -> str:
    """Staleness label: 12m / 3h / 2d."""
    s = max(0, now - int(ts or 0))
    if s < 3600:
        return f"{s // 60}m"
    if s < 86400:
        return f"{s // 3600}h"
    return f"{s // 86400}d"


def _flat(s: str, cap: int) -> str:
    out = " ".join(str(s).split())
    return out if len(out) <= cap else out[: cap - 1] + "…"


# ── sessions (ledger) ────────────────────────────────────────────────────────

def _sessions(now: int, machine: str | None) -> list[dict]:
    """Live sessions plus those ended in the last 24h, all machines."""
    try:
        with db.connect() as conn:
            db.ensure_hive_schema(conn)
            q = """SELECT machine, session_id, title, stream_slug,
                          last_activity, ended_at
                   FROM hive_sessions
                   WHERE (ended_at IS NULL OR ended_at >= ?)"""
            params: list = [now - _ENDED_WINDOW_S]
            if machine:
                q += " AND machine=?"
                params.append(machine)
            q += " ORDER BY (ended_at IS NULL) DESC, last_activity DESC"
            rows = [dict(r) for r in conn.execute(q, params).fetchall()]
    except Exception as exc:
        util.log(config.TICK_LOG, f"brief: sessions query failed: {exc!r}")
        return []
    for r in rows:
        r["live"] = r["ended_at"] is None
    return rows


def _session_line(row: dict, now: int) -> str:
    label = row["title"] or row["session_id"][:8]
    slug = row["stream_slug"] or "-"
    age = _age(now, row["last_activity"])
    if row["machine"] != config.machine_id():
        age = f"data {age} old"
    line = (f"  {row['machine']} · {_flat(label, 60)} · stream:{slug} · {age}")
    if row["live"]:
        line += " [LIVE]"
    return line


def _render_sessions(rows: list[dict], now: int, lift: bool) -> list[str]:
    out = ["sessions — live + ended <24h"]
    if not rows:
        out.append("  (none)")
        return out
    # Group by machine, ours first; live-first/recency order is preserved
    # within each group by the stable sort.
    ours = config.machine_id()
    rows = sorted(rows, key=lambda r: (r["machine"] != ours, r["machine"]))
    cap = None if lift else _SESSION_CAP
    for r in rows[:cap]:
        out.append(_session_line(r, now))
    if cap and len(rows) > cap:
        out.append(f"  … {len(rows) - cap} more session(s) — hive brief --all")
    return out


# ── streams (STREAM.md frontmatter, read-only) ───────────────────────────────

def _frontmatter(text: str) -> dict[str, str]:
    lines = text.splitlines()
    if not lines or lines[0].strip() != "---":
        return {}
    fm: dict[str, str] = {}
    for line in lines[1:]:
        if line.strip() == "---":
            break
        key, sep, val = line.partition(":")
        if sep:
            fm[key.strip()] = val.strip().strip('"')
    return fm


def _parse_updated(val: str) -> int:
    try:
        return int(datetime.fromisoformat(val).timestamp())
    except (ValueError, TypeError, OverflowError, OSError):
        return 0


def _streams() -> list[dict]:
    """Active/blocked/review streams from the vault, updated desc."""
    root = config.STREAMS_VAULT / "streams"
    out: list[dict] = []
    try:
        dirs = sorted(p for p in root.iterdir() if p.is_dir())
    except OSError:
        return out
    for d in dirs:
        try:
            fm = _frontmatter((d / "STREAM.md").read_text(errors="replace"))
        except OSError:
            continue
        status = fm.get("status", "active")
        if status not in _STREAM_STATUSES:
            continue
        out.append({
            "slug": d.name,
            "title": fm.get("title", d.name),
            "status": status,
            "next": fm.get("next", ""),
            "updated": _parse_updated(fm.get("updated", "")),
        })
    out.sort(key=lambda s: s["updated"], reverse=True)
    return out


def _render_streams(streams: list[dict], now: int, lift: bool) -> list[str]:
    out = ["streams — active/blocked/review"]
    if not streams:
        out.append("  (none)")
        return out
    cap = None if lift else _STREAM_CAP
    for s in streams[:cap]:
        age = _age(now, s["updated"]) if s["updated"] else "?"
        line = f"  {s['status']:<7} {s['slug']} · {age}"
        if s["next"]:
            line += f" · next: {_flat(s['next'], _NEXT_MAX)}"
        out.append(line)
    if cap and len(streams) > cap:
        out.append(f"  … {len(streams) - cap} more stream(s) — hive brief --all")
    return out


# ── suggestions ──────────────────────────────────────────────────────────────

def _render_suggestions(sessions: list[dict], streams: list[dict],
                        now: int, lift: bool) -> list[str]:
    sug: list[str] = []
    for s in streams:
        if (s["status"] == "active" and s["updated"]
                and now - s["updated"] > _PARK_AFTER_S):
            sug.append(f"  consider: hive park {s['slug']} "
                       f"(active, untouched {_age(now, s['updated'])})")
    for r in sessions:
        if r["live"] and now - r["last_activity"] > _IDLE_MENTION_S:
            label = r["title"] or r["session_id"][:8]
            sug.append(
                f"  note: live session '{_flat(label, 50)}' on {r['machine']} "
                f"idle {_age(now, r['last_activity'])} — long-idle sessions "
                "are often intentional; leaving it alone")
    out = ["suggestions"]
    if not sug:
        out.append("  (none)")
        return out
    cap = None if lift else _SUGGEST_CAP
    out += sug[:cap]
    if cap and len(sug) > cap:
        out.append(f"  … {len(sug) - cap} more — hive brief --all")
    return out
