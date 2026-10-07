#!/usr/bin/env bash
# jhh3-mem — lib/db.sh
# Open / bootstrap / version-gate; read vs write invocation policy; pragma policy.
# SPEC §4.1, §6.2.
#
# INVARIANTS ENFORCED HERE
#   - Every WRITE invocation carries -bail. Without it sqlite3 keeps executing
#     after an error and leaves partial writes committed (adjudication 17).
#   - Every READ invocation is -readonly against a mode=ro URI: it cannot block
#     and cannot be blocked (P80).
#   - busy_timeout is set with `-cmd ".timeout N"` BEFORE any SQL so it also
#     covers dot-commands and .import (P55).
#   - Every pragma-bearing invocation discards stdout: `PRAGMA journal_mode=WAL`
#     prints "wal", and in session-start.sh that string would land in the
#     model's context.
#   - Hooks NEVER run DDL. jm_db_bootstrap is a first-run create only.
#
# shellcheck shell=bash

if [ -n "${JM_DB_SOURCED:-}" ]; then
  # shellcheck disable=SC2317  # reached when the file is executed, not sourced
  return 0 2>/dev/null || exit 0
fi
JM_DB_SOURCED=1

if [ -z "${JM_COMMON_SOURCED:-}" ]; then
  _jm_db_src="${BASH_SOURCE[0]}"
  case "$_jm_db_src" in */*) _jm_db_dir="${_jm_db_src%/*}" ;; *) _jm_db_dir="." ;; esac
  # shellcheck source=./common.sh
  . "$_jm_db_dir/common.sh"
  unset _jm_db_src _jm_db_dir
fi

JM_DB_BUSY_WRITE_MS="${JHH3_MEM_BUSY_WRITE_MS:-4000}"
JM_DB_BUSY_READ_MS="${JHH3_MEM_BUSY_READ_MS:-1000}"
case "$JM_DB_BUSY_WRITE_MS" in '' | *[!0-9]*) JM_DB_BUSY_WRITE_MS=4000 ;; esac
case "$JM_DB_BUSY_READ_MS"  in '' | *[!0-9]*) JM_DB_BUSY_READ_MS=1000 ;; esac
readonly JM_DB_BUSY_WRITE_MS JM_DB_BUSY_READ_MS

# Populated by the last jm_db_write / jm_db_read: the raw sqlite3 stderr, for
# the caller's log line. Never printed to stdout, never contains user content
# beyond what sqlite3 itself emitted.
JM_DB_LAST_ERR=''

# fd held by jm_db_lock_ddl for the caller's lifetime.
JM_LOCK_FD="${JM_LOCK_FD:-}"

# ── internals ───────────────────────────────────────────────────────────────

# _jm_db_uri [QUERYSTRING] — percent-encode $JM_DB into a file: URI.
_jm_db_uri() {
  local q="${1:-}" enc
  enc="$(LC_ALL=C printf '%s' "$JM_DB" | LC_ALL=C awk '
    BEGIN { for (i = 0; i < 256; i++) ord[sprintf("%c", i)] = i }
    {
      n = length($0)
      for (i = 1; i <= n; i++) {
        c = substr($0, i, 1)
        if (c ~ /[A-Za-z0-9._~\/-]/) printf "%s", c
        else printf "%%%02X", ord[c]
      }
    }')"
  if [ -n "$q" ]; then
    printf 'file:%s?%s\n' "$enc" "$q"
  else
    printf 'file:%s\n' "$enc"
  fi
}

# _jm_db_err_is_perm STDERR — rc 0 when the message is an access-control
# failure rather than a data failure.
_jm_db_err_is_perm() {
  case "${1:-}" in
    *'unable to open database file'* \
      | *'attempt to write a readonly database'* \
      | *'readonly database'* \
      | *'permission denied'*) return 0 ;;
  esac
  return 1
}

# _jm_db_perm_suspect — rc 0 when $JM_DB exists but this uid cannot both read
# and write it.
#
# Filesystem truth, not sqlite3's message. JM_DB_LAST_ERR is assigned inside a
# command substitution on every read path, so it dies with that subshell and is
# empty in the caller exactly when the caller needs it. Asking the kernel is the
# only answer that survives.
_jm_db_perm_suspect() {
  [ -e "$JM_DB" ] || return 1
  [ -r "$JM_DB" ] && [ -w "$JM_DB" ] && return 1
  return 0
}

# _jm_db_mode_owner — "0000 uid:gid" for $JM_DB, for the log line and `doctor`.
# The one fact that tells a user whether to reach for a backup or for chmod.
_jm_db_mode_owner() {
  local m='' o=''
  m="$(stat -c%a -- "$JM_DB" 2>/dev/null)" || m="$(stat -f%Lp -- "$JM_DB" 2>/dev/null)" || m='?'
  o="$(stat -c%u:%g -- "$JM_DB" 2>/dev/null)" || o="$(stat -f%u:%g -- "$JM_DB" 2>/dev/null)" || o='?'
  printf '%s/%s\n' "${m:-?}" "${o:-?}"
}

# _jm_db_classify RC STDERR — map a sqlite3 failure onto the contract's rc.
#   5  busy (lock contention past busy_timeout)
#   14 cannot open / permission denied — the file is FINE, the access is not
#   26 not a database (corrupt)
#   1  everything else
#
# The permission branch runs BEFORE the corruption branch on purpose. sqlite3
# reports a chmod-000 database with a message that reads like damage, and
# telling a user their memory store is destroyed when it is only unreadable
# sends them to a backup instead of to `chmod`.
_jm_db_classify() {
  local rc="${1:-1}" err="${2:-}"
  if _jm_db_err_is_perm "$err"; then printf '14\n'; return 0; fi
  case "$rc" in 5 | 26) printf '%s\n' "$rc"; return 0 ;; esac
  case "$err" in
    *'file is not a database'* | *'not a database'* | *'file is encrypted'*) printf '26\n' ;;
    *'database is locked'* | *'database table is locked'* | *'is locked'*) printf '5\n' ;;
    *) printf '1\n' ;;
  esac
}

# ── contract ────────────────────────────────────────────────────────────────

# jm_db_present — rc 0 $JM_DB exists and is non-empty · 1 otherwise.
# NOTE: a zero-length file is a VALID EMPTY DATABASE (P57.3). "File exists" is
# not "schema exists" — callers must use jm_db_gate for that.
jm_db_present() {
  [ -f "$JM_DB" ] && [ -s "$JM_DB" ]
}

# jm_db_read SQL | jm_db_read -   (SQL on stdin)
# stdout: whatever the SQL selects.
# rc 0 ok · 1 sqlite3 error · 3 sqlite3 absent.
jm_db_read() {
  jm_require_sqlite || return 3
  local sql="${1:-}"
  [ -n "$sql" ] || return 2
  local out rc err_file
  err_file="$(mktemp "${TMPDIR:-/tmp}/jhh3-mem-err.XXXXXX" 2>/dev/null)" || err_file=/dev/null

  if [ "$sql" = '-' ]; then
    out="$(sqlite3 -bail -batch -readonly -cmd ".timeout $JM_DB_BUSY_READ_MS" \
      "$(_jm_db_uri 'mode=ro')" 2>"$err_file")" && rc=0 || rc=$?
  else
    out="$(printf '%s\n' "$sql" | sqlite3 -bail -batch -readonly \
      -cmd ".timeout $JM_DB_BUSY_READ_MS" "$(_jm_db_uri 'mode=ro')" 2>"$err_file")" && rc=0 || rc=$?
  fi

  JM_DB_LAST_ERR="$(tr '\n' ' ' <"$err_file" 2>/dev/null)"
  [ "$err_file" = /dev/null ] || rm -f -- "$err_file"

  if [ "$rc" -ne 0 ]; then return 1; fi
  [ -n "$out" ] && printf '%s\n' "$out"
  return 0
}

# jm_db_param_read SQL NAME VALUE [NAME VALUE ...]
# THE ONLY SANCTIONED WAY to get agent- or user-supplied text into a query.
# Values are moved as `cast(x'<hex>' as text)`, which cannot be misparsed by the
# shell or by the sqlite3 dot-command tokenizer regardless of content.
# rc 0/1/3 as jm_db_read · 2 misuse (odd number of NAME/VALUE args).
jm_db_param_read() {
  jm_require_sqlite || return 3
  local sql="${1:-}"
  [ -n "$sql" ] || return 2
  shift
  [ $(($# % 2)) -eq 0 ] || return 2

  local -a cmds=(-cmd ".timeout $JM_DB_BUSY_READ_MS")
  local name value
  while [ $# -gt 0 ]; do
    name="$1"; value="$2"; shift 2
    case "$name" in :*) : ;; *) name=":$name" ;; esac
    case "$name" in
      :[A-Za-z_]*) : ;;
      *) return 2 ;;
    esac
    cmds+=(-cmd ".parameter set $name cast(x'$(_jm_hex "$value")'as'text')")
  done

  local out rc err_file
  err_file="$(mktemp "${TMPDIR:-/tmp}/jhh3-mem-err.XXXXXX" 2>/dev/null)" || err_file=/dev/null
  out="$(printf '%s\n' "$sql" | sqlite3 -bail -batch -readonly "${cmds[@]}" \
    "$(_jm_db_uri 'mode=ro')" 2>"$err_file")" && rc=0 || rc=$?
  JM_DB_LAST_ERR="$(tr '\n' ' ' <"$err_file" 2>/dev/null)"
  [ "$err_file" = /dev/null ] || rm -f -- "$err_file"

  [ "$rc" -eq 0 ] || return 1
  [ -n "$out" ] && printf '%s\n' "$out"
  return 0
}

# jm_db_write   (SQL on stdin)
# stdout: nothing — redirected to /dev/null so a stray PRAGMA result can never
#         reach a hook's stdout.
# rc 0 committed · 5 busy · 26 not-a-database · 1 other · 3 sqlite3 absent.
jm_db_write() {
  jm_require_sqlite || return 3
  jm_init_store || { JM_DB_LAST_ERR='store dir not writable'; return 1; }

  local rc err_file
  err_file="$(mktemp "${TMPDIR:-/tmp}/jhh3-mem-err.XXXXXX" 2>/dev/null)" || err_file=/dev/null

  # foreign_keys is a per-CONNECTION pragma and defaults OFF. Without it here,
  # entry_file's `ON DELETE CASCADE` silently does not fire, so `forget` would
  # orphan rows against a table whose primary key starts with path_id.
  sqlite3 -bail -batch -cmd ".timeout $JM_DB_BUSY_WRITE_MS" \
    -cmd 'PRAGMA foreign_keys=ON;' -- "$JM_DB" \
    >/dev/null 2>"$err_file" && rc=0 || rc=$?

  JM_DB_LAST_ERR="$(tr '\n' ' ' <"$err_file" 2>/dev/null)"
  [ "$err_file" = /dev/null ] || rm -f -- "$err_file"

  [ "$rc" -eq 0 ] && { chmod 0600 "$JM_DB" 2>/dev/null || true; return 0; }
  return "$(_jm_db_classify "$rc" "$JM_DB_LAST_ERR")"
}

# jm_db_param_write SQL NAME VALUE [NAME VALUE ...]
# The write-side twin of jm_db_param_read, and THE ONLY SANCTIONED WAY to get
# agent- or user-supplied text into a statement that mutates the store. Values
# move as `cast(x'<hex>' as text)`, immune to both the shell and the sqlite3
# dot-command tokenizer regardless of content. Carries -bail, the write
# busy_timeout, and foreign_keys=ON, exactly like jm_db_write.
# stdout: nothing. rc as jm_db_write · 2 misuse.
jm_db_param_write() {
  jm_require_sqlite || return 3
  local sql="${1:-}"
  [ -n "$sql" ] || return 2
  shift
  [ $(($# % 2)) -eq 0 ] || return 2
  jm_init_store || { JM_DB_LAST_ERR='store dir not writable'; return 1; }

  local -a cmds=(-cmd ".timeout $JM_DB_BUSY_WRITE_MS" -cmd 'PRAGMA foreign_keys=ON;')
  local name value
  while [ $# -gt 0 ]; do
    name="$1"; value="$2"; shift 2
    case "$name" in :*) : ;; *) name=":$name" ;; esac
    case "$name" in
      :[A-Za-z_]*) : ;;
      *) return 2 ;;
    esac
    cmds+=(-cmd ".parameter set $name cast(x'$(_jm_hex "$value")'as'text')")
  done

  local rc err_file
  err_file="$(mktemp "${TMPDIR:-/tmp}/jhh3-mem-err.XXXXXX" 2>/dev/null)" || err_file=/dev/null
  printf '%s\n' "$sql" | sqlite3 -bail -batch "${cmds[@]}" -- "$JM_DB" \
    >/dev/null 2>"$err_file" && rc=0 || rc=$?
  JM_DB_LAST_ERR="$(tr '\n' ' ' <"$err_file" 2>/dev/null)"
  [ "$err_file" = /dev/null ] || rm -f -- "$err_file"

  [ "$rc" -eq 0 ] && { chmod 0600 "$JM_DB" 2>/dev/null || true; return 0; }
  return "$(_jm_db_classify "$rc" "$JM_DB_LAST_ERR")"
}

# jm_db_schema_version — integer, or 0 if the DB or the meta row is absent.
# rc 0 ALWAYS — an unreadable or corrupt DB reports 0.
jm_db_schema_version() {
  local v=''
  if [ -f "$JM_DB" ]; then
    v="$(jm_db_read "SELECT v FROM meta WHERE k='schema_version';" 2>/dev/null)" || v=''
  fi
  case "$v" in
    '' | *[!0-9]*) printf '0\n' ;;
    *) printf '%s\n' "$v" ;;
  esac
  return 0
}

# jm_db_bootstrap — create the store, apply db/schema.sql idempotently, set WAL,
# feature-detect exp(), write meta + schema_migrations.
# rc 0 schema present at JM_SCHEMA_VERSION · 5 busy · 14 permission · 26 corrupt
#    · 1 other failure · 3 sqlite3 absent.
# The classified rc is PROPAGATED rather than flattened to 1: on a first-run
# race, five concurrent SessionEnd hooks all attempt the DDL and the losers see
# SQLITE_BUSY, which the caller must be able to tell apart from a real failure.
# Idempotent. NEVER called from session-start.sh.
jm_db_bootstrap() {
  jm_require_sqlite || return 3
  jm_init_store || return 1
  [ -r "$JM_SCHEMA_SQL" ] || { JM_DB_LAST_ERR="schema.sql missing: $JM_SCHEMA_SQL"; return 1; }

  # NB: `pipeline; local rc=$?` is fatal under `set -e` — the shell dies before
  # the capture runs. Every rc here is taken through `|| wrc=$?`.
  # Pre-v2 stores predate `entries.host`. CREATE TABLE IF NOT EXISTS cannot add a
  # column, and SQLite has no conditional DDL, so probe first and emit the ALTER
  # only when it is actually missing. Idempotent: a v2 store skips this entirely.
  if [ -s "$JM_DB" ]; then
    local _hascol
    _hascol="$(jm_db_read "SELECT count(*) FROM pragma_table_info('entries') WHERE name='host';" 2>/dev/null)"
    if [ "${_hascol:-1}" = "0" ]; then
      printf "ALTER TABLE entries ADD COLUMN host TEXT NOT NULL DEFAULT 'claude-code';\n" \
        | jm_db_write || return $?
    fi
  fi

  local wrc=0

  # journal_mode is persistent and must be set outside a transaction. Its stdout
  # ("wal") is discarded here and nowhere else.
  {
    printf 'PRAGMA journal_mode=WAL;\n'
    printf 'PRAGMA synchronous=NORMAL;\n'
    printf 'PRAGMA temp_store=MEMORY;\n'
    printf 'PRAGMA foreign_keys=ON;\n'
    printf 'BEGIN IMMEDIATE;\n'
    cat -- "$JM_SCHEMA_SQL"
    printf "INSERT OR IGNORE INTO meta(k,v) VALUES('schema_version','%s');\n" "$JM_SCHEMA_VERSION"
    printf "UPDATE meta SET v='%s' WHERE k='schema_version';\n" "$JM_SCHEMA_VERSION"
    printf "INSERT OR IGNORE INTO meta(k,v) VALUES('created_at',CAST(strftime('%%s','now') AS TEXT));\n"
    printf "INSERT OR IGNORE INTO meta(k,v) VALUES('created_by','%s');\n" "$JM_PRODUCER"
    printf "INSERT OR IGNORE INTO schema_migrations(version,applied_at,note) VALUES(%s,CAST(strftime('%%s','now') AS INTEGER),'initial schema');\n" "$JM_SCHEMA_VERSION"
    printf 'COMMIT;\n'
  } | jm_db_write || wrc=$?
  [ "$wrc" -eq 0 ] || return "$wrc"

  # Feature-detect exp(): ENABLE_MATH_FUNCTIONS is present on this host but is
  # not universal, and rank.sql substitutes a stepped decay when it is absent.
  local has_math=0
  if sqlite3 -bail -batch ':memory:' 'SELECT exp(1);' >/dev/null 2>&1; then
    has_math=1
  fi
  wrc=0
  printf "INSERT INTO meta(k,v) VALUES('has_math','%s')
          ON CONFLICT(k) DO UPDATE SET v=excluded.v;\n" "$has_math" | jm_db_write || wrc=$?
  [ "$wrc" -eq 0 ] || return "$wrc"

  chmod 0600 "$JM_DB" 2>/dev/null || true
  [ "$(jm_db_schema_version)" = "$JM_SCHEMA_VERSION" ] || return 1
  return 0
}

# jm_db_gate
# rc 0 meta.schema_version == JM_SCHEMA_VERSION
#    1 DB absent or unreadable
#    2 DB is BEHIND the code (caller prints the one visible upgrade line, §8.4)
#    3 DB is AHEAD of the code (caller logs degraded reason=schema_ahead)
jm_db_gate() {
  jm_have sqlite3 || return 1
  [ -f "$JM_DB" ] || return 1
  local have
  have="$(jm_db_schema_version)"
  [ "$have" = '0' ] && return 1
  [ "$have" -eq "$JM_SCHEMA_VERSION" ] && return 0
  [ "$have" -lt "$JM_SCHEMA_VERSION" ] && return 2
  return 3
}

# jm_db_lock_ddl TIMEOUT_SECS — flock -w T -E 75 on $JM_LOCK.
# The fd is held for the caller's lifetime in JM_LOCK_FD. Without util-linux
# flock (macOS) perl takes the same flock(2) lock on the inherited fd; the lock
# belongs to the shared open file description, so it outlives perl just as it
# outlives flock(1).
# rc 0 acquired · 75 held by another process · 1 error.
# USED BY `migrate` AND `upgrade` ONLY. Never on the capture path: a lockfile on
# the hot path is a stale-lock failure mode that could BLOCK a session.
jm_db_lock_ddl() {
  local secs="${1:-30}"
  case "$secs" in '' | *[!0-9]*) secs=30 ;; esac
  jm_have flock || jm_have perl || return 1
  jm_init_store || return 1
  : >>"$JM_LOCK" 2>/dev/null || return 1
  chmod 0600 "$JM_LOCK" 2>/dev/null || true

  exec {JM_LOCK_FD}>>"$JM_LOCK" || return 1
  local rc
  if jm_have flock; then
    flock -w "$secs" -E 75 "$JM_LOCK_FD" && rc=0 || rc=$?
  else
    perl -MFcntl=:flock -e '
      open(my $fh, ">>&=", $ARGV[0]) or exit 1;
      my $end = time + $ARGV[1];
      until (flock($fh, LOCK_EX | LOCK_NB)) {
        exit 75 if time >= $end;
        select(undef, undef, undef, 0.1);
      }
      exit 0;' "$JM_LOCK_FD" "$secs" && rc=0 || rc=$?
  fi
  if [ "$rc" -ne 0 ]; then
    exec {JM_LOCK_FD}>&- || true
    JM_LOCK_FD=''
    [ "$rc" -eq 75 ] && return 75
    return 1
  fi
  return 0
}

# jm_db_unlock_ddl — release the DDL lock early. rc 0 always.
jm_db_unlock_ddl() {
  if [ -n "${JM_LOCK_FD:-}" ]; then
    exec {JM_LOCK_FD}>&- 2>/dev/null || true
    JM_LOCK_FD=''
  fi
  return 0
}

# jm_db_upsert_ops EVENT OUTCOME [KEY=VAL ...]
# Appends to the in-DB ops_log AND trims it to the last 5000 rows, so the table
# is structurally incapable of unbounded growth. MIRRORS, never replaces, jm_log
# — the text log is authoritative because it works when sqlite3 is missing or
# the DB is corrupt.
# Recognised keys: reason ms bytes_in bytes_out rows redactions repo uid
# rc 0 · 1 failure.
jm_db_upsert_ops() {
  local event="${1:--}" outcome="${2:--}"
  shift 2 2>/dev/null || true
  local reason='' ms=0 bin=0 bout=0 rows=0 red=0
  local repo='' uid="${JM_SESSION_UID:-}"
  local kv k v
  for kv in "$@"; do
    k="${kv%%=*}"; v="${kv#*=}"
    case "$k" in
      reason) reason="$v" ;;
      ms) ms="$v" ;;
      bytes_in) bin="$v" ;;
      bytes_out) bout="$v" ;;
      rows | rows_written) rows="$v" ;;
      red | redactions) red="$v" ;;
      repo | repo_key) repo="$v" ;;
      uid | session_uid) uid="$v" ;;
      *) : ;;
    esac
  done
  for k in ms bin bout rows red; do
    v="${!k}"
    case "$v" in '' | *[!0-9]*) eval "$k=0" ;; esac
  done

  {
    printf 'BEGIN IMMEDIATE;\n'
    printf 'INSERT INTO ops_log(ts,event,outcome,session_uid,repo_key,reason,ms,bytes_in,bytes_out,rows_written,redactions) VALUES('
    printf "CAST(strftime('%%s','now') AS INTEGER),"
    printf '%s,%s,%s,%s,%s,%s,%s,%s,%s,%s);\n' \
      "$(jm_sql_quote "$event")" "$(jm_sql_quote "$outcome")" \
      "$(jm_sql_quote "$uid")" "$(jm_sql_quote "$repo")" "$(jm_sql_quote "$reason")" \
      "$ms" "$bin" "$bout" "$rows" "$red"
    printf 'DELETE FROM ops_log WHERE id <= (SELECT max(id) - 5000 FROM ops_log);\n'
    printf 'COMMIT;\n'
  } | jm_db_write
}

# jm_db_import_jsonl FILE TABLE — bulk .import into a temp-schema table.
# stdout: integer, number of rows imported.
# rc 0 and count > 0 · 1 and count 0 · 3 sqlite3 absent.
# MUST ASSERT count > 0: `.import` into a schema-qualified table name silently
# imports 0 rows and exits 0 (P76) — always `.import --schema temp FILE TABLE`,
# never `.import FILE temp.TABLE`.
jm_db_import_jsonl() {
  jm_require_sqlite || return 3
  local file="${1:-}" table="${2:-raw}"
  [ -n "$file" ] || return 2
  case "$table" in
    [A-Za-z_]*) : ;;
    *) return 2 ;;
  esac
  case "$table" in *[!A-Za-z0-9_]*) return 2 ;; esac
  [ -r "$file" ] || { printf '0\n'; return 1; }

  local qfile n
  qfile="$(jm_sql_quote "$file")"

  n="$({
    printf '.mode ascii\n'
    printf '.separator "\\x1F" "\\n"\n'
    printf 'CREATE TEMP TABLE %s(line TEXT);\n' "$table"
    printf '.import --schema temp %s %s\n' "$qfile" "$table"
    printf '.mode list\n'
    printf 'SELECT count(*) FROM temp.%s;\n' "$table"
  } | sqlite3 -bail -batch -noheader -cmd ".timeout $JM_DB_BUSY_READ_MS" ':memory:' 2>/dev/null)" || n=''
  case "$n" in '' | *[!0-9]*) n=0 ;; esac
  printf '%s\n' "$n"
  [ "$n" -gt 0 ] || return 1
  return 0
}
