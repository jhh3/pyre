"""Distilled-lane sync: NDJSON lanes in the streams vault (docs/PLAN.md §4).

Single-writer-per-lane is the entire conflict strategy: this machine appends
only under mem/lanes/<machine>/ and mem/tombstones/<machine>.ndjson; every
other machine's lanes are imported idempotently (uid + source_bytes) from
per-file byte cursors. Vault history is disposable: a meta/epoch bump forces
importers to hard-reset and re-import from zero (idempotent, so safe).

Redaction gate: every exported line passes through the vendored jhh3-mem
redact.sed before it touches the vault. sed failure aborts the export —
fail closed, dirty rows stay queued.

git hygiene: `git add` is always scoped to mem/ handoffs/ meta/ — streams/
belongs to the vendored ws sync and is never touched from here.
"""

from __future__ import annotations

import json
import sqlite3
import subprocess
import time
from pathlib import Path

from . import config, db, util

PLUGIN_ROOT = Path(__file__).resolve().parent.parent.parent
REDACT_SED = PLUGIN_ROOT / "vendor" / "jhh3-mem" / "lib" / "redact.sed"

# All 33 entries columns (id excluded on import: rowids are machine-local).
ENTRY_COLS = (
    "uid", "source_bytes", "origin", "producer", "host", "repo_key",
    "worktree", "branch", "repo_path", "kind", "ts", "started_at", "title",
    "request", "outcome", "learned", "decided", "next_steps", "facts",
    "concepts", "files_touched", "commands", "capture_mode", "end_reason",
    "turns", "redactions", "durability", "dedup_key", "n_mod", "n_read",
    "ephemeral", "injectable", "superseded",
)

SESSION_COLS = (
    "session_id", "harness", "machine", "repo_key", "worktree", "branch",
    "stream_slug", "tmux_session", "tmux_pane", "transcript_path", "title",
    "cwd", "started_at", "last_activity", "last_checkpoint_seq", "ended_at",
    "end_reason", "predecessor_session_id", "raw_synced_through", "no_ledger",
    "title_locked", "title_derivations", "last_title_at",
)


class SyncError(Exception):
    pass


# ── small helpers ───────────────────────────────────────────────────────────

def _iso() -> str:
    return time.strftime("%Y-%m-%dT%H:%M:%S")


def _git(vault: Path, *args: str, timeout: int = 30) -> subprocess.CompletedProcess:
    return util.run(["git", "-C", str(vault), *args], timeout=timeout)


def _has_remote(vault: Path) -> bool:
    r = _git(vault, "remote")
    return r.returncode == 0 and bool(r.stdout.strip())


def _current_branch(vault: Path) -> str:
    r = _git(vault, "rev-parse", "--abbrev-ref", "HEAD")
    b = r.stdout.strip()
    return b if r.returncode == 0 and b and b != "HEAD" else "main"


def _remote_branch(vault: Path) -> str:
    """origin's default branch: origin/HEAD, else main/master probe."""
    r = _git(vault, "rev-parse", "--abbrev-ref", "origin/HEAD")
    name = r.stdout.strip()
    if r.returncode == 0 and name.startswith("origin/"):
        return name.split("/", 1)[1]
    for cand in ("main", "master"):
        if _git(vault, "rev-parse", "--verify", "--quiet",
                f"origin/{cand}").returncode == 0:
            return cand
    return _current_branch(vault)


def _local_epoch(vault: Path) -> int:
    try:
        return int((vault / "meta" / "epoch").read_text().strip())
    except (OSError, ValueError):
        return 1


def _ensure_layout(vault: Path) -> None:
    machine = config.machine_id()
    (vault / "mem" / "lanes" / machine).mkdir(parents=True, exist_ok=True)
    (vault / "mem" / "tombstones").mkdir(parents=True, exist_ok=True)
    (vault / "meta").mkdir(parents=True, exist_ok=True)
    epoch = vault / "meta" / "epoch"
    if not epoch.exists():
        epoch.write_text("1\n")


def _read_machines(vault: Path) -> dict:
    try:
        data = json.loads((vault / "meta" / "machines.json").read_text())
        return data if isinstance(data, dict) else {}
    except (OSError, ValueError):
        return {}


def _write_machines(vault: Path, data: dict) -> None:
    (vault / "meta").mkdir(parents=True, exist_ok=True)
    (vault / "meta" / "machines.json").write_text(
        json.dumps(data, indent=1, sort_keys=True) + "\n")


def _redact_lines(lines: list[str]) -> list[str]:
    """One sed subprocess for the whole batch, line-per-record. Fail closed."""
    if not REDACT_SED.exists():
        raise SyncError(f"redaction ruleset missing: {REDACT_SED}")
    try:
        proc = subprocess.run(
            ["sed", "-E", "-f", str(REDACT_SED)],
            input="\n".join(lines) + "\n",
            capture_output=True, text=True, timeout=60,
        )
    except (OSError, subprocess.TimeoutExpired, UnicodeError) as e:
        raise SyncError(f"redaction sed failed: {e!r}")
    if proc.returncode != 0:
        raise SyncError(f"redaction sed rc={proc.returncode}: "
                        f"{proc.stderr.strip()[:200]}")
    out = proc.stdout.splitlines()
    if len(out) != len(lines):
        raise SyncError("redaction sed line-count mismatch — aborting export")
    if any(src and not red for src, red in zip(lines, out)):
        raise SyncError("redaction sed emptied a record — aborting export")
    # The sed rules are text-oriented and can eat JSON structure (a key=value
    # redaction may span an escaped quote). Entry content was already redacted
    # at capture time, so on corruption keep the pre-sed line — never emit an
    # unparseable record into a lane.
    fixed = []
    for src, red in zip(lines, out):
        if red is not src and red != src:
            try:
                json.loads(red)
            except ValueError:
                red = src
        fixed.append(red)
    return fixed


# ── export ──────────────────────────────────────────────────────────────────

def export_dirty(conn: sqlite3.Connection) -> int:
    """Append dirty entries + recently-ended sessions to this machine's lane.

    Order of operations: serialize → redact (fail closed) → append to lane
    files → commit DB bookkeeping (revs, dirty clear). A crash between the
    file append and the commit re-exports duplicate lines next time; import
    is idempotent so that is safe, the reverse order would lose records.
    """
    machine = config.machine_id()
    vault = config.STREAMS_VAULT
    now = db.now()

    # Dirty rows whose entry has since been deleted can never export; drop.
    conn.execute("DELETE FROM hive_dirty WHERE entry_id NOT IN "
                 "(SELECT id FROM entries)")

    rows = conn.execute(
        "SELECT e.* FROM entries e JOIN hive_dirty d ON d.entry_id = e.id "
        "ORDER BY e.id").fetchall()

    revs: dict[str, int] = {}

    def next_rev(key: str) -> int:
        if key not in revs:
            cur = conn.execute("SELECT rev FROM hive_revs WHERE uid=?",
                               (key,)).fetchone()
            revs[key] = cur["rev"] if cur else 0
        revs[key] += 1
        return revs[key]

    records: list[tuple[str, dict]] = []  # (lane relpath, record)
    entry_ids: list[int] = []
    for r in rows:
        entry = {k: r[k] for k in r.keys()}
        files = conn.execute(
            "SELECT p.p AS p, ef.mode AS mode FROM entry_file ef "
            "JOIN path p ON p.id = ef.path_id WHERE ef.entry_id=?",
            (r["id"],)).fetchall()
        entry["files"] = [[f["p"], f["mode"]] for f in files]
        month = time.strftime("%Y-%m", time.gmtime(r["ts"] or now))
        records.append((
            f"mem/lanes/{machine}/{month}.ndjson",
            {"op": "put", "uid": r["uid"], "rev": next_rev(r["uid"]),
             "machine": machine, "ts": r["ts"], "entry": entry},
        ))
        entry_ids.append(r["id"])

    # Sessions whose ended_at changed since our last export.
    machines_meta = _read_machines(vault)
    last_ts = int(machines_meta.get(machine, {}).get("last_export_ts", 0))
    month_now = time.strftime("%Y-%m", time.gmtime(now))
    for s in conn.execute(
            "SELECT * FROM hive_sessions WHERE machine=? "
            "AND ended_at IS NOT NULL AND ended_at > ?", (machine, last_ts)):
        skey = f"session:{s['harness']}:{s['session_id']}"
        records.append((
            f"mem/lanes/{machine}/{month_now}.ndjson",
            {"op": "session", "rev": next_rev(skey), "machine": machine,
             "session": {k: s[k] for k in SESSION_COLS}},
        ))

    if not records:
        return 0

    _ensure_layout(vault)
    lines = [json.dumps(rec, separators=(",", ":")) for _, rec in records]
    redacted = _redact_lines(lines)  # raises SyncError → nothing written

    # Append per lane, preserving record order.
    by_lane: dict[str, list[str]] = {}
    for (rel, _), line in zip(records, redacted):
        by_lane.setdefault(rel, []).append(line)
    for rel, chunk in by_lane.items():
        lane = vault / rel
        lane.parent.mkdir(parents=True, exist_ok=True)
        with open(lane, "a", encoding="utf-8") as f:
            f.write("\n".join(chunk) + "\n")

    for key, rev in revs.items():
        conn.execute(
            "INSERT INTO hive_revs(uid, rev) VALUES(?,?) "
            "ON CONFLICT(uid) DO UPDATE SET rev=excluded.rev", (key, rev))
    for eid in entry_ids:
        conn.execute("DELETE FROM hive_dirty WHERE entry_id=?", (eid,))
    conn.commit()

    machines_meta.setdefault(machine, {})["last_export_ts"] = now
    _write_machines(vault, machines_meta)
    return len(records)


def write_tombstone(uid: str, vault: Path | None = None) -> None:
    """Append a tombstone to this machine's tombstone lane (forget flow)."""
    vault = vault or config.STREAMS_VAULT
    machine = config.machine_id()
    _ensure_layout(vault)
    rec = {"op": "tombstone", "uid": uid, "machine": machine, "ts": db.now()}
    with open(vault / "mem" / "tombstones" / f"{machine}.ndjson", "a",
              encoding="utf-8") as f:
        f.write(json.dumps(rec, separators=(",", ":")) + "\n")


# ── import ──────────────────────────────────────────────────────────────────

def _apply_put(conn: sqlite3.Connection, rec: dict) -> None:
    entry = dict(rec.get("entry") or {})
    # Live records nest files under entry; history records carry them at the
    # record top level. Accept both.
    files = entry.pop("files", None) or rec.get("files") or []
    row = {k: entry[k] for k in ENTRY_COLS if k in entry}
    if not row.get("uid"):
        return
    # Track the highest rev seen per uid so a later LOCAL edit of an imported
    # entry exports at rev+1 (not a rev importers will skip as already-seen).
    try:
        rev = int(rec.get("rev") or 0)
    except (TypeError, ValueError):
        rev = 0
    if rev:
        conn.execute(
            "INSERT INTO hive_revs(uid, rev) VALUES(?,?) "
            "ON CONFLICT(uid) DO UPDATE SET rev=MAX(rev, excluded.rev)",
            (row["uid"], rev))
    cols = list(row)
    conn.execute(
        f"INSERT OR IGNORE INTO entries({','.join(cols)}) "
        f"VALUES({','.join('?' * len(cols))})",
        [row[c] for c in cols])
    got = conn.execute("SELECT id FROM entries WHERE uid=? AND source_bytes=?",
                       (row["uid"], row.get("source_bytes", 0))).fetchone()
    if not got:
        return
    # Mirror the vendored supersede invariant: largest source_bytes per uid
    # is current (covers both directions — an older import never demotes a
    # newer local row).
    conn.execute(
        "UPDATE entries SET superseded=1 WHERE uid=? AND superseded=0 "
        "AND source_bytes < (SELECT MAX(source_bytes) FROM entries "
        "                    WHERE uid=?)",
        (row["uid"], row["uid"]))
    for item in files:
        try:
            p, mode = str(item[0]), int(item[1])
        except (TypeError, ValueError, IndexError):
            continue
        base = p.rsplit("/", 1)[-1]
        conn.execute("INSERT OR IGNORE INTO path(p, base) VALUES(?,?)",
                     (p, base))
        pid = conn.execute("SELECT id FROM path WHERE p=?", (p,)).fetchone()
        if pid:
            conn.execute(
                "INSERT OR IGNORE INTO entry_file(path_id, entry_id, mode) "
                "VALUES(?,?,?)", (pid["id"], got["id"], mode))


def _apply_session(conn: sqlite3.Connection, rec: dict) -> None:
    s = dict(rec.get("session") or {})
    row = {k: s[k] for k in SESSION_COLS if k in s}
    # Keyed by the RECORD's machine, not ours.
    if not (row.get("session_id") and row.get("harness") and row.get("machine")):
        return
    cols = list(row)
    conn.execute(
        f"INSERT OR REPLACE INTO hive_sessions({','.join(cols)}) "
        f"VALUES({','.join('?' * len(cols))})",
        [row[c] for c in cols])


def _apply_tombstone(conn: sqlite3.Connection, rec: dict) -> None:
    uid = rec.get("uid")
    if not uid:
        return
    ids = [r["id"] for r in
           conn.execute("SELECT id FROM entries WHERE uid=?", (uid,))]
    for eid in ids:
        # Explicit child deletes: PRAGMA foreign_keys defaults OFF, so the
        # ON DELETE CASCADE on entry_file cannot be relied upon here.
        conn.execute("DELETE FROM entry_file WHERE entry_id=?", (eid,))
        conn.execute("DELETE FROM hive_dirty WHERE entry_id=?", (eid,))
    conn.execute("DELETE FROM entries WHERE uid=?", (uid,))


def import_lanes(conn: sqlite3.Connection) -> int:
    """Import new complete lines from every foreign lane. Returns records applied."""
    machine = config.machine_id()
    vault = config.STREAMS_VAULT
    lanes: list[Path] = []
    lanes_dir = vault / "mem" / "lanes"
    if lanes_dir.is_dir():
        for mdir in sorted(lanes_dir.iterdir()):
            if mdir.is_dir() and mdir.name != machine:
                lanes.extend(sorted(mdir.glob("*.ndjson")))
    tomb_dir = vault / "mem" / "tombstones"
    if tomb_dir.is_dir():
        lanes.extend(p for p in sorted(tomb_dir.glob("*.ndjson"))
                     if p.stem != machine)

    total = 0
    for lane in lanes:
        rel = lane.relative_to(vault).as_posix()
        cur = conn.execute("SELECT byte_off FROM hive_lane_cursors "
                           "WHERE lane_path=?", (rel,)).fetchone()
        off = cur["byte_off"] if cur else 0
        try:
            size = lane.stat().st_size
        except OSError:
            continue
        if off > size:  # lane rewritten under us — re-read (idempotent)
            util.log(config.TICK_LOG,
                     f"sync import: {rel} shrank; resetting cursor")
            off = 0
        if off >= size:
            continue
        with open(lane, "rb") as f:
            f.seek(off)
            chunk = f.read()
        nl = chunk.rfind(b"\n")
        if nl < 0:  # no complete new line yet
            continue
        chunk = chunk[:nl + 1]
        new_off = off + len(chunk)

        touched_uids: set[str] = set()
        for rawline in chunk.split(b"\n"):
            if not rawline.strip():
                continue
            rec = util.read_json_line(rawline.decode("utf-8", errors="replace"))
            if not isinstance(rec, dict):
                util.log(config.TICK_LOG, f"sync import: bad line in {rel}")
                continue
            op = rec.get("op")
            try:
                if op == "put":
                    _apply_put(conn, rec)
                    touched_uids.add(rec.get("uid") or "")
                elif op == "session":
                    _apply_session(conn, rec)
                elif op == "tombstone":
                    _apply_tombstone(conn, rec)
                else:
                    util.log(config.TICK_LOG,
                             f"sync import: unknown op {op!r} in {rel}")
                    continue
                total += 1
            except sqlite3.Error as e:
                util.log(config.TICK_LOG, f"sync import: {rel}: {e!r}")

        # Echo prevention: the entries triggers marked imported rows dirty,
        # which would re-export foreign entries into our lane. Scrub every
        # entry id belonging to a uid this batch touched (covers the rows
        # the supersede invariant just updated, too).
        touched_uids.discard("")
        for uid in touched_uids:
            conn.execute(
                "DELETE FROM hive_dirty WHERE entry_id IN "
                "(SELECT id FROM entries WHERE uid=?)", (uid,))
        conn.commit()
        # Cursor advances only after the data commit: a crash in between
        # re-imports (idempotent) instead of skipping.
        conn.execute(
            "INSERT INTO hive_lane_cursors(lane_path, byte_off, updated_at) "
            "VALUES(?,?,?) ON CONFLICT(lane_path) DO UPDATE SET "
            "byte_off=excluded.byte_off, updated_at=excluded.updated_at",
            (rel, new_off, db.now()))
        conn.commit()
    return total


# ── git: push / pull ────────────────────────────────────────────────────────

def push(vault: Path, min_interval_s: int = 0) -> bool:
    """Scoped add + commit + push with rebase-retry. Single-flight + debounce."""
    machine = config.machine_id()
    if not (vault / ".git").exists():
        return False
    with util.flocked(config.STATE_DIR / "sync.lock") as got:
        if not got:
            util.log(config.TICK_LOG, "sync push: already in flight; skipping")
            return False
        stamp = config.STATE_DIR / "sync.push.stamp"
        now = time.time()
        try:
            last = float(stamp.read_text().strip())
        except (OSError, ValueError):
            last = 0.0
        if min_interval_s and now - last < min_interval_s:
            return False
        # NEVER add streams/ — the vendored ws sync owns those paths.
        scopes = [p for p in ("mem", "handoffs", "meta")
                  if (vault / p).exists()]
        if scopes:
            _git(vault, "add", "-A", "--", *scopes)
        if _git(vault, "diff", "--cached", "--quiet").returncode != 0:
            c = _git(vault, "commit", "-m", f"hive sync {_iso()} {machine}")
            if c.returncode != 0:
                util.log(config.TICK_LOG,
                         f"sync commit failed: {c.stderr.strip()[:200]}")
                return False
        if not _has_remote(vault):
            return True  # local-only vault: committed, nothing to push
        branch = _current_branch(vault)
        p = None
        for _ in range(2):
            p = _git(vault, "push", "origin", branch, timeout=60)
            if p.returncode == 0:
                try:
                    config.STATE_DIR.mkdir(parents=True, exist_ok=True)
                    stamp.write_text(str(now))
                except OSError:
                    pass
                return True
            _git(vault, "pull", "--rebase", "--autostash", "origin", branch, timeout=60)
        util.log(config.TICK_LOG,
                 f"sync push failed after retry: "
                 f"{(p.stderr if p else '').strip()[:200]}")
        return False


def pull(vault: Path, conn: sqlite3.Connection | None = None) -> None:
    """fetch + epoch check; hard-reset + cursor zero on epoch bump, else rebase."""
    if not (vault / ".git").exists() or not _has_remote(vault):
        return
    f = _git(vault, "fetch", "--quiet", "origin", timeout=30)
    if f.returncode != 0:
        util.log(config.TICK_LOG,
                 f"sync fetch failed: {f.stderr.strip()[:200]}")
        return
    branch = _remote_branch(vault)
    remote_epoch = None
    show = _git(vault, "show", f"origin/{branch}:meta/epoch")
    if show.returncode == 0:
        try:
            remote_epoch = int(show.stdout.strip())
        except ValueError:
            pass
    local_epoch = _local_epoch(vault)
    if remote_epoch is not None and remote_epoch > local_epoch:
        msg = (f"EPOCH RESET {local_epoch} -> {remote_epoch}: hard reset to "
               f"origin/{branch}; zeroing all lane cursors (full reimport)")
        util.log(config.TICK_LOG, f"sync pull: {msg}")
        util.eprint(f"hive sync: {msg}")
        _git(vault, "reset", "--hard", f"origin/{branch}", timeout=30)
        own = conn or db.connect()
        try:
            db.ensure_hive_schema(own)
            own.execute("UPDATE hive_lane_cursors SET byte_off=0, "
                        "updated_at=?", (db.now(),))
            own.commit()
        finally:
            if conn is None:
                own.close()
        return
    r = _git(vault, "pull", "--rebase", "--autostash", "origin", branch, timeout=30)
    if r.returncode != 0:
        util.log(config.TICK_LOG,
                 f"sync pull failed: {r.stderr.strip()[:200]}")


def squash(vault: Path) -> int:
    """Single-commit squash of vault history + epoch bump + force-push."""
    machine = config.machine_id()
    if not (vault / ".git").exists():
        util.eprint("hive sync: no git repo at vault")
        return 1
    branch = _current_branch(vault)
    epoch = _local_epoch(vault) + 1
    (vault / "meta").mkdir(parents=True, exist_ok=True)
    (vault / "meta" / "epoch").write_text(f"{epoch}\n")
    steps = [
        ["checkout", "--orphan", "hive-squash-tmp"],
        ["add", "-A"],
        ["commit", "-m", f"hive squash {_iso()} epoch={epoch} {machine}"],
        ["branch", "-M", branch],
    ]
    for step in steps:
        r = _git(vault, *step, timeout=60)
        if r.returncode != 0:
            util.eprint(f"hive sync --squash: git {' '.join(step)} failed: "
                        f"{r.stderr.strip()[:200]}")
            return 1
    if _has_remote(vault):
        r = _git(vault, "push", "--force", "origin", branch, timeout=60)
        if r.returncode != 0:
            util.eprint(f"hive sync --squash: force-push failed: "
                        f"{r.stderr.strip()[:200]}")
            return 1
    print(f"vault history squashed; epoch -> {epoch} "
          f"(importers will hard-reset and re-import)")
    return 0


# ── tick phase / CLI ────────────────────────────────────────────────────────

def tick_phase() -> None:
    """pull → import_lanes → export_dirty → push. Every step fail-soft."""
    vault = config.STREAMS_VAULT
    try:
        conn = db.connect()
        db.ensure_hive_schema(conn)
    except Exception as e:
        util.log(config.TICK_LOG, f"sync: db unavailable: {e!r}")
        return
    try:
        try:
            pull(vault, conn)
        except Exception as e:
            util.log(config.TICK_LOG, f"sync pull crashed: {e!r}")
        try:
            n = import_lanes(conn)
            if n:
                util.log(config.TICK_LOG, f"sync: imported {n} record(s)")
        except Exception as e:
            util.log(config.TICK_LOG, f"sync import crashed: {e!r}")
        try:
            n = export_dirty(conn)
            if n:
                util.log(config.TICK_LOG, f"sync: exported {n} record(s)")
        except SyncError as e:
            util.log(config.TICK_LOG, f"sync export ABORTED (fail closed): {e}")
        except Exception as e:
            util.log(config.TICK_LOG, f"sync export crashed: {e!r}")
        try:
            debounce = int(config.load()["sync"]["close_debounce_s"])
            push(vault, debounce)
        except Exception as e:
            util.log(config.TICK_LOG, f"sync push crashed: {e!r}")
    finally:
        conn.close()


def run(argv: list[str]) -> int:
    """hive sync [--push-only|--pull-only|--squash --yes]"""
    vault = config.STREAMS_VAULT
    if "--squash" in argv:
        if "--yes" not in argv:
            util.eprint("hive sync --squash rewrites vault history "
                        "(orphan commit + force-push + epoch bump). "
                        "Re-run with --yes to confirm.")
            return 2
        return squash(vault)
    conn = db.connect()
    db.ensure_hive_schema(conn)
    try:
        if "--pull-only" in argv:
            pull(vault, conn)
            n = import_lanes(conn)
            print(f"hive sync: imported {n} record(s)")
            return 0
        if "--push-only" in argv:
            try:
                n = export_dirty(conn)
            except SyncError as e:
                util.eprint(f"hive sync: export aborted: {e}")
                return 1
            ok = push(vault, 0)
            print(f"hive sync: exported {n} record(s); "
                  f"push {'ok' if ok else 'failed/skipped'}")
            return 0 if ok else 1
        pull(vault, conn)
        ni = import_lanes(conn)
        try:
            ne = export_dirty(conn)
        except SyncError as e:
            util.eprint(f"hive sync: export aborted: {e}")
            return 1
        ok = push(vault, 0)
        print(f"hive sync: imported {ni}, exported {ne}, "
              f"push {'ok' if ok else 'failed/skipped'}")
        return 0 if ok else 1
    finally:
        conn.close()
