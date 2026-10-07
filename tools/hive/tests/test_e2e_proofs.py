"""End-to-end proofs: second-machine bootstrap, crash/reboot recovery, and
the planted-secrets redaction gate.

Same hermeticity rules as test_sync/test_lifecycle: real git repos under
tmp_path (a bare "origin" as the only remote), real sqlite DBs built from the
vendored jhh3-mem schema plus the hive schema, real sed. Never the live DB,
crontab, ~/streams, or tmux — config paths, HIVE_MACHINE and HIVE_DB are
monkeypatched per test.

Run: cd <repo> && python3 -m pytest tests/test_e2e_proofs.py -q
"""

from __future__ import annotations

import json
import sqlite3
import subprocess
import sys
import time
from pathlib import Path

import pytest

REPO = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(REPO / "plugins" / "hive" / "lib"))

from hive import admin, config, db, hooks, sync, tick, util, vendored  # noqa: E402

SCHEMA_SQL = REPO / "plugins" / "hive" / "vendor" / "jhh3-mem" / "db" / "schema.sql"


def git(cwd: Path, *args: str) -> str:
    r = subprocess.run(["git", "-C", str(cwd), *args],
                       capture_output=True, text=True, timeout=30)
    assert r.returncode == 0, f"git {args} failed: {r.stderr}"
    return r.stdout


def make_db(path: Path) -> sqlite3.Connection:
    conn = sqlite3.connect(path)
    conn.row_factory = sqlite3.Row
    conn.executescript(SCHEMA_SQL.read_text())
    db.ensure_hive_schema(conn)
    return conn


ENTRY = dict(
    uid="sess-e2e-000", source_bytes=1000, origin="live",
    producer="jhh3-mem/0.4.5", host="claude-code", repo_key="jhh3/hive",
    worktree="wt-one", branch="main", repo_path="/x/hive", kind="session",
    ts=1756200000, started_at=1756190000, title="Baseline entry",
    request="please do the thing", outcome="did the thing",
    learned="things are learnable", decided="approach A", next_steps="none",
    facts="", concepts="", files_touched="", commands="",
    capture_mode="deterministic", end_reason="clear", turns=3, redactions=0,
    durability=1.0, dedup_key="", n_mod=1, n_read=2,
    ephemeral=0, injectable=1, superseded=0,
)


def insert_entry(conn: sqlite3.Connection, **over) -> int:
    e = {**ENTRY, **over}
    cols = ",".join(e)
    conn.execute(f"INSERT INTO entries({cols}) "
                 f"VALUES({','.join('?' * len(e))})", list(e.values()))
    conn.commit()
    return conn.execute("SELECT id FROM entries WHERE uid=? AND source_bytes=?",
                        (e["uid"], e["source_bytes"])).fetchone()["id"]


def add_file(conn: sqlite3.Connection, entry_id: int, p: str, mode: int) -> None:
    conn.execute("INSERT OR IGNORE INTO path(p, base) VALUES(?,?)",
                 (p, p.rsplit("/", 1)[-1]))
    pid = conn.execute("SELECT id FROM path WHERE p=?", (p,)).fetchone()["id"]
    conn.execute("INSERT OR IGNORE INTO entry_file(path_id, entry_id, mode) "
                 "VALUES(?,?,?)", (pid, entry_id, mode))
    conn.commit()


@pytest.fixture
def hermetic(tmp_path, monkeypatch):
    """Neutral config: every path hive can touch is under tmp_path."""
    for var, val in (("GIT_AUTHOR_NAME", "hive-test"),
                     ("GIT_AUTHOR_EMAIL", "hive-test@local"),
                     ("GIT_COMMITTER_NAME", "hive-test"),
                     ("GIT_COMMITTER_EMAIL", "hive-test@local"),
                     ("GIT_CONFIG_GLOBAL", str(tmp_path / "gitconfig")),
                     ("GIT_CONFIG_SYSTEM", "/dev/null")):
        monkeypatch.setenv(var, val)
    monkeypatch.delenv("TMUX_PANE", raising=False)
    monkeypatch.setenv("HIVE_MACHINE", "macha")
    state = tmp_path / "state"
    data = tmp_path / "data"
    monkeypatch.setattr(config, "CONFIG_PATH", tmp_path / "absent.toml")
    monkeypatch.setattr(config, "DATA_DIR", data)
    monkeypatch.setattr(config, "STATE_DIR", state)
    monkeypatch.setattr(config, "DB_PATH", data / "index.db")
    monkeypatch.setattr(config, "QUARANTINE_DIR", data / "raw" / "quarantine")
    monkeypatch.setattr(config, "SNAPSHOT_DIR", data / "raw" / "snapshots")
    monkeypatch.setattr(config, "HOOK_LOG", state / "hooks.log")
    monkeypatch.setattr(config, "TICK_LOG", state / "tick.log")
    monkeypatch.setattr(config, "TICK_LOCK", state / "tick.lock")
    monkeypatch.setattr(config, "STREAMS_VAULT", tmp_path / "streams")
    monkeypatch.setattr(config, "RAW_VAULT", tmp_path / "vault-raw")
    monkeypatch.setattr(config, "CLAUDE_PROJECTS", tmp_path / "claude-projects")
    monkeypatch.setattr(config, "CODEX_SESSIONS", tmp_path / "codex-sessions")
    monkeypatch.setattr(db, "JHH3_MEM_DB", tmp_path / "absent-mem.db")
    config.load.cache_clear()
    yield tmp_path
    config.load.cache_clear()


# ── 1. second-machine bootstrap ────────────────────────────────────────────

GOLDEN_UIDS = ("sess-e2e-003", "sess-e2e-007", "sess-e2e-011",
               "sess-e2e-015", "sess-e2e-019")


def test_second_machine_bootstrap(hermetic, monkeypatch):
    tmp = hermetic
    # Machine A: DB with ~20 entries (files on some) + 2 ended sessions.
    conn_a = make_db(tmp / "a.db")
    monkeypatch.setenv("HIVE_DB", str(tmp / "a.db"))
    for i in range(20):
        eid = insert_entry(
            conn_a, uid=f"sess-e2e-{i:03d}", source_bytes=1000 + i,
            ts=1756200000 + i * 3600,
            title=f"Task {i}: fix the zanzibar widget" if i == 7
            else f"Task {i} title",
            learned=f"lesson number {i}",
            outcome=f"outcome for task {i}")
        if i % 4 == 0:
            add_file(conn_a, eid, f"src/mod{i}.py", 2)
            add_file(conn_a, eid, "README.md", 1)
    now = int(time.time())
    for n, sid in enumerate(("sess-e2e-003", "sess-e2e-007")):
        conn_a.execute(
            """INSERT INTO hive_sessions
               (session_id, harness, machine, started_at, last_activity,
                ended_at, end_reason)
               VALUES (?,?,?,?,?,?,?)""",
            (sid, "claude-code", "macha", now - 500, now - 10 - n,
             now - 5 - n, "clean"))
    conn_a.commit()

    # Vault A: git repo with a bare origin, filled by export_dirty + push.
    bare = tmp / "origin.git"
    subprocess.run(["git", "init", "-q", "--bare", "-b", "main", str(bare)],
                   check=True, timeout=30)
    vault_a = config.STREAMS_VAULT
    vault_a.mkdir()
    git(vault_a, "init", "-q", "-b", "main")
    git(vault_a, "remote", "add", "origin", str(bare))
    n = sync.export_dirty(conn_a)
    assert n == 22  # 20 puts + 2 sessions
    assert sync.push(vault_a, 0)

    # Machine B: fresh clone, fresh empty DB via the rebuild path.
    clone = tmp / "vault-b"
    subprocess.run(["git", "clone", "-q", str(bare), str(clone)],
                   check=True, timeout=30)
    monkeypatch.setattr(config, "STREAMS_VAULT", clone)
    dst = tmp / "b.db"
    assert admin.cmd_rebuild(["--to", str(dst)]) == 0

    conn_b = sqlite3.connect(dst)
    conn_b.row_factory = sqlite3.Row
    try:
        # Golden query 1: entry count matches.
        count_a = conn_a.execute("SELECT count(*) FROM entries").fetchone()[0]
        count_b = conn_b.execute("SELECT count(*) FROM entries").fetchone()[0]
        assert count_b == count_a == 20
        # Golden query 2: specific uids carry identical title/learned.
        for uid in GOLDEN_UIDS:
            a = conn_a.execute(
                "SELECT title, learned FROM entries WHERE uid=?",
                (uid,)).fetchone()
            b = conn_b.execute(
                "SELECT title, learned FROM entries WHERE uid=?",
                (uid,)).fetchone()
            assert b is not None, f"{uid} missing after rebuild"
            assert (b["title"], b["learned"]) == (a["title"], a["learned"])
        # Golden query 3: FTS5 MATCH returns the same uid on both sides.
        q = ("SELECT e.uid FROM entries_fts JOIN entries e "
             "ON e.id = entries_fts.rowid WHERE entries_fts MATCH 'zanzibar'")
        hits_a = sorted(r["uid"] for r in conn_a.execute(q))
        hits_b = sorted(r["uid"] for r in conn_b.execute(q))
        assert hits_a == hits_b == ["sess-e2e-007"]
        # Golden query 4: entry_file join intact.
        q = ("SELECT p.p AS p, ef.mode AS mode FROM entry_file ef "
             "JOIN path p ON p.id = ef.path_id "
             "JOIN entries e ON e.id = ef.entry_id WHERE e.uid=?")
        for uid in ("sess-e2e-000", "sess-e2e-016"):
            files_a = {(r["p"], r["mode"]) for r in conn_a.execute(q, (uid,))}
            files_b = {(r["p"], r["mode"]) for r in conn_b.execute(q, (uid,))}
            assert files_b == files_a and files_b
    finally:
        conn_a.close()
        conn_b.close()


# ── 2. crash / reboot flow ─────────────────────────────────────────────────

class FakeProc:
    def __init__(self, returncode=0, stdout="", stderr=""):
        self.returncode = returncode
        self.stdout = stdout
        self.stderr = stderr


def test_crash_reboot_flow(hermetic, monkeypatch, capsys):
    tmp = hermetic
    monkeypatch.setenv("HIVE_DB", str(config.DB_PATH))
    config.ensure_dirs()
    with db.connect() as conn:
        # Minimal vendored-owned entries table (the columns hive reads).
        conn.execute("""CREATE TABLE IF NOT EXISTS entries (
            id INTEGER PRIMARY KEY,
            uid TEXT NOT NULL,
            source_bytes INTEGER NOT NULL DEFAULT 0,
            ts INTEGER NOT NULL DEFAULT 0,
            title TEXT NOT NULL DEFAULT '',
            next_steps TEXT NOT NULL DEFAULT '',
            files_touched TEXT NOT NULL DEFAULT '')""")
        db.ensure_hive_schema(conn)
        now = int(time.time())
        cwd1, cwd2 = str(tmp / "proj1"), str(tmp / "proj2")
        for sid, cwd in (("s-crash-1", cwd1), ("s-crash-2", cwd2)):
            Path(cwd).mkdir(exist_ok=True)
            t = tmp / f"{sid}.jsonl"
            t.write_bytes(b'{"type":"assistant"}\n' * 20)
            conn.execute(
                """INSERT INTO hive_sessions
                   (session_id, harness, machine, cwd, transcript_path,
                    started_at, last_activity)
                   VALUES (?,?,?,?,?,?,?)""",
                (sid, "claude-code", "macha", cwd, str(t), now - 300, now))
        conn.execute(
            "INSERT INTO entries (uid, ts, title, next_steps, files_touched) "
            "VALUES (?,?,?,?,?)",
            ("s-crash-1", now, "Half-done refactor",
             "wire the frobnicator into main", "src/frob.py\nsrc/main.py"))
        conn.commit()

    mem_calls: list[list[str]] = []
    monkeypatch.setattr(vendored, "mem",
                        lambda a, timeout=60, capture=True:
                        mem_calls.append(list(a)) or FakeProc())
    ws_calls: list[list[str]] = []
    monkeypatch.setattr(vendored, "ws",
                        lambda a, timeout=60, capture=True:
                        ws_calls.append(list(a)) or FakeProc())
    # Sync is "absent" at boot: the reaper's vault pull must be a no-op.
    monkeypatch.setattr(sync, "pull_once", lambda: None, raising=False)

    assert tick.run(["--boot", "--phases", "reap"]) == 0

    with db.connect() as conn:
        rows = {r["session_id"]: r for r in conn.execute(
            "SELECT * FROM hive_sessions")}
    for sid in ("s-crash-1", "s-crash-2"):
        assert rows[sid]["ended_at"] is not None
        assert rows[sid]["end_reason"] == "interrupted"
    # Finalize attempted for both (vendored capture with the boot reason).
    reasons = [c[c.index("--reason") + 1] for c in mem_calls if "--reason" in c]
    assert reasons == ["interrupted", "interrupted"]
    assert ["up"] in ws_calls  # fleet resume attempted

    # New session in the same cwd gets the interruption brief injected.
    capsys.readouterr()  # drop tick noise
    hooks.on_session_start({"session_id": "s-new", "source": "startup",
                            "cwd": cwd1})
    out = capsys.readouterr().out
    assert "hive-interruption-brief" in out
    payload = json.loads(out.strip().splitlines()[-1])
    ctx = payload["hookSpecificOutput"]["additionalContext"]
    assert "interrupted" in ctx
    assert "wire the frobnicator into main" in ctx
    assert "src/frob.py" in ctx

    # Single-shot: the session is marked crashed-briefed; no second brief.
    with db.connect() as conn:
        row = conn.execute(
            "SELECT end_reason FROM hive_sessions WHERE session_id=?",
            ("s-crash-1",)).fetchone()
    assert row["end_reason"] == "crashed-briefed"
    hooks.on_session_start({"session_id": "s-new2", "source": "startup",
                            "cwd": cwd1})
    assert "hive-interruption-brief" not in capsys.readouterr().out


# ── 3. planted secrets never reach the vault ───────────────────────────────

FAKE_OPENAI = "sk-proj-abc123456789abcdefghijklmnop"
FAKE_GHP = "ghp_ABCDEFGHIJKLMNOPQRSTUVWXyz0123456789"  # ghp_ + 36 chars
FAKE_AWS = "AKIAIOSFODNN7EXAMPLE"                       # AKIA + 16 chars
FAKE_KV = "password=hunter2secret"

SECRET_LITERALS = ("abc123456789abcdefghijklmnop",
                   "ABCDEFGHIJKLMNOPQRSTUVWXyz0123456789",
                   "AKIAIOSFODNN7EXAMPLE", "hunter2secret")


@pytest.mark.secrets
def test_planted_secrets_never_reach_vault(hermetic, monkeypatch):
    tmp = hermetic
    conn = make_db(tmp / "secrets.db")
    monkeypatch.setenv("HIVE_DB", str(tmp / "secrets.db"))
    insert_entry(conn, uid="sess-sec-1", source_bytes=10,
                 outcome=f"pasted the key {FAKE_OPENAI} while debugging",
                 learned=f"gh token {FAKE_GHP} must rotate")
    insert_entry(conn, uid="sess-sec-2", source_bytes=20,
                 outcome=f"aws creds were {FAKE_AWS} in the env",
                 learned=f"login used {FAKE_KV} the whole time")

    assert sync.export_dirty(conn) == 2
    conn.close()

    lanes = sorted(config.STREAMS_VAULT.glob("mem/**/*.ndjson"))
    assert lanes, "export wrote no lane files"
    all_text = ""
    for lane in lanes:
        text = lane.read_text()
        all_text += text
        for line in text.splitlines():
            if not line.strip():
                continue
            json.loads(line)  # every lane line must be valid JSON
    for literal in SECRET_LITERALS:
        assert literal not in all_text, f"secret leaked to vault: {literal}"
    assert "[REDACTED" in all_text
    # The identifying prefixes survive redaction (names stay searchable).
    assert "password=" in all_text
