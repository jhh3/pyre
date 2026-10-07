#!/bin/sh
# ws PostToolUse hook: deliver unread inbox mail + channel activity mid-turn.
# Runs on EVERY tool call in EVERY session — the pure-shell fast path below
# must stay first so the common (nothing waiting anywhere) case never starts
# python. .chan-pending is the channel doorbell marker.
WS_ROOT="${WS_ROOT:-$HOME/streams}"
for f in "$WS_ROOT"/inbox/*/*.md "$WS_ROOT"/inbox/*/.chan-pending; do
  if [ -e "$f" ]; then
    DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
    exec python3 "$DIR/../bin/ws" hook post-tool-use
  fi
done
exit 0
