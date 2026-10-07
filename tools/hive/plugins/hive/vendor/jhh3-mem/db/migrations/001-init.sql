-- ═══════════════════════════════════════════════════════════════════════════
-- jhh3-mem schema_version 1
-- ═══════════════════════════════════════════════════════════════════════════

CREATE TABLE IF NOT EXISTS meta (
  k TEXT PRIMARY KEY,
  v TEXT NOT NULL
);
-- Required keys, written at bootstrap:
--   schema_version   '1'
--   created_at       unix seconds
--   created_by       'jhh3-mem/<version>'
--   has_math         '1' | '0'   -- feature-detected: SELECT exp(1) succeeds?
-- Written by `migrate`:
--   legacy_source    absolute path of the archive
--   legacy_sha256    sha256 of the archive at import time
--   legacy_rows      total rows imported
--   legacy_migrated_at

CREATE TABLE IF NOT EXISTS schema_migrations (
  version    INTEGER PRIMARY KEY,
  applied_at INTEGER NOT NULL,
  note       TEXT NOT NULL DEFAULT ''
);

-- ─── the single retrievable unit ───────────────────────────────────────────
-- One row per: captured session | agent note | migrated session summary |
--              migrated observation | (opt-in) migrated user prompt.
CREATE TABLE IF NOT EXISTS entries (
  id            INTEGER PRIMARY KEY,

  -- identity / idempotency
  uid           TEXT    NOT NULL,           -- see §4.3
  source_bytes  INTEGER NOT NULL DEFAULT 0, -- transcript size at capture; 0 for non-capture rows
  origin        TEXT    NOT NULL,           -- live | noted | legacy-session | legacy-note | legacy-prompt
  producer      TEXT    NOT NULL,           -- 'jhh3-mem/0.1.0'

  -- scope
  repo_key      TEXT    NOT NULL,           -- 'acme/webapp' | 'dir:<base>@<sha1-8>'
  worktree      TEXT    NOT NULL DEFAULT '',-- 'alice-collab-monitor'; SIGNAL ONLY, never a filter
  branch        TEXT    NOT NULL DEFAULT '',
  repo_path     TEXT    NOT NULL DEFAULT '',-- absolute toplevel at capture time

  -- classification
  kind          TEXT    NOT NULL,           -- session|note|decision|bugfix|refactor|change|
                                            -- feature|discovery|security|gotcha|prompt|other
  ts            INTEGER NOT NULL,           -- unix seconds UTC — the sort key
  started_at    INTEGER,

  -- content (all redacted + sanitized before insert)
  title         TEXT    NOT NULL,
  request       TEXT    NOT NULL DEFAULT '',-- typed user prompts, joined
  outcome       TEXT    NOT NULL DEFAULT '',-- closing assistant text (deterministic capture)
  learned       TEXT    NOT NULL DEFAULT '',-- enrich / legacy
  decided       TEXT    NOT NULL DEFAULT '',-- enrich / legacy
  next_steps    TEXT    NOT NULL DEFAULT '',-- enrich / legacy
  facts         TEXT    NOT NULL DEFAULT '',-- legacy JSON array, flattened to newline-joined text
  concepts      TEXT    NOT NULL DEFAULT '',-- space-joined, head-7 vocabulary only
  files_touched TEXT    NOT NULL DEFAULT '',-- newline-joined repo-relative paths (denormalized copy)
  commands      TEXT    NOT NULL DEFAULT '',-- space-joined distinct Bash command heads

  -- capture metadata
  capture_mode  TEXT    NOT NULL DEFAULT 'deterministic',
  end_reason    TEXT    NOT NULL DEFAULT '',-- clear|resume|logout|prompt_input_exit|other|backfill|manual
  turns         INTEGER NOT NULL DEFAULT 0,
  redactions    INTEGER NOT NULL DEFAULT 0,

  -- precomputed retrieval scalars (§5.3) — never computed at query time
  durability    REAL    NOT NULL DEFAULT 1.0,
  dedup_key     TEXT    NOT NULL DEFAULT '',
  n_mod         INTEGER NOT NULL DEFAULT 0,
  n_read        INTEGER NOT NULL DEFAULT 0,
  ephemeral     INTEGER NOT NULL DEFAULT 0,

  -- gating
  injectable    INTEGER NOT NULL DEFAULT 1, -- 0 = search-only, never auto-injected
  superseded    INTEGER NOT NULL DEFAULT 0,

  CHECK (length(title)         <=  200),
  CHECK (length(request)       <= 2000),
  CHECK (length(outcome)       <= 4000),
  CHECK (length(learned)       <= 4000),
  CHECK (length(decided)       <= 4000),
  CHECK (length(next_steps)    <= 2000),
  CHECK (length(facts)         <= 6000),
  CHECK (length(concepts)      <=  400),
  CHECK (length(files_touched) <= 4000),
  CHECK (length(commands)      <=  600),
  CHECK (origin IN ('live','noted','legacy-session','legacy-note','legacy-prompt')),
  CHECK (capture_mode IN ('deterministic','enriched','enrich_failed','noted','legacy')),
  CHECK (injectable IN (0,1)),
  CHECK (superseded IN (0,1)),
  CHECK (ephemeral  IN (0,1))
);

CREATE UNIQUE INDEX IF NOT EXISTS ux_entries_uid_bytes
  ON entries(uid, source_bytes);

CREATE INDEX IF NOT EXISTS ix_entries_repo_ts
  ON entries(repo_key, ts DESC);

CREATE INDEX IF NOT EXISTS ix_entries_ts
  ON entries(ts DESC);

-- partial index: the exact predicate the SessionStart digest uses
CREATE INDEX IF NOT EXISTS ix_entries_inject
  ON entries(repo_key, ts DESC)
  WHERE superseded = 0 AND injectable = 1 AND durability >= 0.9;

CREATE INDEX IF NOT EXISTS ix_entries_dedup
  ON entries(dedup_key);

CREATE INDEX IF NOT EXISTS ix_entries_durability
  ON entries(repo_key, durability DESC, ts DESC);

-- ─── file lane: the cross-worktree bridge ──────────────────────────────────
CREATE TABLE IF NOT EXISTS path (
  id   INTEGER PRIMARY KEY,
  p    TEXT NOT NULL UNIQUE,   -- always repo-relative, normalized, no leading './'
  base TEXT NOT NULL           -- basename, for loose matching
);
CREATE INDEX IF NOT EXISTS ix_path_base ON path(base);

CREATE TABLE IF NOT EXISTS entry_file (
  path_id  INTEGER NOT NULL REFERENCES path(id),
  entry_id INTEGER NOT NULL REFERENCES entries(id) ON DELETE CASCADE,
  mode     INTEGER NOT NULL,   -- 1 = read, 2 = modified
  PRIMARY KEY (path_id, entry_id)
) WITHOUT ROWID;
-- path-leading PK: "which entries touched these 12 paths" is an index seek, not a scan.
CREATE INDEX IF NOT EXISTS ix_entry_file_entry ON entry_file(entry_id);

-- ─── full-text index ───────────────────────────────────────────────────────
CREATE VIRTUAL TABLE IF NOT EXISTS entries_fts USING fts5(
  title, request, outcome, learned, decided, next_steps, facts, concepts, files_touched,
  content='entries',
  content_rowid='id',
  tokenize="unicode61 remove_diacritics 2 tokenchars '_'"
);

CREATE TRIGGER IF NOT EXISTS entries_ai AFTER INSERT ON entries BEGIN
  INSERT INTO entries_fts(rowid, title, request, outcome, learned, decided,
                          next_steps, facts, concepts, files_touched)
  VALUES (new.id, new.title, new.request, new.outcome, new.learned, new.decided,
          new.next_steps, new.facts, new.concepts, new.files_touched);
END;

CREATE TRIGGER IF NOT EXISTS entries_ad AFTER DELETE ON entries BEGIN
  INSERT INTO entries_fts(entries_fts, rowid, title, request, outcome, learned, decided,
                          next_steps, facts, concepts, files_touched)
  VALUES ('delete', old.id, old.title, old.request, old.outcome, old.learned, old.decided,
          old.next_steps, old.facts, old.concepts, old.files_touched);
END;

CREATE TRIGGER IF NOT EXISTS entries_au AFTER UPDATE ON entries BEGIN
  INSERT INTO entries_fts(entries_fts, rowid, title, request, outcome, learned, decided,
                          next_steps, facts, concepts, files_touched)
  VALUES ('delete', old.id, old.title, old.request, old.outcome, old.learned, old.decided,
          old.next_steps, old.facts, old.concepts, old.files_touched);
  INSERT INTO entries_fts(rowid, title, request, outcome, learned, decided,
                          next_steps, facts, concepts, files_touched)
  VALUES (new.id, new.title, new.request, new.outcome, new.learned, new.decided,
          new.next_steps, new.facts, new.concepts, new.files_touched);
END;

-- ─── operational log (bounded by construction) ─────────────────────────────
CREATE TABLE IF NOT EXISTS ops_log (
  id           INTEGER PRIMARY KEY,
  ts           INTEGER NOT NULL,
  event        TEXT NOT NULL,   -- session_start|session_end|backfill|migrate|enrich|note|forget|upgrade
  outcome      TEXT NOT NULL,   -- ok|noop|degraded|error|busy
  session_uid  TEXT NOT NULL DEFAULT '',
  repo_key     TEXT NOT NULL DEFAULT '',
  reason       TEXT NOT NULL DEFAULT '',
  ms           INTEGER NOT NULL DEFAULT 0,
  bytes_in     INTEGER NOT NULL DEFAULT 0,
  bytes_out    INTEGER NOT NULL DEFAULT 0,
  rows_written INTEGER NOT NULL DEFAULT 0,
  redactions   INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS ix_ops_log_ts ON ops_log(ts DESC);

-- ─── forward-migration marker (001-init.sql only) ──────────────────────────
INSERT OR IGNORE INTO schema_migrations(version, applied_at, note)
  VALUES (1, CAST(strftime('%s','now') AS INTEGER), 'initial schema');
