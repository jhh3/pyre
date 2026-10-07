#!/usr/bin/env bash
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export CLAUDE_PLUGIN_ROOT="$HERE/vendor/ws"
exec bash "$CLAUDE_PLUGIN_ROOT/hooks/post-tool-use.sh"
