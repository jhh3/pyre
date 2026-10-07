"""hive database layer.

Strategy (docs/PLAN.md §8, staged): until cutover, hive shares the existing
jhh3-mem database (~/.local/share/jhh3-mem/memory.db) so the vendored capture
and enrichment paths keep working untouched; hive adds only `hive_*` tables
and two triggers. After cutover the same schema lives at
~/.local/share/hive/index.db and lanes are canonical.

The `entries` schema itself is owned by the vendored jhh3-mem code — hive
never alters its columns, only reads/writes rows through the same contracts
(uid + source_bytes idempotency, redaction before insert).
"""

from __future__ import annotations

import sqlite3
import time
from pathlib import Path

from . import config

JHH3_MEM_DB = Path.home() / ".local/share/jhh3-mem/memory.db"

HIVE_SCHEMA = """
-- ── identity ledger ────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS hive_sessions (
  session_id     TEXT NOT NULL,
  harness        TEXT NOT NULL,             -- claude-code | codex
  machine        TEXT NOT NULL,
  repo_key       TEXT NOT NULL DEFAULT '',
  worktree       TEXT NOT NULL DEFAULT '',
  branch         TEXT NOT NULL DEFAULT '',
  stream_slug    TEXT NOT NULL DEFAULT '',
  tmux_session   TEXT NOT NULL DEFAULT '',
  tmux_pane      TEXT NOT NULL DEFAULT '',
  transcript_path TEXT NOT NULL DEFAULT '',
  title          TEXT NOT NULL DEFAULT '',
  cwd            TEXT NOT NULL DEFAULT '',
  started_at     INTEGER NOT NULL,
  last_activity  INTEGER NOT NULL,
  last_checkpoint_seq INTEGER NOT NULL DEFAULT 0,
  ended_at       INTEGER,
  end_reason     TEXT NOT NULL DEFAULT '',  -- clean|compacted|crashed|reaped|interrupted
  predecessor_session_id TEXT NOT NULL DEFAULT '',
  raw_synced_through INTEGER NOT NULL DEFAULT 0,   -- transcript byte offset
  no_ledger      INTEGER NOT NULL DEFAULT 0,       -- non-interactive tier marker
  title_locked   INTEGER NOT NULL DEFAULT 0,
  title_derivations INTEGER NOT NULL DEFAULT 0,
  last_title_at  INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (harness, session_id, machine)
);
CREATE INDEX IF NOT EXISTS ix_hive_sessions_live
  ON hive_sessions(last_activity DESC) WHERE ended_at IS NULL;
CREATE INDEX IF NOT EXISTS ix_hive_sessions_slug ON hive_sessions(stream_slug);

-- ── per-session capture state (incremental checkpointing) ─────────────────
CREATE TABLE IF NOT EXISTS hive_capture_state (
  harness        TEXT NOT NULL,
  session_id     TEXT NOT NULL,
  transcript_path TEXT NOT NULL DEFAULT '',
  offset_bytes   INTEGER NOT NULL DEFAULT 0,  -- high-water mark consumed
  last_checkpoint_at INTEGER NOT NULL DEFAULT 0,
  entry_uid      TEXT NOT NULL DEFAULT '',    -- the single live entry we upsert
  checkpoint_seq INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (harness, session_id)
);

-- ── machine-local enrichment queue ─────────────────────────────────────────
CREATE TABLE IF NOT EXISTS hive_jobs (
  id          INTEGER PRIMARY KEY,
  kind        TEXT NOT NULL,       -- enrich_close|enrich_compact|enrich_checkpoint|title
  session_uid TEXT NOT NULL DEFAULT '',
  entry_uid   TEXT NOT NULL DEFAULT '',
  payload     TEXT NOT NULL DEFAULT '{}',
  priority    INTEGER NOT NULL DEFAULT 5,   -- lower = sooner
  state       TEXT NOT NULL DEFAULT 'pending', -- pending|running|done|failed
  attempts    INTEGER NOT NULL DEFAULT 0,
  created_at  INTEGER NOT NULL,
  updated_at  INTEGER NOT NULL,
  not_before  INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS ix_hive_jobs_pending
  ON hive_jobs(priority, created_at) WHERE state = 'pending';
-- dedupe: one pending job per (kind, entry)
CREATE UNIQUE INDEX IF NOT EXISTS ux_hive_jobs_dedupe
  ON hive_jobs(kind, entry_uid) WHERE state IN ('pending','running');

-- ── lane export queue (fed by triggers on entries) ────────────────────────
CREATE TABLE IF NOT EXISTS hive_dirty (
  entry_id  INTEGER PRIMARY KEY,
  marked_at INTEGER NOT NULL
);

-- ── lane import cursors (per foreign lane file) ────────────────────────────
CREATE TABLE IF NOT EXISTS hive_lane_cursors (
  lane_path  TEXT PRIMARY KEY,     -- vault-relative, e.g. mem/lanes/desk/2026-08.ndjson
  byte_off   INTEGER NOT NULL DEFAULT 0,
  updated_at INTEGER NOT NULL
);

-- ── entry revision bookkeeping for lanes ───────────────────────────────────
CREATE TABLE IF NOT EXISTS hive_revs (
  uid  TEXT PRIMARY KEY,
  rev  INTEGER NOT NULL DEFAULT 0
);

-- ── commit xref (Claude-Session trailers) ──────────────────────────────────
CREATE TABLE IF NOT EXISTS hive_commit_xref (
  sha         TEXT NOT NULL,
  repo_key    TEXT NOT NULL,
  pr          TEXT NOT NULL DEFAULT '',
  session_id  TEXT NOT NULL DEFAULT '',
  stream_slug TEXT NOT NULL DEFAULT '',
  subject     TEXT NOT NULL DEFAULT '',
  committed_at INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (repo_key, sha)
);
CREATE INDEX IF NOT EXISTS ix_hive_xref_session ON hive_commit_xref(session_id);

-- ── xref scan state ────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS hive_xref_state (
  repo_path TEXT PRIMARY KEY,
  last_sha  TEXT NOT NULL DEFAULT '',
  updated_at INTEGER NOT NULL
);

-- ── injection feedback ─────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS hive_feedback (
  uid       TEXT NOT NULL,
  kind      TEXT NOT NULL,        -- wrong|pin
  ts        INTEGER NOT NULL,
  note      TEXT NOT NULL DEFAULT ''
);
"""

TRIGGERS = """
CREATE TRIGGER IF NOT EXISTS hive_entries_dirty_ai AFTER INSERT ON entries BEGIN
  INSERT OR REPLACE INTO hive_dirty(entry_id, marked_at)
  VALUES (new.id, strftime('%s','now'));
END;
CREATE TRIGGER IF NOT EXISTS hive_entries_dirty_au AFTER UPDATE ON entries BEGIN
  INSERT OR REPLACE INTO hive_dirty(entry_id, marked_at)
  VALUES (new.id, strftime('%s','now'));
END;
"""


def path() -> Path:
    """Active DB: shared jhh3-mem DB until cutover, then hive's own index.db.

    Cutover is signaled by the presence of index.db (created by `hive migrate`
    at cutover); before that we piggyback on memory.db so vendored code and
    hive observe one consistent store.
    """
    import os
    override = os.environ.get("HIVE_DB")
    if override:
        return Path(override)
    if config.DB_PATH.exists():
        return config.DB_PATH
    if JHH3_MEM_DB.exists():
        return JHH3_MEM_DB
    return config.DB_PATH


def connect(db_path: Path | None = None, timeout: float = 5.0) -> sqlite3.Connection:
    p = db_path or path()
    p.parent.mkdir(parents=True, exist_ok=True)
    conn = sqlite3.connect(p, timeout=timeout)
    conn.row_factory = sqlite3.Row
    conn.execute("PRAGMA journal_mode=WAL")
    conn.execute("PRAGMA synchronous=NORMAL")
    conn.execute("PRAGMA busy_timeout=4000")
    return conn


def ensure_hive_schema(conn: sqlite3.Connection) -> None:
    """Idempotent; additive only. Never touches jhh3-mem-owned objects."""
    conn.executescript(HIVE_SCHEMA)
    # Dirty triggers only make sense where an entries table exists.
    has_entries = conn.execute(
        "SELECT 1 FROM sqlite_master WHERE type='table' AND name='entries'"
    ).fetchone()
    if has_entries:
        conn.executescript(TRIGGERS)
    conn.commit()


def now() -> int:
    return int(time.time())
