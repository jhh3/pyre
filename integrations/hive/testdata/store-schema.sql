-- Structural subset of the hive-mem schema_version 2 store plus the hive_*
-- tables, as observed in jhh3/hive a8d95a7 (the vendored store's db/schema.sql and
-- lib/hive/db.py). Used only to build synthetic fake-Hive stores for tests.
CREATE TABLE meta (k TEXT PRIMARY KEY, v TEXT NOT NULL);
CREATE TABLE entries (
  id INTEGER PRIMARY KEY,
  uid TEXT NOT NULL,
  source_bytes INTEGER NOT NULL DEFAULT 0,
  origin TEXT NOT NULL,
  producer TEXT NOT NULL DEFAULT 'fake',
  host TEXT NOT NULL DEFAULT 'claude-code',
  repo_key TEXT NOT NULL,
  worktree TEXT NOT NULL DEFAULT '',
  branch TEXT NOT NULL DEFAULT '',
  repo_path TEXT NOT NULL DEFAULT '',
  kind TEXT NOT NULL,
  ts INTEGER NOT NULL,
  started_at INTEGER,
  title TEXT NOT NULL,
  request TEXT NOT NULL DEFAULT '',
  outcome TEXT NOT NULL DEFAULT '',
  learned TEXT NOT NULL DEFAULT '',
  decided TEXT NOT NULL DEFAULT '',
  next_steps TEXT NOT NULL DEFAULT '',
  facts TEXT NOT NULL DEFAULT '',
  concepts TEXT NOT NULL DEFAULT '',
  files_touched TEXT NOT NULL DEFAULT '',
  commands TEXT NOT NULL DEFAULT '',
  capture_mode TEXT NOT NULL DEFAULT 'deterministic',
  durability REAL NOT NULL DEFAULT 1.0,
  dedup_key TEXT NOT NULL DEFAULT '',
  injectable INTEGER NOT NULL DEFAULT 1,
  superseded INTEGER NOT NULL DEFAULT 0,
  CHECK (length(title) <= 200),
  CHECK (length(outcome) <= 4000)
);
CREATE UNIQUE INDEX ux_entries_uid_bytes ON entries(uid, source_bytes);
CREATE VIRTUAL TABLE entries_fts USING fts5(
  title, request, outcome, learned, decided, next_steps, facts, concepts, files_touched,
  content='entries', content_rowid='id',
  tokenize="unicode61 remove_diacritics 2 tokenchars '_'"
);
CREATE TRIGGER entries_ai AFTER INSERT ON entries BEGIN
  INSERT INTO entries_fts(rowid, title, request, outcome, learned, decided, next_steps, facts, concepts, files_touched)
  VALUES (new.id, new.title, new.request, new.outcome, new.learned, new.decided, new.next_steps, new.facts, new.concepts, new.files_touched);
END;
CREATE TABLE hive_sessions (
  session_id TEXT NOT NULL, harness TEXT NOT NULL, machine TEXT NOT NULL,
  repo_key TEXT NOT NULL DEFAULT '', worktree TEXT NOT NULL DEFAULT '', branch TEXT NOT NULL DEFAULT '',
  stream_slug TEXT NOT NULL DEFAULT '', tmux_session TEXT NOT NULL DEFAULT '', tmux_pane TEXT NOT NULL DEFAULT '',
  transcript_path TEXT NOT NULL DEFAULT '', title TEXT NOT NULL DEFAULT '', cwd TEXT NOT NULL DEFAULT '',
  started_at INTEGER NOT NULL, last_activity INTEGER NOT NULL, ended_at INTEGER,
  PRIMARY KEY (harness, session_id, machine)
);
CREATE TABLE hive_jobs (
  id INTEGER PRIMARY KEY, kind TEXT NOT NULL, entry_uid TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL DEFAULT 'pending', created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
);
CREATE TABLE hive_lane_cursors (lane_path TEXT PRIMARY KEY, byte_off INTEGER NOT NULL DEFAULT 0, updated_at INTEGER NOT NULL);
