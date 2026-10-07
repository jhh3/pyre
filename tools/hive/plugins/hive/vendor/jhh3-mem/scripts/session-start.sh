#!/usr/bin/env bash
# jhh3-mem — scripts/session-start.sh
# SessionStart hook GUARD.  SPEC §7.2, §7.3, §7.5, §8.1.
#
# CONTRACT, ABSOLUTELY:
#   stdout    the digest block (≤ JM_INJECT_MAX bytes) or NOTHING
#   stderr    always empty — every diagnostic is redirected into $JM_LOG
#   exit      always 0
#
# This is a GUARD, not a worker: `set -u` only (no `-e`), one trap that logs and
# exits 0, and the real work delegated to lib/inject.sh under a hard wall clock.
# The two requirements "set -euo pipefail-clean" and "must always exit 0" fight;
# this split is the resolution (SPEC §7.5).
#
# ORDER IS LOAD-BEARING: the digest is printed FIRST, the backfill sweep runs
# after.  A wedged sweep must never cost the user their digest.
#
# The worker writes to a temp file and this guard cats it only on a clean exit,
# so a SIGKILL mid-write cannot put a half-line of memory into the model's
# context.  The guard then re-checks the byte count itself: the cap is asserted
# twice, by two different pieces of code, on the boundary that matters.

set -u

PLUGIN_ROOT="$(cd -- "${BASH_SOURCE[0]%/*}/.." && pwd -P)"
# shellcheck source=../lib/common.sh
. "$PLUGIN_ROOT/lib/common.sh" 2>/dev/null || exit 0

# --- do not inject into the plugin's own model calls -------------------------
# The counterpart of the guard in session-end.sh. `jhh3-mem enrich` shells out to
# `claude -p`, and that call is a session: without this it would be handed a digest
# of unrelated past work, which both wastes its context and biases the summary it
# is being asked to write about a different session entirely.
if [ "${JHH3_MEM_NO_CAPTURE:-0}" = '1' ]; then
  jm_log session_start - noop 'reason=self_call' 2>/dev/null || true
  exit 0
fi

started_ns="$(jm_now_ns)"
stage=''
payload=''

# --- pick a sink for the worker's output ------------------------------------
# EXACTLY the guard session-end.sh has carried since the start, and its absence
# here was an oversight, not a decision. SessionStart deliberately never
# bootstraps the store, so on a genuinely fresh install (and after every
# `uninstall`) the very first SessionStart is GUARANTEED to find no $JM_HOME —
# the `>>"$JM_LOG"` redirect then fails, bash prints "No such file or
# directory" naming an internal path on this hook's stderr, which Claude Code
# surfaces in the user's terminal (P92), and — worse — bash never runs the
# command, returns 1, and the `1)` arm below reads that as "the worker had
# nothing to say", silently costing the user their digest forever.
#
# jm_init_store only creates the 0700 directory; it does not create a database
# and does not run DDL, so the "SessionStart never bootstraps" rule is intact.
jm_init_store 2>/dev/null || true
logsink="$JM_LOG"
if ! { : >>"$JM_LOG"; } 2>/dev/null; then
  logsink=/dev/null
fi

# shellcheck disable=SC2329  # invoked by the EXIT/INT/TERM trap below
cleanup() {
  [ -n "$payload" ] && rm -f -- "$payload" 2>/dev/null
  [ -n "$stage" ] && rm -rf -- "$stage" 2>/dev/null
  return 0
}
trap 'cleanup; exit 0' EXIT INT TERM

# 1. Drain stdin.  A hook that does not drain stdin can wedge the writer.
payload="$(jm_payload_read)" || {
  jm_log session_start error "reason=no_stdin"
  exit 0
}

stage="$(jm_mkstage)" || stage=''
out=''
if [ -n "$stage" ]; then out="$stage/digest.txt"; fi

# 2. The digest, under its own sub-budget.  stderr goes to the log, never to
#    the terminal.
#
#    BUDGET DEVIATION, MEASURED, DELIBERATE.  SPEC §7.2 sets this to 2 s.  On
#    this host, against the real 78 MB migrated store in acme/webapp, a
#    WARM run costs 140 ms — but the first two runs after the store was written
#    both hit 2 s and were killed, and only the third (fully warm) completed:
#
#      session_start degraded reason=wall_timeout ms=2009
#      session_start degraded reason=wall_timeout ms=2024
#      session_start noop     reason=no_rows ... ms=134
#
#    Faulting a 78 MB WAL database into page cache on WSL2 costs seconds of
#    real I/O, so a 2 s digest budget silently drops the digest on exactly the
#    case that matters most: the first session after a reboot.  4 s here, and
#    the backfill below is given whatever remains of a 6 s TOTAL — so the
#    aggregate worst case is still the 6 s internal cap §7.2 specifies, and
#    still 4 s inside the 10 s declared hook timeout.
# Stock macOS has no coreutils `timeout`: run unwrapped there (the declared hook
# timeout still bounds the hook) instead of failing every SessionStart with 127.
JM_TIMEOUT=(timeout -k 1)
if ! command -v timeout >/dev/null 2>&1; then
  if command -v gtimeout >/dev/null 2>&1; then JM_TIMEOUT=(gtimeout -k 1); else JM_TIMEOUT=(_jm_no_timeout); fi
fi
_jm_no_timeout() { shift; "$@"; }
rc=0
budget_deadline=$(( $(jm_now_epoch) + 6 ))
if [ -n "$out" ]; then
  ${JM_TIMEOUT[@]+"${JM_TIMEOUT[@]}"} 4 bash "$PLUGIN_ROOT/lib/inject.sh" --payload "$payload" --out "$out" \
    >>"$logsink" 2>&1 || rc=$?
else
  rc=1
fi

case "$rc" in
  0)
    if [ -s "$out" ]; then
      sz="$(LC_ALL=C wc -c <"$out" 2>/dev/null)" || sz=0
      case "$sz" in '' | *[!0-9\ ]*) sz=0 ;; esac
      sz="${sz// /}"
      if [ "$sz" -le "$JM_INJECT_MAX" ] 2>/dev/null; then
        cat -- "$out"
      else
        # Unreachable by construction; if it ever fires, the digest is dropped
        # rather than trimmed here, because a guard that edits the payload is
        # the class of behaviour this plugin exists to not have.
        jm_log session_start error "reason=guard_overflow bytes=$sz cap=$JM_INJECT_MAX"
      fi
    fi
    ;;
  1) : ;;                                    # worker had nothing to say; it logged why
  124 | 137)
    jm_log session_start degraded "reason=wall_timeout ms=$(jm_elapsed_ms "$started_ns")"
    ;;
  *)
    jm_log session_start error "rc=$rc ms=$(jm_elapsed_ms "$started_ns")"
    ;;
esac

# 3. Backfill sweep — AFTER the output, never before (SPEC §7.3).
#    Owned by lib/capture.sh (E2).  Absent, unreadable, or slow: skipped in
#    silence.  Its stdout is redirected into the log because SessionStart stdout
#    belongs to the digest alone.
if [ "${JHH3_MEM_BACKFILL:-1}" != '0' ] && [ -r "$PLUGIN_ROOT/lib/capture.sh" ]; then
  parsed="$(jm_payload_parse "$payload" 2>/dev/null)" || parsed=''
  if [ -n "$parsed" ]; then
    tpath="$(printf '%s' "$parsed" | LC_ALL=C cut -f2)"
    src="$(printf '%s' "$parsed" | LC_ALL=C cut -f5)"
    case "$src" in
      clear | compact) tpath='' ;;           # mid-session; nothing has ended
    esac
    if [ -n "$tpath" ]; then
      tdir="${tpath%/*}"
      if [ -n "$tdir" ] && [ -d "$tdir" ]; then
        # Whatever is left of the 6 s total, capped at the spec's 3 s.
        left=$(( budget_deadline - $(jm_now_epoch) ))
        [ "$left" -gt 3 ] && left=3
        deadline=$(( $(jm_now_epoch) + left ))
      fi
      if [ -n "$tdir" ] && [ -d "$tdir" ] && [ "${left:-0}" -ge 1 ]; then
        # shellcheck disable=SC2016  # $1..$3 are the inner shell's args, by design
        ${JM_TIMEOUT[@]+"${JM_TIMEOUT[@]}"} "$left" bash -c '
          set -u
          . "$1/lib/common.sh" || exit 0
          . "$1/lib/capture.sh" || exit 0
          command -v jm_capture_backfill >/dev/null 2>&1 || exit 0
          jm_capture_backfill "$2" 3 "$3" >/dev/null 2>&1 || exit 0
        ' _ "$PLUGIN_ROOT" "$tdir" "$deadline" >>"$logsink" 2>&1 || true
      fi
    fi
  fi
fi

exit 0
