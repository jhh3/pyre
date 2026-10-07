#!/usr/bin/env bash
# Keystroke-to-echo latency, same machine: native `tmux attach` vs the Pyre
# TUI with the same session embedded and focused. Uses a private tmux
# server (tmux -L pyre-wlat-$$), killed on exit. Usage:
#   tests/latency/run_local.sh [samples] [go binary]
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
root="$(cd "$here/../.." && pwd)"
n="${1:-200}"
go="${2:-go}"
sock="pyre-wlat-$$"
py="$(command -v python3)"
[ -x /usr/bin/python3 ] && py=/usr/bin/python3
bin="$(mktemp -d)/latency"
"$go" -C "$root" build -o "$bin" ./tests/latency
cleanup() {
  tmux -L "$sock" kill-server 2>/dev/null || true
  rm -rf "$(dirname "$bin")"
}
trap cleanup EXIT
for app in echo ink; do
  for mode in native pyre; do
    tmux -L "$sock" -f /dev/null new-session -d -s lat -x 160 -y 44 "$py $here/latapp.py $app"
    tmux -L "$sock" set -g status off >/dev/null
    tmux -L "$sock" set -g window-size latest >/dev/null
    "$bin" -mode "$mode" -n "$n" -label "local $mode ($app)" -cmd "tmux -u -L $sock attach-session -t lat"
    tmux -L "$sock" kill-server 2>/dev/null || true
    sleep 0.3
  done
done
