#!/bin/sh
# ws Stop hook: block idling when unread inbox mail, channel activity, or an
# owed reply (pending/) exists. Delivery marks mail read and replying clears
# the obligation, so a re-stop can never loop. Same fast path as post-tool-use.
WS_ROOT="${WS_ROOT:-$HOME/streams}"
for f in "$WS_ROOT"/inbox/*/*.md "$WS_ROOT"/inbox/*/.chan-pending \
         "$WS_ROOT"/inbox/*/pending/*.md; do
  if [ -e "$f" ]; then
    DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
    exec python3 "$DIR/../bin/ws" hook stop
  fi
done
exit 0
