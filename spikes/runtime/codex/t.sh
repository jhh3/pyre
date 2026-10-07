#!/usr/bin/env bash
# Isolated tmux wrapper for the Pyre Codex spike. Never touches the default tmux server.
exec tmux -S "${PYRE_TMUX_SOCK:-/tmp/pyre-wp01/codex/tmux.sock}" "$@"
