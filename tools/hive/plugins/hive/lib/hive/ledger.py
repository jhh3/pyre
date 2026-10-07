"""Identity ledger: hive_sessions rows tying slug ↔ session ↔ machine ↔ tmux.

Written from hooks (fast paths — every function here must stay cheap and
never raise out), the tick (reaper), and wt (birth).
"""

from __future__ import annotations

import os
from pathlib import Path

from . import config, db, util


def _repo_info(cwd: str) -> tuple[str, str, str]:
    """(repo_key, worktree, branch) — matches jhh3-mem's derivation closely
    enough for joining; the vendored capture remains the authority on the
    repo_key written into entries."""
    try:
        r = util.run(["git", "-C", cwd, "rev-parse", "--show-toplevel"],
                     timeout=3)
        if r.returncode != 0 or not r.stdout.strip():
            return "", "", ""
        top = r.stdout.strip().splitlines()[0]
        # Branch is best-effort: unborn/detached HEAD must not blank the key.
        rb = util.run(["git", "-C", cwd, "rev-parse", "--abbrev-ref", "HEAD"],
                      timeout=3)
        branch = rb.stdout.strip() if rb.returncode == 0 else ""
        if branch == "HEAD":
            branch = ""
        worktree = Path(top).name if top else ""
        # org/repo from origin URL when available; else dir:<base>@<sha1-8>
        rr = util.run(["git", "-C", cwd, "remote", "get-url", "origin"], timeout=3)
        if rr.returncode == 0 and rr.stdout.strip():
            # Mirror jm_repo_key_from_origin: owner/repo, lowercased, .git off.
            url = rr.stdout.strip()
            for pre in ("ssh://", "git://", "https://", "http://", "github:", "gh:"):
                url = url.removeprefix(pre)
            url = url.split("@")[-1].replace(":", "/")
            parts = [p for p in url.rstrip("/").split("/") if p]
            key = "/".join(parts[-2:]).removesuffix(".git").lower()
        else:
            # jhh3-mem fallback: dir:<base>@<first 8 hex of sha256(abs path)>
            import hashlib
            anchor = str(Path(top).resolve())
            digest = hashlib.sha256(anchor.encode()).hexdigest()[:8]
            key = f"dir:{Path(anchor).name}@{digest}"
        return key, worktree, branch
    except Exception:
        return "", "", ""


def _tmux_context() -> tuple[str, str]:
    pane = os.environ.get("TMUX_PANE", "")
    sess = ""
    if pane:
        try:
            r = util.run(["tmux", "display-message", "-p", "-t", pane, "#S"],
                         timeout=2)
            if r.returncode == 0:
                sess = r.stdout.strip()
        except Exception:
            pass
    return sess, pane


def touch_start(payload: dict, harness: str = "claude-code") -> None:
    sid = payload.get("session_id", "")
    if not sid:
        return
    cwd = payload.get("cwd", os.getcwd())
    repo_key, worktree, branch = _repo_info(cwd)
    tmux_sess, tmux_pane = _tmux_context()
    no_ledger = 1 if os.environ.get("HIVE_SPAWNED_NONINTERACTIVE") else 0
    now = db.now()
    with db.connect() as conn:
        db.ensure_hive_schema(conn)
        conn.execute(
            """INSERT INTO hive_sessions
               (session_id, harness, machine, repo_key, worktree, branch,
                tmux_session, tmux_pane, transcript_path, cwd,
                started_at, last_activity, no_ledger)
               VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)
               ON CONFLICT(harness, session_id, machine) DO UPDATE SET
                 last_activity=excluded.last_activity,
                 tmux_session=CASE WHEN excluded.tmux_session != ''
                                   THEN excluded.tmux_session ELSE tmux_session END,
                 tmux_pane=CASE WHEN excluded.tmux_pane != ''
                                THEN excluded.tmux_pane ELSE tmux_pane END,
                 transcript_path=CASE WHEN excluded.transcript_path != ''
                                      THEN excluded.transcript_path ELSE transcript_path END,
                 ended_at=NULL, end_reason=''""",
            (sid, harness, config.machine_id(), repo_key, worktree, branch,
             tmux_sess, tmux_pane, payload.get("transcript_path", ""), cwd,
             now, now, no_ledger),
        )
        conn.commit()


def heartbeat(session_id: str, harness: str = "claude-code") -> None:
    if not session_id:
        return
    try:
        with db.connect() as conn:
            conn.execute(
                "UPDATE hive_sessions SET last_activity=? "
                "WHERE harness=? AND session_id=? AND machine=?",
                (db.now(), harness, session_id, config.machine_id()),
            )
            conn.commit()
    except Exception:
        pass


def close(session_id: str, end_reason: str, harness: str = "claude-code") -> None:
    if not session_id:
        return
    with db.connect() as conn:
        db.ensure_hive_schema(conn)
        conn.execute(
            "UPDATE hive_sessions SET ended_at=?, end_reason=?, last_activity=? "
            "WHERE harness=? AND session_id=? AND machine=?",
            (db.now(), end_reason, db.now(), harness, session_id,
             config.machine_id()),
        )
        conn.commit()


def record_wt_birth(slug: str = "", repo: str = "", worktree: str = "",
                    branch: str = "", tmux_session: str = "") -> None:
    """Called by wt at worktree/session birth. The claude session id doesn't
    exist yet at this moment; SessionStart fills it in. We stamp the stream
    slug onto the eventual session via a pending-birth marker keyed by
    tmux session name."""
    if not tmux_session:
        return
    marker = config.STATE_DIR / "births" / f"{tmux_session}.json"
    try:
        marker.parent.mkdir(parents=True, exist_ok=True)
        import json
        marker.write_text(json.dumps({
            "slug": slug, "repo": repo, "worktree": worktree,
            "branch": branch, "ts": db.now(),
        }))
    except OSError:
        pass


def claim_birth(tmux_session: str) -> dict | None:
    """SessionStart consumes a pending wt birth marker for its tmux session."""
    if not tmux_session:
        return None
    marker = config.STATE_DIR / "births" / f"{tmux_session}.json"
    try:
        import json
        data = json.loads(marker.read_text())
        marker.unlink(missing_ok=True)
        return data
    except (OSError, ValueError):
        return None


def annotate_worktrees(rows: list[dict]) -> list[dict]:
    """Enrich `hive wt ls` rows with ledger knowledge (title, last activity).

    Rows carry branch/path/session/live; we add a `note` when the ledger has
    a matching live session. Fail-soft: any error returns rows unchanged.
    """
    try:
        import time
        with db.connect() as conn:
            db.ensure_hive_schema(conn)
            for row in rows:
                sess = row.get("session") or ""
                if not sess:
                    continue
                hit = conn.execute(
                    """SELECT title, last_activity, ended_at, stream_slug
                       FROM hive_sessions WHERE tmux_session=? AND machine=?
                       ORDER BY last_activity DESC LIMIT 1""",
                    (sess, config.machine_id()),
                ).fetchone()
                if not hit:
                    continue
                age_m = max(0, (db.now() - hit["last_activity"]) // 60)
                bits = []
                if hit["title"]:
                    bits.append(hit["title"][:60])
                bits.append(f"{age_m}m ago" if age_m < 600 else time.strftime(
                    "%m-%d", time.localtime(hit["last_activity"])))
                if hit["stream_slug"]:
                    bits.append(f"stream:{hit['stream_slug']}")
                row["note"] = " · ".join(bits)
    except Exception:
        pass
    return rows


def set_stream(session_id: str, slug: str, harness: str = "claude-code") -> None:
    with db.connect() as conn:
        db.ensure_hive_schema(conn)
        conn.execute(
            "UPDATE hive_sessions SET stream_slug=? "
            "WHERE harness=? AND session_id=? AND machine=?",
            (slug, harness, session_id, config.machine_id()),
        )
        conn.commit()
