"""Vault sync tests: distilled lanes (sync.py) + raw segments (raw.py).

Real git repos as vaults (a bare "origin" + two clones simulating machines
A and B), real sqlite DBs built from the vendored jhh3-mem schema plus the
hive schema, real sed/zstd subprocesses. No network: the only remote is a
local bare repo. Nothing outside tmp_path is ever touched — config paths,
HIVE_MACHINE, and HIVE_DB are monkeypatched per test.
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

from hive import config, db, raw, sync  # noqa: E402

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
    uid="sess-abc-123", source_bytes=1234, origin="live",
    producer="jhh3-mem/0.4.5", host="codex", repo_key="jhh3/hive",
    worktree="wt-one", branch="main", repo_path="/x/hive", kind="session",
    ts=1756200000, started_at=1756190000, title="Fix the widget",
    request="please fix", outcome="fixed it", learned="widgets are tricky",
    decided="use approach B", next_steps="ship it", facts="fact one",
    concepts="widget fix", files_touched="a.py\nb/c.py", commands="git pytest",
    capture_mode="deterministic", end_reason="clear", turns=7, redactions=0,
    durability=1.62, dedup_key="fixthewidget", n_mod=1, n_read=2,
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


def insert_session(conn: sqlite3.Connection, machine: str, **over) -> None:
    now = int(time.time())
    row = dict(session_id="sess-abc-123", harness="claude-code",
               machine=machine, repo_key="jhh3/hive", worktree="", branch="main",
               stream_slug="", tmux_session="", tmux_pane="",
               transcript_path="", title="t", cwd="/home/x/proj",
               started_at=now - 100, last_activity=now,
               last_checkpoint_seq=0, ended_at=None, end_reason="",
               predecessor_session_id="", raw_synced_through=0, no_ledger=0,
               title_locked=0, title_derivations=0, last_title_at=0)
    row.update(over)
    cols = ",".join(row)
    conn.execute(f"INSERT OR REPLACE INTO hive_sessions({cols}) "
                 f"VALUES({','.join('?' * len(row))})", list(row.values()))
    conn.commit()


class World:
    """Bare origin + machines A and B (clone each, own DB and state dir)."""

    def __init__(self, tmp: Path, mp: pytest.MonkeyPatch):
        self.tmp, self.mp = tmp, mp
        for var, val in (("GIT_AUTHOR_NAME", "hive-test"),
                         ("GIT_AUTHOR_EMAIL", "hive-test@local"),
                         ("GIT_COMMITTER_NAME", "hive-test"),
                         ("GIT_COMMITTER_EMAIL", "hive-test@local"),
                         ("GIT_CONFIG_GLOBAL", str(tmp / "gitconfig")),
                         ("GIT_CONFIG_SYSTEM", "/dev/null")):
            mp.setenv(var, val)
        self.bare = tmp / "origin.git"
        subprocess.run(["git", "init", "-q", "--bare", "-b", "main",
                        str(self.bare)], check=True, timeout=30)
        self.vault_a = tmp / "vault-a"
        self.vault_a.mkdir()
        git(self.vault_a, "init", "-q", "-b", "main")
        (self.vault_a / "meta").mkdir()
        (self.vault_a / "meta" / "epoch").write_text("1\n")
        git(self.vault_a, "add", "-A")
        git(self.vault_a, "commit", "-q", "-m", "init")
        git(self.vault_a, "remote", "add", "origin", str(self.bare))
        git(self.vault_a, "push", "-q", "-u", "origin", "main")
        self.vault_b = tmp / "vault-b"
        subprocess.run(["git", "clone", "-q", str(self.bare),
                        str(self.vault_b)], check=True, timeout=30)
        self.db_a_path = tmp / "a.db"
        self.db_b_path = tmp / "b.db"
        self.conn_a = make_db(self.db_a_path)
        self.conn_b = make_db(self.db_b_path)
        # Neutral defaults; keep the real user config out of the picture.
        mp.setattr(config, "CONFIG_PATH", tmp / "hive.toml")
        mp.setattr(config, "DATA_DIR", tmp / "data")
        mp.setattr(config, "RAW_VAULT", tmp / "vault-raw")
        mp.setattr(config, "CLAUDE_PROJECTS", tmp / "claude-projects")
        mp.setattr(config, "CODEX_SESSIONS", tmp / "codex-sessions")
        config.load.cache_clear()
        self.use("a")

    def use(self, which: str) -> None:
        vault = self.vault_a if which == "a" else self.vault_b
        dbp = self.db_a_path if which == "a" else self.db_b_path
        state = self.tmp / f"state-{which}"
        self.mp.setenv("HIVE_MACHINE", f"mach{which}")
        self.mp.setenv("HIVE_DB", str(dbp))
        self.mp.setattr(config, "STREAMS_VAULT", vault)
        self.mp.setattr(config, "STATE_DIR", state)
        self.mp.setattr(config, "TICK_LOG", state / "tick.log")

    @property
    def conn(self) -> sqlite3.Connection:
        return self.conn_a  # convenience for single-machine tests


@pytest.fixture
def world(tmp_path, monkeypatch):
    w = World(tmp_path, monkeypatch)
    yield w
    w.conn_a.close()
    w.conn_b.close()
    config.load.cache_clear()


# ── distilled lanes ─────────────────────────────────────────────────────────

def test_export_import_roundtrip(world):
    eid = insert_entry(world.conn_a)
    add_file(world.conn_a, eid, "a.py", 2)
    add_file(world.conn_a, eid, "b/c.py", 1)
    now = int(time.time())
    insert_session(world.conn_a, "macha", ended_at=now - 5,
                   end_reason="clean", stream_slug="wt-one",
                   raw_synced_through=42)

    n = sync.export_dirty(world.conn_a)
    assert n == 2  # 1 put + 1 session

    month = time.strftime("%Y-%m", time.gmtime(ENTRY["ts"]))
    lane = world.vault_a / "mem" / "lanes" / "macha" / f"{month}.ndjson"
    assert lane.exists()
    # dirty queue drained; rev bookkeeping advanced
    assert world.conn_a.execute(
        "SELECT count(*) c FROM hive_dirty").fetchone()["c"] == 0
    assert world.conn_a.execute(
        "SELECT rev FROM hive_revs WHERE uid=?",
        (ENTRY["uid"],)).fetchone()["rev"] == 1
    assert (world.vault_a / "meta" / "epoch").read_text().strip() == "1"

    assert sync.push(world.vault_a, 0)

    world.use("b")
    sync.pull(world.vault_b, world.conn_b)
    assert sync.import_lanes(world.conn_b) == 2

    src = world.conn_a.execute("SELECT * FROM entries WHERE uid=?",
                               (ENTRY["uid"],)).fetchone()
    dst = world.conn_b.execute("SELECT * FROM entries WHERE uid=?",
                               (ENTRY["uid"],)).fetchone()
    assert dst is not None
    for col in src.keys():
        if col == "id":
            continue
        assert dst[col] == src[col], f"column {col} did not survive roundtrip"

    files = {(r["p"], r["mode"]) for r in world.conn_b.execute(
        "SELECT p.p AS p, ef.mode AS mode FROM entry_file ef "
        "JOIN path p ON p.id=ef.path_id WHERE ef.entry_id=?",
        (dst["id"],))}
    assert files == {("a.py", 2), ("b/c.py", 1)}

    sess = world.conn_b.execute(
        "SELECT * FROM hive_sessions WHERE machine='macha'").fetchone()
    assert sess is not None
    assert sess["end_reason"] == "clean"
    assert sess["stream_slug"] == "wt-one"
    assert sess["raw_synced_through"] == 42


def test_no_echo_reexport_of_imported_rows(world):
    insert_entry(world.conn_a)
    sync.export_dirty(world.conn_a)
    sync.push(world.vault_a, 0)

    world.use("b")
    sync.pull(world.vault_b, world.conn_b)
    sync.import_lanes(world.conn_b)
    # Triggers fired on import, but the echo scrub must have cleaned up.
    assert world.conn_b.execute(
        "SELECT count(*) c FROM hive_dirty").fetchone()["c"] == 0
    assert sync.export_dirty(world.conn_b) == 0
    lane_dir = world.vault_b / "mem" / "lanes" / "machb"
    assert not lane_dir.exists() or not list(lane_dir.glob("*.ndjson"))


def test_supersede_on_import(world):
    world.use("b")
    old_id = insert_entry(world.conn_b, source_bytes=100, outcome="stale")
    world.use("a")
    insert_entry(world.conn_a, source_bytes=1234)
    sync.export_dirty(world.conn_a)
    sync.push(world.vault_a, 0)

    world.use("b")
    sync.pull(world.vault_b, world.conn_b)
    sync.import_lanes(world.conn_b)
    rows = {r["source_bytes"]: r["superseded"] for r in world.conn_b.execute(
        "SELECT source_bytes, superseded FROM entries WHERE uid=?",
        (ENTRY["uid"],))}
    assert rows == {100: 1, 1234: 0}
    assert old_id  # (still present, just superseded)


def test_cursor_advance_and_incremental_import(world):
    insert_entry(world.conn_a)
    sync.export_dirty(world.conn_a)
    sync.push(world.vault_a, 0)

    world.use("b")
    sync.pull(world.vault_b, world.conn_b)
    assert sync.import_lanes(world.conn_b) >= 1
    assert sync.import_lanes(world.conn_b) == 0  # cursor is at EOF

    world.use("a")
    insert_entry(world.conn_a, uid="sess-def-456", source_bytes=999,
                 title="Second entry")
    sync.export_dirty(world.conn_a)
    sync.push(world.vault_a, 0)

    world.use("b")
    sync.pull(world.vault_b, world.conn_b)
    assert sync.import_lanes(world.conn_b) == 1  # only the new record
    assert world.conn_b.execute(
        "SELECT count(*) c FROM entries").fetchone()["c"] == 2


def test_tombstone(world):
    insert_entry(world.conn_a)
    sync.export_dirty(world.conn_a)
    sync.push(world.vault_a, 0)
    world.use("b")
    sync.pull(world.vault_b, world.conn_b)
    sync.import_lanes(world.conn_b)
    assert world.conn_b.execute(
        "SELECT count(*) c FROM entries").fetchone()["c"] == 1

    world.use("a")
    sync.write_tombstone(ENTRY["uid"])
    sync.push(world.vault_a, 0)

    world.use("b")
    sync.pull(world.vault_b, world.conn_b)
    assert sync.import_lanes(world.conn_b) == 1
    assert world.conn_b.execute(
        "SELECT count(*) c FROM entries").fetchone()["c"] == 0
    assert world.conn_b.execute(
        "SELECT count(*) c FROM entry_file").fetchone()["c"] == 0


def test_redaction_gate_masks_secrets_and_fails_closed(world, monkeypatch):
    insert_entry(world.conn_a,
                 outcome="token was ghp_" + "a1B2c3D4e5F6g7H8i9J0" * 2)
    sync.export_dirty(world.conn_a)
    month = time.strftime("%Y-%m", time.gmtime(ENTRY["ts"]))
    lane = world.vault_a / "mem" / "lanes" / "macha" / f"{month}.ndjson"
    text = lane.read_text()
    assert "REDACTED" in text
    assert "a1B2c3D4e5F6g7H8i9J0" not in text
    rec = json.loads(text.splitlines()[0])
    assert rec["op"] == "put"  # still valid JSON after sed

    # Fail closed: sed unavailable -> export aborts, dirty rows survive.
    insert_entry(world.conn_a, uid="sess-xyz-999", source_bytes=50)
    monkeypatch.setattr(sync, "REDACT_SED",
                        world.tmp / "missing-redact.sed")
    with pytest.raises(sync.SyncError):
        sync.export_dirty(world.conn_a)
    assert world.conn_a.execute(
        "SELECT count(*) c FROM hive_dirty").fetchone()["c"] == 1


def test_epoch_reset_zeroes_cursors_and_reimports(world):
    insert_entry(world.conn_a)
    sync.export_dirty(world.conn_a)
    sync.push(world.vault_a, 0)
    world.use("b")
    sync.pull(world.vault_b, world.conn_b)
    sync.import_lanes(world.conn_b)
    assert world.conn_b.execute(
        "SELECT count(*) c FROM hive_lane_cursors WHERE byte_off > 0"
    ).fetchone()["c"] == 1

    # Machine A bumps the epoch (as squash would) and publishes it.
    world.use("a")
    (world.vault_a / "meta" / "epoch").write_text("2\n")
    git(world.vault_a, "add", "-A", "--", "meta")
    git(world.vault_a, "commit", "-q", "-m", "epoch bump")
    git(world.vault_a, "push", "-q", "origin", "main")

    world.use("b")
    sync.pull(world.vault_b, world.conn_b)
    assert (world.vault_b / "meta" / "epoch").read_text().strip() == "2"
    cursors = world.conn_b.execute(
        "SELECT byte_off FROM hive_lane_cursors").fetchall()
    assert cursors and all(r["byte_off"] == 0 for r in cursors)

    # Full reimport is idempotent: no duplicate rows appear.
    sync.import_lanes(world.conn_b)
    assert world.conn_b.execute(
        "SELECT count(*) c FROM entries WHERE uid=?",
        (ENTRY["uid"],)).fetchone()["c"] == 1


# ── raw lane ────────────────────────────────────────────────────────────────

def _jsonl_bytes(n: int, tag: str) -> bytes:
    line = json.dumps({"type": "assistant", "tag": tag,
                       "pad": "x" * 80}) + "\n"
    out = (line * (n // len(line) + 1)).encode()[:n]
    return out


def test_raw_segments_roundtrip_and_refusal(world):
    transcript = world.tmp / "transcript.jsonl"
    part1 = _jsonl_bytes(8000, "one")
    transcript.write_bytes(part1)
    insert_session(world.conn_a, "macha", session_id="sess-raw-1",
                   transcript_path=str(transcript), cwd="/home/x/proj",
                   ended_at=None)

    raw.tick_phase()
    seg_dir = (config.RAW_VAULT / "raw" / "macha" / "claude-code"
               / "sess-raw-1")
    assert (seg_dir / "000000.jsonl.zst").exists()
    row = world.conn_a.execute(
        "SELECT raw_synced_through FROM hive_sessions "
        "WHERE session_id='sess-raw-1'").fetchone()
    assert row["raw_synced_through"] == 8000

    # Grow the transcript -> second segment with only the new bytes.
    part2 = _jsonl_bytes(5000, "two")
    with open(transcript, "ab") as f:
        f.write(part2)
    raw.tick_phase()
    assert (seg_dir / "000001.jsonl.zst").exists()

    # Below the 4096-byte threshold -> no third segment.
    with open(transcript, "ab") as f:
        f.write(b'{"pad": "tiny"}\n')
    raw.tick_phase()
    assert not (seg_dir / "000002.jsonl.zst").exists()

    manifest = json.loads((seg_dir / "manifest.json").read_text())
    assert manifest["cwd"] == "/home/x/proj"
    assert [(s["start"], s["end"]) for s in manifest["segments"]] == \
        [(0, 8000), (8000, 13000)]

    # Byte-identical reassembly into Claude's project-dir layout.
    dest = raw.import_session("sess-raw-1")
    assert dest == (config.CLAUDE_PROJECTS / "-home-x-proj"
                    / "sess-raw-1.jsonl")
    assert dest.read_bytes() == part1 + part2

    # Refusal to overwrite an existing transcript.
    with pytest.raises(raw.RawError, match="refusing to overwrite"):
        raw.import_session("sess-raw-1")


def test_raw_vault_commits_without_remote(world):
    transcript = world.tmp / "t2.jsonl"
    transcript.write_bytes(_jsonl_bytes(6000, "solo"))
    insert_session(world.conn_a, "macha", session_id="sess-raw-2",
                   transcript_path=str(transcript))
    raw.tick_phase()
    # Committed locally; no remote configured -> logged once, no crash.
    log = git(config.RAW_VAULT, "log", "--oneline")
    assert "hive raw" in log
    assert (config.STATE_DIR / "raw.no-remote").exists()


def test_iter_transcripts_local_and_foreign(world):
    # Local claude transcript.
    proj = config.CLAUDE_PROJECTS / "-home-x-proj"
    proj.mkdir(parents=True)
    (proj / "local-sess.jsonl").write_text('{"msg": "needle-local"}\n')

    # Foreign machine's segments in vault-raw.
    transcript = world.tmp / "t3.jsonl"
    transcript.write_bytes(_jsonl_bytes(6000, "needle-foreign"))
    insert_session(world.conn_a, "macha", session_id="sess-raw-3",
                   transcript_path=str(transcript))
    raw.tick_phase()
    world.mp.setenv("HIVE_MACHINE", "other-machine")  # our segments = foreign

    pairs = dict(raw.iter_transcripts(all_machines=True))
    assert any("needle-local" in t for t in pairs.values())
    assert "macha:claude-code:sess-raw-3" in pairs
    assert "needle-foreign" in pairs["macha:claude-code:sess-raw-3"]

    local_only = dict(raw.iter_transcripts(all_machines=False))
    assert "macha:claude-code:sess-raw-3" not in local_only
