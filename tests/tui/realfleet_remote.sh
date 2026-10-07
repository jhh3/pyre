#!/bin/bash
# Runs ON the remote test host (copied there by realfleet.sh). Everything
# lives under $PYRE_RF_DIR (default ~/.cache/pyre-wtuireal); never installs
# services, never touches the user's Pyre config or default tmux server.
#   realfleet_remote.sh node-enroll <hub_url> <pin> <node_id>   (secret on stdin)
#   realfleet_remote.sh node-start <name> [pyred flags...]
#   realfleet_remote.sh node-stop | node-kill | node-pause | node-resume
#   realfleet_remote.sh kill-tmux | panes | logs [n] | journal
set -uo pipefail
R=${PYRE_RF_DIR:-$HOME/.cache/pyre-wtuireal}
BIN=$R/bin
mkdir -p "$R/logs" "$R/work"
TMUX_SOCK=$R/node/tmux/pyre.sock

findtmux() {
  for t in /usr/bin/tmux /opt/homebrew/bin/tmux "$HOME/homebrew/bin/tmux" /usr/local/bin/tmux; do
    if [ -x "$t" ]; then echo "$t"; return; fi
  done
  command -v tmux
}
pidf() { if [ -f "$R/$1.pid" ]; then cat "$R/$1.pid"; fi; }
alive() { [ -n "${1:-}" ] && kill -0 "$1" 2>/dev/null; }

cmd=${1:-}
shift || true
case "$cmd" in
  node-enroll)
    mkdir -p "$R/node"
    if [ -f "$R/node/node-credential.json" ]; then echo "already enrolled"; exit 0; fi
    "$BIN/pyred" enroll --hub "$1" --pin "$2" --node-id "$3" --credential-file "$R/node/node-credential.json" --secret-stdin >/dev/null
    echo "enrolled $3"
    ;;
  node-start)
    NAME=$1; shift
    if alive "$(pidf node)"; then echo "node already running pid $(pidf node)"; exit 0; fi
    # Hive is pointed at a path that does not exist so the hub's notes are
    # never bridged into the owner's real vault; profiles stay isolated.
    PYRE_HIVE_BIN=/nonexistent-pyre-rf/hive PYRE_PROFILE_ROOT=$R/profiles nohup "$BIN/pyred" run --state "$R/node" \
      --credential "$R/node/node-credential.json" --name "$NAME" "$@" >>"$R/logs/node.stderr" 2>&1 </dev/null &
    echo $! >"$R/node.pid"
    sleep 2
    if alive "$(pidf node)"; then echo "node started pid $(pidf node)"; else echo "node FAILED"; tail -5 "$R/logs/node.stderr"; exit 1; fi
    ;;
  node-stop|node-kill)
    p=$(pidf node)
    sig=TERM
    if [ "$cmd" = node-kill ]; then sig=KILL; fi
    if alive "$p"; then
      kill -CONT "$p" 2>/dev/null
      kill "-$sig" "$p"
      for _ in $(seq 1 50); do alive "$p" || break; sleep 0.1; done
      echo "stopped pid $p ($sig)"
    else echo "not running"; fi
    rm -f "$R/node.pid"
    ;;
  node-pause) kill -STOP "$(pidf node)" && echo "paused $(pidf node)" ;;
  node-resume) kill -CONT "$(pidf node)" && echo "resumed $(pidf node)" ;;
  kill-tmux)
    T=$(findtmux)
    if [ -S "$TMUX_SOCK" ]; then "$T" -S "$TMUX_SOCK" kill-server 2>/dev/null; echo "killed isolated tmux"; else echo "no isolated tmux"; fi
    ;;
  panes)
    T=$(findtmux)
    if [ -S "$TMUX_SOCK" ]; then
      "$T" -S "$TMUX_SOCK" list-panes -a -F '#{session_name} pane_pid=#{pane_pid} dead=#{pane_dead} cmd=#{pane_current_command}'
    else echo "no isolated tmux socket"; fi
    ;;
  logs) tail -n "${1:-30}" "$R/logs/node.stderr" ;;
  journal)
    find "$R/node" -name deliveries.jsonl -exec sh -c 'echo "== $1"; cat "$1"' _ {} \;
    ;;
  *) echo "unknown command $cmd"; exit 2 ;;
esac
