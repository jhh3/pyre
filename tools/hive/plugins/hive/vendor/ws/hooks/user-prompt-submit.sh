#!/bin/sh
# ws UserPromptSubmit hook: deliver unread inbox mail + channel activity.
# Pure-shell fast path: skip python entirely when nothing is waiting anywhere.
WS_ROOT="${WS_ROOT:-$HOME/streams}"
for f in "$WS_ROOT"/inbox/*/*.md "$WS_ROOT"/inbox/*/.chan-pending; do
  if [ -e "$f" ]; then
    DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
    exec python3 "$DIR/../bin/ws" hook user-prompt-submit
  fi
done
exit 0
