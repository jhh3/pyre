#!/bin/bash
# Test double for launchd/systemd, used by rehearse-install.sh and the
# real-host release update test through PYRE_UPGRADE_RESTART_HOOK. It starts
# exactly the command line and environment of the rendered unit in a clean
# environment (env -i), as the service manager would, and records the pid.
# It never calls launchctl or systemctl.
#
#   rehearsal-supervisor.sh start|stop|restart|status node|hub
#
# Units: <home>/Library/LaunchAgents/dev.jhh3.pyre.{pyred,hub}.plist
# (ProgramArguments, EnvironmentVariables) or
# <home>/.config/systemd/user/{pyred,pyre-hub}.service (ExecStart=,
# Environment=; systemd quoting, %% and $$ unescaped). A systemd user manager
# also provides HOME, USER and LOGNAME; this double sets them too.
#
# Environment: PYRE_REHEARSAL_HOME (the fake home holding the units; never the
# real home), PYRE_REHEARSAL_RUN (directory for pid files and, for systemd
# units, the service log), optional PYRE_REHEARSAL_SERVICE_HOME (HOME given to
# the service; default: the plist's HOME, or the fake home for systemd units),
# optional PYRE_REHEARSAL_PASS_ENV (space-separated variable names whose
# current values are added to the service environment, e.g. the test's
# PYRE_RELEASE_ALLOWED_SIGNERS; a real unit would carry them as Environment=).
set -euo pipefail
cmd=${1:?start|stop|restart|status}
svc=${2:-node}
FH=${PYRE_REHEARSAL_HOME:?}
RUN=${PYRE_REHEARSAL_RUN:?}
case "$FH" in "$HOME"|"$HOME/") echo "refusing: PYRE_REHEARSAL_HOME is the real home" >&2; exit 2 ;; esac
label=dev.jhh3.pyre.pyred unit=pyred.service
if [ "$svc" = hub ]; then label=dev.jhh3.pyre.hub unit=pyre-hub.service; fi
plist="$FH/Library/LaunchAgents/$label.plist"
sdunit="$FH/.config/systemd/user/$unit"
pidf="$RUN/$svc.pid"
mkdir -p "$RUN"

# unit_fields: NUL-separated "A" args, "E" env entries and "L" the log path.
unit_fields() {
  if [ -f "$plist" ]; then
    plutil -convert json -o - "$plist" | python3 -c '
import json, sys
d = json.load(sys.stdin)
out = ["A" + a for a in d["ProgramArguments"]]
out += ["E%s=%s" % kv for kv in d.get("EnvironmentVariables", {}).items()]
out.append("L" + d["StandardErrorPath"])
sys.stdout.write("".join(x + "\0" for x in out))'
  elif [ -f "$sdunit" ]; then
    python3 - "$sdunit" "$RUN/$svc.log" <<'PY'
import shlex, sys
unit, log = sys.argv[1], sys.argv[2]
def unesc(w):
    return w.replace("%%", "\x00").replace("$$", "$").replace("\x00", "%")
args, env = [], []
for line in open(unit):
    line = line.rstrip("\n")
    if line.startswith("ExecStart="):
        args = [unesc(w) for w in shlex.split(line[len("ExecStart="):])]
    elif line.startswith("Environment="):
        env += [unesc(w) for w in shlex.split(line[len("Environment="):])]
out = ["A" + a for a in args] + ["E" + e for e in env] + ["L" + log]
sys.stdout.write("".join(x + "\0" for x in out))
PY
  else
    echo "no unit for $svc under $FH" >&2
    return 1
  fi
}

start() {
  local args=() envs=() log="" f
  while IFS= read -r -d '' f; do
    case "$f" in
      A*) args+=("${f#A}") ;;
      E*) envs+=("${f#E}") ;;
      L*) log=${f#L} ;;
    esac
  done < <(unit_fields)
  [ "${#args[@]}" -gt 0 ] || { echo "unit for $svc has no command" >&2; exit 1; }
  if [ -f "$sdunit" ] && [ ! -f "$plist" ]; then
    envs=("HOME=${PYRE_REHEARSAL_SERVICE_HOME:-$FH}" "USER=$(id -un)" "LOGNAME=$(id -un)" ${envs[@]+"${envs[@]}"})
  elif [ -n "${PYRE_REHEARSAL_SERVICE_HOME:-}" ]; then
    envs+=("HOME=$PYRE_REHEARSAL_SERVICE_HOME" "USER=$(id -un)" "LOGNAME=$(id -un)")
  fi
  for v in ${PYRE_REHEARSAL_PASS_ENV:-}; do
    if [ -n "${!v:-}" ]; then envs+=("$v=${!v}"); fi
  done
  mkdir -p "$(dirname "$log")"
  echo "start $svc: ${args[*]}"
  env -i ${envs[@]+"${envs[@]}"} "${args[@]}" >>"$log" 2>&1 </dev/null &
  echo $! >"$pidf"
}

stop() {
  [ -f "$pidf" ] || return 0
  pid=$(cat "$pidf")
  if kill "$pid" 2>/dev/null; then
    for _ in $(seq 1 200); do kill -0 "$pid" 2>/dev/null || break; sleep 0.1; done
  fi
  rm -f "$pidf"
  echo "stopped $svc ($pid)"
}

case "$cmd" in
  start) start ;;
  stop) stop ;;
  restart) stop; start ;;
  status) if [ -f "$pidf" ] && kill -0 "$(cat "$pidf")" 2>/dev/null; then echo "$svc running ($(cat "$pidf"))"; else echo "$svc stopped"; exit 3; fi ;;
  *) echo "usage: $0 start|stop|restart|status node|hub" >&2; exit 2 ;;
esac
