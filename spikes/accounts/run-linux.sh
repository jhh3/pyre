#!/bin/bash
# Run the Claude experiments on a Linux host from a copy of spikes/accounts at
# ~/.cache/pyre-spike-accounts/accounts. Scratch state stays under ~/.cache/pyre-spike-accounts/runs.
set -u
BASE="$HOME/.cache/pyre-spike-accounts"
D="$BASE/accounts"
OUT="$BASE/runs"; mkdir -p "$OUT"
echo "host: $(uname -srm)"; echo "claude: $(claude --version 2>&1)"
echo "default creds before: $(stat -c '%n size=%s mtime=%Y' "$HOME/.claude/.credentials.json" 2>&1 | sed "s#$HOME#~#")"
for e in e1-claude-isolation e2-claude-refresh e3-claude-envtoken-resume; do
  echo; echo "################ $e"
  SPIKE_ROOT="$OUT/$e" bash "$D/experiments/$e.sh" 2>&1
done
echo "default creds after: $(stat -c '%n size=%s mtime=%Y' "$HOME/.claude/.credentials.json" 2>&1 | sed "s#$HOME#~#")"
echo "secret-tool/libsecret present: $(command -v secret-tool || echo no)"
