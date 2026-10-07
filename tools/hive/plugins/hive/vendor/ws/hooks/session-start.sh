#!/bin/sh
# ws SessionStart hook: inject stream context (or HQ brief, or the register nudge).
DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
exec python3 "$DIR/../bin/ws" hook session-start
