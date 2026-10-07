#!/usr/bin/env bash
# jhh3-mem — tests/lib/e2-harness.sh
# Assertions + throwaway-store fixtures for the E2 (capture / redaction /
# fail-open / concurrency) suites. Plain bash: `bats` is absent on this host.
#
# Deliberately separate from tests/lib/harness.sh so the capture lane and the
# lifecycle lane never edit the same file.
#
# shellcheck shell=bash

JT_PASS=0
JT_FAIL=0
JT_SKIP=0
JT_SUITE="${JT_SUITE:-e2}"

JT_PLUGIN_ROOT="${JT_PLUGIN_ROOT:-}"
if [ -z "$JT_PLUGIN_ROOT" ]; then
  _jt_src="${BASH_SOURCE[0]}"
  case "$_jt_src" in */*) _jt_dir="${_jt_src%/*}" ;; *) _jt_dir="." ;; esac
  JT_PLUGIN_ROOT="$(cd -- "$_jt_dir/../.." && pwd -P)"
  unset _jt_src _jt_dir
fi
# shellcheck disable=SC2034  # consumed by the t-*.sh files that source this
JT_FIXTURES="$JT_PLUGIN_ROOT/tests/fixtures"
JT_CAPTURE="$JT_PLUGIN_ROOT/lib/capture.sh"
# shellcheck disable=SC2034  # consumed by the t-*.sh files that source this
JT_SESSION_END="$JT_PLUGIN_ROOT/scripts/session-end.sh"

# jt_ok NAME [DETAIL]
jt_ok() {
  JT_PASS=$((JT_PASS + 1))
  printf '  ok   %-10s %s\n' "$1" "${2:-}"
}

# jt_no NAME DETAIL
jt_no() {
  JT_FAIL=$((JT_FAIL + 1))
  printf '  FAIL %-10s %s\n' "$1" "${2:-}"
}

# jt_skip NAME WHY
jt_skip() {
  JT_SKIP=$((JT_SKIP + 1))
  printf '  skip %-10s %s\n' "$1" "${2:-}"
}

# jt_eq NAME EXPECTED ACTUAL [LABEL]
jt_eq() {
  if [ "$2" = "$3" ]; then
    jt_ok "$1" "${4:-} = $3"
  else
    jt_no "$1" "${4:-}: expected [$2] got [$3]"
  fi
}

# jt_ne NAME UNEXPECTED ACTUAL [LABEL]
jt_ne() {
  if [ "$2" != "$3" ]; then
    jt_ok "$1" "${4:-} != $2"
  else
    jt_no "$1" "${4:-}: expected anything but [$2]"
  fi
}

# jt_has NAME HAYSTACK NEEDLE [LABEL]
jt_has() {
  case "$2" in
    *"$3"*) jt_ok "$1" "${4:-} contains '$3'" ;;
    *) jt_no "$1" "${4:-}: '$3' not found in [${2:0:160}]" ;;
  esac
}

# jt_hasnt NAME HAYSTACK NEEDLE [LABEL]
jt_hasnt() {
  case "$2" in
    *"$3"*) jt_no "$1" "${4:-}: '$3' WAS found in [${2:0:160}]" ;;
    *) jt_ok "$1" "${4:-} free of '$3'" ;;
  esac
}

# jt_store — fresh isolated store; exports JHH3_MEM_HOME. Registers cleanup.
jt_store() {
  JT_HOME="$(mktemp -d "${TMPDIR:-/tmp}/jhh3-mem-test.XXXXXXXX")"
  export JHH3_MEM_HOME="$JT_HOME"
  JT_DB="$JT_HOME/memory.db"
  # shellcheck disable=SC2034  # consumed by the t-*.sh files that source this
  JT_LOG="$JT_HOME/jhh3-mem.log"
  JT_TMP="$(mktemp -d "${TMPDIR:-/tmp}/jhh3-mem-work.XXXXXXXX")"
  JT_CLEAN="${JT_CLEAN:-} $JT_HOME $JT_TMP"
}

# jt_cleanup — remove every store this run created.
jt_cleanup() {
  local d
  for d in ${JT_CLEAN:-}; do
    case "$d" in
      /tmp/* | "${TMPDIR:-/tmp}"/*) chmod -R u+rwX "$d" 2>/dev/null || true; rm -rf -- "$d" 2>/dev/null || true ;;
    esac
  done
}

# jt_sql SQL... — read the store DB. Never fails the caller.
jt_sql() {
  sqlite3 -batch -noheader "$JT_DB" "$@" 2>/dev/null || true
}

# jt_payload FILE SESSION TRANSCRIPT CWD REASON [TITLE] — a real SessionEnd payload.
jt_payload() {
  local f="$1" sid="$2" tp="$3" cwd="$4" reason="$5" title="${6:-}"
  {
    printf '{"session_id":"%s","transcript_path":"%s","cwd":"%s",' "$sid" "$tp" "$cwd"
    printf '"prompt_id":"e581968d-7a44-4b0e-9c31-2ab5f0d81c47",'
    printf '"hook_event_name":"SessionEnd","reason":"%s"' "$reason"
    [ -z "$title" ] || printf ',"session_title":"%s"' "$title"
    printf '}\n'
  } >"$f"
}

# jt_capture TRANSCRIPT SESSION CWD REASON [--dry-run] — run the worker directly.
jt_capture() {
  local tp="$1" sid="$2" cwd="$3" reason="$4"
  shift 4
  bash "$JT_CAPTURE" --transcript "$tp" --session "$sid" --cwd "$cwd" --reason "$reason" "$@"
}

# jt_summary — print totals, return 1 if anything failed.
jt_summary() {
  printf '%s: %d passed, %d failed, %d skipped\n' "$JT_SUITE" "$JT_PASS" "$JT_FAIL" "$JT_SKIP"
  [ "$JT_FAIL" -eq 0 ]
}
