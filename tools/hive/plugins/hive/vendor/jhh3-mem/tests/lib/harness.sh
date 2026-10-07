#!/usr/bin/env bash
# harness.sh — shared test scaffolding for jhh3-mem.
#
# Sourced by every tests/t-*.sh. Suites run under `set -uo pipefail` and
# deliberately NOT under `set -e`: an assertion must be able to fail without
# aborting the rest of the suite.
#
# Contract with tests/run.sh
# -------------------------
# A suite's last line of stdout is the machine-readable summary:
#
#     ## SUITE <name> pass=<n> fail=<n> pend=<n> skip=<n>
#
# and its exit status is 0 when fail == 0, 1 otherwise. `pend` never fails the
# suite; run.sh turns pend into a failure only under --strict.
#
# Outcome vocabulary
# ------------------
#   pass  the assertion held
#   fail  the assertion did not hold           -> suite exits 1
#   pend  a file this case needs has not been  -> not a failure; named with its
#         written yet by its owning lane          owning lane so it is actionable
#   skip  the case cannot run in this          -> not a failure; e.g. `ss` absent,
#         environment, by design                  or the live session is opt-in
#
# API
# ---
#   jt_begin <suite-name>              start; installs the cleanup trap
#   jt_end                             print the summary line, exit 0/1
#   jt_case <id> <description>         open a case (all later asserts label with <id>)
#   jt_ok / jt_fail / jt_pend / jt_skip <msg>
#   jt_assert_eq <expected> <actual> <msg>
#   jt_assert_ne <unexpected> <actual> <msg>
#   jt_assert_rc <expected> <actual> <msg>
#   jt_assert_contains <haystack> <needle> <msg>
#   jt_assert_not_contains <haystack> <needle> <msg>
#   jt_assert_empty <value> <msg>
#   jt_assert_file <path> <msg>
#   jt_assert_no_file <path> <msg>
#   jt_need <path> <owning-lane>       rc 0 if present; else records pend, rc 1
#   jt_need_cmd <cmd> <why>            rc 0 if present; else records skip,  rc 1
#   jt_store                           fresh store dir, exports JHH3_MEM_HOME, auto-cleaned
#   jt_tmpdir                          throwaway dir, auto-cleaned
#   jt_make_transcript <path> [marker] write a synthetic Claude Code transcript
#   jt_payload_start <sid> <tpath> <cwd> [source]
#   jt_payload_end   <sid> <tpath> <cwd> [reason]
#   jt_stray_processes                 lines for jhh3-mem processes that are not us
#
# All paths are absolute. Nothing here writes outside $TMPDIR.

# ---------------------------------------------------------------- resolution --

# tests/lib/harness.sh -> plugin root is two levels up. Parameter expansion only:
# `dirname` is unavailable when a fail-open test strips PATH.
_jt_here="${BASH_SOURCE[0]}"
case "$_jt_here" in
  */*) _jt_dir="${_jt_here%/*}" ;;
  *) _jt_dir="." ;;
esac
JT_TESTS_DIR="$(cd -- "$_jt_dir/.." && pwd -P)"
JT_PLUGIN_ROOT="$(cd -- "$JT_TESTS_DIR/.." && pwd -P)"
JT_REPO_ROOT="$(cd -- "$JT_PLUGIN_ROOT/../.." && pwd -P)"
unset _jt_here _jt_dir
export JT_TESTS_DIR JT_PLUGIN_ROOT JT_REPO_ROOT

JT_SUITE=""
JT_CASE=""
JT_TMP=""
JT_STORE=""
JT_PASS=0
JT_FAIL=0
JT_PEND=0
JT_SKIP=0
JT_CLEAN=()

# Colour only when stdout is a terminal; run.sh captures suites, so this is off
# in the aggregate run and on when a suite is invoked directly.
if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
  JT_C_OK=$'\033[32m'; JT_C_BAD=$'\033[31m'; JT_C_PEND=$'\033[33m'
  JT_C_DIM=$'\033[2m'; JT_C_OFF=$'\033[0m'
else
  JT_C_OK=""; JT_C_BAD=""; JT_C_PEND=""; JT_C_DIM=""; JT_C_OFF=""
fi

# ------------------------------------------------------------------ lifecycle --

_jt_cleanup() {
  local d
  for d in ${JT_CLEAN+"${JT_CLEAN[@]}"}; do
    case "$d" in
      /tmp/* | /var/tmp/* | "${TMPDIR:-/nonexistent}"/*) rm -rf -- "$d" 2>/dev/null || true ;;
      *) printf 'harness: refusing to clean suspicious path: %s\n' "$d" >&2 ;;
    esac
  done
  JT_CLEAN=()
}

jt_begin() {
  JT_SUITE="${1:?suite name}"
  trap _jt_cleanup EXIT INT TERM
  printf '%s=== %s ===%s\n' "$JT_C_DIM" "$JT_SUITE" "$JT_C_OFF"
}

jt_end() {
  _jt_cleanup
  printf '## SUITE %s pass=%d fail=%d pend=%d skip=%d\n' \
    "$JT_SUITE" "$JT_PASS" "$JT_FAIL" "$JT_PEND" "$JT_SKIP"
  [ "$JT_FAIL" -eq 0 ] || exit 1
  exit 0
}

jt_case() {
  JT_CASE="${1:?case id}"
  shift
  printf '%s--- %s: %s%s\n' "$JT_C_DIM" "$JT_CASE" "$*" "$JT_C_OFF"
}

# ------------------------------------------------------------------- outcomes --

jt_ok() {
  JT_PASS=$((JT_PASS + 1))
  printf '%sPASS%s %-10s %s\n' "$JT_C_OK" "$JT_C_OFF" "$JT_CASE" "$*"
}

jt_fail() {
  JT_FAIL=$((JT_FAIL + 1))
  printf '%sFAIL%s %-10s %s\n' "$JT_C_BAD" "$JT_C_OFF" "$JT_CASE" "$*"
}

jt_pend() {
  JT_PEND=$((JT_PEND + 1))
  printf '%sPEND%s %-10s %s\n' "$JT_C_PEND" "$JT_C_OFF" "$JT_CASE" "$*"
}

jt_skip() {
  JT_SKIP=$((JT_SKIP + 1))
  printf '%sSKIP%s %-10s %s\n' "$JT_C_DIM" "$JT_C_OFF" "$JT_CASE" "$*"
}

# -------------------------------------------------------------------- asserts --

jt_assert_eq() {
  local want="$1" got="$2" msg="$3"
  if [ "$want" = "$got" ]; then
    jt_ok "$msg"
  else
    jt_fail "$msg -- want [$want] got [$got]"
  fi
}

jt_assert_ne() {
  local bad="$1" got="$2" msg="$3"
  if [ "$bad" != "$got" ]; then
    jt_ok "$msg"
  else
    jt_fail "$msg -- value must not be [$bad]"
  fi
}

jt_assert_rc() {
  local want="$1" got="$2" msg="$3"
  if [ "$want" = "$got" ]; then
    jt_ok "$msg (rc $got)"
  else
    jt_fail "$msg -- want rc $want got rc $got"
  fi
}

jt_assert_contains() {
  local hay="$1" needle="$2" msg="$3"
  case "$hay" in
    *"$needle"*) jt_ok "$msg" ;;
    *) jt_fail "$msg -- [$needle] not found in: $(printf '%.200s' "$hay")" ;;
  esac
}

jt_assert_not_contains() {
  local hay="$1" needle="$2" msg="$3"
  case "$hay" in
    *"$needle"*) jt_fail "$msg -- [$needle] present in: $(printf '%.200s' "$hay")" ;;
    *) jt_ok "$msg" ;;
  esac
}

jt_assert_empty() {
  local val="$1" msg="$2"
  if [ -z "$val" ]; then
    jt_ok "$msg"
  else
    jt_fail "$msg -- expected empty, got ${#val} bytes: $(printf '%.120s' "$val")"
  fi
}

jt_assert_file() {
  if [ -e "$1" ]; then jt_ok "$2"; else jt_fail "$2 -- missing: $1"; fi
}

jt_assert_no_file() {
  if [ ! -e "$1" ]; then jt_ok "$2"; else jt_fail "$2 -- still present: $1"; fi
}

# ------------------------------------------------------------- preconditions --

# jt_need PATH OWNER — the file belongs to a lane that has not landed yet.
# Records a PEND (not a failure) and returns 1 so the caller can `|| return 0`.
jt_need() {
  local p="$1" owner="${2:-unknown lane}"
  if [ -e "$p" ]; then return 0; fi
  jt_pend "not implemented yet: ${p#"$JT_PLUGIN_ROOT"/} (owner: $owner)"
  return 1
}

# jt_need_cmd CMD WHY — environment capability, not a defect.
jt_need_cmd() {
  if command -v -- "$1" >/dev/null 2>&1; then return 0; fi
  jt_skip "$1 not available: ${2:-required for this case}"
  return 1
}

# ------------------------------------------------------------------ scratch --

# jt_tmpdir — sets $JT_TMP to a fresh throwaway dir and registers it for
# cleanup.
#
# CALL IT DIRECTLY, NEVER IN `$( )`. A command substitution runs it in a
# subshell, where both the cleanup registration and (for jt_store) the
# `export` are discarded when the subshell exits — the latter silently aiming
# every subsequent hook, CLI call and *uninstall* at the user's real store.
# That bug was live in this harness once; hence jt_require_sandbox below.
#
#   jt_tmpdir; work="$JT_TMP"
jt_tmpdir() {
  JT_TMP="$(mktemp -d "${TMPDIR:-/tmp}/jhh3-mem-test.XXXXXX")" || return 1
  JT_CLEAN+=("$JT_TMP")
  return 0
}

# jt_store — sets $JT_STORE and exports JHH3_MEM_HOME, so every jhh3-mem code
# path (library, hook, CLI) lands inside it. Same no-subshell rule.
#
#   jt_store; store="$JT_STORE"
jt_store() {
  jt_tmpdir || return 1
  JT_STORE="$JT_TMP/store"
  mkdir -p -- "$JT_STORE" || return 1
  chmod 0700 "$JT_STORE" || return 1
  export JHH3_MEM_HOME="$JT_STORE"
  return 0
}

# jt_require_sandbox — hard gate before anything destructive. Aborts the suite
# (exit 1, after printing the summary) unless JHH3_MEM_HOME is exported and
# lives under a temp root. A test that can delete the user's memory because an
# export silently failed is worse than no test.
jt_require_sandbox() {
  local h="${JHH3_MEM_HOME:-}"
  # macOS TMPDIR ends in '/', so mktemp "$TMPDIR/..." yields '...T//jhh3-mem-test.*'.
  while [ "${h#*//}" != "$h" ]; do h="${h%%//*}/${h#*//}"; done
  case "$h" in
    /tmp/jhh3-mem-test.* | /var/tmp/jhh3-mem-test.*) return 0 ;;
  esac
  case "${TMPDIR:-}" in
    /*) case "$h" in "${TMPDIR%/}"/jhh3-mem-test.*) return 0 ;; esac ;;
  esac
  jt_fail "SANDBOX GUARD: JHH3_MEM_HOME is [${h:-<unset>}], not a test store -- refusing to continue"
  jt_end
}

# ---------------------------------------------------------------- fixtures --

# jt_make_transcript PATH [MARKER] — a synthetic Claude Code transcript in the
# shape lib/capture.sh extracts from (SPEC 6.3.2): one ai-title line, typed and
# untyped user lines, assistant lines carrying Edit/Write/Read/Bash tool_use
# blocks. MARKER is woven into the title so a test can find the row again.
jt_make_transcript() {
  local out="${1:?path}" marker="${2:-zqmarker}" cwd="${3:-$JT_REPO_ROOT}"
  local ts="2026-08-17T12:00:00.000Z"
  {
    printf '{"type":"user","promptSource":"typed","timestamp":"%s","cwd":"%s","gitBranch":"jhh3-mem","message":{"role":"user","content":"add the %s guard to the retry path"}}\n' "$ts" "$cwd" "$marker"
    printf '{"type":"user","promptSource":"slash_command","timestamp":"%s","cwd":"%s","message":{"role":"user","content":"<command-name>/clear</command-name>"}}\n' "$ts" "$cwd"
    printf '{"type":"assistant","timestamp":"%s","cwd":"%s","gitBranch":"jhh3-mem","message":{"role":"assistant","content":[{"type":"tool_use","name":"Read","input":{"file_path":"%s/lib/common.sh"}}]}}\n' "$ts" "$cwd" "$cwd"
    printf '{"type":"assistant","timestamp":"%s","cwd":"%s","gitBranch":"jhh3-mem","message":{"role":"assistant","content":[{"type":"tool_use","name":"Edit","input":{"file_path":"%s/lib/retry.sh"}}]}}\n' "$ts" "$cwd" "$cwd"
    printf '{"type":"assistant","timestamp":"%s","cwd":"%s","gitBranch":"jhh3-mem","message":{"role":"assistant","content":[{"type":"tool_use","name":"Write","input":{"file_path":"%s/lib/retry_test.sh"}}]}}\n' "$ts" "$cwd" "$cwd"
    printf '{"type":"assistant","timestamp":"%s","cwd":"%s","message":{"role":"assistant","content":[{"type":"tool_use","name":"Bash","input":{"command":"bash tests/run.sh t-invariants"}}]}}\n' "$ts" "$cwd"
    printf '{"type":"ai-title","timestamp":"%s","aiTitle":"Retry guard %s added to the backoff path"}\n' "$ts" "$marker"
    printf '{"type":"assistant","timestamp":"%s","cwd":"%s","message":{"role":"assistant","content":[{"type":"text","text":"Added the %s guard in lib/retry.sh and a regression test in lib/retry_test.sh. The backoff now caps at 30s instead of growing without bound, and the guard is exercised by tests/run.sh. Next: wire the same cap into the migration path and delete the legacy sleep loop."}]}}\n' "$ts" "$cwd" "$marker"
  } >"$out"
}

jt_payload_start() {
  printf '{"session_id":"%s","transcript_path":"%s","cwd":"%s","hook_event_name":"SessionStart","source":"%s"}\n' \
    "${1:?sid}" "${2:?transcript}" "${3:?cwd}" "${4:-startup}"
}

jt_payload_end() {
  printf '{"session_id":"%s","transcript_path":"%s","cwd":"%s","hook_event_name":"SessionEnd","reason":"%s"}\n' \
    "${1:?sid}" "${2:?transcript}" "${3:?cwd}" "${4:-other}"
}

# ------------------------------------------------------------------ probes --

# jt_stray_processes — jhh3-mem processes that are NOT part of this test run.
# Excludes: our own ancestor chain, the test tree itself, any `claude
# --plugin-dir .../jhh3-mem` (the plugin path is in claude's own argv), and
# pgrep. Anything left over is a genuine leaked process, which this plugin must
# never have.
jt_stray_processes() {
  command -v pgrep >/dev/null 2>&1 || return 0
  local ancestors="" pid=$$ ppid
  while [ "$pid" -gt 1 ] 2>/dev/null; do
    ancestors="$ancestors $pid"
    ppid="$(ps -o ppid= -p "$pid" 2>/dev/null | tr -d ' ')"
    [ -n "$ppid" ] && [ "$ppid" != "$pid" ] || break
    pid="$ppid"
  done
  # `pgrep -f` matches the string ANYWHERE in a command line, which is not the
  # same question as "is this process running jhh3-mem". Any shell whose cwd or
  # script text merely mentions the name matches -- including the test runner's
  # own parent, a sibling editor, or a CI wrapper. Ancestor exclusion alone does
  # not cover those, and a false positive here reads as "the plugin left a daemon
  # running", which is the single most serious claim this suite makes.
  #
  # So decide on the ARGV, not on a substring: drop a leading interpreter and its
  # flags, then require the program actually being executed to be jhh3-mem.
  pgrep -fa jhh3-mem 2>/dev/null | while IFS= read -r line; do
    local lpid="${line%% *}" argv prog
    case " $ancestors " in *" $lpid "*) continue ;; esac
    case "$line" in
      *"--plugin-dir"* | *"/tests/"* | *pgrep*) continue ;;
    esac

    argv="${line#* }"
    # Strip `bash`/`sh`/`zsh` and any following options, so `bash /x/bin/jhh3-mem`
    # is still judged on `/x/bin/jhh3-mem`.
    case "$argv" in
      */bash\ * | */sh\ * | */zsh\ * | bash\ * | sh\ * | zsh\ *)
        argv="${argv#* }"
        while :; do
          case "$argv" in -*) argv="${argv#* }" ;; *) break ;; esac
        done
        ;;
    esac
    prog="${argv%% *}"
    case "$prog" in
      jhh3-mem | */jhh3-mem) printf '%s\n' "$line" ;;
    esac
  done
}
