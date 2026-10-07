#!/usr/bin/env bash
# Keystroke-to-echo latency to a remote node: native `ssh -t HOST tmux
# attach` vs the Pyre TUI embedding the same session through the same
# ssh_tmux command. The remote side is a private tmux server (tmux -L
# pyre-wlat-r-$$) running latapp.py in a temp dir, removed on exit. Light:
# one python process and one tmux server on HOST. Usage:
#   tests/latency/run_remote.sh HOST [samples] [go binary | prebuilt harness]
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
root="$(cd "$here/../.." && pwd)"
host="$1"
n="${2:-200}"
go="${3:-go}"
sock="pyre-wlat-r-$$"
rdir="/tmp/pyre-wlat-$$"
tmpd="$(mktemp -d)"
if [ -x "$go" ] && [ "$(basename "$go")" != go ]; then
  bin="$go"
else
  bin="$tmpd/latency"
  "$go" -C "$root" build -o "$bin" ./tests/latency
fi
cleanup() {
  ssh -o BatchMode=yes "$host" tmux -L "$sock" kill-server 2>/dev/null || true
  ssh -o BatchMode=yes "$host" rm -rf "$rdir" || true
  rm -rf "$tmpd"
}
trap cleanup EXIT
ssh -o BatchMode=yes "$host" mkdir -p "$rdir"
scp -q "$here/latapp.py" "$host:$rdir/latapp.py"
for app in echo ink; do
  for mode in native pyre; do
    ssh -o BatchMode=yes "$host" tmux -L "$sock" -f /dev/null new-session -d -s lat -x 160 -y 44 "python3 $rdir/latapp.py $app"
    ssh -o BatchMode=yes "$host" tmux -L "$sock" set -g status off >/dev/null
    "$bin" -mode "$mode" -n "$n" -label "$host $mode ($app)" -cmd "ssh -t $host tmux -u -L $sock attach-session -t lat"
    ssh -o BatchMode=yes "$host" tmux -L "$sock" kill-server 2>/dev/null || true
  done
done
