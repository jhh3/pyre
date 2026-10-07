#!/usr/bin/env bash
# jhh3-mem — lib/query.sh
# FTS5 query construction: escaping, prefix expansion, and the loader for
# lib/rank.sql.  SPEC §5.5, §5.6, §6.4.
#
# INVARIANTS ENFORCED HERE
#   - No agent- or user-supplied byte is ever concatenated into SQL.  Everything
#     leaves this file as a bound parameter value handed to jm_db_param_read.
#   - The assembled MATCH expression is capped at 200 characters and is always
#     syntactically complete: the cap drops whole terms, never half of one.
#   - Every term is emitted inside an FTS5 double-quoted string, so a term that
#     happens to be an FTS5 keyword (AND / OR / NOT / NEAR) or punctuation
#     cannot change the shape of the expression.
#
# shellcheck shell=bash

if [ -n "${JM_QUERY_SOURCED:-}" ]; then
  # shellcheck disable=SC2317  # reached when executed rather than sourced
  return 0 2>/dev/null || exit 0
fi
JM_QUERY_SOURCED=1

if [ -z "${JM_DB_SOURCED:-}" ]; then
  _jm_q_src="${BASH_SOURCE[0]}"
  case "$_jm_q_src" in */*) _jm_q_dir="${_jm_q_src%/*}" ;; *) _jm_q_dir="." ;; esac
  # shellcheck source=./db.sh
  . "$_jm_q_dir/db.sh"
  unset _jm_q_src _jm_q_dir
fi

JM_RANK_SQL="${JM_RANK_SQL:-$JM_PLUGIN_ROOT/lib/rank.sql}"
readonly JM_RANK_SQL

# Longest MATCH expression we will ever hand to sqlite3 (SPEC §5.5 step 6).
JM_QUERY_MAX_CHARS=200
# Beyond this many distinct terms the marginal recall is nil and the 200-char
# cap would truncate anyway.
JM_QUERY_MAX_TERMS=16
readonly JM_QUERY_MAX_CHARS JM_QUERY_MAX_TERMS

# Cached feature detection (meta.has_math), populated on first use.
JM_HAS_MATH="${JM_HAS_MATH:-}"

# ── query building ──────────────────────────────────────────────────────────

# jm_query_is_path RAW_QUERY
# rc 0 the query looks like a path and must be routed to the file lane
# rc 1 otherwise.
# SPEC §5.5 step 5: "contains a `/` and a `.`".  A string with whitespace is
# prose that happens to mention a file, not a path argument.
jm_query_is_path() {
  local q="${1:-}"
  [ -n "$q" ] || return 1
  case "$q" in *[[:space:]]*) return 1 ;; esac
  case "$q" in */*) : ;; *) return 1 ;; esac
  case "$q" in *.*) return 0 ;; esac
  return 1
}

# jm_query_build RAW_QUERY
# stdout: an FTS5 MATCH expression, ≤ JM_QUERY_MAX_CHARS characters
# rc 0 built · 1 the query reduced to nothing after escaping · 2 misuse
#
# Per whitespace-separated term (SPEC §5.5):
#   1. FTS5 specials (" * ( ) : ^ -) and every byte < 0x20 become SPACES.
#      Deleting them instead would join `well-known` into the token `wellknown`,
#      which the unicode61 tokenizer never produces, so the term could not match
#      anything.  Turning them into separators is what "strip" has to mean for
#      the result to be searchable.  `_` is deliberately NOT a special: it is a
#      tokenchar in this schema.
#   2. term contains `_`, a digit, or mixed case  -> verbatim only (identifier)
#   3. else purely alphabetic and ≥ 5 chars       -> ("term" OR "term"*)
#   4. else                                        -> verbatim
#   6. joined with ` OR `, capped at 200 chars on a whole-term boundary.
jm_query_build() {
  local raw="${1-}"
  [ $# -ge 1 ] || return 2
  [ -n "$raw" ] || return 1

  local cleaned
  # NOTE: control bytes are mapped explicitly, never with `tr -c '[:print:]'` —
  # under LC_ALL=C that class is 0x20–0x7E, so the complement would also flatten
  # every UTF-8 continuation byte and mangle a non-ASCII query.
  cleaned="$(printf '%s' "$raw" \
    | LC_ALL=C tr '"*():^-' '       ' \
    | LC_ALL=C tr '\001-\037\177' ' ')" || return 1

  local out='' seen=' ' nterms=0
  local t piece lower upper cand
  # Word splitting on IFS is exactly the tokenisation we want here.
  # shellcheck disable=SC2086
  set -- $cleaned
  for t in "$@"; do
    [ -n "$t" ] || continue
    [ "$nterms" -lt "$JM_QUERY_MAX_TERMS" ] || break

    # De-duplicate case-insensitively: `Foo foo` must not become `"Foo" OR "foo"`.
    lower="$(printf '%s' "$t" | LC_ALL=C tr '[:upper:]' '[:lower:]')"
    case "$seen" in *" $lower "*) continue ;; esac

    upper="$(printf '%s' "$t" | LC_ALL=C tr '[:lower:]' '[:upper:]')"
    if [ "${#t}" -ge 5 ] \
      && case "$t" in *[!A-Za-z]*) false ;; *) true ;; esac \
      && { [ "$t" = "$lower" ] || [ "$t" = "$upper" ]; }; then
      # purely alphabetic, single case, long enough to be worth a prefix scan
      piece="(\"$t\" OR \"$t\"*)"
    else
      piece="\"$t\""
    fi

    if [ -z "$out" ]; then cand="$piece"; else cand="$out OR $piece"; fi
    [ "${#cand}" -le "$JM_QUERY_MAX_CHARS" ] || break
    out="$cand"
    seen="$seen$lower "
    nterms=$((nterms + 1))
  done

  [ -n "$out" ] || return 1
  printf '%s\n' "$out"
  return 0
}

# ── rank.sql loader ─────────────────────────────────────────────────────────

# jm_has_math — '1' when sqlite3 on this host has exp(), else '0'.
# Reads the value feature-detected at bootstrap.  Any failure answers '0',
# which selects the stepped ladder — always correct, merely coarser.
jm_has_math() {
  if [ -z "$JM_HAS_MATH" ]; then
    local v=''
    v="$(jm_db_read "SELECT v FROM meta WHERE k='has_math';" 2>/dev/null)" || v=''
    case "$v" in 1) JM_HAS_MATH=1 ;; *) JM_HAS_MATH=0 ;; esac
  fi
  printf '%s\n' "$JM_HAS_MATH"
}

# The stepped decay ladder used when exp() is unavailable (SPEC §5.2).
# One line: jm_rank_sql substitutes it for the marked region with a single sed.
_jm_rank_stepped_expr() {
  printf '%s' \
    "(CASE WHEN CAST(:half_life AS REAL) <= 0.0 THEN 1.0" \
    " WHEN (max(0, CAST(:now AS INTEGER) - e.ts) / 86400.0) <   3 THEN 1.00" \
    " WHEN (max(0, CAST(:now AS INTEGER) - e.ts) / 86400.0) <   7 THEN 0.85" \
    " WHEN (max(0, CAST(:now AS INTEGER) - e.ts) / 86400.0) <  21 THEN 0.65" \
    " WHEN (max(0, CAST(:now AS INTEGER) - e.ts) / 86400.0) <  60 THEN 0.45" \
    " WHEN (max(0, CAST(:now AS INTEGER) - e.ts) / 86400.0) < 180 THEN 0.30" \
    " ELSE 0.20 END)"
}

# jm_rank_sql — the ranking statement, with the recency expression resolved for
# this host.  rc 0 · 1 rank.sql missing.
jm_rank_sql() {
  [ -r "$JM_RANK_SQL" ] || return 1
  if [ "$(jm_has_math)" = '1' ]; then
    cat -- "$JM_RANK_SQL"
    return 0
  fi
  local repl
  repl="$(_jm_rank_stepped_expr)"
  LC_ALL=C sed -e "s|/\*{{RECENCY}}\*/.*/\*{{/RECENCY}}\*/|$repl|" -- "$JM_RANK_SQL"
}

# jm_rank_run NAME VALUE [NAME VALUE ...]
# Runs lib/rank.sql with the supplied bindings.  Every parameter the statement
# references must be supplied; an unbound parameter is NULL and would silently
# empty the result.  rc 0/1/2/3 as jm_db_param_read.
jm_rank_run() {
  local sql
  sql="$(jm_rank_sql)" || return 1
  jm_db_param_read "$sql" "$@"
}

# jm_query_defaults — the full parameter set with safe defaults, as a flat
# NAME VALUE list on stdout, one per line.  Callers override by appending their
# own NAME VALUE pairs after these (later bindings win: .parameter set is
# last-write-wins, verified).
jm_query_defaults() {
  printf '%s\n' \
    q '' \
    sem_ids '' \
    repo_key '' \
    worktree '' \
    scope_all 0 \
    scope_penalty 0.45 \
    half_life 120 \
    durability_floor 0.0 \
    origin_filter '' \
    kind '' \
    since_epoch 0 \
    now "$(jm_now_epoch)" \
    limit 20 \
    uid_cap 1000000 \
    dup_cap 1 \
    order_recency 0 \
    injectable_only 0 \
    fmt text
}

# ── the file lane ───────────────────────────────────────────────────────────
# HARD-SCOPED TO :repo_key WITH NO OVERRIDE, EVER (SPEC §13.3).  `src/index.ts`
# exists in many of the 19 client orgs' repos; a cross-repo file anchor would
# surface one client's architecture while working in another's.  There is no
# :scope_all in this statement and no code path that adds one.
#
# Recency is time-invariant here (SPEC §5.2: "recency = 1.0 for the file-anchor
# lane"); ordering is durability × overlap_count.
jm_file_lane_sql() {
  cat <<'SQL'
WITH RECURSIVE
split(p, rest) AS (
  SELECT '', :paths || char(10)
  UNION ALL
  SELECT substr(rest, 1, instr(rest, char(10)) - 1),
         substr(rest, instr(rest, char(10)) + 1)
    FROM split
   WHERE rest <> ''
),
want AS (SELECT DISTINCT p FROM split WHERE p <> ''),
matched AS (
  SELECT pa.id AS path_id FROM want w JOIN path pa ON pa.p = w.p
  UNION
  SELECT pa.id AS path_id FROM want w JOIN path pa ON pa.base = w.p AND instr(w.p, '/') = 0
),
hits AS (
  SELECT ef.entry_id           AS id,
         count(DISTINCT ef.path_id) AS overlap,
         max(ef.mode)          AS mode,
         min(pa.p)             AS anchor
    FROM matched m
    JOIN entry_file ef ON ef.path_id = m.path_id
    JOIN path pa       ON pa.id      = m.path_id
   GROUP BY ef.entry_id
),
scored AS (
  SELECT e.id, e.uid, e.ts, e.kind, e.repo_key, e.worktree, e.origin, e.ephemeral,
         e.dedup_key, e.title, h.anchor, h.overlap,
         CASE
           WHEN e.next_steps <> '' THEN 'next: ' || e.next_steps
           WHEN e.outcome    <> '' THEN substr(e.outcome, 1,
                  CASE WHEN instr(e.outcome, '. ') > 0
                       THEN instr(e.outcome, '. ')
                       ELSE min(length(e.outcome), 400) END)
           ELSE ''
         END AS cont,
         (e.durability * h.overlap) AS score
    FROM hits h
    JOIN entries e ON e.id = h.id
   WHERE e.superseded = 0
     AND e.repo_key = :repo_key
     AND (CAST(:injectable_only AS INTEGER) = 0 OR e.injectable = 1)
     AND e.durability >= CAST(:durability_floor AS REAL)
     AND e.ts >= CAST(:since_epoch AS INTEGER)
     AND (:origin_filter = '' OR instr(',' || :origin_filter || ',', ',' || e.origin || ',') > 0)
     AND (:kind = '' OR e.kind = :kind)
),
ranked AS (
  SELECT s.*,
         ROW_NUMBER() OVER (PARTITION BY s.dedup_key ORDER BY s.score DESC, s.ts DESC, s.id DESC) AS rk,
         COUNT(*)     OVER (PARTITION BY s.dedup_key)                                             AS recurrence,
         ROW_NUMBER() OVER (PARTITION BY s.uid       ORDER BY s.ts DESC, s.score DESC, s.id DESC) AS urk
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
           'path',       r.anchor,
           'overlap',    r.overlap,
           'title',      r.title,
           'cont',       r.cont)
       ELSE
         r.id || char(31) || r.uid || char(31) || r.ts || char(31) || r.kind || char(31)
              || r.repo_key || char(31) || r.worktree || char(31) || r.origin || char(31)
              || r.ephemeral || char(31) || r.recurrence || char(31)
              || printf('%.4f', r.score) || char(31)
              || replace(replace(replace(replace(replace(r.title,  char(96), ''''), char(31), ' '), char(9), ' '), char(10), ' '), char(13), ' ')
              || char(31)
              || replace(replace(replace(replace(replace(r.cont,   char(96), ''''), char(31), ' '), char(9), ' '), char(10), ' '), char(13), ' ')
              || char(31)
              || replace(replace(replace(replace(r.anchor, char(31), ' '), char(9), ' '), char(10), ' '), char(13), ' ')
       END AS line
  FROM ranked r
 WHERE r.rk  <= CAST(:dup_cap AS INTEGER)
   AND r.urk <= CAST(:uid_cap AS INTEGER)
 ORDER BY r.score DESC, r.ts DESC, r.id DESC
 LIMIT CAST(:limit AS INTEGER);
SQL
}

# jm_file_lane_run PATHS_NEWLINE_JOINED NAME VALUE [NAME VALUE ...]
jm_file_lane_run() {
  local paths="${1-}"
  [ $# -ge 1 ] || return 2
  shift
  local sql
  sql="$(jm_file_lane_sql)" || return 1
  jm_db_param_read "$sql" paths "$paths" "$@"
}

# jm_file_lane_defaults — see jm_query_defaults.
jm_file_lane_defaults() {
  printf '%s\n' \
    repo_key '' \
    durability_floor 0.0 \
    origin_filter '' \
    kind '' \
    since_epoch 0 \
    limit 20 \
    uid_cap 1000000 \
    dup_cap 1 \
    injectable_only 0 \
    fmt text
}
