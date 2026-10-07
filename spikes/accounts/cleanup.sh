#!/bin/bash
# Remove WP02 scratch state on this host (synthetic homes, logs, throwaway CA, isolated codex)
# and stop any leftover fake providers / isolated tmux servers started by the experiments.
set -u
pkill -f "spikes/accounts/fakeprovider.py" 2>/dev/null
pkill -f "pyre-spike-accounts/accounts/fakeprovider.py" 2>/dev/null
for s in /tmp/tmux-"$(id -u)"/pyre-wp02-* "${TMPDIR:-/tmp}"/tmux-"$(id -u)"/pyre-wp02-*; do
  [ -S "$s" ] && tmux -S "$s" kill-server 2>/dev/null
done
rm -rf "${TMPDIR:-/tmp}"/pyre-wp02-* "$HOME/.cache/pyre-spike-accounts" 2>/dev/null
echo "remaining fake providers: $(pgrep -f fakeprovider.py | wc -l | tr -d ' ')"
