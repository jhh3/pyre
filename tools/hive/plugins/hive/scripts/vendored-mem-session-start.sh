#!/usr/bin/env bash
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export CLAUDE_PLUGIN_ROOT="$HERE/vendor/jhh3-mem"
exec bash "$CLAUDE_PLUGIN_ROOT/scripts/session-start.sh"
