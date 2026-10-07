"""Core module tests: config, util, db, ledger, hook handlers.

All state under tmp dirs — never the live DB, streams, or crontab.
"""

import json
import os
import sqlite3
import sys
from pathlib import Path

import pytest

REPO = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(REPO / "plugins/hive/lib"))

from hive import config, db, util  # noqa: E402


@pytest.fixture()
def tmp_env(tmp_path, monkeypatch):
    """Point every path-bearing module attribute at tmp."""
    monkeypatch.setattr(config, "DATA_DIR", tmp_path / "data")
    monkeypatch.setattr(config, "STATE_DIR", tmp_path / "state")
    monkeypatch.setattr(config, "DB_PATH", tmp_path / "data/index.db")
    monkeypatch.setattr(config, "HOOK_LOG", tmp_path / "state/hooks.log")
    monkeypatch.setattr(config, "SNAPSHOT_DIR", tmp_path / "data/raw/snapshots")
    monkeypatch.setattr(config, "QUARANTINE_DIR", tmp_path / "data/raw/q")
    monkeypatch.setattr(db, "JHH3_MEM_DB", tmp_path / "nonexistent-mem.db")
    monkeypatch.setenv("HIVE_MACHINE", "testbox")
    config.load.cache_clear()
    return tmp_path


def _mkdb(tmp_env):
    conn = db.connect(config.DB_PATH)
    schema = (REPO / "plugins/hive/vendor/jhh3-mem/db/schema.sql").read_text()
    conn.executescript(schema)
    db.ensure_hive_schema(conn)
    return conn


def test_ulid_sortable_and_unique():
    a, b = util.ulid(1000.0), util.ulid(2000.0)
    assert len(a) == 26 and a < b
    assert util.new_uid().startswith("testbox-") or "-" in util.new_uid()


def test_config_defaults_survive_missing_toml(tmp_env, monkeypatch):
    monkeypatch.setattr(config, "CONFIG_PATH", tmp_env / "missing.toml")
    config.load.cache_clear()
    cfg = config.load()
    assert cfg["checkpoint"]["min_new_bytes"] == 8192
    config.load.cache_clear()


def test_hive_schema_additive_and_idempotent(tmp_env):
    conn = _mkdb(tmp_env)
    db.ensure_hive_schema(conn)  # second run: no error
    tables = {r[0] for r in conn.execute(
        "SELECT name FROM sqlite_master WHERE type='table'")}
    assert {"entries", "hive_sessions", "hive_jobs", "hive_dirty",
            "hive_lane_cursors"} <= tables
    # dirty trigger fires on entries insert
    conn.execute(
        "INSERT INTO entries (uid, origin, producer, repo_key, kind, ts, title)"
        " VALUES ('s1', 'live', 'test', 'r/k', 'session', 1, 't')")
    conn.commit()
    assert conn.execute("SELECT COUNT(*) FROM hive_dirty").fetchone()[0] == 1


def test_ledger_touch_and_close(tmp_env, monkeypatch):
    from hive import ledger
    _mkdb(tmp_env).close()
    monkeypatch.setattr(ledger, "_repo_info", lambda cwd: ("o/r", "wtx", "main"))
    monkeypatch.setattr(ledger, "_tmux_context", lambda: ("sess1", "%5"))
    payload = {"session_id": "abc123", "cwd": "/tmp/x",
               "transcript_path": "/tmp/x/t.jsonl"}
    ledger.touch_start(payload)
    ledger.heartbeat("abc123")
    ledger.close("abc123", "clean")
    with db.connect(config.DB_PATH) as conn:
        row = conn.execute("SELECT * FROM hive_sessions").fetchone()
        assert row["session_id"] == "abc123" and row["end_reason"] == "clean"
        assert row["repo_key"] == "o/r" and row["tmux_session"] == "sess1"


def test_birth_marker_roundtrip(tmp_env):
    from hive import ledger
    ledger.record_wt_birth(slug="s", repo="r", worktree="w", branch="b",
                           tmux_session="tm1")
    got = ledger.claim_birth("tm1")
    assert got and got["slug"] == "s"
    assert ledger.claim_birth("tm1") is None  # consumed


def test_session_start_compact_injects_snapshot(tmp_env, monkeypatch, capsys):
    from hive import hooks, ledger
    _mkdb(tmp_env).close()
    monkeypatch.setattr(ledger, "_repo_info", lambda cwd: ("", "", ""))
    monkeypatch.setattr(ledger, "_tmux_context", lambda: ("", ""))
    config.ensure_dirs()
    snap = config.SNAPSHOT_DIR / "compact-sid9.md"
    snap.parent.mkdir(parents=True, exist_ok=True)
    snap.write_text("NEXT: finish the lane importer")
    hooks.on_session_start({"session_id": "sid9", "source": "compact",
                            "cwd": "/tmp"})
    out = capsys.readouterr().out
    payload = json.loads(out)
    ctx = payload["hookSpecificOutput"]["additionalContext"]
    assert "finish the lane importer" in ctx and "hive-compact-snapshot" in ctx


def test_session_start_plain_no_output(tmp_env, monkeypatch, capsys):
    from hive import hooks, ledger
    _mkdb(tmp_env).close()
    monkeypatch.setattr(ledger, "_repo_info", lambda cwd: ("", "", ""))
    monkeypatch.setattr(ledger, "_tmux_context", lambda: ("", ""))
    hooks.on_session_start({"session_id": "sid1", "source": "startup",
                            "cwd": "/tmp/none"})
    assert capsys.readouterr().out == ""


def test_repo_key_sha256_fallback(tmp_env, tmp_path):
    from hive import ledger
    repo = tmp_path / "solo"
    repo.mkdir()
    util.run(["git", "init", "-q", str(repo)])
    key, _, _ = ledger._repo_info(str(repo))
    import hashlib
    real = str(repo.resolve())
    expect = f"dir:solo@{hashlib.sha256(real.encode()).hexdigest()[:8]}"
    assert key == expect


def test_titles_rename_throttle_freeze(tmp_env, monkeypatch):
    from hive import titles
    conn = _mkdb(tmp_env)
    conn.execute(
        "INSERT INTO entries (uid, origin, producer, repo_key, kind, ts, title)"
        " VALUES ('sid7', 'live', 't', 'r/k', 'session', 5, 'Fix lane importer')")
    conn.execute(
        "INSERT INTO hive_sessions (session_id, harness, machine, tmux_session,"
        " tmux_pane, started_at, last_activity) "
        "VALUES ('sid7', 'claude-code', 'testbox', 'ws-hive', '%3', 1, 1)")
    conn.commit()
    conn.close()

    calls = []
    monkeypatch.setattr(titles.util, "run",
                        lambda cmd, timeout=3: (calls.append(cmd),
                                                type("R", (), {"returncode": 0})())[1])
    assert titles.tick_phase() == 1
    assert calls[0][:3] == ["tmux", "rename-window", "-t"]
    assert calls[0][3] == "%3" and calls[0][4] == "Fix lane importer"

    # Immediately again: throttled (last_title_at just set).
    assert titles.tick_phase() == 0

    # Expire throttle; same derived title twice -> freeze, no rename.
    with db.connect(config.DB_PATH) as conn:
        conn.execute("UPDATE hive_sessions SET last_title_at=0")
        conn.commit()
    assert titles.tick_phase() == 0
    with db.connect(config.DB_PATH) as conn:
        conn.execute("UPDATE hive_sessions SET last_title_at=0")
        conn.commit()
    assert titles.tick_phase() == 0
    with db.connect(config.DB_PATH) as conn:
        locked = conn.execute(
            "SELECT title_locked FROM hive_sessions").fetchone()[0]
    assert locked == 1
