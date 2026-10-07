#!/bin/bash
# Run every synthetic experiment (e1-e8) on this Mac and write transcripts to docs/evidence/accounts/raw.
# Optional: LINUX_TRANSCRIPTS="file..." copies Linux transcripts (from run-linux*.sh) alongside.
set -u
D="$(CDPATH= cd -- "$(dirname "$0")" >/dev/null && pwd)"
EV="$D/../../docs/evidence/accounts/raw"; mkdir -p "$EV"
NOISE='codex_core_plugins|rmcp::|codex_rmcp_client|plugin catalog|featured plugin'
for e in e1-claude-isolation e2-claude-refresh e2r-claude-shared-home-race e3-claude-envtoken-resume \
         e5-codex-isolation e6-claude-interactive-tmux e7-claude-token-exposure e8-claude-live-swap; do
  echo "== $e"
  { echo "# $e on $(uname -sm) claude=$(claude --version 2>&1) codex=$(codex --version 2>&1)"
    SPIKE_ROOT="${TMPDIR:-/tmp}/pyre-wp02-$e" bash "$D/experiments/$e.sh" 2>&1; } \
    | grep -Ev "$NOISE" | sed "s#$HOME#~#g" > "$EV/mac-$e.txt"
done
for f in ${LINUX_TRANSCRIPTS:-}; do
  grep -Ev "$NOISE" "$f" | sed "s#/home/$USER#~#g" > "$EV/linux-$(basename "$f")"
done
ls -la "$EV"
