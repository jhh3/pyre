#!/usr/bin/env bash
# hive launcher shim — written to ~/.local/bin/hive by `hive install`.
# Resolves the newest installed plugin copy by version (plugin auto-update
# deletes old version dirs), falling back to the dev checkout.
# HIVE_DEV=1 inverts the order: run the dev checkout without the
# version-bump + marketplace-update dance while iterating.
set -u
dev="$HOME/code/hive/plugins/hive/bin/hive"
if [ "${HIVE_DEV:-0}" != "0" ] && [ -x "$dev" ]; then
  exec "$dev" "$@"
fi
best="$(ls -d "$HOME"/.claude/plugins/cache/*/hive/*/bin/hive 2>/dev/null | sort -V | tail -n 1)"
if [ -z "$best" ] || [ ! -x "$best" ]; then
  best="$dev"
fi
exec "$best" "$@"
