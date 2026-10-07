#!/usr/bin/env bash
# jhh3-mem — lib/capture.sh
# Transcript -> one redacted `entries` row + `entry_file` rows.
# SPEC §6.3, §7.4, §7.5.
#
# WHAT THIS FILE IS NOT
#   - It is not an LLM call. Capture is deterministic, always (adjudication 1).
#     Prose enrichment is `jhh3-mem enrich`, out of band, never a hook.
#   - It is not a per-tool-call hook. It runs once, at SessionEnd, on a file that
#     already exists on disk.
#   - It never mutates, intercepts, or rewrites another tool's input or result.
#
# INVARIANTS ENFORCED HERE
#   - Redaction is FAIL CLOSED: a sed failure aborts the write. Losing one entry
#     beats persisting an unfiltered one.
#   - All expensive work (.import, JSON extraction, sed) happens BEFORE
#     BEGIN IMMEDIATE. The exclusive lock covers four small statements.
#   - Truncation happens AFTER redaction, so a cut can never split a secret and
#     defeat a pattern (SPEC §7.4 step 7, T-RED-7).
#   - Every write invocation carries -bail (via jm_db_write). The one non-write
#     sqlite3 invocation here (:memory: extraction) also carries -bail.
#   - Every .import uses `--schema temp` (P76 / T-INV-4).
#   - stdout is silent in hook mode. Only --dry-run and the CLI entry points
#     print, and neither is reachable from a hook.
#
# ADDITIVE GLOBALS (not in the frozen §6.3 signatures; set by the caller)
#   JM_CAPTURE_EVENT   ops_log/jm_log event name. Default 'session_end'.
#   JM_CAPTURE_MS      elapsed ms recorded in ops_log. Default 0.
#   JM_CAPTURE_DRY     1 = print the row, write nothing. Default 0.
# ADDITIVE OUTPUTS (set by jm_capture_extract / jm_capture_run)
#   JM_CAPTURE_REASON    machine reason token for the caller's log line
#   JM_CAPTURE_DEGRADED  '' or a degraded-reason token
#
# shellcheck shell=bash

if [ -n "${JM_CAPTURE_SOURCED:-}" ]; then
  # shellcheck disable=SC2317  # reached when the file is executed, not sourced
  return 0 2>/dev/null || exit 0
fi
JM_CAPTURE_SOURCED=1

if [ -z "${JM_DB_SOURCED:-}" ]; then
  _jm_cap_src="${BASH_SOURCE[0]}"
  case "$_jm_cap_src" in */*) _jm_cap_dir="${_jm_cap_src%/*}" ;; *) _jm_cap_dir="." ;; esac
  # shellcheck source=./db.sh
  . "$_jm_cap_dir/db.sh"
  unset _jm_cap_src _jm_cap_dir
fi

# ── tunables (all fixed by SPEC; env overrides exist only for the test suite) ──
JM_CAP_MAX_BYTES=67108864          # 64 MiB transcript guard (§6.3.2)
JM_CAP_MAX_FILES=40                # stored files_touched entries (§6.3.2)
JM_CAP_MAX_CMDS=30                 # stored command heads (§6.3.2)
JM_CAP_MAX_PROMPTS=12              # typed prompts folded into `request` (§6.3.2)
JM_CAP_TRIVIAL_OUTCOME=200         # skip rule threshold (§6.3.2)
JM_CAP_BACKFILL_MIN_AGE=600        # seconds; never touch a live sibling (§7.3)
readonly JM_CAP_MAX_BYTES JM_CAP_MAX_FILES JM_CAP_MAX_CMDS JM_CAP_MAX_PROMPTS
readonly JM_CAP_TRIVIAL_OUTCOME JM_CAP_BACKFILL_MIN_AGE

# Per-column CHECK limits from §4.2, in fields.tsv field order.
JM_CAP_LIM_TITLE=200
JM_CAP_LIM_REQUEST=2000
JM_CAP_LIM_OUTCOME=4000
JM_CAP_LIM_FILES=4000
JM_CAP_LIM_COMMANDS=600
readonly JM_CAP_LIM_TITLE JM_CAP_LIM_REQUEST JM_CAP_LIM_OUTCOME
readonly JM_CAP_LIM_FILES JM_CAP_LIM_COMMANDS

JM_CAPTURE_EVENT="${JM_CAPTURE_EVENT:-session_end}"
JM_CAPTURE_MS="${JM_CAPTURE_MS:-0}"
JM_CAPTURE_DRY="${JM_CAPTURE_DRY:-0}"
JM_CAPTURE_REASON=''
JM_CAPTURE_DEGRADED=''
JM_CAPTURE_TITLE_HINT="${JM_CAPTURE_TITLE_HINT:-}"
# Where jm_capture_write records WHY a 0-row write happened. Set per call by
# jm_capture_run; empty means "do not report".
JM_CAPTURE_WRITE_STATUS_FILE="${JM_CAPTURE_WRITE_STATUS_FILE:-}"

# ── internals ───────────────────────────────────────────────────────────────

# _jm_cap_size FILE — byte size, or 0.
_jm_cap_size() {
  local f="${1:-}" s=''
  [ -f "$f" ] || { printf '0\n'; return 0; }
  s="$(stat -c%s -- "$f" 2>/dev/null)" || s="$(stat -f%z -- "$f" 2>/dev/null)" || s=''
  case "$s" in '' | *[!0-9]*) printf '0\n' ;; *) printf '%s\n' "$s" ;; esac
}

# _jm_cap_mtime FILE — mtime epoch, or 0.
_jm_cap_mtime() {
  local f="${1:-}" s=''
  [ -e "$f" ] || { printf '0\n'; return 0; }
  s="$(stat -c%Y -- "$f" 2>/dev/null)" || s="$(stat -f%m -- "$f" 2>/dev/null)" || s=''
  case "$s" in '' | *[!0-9]*) printf '0\n' ;; *) printf '%s\n' "$s" ;; esac
}

# _jm_cap_int VALUE [DEFAULT] — VALUE if it is a non-negative integer, else DEFAULT.
_jm_cap_int() {
  local v="${1:-}" d="${2:-0}"
  case "$v" in '' | *[!0-9]*) printf '%s\n' "$d" ;; *) printf '%s\n' "$v" ;; esac
}

# _jm_cap_enc (filter) — encode a value for fields.tsv: CR dropped, TAB -> \t,
# LF -> \n. The inverse is applied in SQL by _jm_cap_dec_sql.
_jm_cap_enc() {
  LC_ALL=C awk 'BEGIN{RS="\n"; ORS=""} {gsub(/\r/,""); gsub(/\t/,"\\t"); if (NR>1) printf "\\n"; printf "%s", $0}'
}

# _jm_cap_us STRING — the string with TABs swapped for \x1F (US).
#
# TRAP, hit for real during development: TAB is an IFS *whitespace* character, so
# `IFS=$'\t' read -r a b c d` collapses runs of tabs into one delimiter and
# silently shifts every field after an empty one. `jm_repo_ident` on a non-repo
# emits `key\t\t\t/path` and the naive read assigned `/path` to `worktree`.
# \x1F is not IFS whitespace, so `read` preserves empty fields exactly.
# Nothing can smuggle a \x1F in: every text field is passed through jm_sanitize,
# which strips every byte below 0x20 except newline.
_jm_cap_us() { local s="${1:-}"; printf '%s' "${s//$'\t'/$'\x1f'}"; }

# _jm_cap_firstline FILE — the first line of FILE, TABs swapped for \x1F.
_jm_cap_firstline() {
  local l=''
  IFS= read -r l <"${1:-/dev/null}" || true
  _jm_cap_us "$l"
}

# _jm_cap_dec_sql SQL_EXPR — wrap an SQL expression in the fields.tsv decoder.
_jm_cap_dec_sql() {
  printf "replace(replace(%s,'\\\\t',char(9)),'\\\\n',char(10))" "$1"
}

# _jm_cap_fit ENCODED MAX — decode, byte-truncate to MAX, re-encode.
# The schema CHECK measures the DECODED value, so this is what actually fills the
# column budget without ever leaving half an escape sequence behind.
_jm_cap_fit() {
  local s="${1:-}" max="${2:-0}"
  [ -n "$s" ] || { printf ''; return 0; }
  s="${s//\\t/$'\t'}"
  s="${s//\\n/$'\n'}"
  printf '%s' "$s" | jm_truncate_bytes "$max" | _jm_cap_enc
}

# _jm_cap_ephemeral TITLE — 1 if the title matches any ERE in lib/ephemeral.ere.
# The .ere file is the single source of truth; it is never re-implemented in SQL.
_jm_cap_ephemeral() {
  local title="${1:-}"
  [ -r "$JM_EPHEMERAL_ERE" ] || { printf '0\n'; return 0; }
  if printf '%s\n' "$title" | LC_ALL=C grep -q -i -E -f "$JM_EPHEMERAL_ERE" 2>/dev/null; then
    printf '1\n'
  else
    printf '0\n'
  fi
}

# ── §6.3 contract ───────────────────────────────────────────────────────────

# jm_capture_redact IN_FILE OUT_FILE
# stdout: integer, number of redactions applied.
# rc 0 ok · 1 sed failed — FAIL CLOSED, the caller must abort the write · 2 misuse.
# One-line wrapper over jm_redact_file so the ruleset has exactly one implementation.
jm_capture_redact() { jm_redact_file "${1:-}" "${2:-}"; }

# jm_capture_durability KIND N_MOD N_READ EPHEMERAL
# stdout: durability to 2 decimal places (§5.3). rc 0 · 2 bad args.
# Integer arithmetic throughout: no bc, no awk, no locale-dependent decimal point.
jm_capture_durability() {
  local kind="${1:-}" nmod="${2:-}" nread="${3:-}" eph="${4:-}"
  [ -n "$kind" ] || return 2
  case "$nmod$nread" in *[!0-9]*) return 2 ;; esac
  case "$eph" in 0 | 1) : ;; *) return 2 ;; esac

  local prior
  case "$kind" in
    security) prior=180 ;;
    note) prior=160 ;;
    bugfix) prior=150 ;;
    refactor) prior=150 ;;
    decision) prior=140 ;;
    change) prior=130 ;;
    gotcha) prior=130 ;;
    session) prior=120 ;;
    other) prior=100 ;;
    feature) prior=90 ;;
    discovery) prior=85 ;;
    prompt) prior=50 ;;
    *) prior=100 ;;
  esac

  local ground
  if [ "$nmod" -gt 0 ]; then ground=135
  elif [ "$nread" -gt 0 ]; then ground=115
  else ground=70
  fi

  local pen=100
  [ "$eph" -eq 1 ] && pen=35

  # hundredths, rounded half-up: prior*ground*pen is at most 2_430_000.
  local h=$(((prior * ground * pen + 5000) / 10000))
  printf '%d.%02d\n' "$((h / 100))" "$((h % 100))"
}

# jm_capture_extract TRANSCRIPT_PATH STAGE_DIR
# Writes $STAGE_DIR/fields.tsv: one line, 18 TAB-separated fields (§6.3.1).
# Fields 3..6 (scope) and 14/15 (ts/end_reason) are placeholders here; jm_capture_run
# fills them from the hook payload. Field 18 (redactions) is filled after redaction.
# rc 0 ok · 1 transcript missing/unreadable/zero valid JSON lines · 2 bad args · 3 no sqlite3.
# Sets JM_CAPTURE_REASON and JM_CAPTURE_DEGRADED.
# _jm_cap_detect_host TRANSCRIPT — echoes "codex" or "claude-code". Internal.
#
# Content-sniffing, not env-sniffing. $CODEX_HOME is present in the hook environment
# and would work for the live path, but the backfill sweep runs over files with no
# host environment at all, and a Codex session launched from inside a Claude Code
# session inherits CLAUDECODE=1 (verified). The transcript's own first line is the
# only signal available on every path and it cannot be inherited or spoofed by nesting.
_jm_cap_detect_host() {
  local tp="${1:-}" first=''
  first="$(LC_ALL=C head -c 4096 -- "$tp" 2>/dev/null | LC_ALL=C head -n 1)"
  case "$first" in
    *'"session_meta"'*) printf 'codex\n' ;;
    *)                  printf 'claude-code\n' ;;
  esac
}

# _jm_cap_load_adapter TRANSCRIPT — sources the right adapter. rc 1 if absent.
_jm_cap_load_adapter() {
  local host lib
  host="$(_jm_cap_detect_host "${1:-}")"
  lib="${JM_LIB_DIR:-$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)}"
  JM_CAPTURE_HOST="$host"
  # shellcheck source=/dev/null
  [ -r "$lib/adapters/$host.sh" ] && . "$lib/adapters/$host.sh" || return 1
  # shellcheck source=/dev/null
  [ -r "$lib/adapters/_projection.sh" ] && . "$lib/adapters/_projection.sh" || return 1
  return 0
}

jm_capture_extract() {
  local tp="${1:-}" stage="${2:-}"
  [ -n "$tp" ] && [ -n "$stage" ] && [ -d "$stage" ] || return 2
  jm_require_sqlite || { JM_CAPTURE_REASON='no_sqlite3'; return 3; }

  JM_CAPTURE_REASON=''
  JM_CAPTURE_DEGRADED=''

  if [ ! -f "$tp" ] || [ ! -r "$tp" ]; then
    JM_CAPTURE_REASON='no_transcript'
    return 1
  fi

  local bytes
  bytes="$(_jm_cap_size "$tp")"
  if [ "$bytes" -eq 0 ]; then
    JM_CAPTURE_REASON='empty_transcript'
    return 1
  fi

  # Work from a stage-local path so the .import argument is always quote-free and
  # so the 64 MiB guard has somewhere to put its tail.
  local src="$stage/t.jsonl"
  if [ "$bytes" -gt "$JM_CAP_MAX_BYTES" ]; then
    # Line-boundary tail: drop the (necessarily partial) first line.
    if ! { LC_ALL=C tail -c "$JM_CAP_MAX_BYTES" -- "$tp" | LC_ALL=C tail -n +2 >"$src"; } 2>/dev/null; then
      JM_CAPTURE_REASON='tail_failed'
      return 1
    fi
    JM_CAPTURE_DEGRADED='transcript_too_large'
  else
    ln -s -- "$tp" "$src" 2>/dev/null || cp -- "$tp" "$src" 2>/dev/null || {
      JM_CAPTURE_REASON='stage_failed'
      return 1
    }
  fi

  local qsrc
  qsrc="$(jm_sql_quote "$src")"

  # One sqlite3 invocation: .import the transcript into a temp table, then extract
  # every transcript-derived field in a single statement. Measured at ~48 ms to
  # ingest 3.9 MB / 404 lines (P65).
  #
  # Every json_extract on a json_each element is guarded by `c.type='object'`
  # inside a CASE — an unguarded json_extract over a scalar element is a
  # malformed-JSON error, and with -bail that would abort the whole extraction.
  # Detect the host OUTSIDE the command substitution below: a variable set inside
  # $( ) dies with the subshell, and the caller needs the host to tag `producer`.
  JM_CAPTURE_HOST="$(_jm_cap_detect_host "$src")"

  local out=''
  local rc=0
  local errf="$stage/import.err"
  out="$(
    {
      printf 'CREATE TEMP TABLE raw(line TEXT);\n'
      printf '.mode ascii\n'
      printf '.separator "\\x1F" "\\n"\n'
      printf '.import --schema temp %s raw\n' "$qsrc"
      printf '.mode list\n'
      printf '.separator "\\t" "\\n"\n'
      printf '.headers off\n'
      printf '.nullvalue ""\n'
      # Host dispatch: the adapter supplies the CTE chain, the shared projection
      # supplies the 14-column contract. See lib/adapters/claude-code.sh.
      _jm_cap_load_adapter "$src" || { JM_CAPTURE_REASON="no_adapter"; return 1; }
      _jm_adapter_cte
      _jm_adapter_projection
    } | sqlite3 -bail -batch -noheader ':memory:' 2>"$errf"
  )" || rc=$?

  if [ "$rc" -ne 0 ] || [ -z "$out" ]; then
    JM_CAPTURE_REASON='extract_failed'
    return 1
  fi

  # The SELECT is a single row; guard against a stray .import warning line.
  local line
  line="$(printf '%s\n' "$out" | LC_ALL=C tail -n 1)"

  local a_title a_first a_request a_outcome a_files a_nmod a_nread a_cmds
  local a_branch a_cwd a_turns a_started a_valid a_total
  IFS=$'\x1f' read -r a_title a_first a_request a_outcome a_files a_nmod a_nread \
    a_cmds a_branch a_cwd a_turns a_started a_valid a_total <<<"$(_jm_cap_us "$line")"

  a_nmod="$(_jm_cap_int "${a_nmod:-0}")"
  a_nread="$(_jm_cap_int "${a_nread:-0}")"
  a_turns="$(_jm_cap_int "${a_turns:-0}")"
  a_started="$(_jm_cap_int "${a_started:-0}")"
  a_valid="$(_jm_cap_int "${a_valid:-0}")"
  a_total="$(_jm_cap_int "${a_total:-0}")"

  if [ "$a_total" -eq 0 ]; then
    JM_CAPTURE_REASON='import_empty'
    return 1
  fi
  if [ "$a_valid" -eq 0 ]; then
    JM_CAPTURE_REASON='no_valid_json'
    return 1
  fi
  if [ "$a_valid" -lt "$a_total" ] && [ -z "$JM_CAPTURE_DEGRADED" ]; then
    JM_CAPTURE_DEGRADED='partial_transcript'
  fi

  # Title fallback chain (§6.3.2): the transcript's own ai-title (LLM-written and
  # free) -> the hook payload's session_title -> the first typed prompt ->
  # '(untitled session)', supplied by jm_capture_run.
  # The chain resolves HERE, before redaction, so every branch of it is filtered.
  #
  # NO TRUNCATION ON THIS PATH.  A `cut -c1-120` used to live here, and it was a
  # credential leak: it ran BEFORE jm_capture_redact, so a token straddling byte
  # 120 was cut short, no longer matched its pattern, and was persisted — into
  # `title`, the single most-exposed field there is, the one the SessionStart
  # digest injects and `search` prints.  The length cap belongs where it already
  # exists: jm_capture_run applies JM_CAP_LIM_TITLE to the REDACTED text.
  # (The `cut -c` was also byte-based under LC_ALL=C and could split a UTF-8
  # sequence — the P81 hazard this codebase avoids by never using `head -c`.)
  local title="${a_title:-}"
  if [ -z "$title" ]; then title="${JM_CAPTURE_TITLE_HINT:-}"; fi
  if [ -z "$title" ] && [ -n "${a_first:-}" ]; then
    title="$a_first"
  fi

  # Sanitize every free-text field once, here, before anything else looks at it.
  title="$(printf '%s' "$title" | jm_sanitize | jm_flatten)"
  a_request="$(printf '%s' "${a_request:-}" | jm_sanitize | _jm_cap_enc)"
  a_outcome="$(printf '%s' "${a_outcome:-}" | jm_sanitize | _jm_cap_enc)"
  a_files="$(printf '%s' "${a_files:-}" | jm_sanitize | _jm_cap_enc)"
  a_cmds="$(printf '%s' "${a_cmds:-}" | jm_sanitize | jm_flatten)"
  a_branch="$(printf '%s' "${a_branch:-}" | jm_sanitize | jm_flatten)"
  a_cwd="$(printf '%s' "${a_cwd:-}" | jm_sanitize | jm_flatten)"

  # fields.tsv (§6.3.1): 18 fields. 3..6 and 14/15 are placeholders for run().
  {
    printf '%s\t' \
      '-' "$bytes" '-' '-' "$a_branch" "$a_cwd" \
      "$title" "$a_request" "$a_outcome" "$a_files" "$a_cmds" \
      "$a_turns" "$a_started" '0' '-' "$a_nmod" "$a_nread"
    printf '0\n'
  } >"$stage/fields.tsv" || {
    JM_CAPTURE_REASON='stage_write_failed'
    return 1
  }
  # fields.tsv holds PRE-redaction transcript text. It is deleted at the end of
  # the run, but a SIGKILL (hook timeout) can outlive that, so it is never left
  # readable by another uid even for the seconds it exists.
  chmod 0600 "$stage/fields.tsv" "$errf" 2>/dev/null || true
  return 0
}

# jm_capture_write FIELDS_TSV
# stdout: integer, rows written (0 or 1).
# rc 0 committed (including the idempotent 0-row case) · 5 busy · 26 corrupt · 1 other.
# Reads JM_CAPTURE_EVENT / JM_CAPTURE_MS for the ops_log row.
jm_capture_write() {
  local tsv="${1:-}"
  [ -n "$tsv" ] && [ -r "$tsv" ] || { printf '0\n'; return 1; }

  local uid bytes repo_key worktree branch repo_path title request outcome files
  local cmds turns started ts end_reason nmod nread red
  IFS=$'\x1f' read -r uid bytes repo_key worktree branch repo_path title request \
    outcome files cmds turns started ts end_reason nmod nread red \
    <<<"$(_jm_cap_firstline "$tsv")"

  [ -n "${uid:-}" ] && [ "$uid" != '-' ] || { printf '0\n'; return 1; }
  bytes="$(_jm_cap_int "${bytes:-0}")"
  turns="$(_jm_cap_int "${turns:-0}")"
  started="$(_jm_cap_int "${started:-0}")"
  ts="$(_jm_cap_int "${ts:-0}")"
  nmod="$(_jm_cap_int "${nmod:-0}")"
  nread="$(_jm_cap_int "${nread:-0}")"
  red="$(_jm_cap_int "${red:-0}")"
  [ "$ts" -gt 0 ] || ts="$(jm_now_epoch)"

  local eph dur
  eph="$(_jm_cap_ephemeral "$title")"
  dur="$(jm_capture_durability session "$nmod" "$nread" "$eph")" || dur='1.20'

  # NO `trap ... RETURN` HERE.  Bash keeps one RETURN trap per shell, not one
  # per frame: a nested function's trap REPLACES its caller's, so the outer
  # jm_capture_run's `rm -rf $stage` would never run and this one would run
  # twice.  Cleanup on this path is explicit, at every exit.
  local stage=''
  stage="$(jm_mkstage)" || { printf '0\n'; return 1; }

  local q_uid q_repo q_wt q_br q_rp q_title q_req q_out q_files q_cmds q_end q_prod
  q_uid="$(jm_sql_quote "$uid")"
  q_repo="$(jm_sql_quote "${repo_key:-}")"
  q_wt="$(jm_sql_quote "${worktree:-}")"
  q_br="$(jm_sql_quote "${branch:-}")"
  q_rp="$(jm_sql_quote "${repo_path:-}")"
  q_title="$(jm_sql_quote "${title:-}")"
  q_req="$(jm_sql_quote "${request:-}")"
  q_out="$(jm_sql_quote "${outcome:-}")"
  q_files="$(jm_sql_quote "${files:-}")"
  q_cmds="$(jm_sql_quote "${cmds:-}")"
  q_end="$(jm_sql_quote "${end_reason:-}")"
  q_prod="$(jm_sql_quote "$JM_PRODUCER")"
  local q_host
  q_host="$(jm_sql_quote "${JM_CAPTURE_HOST:-claude-code}")"

  local d_title d_req d_out d_files
  d_title="$(_jm_cap_dec_sql "$q_title")"
  d_req="$(_jm_cap_dec_sql "$q_req")"
  d_out="$(_jm_cap_dec_sql "$q_out")"
  d_files="$(_jm_cap_dec_sql "$q_files")"

  # entry_file lane: the decoded, deduplicated path list.
  local -a paths=()
  if [ -n "${files:-}" ]; then
    local decoded="${files//\\n/$'\n'}"
    local p
    while IFS= read -r p; do
      [ -n "$p" ] || continue
      paths+=("$p")
    done <<<"$decoded"
  fi

  local q_event q_reason ms
  q_event="$(jm_sql_quote "${JM_CAPTURE_EVENT:-session_end}")"
  q_reason="$(jm_sql_quote "${end_reason:-}")"
  ms="$(_jm_cap_int "${JM_CAPTURE_MS:-0}")"

  local rowfile="$stage/rows"

  {
    printf 'CREATE TEMP TABLE _cap(k TEXT PRIMARY KEY, v INTEGER);\n'
    printf 'BEGIN IMMEDIATE;\n'
    # `pre` is recorded BEFORE the insert so a 0-row result can be attributed
    # honestly. `INSERT OR IGNORE` swallows a CHECK violation exactly as it
    # swallows a uniqueness conflict — both yield changes()=0 — and reporting a
    # rejected row as `duplicate` would make real, silent data loss look like a
    # correct idempotent re-run.
    printf "INSERT INTO _cap(k,v) VALUES('pre',(SELECT EXISTS(SELECT 1 FROM entries WHERE uid=%s AND source_bytes=%s)));\n" \
      "$q_uid" "$bytes"
    printf 'INSERT OR IGNORE INTO entries('
    printf 'uid,source_bytes,origin,producer,host,repo_key,worktree,branch,repo_path,'
    printf 'kind,ts,started_at,title,request,outcome,files_touched,commands,'
    printf 'capture_mode,end_reason,turns,redactions,'
    printf 'durability,dedup_key,n_mod,n_read,ephemeral,injectable,superseded) VALUES('
    printf "%s,%s,'live',%s,%s,%s,%s,%s,%s," "$q_uid" "$bytes" "$q_prod" "$q_host" "$q_repo" "$q_wt" "$q_br" "$q_rp"
    printf "'session',%s,%s,%s,%s,%s,%s,%s," "$ts" "$started" "$d_title" "$d_req" "$d_out" "$d_files" "$q_cmds"
    printf "'deterministic',%s,%s,%s," "$q_end" "$turns" "$red"
    printf '%s,' "$dur"
    printf "substr(replace(replace(replace(replace(replace(lower(%s),' ',''),'-',''),'_',''),'.',''),':',''),1,48)," "$d_title"
    printf '%s,%s,%s,1,0);\n' "$nmod" "$nread" "$eph"
    printf "INSERT INTO _cap(k,v) VALUES('n',changes());\n"
    # Supersede every earlier capture of this session. (uid, source_bytes) is
    # unique, so `source_bytes <> N` can never touch the row just inserted —
    # which `id <> last_insert_rowid()` would, on the OR IGNORE no-op path where
    # last_insert_rowid() is 0.
    printf 'UPDATE entries SET superseded=1 WHERE uid=%s AND source_bytes<>%s AND superseded=0;\n' \
      "$q_uid" "$bytes"

    if [ "${#paths[@]}" -gt 0 ]; then
      local i='' qp='' qb='' base=''
      printf 'INSERT OR IGNORE INTO path(p,base) VALUES'
      for i in "${!paths[@]}"; do
        base="${paths[$i]##*/}"
        qp="$(jm_sql_quote "${paths[$i]}")"
        qb="$(jm_sql_quote "$base")"
        [ "$i" -eq 0 ] || printf ','
        printf '(%s,%s)' "$qp" "$qb"
      done
      printf ';\n'
      printf 'INSERT OR IGNORE INTO entry_file(path_id,entry_id,mode) '
      printf 'SELECT pa.id, e.id, 2 FROM path pa, '
      printf '(SELECT id FROM entries WHERE uid=%s AND source_bytes=%s) e ' "$q_uid" "$bytes"
      printf 'WHERE pa.p IN ('
      for i in "${!paths[@]}"; do
        [ "$i" -eq 0 ] || printf ','
        printf '%s' "$(jm_sql_quote "${paths[$i]}")"
      done
      printf ');\n'
    fi

    printf 'INSERT INTO ops_log(ts,event,outcome,session_uid,repo_key,reason,ms,bytes_in,bytes_out,rows_written,redactions) '
    printf "SELECT %s,%s,CASE WHEN c.n=1 THEN 'ok' ELSE 'noop' END,%s,%s," "$ts" "$q_event" "$q_uid" "$q_repo"
    printf "CASE WHEN c.n=1 THEN %s WHEN c.pre=1 THEN 'duplicate' ELSE 'check_violation' END," "$q_reason"
    printf '%s,%s,0,c.n,%s FROM (SELECT ' "$ms" "$bytes" "$red"
    printf "max(CASE WHEN k='n' THEN v END) AS n, max(CASE WHEN k='pre' THEN v END) AS pre FROM _cap) c;\n"
    printf 'DELETE FROM ops_log WHERE id <= (SELECT max(id) - 5000 FROM ops_log);\n'
    printf 'COMMIT;\n'
    printf ".once %s\n" "$(jm_sql_quote "$rowfile")"
    printf "SELECT (SELECT v FROM _cap WHERE k='n') || ' ' || (SELECT v FROM _cap WHERE k='pre');\n"
  } | jm_db_write
  local wrc=$?

  if [ "$wrc" -ne 0 ]; then
    rm -rf -- "$stage"
    printf '0\n'
    return "$wrc"
  fi

  local n=0 pre=0
  if [ -r "$rowfile" ]; then
    local pair
    IFS= read -r pair <"$rowfile" || pair=''
    n="$(_jm_cap_int "${pair%% *}" 0)"
    pre="$(_jm_cap_int "${pair##* }" 0)"
  fi
  [ "$n" -le 1 ] || n=1
  # A 0-row write is either idempotency doing its job or a rejected row. The
  # caller learns which through this file rather than through stdout, whose
  # contract (a bare integer) other callers depend on.
  if [ -n "${JM_CAPTURE_WRITE_STATUS_FILE:-}" ]; then
    if [ "$n" = '1' ]; then
      printf 'inserted\n' >"$JM_CAPTURE_WRITE_STATUS_FILE" 2>/dev/null || true
    elif [ "$pre" = '1' ]; then
      printf 'duplicate\n' >"$JM_CAPTURE_WRITE_STATUS_FILE" 2>/dev/null || true
    else
      printf 'check_violation\n' >"$JM_CAPTURE_WRITE_STATUS_FILE" 2>/dev/null || true
    fi
  fi
  rm -rf -- "$stage"
  printf '%s\n' "$n"
  return 0
}

# jm_capture_run TRANSCRIPT_PATH SESSION_ID CWD END_REASON [DRY_RUN]
# The full pipeline; the only function session-end.sh calls.
# stdout: nothing (DRY_RUN=1 prints the assembled row and writes nothing).
# rc 0 ok/noop · 1 failure (already logged).
jm_capture_run() {
  local tp="${1:-}" sid="${2:-}" cwd="${3:-}" reason="${4:-other}"
  local dry="${5:-${JM_CAPTURE_DRY:-0}}"
  local event="${JM_CAPTURE_EVENT:-session_end}"
  local t0
  t0="$(jm_now_ns)"

  # uid: the session_id verbatim (§4.3). Backfill has no payload, so the
  # transcript's own basename — which IS the session id in Claude Code's layout —
  # is the documented fallback.
  local uid="$sid"
  if [ -z "$uid" ]; then
    uid="${tp##*/}"
    uid="${uid%.jsonl}"
  fi
  # The uid is an identifier, not content: strip control bytes and separators and
  # cap it, so it can never break the TSV or the log line.
  uid="$(printf '%s' "$uid" | jm_sanitize | jm_flatten)"
  uid="${uid:0:64}"
  JM_SESSION_UID="$uid"

  if ! jm_require_sqlite; then
    jm_log "$event" degraded "reason=no_sqlite3"
    return 1
  fi
  if ! jm_init_store; then
    jm_log "$event" error "reason=readonly_home"
    return 1
  fi

  # Schema gate. 3 = DB ahead of the code: do nothing at all.
  # A dry run never bootstraps: `jhh3-mem capture --dry-run` on a machine with no
  # store must not be the thing that creates one.
  local grc=0
  [ "$dry" = '1' ] || jm_db_gate || grc=$?
  case "$grc" in
    3)
      jm_log "$event" degraded "reason=schema_ahead have=$(jm_db_schema_version) want=$JM_SCHEMA_VERSION"
      return 0
      ;;
    1)
      # gate rc 1 is three different worlds and they must not be conflated:
      #   (a) no DB file at all      -> first run: bootstrap. This is the ONLY
      #                                 hook permitted to (§7.4 step 4), and it
      #                                 is a create, not a DDL migration.
      #   (b) a DB that will not open -> corrupt. Never auto-repair, never delete.
      #   (c) a DB that opens but has no `entries` -> someone else's file, or a
      #       half-finished bootstrap: apply the idempotent DDL.
      #   (d) a DB that opens and HAS `entries` but reports schema_version 0 ->
      #       behind the code. Hooks never run DDL to fix that.
      if jm_db_present; then
        local probe='' prc2=0
        probe="$(jm_db_read "SELECT count(*) FROM sqlite_master WHERE type='table' AND name='entries';")" || prc2=$?
        if [ "$prc2" -ne 0 ]; then
          # An unreadable file is not a corrupt one. Telling the user their
          # memory store is destroyed when the real cause is a mode or
          # ownership accident is the wrong signal, and it is the one case
          # where the honest answer is "fix the permissions, your data is fine".
          if _jm_db_perm_suspect || _jm_db_err_is_perm "$JM_DB_LAST_ERR"; then
            jm_log "$event" error "rc=14 reason=db_permission_denied mode=$(_jm_db_mode_owner)"
          else
            jm_log "$event" error "rc=26 reason=sqlite_not_a_database"
          fi
          return 1
        fi
        if [ "${probe:-0}" != '0' ]; then
          jm_log "$event" degraded "reason=schema_behind have=$(jm_db_schema_version) want=$JM_SCHEMA_VERSION"
          return 0
        fi
      fi
      # First-run bootstrap is the one racy moment in the design: N sessions
      # ending together (machine shutdown, /clear sweep, multiplexer teardown)
      # all find no schema and all run the DDL. Serialize it with the DDL lock
      # when flock is available — bounded at 2 s, off the warm path, and inside
      # the 8 s hook budget — and, whatever happened, re-check the gate before
      # giving up: a sibling that won the race has already created the schema we
      # need, and reporting `bootstrap_failed` there silently loses a capture.
      local brc=0 have_lock=0
      if jm_have flock && jm_db_lock_ddl 2 >/dev/null 2>&1; then
        have_lock=1
      fi
      if jm_db_gate; then
        brc=0
      else
        jm_db_bootstrap || brc=$?
      fi
      if [ "$have_lock" = '1' ]; then jm_db_unlock_ddl; fi
      if [ "$brc" -ne 0 ] && jm_db_gate; then
        brc=0
      fi
      if [ "$brc" -ne 0 ]; then
        # One jittered retry, for the no-flock host where the loser of the race
        # can still see a half-applied schema.
        sleep "0.$((RANDOM % 4 + 1))" 2>/dev/null || true
        brc=0
        jm_db_gate || jm_db_bootstrap || brc=$?
      fi
      if [ "$brc" -ne 0 ]; then
        jm_log "$event" error "reason=bootstrap_failed rc=$brc"
        return 1
      fi
      ;;
    2)
      # DB behind the code. SessionEnd never runs DDL and never speaks up.
      jm_log "$event" degraded "reason=schema_behind have=$(jm_db_schema_version) want=$JM_SCHEMA_VERSION"
      return 0
      ;;
    *) : ;;
  esac

  local stage=''
  stage="$(jm_mkstage)" || {
    jm_log "$event" error "reason=no_stage"
    return 1
  }
  # shellcheck disable=SC2064  # $stage must expand now, not at trap time
  trap "rm -rf -- '$stage'" RETURN

  local erc=0
  jm_capture_extract "$tp" "$stage" || erc=$?
  if [ "$erc" -ne 0 ]; then
    jm_log "$event" error "reason=${JM_CAPTURE_REASON:-extract_failed} ms=$(jm_elapsed_ms "$t0")"
    return 1
  fi
  [ -z "$JM_CAPTURE_DEGRADED" ] || \
    jm_log "$event" degraded "reason=$JM_CAPTURE_DEGRADED bytes_in=$(_jm_cap_size "$tp")"

  # ── redact BEFORE truncating, so a cut can never split a secret (§7.4.7) ──
  local red=0 rrc=0
  red="$(jm_capture_redact "$stage/fields.tsv" "$stage/fields.red.tsv")" || rrc=$?
  if [ "$rrc" -ne 0 ]; then
    jm_log "$event" error "reason=redact_failed ms=$(jm_elapsed_ms "$t0")"
    return 1
  fi
  red="$(_jm_cap_int "$red" 0)"

  # ── assemble: scope + timing + per-column truncation ──
  local f_uid f_bytes f_rk f_wt f_branch f_rp f_title f_req f_out f_files
  local f_cmds f_turns f_started f_ts f_end f_nmod f_nread f_red
  IFS=$'\x1f' read -r f_uid f_bytes f_rk f_wt f_branch f_rp f_title f_req f_out \
    f_files f_cmds f_turns f_started f_ts f_end f_nmod f_nread f_red \
    <<<"$(_jm_cap_firstline "$stage/fields.red.tsv")"
  # Fields the writer recomputes rather than trusting from the staged row.
  : "$f_uid" "$f_rk" "$f_wt" "$f_ts" "$f_end" "$f_red"

  # Effective cwd: the payload's, else the transcript's, else $PWD. A backfilled
  # transcript may name a directory that no longer exists.
  local eff="$cwd"
  [ -n "$eff" ] && [ -d "$eff" ] || eff="${f_rp:-}"
  [ -n "$eff" ] && [ -d "$eff" ] || eff="$PWD"

  local ident rk wt br rp
  ident="$(jm_repo_ident "$eff")"
  IFS=$'\x1f' read -r rk wt br rp <<<"$(_jm_cap_us "$ident")"
  # jm_repo_ident wins where it has an answer; the transcript fills the gaps.
  [ -n "$br" ] || br="${f_branch:-}"
  [ -n "$rp" ] || rp="${f_rp:-}"
  rk="$(printf '%s' "$rk" | jm_sanitize | jm_flatten)"
  wt="$(printf '%s' "$wt" | jm_sanitize | jm_flatten)"
  br="$(printf '%s' "$br" | jm_sanitize | jm_flatten)"
  rp="$(printf '%s' "$rp" | jm_sanitize | jm_flatten)"

  # Paths recorded by the transcript are absolute; store them repo-relative so
  # the file lane bridges worktrees.
  local rel_files='' p
  if [ -n "${f_files:-}" ]; then
    local decoded="${f_files//\\n/$'\n'}"
    while IFS= read -r p; do
      [ -n "$p" ] || continue
      p="$(jm_rel_path "$rp" "$p")"
      [ -n "$p" ] || continue
      rel_files="${rel_files}${p}"$'\n'
    done <<<"$decoded"
    rel_files="$(printf '%s' "$rel_files" | LC_ALL=C awk 'NF && !seen[$0]++' | _jm_cap_enc)"
  fi

  local title="${f_title:-}"
  [ -n "$title" ] || title='(untitled session)'

  # Truncation runs on the DECODED text and the result is re-encoded. The schema
  # CHECKs measure the decoded value, so truncating the encoded form would both
  # undershoot the budget (every \n costs 2) and risk leaving a dangling escape
  # for the writer's replace() to misread.
  f_title="$(printf '%s' "$title" | jm_truncate_bytes "$JM_CAP_LIM_TITLE" | jm_flatten)"
  f_req="$(_jm_cap_fit "${f_req:-}" "$JM_CAP_LIM_REQUEST")"
  f_out="$(_jm_cap_fit "${f_out:-}" "$JM_CAP_LIM_OUTCOME")"
  rel_files="$(_jm_cap_fit "$rel_files" "$JM_CAP_LIM_FILES")"
  f_cmds="$(printf '%s' "${f_cmds:-}" | jm_truncate_bytes "$JM_CAP_LIM_COMMANDS" | jm_flatten)"

  local end_reason="$reason"
  [ -n "$end_reason" ] || end_reason='other'
  case "$JM_CAPTURE_DEGRADED" in
    transcript_too_large) end_reason="${end_reason}+truncated" ;;
    *) : ;;
  esac

  f_nmod="$(_jm_cap_int "${f_nmod:-0}")"
  f_nread="$(_jm_cap_int "${f_nread:-0}")"

  # ── skip rule (§6.3.2) ──
  local out_len=${#f_out}
  if [ "$f_nmod" -eq 0 ] && [ "$out_len" -lt "$JM_CAP_TRIVIAL_OUTCOME" ] && [ -z "${f_req:-}" ]; then
    jm_log "$event" noop "reason=trivial_session ms=$(jm_elapsed_ms "$t0") repo=$rk"
    return 0
  fi

  {
    printf '%s\t' \
      "$uid" "$(_jm_cap_int "${f_bytes:-0}")" "$rk" "$wt" "$br" "$rp" \
      "$f_title" "$f_req" "$f_out" "$rel_files" "$f_cmds" \
      "$(_jm_cap_int "${f_turns:-0}")" "$(_jm_cap_int "${f_started:-0}")" \
      "$(jm_now_epoch)" "$end_reason" "$f_nmod" "$f_nread"
    printf '%s\n' "$red"
  } >"$stage/final.tsv"
  chmod 0600 "$stage/final.tsv" 2>/dev/null || true

  if [ "$dry" = '1' ]; then
    _jm_cap_print_row "$stage/final.tsv"
    jm_log "$event" noop "reason=dry_run ms=$(jm_elapsed_ms "$t0") repo=$rk"
    return 0
  fi

  JM_CAPTURE_MS="$(jm_elapsed_ms "$t0")"
  local rows=0 wrc=0
  JM_CAPTURE_WRITE_STATUS_FILE="$stage/write.status"
  rows="$(jm_capture_write "$stage/final.tsv")" || wrc=$?
  rows="$(_jm_cap_int "$rows" 0)"

  case "$wrc" in
    0) : ;;
    5)
      jm_log "$event" busy "rc=5 ms=$(jm_elapsed_ms "$t0") repo=$rk"
      return 1
      ;;
    14)
      jm_log "$event" error \
        "rc=14 reason=db_permission_denied mode=$(_jm_db_mode_owner) ms=$(jm_elapsed_ms "$t0")"
      return 1
      ;;
    26)
      jm_log "$event" error "rc=26 reason=sqlite_not_a_database ms=$(jm_elapsed_ms "$t0")"
      return 1
      ;;
    *)
      jm_log "$event" error "rc=$wrc reason=write_failed ms=$(jm_elapsed_ms "$t0")"
      return 1
      ;;
  esac

  local wstatus='' outcome='ok'
  if [ -r "$stage/write.status" ]; then
    IFS= read -r wstatus <"$stage/write.status" || wstatus=''
  fi
  if [ "$rows" -eq 1 ]; then
    jm_log "$event" ok \
      "ms=$(jm_elapsed_ms "$t0") repo=$rk bytes_in=$(_jm_cap_int "${f_bytes:-0}") rows=1 red=$red"
    return 0
  fi
  outcome='noop'
  case "$wstatus" in
    check_violation)
      # The row was REJECTED, not deduplicated. That is data loss, and it must
      # never be logged as a successful idempotent re-run.
      jm_log "$event" error \
        "reason=check_violation ms=$(jm_elapsed_ms "$t0") repo=$rk bytes_in=$(_jm_cap_int "${f_bytes:-0}") rows=0 red=$red"
      return 1
      ;;
    *)
      jm_log "$event" "$outcome" \
        "reason=duplicate ms=$(jm_elapsed_ms "$t0") repo=$rk bytes_in=$(_jm_cap_int "${f_bytes:-0}") rows=0 red=$red"
      return 0
      ;;
  esac
}

# _jm_cap_print_row FIELDS_TSV — human-readable dump for --dry-run. Internal.
_jm_cap_print_row() {
  local tsv="${1:-}"
  local uid bytes rk wt br rp title req out files cmds turns started ts endr nmod nread red
  IFS=$'\x1f' read -r uid bytes rk wt br rp title req out files cmds turns \
    started ts endr nmod nread red <<<"$(_jm_cap_firstline "$tsv")"
  local eph dur
  eph="$(_jm_cap_ephemeral "$title")"
  dur="$(jm_capture_durability session "$(_jm_cap_int "$nmod")" "$(_jm_cap_int "$nread")" "$eph")"
  printf 'uid           %s\n' "$uid"
  printf 'source_bytes  %s\n' "$bytes"
  printf 'origin        live\n'
  printf 'kind          session\n'
  printf 'capture_mode  deterministic\n'
  printf 'repo_key      %s\n' "$rk"
  printf 'worktree      %s\n' "$wt"
  printf 'branch        %s\n' "$br"
  printf 'repo_path     %s\n' "$rp"
  printf 'ts            %s (%s)\n' "$ts" "$(jm_iso_local "$(_jm_cap_int "$ts")")"
  printf 'started_at    %s\n' "$started"
  printf 'end_reason    %s\n' "$endr"
  printf 'turns         %s\n' "$turns"
  printf 'n_mod/n_read  %s / %s\n' "$nmod" "$nread"
  printf 'redactions    %s\n' "$red"
  printf 'ephemeral     %s\n' "$eph"
  printf 'durability    %s\n' "$dur"
  printf 'title         %s\n' "$title"
  printf 'request       %s\n' "$req"
  printf 'outcome       %s\n' "$out"
  printf 'files_touched %s\n' "$files"
  printf 'commands      %s\n' "$cmds"
}

# jm_capture_backfill TRANSCRIPT_DIR MAX_N DEADLINE_EPOCH
# stdout: integer, rows written. rc 0 ALWAYS (best-effort by contract).
# Never touches a transcript younger than 600 s: that is a live sibling session.
jm_capture_backfill() {
  local dir="${1:-}" maxn="${2:-3}" deadline="${3:-0}"
  local written=0

  if [ "${JHH3_MEM_BACKFILL:-1}" = '0' ]; then
    printf '0\n'
    return 0
  fi
  maxn="$(_jm_cap_int "$maxn" 3)"
  deadline="$(_jm_cap_int "$deadline" 0)"
  if [ -z "$dir" ] || [ ! -d "$dir" ] || [ "$maxn" -eq 0 ]; then
    printf '0\n'
    return 0
  fi
  # No store yet means nothing to backfill INTO. SessionEnd is the only hook
  # permitted to bootstrap (§7.4 step 4), and the sweep runs from SessionStart —
  # so an absent DB is a silent no-op, never a create.
  if ! jm_require_sqlite || ! jm_db_present; then
    printf '0\n'
    return 0
  fi

  local now cutoff
  now="$(jm_now_epoch)"
  cutoff=$((now - JM_CAP_BACKFILL_MIN_AGE))

  # Newest-first, bounded: never stat more than 24 candidates however large the
  # project directory has grown.
  local -a cands=()
  local f=''
  while IFS= read -r f; do
    [ -n "$f" ] || continue
    cands+=("$f")
    [ "${#cands[@]}" -lt 24 ] || break
  done < <(LC_ALL=C ls -1t -- "$dir"/*.jsonl 2>/dev/null || true)

  local saved_event="${JM_CAPTURE_EVENT:-session_end}"
  local saved_uid="${JM_SESSION_UID:-}"
  JM_CAPTURE_EVENT='backfill'
  # A backfilled transcript has no hook payload, so no session_title hint.
  JM_CAPTURE_TITLE_HINT=''

  local uid bytes mtime hit rc
  for f in "${cands[@]}"; do
    [ "$written" -lt "$maxn" ] || break
    if [ "$deadline" -gt 0 ] && [ "$(jm_now_epoch)" -ge "$deadline" ]; then break; fi
    [ -f "$f" ] || continue

    mtime="$(_jm_cap_mtime "$f")"
    [ "$mtime" -le "$cutoff" ] || continue
    bytes="$(_jm_cap_size "$f")"
    [ "$bytes" -gt 0 ] || continue
    uid="${f##*/}"
    uid="${uid%.jsonl}"

    hit=''
    hit="$(jm_db_param_read \
      'SELECT 1 FROM entries WHERE uid = :uid AND source_bytes = CAST(:bytes AS INTEGER) LIMIT 1;' \
      uid "$uid" bytes "$bytes" 2>/dev/null)" || hit=''
    [ -z "$hit" ] || continue

    rc=0
    jm_capture_run "$f" "$uid" '' 'backfill' 0 || rc=$?
    [ "$rc" -eq 0 ] && written=$((written + 1))
  done

  JM_CAPTURE_EVENT="$saved_event"
  JM_SESSION_UID="$saved_uid"
  printf '%s\n' "$written"
  return 0
}

# ── entry points ────────────────────────────────────────────────────────────

_jm_capture_usage() {
  cat <<'USAGE'
capture.sh — jhh3-mem session capture (deterministic, no network, no LLM).

  capture.sh PAYLOAD_FILE                      hook worker: read a SessionEnd payload
  capture.sh --transcript P [--session S] [--cwd D] [--reason R] [--dry-run]
  capture.sh --backfill DIR [--max N] [--seconds S]
USAGE
}

# jm_capture_hook PAYLOAD_FILE — the SessionEnd worker. rc 0 ok/noop · 1 failure.
jm_capture_hook() {
  local pf="${1:-}"
  [ -n "$pf" ] && [ -r "$pf" ] || {
    jm_log session_end error "reason=no_payload"
    return 1
  }

  local parsed prc=0
  parsed="$(jm_payload_parse "$pf")" || prc=$?
  if [ "$prc" -eq 3 ]; then
    # The payload parser needs sqlite3 too, so a missing sqlite3 arrives here
    # first. Report the real cause, not "bad payload".
    jm_log session_end degraded "reason=no_sqlite3"
    return 1
  fi
  if [ "$prc" -ne 0 ] || [ -z "$parsed" ]; then
    jm_log session_end error "reason=bad_payload"
    return 1
  fi

  local sid tpath cwd hev src reason stitle
  IFS=$'\x1f' read -r sid tpath cwd hev src reason stitle <<<"$(_jm_cap_us "$parsed")"
  : "$hev" "$src"

  # The payload's session_title is the second link in the title fallback chain
  # (§6.3.2). It is agent/user-authored text on the untrusted side of the ingress,
  # so it is sanitized here, exactly like transcript text.
  if [ -n "${stitle:-}" ]; then
    JM_CAPTURE_TITLE_HINT="$(printf '%s' "$stitle" | jm_sanitize | jm_flatten)"
  else
    JM_CAPTURE_TITLE_HINT=''
  fi

  if [ -z "$tpath" ]; then
    JM_SESSION_UID="$sid"
    jm_log session_end error "reason=no_transcript"
    return 1
  fi

  JM_CAPTURE_EVENT='session_end'
  jm_capture_run "$tpath" "$sid" "$cwd" "${reason:-other}" 0
}

jm_capture_main() {
  local mode='' tp='' sid='' cwd='' reason='other' dry=0
  local dir='' maxn=3 secs=3

  if [ $# -eq 0 ]; then
    _jm_capture_usage >&2
    return 2
  fi

  case "${1:-}" in
    -h | --help)
      _jm_capture_usage
      return 0
      ;;
    --transcript | --backfill) : ;;
    -*)
      printf 'capture: unknown argument: %s\n' "$1" >&2
      _jm_capture_usage >&2
      return 2
      ;;
    *)
      mode='hook'
      jm_capture_hook "$1"
      return $?
      ;;
  esac

  while [ $# -gt 0 ]; do
    case "$1" in
      --transcript) mode='one'; tp="${2:-}"; shift 2 ;;
      --session) sid="${2:-}"; shift 2 ;;
      --cwd) cwd="${2:-}"; shift 2 ;;
      --reason) reason="${2:-other}"; shift 2 ;;
      --dry-run) dry=1; shift ;;
      --backfill) mode='backfill'; dir="${2:-}"; shift 2 ;;
      --max) maxn="${2:-3}"; shift 2 ;;
      --seconds) secs="${2:-3}"; shift 2 ;;
      -h | --help) _jm_capture_usage; return 0 ;;
      *)
        printf 'capture: unknown argument: %s\n' "$1" >&2
        _jm_capture_usage >&2
        return 2
        ;;
    esac
  done

  case "$mode" in
    one)
      [ -n "$tp" ] || { printf 'capture: --transcript requires a path\n' >&2; return 2; }
      [ -n "$cwd" ] || cwd="$PWD"
      JM_CAPTURE_EVENT="${JM_CAPTURE_EVENT:-session_end}"
      jm_capture_run "$tp" "$sid" "$cwd" "$reason" "$dry"
      ;;
    backfill)
      [ -n "$dir" ] || { printf 'capture: --backfill requires a directory\n' >&2; return 2; }
      secs="$(_jm_cap_int "$secs" 3)"
      jm_capture_backfill "$dir" "$maxn" "$(($(jm_now_epoch) + secs))"
      ;;
    *)
      _jm_capture_usage >&2
      return 2
      ;;
  esac
}

if [ "${BASH_SOURCE[0]}" = "${0}" ]; then
  set -euo pipefail
  jm_capture_main "$@"
fi
