#!/usr/bin/env bash
# hive hook harness — every hook enters through here.
#
# Contract (the uninstall-trigger firewall):
#   * ALWAYS exit 0. A hive failure must never block or dirty a live session.
#   * Hard wall-clock timeout per event (defaults below).
#   * stderr -> ~/.local/state/hive/hooks.log, never the session.
#   * stdout passes through (it may carry additionalContext JSON).
#   * HIVE_DISABLE=1 short-circuits everything.
#   * Circuit breaker: 3 consecutive failures of one event auto-disables that
#     event (breaker file); a single loud one-line notice is emitted once.
#
# Usage: hook-harness.sh <event> [timeout_seconds]
set -u

EVENT="${1:-unknown}"
STATE_DIR="${HIVE_STATE_DIR:-$HOME/.local/state/hive}"
LOG="$STATE_DIR/hooks.log"
BREAKER="$STATE_DIR/breaker.$EVENT"
PLUGIN_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

mkdir -p "$STATE_DIR" 2>/dev/null || exit 0

[ "${HIVE_DISABLE:-0}" != "0" ] && { cat >/dev/null; exit 0; }
[ "${HIVE_NO_CAPTURE:-0}" != "0" ] && { cat >/dev/null; exit 0; }

# Breaker tripped?
if [ -f "$BREAKER" ]; then
  count=$(head -c 8 "$BREAKER" 2>/dev/null || echo 0)
  if [ "${count:-0}" -ge 3 ] 2>/dev/null; then
    if [ ! -f "$BREAKER.notified" ]; then
      echo '{"systemMessage": "hive: '"$EVENT"' hook degraded (breaker tripped) - run `hive doctor`"}'
      touch "$BREAKER.notified" 2>/dev/null
    fi
    cat >/dev/null
    exit 0
  fi
fi

case "$EVENT" in
  SessionStart) TMO=6 ;;
  Stop) TMO=3 ;;
  PreCompact) TMO=3 ;;
  SessionEnd) TMO=8 ;;
  *) TMO=3 ;;
esac
TMO="${2:-$TMO}"

# Portable helpers: stock macOS has no coreutils `timeout`, no `stat -c`, no
# `date -Is`, and (before macOS 13) no `date +%N`.
_now_ns() {
  local n
  n=$(date +%s%N 2>/dev/null)
  case "$n" in (*[!0-9]*|"") n="$(date +%s)000000000" ;; esac
  printf '%s\n' "$n"
}
_stamp() { date +%Y-%m-%dT%H:%M:%S%z; }
if command -v timeout >/dev/null 2>&1; then TIMEOUT=(timeout "$TMO")
elif command -v gtimeout >/dev/null 2>&1; then TIMEOUT=(gtimeout "$TMO")
else TIMEOUT=()  # the harness's declared hook timeout still bounds the run
fi

# Run the python hook handler with stdin passed through; stderr to log.
t0=$(_now_ns)
${TIMEOUT[@]+"${TIMEOUT[@]}"} python3 "$PLUGIN_DIR/lib/hook_entry.py" "$EVENT" 2>>"$LOG"
rc=$?
t1=$(_now_ns)

# Latency telemetry: one line per handler run (separate from hooks.log).
METRICS="$STATE_DIR/hook-metrics.log"
ms=$(( (t1 - t0) / 1000000 ))
size=$(stat -c %s "$METRICS" 2>/dev/null || stat -f %z "$METRICS" 2>/dev/null || echo 0)
case "$size" in (*[!0-9]*|"") size=0 ;; esac
[ "$size" -gt 1048576 ] && mv -f "$METRICS" "$METRICS.1" 2>/dev/null
status=ok
[ "$rc" -ne 0 ] && status=fail
echo "$(_stamp) [$EVENT] $status rc=$rc ms=$ms" >> "$METRICS" 2>/dev/null

if [ "$rc" -eq 0 ]; then
  rm -f "$BREAKER" "$BREAKER.notified" 2>/dev/null
else
  count=$(head -c 8 "$BREAKER" 2>/dev/null || echo 0)
  case "$count" in (*[!0-9]*|"") count=0 ;; esac
  echo $((count + 1)) > "$BREAKER" 2>/dev/null
  echo "$(_stamp) [$EVENT] handler rc=$rc (breaker $((count + 1))/3)" >> "$LOG" 2>/dev/null
fi

exit 0
