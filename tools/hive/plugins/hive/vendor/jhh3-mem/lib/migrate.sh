#!/usr/bin/env bash
# jhh3-mem — lib/migrate.sh
# Legacy import from the retired claude-mem archive. SPEC §11.
#
# OUT-OF-BAND ONLY. Never a hook, never automatic: nobody's startup hook should
# import 107 MB.
#
# NON-NEGOTIABLES ENFORCED HERE
#   - The archive is opened `file:...?mode=ro&immutable=1`. `immutable=1` is
#     mandatory, not cosmetic: plain mode=ro on a WAL archive CREATES -wal and
#     -shm sidecars next to it (A-E11, P102). "Never mutate the legacy file"
#     includes its directory. The archive sha256 is checked before and after.
#   - Redaction happens in the byte stream BEFORE anything is persisted, and
#     truncation happens after, so a cut can never split a secret.
#   - Idempotent: every write is INSERT OR IGNORE on ux_entries_uid_bytes, so a
#     re-run is a zero-row no-op and an interrupted run is resumed by re-running.
#   - Guarded by flock (jm_db_lock_ddl 30) — the one place a lock is justified,
#     because this is the only long write.
#
# shellcheck shell=bash

if [ -z "${JM_MIGRATE_SOURCED:-}" ]; then
  JM_MIGRATE_SOURCED=1

  _jm_mig_src="${BASH_SOURCE[0]}"
  case "$_jm_mig_src" in */*) _jm_mig_dir="${_jm_mig_src%/*}" ;; *) _jm_mig_dir="." ;; esac
  # shellcheck source=./db.sh
  . "$_jm_mig_dir/db.sh"
  unset _jm_mig_src _jm_mig_dir

  # Legacy project root -> repo_key, and root -> on-disk toplevel.
  declare -gA JM_SCOPE_KEY=()
  declare -gA JM_SCOPE_PATH=()
  declare -gA JM_SCOPE_SRC=()

  # Bounded search roots used to auto-resolve an unknown legacy project root.
  JM_REPO_SEARCH_ROOTS="${JHH3_MEM_REPO_SEARCH_ROOTS:-$HOME/code}"
fi

# ── scope resolution ────────────────────────────────────────────────────────

# _jm_scope_probe ROOT — echo "repo_key<TAB>repo_path" if a repo named ROOT is
# found under the bounded search roots and has an origin; else nothing.
_jm_scope_probe() {
  local root="$1" d origin key sr
  local -a search_roots=() t=(timeout 10)
  jm_have timeout || t=()   # stock macOS has no coreutils timeout
  jm_have find || return 0
  jm_have git || return 0
  IFS=':' read -r -a search_roots <<<"$JM_REPO_SEARCH_ROOTS"
  for sr in "${search_roots[@]}"; do
    [ -d "$sr" ] || continue
    while IFS= read -r d; do
      [ -n "$d" ] || continue
      [ -e "$d/.git" ] || continue
      origin="$(git -C "$d" remote get-url origin 2>/dev/null || true)"
      [ -n "$origin" ] || continue
      key="$(jm_repo_key_from_origin "$origin")"
      [ -n "$key" ] || continue
      printf '%s\t%s\n' "$key" "$d"
      return 0
    done < <(${t[@]+"${t[@]}"} find "$sr" -maxdepth 4 -type d -name "$root" \
      -not -path '*/node_modules/*' -not -path '*/.git/*' 2>/dev/null || true)
  done
  return 0
}

# jm_migrate_resolve_scope ROOT [ROOT ...] — populate JM_SCOPE_KEY/PATH.
# Resolution order: explicit --map · persisted meta · bounded filesystem probe ·
# interactive prompt (TTY only) · deterministic dir: fallback.
jm_migrate_resolve_scope() {
  local root probe key path answer
  for root in "$@"; do
    [ -n "$root" ] || continue
    [ -n "${JM_SCOPE_KEY[$root]:-}" ] && continue

    # persisted from a previous run
    key="$(jm_db_param_read "SELECT v FROM meta WHERE k = :k;" k "scope.$root" 2>/dev/null || true)"
    if [ -n "$key" ]; then
      JM_SCOPE_KEY[$root]="${key%%$'\t'*}"
      path="${key#*$'\t'}"
      [ "$path" = "$key" ] && path=''
      JM_SCOPE_PATH[$root]="$path"
      JM_SCOPE_SRC[$root]='meta'
      continue
    fi

    probe="$(_jm_scope_probe "$root")"
    if [ -n "$probe" ]; then
      JM_SCOPE_KEY[$root]="${probe%%$'\t'*}"
      JM_SCOPE_PATH[$root]="${probe#*$'\t'}"
      JM_SCOPE_SRC[$root]='probe'
      continue
    fi

    if [ -t 0 ] && [ -t 1 ]; then
      printf 'jhh3-mem migrate: legacy project root "%s" is unresolved.\n' "$root" >&2
      printf '  repo_key for "%s" (blank to keep it local-only): ' "$root" >&2
      IFS= read -r answer || answer=''
      if [ -n "$answer" ]; then
        JM_SCOPE_KEY[$root]="$answer"
        JM_SCOPE_PATH[$root]=''
        JM_SCOPE_SRC[$root]='prompt'
        continue
      fi
    fi

    JM_SCOPE_KEY[$root]="dir:$root@$(jm_sha256_str "legacy:$root" | cut -c1-8)"
    JM_SCOPE_PATH[$root]=''
    JM_SCOPE_SRC[$root]='unresolved'
  done
  return 0
}

# jm_migrate_scope_map LEGACY_PROJECT — "repo_key<TAB>worktree". rc 0 always.
jm_migrate_scope_map() {
  local project="${1:-}" root worktree
  root="${project%%/*}"
  if [ "$root" = "$project" ]; then worktree=''; else worktree="${project#*/}"; fi
  local key="${JM_SCOPE_KEY[$root]:-}"
  if [ -z "$key" ]; then
    jm_migrate_resolve_scope "$root"
    key="${JM_SCOPE_KEY[$root]:-}"
  fi
  printf '%s\t%s\n' "$key" "$worktree"
  return 0
}

# ── SQL fragments ───────────────────────────────────────────────────────────

# _jm_sql_text COLUMN — NULL-safe, fence-neutralised text expression.
# Control characters, tabs and backticks are stripped from the BYTE STREAM after
# the dump (they cannot alter SQL structure). Code fences must be handled here,
# in SQL, because ''' inside a generated string literal would unbalance it.
_jm_sql_text() {
  printf "replace(coalesce(%s,''), '\`\`\`', char(39,39,39))" "$1"
}

# _jm_sql_json_lines COLUMN — a JSON array flattened to newline-joined text,
# or '' when the column is not a JSON array.
_jm_sql_json_lines() {
  local c="$1"
  printf "coalesce((SELECT group_concat(replace(je.value, '\`\`\`', char(39,39,39)), char(10))
             FROM json_each(%s) je
             WHERE json_valid(%s) AND json_type(%s) = 'array'), '')" "$c" "$c" "$c"
}

# _jm_sql_ts COLUMN — legacy epochs are milliseconds; some rows are seconds.
_jm_sql_ts() {
  printf "CAST(CASE WHEN %s > 100000000000 THEN %s / 1000 ELSE %s END AS INTEGER)" "$1" "$1" "$1"
}

# _jm_sql_narray COLUMN — element count of a JSON array column, else 0.
_jm_sql_narray() {
  local c="$1"
  printf "CASE WHEN json_valid(%s) AND json_type(%s) = 'array' THEN json_array_length(%s) ELSE 0 END" "$c" "$c" "$c"
}

# _jm_defang (filter) — neutralise role markers AND the digest's fence tokens in
# the dumped stream, matching jm_sanitize on the capture path.
#
# The angle-bracket mapping is what stops a migrated title containing
# `</jhh3-mem>` from closing the SessionStart envelope early. lib/render.sh does
# the same thing at READ time (which is what protects rows migrated before this
# rule existed); doing it here as well means anything imported from now on is
# already safe in the database, where the guarantee is auditable.
#
# Safe on this stream specifically: it carries `.mode insert` statements, so
# every `<` or `>` is inside a string literal and the SQL stays valid.
# `.mode insert` encodes embedded newlines as the two characters \n, so an inner
# line start is either the start of the dump line or a literal \n.  '~' contains
# no quote character, so the generated SQL stays valid.
_jm_defang() {
  LC_ALL=C sed -E \
    -e 's/(\\n|[[:space:]]|^)(system|assistant|user|human)([[:space:]]*):/\1~\2\3:/gI' \
    -e 's/`//g' \
    -e 's/</‹/g' \
    -e 's/>/›/g'
}

# ── the stage schema ────────────────────────────────────────────────────────

_JM_STAGE_DDL="CREATE TABLE IF NOT EXISTS stage (
  uid           TEXT,
  origin        TEXT,
  repo_key      TEXT,
  worktree      TEXT,
  repo_path     TEXT,
  kind          TEXT,
  ts            INTEGER,
  title         TEXT,
  request       TEXT,
  outcome       TEXT,
  learned       TEXT,
  decided       TEXT,
  next_steps    TEXT,
  facts         TEXT,
  concepts      TEXT,
  files_touched TEXT,
  n_mod         INTEGER,
  n_read        INTEGER,
  injectable    INTEGER,
  ephemeral     INTEGER DEFAULT 0
);"

# Every stage SELECT lists all 20 columns positionally, in exactly this order —
# `.mode insert` emits positional INSERTs, so the count must match the DDL.
# `ephemeral` is seeded 0 and set by the grep pass against lib/ephemeral.ere.
# shellcheck disable=SC2034  # documentation of the positional column order
_JM_STAGE_COLS="uid,origin,repo_key,worktree,repo_path,kind,ts,title,request,outcome,learned,decided,next_steps,facts,concepts,files_touched,n_mod,n_read,injectable,ephemeral"

# ── the legacy SELECTs ──────────────────────────────────────────────────────

# _jm_legacy_scopemap_sql — TEMP scope table, built from the resolved map.
# A TEMP table is legal against an immutable database: it lives in the separate
# temp schema and touches no page of the archive.
_jm_legacy_scopemap_sql() {
  printf 'CREATE TEMP TABLE scopemap(root TEXT PRIMARY KEY, repo_key TEXT NOT NULL, repo_path TEXT NOT NULL);\n'
  local root
  for root in "${!JM_SCOPE_KEY[@]}"; do
    printf 'INSERT OR REPLACE INTO temp.scopemap VALUES(%s,%s,%s);\n' \
      "$(jm_sql_quote "$root")" \
      "$(jm_sql_quote "${JM_SCOPE_KEY[$root]}")" \
      "$(jm_sql_quote "${JM_SCOPE_PATH[$root]:-}")"
  done
}

# _jm_path_norm_cte — the repo-relative path normaliser, as a CTE chain.
# 89% of legacy files_modified paths are already repo-relative; the ~10%
# absolute and ~1% tilde paths are folded onto the same shape so they JOIN the
# majority. This is what makes the file lane bridge worktrees.
_jm_path_norm_cte() {
  local tbl="$1" col="$2" home
  home="$(jm_sql_quote "$HOME")"
  cat <<SQL
fm0 AS (
  SELECT o.id AS oid, je.key AS k, je.value AS raw,
         s.repo_path AS rp, s.root AS root,
         CASE WHEN instr(o.project,'/') = 0 THEN '' ELSE substr(o.project, instr(o.project,'/')+1) END AS w
  FROM $tbl o
  JOIN scoped s ON s.id = o.id
  , json_each(o.$col) je
  WHERE json_valid(o.$col) AND json_type(o.$col) = 'array'
),
fm1 AS (
  SELECT oid, k, rp, root, w,
         CASE WHEN raw LIKE '~/%' THEN $home || substr(raw, 2) ELSE raw END AS p
  FROM fm0
  WHERE raw IS NOT NULL AND raw <> ''
),
fm2 AS (
  SELECT oid, k,
    CASE
      WHEN w <> '' AND rp <> '' AND instr(p, rp || '/.worktrees/' || w || '/') = 1
        THEN substr(p, length(rp || '/.worktrees/' || w || '/') + 1)
      WHEN rp <> '' AND instr(p, rp || '/') = 1
        THEN substr(p, length(rp) + 2)
      WHEN instr(p, '/.worktrees/') > 0
           AND instr(substr(p, instr(p, '/.worktrees/') + 12), '/') > 0
        THEN substr(substr(p, instr(p, '/.worktrees/') + 12),
                    instr(substr(p, instr(p, '/.worktrees/') + 12), '/') + 1)
      WHEN root <> '' AND instr(p, '/' || root || '/') > 0
        THEN substr(p, instr(p, '/' || root || '/') + length(root) + 2)
      ELSE p
    END AS p
  FROM fm1
),
fm3 AS (
  SELECT oid, k, CASE WHEN p LIKE './%' THEN substr(p, 3) ELSE p END AS p FROM fm2
),
-- A path that still starts with '/' but whose first segment is not a real
-- filesystem root directory was a repo-relative path recorded with a stray
-- leading slash ('/backend/app/x.py'). Strip it so it JOINs the majority.
-- Genuinely-outside-the-repo paths (/home/..., /tmp/...) are left alone, per
-- jm_rel_path's contract.
fm4 AS (
  SELECT oid, k,
    CASE WHEN p LIKE '/%'
              AND p NOT LIKE '/home/%'  AND p NOT LIKE '/root/%'
              AND p NOT LIKE '/tmp/%'   AND p NOT LIKE '/var/%'
              AND p NOT LIKE '/usr/%'   AND p NOT LIKE '/etc/%'
              AND p NOT LIKE '/opt/%'   AND p NOT LIKE '/mnt/%'
              AND p NOT LIKE '/media/%' AND p NOT LIKE '/srv/%'
              AND p NOT LIKE '/proc/%'  AND p NOT LIKE '/sys/%'
              AND p NOT LIKE '/dev/%'   AND p NOT LIKE '/Users/%'
         THEN substr(p, 2) ELSE p END AS p
  FROM fm3
),
files AS (
  SELECT oid, group_concat(p, char(10)) AS files_touched
  FROM (SELECT oid, k, p FROM fm4 WHERE p <> '' ORDER BY oid, k)
  GROUP BY oid
)
SQL
}

# _jm_select_observations INCLUDE_SENSITIVE CONCEPT_VOCAB_SQL
_jm_select_observations() {
  local include_sensitive="$1" vocab="$2"
  local t_title t_sub t_narr f_facts n_mod n_read ts
  t_title="$(_jm_sql_text 'o.title')"
  t_sub="$(_jm_sql_text 'o.subtitle')"
  t_narr="$(_jm_sql_text 'o.narrative')"
  f_facts="$(_jm_sql_json_lines 'o.facts')"
  n_mod="$(_jm_sql_narray 'o.files_modified')"
  n_read="$(_jm_sql_narray 'o.files_read')"
  ts="$(_jm_sql_ts 'o.created_at_epoch')"

  local type_filter="o.type <> 'sensitive'"
  [ "$include_sensitive" = '1' ] && type_filter='1=1'

  cat <<SQL
WITH scoped AS (
  SELECT o.id AS id, sm.repo_key AS repo_key, sm.repo_path AS repo_path, sm.root AS root
  FROM observations o
  JOIN temp.scopemap sm
    ON sm.root = CASE WHEN instr(o.project,'/') = 0 THEN o.project
                      ELSE substr(o.project, 1, instr(o.project,'/') - 1) END
  WHERE $type_filter
),
$(_jm_path_norm_cte observations files_modified)
SELECT
  'lgc:obs:' || o.id,
  'legacy-note',
  s.repo_key,
  CASE WHEN instr(o.project,'/') = 0 THEN '' ELSE substr(o.project, instr(o.project,'/')+1) END,
  s.repo_path,
  CASE o.type
    WHEN 'security_alert' THEN 'security'
    WHEN 'security_note'  THEN 'security'
    WHEN 'sensitive'      THEN 'other'
    WHEN 'discovery'      THEN 'discovery'
    WHEN 'feature'        THEN 'feature'
    WHEN 'change'         THEN 'change'
    WHEN 'bugfix'         THEN 'bugfix'
    WHEN 'decision'       THEN 'decision'
    WHEN 'refactor'       THEN 'refactor'
    ELSE 'other'
  END,
  $ts,
  CASE
    WHEN trim(coalesce(o.title,'')) <> ''    THEN $t_title
    WHEN trim(coalesce(o.subtitle,'')) <> '' THEN $t_sub
    ELSE '(untitled observation)'
  END,
  '',
  trim($t_sub
       || CASE WHEN trim(coalesce(o.subtitle,'')) <> ''
                AND trim(coalesce(o.narrative,'')) <> ''
               THEN char(10) || char(10) ELSE '' END
       || $t_narr),
  '', '', '',
  $f_facts,
  coalesce((SELECT group_concat(je2.value, ' ')
            FROM json_each(o.concepts) je2
            WHERE json_valid(o.concepts) AND json_type(o.concepts)='array'
              AND je2.value IN ($vocab)), ''),
  coalesce(f.files_touched, ''),
  $n_mod,
  $n_read,
  CASE WHEN o.type = 'sensitive' THEN 0 ELSE 1 END,
  0
FROM observations o
JOIN scoped s ON s.id = o.id
LEFT JOIN files f ON f.oid = o.id
ORDER BY o.id;
SQL
}

# _jm_select_summaries
_jm_select_summaries() {
  local t_req t_inv t_learn t_comp t_next t_notes n_read ts
  t_req="$(_jm_sql_text 'o.request')"
  t_inv="$(_jm_sql_text 'o.investigated')"
  t_learn="$(_jm_sql_text 'o.learned')"
  t_comp="$(_jm_sql_text 'o.completed')"
  t_next="$(_jm_sql_text 'o.next_steps')"
  t_notes="$(_jm_sql_text 'o.notes')"
  n_read="$(_jm_sql_narray 'o.files_read')"
  ts="$(_jm_sql_ts 'o.created_at_epoch')"

  cat <<SQL
WITH scoped AS (
  SELECT o.id AS id, sm.repo_key AS repo_key, sm.repo_path AS repo_path, sm.root AS root
  FROM session_summaries o
  JOIN temp.scopemap sm
    ON sm.root = CASE WHEN instr(o.project,'/') = 0 THEN o.project
                      ELSE substr(o.project, 1, instr(o.project,'/') - 1) END
)
SELECT
  'lgc:sum:' || o.id,
  'legacy-session',
  s.repo_key,
  CASE WHEN instr(o.project,'/') = 0 THEN '' ELSE substr(o.project, instr(o.project,'/')+1) END,
  s.repo_path,
  'session',
  $ts,
  CASE WHEN trim(coalesce(o.request,'')) <> '' THEN substr($t_req, 1, 200)
       ELSE '(untitled session)' END,
  $t_req,
  $t_comp,
  $t_learn,
  trim(coalesce($t_inv,'') || CASE WHEN trim(coalesce(o.investigated,'')) <> ''
                                    AND trim(coalesce(o.notes,'')) <> ''
                                   THEN char(10) || char(10) ELSE '' END
                           || coalesce($t_notes,'')),
  $t_next,
  '',
  '',
  '',
  0,
  $n_read,
  1,
  0
FROM session_summaries o
JOIN scoped s ON s.id = o.id
ORDER BY o.id;
SQL
}

# _jm_select_prompts
_jm_select_prompts() {
  local t_p ts
  t_p="$(_jm_sql_text 'o.prompt_text')"
  ts="$(_jm_sql_ts 'o.created_at_epoch')"
  cat <<SQL
SELECT
  'lgc:prm:' || o.id,
  'legacy-prompt',
  coalesce((SELECT sm.repo_key FROM temp.scopemap sm
            JOIN sdk_sessions ss ON ss.id = o.session_db_id
            WHERE sm.root = CASE WHEN instr(ss.project,'/') = 0 THEN ss.project
                                 ELSE substr(ss.project, 1, instr(ss.project,'/') - 1) END),
           'dir:legacy@00000000'),
  '',
  '',
  'prompt',
  $ts,
  substr($t_p, 1, 200),
  $t_p,
  '', '', '', '', '', '', '',
  0, 0, 0, 0
FROM user_prompts o
ORDER BY o.id;
SQL
}

# ── the run ─────────────────────────────────────────────────────────────────

# jm_migrate_run LEGACY_PATH DRY_RUN WITH_PROMPTS INCLUDE_SENSITIVE
# stdout: the human-readable report.
# rc 0 ok · 1 failed · 3 no sqlite3 · 75 another migration holds the DDL lock.
jm_migrate_run() {
  local legacy="${1:-}" dry="${2:-0}" with_prompts="${3:-0}" include_sensitive="${4:-0}"

  jm_require_sqlite || { jm_log migrate degraded 'reason=no_sqlite3'; return 3; }
  [ -n "$legacy" ] || { printf 'migrate: --from <legacy.db> is required\n' >&2; return 1; }
  [ -r "$legacy" ] || { printf 'migrate: cannot read %s\n' "$legacy" >&2; return 1; }

  local legacy_abs
  legacy_abs="$(_jm_realpath "$legacy")"

  local lock_wait="${JHH3_MEM_LOCK_WAIT:-30}"
  case "$lock_wait" in '' | *[!0-9]*) lock_wait=30 ;; esac
  # NB: `cmd; local rc=$?` is fatal under `set -e` — the shell dies on the 75
  # before the log line runs. Capture through `||` so the branch is reachable.
  local lrc=0
  jm_db_lock_ddl "$lock_wait" || lrc=$?
  if [ "$lrc" -eq 75 ]; then
    jm_log migrate noop 'reason=migration_in_progress'
    printf 'migrate: another migration holds the lock; nothing written\n' >&2
    return 75
  elif [ "$lrc" -ne 0 ]; then
    jm_log migrate error 'reason=lock_failed'
    printf 'migrate: could not acquire the DDL lock\n' >&2
    return 1
  fi

  local stage rc=0
  stage="$(jm_mkstage)" || { jm_db_unlock_ddl; return 1; }

  _jm_migrate_body "$stage" "$legacy_abs" "$dry" "$with_prompts" "$include_sensitive" || rc=$?

  rm -rf -- "$stage"
  jm_db_unlock_ddl
  return "$rc"
}

# _jm_migrate_body STAGE LEGACY_ABS DRY WITH_PROMPTS INCLUDE_SENSITIVE
# Everything that can fail. The caller owns STAGE and the DDL lock, so no early
# return here can leak either.
_jm_migrate_body() {
  local STAGE="$1" legacy_abs="$2" dry="$3" with_prompts="$4" include_sensitive="$5"
  local t0; t0="$(jm_now_ns)"

  local sha_before sha_after
  sha_before="$(jm_sha256 "$legacy_abs")" || {
    printf 'migrate: cannot hash %s\n' "$legacy_abs" >&2; return 1; }

  # Record which archive sidecars already exist, so the post-run check reports
  # only files THIS run created.
  local sidecars_before
  sidecars_before="$( (ls -1 "$legacy_abs"-wal "$legacy_abs"-shm 2>/dev/null || true) | LC_ALL=C sort | tr '\n' ' ')"

  local legacy_uri="file:${legacy_abs}?mode=ro&immutable=1"

  # ── target DB ────────────────────────────────────────────────────────────
  if ! jm_db_bootstrap; then
    jm_log migrate error 'reason=bootstrap_failed'
    printf 'migrate: could not bootstrap %s (%s)\n' "$JM_DB" "$JM_DB_LAST_ERR" >&2
    return 1
  fi

  local rows_before
  rows_before="$(jm_db_read 'SELECT count(*) FROM entries;')" || rows_before=0
  case "$rows_before" in '' | *[!0-9]*) rows_before=0 ;; esac

  # A backup of OUR db (the 107 MB archive is already an archive and is not copied).
  local backup=''
  if [ "$dry" = '0' ] && [ "$rows_before" -gt 0 ]; then
    mkdir -p -- "$JM_BACKUPS" 2>/dev/null || true
    chmod 0700 "$JM_BACKUPS" 2>/dev/null || true
    # `VACUUM INTO` REFUSES to overwrite, and a second-resolution timestamp is
    # not unique: two migrations inside the same second (a re-run, a scripted
    # retry, the test suite) collided and the second one failed with
    # "output file already exists" — losing the migration, not just the backup.
    # The pid disambiguates concurrent processes; the counter disambiguates a
    # single process that migrates twice inside one second.
    local bstamp bseq=0
    bstamp="$JM_BACKUPS/pre-migrate-$(date +%Y%m%dT%H%M%S)-$$"
    backup="$bstamp.db"
    while [ -e "$backup" ] && [ "$bseq" -lt 100 ]; do
      bseq=$((bseq + 1))
      backup="$bstamp-$bseq.db"
    done
    if ! printf "VACUUM INTO %s;\n" "$(jm_sql_quote "$backup")" | jm_db_write; then
      printf 'migrate: backup to %s failed (%s)\n' "$backup" "$JM_DB_LAST_ERR" >&2
      return 1
    fi
    chmod 0600 "$backup" 2>/dev/null || true
  fi

  # ── scope map ────────────────────────────────────────────────────────────
  local roots
  roots="$(sqlite3 -bail -batch -noheader "$legacy_uri" "
    SELECT DISTINCT CASE WHEN instr(project,'/') = 0 THEN project
                         ELSE substr(project, 1, instr(project,'/') - 1) END
    FROM (SELECT project FROM observations UNION SELECT project FROM session_summaries)
    ORDER BY 1;" 2>/dev/null)" || {
    jm_log migrate error 'reason=legacy_unreadable'
    printf 'migrate: cannot read the legacy archive as a database\n' >&2
    return 1
  }

  local root
  while IFS= read -r root; do
    [ -n "$root" ] || continue
    jm_migrate_resolve_scope "$root"
  done <<<"$roots"

  # ── concept vocabulary (head-7 of the corpus) ────────────────────────────
  local vocab_rows vocab_sql=''
  vocab_rows="$(sqlite3 -bail -batch -noheader "$legacy_uri" "
    SELECT je.value FROM observations o, json_each(o.concepts) je
    WHERE json_valid(o.concepts) AND json_type(o.concepts) = 'array'
    GROUP BY je.value ORDER BY count(*) DESC LIMIT 7;" 2>/dev/null)" || vocab_rows=''
  local v
  while IFS= read -r v; do
    [ -n "$v" ] || continue
    [ -z "$vocab_sql" ] || vocab_sql="$vocab_sql,"
    vocab_sql="$vocab_sql$(jm_sql_quote "$v")"
  done <<<"$vocab_rows"
  [ -n "$vocab_sql" ] || vocab_sql="''"

  # ── dump -> sanitize -> redact -> defang -> stage.sql ────────────────────
  local dump_sql="$STAGE/dump.sql"
  local scopemap_sql="$STAGE/scopemap.sql"
  _jm_legacy_scopemap_sql >"$scopemap_sql"

  {
    printf '.bail on\n'
    cat -- "$scopemap_sql"
    printf '.mode insert stage\n'
    printf '.headers off\n'
    _jm_select_summaries
    _jm_select_observations "$include_sensitive" "$vocab_sql"
    [ "$with_prompts" = '1' ] && _jm_select_prompts
  } >"$STAGE/extract.sql"

  local t_dump; t_dump="$(jm_now_ns)"
  if ! sqlite3 -bail -batch -noheader "$legacy_uri" <"$STAGE/extract.sql" \
    | LC_ALL=C tr '\011' ' ' \
    | LC_ALL=C tr -d '\000-\010\013-\037\177' \
    | jm_redact_stream \
    | _jm_defang \
    >"$dump_sql"; then
    jm_log migrate error 'reason=extract_failed'
    printf 'migrate: legacy extract failed\n' >&2
    return 1
  fi
  local ms_dump; ms_dump="$(jm_elapsed_ms "$t_dump")"

  local red_total red_hist
  red_total="$(jm_redact_count "$dump_sql")" || red_total=0
  red_hist="$(jm_redact_histogram "$dump_sql")" || red_hist=''

  # ── load the stage DB ────────────────────────────────────────────────────
  local stage_db="$STAGE/stage.db"
  local t_load; t_load="$(jm_now_ns)"
  {
    printf '.bail on\n'
    printf 'PRAGMA journal_mode=OFF;\nPRAGMA synchronous=OFF;\n'
    printf '%s\n' "$_JM_STAGE_DDL"
    printf 'BEGIN;\n'
    cat -- "$dump_sql"
    printf 'COMMIT;\n'
  } | sqlite3 -bail -batch "$stage_db" >/dev/null 2>"$STAGE/load.err" || {
    jm_log migrate error 'reason=stage_load_failed'
    printf 'migrate: staging failed: %s\n' "$(head -3 "$STAGE/load.err" 2>/dev/null | tr '\n' ' ')" >&2
    return 1
  }
  local ms_load; ms_load="$(jm_elapsed_ms "$t_load")"

  local staged
  staged="$(sqlite3 -bail -batch -noheader "$stage_db" 'SELECT count(*) FROM stage;')" || staged=0
  case "$staged" in '' | *[!0-9]*) staged=0 ;; esac
  if [ "$staged" -eq 0 ]; then
    jm_log migrate error 'reason=stage_empty'
    printf 'migrate: staged 0 rows — nothing to import\n' >&2
    return 1
  fi

  # ── ephemeral flagging: one grep against lib/ephemeral.ere ───────────────
  # The ERE file is the single source of truth; translating it into SQL LIKE /
  # GLOB would be a second, silently-drifting implementation.
  local n_ephemeral=0
  if [ -r "$JM_EPHEMERAL_ERE" ]; then
    sqlite3 -bail -batch -noheader "$stage_db" \
      "SELECT rowid || char(9) || replace(replace(lower(title), char(10), ' '), char(9), ' ') FROM stage;" \
      >"$STAGE/titles.tsv" 2>/dev/null || : >"$STAGE/titles.tsv"
    LC_ALL=C grep -i -E -f "$JM_EPHEMERAL_ERE" -- "$STAGE/titles.tsv" 2>/dev/null \
      | LC_ALL=C cut -f1 >"$STAGE/ephemeral.ids" || : >"$STAGE/ephemeral.ids"
    if [ -s "$STAGE/ephemeral.ids" ]; then
      {
        printf '.bail on\nCREATE TEMP TABLE eph(id INTEGER);\n.mode list\n.import --schema temp %s eph\n' \
          "$(jm_sql_quote "$STAGE/ephemeral.ids")"
        printf 'UPDATE stage SET ephemeral = 1 WHERE rowid IN (SELECT id FROM temp.eph);\n'
        printf 'SELECT changes();\n'
      } | sqlite3 -bail -batch -noheader "$stage_db" >"$STAGE/eph.out" 2>/dev/null || true
      n_ephemeral="$(tail -1 "$STAGE/eph.out" 2>/dev/null || echo 0)"
      case "$n_ephemeral" in '' | *[!0-9]*) n_ephemeral=0 ;; esac
    fi
  fi

  # ── the collapse table + per-origin staged counts (reportable in dry-run) ─
  local collapse staged_by_origin durability_hist
  collapse="$(sqlite3 -bail -batch -noheader -separator '|' "$stage_db" "
    SELECT repo_key, count(DISTINCT worktree), count(*)
    FROM stage GROUP BY repo_key ORDER BY count(*) DESC;")" || collapse=''
  staged_by_origin="$(sqlite3 -bail -batch -noheader -separator '|' "$stage_db" "
    SELECT origin, count(*) FROM stage GROUP BY origin ORDER BY 1;")" || staged_by_origin=''

  # ── the copy into entries ────────────────────────────────────────────────
  local inserted=0 ms_copy=0 ms_fts=0 ms_vac=0
  if [ "$dry" = '0' ]; then
    local t_copy; t_copy="$(jm_now_ns)"
    local maxrow batch lo
    maxrow="$(sqlite3 -bail -batch -noheader "$stage_db" 'SELECT coalesce(max(rowid),0) FROM stage;')" || maxrow=0
    case "$maxrow" in '' | *[!0-9]*) maxrow=0 ;; esac
    batch=2000
    lo=0
    while [ "$lo" -lt "$maxrow" ]; do
      {
        printf '.bail on\n'
        printf "ATTACH DATABASE %s AS stg;\n" "$(jm_sql_quote "$stage_db")"
        printf 'BEGIN IMMEDIATE;\n'
        printf '%s\n' "$(_jm_copy_sql "$lo" "$((lo + batch))")"
        printf 'COMMIT;\nDETACH DATABASE stg;\n'
      } | jm_db_write || {
        jm_log migrate error "reason=copy_failed rc=$? lo=$lo"
        printf 'migrate: copy failed at rowid > %s (%s)\n' "$lo" "$JM_DB_LAST_ERR" >&2
        return 1
      }
      lo=$((lo + batch))
    done
    ms_copy="$(jm_elapsed_ms "$t_copy")"

    # path / entry_file, derived from the entries that just landed.
    local t_fts; t_fts="$(jm_now_ns)"
    _jm_files_sql | jm_db_write || {
      jm_log migrate error 'reason=file_lane_failed'
      printf 'migrate: file lane population failed (%s)\n' "$JM_DB_LAST_ERR" >&2
      return 1
    }
    ms_fts="$(jm_elapsed_ms "$t_fts")"

    local t_vac; t_vac="$(jm_now_ns)"
    printf 'VACUUM;\n' | jm_db_write || true
    ms_vac="$(jm_elapsed_ms "$t_vac")"

    local rows_after
    rows_after="$(jm_db_read 'SELECT count(*) FROM entries;')" || rows_after=0
    case "$rows_after" in '' | *[!0-9]*) rows_after=0 ;; esac
    inserted=$((rows_after - rows_before))

    # meta provenance
    {
      printf 'BEGIN IMMEDIATE;\n'
      _jm_meta_set legacy_source "$legacy_abs"
      _jm_meta_set legacy_sha256 "$sha_before"
      _jm_meta_set legacy_rows "$(jm_db_read "SELECT count(*) FROM entries WHERE origin LIKE 'legacy-%';")"
      _jm_meta_set legacy_migrated_at "$(jm_now_epoch)"
      for root in "${!JM_SCOPE_KEY[@]}"; do
        _jm_meta_set "scope.$root" "${JM_SCOPE_KEY[$root]}"$'\t'"${JM_SCOPE_PATH[$root]:-}"
      done
      printf 'COMMIT;\n'
    } | jm_db_write || true
  fi

  # ── archive integrity ────────────────────────────────────────────────────
  sha_after="$(jm_sha256 "$legacy_abs")" || sha_after='(unreadable)'
  local sidecars_after
  sidecars_after="$( (ls -1 "$legacy_abs"-wal "$legacy_abs"-shm 2>/dev/null || true) | LC_ALL=C sort | tr '\n' ' ')"

  # ── report ───────────────────────────────────────────────────────────────
  local total_ms; total_ms="$(jm_elapsed_ms "$t0")"
  {
    printf 'jhh3-mem migrate %s\n' "$([ "$dry" = '1' ] && printf '(DRY RUN — nothing written)' || printf '')"
    printf 'source\n'
    printf '  archive        %s (%s)\n' "$legacy_abs" "$(jm_human_bytes "$(_jm_file_size "$legacy_abs")")"
    printf '  opened as      file:...?mode=ro&immutable=1\n'
    printf '  sha256 before  %s\n' "$sha_before"
    printf '  sha256 after   %s   %s\n' "$sha_after" \
      "$([ "$sha_before" = "$sha_after" ] && printf 'IDENTICAL' || printf '*** MISMATCH ***')"
    if [ "$sidecars_before" = "$sidecars_after" ]; then
      printf '  sidecars       unchanged%s\n' "$([ -n "$sidecars_after" ] && printf ' (pre-existing: %s)' "$sidecars_after" || printf ' (none)')"
    else
      printf '  sidecars       *** THIS RUN CREATED: before=[%s] after=[%s] ***\n' "$sidecars_before" "$sidecars_after"
    fi
    printf 'scope map (legacy project root -> repo_key)\n'
    for root in $(printf '%s\n' "${!JM_SCOPE_KEY[@]}" | LC_ALL=C sort); do
      printf '  %-16s -> %-28s [%s]%s\n' "$root" "${JM_SCOPE_KEY[$root]}" \
        "${JM_SCOPE_SRC[$root]:-?}" \
        "$([ -n "${JM_SCOPE_PATH[$root]:-}" ] && printf '  %s' "${JM_SCOPE_PATH[$root]}" || printf '')"
    done
    printf 'scope collapse (repo_key · distinct worktrees · rows)\n'
    printf '%s\n' "$collapse" | LC_ALL=C awk -F'|' 'NF>=3{printf "  %-30s %4s worktrees %8s rows\n",$1,$2,$3}'
    printf 'staged\n'
    printf '%s\n' "$staged_by_origin" | LC_ALL=C awk -F'|' 'NF>=2{printf "  %-16s %8s\n",$1,$2}'
    printf '  %-16s %8s\n' TOTAL "$staged"
    printf '  %-16s %8s  (durability x0.35; penalty, not a filter)\n' ephemeral "$n_ephemeral"
    printf 'redaction (applied to the byte stream BEFORE anything was persisted)\n'
    if [ -n "$red_hist" ]; then
      printf '%s\n' "$red_hist" | LC_ALL=C awk -F'\t' '{printf "  %-16s %8s\n",$1,$2}'
    fi
    printf '  %-16s %8s\n' TOTAL "$red_total"
    if [ "$dry" = '0' ]; then
      printf 'written\n'
      printf '  entries before %8s\n' "$rows_before"
      printf '  entries after  %8s\n' "$rows_after"
      printf '  new rows       %8s\n' "$inserted"
      printf '  suppressed     %8s  (already present: INSERT OR IGNORE on ux_entries_uid_bytes)\n' \
        "$((staged - inserted))"
      [ -n "$backup" ] && printf '  backup         %s\n' "$backup"
      durability_hist="$(jm_db_read "
        SELECT bucket || '|' || n FROM (
          SELECT CASE WHEN durability <  0.30 THEN '< 0.30'
                      WHEN durability <  0.60 THEN '0.30-0.60'
                      WHEN durability <  0.90 THEN '0.60-0.90'
                      WHEN durability <  1.50 THEN '0.90-1.50'
                      ELSE                         '>= 1.50' END AS bucket,
                 count(*) AS n
          FROM entries WHERE origin LIKE 'legacy-%'
          GROUP BY bucket
        ) ORDER BY bucket;")" || durability_hist=''
      printf 'durability histogram (legacy rows; injection floor is 0.9)\n'
      printf '%s\n' "$durability_hist" | LC_ALL=C awk -F'|' 'NF>=2{printf "  %-12s %8s\n",$1,$2}'
      printf 'dedup\n'
      printf '  dedup_key groups with >1 row: %s  (rows collapsed at query time: %s)\n' \
        "$( (jm_db_read "SELECT count(*) FROM (SELECT dedup_key FROM entries GROUP BY dedup_key HAVING count(*)>1);") || echo '-')" \
        "$( (jm_db_read "SELECT coalesce(sum(n-1),0) FROM (SELECT count(*) n FROM entries GROUP BY dedup_key HAVING count(*)>1);") || echo '-')"
      printf 'file lane\n'
      printf '  path rows      %8s\n' "$( (jm_db_read 'SELECT count(*) FROM path;') || echo '-')"
      printf '  entry_file     %8s\n' "$( (jm_db_read 'SELECT count(*) FROM entry_file;') || echo '-')"
      printf 'fts5\n'
      if printf "INSERT INTO entries_fts(entries_fts) VALUES('integrity-check');\n" | jm_db_write; then
        printf '  integrity      ok\n'
      else
        printf '  integrity      *** FAILED (%s) ***\n' "$JM_DB_LAST_ERR"
      fi
    fi
    printf 'timing\n'
    printf '  extract+redact %6s ms\n' "$ms_dump"
    printf '  stage load     %6s ms\n' "$ms_load"
    [ "$dry" = '0' ] && printf '  copy           %6s ms\n  file lane      %6s ms\n  vacuum         %6s ms\n' \
      "$ms_copy" "$ms_fts" "$ms_vac"
    printf '  TOTAL          %6s ms\n' "$total_ms"
  }

  jm_log migrate ok "rows=$inserted staged=$staged red=$red_total ms=$total_ms dry=$dry"
  [ "$dry" = '0' ] && jm_db_upsert_ops migrate ok "rows=$inserted redactions=$red_total ms=$total_ms" || true

  if [ "$sha_before" != "$sha_after" ]; then
    printf 'migrate: FATAL — the archive changed during the import\n' >&2
    return 1
  fi
  return 0
}

# _jm_meta_set K V — one meta upsert statement.
_jm_meta_set() {
  printf "INSERT INTO meta(k,v) VALUES(%s,%s) ON CONFLICT(k) DO UPDATE SET v = excluded.v;\n" \
    "$(jm_sql_quote "$1")" "$(jm_sql_quote "$2")"
}

# _jm_copy_sql LO HI — INSERT OR IGNORE INTO entries SELECT ... FROM stg.stage
# for stage rowids in (LO, HI]. Every text column is truncated to its CHECK
# limit here, AFTER redaction, so no CHECK constraint error can ever reach the
# log and no cut can split a secret.
_jm_copy_sql() {
  local lo="$1" hi="$2"
  cat <<SQL
INSERT OR IGNORE INTO entries(
  uid, source_bytes, origin, producer, host, repo_key, worktree, branch, repo_path,
  kind, ts, started_at, title, request, outcome, learned, decided, next_steps,
  facts, concepts, files_touched, commands, capture_mode, end_reason, turns,
  redactions, durability, dedup_key, n_mod, n_read, ephemeral, injectable, superseded)
SELECT
  s.uid,
  0,
  s.origin,
  '$JM_PRODUCER',
  'claude-code',   -- legacy rows came from claude-mem, which was Claude Code only
  s.repo_key,
  substr(coalesce(s.worktree,''), 1, 120),
  '',
  coalesce(s.repo_path,''),
  s.kind,
  s.ts,
  NULL,
  substr(nullif(trim(coalesce(s.title,'')),''), 1, 200),
  substr(coalesce(s.request,''),       1, 2000),
  substr(coalesce(s.outcome,''),       1, 4000),
  substr(coalesce(s.learned,''),       1, 4000),
  substr(coalesce(s.decided,''),       1, 4000),
  substr(coalesce(s.next_steps,''),    1, 2000),
  substr(coalesce(s.facts,''),         1, 6000),
  substr(coalesce(s.concepts,''),      1,  400),
  substr(coalesce(s.files_touched,''), 1, 4000),
  '',
  'legacy',
  '',
  0,
  (length(coalesce(s.title,'') || coalesce(s.request,'') || coalesce(s.outcome,'') ||
          coalesce(s.learned,'') || coalesce(s.decided,'') || coalesce(s.next_steps,'') ||
          coalesce(s.facts,''))
   - length(replace(coalesce(s.title,'') || coalesce(s.request,'') || coalesce(s.outcome,'') ||
          coalesce(s.learned,'') || coalesce(s.decided,'') || coalesce(s.next_steps,'') ||
          coalesce(s.facts,''), '[REDACTED:', ''))) / 10,
  round(
    (CASE s.kind
       WHEN 'security'  THEN 1.80 WHEN 'note'     THEN 1.60
       WHEN 'bugfix'    THEN 1.50 WHEN 'refactor' THEN 1.50
       WHEN 'decision'  THEN 1.40 WHEN 'change'   THEN 1.30
       WHEN 'gotcha'    THEN 1.30 WHEN 'session'  THEN 1.20
       WHEN 'feature'   THEN 0.90 WHEN 'discovery' THEN 0.85
       WHEN 'prompt'    THEN 0.50 ELSE 1.00 END)
  * (CASE WHEN coalesce(s.n_mod,0)  > 0 THEN 1.35
          WHEN coalesce(s.n_read,0) > 0 THEN 1.15
          ELSE 0.70 END)
  * (CASE WHEN coalesce(s.ephemeral,0) = 1 THEN 0.35 ELSE 1.00 END), 2),
  substr(replace(replace(replace(replace(replace(
    lower(coalesce(s.title,'')), ' ',''), '-',''), '_',''), '.',''), ':',''), 1, 48),
  coalesce(s.n_mod, 0),
  coalesce(s.n_read, 0),
  coalesce(s.ephemeral, 0),
  coalesce(s.injectable, 1),
  0
FROM stg.stage s
WHERE s.rowid > $lo AND s.rowid <= $hi
  AND coalesce(nullif(trim(coalesce(s.title,'')),''), '') <> '';
SQL
}

# _jm_files_sql — populate path/entry_file from entries.files_touched.
_jm_files_sql() {
  cat <<'SQL'
BEGIN IMMEDIATE;
CREATE TEMP TABLE IF NOT EXISTS ft(entry_id INTEGER, p TEXT);
DELETE FROM temp.ft;
WITH RECURSIVE split(id, rest, item) AS (
  SELECT id, files_touched || char(10), ''
    FROM entries
   WHERE files_touched <> ''
     AND id NOT IN (SELECT DISTINCT entry_id FROM entry_file)
  UNION ALL
  SELECT id,
         substr(rest, instr(rest, char(10)) + 1),
         substr(rest, 1, instr(rest, char(10)) - 1)
    FROM split
   WHERE rest <> ''
)
INSERT INTO temp.ft(entry_id, p)
SELECT id, item FROM split WHERE item <> '';

INSERT OR IGNORE INTO path(p, base)
SELECT DISTINCT f.p,
       CASE WHEN instr(f.p, '/') = 0 THEN f.p
            ELSE replace(f.p, rtrim(f.p, replace(f.p, '/', '')), '') END
  FROM temp.ft f;

INSERT OR IGNORE INTO entry_file(path_id, entry_id, mode)
SELECT pt.id, f.entry_id, 2
  FROM temp.ft f JOIN path pt ON pt.p = f.p;

DROP TABLE temp.ft;
COMMIT;
SQL
}

# ── CLI ─────────────────────────────────────────────────────────────────────

_jm_migrate_usage() {
  cat <<'USAGE'
usage: jhh3-mem migrate --from <legacy.db> [options]

  --from PATH            the legacy claude-mem archive (opened read-only+immutable)
  --dry-run              stage, redact and report; write nothing
  --with-prompts         also import legacy user_prompts (kind=prompt, injectable=0)
  --include-sensitive    also import observations.type='sensitive' (injectable=0)
  --map ROOT=REPO_KEY    pin a legacy project root to a repo_key (repeatable)
  -h, --help             this text
USAGE
}

jm_migrate_main() {
  local legacy='' dry=0 with_prompts=0 include_sensitive=0
  while [ $# -gt 0 ]; do
    case "$1" in
      --from) legacy="${2:-}"; shift 2 ;;
      --from=*) legacy="${1#--from=}"; shift ;;
      --dry-run) dry=1; shift ;;
      --with-prompts) with_prompts=1; shift ;;
      --include-sensitive) include_sensitive=1; shift ;;
      --map)
        local pair="${2:-}"; shift 2
        JM_SCOPE_KEY["${pair%%=*}"]="${pair#*=}"
        JM_SCOPE_PATH["${pair%%=*}"]=''
        JM_SCOPE_SRC["${pair%%=*}"]='flag'
        ;;
      --map=*)
        local pair2="${1#--map=}"; shift
        JM_SCOPE_KEY["${pair2%%=*}"]="${pair2#*=}"
        JM_SCOPE_PATH["${pair2%%=*}"]=''
        JM_SCOPE_SRC["${pair2%%=*}"]='flag'
        ;;
      -h | --help) _jm_migrate_usage; return 0 ;;
      *) printf 'migrate: unknown argument: %s\n' "$1" >&2; _jm_migrate_usage >&2; return 2 ;;
    esac
  done
  jm_migrate_run "$legacy" "$dry" "$with_prompts" "$include_sensitive"
}

if [ "${BASH_SOURCE[0]}" = "${0}" ]; then
  set -euo pipefail
  jm_migrate_main "$@"
fi
