#!/bin/bash
# Run the Codex experiment on Linux with an isolated codex binary (tools/fetch-linux-codex.sh).
set -u
BASE="$HOME/.cache/pyre-spike-accounts"
export CODEX_BIN="$BASE/codex/package/vendor/x86_64-unknown-linux-musl/bin/codex"
echo "codex: $("$CODEX_BIN" --version)"
echo "default codex auth before: $(stat -c '%n size=%s mtime=%Y' "$HOME/.codex/auth.json" 2>&1 | sed "s#$HOME#~#")"
SPIKE_ROOT="$BASE/runs/e5-codex-isolation" bash "$BASE/accounts/experiments/e5-codex-isolation.sh" 2>&1
echo "default codex auth after: $(stat -c '%n size=%s mtime=%Y' "$HOME/.codex/auth.json" 2>&1 | sed "s#$HOME#~#")"
