#!/usr/bin/env bash
# jhh3-mem — scripts/session-end.sh
# SessionEnd hook GUARD. SPEC §7.4, §7.5.
#
# CONTRACT, ABSOLUTE
#   stdout      always empty  (SessionEnd output is not context; it is pure noise)
#   stderr      always empty  (Claude Code writes a failing hook's stderr into the
#                              user's terminal, P92 — a memory bug must never
#                              garble a coding session)
#   exit code   always 0
#
# `set -u` only. NOT `set -e`: the brief demands both "-euo pipefail clean" and
# "any hook that errors must exit 0", and those fight. The resolution (§7.5) is
# that guards are -u only with one trap, and the workers under lib/ carry the
# full -euo pipefail.
#
# The worker runs under `timeout -k 1 1`. That 1 s internal wall cap is NOT the
# declared 8 s hook timeout: SPEC §1.2 uncertainty 1 records that the observed
# per-hook kill deadline may be a hard 1500 ms floor regardless of the declared
# value, so capture must finish inside 1200 ms of real budget. Measured need is
# ~140 ms. DO NOT RAISE THIS WITHOUT RE-TESTING.

set -u

# --- resolve the plugin root without dirname/basename ------------------------
# `dirname` is an external binary; if PATH is stripped (T-FO-1) this script must
# still reach its own library. Parameter expansion has no such dependency.
_jm_self="${BASH_SOURCE[0]:-$0}"
case "$_jm_self" in
  */*) _jm_self_dir="${_jm_self%/*}" ;;
  *) _jm_self_dir="." ;;
esac
PLUGIN_ROOT="$(cd -- "$_jm_self_dir/.." 2>/dev/null && pwd -P)" || PLUGIN_ROOT="$_jm_self_dir/.."
unset _jm_self _jm_self_dir

# shellcheck source=../lib/common.sh
. "$PLUGIN_ROOT/lib/common.sh" 2>/dev/null || {
  # The library is the only thing that knows how to log. Without it there is
  # nothing safe to do but leave the session alone.
  exit 0
}

# --- do not capture the plugin's own model calls -----------------------------
# `jhh3-mem enrich` reaches a model with `claude -p`, and that call is itself a
# session: hooks fire, a row is written, and the new row is deterministic -- so it
# needs enriching, which makes another call, which writes another row. Nightly with
# a limit of 40 that was a slow leak; hourly with a limit of 500 it is a treadmill
# that bills a model call and an embedding for rows describing nothing but their
# own creation. Measured: one enrich-sized `claude -p` produced two rows.
#
# enrich exports this for the duration of the call, and the child process inherits
# it. Deliberately NOT a cwd or transcript heuristic: those misfire on real work
# done in the same directory.
if [ "${JHH3_MEM_NO_CAPTURE:-0}" = '1' ]; then
  jm_log session_end - noop 'reason=self_call' 2>/dev/null || true
  exit 0
fi

started_ns="$(jm_now_ns)"

# --- sweep staging artifacts a SIGKILL orphaned -----------------------------
# Normal runs clean up after themselves; a hook killed at its timeout does not,
# and what it leaves behind under $TMPDIR includes PRE-redaction transcript
# text. Nothing else would ever reap it, so over months it accumulates — which
# is structurally the same complaint as the predecessor's "uninstall left
# caches across six paths". Bounded (2 s, one directory, maxdepth 1), never
# touches anything younger than an hour, and can never fail this hook.
jm_reap_stale_stage

# --- drain stdin ------------------------------------------------------------
# A hook that does not drain stdin can wedge the writer.
payload=''
payload="$(jm_payload_read)" || {
  jm_log session_end error "reason=no_stdin"
  exit 0
}
# shellcheck disable=SC2064  # $payload must expand now, not at trap time
trap "rm -f -- '$payload'" EXIT INT TERM

# Best-effort session id for this guard's own log lines only. Never used as data:
# jm_log strips separators and caps it at 36 characters.
JM_SESSION_UID="$(
  LC_ALL=C sed -n 's/.*"session_id"[[:space:]]*:[[:space:]]*"\([0-9A-Za-z_-]\{1,36\}\)".*/\1/p' \
    "$payload" 2>/dev/null | head -n 1
)"
export JM_SESSION_UID

# --- pick a sink for the worker's output ------------------------------------
# The worker must never write to this hook's stdout or stderr. If $JM_LOG itself
# cannot be opened, the redirect would fail and bash would print to stderr — so
# fall back to /dev/null and let jm_log emit its single permitted stderr line.
logsink="$JM_LOG"
if ! { : >>"$JM_LOG"; } 2>/dev/null; then
  logsink=/dev/null
fi

# --- run the worker under a hard wall clock ---------------------------------
worker="$PLUGIN_ROOT/lib/capture.sh"
# Test seam (T-FO-9 / T-FO-10): only honoured for an existing readable file.
if [ -n "${JHH3_MEM_CAPTURE_WORKER:-}" ] && [ -f "${JHH3_MEM_CAPTURE_WORKER}" ]; then
  worker="$JHH3_MEM_CAPTURE_WORKER"
fi

wall="${JHH3_MEM_END_WALL:-1}"
case "$wall" in '' | *[!0-9]*) wall=1 ;; esac

# Run it as an async job and `wait` for it. This is not stylistic: when the worker
# dies by a signal, `timeout` re-raises that signal on itself, and for a
# FOREGROUND child a non-interactive bash announces "Killed …" on ITS OWN stderr —
# the one place this hook is never allowed to write (P92: Claude Code surfaces a
# hook's stderr in the user's terminal). Async jobs get no such announcement, and
# `wait` still yields 128+signal, so rc 137 is preserved for the log line.
# The job is not "backgrounded work": `wait` is the very next statement, and the
# plugin still owns no process past this line.
rc=0
if command -v timeout >/dev/null 2>&1; then
  timeout -k 1 "$wall" bash "$worker" "$payload" >>"$logsink" 2>&1 &
else
  # No coreutils `timeout`. The declared 8 s hook timeout still bounds us, and
  # Claude Code kills the hook's whole process group (P93), so this cannot leak.
  jm_log session_end degraded "reason=no_timeout_cmd"
  bash "$worker" "$payload" >>"$logsink" 2>&1 &
fi
wait "$!" 2>/dev/null
rc=$?

case "$rc" in
  0) : ;; # the worker logged its own ok/noop line
  124 | 137)
    jm_log session_end degraded "reason=wall_timeout rc=$rc ms=$(jm_elapsed_ms "$started_ns")"
    ;;
  1) : ;; # the worker logged its own error line before returning 1
  *)
    jm_log session_end error "rc=$rc ms=$(jm_elapsed_ms "$started_ns")"
    ;;
esac

exit 0
