-- FUTURE WORK: retrieval here is lexical (BM25 + recency/durability/scope), by
-- design and on measured evidence -- see SPEC.md 5.6. The known weakness is
-- paraphrase. If semantic retrieval is ever added it goes in as a SECOND ranked
-- list fused with this one (RRF), never as a replacement, and it must not add a
-- network call at query time or a resident process (SPEC.md 13).

-- ═══════════════════════════════════════════════════════════════════════════
-- jhh3-mem — lib/rank.sql
-- THE ranking statement.  One file, two profiles (SPEC §5.6).  The profiles
-- differ ONLY in bound parameters; there is no second copy of this logic
-- anywhere in the tree, which is what makes `explain-inject` byte-identical to
-- the real hook (INJ-16).
--
-- Two-stage in a single statement:
--   stage 1  candidate generation, hard-capped at 400 rows
--            · prose profile : FTS5 MATCH, ordered by raw bm25
--            · recency profile: entries by ts DESC (the digest; :q = '')
--   stage 2  rescore those ≤400 rows.  O(400) regardless of corpus size.
--
-- BOUND PARAMETERS — every one is bound as TEXT by jm_db_param_read (values
-- travel as cast(x'<hex>' as text)).  Therefore every numeric use below is
-- wrapped in an explicit CAST: with :x bound to the text '1', the expression
-- `:x = 1` is FALSE in SQLite, while `CAST(:x AS INTEGER) = 1` is TRUE.
-- This is not defensive noise; it is a verified correctness requirement.
--
--   :q                FTS5 MATCH expression, or '' to select the recency profile
--   :repo_key         current repo_key ('' only meaningful with :scope_all='1')
--   :worktree         current worktree leaf name, or '' — SIGNAL ONLY, never a filter
--   :scope_all        '0' repo-scoped · '1' all repos (search --all-repos only)
--   :scope_penalty    multiplier applied to rows from a different repo_key (0.45)
--   :half_life        recency half-life in days (120 for search) · '0' ⇒ recency = 1.0
--   :durability_floor rows below this are invisible to this profile (0.9 digest / 0.0 search)
--   :sem_ids          comma-separated ids from semantic search, or '' (see stage 1c)
--   :origin_filter    comma-list, e.g. 'live,noted' · '' ⇒ every origin
--   :kind             single kind to filter to, or '' for all
--   :since_epoch      lower bound on ts (0 = no bound)
--   :now              caller's unix seconds — passed in so a run is reproducible
--   :limit            final row cap
--   :uid_cap          max rows per uid (2 for the digest, huge for search)
--   :dup_cap          max rows per dedup_key (1 normally, huge for `search --dups`)
--   :order_recency    '1' order by ts DESC (digest) · '0' order by score DESC (search)
--   :injectable_only  '1' require injectable = 1 (digest) · '0' ignore the column
--   :fmt              'text' ⇒ one 0x1F-separated line · 'json' ⇒ one JSON object
--
-- OUTPUT is a SINGLE column, one row per result, so the caller never depends on
-- sqlite3's list-mode separator.  Text rows are separated by char(31) (unit
-- separator) and NOT by TAB: bash `read` with IFS=$'\t' silently collapses
-- consecutive tabs (tab is IFS whitespace), which would shift every field after
-- an empty one.  0x1F is not IFS whitespace, and jm_sanitize strips bytes < 0x20
-- from stored content at write time, so it cannot occur inside a field.
--
-- Text field order (12):
--   id · uid · ts · kind · repo_key · worktree · origin · ephemeral ·
--   recurrence · score · title · cont
--
-- The `/*{{RECENCY}}*/ … /*{{/RECENCY}}*/` region is substituted with the
-- stepped decay ladder by jm_rank_sql() when meta.has_math = '0'.  It is a
-- textual substitution rather than a CASE branch because a build without
-- ENABLE_MATH_FUNCTIONS fails to PREPARE a statement mentioning exp(), whether
-- or not that branch is ever taken.  Both markers must stay on one line.
-- ═══════════════════════════════════════════════════════════════════════════

WITH RECURSIVE
-- ── stage 1a: prose candidates (skipped when :q = '') ──────────────────────
-- The MATCH argument is never the empty string: fts5 raises a syntax error on
-- ''.  A sentinel token that cannot occur in the corpus keeps this branch
-- syntactically valid and empty, instead of relying on the planner to
-- short-circuit `:q <> '' AND … MATCH :q`.
fts AS (
  SELECT f.rowid AS id,
         max(-bm25(entries_fts, 8.0, 2.0, 2.0, 3.0, 3.0, 2.5, 1.5, 2.0, 1.0), 0.05) AS bm
    FROM entries_fts f
   WHERE entries_fts MATCH (CASE WHEN :q = '' THEN '"jhh3memzzsentinelzz"' ELSE :q END)
   ORDER BY bm25(entries_fts, 8.0, 2.0, 2.0, 3.0, 3.0, 2.5, 1.5, 2.0, 1.0)
   LIMIT 400
),

-- ── stage 1b: recency candidates (the digest; skipped when :q <> '') ───────
-- The eligibility predicate is repeated here so that LIMIT 400 counts only
-- rows that can actually survive stage 2.
recency AS (
  SELECT e.id AS id, 1.0 AS bm
    FROM entries e
   WHERE :q = ''
     AND e.superseded = 0
     AND (CAST(:injectable_only AS INTEGER) = 0 OR e.injectable = 1)
     AND e.durability >= CAST(:durability_floor AS REAL)
     AND e.ts >= CAST(:since_epoch AS INTEGER)
     AND (CAST(:scope_all AS INTEGER) = 1 OR e.repo_key = :repo_key)
     AND (:origin_filter = '' OR instr(',' || :origin_filter || ',', ',' || e.origin || ',') > 0)
     AND (:kind = '' OR e.kind = :kind)
   ORDER BY e.ts DESC
   LIMIT 400
),

-- ── stage 1c: semantic candidates (empty unless `embed` has run) ──────────
-- :sem_ids is a comma-separated id list produced OUTSIDE this query by
-- lib/embed.sh, which owns the only code that loads sqlite-vec. Passing ids in
-- rather than joining a vec0 table here is deliberate: rank.sql must keep working
-- on a machine that has never fetched the extension, and a virtual table whose
-- module is missing makes the whole database unopenable -- turning an optional
-- feature into a total outage of lexical search.
--
-- Scoring across two lanes whose units do not compare. BM25 here lands around
-- 10-14; a raw cosine distance is ~1. Giving semantic rows a flat bm of 1.0 makes
-- them formally candidates and practically unrankable -- they can never clear a
-- lexical hit, so the lane is dead weight. Full RRF (discard both scores, fuse on
-- rank alone) is the textbook fix, but it also flattens BM25's own magnitudes,
-- which would let recency and durability dominate every lexical query -- a real
-- behaviour change on the path that works today.
--
-- So: express the semantic lane IN the lexical lane's units. Anchor to this
-- query's own best BM25 score and decay by semantic rank (json_each.key is the
-- position in the id list, which lib/embed.sh emits nearest-first). coalesce
-- covers a query with NO lexical hits at all, where semantic rows are the only
-- candidates and relative order is all that matters.
--
-- The 1.5 weight is measured, not taste, and it is deliberately ABOVE 1.0. A high
-- BM25 score means a rare term matched often; it is not evidence the row is on
-- topic. Because this design keeps BM25's raw spread (the thing full RRF throws
-- away), the lexical lane's top end is inflated relative to a rank-decayed
-- semantic lane, so parity needs a weight over 1. What protects exact-literal
-- queries is NOT this cap -- it is the sum in `cand`: a literal match is in both
-- lanes and its combined score runs away from everything else.
--
-- Evidence, against the real 20k-row store: 10 rare-literal queries (busy_timeout,
-- ENG-3077, cloudbuild.yaml, ...) returned an identical top 3 at weight 1.0 and
-- 1.5 in 8 cases, with 2 neutral reorderings between rows on the same topic; 4
-- natural-language queries returned the same correct top hit at every weight. The
-- weight only moves results in the case it exists for -- 'live sync is unreliable',
-- where BM25 ranks a Swift Task-equality bug and an API-client token-cache bug top
-- because both contain the word 'unreliable', and the Yjs live-collaboration and
-- KV-resync rows that actually answer the question are found only by meaning.
-- Split on commas by hand rather than with json_each('[' || :sem_ids || ']').
-- json_each RAISES on malformed JSON, and a raise here fails the entire statement
-- -- so one bad character in a value derived from an HTTP response would take
-- lexical search down with it. This form cannot raise: a non-numeric token CASTs
-- to 0 and matches no row. lib/embed.sh still validates before binding; this is
-- the backstop that keeps the blast radius at "no semantic lane".
sem_split(rest, tok, pos) AS (
  SELECT :sem_ids || ',', '', -1
  UNION ALL
  SELECT substr(rest, instr(rest, ',') + 1),
         substr(rest, 1, instr(rest, ',') - 1),
         pos + 1
    FROM sem_split
   WHERE rest <> '' AND instr(rest, ',') > 0 AND pos < 400
),

semantic AS (
  SELECT CAST(sp.tok AS INTEGER) AS id,
         coalesce((SELECT max(bm) FROM fts), 1.0)
           * 1.5      -- see note above; measured, not taste
           * (60.0 / (60.0 + sp.pos))        AS bm
    FROM sem_split sp
   WHERE sp.pos >= 0 AND sp.tok <> ''
),

-- GROUP BY is load-bearing, not tidiness: a row both lanes find would otherwise
-- be JOINed twice in stage 2 and rendered twice. Summing is also the fusion rule
-- -- agreement between an independent lexical and semantic lane is the strongest
-- evidence the ranker has, and it is the only way a row outscores the best pure
-- lexical hit. With :sem_ids empty this collapses to a no-op over unique rowids,
-- so the lexical-only path is bit-for-bit what it was before.
cand AS (
  SELECT id, sum(bm) AS bm
    FROM (SELECT id, bm FROM fts
          UNION ALL
          SELECT id, bm FROM recency
          UNION ALL
          SELECT id, bm FROM semantic)
   GROUP BY id
),

-- ── stage 2: rescore ──────────────────────────────────────────────────────
scored AS (
  SELECT e.id            AS id,
         e.uid           AS uid,
         e.ts            AS ts,
         e.kind          AS kind,
         e.repo_key      AS repo_key,
         e.worktree      AS worktree,
         e.origin        AS origin,
         e.ephemeral     AS ephemeral,
         e.dedup_key     AS dedup_key,
         e.title         AS title,
         CASE
           WHEN e.next_steps <> '' THEN 'next: ' || e.next_steps
           WHEN e.outcome    <> '' THEN substr(e.outcome, 1,
                  CASE WHEN instr(e.outcome, '. ') > 0
                       THEN instr(e.outcome, '. ')
                       ELSE min(length(e.outcome), 400) END)
           ELSE ''
         END             AS cont,
         c.bm
           * /*{{RECENCY}}*/ (CASE WHEN CAST(:half_life AS REAL) <= 0.0 THEN 1.0 ELSE 0.20 + 0.80 * exp(-0.6931471805599453 * (max(0, CAST(:now AS INTEGER) - e.ts) / 86400.0) / CAST(:half_life AS REAL)) END) /*{{/RECENCY}}*/
           * e.durability
           * (CASE
                WHEN e.repo_key = :repo_key AND :worktree <> '' AND e.worktree = :worktree THEN 1.10
                WHEN e.repo_key = :repo_key THEN 1.00
                ELSE CAST(:scope_penalty AS REAL)
              END)
                       AS score
    FROM cand c
    JOIN entries e ON e.id = c.id
   WHERE e.superseded = 0
     AND (CAST(:injectable_only AS INTEGER) = 0 OR e.injectable = 1)
     AND e.durability >= CAST(:durability_floor AS REAL)
     AND e.ts >= CAST(:since_epoch AS INTEGER)
     AND (CAST(:scope_all AS INTEGER) = 1 OR e.repo_key = :repo_key)
     AND (:origin_filter = '' OR instr(',' || :origin_filter || ',', ',' || e.origin || ',') > 0)
     AND (:kind = '' OR e.kind = :kind)
),

-- ── dedup: a window function, never a GROUP BY, and the recurrence count is
--    rendered rather than hidden (SPEC §5.4).
ranked AS (
  SELECT s.*,
         ROW_NUMBER() OVER (PARTITION BY s.dedup_key ORDER BY s.score DESC, s.ts DESC, s.id DESC) AS rk,
         COUNT(*)     OVER (PARTITION BY s.dedup_key)                                             AS recurrence,
         ROW_NUMBER() OVER (PARTITION BY s.uid       ORDER BY s.ts    DESC, s.score DESC, s.id DESC) AS urk
    FROM scored s
)

SELECT CASE WHEN :fmt = 'json' THEN
         json_object(
           'id',         r.id,
           'uid',        r.uid,
           'ts',         r.ts,
           'date',       strftime('%Y-%m-%d', r.ts, 'unixepoch'),
           'kind',       r.kind,
           'repo_key',   r.repo_key,
           'worktree',   r.worktree,
           'origin',     r.origin,
           'ephemeral',  r.ephemeral,
           'recurrence', r.recurrence,
           'score',      round(r.score, 4),
           'title',      r.title,
           'cont',       r.cont)
       ELSE
         r.id || char(31) || r.uid || char(31) || r.ts || char(31) || r.kind || char(31)
              || r.repo_key || char(31) || r.worktree || char(31) || r.origin || char(31)
              || r.ephemeral || char(31) || r.recurrence || char(31)
              || printf('%.4f', r.score) || char(31)
              || replace(replace(replace(replace(replace(r.title, char(96), ''''), char(31), ' '), char(9), ' '), char(10), ' '), char(13), ' ')
              || char(31)
              || replace(replace(replace(replace(replace(r.cont,  char(96), ''''), char(31), ' '), char(9), ' '), char(10), ' '), char(13), ' ')
       END AS line
  FROM ranked r
 WHERE r.rk  <= CAST(:dup_cap AS INTEGER)
   AND r.urk <= CAST(:uid_cap AS INTEGER)
 ORDER BY (CASE WHEN CAST(:order_recency AS INTEGER) = 1 THEN r.ts ELSE r.score END) DESC,
          r.ts DESC, r.id DESC
 LIMIT CAST(:limit AS INTEGER);
