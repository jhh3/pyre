"""Lifecycle module tests: capture gating, compact snapshots, sweep, reap,
enqueue dedupe, janitor gating.

Everything runs against a throwaway SQLite DB and tmp dirs — never the real
~/.local/share/jhh3-mem/memory.db, crontab, or tmux. The vendored jhh3-mem /
ws subprocesses are monkeypatched to recorders.

Run: cd <repo> && PYTHONPATH=plugins/hive/lib python3 -m pytest tests/test_lifecycle.py -q
"""

from __future__ import annotations

import os
import sys
import time
from pathlib import Path

import pytest

LIB = Path(__file__).resolve().parents[1] / "plugins" / "hive" / "lib"
if str(LIB) not in sys.path:
    sys.path.insert(0, str(LIB))

from hive import capture, config, db, enrich, ledger, lifecycle, tick, util, vendored  # noqa: E402


class FakeProc:
    def __init__(self, returncode=0, stdout="", stderr=""):
        self.returncode = returncode
        self.stdout = stdout
        self.stderr = stderr


@pytest.fixture
def env(tmp_path, monkeypatch):
    """Hermetic hive: tmp data/state dirs, tmp DB, no user config."""
    data = tmp_path / "data"
    state = tmp_path / "state"
    monkeypatch.setattr(config, "DATA_DIR", data)
    monkeypatch.setattr(config, "STATE_DIR", state)
    monkeypatch.setattr(config, "DB_PATH", data / "index.db")
    monkeypatch.setattr(config, "QUARANTINE_DIR", data / "raw" / "quarantine")
    monkeypatch.setattr(config, "SNAPSHOT_DIR", data / "raw" / "snapshots")
    monkeypatch.setattr(config, "HOOK_LOG", state / "hooks.log")
    monkeypatch.setattr(config, "TICK_LOG", state / "tick.log")
    monkeypatch.setattr(config, "TICK_LOCK", state / "tick.lock")
    monkeypatch.setattr(config, "STREAMS_VAULT", tmp_path / "streams")
    monkeypatch.setattr(config, "CLAUDE_PROJECTS", tmp_path / "claude-projects")
    monkeypatch.setattr(config, "CODEX_SESSIONS", tmp_path / "codex-sessions")
    monkeypatch.setattr(config, "CONFIG_PATH", tmp_path / "absent.toml")
    monkeypatch.setattr(db, "JHH3_MEM_DB", tmp_path / "absent-mem.db")
    monkeypatch.setenv("HIVE_DB", str(data / "index.db"))
    monkeypatch.setenv("HIVE_MACHINE", "testbox")
    config.load.cache_clear()
    config.ensure_dirs()
    with db.connect() as conn:
        # Minimal vendored-owned entries table (the columns hive reads).
        conn.execute("""CREATE TABLE IF NOT EXISTS entries (
            id INTEGER PRIMARY KEY,
            uid TEXT NOT NULL,
            source_bytes INTEGER NOT NULL DEFAULT 0,
            ts INTEGER NOT NULL DEFAULT 0,
            started_at INTEGER,
            title TEXT NOT NULL DEFAULT '',
            outcome TEXT NOT NULL DEFAULT '',
            learned TEXT NOT NULL DEFAULT '',
            decided TEXT NOT NULL DEFAULT '',
            next_steps TEXT NOT NULL DEFAULT '',
            files_touched TEXT NOT NULL DEFAULT '',
            commands TEXT NOT NULL DEFAULT '',
            capture_mode TEXT NOT NULL DEFAULT 'deterministic',
            n_mod INTEGER NOT NULL DEFAULT 0,
            superseded INTEGER NOT NULL DEFAULT 0)""")
        db.ensure_hive_schema(conn)
        conn.commit()
    yield tmp_path
    config.load.cache_clear()


@pytest.fixture
def fake_mem(monkeypatch):
    calls: list[list[str]] = []

    def mem(args, timeout=60, capture=True):
        calls.append(list(args))
        return FakeProc()

    monkeypatch.setattr(vendored, "mem", mem)
    # The janitor also shells out to `ws sync`; never run the real one here.
    monkeypatch.setattr(vendored, "ws",
                        lambda args, timeout=60, capture=True: FakeProc())
    return calls


@pytest.fixture
def fake_ws(monkeypatch):
    calls: list[list[str]] = []

    def ws(args, timeout=60, capture=True):
        calls.append(list(args))
        return FakeProc()

    monkeypatch.setattr(vendored, "ws", ws)
    return calls


def add_session(sid, harness="claude-code", cwd="", transcript_path="",
                tmux_session="", tmux_pane="", stream_slug="",
                last_activity=None):
    now = int(time.time())
    with db.connect() as conn:
        conn.execute(
            """INSERT INTO hive_sessions
               (session_id, harness, machine, cwd, transcript_path,
                tmux_session, tmux_pane, stream_slug, started_at,
                last_activity)
               VALUES (?,?,?,?,?,?,?,?,?,?)""",
            (sid, harness, "testbox", cwd, transcript_path, tmux_session,
             tmux_pane, stream_slug, now,
             now if last_activity is None else last_activity))
        conn.commit()


def add_entry(uid, **fields):
    cols = {"uid": uid, "source_bytes": 100, "ts": int(time.time()),
            "started_at": int(time.time())}
    cols.update(fields)
    with db.connect() as conn:
        conn.execute(
            f"INSERT INTO entries ({','.join(cols)}) "
            f"VALUES ({','.join('?' * len(cols))})",
            tuple(cols.values()))
        conn.commit()


def seed_state(sid, offset_bytes, last_checkpoint_at, harness="claude-code"):
    with db.connect() as conn:
        conn.execute(
            """INSERT INTO hive_capture_state
               (harness, session_id, offset_bytes, last_checkpoint_at)
               VALUES (?,?,?,?)""",
            (harness, sid, offset_bytes, last_checkpoint_at))
        conn.commit()


def get_session(sid):
    with db.connect() as conn:
        return conn.execute(
            "SELECT * FROM hive_sessions WHERE session_id=?", (sid,)
        ).fetchone()


def get_state(sid):
    with db.connect() as conn:
        return conn.execute(
            "SELECT * FROM hive_capture_state WHERE session_id=?", (sid,)
        ).fetchone()


# ── checkpoint gating ──────────────────────────────────────────────────────

def test_checkpoint_gate_below_both_thresholds(env, fake_mem):
    sid = "sess-gate"
    t = env / "t.jsonl"
    t.write_bytes(b"x" * 5000)
    add_session(sid, cwd=str(env))
    seed_state(sid, offset_bytes=4000, last_checkpoint_at=db.now())
    assert capture.maybe_checkpoint("claude-code", sid, str(t)) is False
    assert fake_mem == []


def test_checkpoint_gate_bytes_passes(env, fake_mem):
    sid = "sess-bytes"
    t = env / "t.jsonl"
    t.write_bytes(b"x" * 13000)
    add_session(sid, cwd=str(env))
    seed_state(sid, offset_bytes=4000, last_checkpoint_at=db.now())
    assert capture.maybe_checkpoint("claude-code", sid, str(t)) is True
    assert fake_mem == [["capture", "--transcript", str(t),
                         "--session", sid, "--cwd", str(env),
                         "--reason", "checkpoint"]]
    state = get_state(sid)
    assert state["offset_bytes"] == 13000
    assert state["checkpoint_seq"] == 1
    assert state["entry_uid"] == sid
    assert get_session(sid)["last_checkpoint_seq"] == 1
    # No growth since -> gated again.
    assert capture.maybe_checkpoint("claude-code", sid, str(t)) is False
    assert len(fake_mem) == 1


def test_checkpoint_gate_time_passes(env, fake_mem):
    sid = "sess-time"
    t = env / "t.jsonl"
    t.write_bytes(b"x" * 5000)   # only 1000 new bytes, but interval elapsed
    add_session(sid, cwd=str(env))
    seed_state(sid, offset_bytes=4000, last_checkpoint_at=db.now() - 700)
    assert capture.maybe_checkpoint("claude-code", sid, str(t)) is True
    assert len(fake_mem) == 1


def test_checkpoint_significance_enqueues(env, monkeypatch):
    sid = "sess-sig"
    t = env / "t.jsonl"
    t.write_bytes(b"x" * 20000)
    add_session(sid, cwd=str(env))
    calls = []

    def mem(args, timeout=60, capture=True):
        calls.append(list(args))
        add_entry(sid, source_bytes=20000, n_mod=5, title="big work")
        return FakeProc()

    monkeypatch.setattr(vendored, "mem", mem)
    assert capture.maybe_checkpoint("claude-code", sid, str(t)) is True
    with db.connect() as conn:
        job = conn.execute(
            "SELECT * FROM hive_jobs WHERE kind='enrich_checkpoint'"
        ).fetchone()
    assert job is not None
    assert job["entry_uid"] == sid
    assert job["state"] == "pending"
    assert job["priority"] == 5


# ── compact_save ───────────────────────────────────────────────────────────

def test_compact_save_snapshot_rendering(env, fake_mem):
    sid = "sess-compact"
    t = env / "t.jsonl"
    t.write_bytes(b"y" * 3333)
    add_session(sid, cwd=str(env))
    files = "\n".join(f"src/f{i}.py" for i in range(20))
    add_entry(sid, title="Fix parser", next_steps="",
              outcome="Fixed the tokenizer. Also cleaned tests.",
              files_touched=files, commands="git pytest")
    snap = env / "snap.md"
    capture.compact_save("claude-code", sid, str(t), trigger="auto",
                         snapshot_to=snap)
    assert ["capture", "--transcript", str(t), "--session", sid,
            "--cwd", str(env), "--reason", "compact"] in fake_mem
    text = snap.read_text()
    assert len(text.encode()) <= 2048
    assert "# Fix parser" in text
    assert "Fixed the tokenizer." in text        # first sentence of outcome
    assert "- src/f14.py" in text                # 15th file kept
    assert "src/f15.py" not in text              # 16th trimmed
    assert "git pytest" in text
    # Best-effort hardlink of the doomed transcript.
    link = config.SNAPSHOT_DIR / f"precompact-{sid}-3333.jsonl"
    assert link.exists()
    assert os.stat(link).st_ino == os.stat(t).st_ino


# ── finalize ───────────────────────────────────────────────────────────────

def test_finalize_returns_uid_only_with_entry(env, fake_mem):
    t = env / "t.jsonl"
    t.write_bytes(b"z" * 500)
    add_session("sess-a", cwd=str(env))
    add_session("sess-b", cwd=str(env))
    add_entry("sess-a", title="did things")
    assert capture.finalize("claude-code", "sess-a", str(t), "exit") == "sess-a"
    assert capture.finalize("claude-code", "sess-b", str(t), "exit") is None
    reasons = [c[c.index("--reason") + 1] for c in fake_mem if "--reason" in c]
    assert reasons == ["exit", "exit"]


# ── sweep ──────────────────────────────────────────────────────────────────

def test_sweep_skip_rules(env, fake_mem):
    p = config.CLAUDE_PROJECTS / "proj" / "abc-123.jsonl"
    p.parent.mkdir(parents=True, exist_ok=True)
    p.write_bytes(b"a" * 400)
    # Fresh mtime -> skipped (still being written).
    assert capture.sweep_transcript(p, "claude-code") is False
    assert fake_mem == []
    # Old enough -> captured with --session (claude id = basename).
    old = time.time() - 600
    os.utime(p, (old, old))
    assert capture.sweep_transcript(p, "claude-code") is True
    assert fake_mem == [["capture", "--transcript", str(p),
                         "--session", "abc-123"]]
    # Size unchanged -> recorded offset short-circuits.
    os.utime(p, (old, old))
    assert capture.sweep_transcript(p, "claude-code") is False
    assert len(fake_mem) == 1
    # Grown -> captured again.
    p.write_bytes(b"a" * 900)
    os.utime(p, (old, old))
    assert capture.sweep_transcript(p, "claude-code") is True
    assert len(fake_mem) == 2


def test_sweep_codex_omits_session_flag(env, fake_mem):
    p = config.CODEX_SESSIONS / "2026" / "rollout-xyz.jsonl"
    p.parent.mkdir(parents=True, exist_ok=True)
    p.write_bytes(b"c" * 300)
    old = time.time() - 600
    os.utime(p, (old, old))
    assert capture.sweep_transcript(p, "codex") is True
    assert fake_mem == [["capture", "--transcript", str(p)]]


# ── reap ───────────────────────────────────────────────────────────────────

def test_reap_decisions(env, fake_mem, monkeypatch):
    def fake_run(cmd, timeout=30, **kw):
        alive = cmd[:2] == ["tmux", "has-session"] and "alive-1" in cmd
        return FakeProc(returncode=0 if alive else 1)

    monkeypatch.setattr(util, "run", fake_run)
    now = time.time()
    stale = now - 3600
    dead_t = env / "dead.jsonl"
    dead_t.write_bytes(b"d" * 100)
    os.utime(dead_t, (stale, stale))
    live_t = env / "live.jsonl"
    live_t.write_bytes(b"l" * 100)   # fresh mtime: still growing

    add_session("s-dead", transcript_path=str(dead_t), tmux_session="gone",
                tmux_pane="%1", last_activity=int(stale))
    add_session("s-growing", transcript_path=str(live_t), tmux_session="gone",
                tmux_pane="%2", last_activity=int(stale))
    add_session("s-idle-alive", transcript_path=str(dead_t),
                tmux_session="alive-1", tmux_pane="%3",
                last_activity=int(stale))
    add_entry("s-dead", title="was working")

    tick._reap(time.monotonic() + 60)

    dead = get_session("s-dead")
    assert dead["ended_at"] is not None
    assert dead["end_reason"] == "reaped"
    reasons = [c[c.index("--reason") + 1] for c in fake_mem if "--reason" in c]
    assert reasons == ["reaped"]

    growing = get_session("s-growing")
    assert growing["ended_at"] is None
    assert growing["last_activity"] >= int(now) - 5   # heartbeat only

    idle = get_session("s-idle-alive")
    assert idle["ended_at"] is None                   # tmux alive: untouched
    assert idle["last_activity"] == int(stale)


def test_reap_boot_marks_all_interrupted(env, fake_mem, fake_ws, monkeypatch):
    t = env / "t.jsonl"
    t.write_bytes(b"b" * 100)
    add_session("s-boot", transcript_path=str(t))   # activity seconds ago
    tick._reap(time.monotonic() + 60, boot=True)
    row = get_session("s-boot")
    assert row["ended_at"] is not None
    assert row["end_reason"] == "interrupted"
    assert ["up"] in fake_ws


# ── enqueue / drain ────────────────────────────────────────────────────────

def test_enqueue_dedupe(env):
    enrich.enqueue("enrich_close", entry_uid="u1", session_uid="s1", priority=1)
    enrich.enqueue("enrich_close", entry_uid="u1", session_uid="s1", priority=1)
    enrich.enqueue("enrich_checkpoint", entry_uid="u1")   # other kind: allowed
    with db.connect() as conn:
        n_close = conn.execute(
            "SELECT count(*) FROM hive_jobs WHERE kind='enrich_close'"
        ).fetchone()[0]
        n_all = conn.execute("SELECT count(*) FROM hive_jobs").fetchone()[0]
        # A completed job no longer blocks the dedupe index.
        conn.execute("UPDATE hive_jobs SET state='done'")
        conn.commit()
    assert (n_close, n_all) == (1, 2)
    enrich.enqueue("enrich_close", entry_uid="u1")
    with db.connect() as conn:
        assert conn.execute("SELECT count(*) FROM hive_jobs").fetchone()[0] == 3


def test_drain_runs_once_and_marks_done(env, fake_mem):
    enrich.enqueue("enrich_close", entry_uid="u1", priority=1)
    enrich.enqueue("enrich_checkpoint", entry_uid="u2")
    assert enrich.drain(budget_s=30) == 2
    assert fake_mem == [["enrich", "--limit", "50"]]
    with db.connect() as conn:
        states = {r["state"] for r in conn.execute(
            "SELECT state FROM hive_jobs")}
    assert states == {"done"}
    # Nothing pending -> no vendored run at all.
    assert enrich.drain(budget_s=30) == 0
    assert len(fake_mem) == 1


# ── janitor ────────────────────────────────────────────────────────────────

def _local(hour, minute, day_offset=0):
    lt = time.localtime()
    return time.mktime((lt.tm_year, lt.tm_mon, lt.tm_mday + day_offset,
                        hour, minute, 0, 0, 0, -1))


def test_janitor_hourly_gate(env, fake_mem):
    noon = _local(12, 30)
    tick._janitor(time.monotonic() + 60, now=noon)
    assert ["catchup"] in fake_mem
    assert ["nightly"] not in fake_mem            # outside 03:00-04:59
    tick._janitor(time.monotonic() + 60, now=noon + 60)
    assert fake_mem.count(["catchup"]) == 1       # stamp gates the second run
    tick._janitor(time.monotonic() + 60, now=noon + 3700)
    assert fake_mem.count(["catchup"]) == 2


def test_janitor_nightly_gate(env, fake_mem):
    night = _local(3, 30, day_offset=1)
    tick._janitor(time.monotonic() + 60, now=night)
    assert fake_mem.count(["nightly"]) == 1
    tick._janitor(time.monotonic() + 60, now=night + 600)
    assert fake_mem.count(["nightly"]) == 1       # <20h since the last one


def test_janitor_requeues_stuck_jobs(env, fake_mem):
    now = int(time.time())
    with db.connect() as conn:
        conn.execute(
            """INSERT INTO hive_jobs
               (kind, entry_uid, state, created_at, updated_at)
               VALUES ('enrich_close','u-stuck','running',?,?)""",
            (now - 7200, now - 7200))
        conn.commit()
    tick._janitor(time.monotonic() + 60, now=float(now))
    with db.connect() as conn:
        state = conn.execute(
            "SELECT state FROM hive_jobs WHERE entry_uid='u-stuck'"
        ).fetchone()[0]
    assert state == "pending"


# ── close_stream ───────────────────────────────────────────────────────────

def test_close_stream_writes_handoff_and_updates(env, fake_mem, fake_ws):
    sid = "sess-stream"
    t = env / "t.jsonl"
    t.write_bytes(b"s" * 800)
    add_session(sid, cwd=str(env), transcript_path=str(t),
                stream_slug="my-feature")
    add_entry(sid, title="Ship my-feature", learned="flock beats cron",
              decided="stdlib only", next_steps="write docs",
              files_touched="a.py\nb.py")
    handoff = lifecycle.close_stream("my-feature", "clean", status="parked")
    assert handoff is not None and handoff.exists()
    assert handoff.parent == config.STREAMS_VAULT / "handoffs" / "my-feature"
    text = handoff.read_text()
    assert len(text.encode()) <= 4096
    assert "# Ship my-feature" in text
    assert "flock beats cron" in text
    assert "- a.py" in text
    row = get_session(sid)
    assert row["ended_at"] is not None
    assert row["end_reason"] == "clean"
    assert ["update", "my-feature", "--status", "parked"] in fake_ws
    reasons = [c[c.index("--reason") + 1] for c in fake_mem if "--reason" in c]
    assert reasons == ["clean"]
