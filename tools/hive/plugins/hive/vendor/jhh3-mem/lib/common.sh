#!/usr/bin/env bash
# jhh3-mem — lib/common.sh
# Paths, logging, payload parsing, repo identity, sanitizers, redaction,
# byte-truncation. SPEC §6.1.
#
# CONTRACT
#   - Sourceable with `set -u` already active. Every JM_* constant is defined
#     before any function body runs.
#   - Sourceable more than once (idempotent; readonly re-assignment is guarded).
#   - Data goes to stdout; nothing else ever does. Diagnostics go through jm_log.
#   - rc 0 success · 1 expected failure the caller handles · 2 misuse · 3 a
#     required dependency is unavailable.
#   - No network client, no daemon, no background process, no listening socket.
#
# shellcheck shell=bash

if [ -n "${JM_COMMON_SOURCED:-}" ]; then
  # shellcheck disable=SC2317  # reached when the file is executed, not sourced
  return 0 2>/dev/null || exit 0
fi
JM_COMMON_SOURCED=1

# ── constants ───────────────────────────────────────────────────────────────

JM_VERSION="0.4.5"
JM_SCHEMA_VERSION=2
JM_PRODUCER="jhh3-mem/${JM_VERSION}"

# Store location (SPEC adjudication 6). The plugin-data dir Claude Code manages
# is deliberately NOT used and its variable is never named anywhere in this tree:
# the harness rm -rf's that directory on prune/uninstall (P70) and merely naming
# it triggers a mkdir (P69). Losing months of memory to `/plugin update` is worse
# than any convenience. This is CI invariant T-INV-2.
JM_HOME="${JHH3_MEM_HOME:-${XDG_DATA_HOME:-$HOME/.local/share}/jhh3-mem}"
while [ "$JM_HOME" != "/" ] && [ "${JM_HOME%/}" != "$JM_HOME" ]; do
  JM_HOME="${JM_HOME%/}"
done

JM_DB="$JM_HOME/memory.db"
JM_LOG="$JM_HOME/jhh3-mem.log"
JM_LOCK="$JM_HOME/.lock"
JM_BACKUPS="$JM_HOME/backups"
# Semantic search is OPTIONAL and lives entirely inside the store: the loadable
# extension is fetched here, so `paths` lists it and `uninstall --purge` removes it
# like everything else. The core store never depends on it -- bootstrap, capture,
# injection and lexical search all work with this file absent.
JM_VEC_DIR="$JM_HOME/lib"
JM_VEC_SO="$JM_VEC_DIR/vec0"
JM_EMBED_DIM="${JHH3_MEM_EMBED_DIM:-1536}"
JM_EMBED_MODEL="${JHH3_MEM_EMBED_MODEL:-text-embedding-3-small}"
JM_LOG_MAX_BYTES=2097152

# Injection cap: env override, clamped to [400, 4000] (SPEC adjudication 4).
JM_INJECT_MAX="${JHH3_MEM_INJECT_MAX_BYTES:-2400}"
case "$JM_INJECT_MAX" in
  '' | *[!0-9]*) JM_INJECT_MAX=2400 ;;
esac
[ "$JM_INJECT_MAX" -lt 400 ] && JM_INJECT_MAX=400
[ "$JM_INJECT_MAX" -gt 4000 ] && JM_INJECT_MAX=4000

# Plugin-tree locations, derived from this file so they survive the install copy
# into ~/.claude/plugins/cache. Never uses ${CLAUDE_PLUGIN_ROOT} at import time
# so the library also works when sourced directly from a checkout.
_jm_src="${BASH_SOURCE[0]}"
case "$_jm_src" in */*) _jm_src_dir="${_jm_src%/*}" ;; *) _jm_src_dir="." ;; esac
JM_LIB_DIR="$(cd -- "$_jm_src_dir" && pwd -P)"
unset _jm_src _jm_src_dir
JM_PLUGIN_ROOT="$(cd -- "$JM_LIB_DIR/.." && pwd -P)"
JM_SCHEMA_SQL="$JM_PLUGIN_ROOT/db/schema.sql"
JM_MIGRATIONS_DIR="$JM_PLUGIN_ROOT/db/migrations"
JM_REDACT_SED="$JM_PLUGIN_ROOT/lib/redact.sed"
JM_EPHEMERAL_ERE="$JM_PLUGIN_ROOT/lib/ephemeral.ere"

# These are the shared contract for db.sh / capture.sh / migrate.sh and are
# referenced from those files, not from this one.
# shellcheck disable=SC2034
readonly JM_VERSION JM_SCHEMA_VERSION JM_PRODUCER
readonly JM_HOME JM_DB JM_LOG JM_LOCK JM_BACKUPS JM_LOG_MAX_BYTES JM_INJECT_MAX
# shellcheck disable=SC2034  # consumed by lib/db.sh (bootstrap) and the upgrade path
readonly JM_LIB_DIR JM_PLUGIN_ROOT JM_SCHEMA_SQL JM_MIGRATIONS_DIR
# shellcheck disable=SC2034  # consumed by lib/migrate.sh and lib/capture.sh
readonly JM_REDACT_SED JM_EPHEMERAL_ERE

# Mutable session context used by jm_log. Not readonly: hooks set it after
# parsing the payload. Never contains user content.
JM_SESSION_UID="${JM_SESSION_UID:-}"

# ── store ───────────────────────────────────────────────────────────────────

# jm_home — absolute store dir, no trailing slash. rc 0 always.
jm_home() { printf '%s\n' "$JM_HOME"; }

# jm_init_store — create the store dir at 0700. rc 0 ok · 1 could not create/chmod.
jm_init_store() {
  if [ ! -d "$JM_HOME" ]; then
    mkdir -p -- "$JM_HOME" 2>/dev/null || return 1
  fi
  chmod 0700 "$JM_HOME" 2>/dev/null || return 1
  [ -w "$JM_HOME" ] || return 1
  return 0
}

# ── time ────────────────────────────────────────────────────────────────────

jm_now_epoch() { date +%s; }

# jm_now_ns — nanoseconds since epoch. Falls back to seconds×1e9 if %N is unsupported.
jm_now_ns() {
  local n
  n="$(date +%s%N 2>/dev/null)" || n=''
  case "$n" in
    '' | *[!0-9]*) printf '%s000000000\n' "$(date +%s)" ;;
    *) printf '%s\n' "$n" ;;
  esac
}

# jm_elapsed_ms START_NS — integer milliseconds since START_NS.
jm_elapsed_ms() {
  local start="${1:-0}" now
  case "$start" in '' | *[!0-9]*) printf '0\n'; return 0 ;; esac
  now="$(jm_now_ns)"
  printf '%s\n' "$(( (now - start) / 1000000 ))"
}

# jm_iso_local EPOCH — YYYY-MM-DDTHH:MM:SS±HH:MM
jm_iso_local() {
  local e="${1:-}"
  case "$e" in '' | *[!0-9]*) e="$(date +%s)" ;; esac
  date -d "@$e" +%Y-%m-%dT%H:%M:%S%:z 2>/dev/null \
    || date -r "$e" +%Y-%m-%dT%H:%M:%S%z 2>/dev/null \
    || printf '1970-01-01T00:00:00+00:00\n'
}

# jm_short_date EPOCH — MM-DD
jm_short_date() {
  local e="${1:-}"
  case "$e" in '' | *[!0-9]*) e="$(date +%s)" ;; esac
  date -d "@$e" +%m-%d 2>/dev/null || date -r "$e" +%m-%d 2>/dev/null || printf '01-01\n'
}

# ── logging ─────────────────────────────────────────────────────────────────

# jm_log EVENT OUTCOME [KEY=VAL ...]
# Appends one TSV line: <ISO8601-local>\t<EVENT>\t<uid or ->\t<OUTCOME>\t<KEY=VAL ...>
# Rotates by a single `mv` past JM_LOG_MAX_BYTES. Never emits user content.
# rc 0 ALWAYS — including when the log is unwritable (one stderr line then).
jm_log() {
  local event="${1:--}" outcome="${2:--}"
  shift 2 2>/dev/null || true
  local uid="${JM_SESSION_UID:-}"
  [ -n "$uid" ] || uid='-'
  # Keep the uid short and free of separators; it is an id, not content.
  uid="${uid//[$'\t\n']/ }"
  uid="${uid:0:36}"

  local kv="$*"
  kv="${kv//[$'\t\n']/ }"

  local line
  line="$(jm_iso_local "$(date +%s)")"$'\t'"$event"$'\t'"$uid"$'\t'"$outcome"$'\t'"$kv"

  if [ ! -d "$JM_HOME" ]; then
    { mkdir -p -- "$JM_HOME" && chmod 0700 "$JM_HOME"; } 2>/dev/null || {
      printf '[jhh3-mem] log unwritable: %s\n' "$JM_LOG" >&2
      return 0
    }
  fi

  # Rotation: at most one rotated file, ever (SPEC §8.2).
  local sz
  sz="$(_jm_file_size "$JM_LOG")"
  if [ "$sz" -gt "$JM_LOG_MAX_BYTES" ] 2>/dev/null; then
    mv -f -- "$JM_LOG" "$JM_LOG.1" 2>/dev/null || true
  fi

  if printf '%s\n' "$line" >>"$JM_LOG" 2>/dev/null; then
    chmod 0600 "$JM_LOG" 2>/dev/null || true
  else
    printf '[jhh3-mem] log unwritable: %s\n' "$JM_LOG" >&2
  fi
  return 0
}

# _jm_file_size PATH — bytes, or 0. Internal.
_jm_file_size() {
  local f="${1:-}" s
  [ -f "$f" ] || { printf '0\n'; return 0; }
  s="$(stat -c%s -- "$f" 2>/dev/null)" || s="$(stat -f%z -- "$f" 2>/dev/null)" || s=''
  case "$s" in '' | *[!0-9]*) printf '0\n' ;; *) printf '%s\n' "$s" ;; esac
}

# ── dependencies ────────────────────────────────────────────────────────────

# jm_have CMD — rc 0 present · 1 absent.
jm_have() { command -v -- "${1:-}" >/dev/null 2>&1; }

# jm_require_sqlite — rc 0 present · 3 absent.
jm_require_sqlite() { jm_have sqlite3 && return 0; return 3; }

# sqlite3 >= 3.50 (macOS ships 3.51) escapes control characters in CLI output
# by default, printing char(31) as the two bytes "^_". Every multi-column read
# here splits on \x1F, so on such a CLI digests, search and enrichment silently
# lose their fields. Turn the escaping off where the flag exists; older CLIs
# reject the flag and never escaped. One ~3 ms probe per sourcing; the wrapper
# exists only when a real sqlite3 answered, so jm_have sqlite3 stays truthful.
if command sqlite3 -escape off :memory: 'SELECT 1;' >/dev/null 2>&1; then
  sqlite3() { command sqlite3 -escape off "$@"; }
fi

# jm_sha256_file PATH — lowercase hex digest, or rc 1. Prefers sha256sum, falls
# back to shasum (macOS ships the latter).
jm_sha256_file() {
  local f="${1:-}" out=''
  [ -r "$f" ] || return 1
  if jm_have sha256sum;   then out="$(sha256sum -- "$f" 2>/dev/null)"
  elif jm_have shasum;    then out="$(shasum -a 256 -- "$f" 2>/dev/null)"
  else return 1; fi
  [ -n "$out" ] || return 1
  printf '%s\n' "${out%% *}"
}

# jm_json_string TEXT — emit TEXT as a correctly quoted JSON string, on stdout.
#
# Via sqlite3's json_quote rather than shell escaping: a session title can contain
# quotes, backslashes, newlines and control bytes, and hand-rolled escaping of
# arbitrary captured text into a request body is exactly the sort of thing that
# works until the day it silently produces a malformed request.
jm_json_string() {
  jm_require_sqlite || return 3
  sqlite3 -bail -batch -noheader \
    -cmd ".parameter set :t cast(x'$(printf '%s' "${1:-}" | od -An -v -tx1 | tr -d ' \n')'as'text')" \
    ':memory:' 'SELECT json_quote(:t);' 2>/dev/null
}

# jm_db_read_mem SQL — run SQL against an in-memory database, no store involved.
# For pure computation (JSON parsing, quoting) that must not touch or lock the store.
jm_db_read_mem() {
  jm_require_sqlite || return 3
  printf '%s\n' "${1:-}" | sqlite3 -bail -batch -noheader ':memory:' 2>/dev/null
}


# jm_current_host — which agent host is running us RIGHT NOW. For paths with no
# transcript to sniff (the CLI, `note`). Capture uses _jm_cap_detect_host instead,
# because it must classify a file that may have been written by the other host.
#
# CODEX_HOME is exported into the hook and tool environment by codex (verified by
# probe against 0.147.0). Claude Code does not set it. When Codex runs *inside* a
# Claude Code session both CLAUDECODE and CODEX_HOME are present -- and codex is
# still the correct answer, because codex is the process invoking us.
jm_current_host() {
  [ -n "${CODEX_HOME:-}" ] && { printf 'codex\n'; return 0; }
  printf 'claude-code\n'
}


# ── hook payload ────────────────────────────────────────────────────────────

# jm_payload_read (filter-in) — drain stdin to a temp file, capped at 1 MiB.
# stdout: absolute path of the temp file. rc 0 ok · 1 stdin unreadable.
# Always drains stdin fully: a hook that does not drain stdin can wedge the writer.
jm_payload_read() {
  local f
  f="$(mktemp "${TMPDIR:-/tmp}/jhh3-mem-payload.XXXXXXXX" 2>/dev/null)" || return 1
  chmod 0600 "$f" 2>/dev/null || true
  { LC_ALL=C head -c 1048576 >"$f"; } 2>/dev/null || true
  # Drain whatever is left so the writer never blocks on a full pipe.
  { LC_ALL=C cat >/dev/null; } 2>/dev/null || true
  printf '%s\n' "$f"
  return 0
}

# jm_payload_parse PAYLOAD_FILE
# stdout: one TAB-separated line, exactly 7 fields:
#   session_id, transcript_path, cwd, hook_event_name, source, reason, session_title
# rc 0 valid JSON · 1 invalid (caller logs error reason=bad_payload and exits 0)
#     · 2 misuse · 3 sqlite3 absent.
# json_valid() guards the extract: unguarded json_extract on bad JSON exits 1 (P63).
jm_payload_parse() {
  local f="${1:-}"
  [ -n "$f" ] || return 2
  [ -r "$f" ] || return 1
  jm_require_sqlite || return 3

  local out sel k
  sel=''
  for k in session_id transcript_path cwd hook_event_name source reason session_title; do
    [ -z "$sel" ] || sel="$sel || char(9) || "
    sel="${sel}replace(replace(coalesce(json_extract(j,'\$.${k}'),''),char(9),' '),char(10),' ')"
  done

  out="$(sqlite3 -bail -batch -noheader \
    -cmd '.mode list' \
    -cmd ".parameter set :f cast(x'$(_jm_hex "$f")'as'text')" \
    ':memory:' \
    "SELECT $sel FROM (SELECT readfile(:f) AS j) WHERE json_valid(j) = 1;" 2>/dev/null)" || return 1

  [ -n "$out" ] || return 1
  printf '%s\n' "$out"
  return 0
}

# _jm_hex STRING — lowercase hex of the argument's bytes. Internal.
# Used to move arbitrary text into sqlite3 as `cast(x'..' as text)`, which is
# immune to both the shell and the sqlite3 dot-command tokenizer.
_jm_hex() {
  printf '%s' "${1:-}" | od -An -v -tx1 | tr -d ' \n'
}

# ── repo identity ───────────────────────────────────────────────────────────

# jm_repo_key_from_origin ORIGIN — normalized repo_key, or empty.
# git@github.com:Acme/agent-plugins.git -> acme/agent-plugins
# github:acme/webapp                    -> acme/webapp
# https://github.com/Foo/Bar.git             -> foo/bar
jm_repo_key_from_origin() {
  local o="${1:-}"
  o="${o#"${o%%[![:space:]]*}"}"
  o="${o%"${o##*[![:space:]]}"}"
  [ -n "$o" ] || return 0

  # A filesystem remote is not a namespaced key; let the caller fall back.
  case "$o" in
    /* | ./* | ../* | file://*) return 0 ;;
  esac

  o="${o%.git}"
  o="${o%/}"

  local p="$o"
  p="${p#ssh://}"; p="${p#git+ssh://}"; p="${p#git://}"
  p="${p#https://}"; p="${p#http://}"
  p="${p#github:}"; p="${p#gh:}"

  case "$p" in
    *@*:*) p="${p#*@}"; p="${p#*:}" ;;   # git@host:owner/repo  or  git@host:22/owner/repo
    *@*)   p="${p#*@}" ;;                # user@host/owner/repo
  esac

  # A leading ":<port>/" survivor from an ssh:// URL is not an owner.
  case "$p" in
    [0-9]*/*)
      local maybe_port="${p%%/*}"
      case "$maybe_port" in
        *[!0-9]*) : ;;
        *) p="${p#*/}" ;;
      esac
      ;;
  esac

  # Drop a leading host segment (a first path component containing a dot).
  case "$p" in
    */*)
      local first="${p%%/*}"
      case "$first" in
        *.*) p="${p#*/}" ;;
      esac
      ;;
  esac

  p="${p#/}"; p="${p#:}"; p="${p%.git}"
  [ -n "$p" ] || return 0
  printf '%s\n' "$p" | LC_ALL=C tr '[:upper:]' '[:lower:]'
}

# jm_repo_ident DIR
# stdout: one TAB-separated line, 4 fields: repo_key, worktree, branch, repo_path
# rc 0 always. Every git probe is || true guarded; this function must never be
# the reason a hook fails. SPEC §6.1.1.
jm_repo_ident() {
  local dir="${1:-$PWD}"
  local toplevel='' common_dir='' branch='' origin=''
  local repo_key='' worktree='' repo_path=''

  if [ -d "$dir" ] && jm_have git; then
    toplevel="$(git -C "$dir" rev-parse --show-toplevel 2>/dev/null || true)"
    common_dir="$(git -C "$dir" rev-parse --git-common-dir 2>/dev/null || true)"
    branch="$(git -C "$dir" rev-parse --abbrev-ref HEAD 2>/dev/null || true)"
    origin="$(git -C "$dir" remote get-url origin 2>/dev/null || true)"
  fi

  [ "$branch" = 'HEAD' ] && branch=''

  if [ -n "$origin" ]; then
    repo_key="$(jm_repo_key_from_origin "$origin")"
  fi

  if [ -z "$repo_key" ]; then
    local anchor="$toplevel"
    [ -n "$anchor" ] || anchor="$(_jm_realpath "$dir")"
    repo_key="dir:${anchor##*/}@$(jm_sha256_str "$anchor" | cut -c1-8)"
  fi

  if [ -n "$toplevel" ]; then
    repo_path="$toplevel"
    # In a linked worktree the common dir points at the MAIN repo's .git, so it
    # does not live under this toplevel.
    if [ -n "$common_dir" ]; then
      case "$common_dir" in
        /*) : ;;
        *) common_dir="$dir/$common_dir" ;;
      esac
      local rc_common rc_top
      rc_common="$(_jm_realpath "$common_dir")"
      rc_top="$(_jm_realpath "$toplevel")"
      case "$rc_common" in
        "$rc_top"/* | "$rc_top") worktree='' ;;
        *) worktree="${toplevel##*/}" ;;
      esac
    fi
  else
    repo_path="$(_jm_realpath "$dir")"
  fi

  printf '%s\t%s\t%s\t%s\n' "$repo_key" "$worktree" "$branch" "$repo_path"
  return 0
}

# _jm_realpath PATH — best-effort absolute path. Internal, never fails.
_jm_realpath() {
  local p="${1:-}"
  [ -n "$p" ] || { printf '%s\n' "$PWD"; return 0; }
  realpath -- "$p" 2>/dev/null && return 0
  readlink -f -- "$p" 2>/dev/null && return 0
  case "$p" in
    /*) printf '%s\n' "$p" ;;
    *) printf '%s/%s\n' "$PWD" "$p" ;;
  esac
}

# jm_rel_path REPO_PATH ABS_OR_REL_PATH
# stdout: repo-relative normalized path, or the input unchanged if outside REPO_PATH.
jm_rel_path() {
  local root="${1:-}" p="${2:-}"
  [ -n "$p" ] || return 0
  p="${p#./}"
  # A literal leading tilde-slash (recorded text, never a shell expansion) is
  # rooted at $HOME. The tilde is built from its byte so no quoting rule applies.
  local tilde; printf -v tilde '\176'
  if [ "${p:0:2}" = "$tilde/" ]; then p="$HOME/${p:2}"; fi
  if [ -n "$root" ]; then
    root="${root%/}"
    case "$p" in
      "$root"/*) p="${p#"$root"/}" ;;
    esac
    # Also bridge linked worktrees: <root>/.worktrees/<name>/<rel>
    case "$p" in
      .worktrees/*/*) p="${p#.worktrees/}"; p="${p#*/}" ;;
    esac
  fi
  p="${p#./}"
  printf '%s\n' "$p"
}

# ── text sanitizers ─────────────────────────────────────────────────────────

# jm_defang (filter) — STRUCTURAL neutralization of the two things stored text
# could use to escape its frame.  Applied at BOTH write time (inside
# jm_sanitize) and read time (lib/render.sh), because the 20,216 rows migrated
# before this rule existed are already in the database and a guarantee that
# depends on every row having been written by the current code is not a
# guarantee at all.
#
#   1. `<` and `>` are mapped to U+2039/U+203A (‹ ›).  The digest's only
#      structural boundary is the literal `</jhh3-mem>` closing tag; a stored
#      title containing that string would close the "data, not instructions"
#      envelope early and promote everything after it to trusted context.
#      Mapping rather than deleting keeps `Vec<T>` and `a -> b` readable, and
#      neither replacement byte can begin an SGML tag.  It also defuses a
#      forged `<system-reminder>`, which is the tag Claude Code uses for its
#      own trusted notices.
#   2. Role markers are defanged ANYWHERE on the line, not just at its start.
#      Every text field is flattened to one line before it is rendered, so an
#      anchored rule can only ever catch the first of them.  A preceding
#      space (or start of line) is still required so `filesystem:` survives.
jm_defang() {
  LC_ALL=C sed -E \
    -e 's/</‹/g' \
    -e 's/>/›/g' \
    -e 's/([[:space:]]|^)(system|assistant|user|human)([[:space:]]*):/\1~\2\3:/Ig'
}

# jm_sanitize (filter) — applied ONCE, at write time (SPEC §13.3.2).
#   - tabs folded to spaces; all other bytes < 0x20 (except \n) and 0x7F removed
#   - backticks removed, ``` fences neutralized to '''
#   - angle brackets mapped away and role markers defanged, via jm_defang
jm_sanitize() {
  LC_ALL=C tr '\011' ' ' \
    | LC_ALL=C tr -d '\000-\010\013-\037\177' \
    | LC_ALL=C sed -E \
      -e "s/\`\`\`/'''/g" \
      -e "s/\`//g" \
    | jm_defang
}

# jm_flatten (filter) — newlines and tabs to single spaces, runs collapsed, trimmed.
jm_flatten() {
  { LC_ALL=C tr '\n\t' '  '; printf '\n'; } \
    | LC_ALL=C sed -E -e 's/[[:space:]]+/ /g' -e 's/^[[:space:]]+//' -e 's/[[:space:]]+$//'
}

# jm_truncate_bytes MAX (filter)
# Truncate to at most MAX BYTES without splitting a UTF-8 sequence and without
# splitting a line: cut at the last \n at or before MAX; if there is none, cut at
# the last UTF-8 boundary at or before MAX.  NEVER `head -c` (it splits UTF-8, P81).
jm_truncate_bytes() {
  local max="${1:-}"
  case "$max" in '' | *[!0-9]*) return 2 ;; esac

  local LC_ALL=C
  local data
  data="$(cat; printf x)"
  data="${data%x}"

  [ "${#data}" -le "$max" ] && { printf '%s' "$data"; return 0; }

  local cut="${data:0:max}"

  # Prefer a line boundary.
  local head="${cut%$'\n'*}"
  if [ "$head" != "$cut" ]; then
    printf '%s\n' "$head"
    return 0
  fi

  # No newline in range: back off to a UTF-8 character boundary.
  local i="${#cut}" k=0 b
  while [ "$i" -gt 0 ]; do
    b="${cut:i-1:1}"
    case "$b" in
      [$'\x80'-$'\xbf']) k=$((k + 1)); i=$((i - 1)) ;;
      *) break ;;
    esac
  done
  if [ "$i" -eq 0 ]; then
    printf ''
    return 0
  fi
  b="${cut:i-1:1}"
  local need=0
  case "$b" in
    [$'\xc0'-$'\xdf']) need=1 ;;
    [$'\xe0'-$'\xef']) need=2 ;;
    [$'\xf0'-$'\xf7']) need=3 ;;
  esac
  if [ "$k" -lt "$need" ]; then
    cut="${cut:0:i-1}"
  fi
  printf '%s' "$cut"
  return 0
}

# jm_sql_quote STRING — the string wrapped in single quotes, internal ' doubled.
jm_sql_quote() {
  local s="${1:-}"
  printf "'%s'\n" "${s//\'/\'\'}"
}

# ── hashing ─────────────────────────────────────────────────────────────────

# jm_sha256 FILE — 64 lowercase hex. rc 0 · 1 unreadable.
jm_sha256() {
  local f="${1:-}"
  [ -r "$f" ] || return 1
  local h=''
  if jm_have sha256sum; then
    h="$(sha256sum -- "$f" 2>/dev/null | cut -d' ' -f1)"
  elif jm_have shasum; then
    h="$(shasum -a 256 -- "$f" 2>/dev/null | cut -d' ' -f1)"
  elif jm_have openssl; then
    h="$(openssl dgst -sha256 -r -- "$f" 2>/dev/null | cut -d' ' -f1)"
  fi
  [ "${#h}" -eq 64 ] || return 1
  printf '%s\n' "$h"
}

# jm_sha256_str STRING — 64 lowercase hex.
jm_sha256_str() {
  local s="${1:-}" h=''
  if jm_have sha256sum; then
    h="$(printf '%s' "$s" | sha256sum 2>/dev/null | cut -d' ' -f1)"
  elif jm_have shasum; then
    h="$(printf '%s' "$s" | shasum -a 256 2>/dev/null | cut -d' ' -f1)"
  elif jm_have openssl; then
    h="$(printf '%s' "$s" | openssl dgst -sha256 2>/dev/null | sed -E 's/^.*= *//')"
  fi
  if [ "${#h}" -ne 64 ]; then
    # Deterministic last resort so repo_key never becomes empty.
    h="$(printf '%s' "$s" | cksum | tr -d ' ' | LC_ALL=C tr -c '0-9a-f' '0')"
    h="${h}00000000000000000000000000000000000000000000000000000000000000000"
    h="${h:0:64}"
  fi
  printf '%s\n' "$h"
}

# ── staging ─────────────────────────────────────────────────────────────────

# jm_mkstage — absolute path of a fresh mktemp -d. rc 0 · 1 failure.
# THE CALLER MUST install `trap 'rm -rf "$STAGE"' EXIT INT TERM` immediately.
#
# The umask is tightened for the REST OF THIS PROCESS, not just for the mkdir:
# the files a worker writes inside the stage (fields.tsv holds PRE-redaction
# transcript text) do not inherit the directory's 0700, and a SIGKILL — which
# is exactly how Claude Code ends a hook that overruns — leaves them behind.
# Every file any jhh3-mem process creates should be 0600; nothing it writes is
# meant for another uid.
jm_mkstage() {
  umask 0077
  local d
  d="$(mktemp -d "${TMPDIR:-/tmp}/jhh3-mem.XXXXXXXX" 2>/dev/null)" || return 1
  chmod 0700 "$d" 2>/dev/null || true
  printf '%s\n' "$d"
}

# jm_reap_stale_stage — best-effort sweep of staging artifacts orphaned by a
# SIGKILL (hook timeout, machine shutdown, multiplexer teardown).  Bounded by
# its own 2 s wall clock, never fails, never touches anything younger than an
# hour (a live sibling session's stage) and never touches the test harness's
# own `jhh3-mem-test.*` directories.  Called from the SessionEnd guard.
# rc 0 ALWAYS.
jm_reap_stale_stage() {
  local root="${TMPDIR:-/tmp}"
  [ -d "$root" ] || return 0
  jm_have find || return 0
  local -a t=(timeout -k 1 2)
  jm_have timeout || t=()
  ${t[@]+"${t[@]}"} find "$root" -maxdepth 1 \
    \( -name 'jhh3-mem.*' -o -name 'jhh3-mem-payload.*' -o -name 'jhh3-mem-err.*' \) \
    -mmin +60 -exec rm -rf -- '{}' + >/dev/null 2>&1 || true
  return 0
}

# ── redaction (SPEC §12) ────────────────────────────────────────────────────
# Extension to §6.1: the single implementation of the ruleset. lib/capture.sh's
# jm_capture_redact is a thin wrapper over jm_redact_file with identical
# semantics (count on stdout, rc 1 = fail closed).

# jm_redact_file IN_FILE OUT_FILE
# stdout: integer, number of redactions applied.
# rc 0 ok · 1 sed failed or the ruleset is unreadable — FAIL CLOSED: the caller
#      must abort the write rather than persist unfiltered text · 2 misuse.
jm_redact_file() {
  local in="${1:-}" out="${2:-}"
  [ -n "$in" ] && [ -n "$out" ] || return 2
  [ -r "$in" ] || return 1
  [ -r "$JM_REDACT_SED" ] || return 1

  LC_ALL=C sed -E -f "$JM_REDACT_SED" -- "$in" >"$out" 2>/dev/null || return 1
  chmod 0600 "$out" 2>/dev/null || true
  # A zero-length output for a non-empty input means sed died mid-stream.
  if [ -s "$in" ] && [ ! -s "$out" ]; then return 1; fi

  jm_redact_count "$out"
}

# jm_redact_stream (filter) — redacted stdin on stdout. rc 0 · 1 fail closed.
jm_redact_stream() {
  [ -r "$JM_REDACT_SED" ] || return 1
  LC_ALL=C sed -E -f "$JM_REDACT_SED" || return 1
}

# jm_redact_count FILE — total count of [REDACTED:*] markers in FILE.
jm_redact_count() {
  local f="${1:-}" n
  [ -r "$f" ] || { printf '0\n'; return 0; }
  n="$(LC_ALL=C grep -o -- '\[REDACTED:[a-z]*\]' "$f" 2>/dev/null | wc -l | tr -d ' ')"
  case "$n" in '' | *[!0-9]*) n=0 ;; esac
  printf '%s\n' "$n"
}

# jm_redact_histogram FILE — `kind<TAB>count` lines, descending. Reporting only.
jm_redact_histogram() {
  local f="${1:-}"
  [ -r "$f" ] || return 0
  LC_ALL=C grep -o -- '\[REDACTED:[a-z]*\]' "$f" 2>/dev/null \
    | LC_ALL=C sed -E 's/^\[REDACTED:([a-z]*)\]$/\1/' \
    | LC_ALL=C sort | LC_ALL=C uniq -c | LC_ALL=C sort -rn \
    | LC_ALL=C awk '{printf "%s\t%s\n", $2, $1}'
  return 0
}

# ── path manifest (SPEC §10.2) ──────────────────────────────────────────────
# Generated by the same knowledge that creates the paths, so it cannot drift.
# `uninstall` and `doctor` consume it directly. Extension to §6.5, placed here
# because it is pure path knowledge and three lanes need it.

# jm_paths_manifest — one absolute path per line: the exhaustive list of
# everything the plugin ever creates. rc 0 always.
jm_paths_manifest() {
  printf '%s\n' \
    "$JM_HOME" \
    "$JM_DB" \
    "$JM_DB-wal" \
    "$JM_DB-shm" \
    "$JM_LOG" \
    "$JM_LOG.1" \
    "$JM_LOCK" \
    "$JM_BACKUPS" \
    "$JM_VEC_DIR" \
    "$JM_VEC_SO.so"
  return 0
}

# jm_paths_transient — the $TMPDIR globs jhh3-mem creates and deletes within a
# single hook invocation. They are listed because a SIGKILL can outlive the
# cleanup that removes them, and "one command removes everything it ever
# created" has to mean everything. rc 0 always.
jm_paths_transient() {
  local root="${TMPDIR:-/tmp}"
  printf '%s\n' \
    "$root/jhh3-mem.*" \
    "$root/jhh3-mem-payload.*" \
    "$root/jhh3-mem-err.*"
  return 0
}

# jm_paths_manifest_annotated — the §10.2 rendering: path, kind, mode, note.
jm_paths_manifest_annotated() {
  printf '%s/\tdir\t0700\t\n'                      "$JM_HOME"
  printf '%s\tfile\t0600\t\n'                      "$JM_DB"
  printf '%s-wal\tfile\t0600\tsqlite-managed\n'    "$JM_DB"
  printf '%s-shm\tfile\t0600\tsqlite-managed\n'    "$JM_DB"
  printf '%s\tfile\t0600\trotated at 2 MB\n'       "$JM_LOG"
  printf '%s.1\tfile\t0600\tat most one rotation\n' "$JM_LOG"
  printf '%s\tfile\t0600\tDDL only\n'              "$JM_LOCK"
  printf '%s/\tdir\t0700\tcreated only by migrate\n' "$JM_BACKUPS"
  printf '%s/\tdir\t0700\tcreated only by `embed`\n' "$JM_VEC_DIR"
  printf '%s.so\tfile\t0644\tsqlite-vec extension; fetched by `embed`, optional\n' "$JM_VEC_SO"
  local g
  while IFS= read -r g; do
    printf '%s\ttrans\t0600\tstaging; deleted at end of hook, swept if killed\n' "$g"
  done < <(jm_paths_transient)
  # The nightly job is the one thing we create OUTSIDE the store, so it is the one
  # thing that could quietly outlive `uninstall --purge` and make the inventory a
  # lie. List it here and remove it there. See lib/schedule.sh.
  # The launcher shim: like the crontab lines, created outside $JM_HOME and so the
  # other thing that could outlive removal if it were not listed here.
  if [ -e "${JM_SHIM:-$HOME/.local/bin/jhh3-mem}" ]; then
    printf '%s\tfile\t0755\tlauncher shim; puts the CLI on your login PATH; removed by uninstall --purge\n' \
      "${JM_SHIM:-$HOME/.local/bin/jhh3-mem}"
  fi
  if command -v crontab >/dev/null 2>&1 && crontab -l 2>/dev/null | grep -qF '# jhh3-mem-schedule'; then
    printf 'crontab: %s lines tagged %s\tsched\t-\tnightly dream/enrich; removed by uninstall --purge\n' \
      "$(crontab -l 2>/dev/null | grep -cF '# jhh3-mem-schedule')" '# jhh3-mem-schedule'
  fi
  return 0
}

# ── process probe (SPEC §10.2, §10.4) ───────────────────────────────────────

# jm_probe_processes — one line per LIVE process whose command line names
# jhh3-mem and that is NOT part of this probe: `<pid> <command>`.
#
# ONE implementation, shared by `doctor` and `uninstall`, because both make the
# same load-bearing claim — "this plugin has no daemon" — and a claim proved by
# two different pieces of code is a claim that can disagree with itself.
#
# Excluded, in order: non-numeric pgrep noise; this shell; its parent; anything
# in this shell's process group (the probe pipeline lives there); any pid that
# has already exited by the time `ps` is asked, which is the usual fate of the
# `pgrep` subshell and which used to print a BARE PID with no command line
# directly beneath the claim that no such process exists; and `pgrep` itself.
# A row that cannot be named is a row that must not be printed.
# rc 0 always.
jm_probe_processes() {
  jm_have pgrep || return 0
  jm_have ps || return 0
  local mypgid pid ppgid pcmd ancestors='' walk
  mypgid="$(ps -o pgid= -p $$ 2>/dev/null | tr -d ' ')"
  # Our own ancestor chain. A process we are RUNNING INSIDE cannot be a leaked
  # daemon, and the plugin directory is literally named jhh3-mem, so every
  # shell between us and the terminal matches `pgrep -f jhh3-mem` by path.
  walk=$$
  while [ "$walk" -gt 1 ] 2>/dev/null; do
    ancestors="$ancestors $walk"
    walk="$(ps -o ppid= -p "$walk" 2>/dev/null | tr -d ' ')"
    [ -n "$walk" ] || break
  done
  while IFS= read -r pid; do
    [ -n "$pid" ] || continue
    case "$pid" in *[!0-9]*) continue ;; esac
    case " $ancestors " in *" $pid "*) continue ;; esac
    ppgid="$(ps -o pgid= -p "$pid" 2>/dev/null | tr -d ' ')"
    [ -n "$mypgid" ] && [ "$ppgid" = "$mypgid" ] && continue
    pcmd="$(ps -o args= -p "$pid" 2>/dev/null | LC_ALL=C cut -c1-96)"
    pcmd="${pcmd#"${pcmd%%[![:space:]]*}"}"
    [ -n "$pcmd" ] || continue
    case "$pcmd" in *pgrep*) continue ;; esac
    # Match what a process IS, not what its command line mentions. The plugin
    # directory is named jhh3-mem, so `pgrep -f jhh3-mem` also hits any editor,
    # shell, or test runner that merely has the path on its command line -- and
    # those are not leaked daemons of ours. Report only processes actually
    # executing one of our entry points. (The ancestor/pgid exclusions above stay
    # as defence in depth; this is the predicate that makes the answer correct.)
    case "$pcmd" in
      */bin/jhh3-mem|*/bin/jhh3-mem\ *|jhh3-mem|jhh3-mem\ *) ;;
      */scripts/session-start.sh*|*/scripts/session-end.sh*) ;;
      *) continue ;;
    esac
    printf '%s %s\n' "$pid" "$pcmd"
  done < <(pgrep -f 'jhh3-mem' 2>/dev/null || true)
  return 0
}

# jm_human_bytes N — "41.3 MB" style. Reporting only.
jm_human_bytes() {
  local b="${1:-0}"
  case "$b" in '' | *[!0-9]*) b=0 ;; esac
  LC_ALL=C awk -v b="$b" 'BEGIN{
    split("B KB MB GB TB", u, " "); i=1
    while (b >= 1024 && i < 5) { b /= 1024; i++ }
    if (i == 1) printf "%d %s\n", b, u[i]; else printf "%.1f %s\n", b, u[i]
  }'
}
