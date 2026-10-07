#!/usr/bin/env bash
# jhh3-mem — tests/t-lint.sh
#
# The brief requires the shell to be "set -euo pipefail-clean and
# clean under the linter". A claim nothing checks is not a claim, so this suite makes
# it self-verifying:
#
#   - every shell file parses under `bash -n` (always runs, no dependency)
#   - every shell file is shellcheck-clean at -S style (SKIPs, never PASSes,
#     when shellcheck is absent — a skipped check must never read as a green
#     one, which is the whole reason this suite exists)
set -uo pipefail

_t_src="${BASH_SOURCE[0]}"
case "$_t_src" in */*) _t_dir="${_t_src%/*}" ;; *) _t_dir="." ;; esac
# shellcheck source=./lib/harness.sh
. "$_t_dir/lib/harness.sh"

jt_begin t-lint

FILES=()
while IFS= read -r f; do
  [ -n "$f" ] || continue
  FILES+=("$f")
done < <(
  {
    printf '%s\n' "$JT_PLUGIN_ROOT/bin/jhh3-mem"
    # DISCOVER, do not enumerate. This was one glob per directory, so when
    # lib/adapters/ was added its files went unchecked and a shellcheck parse
    # error sat in an adapter while this suite still reported all green. A find
    # cannot go stale the next time a subdirectory appears.
    find "$JT_PLUGIN_ROOT" -type f -name '*.sh' 2>/dev/null
  } | LC_ALL=C sort -u
)

jt_case LINT-1 "every shell file parses"
for f in "${FILES[@]}"; do
  if bash -n "$f" 2>/dev/null; then
    jt_ok "bash -n ${f#"$JT_PLUGIN_ROOT"/}"
  else
    jt_fail "bash -n ${f#"$JT_PLUGIN_ROOT"/} -- $(bash -n "$f" 2>&1 | head -2)"
  fi
done

jt_case LINT-2 "every shell file is shellcheck-clean at -S style"
if ! jt_need_cmd shellcheck 'the "shellcheck-clean" claim cannot be verified without it'; then
  jt_end
fi
for f in "${FILES[@]}"; do
  # Excluded, each a deliberate idiom or a cross-file false positive:
  #   SC2034 exported vars (JM_SESSION_UID) whose use shellcheck cannot see across files
  #   SC2015 `[ x ] && y` used as a statement — the libraries' house style, not if-then-else
  #   SC2016 literal backticks inside single-quoted help/printf text, intentionally unexpanded
  #   SC2317 cleanup bodies reached only via `trap`, which shellcheck cannot follow
  out="$(shellcheck -S style -x -e SC2034,SC2015,SC2016,SC2317 \
    -P "$JT_PLUGIN_ROOT" -P "$JT_PLUGIN_ROOT/lib" \
    -P "$JT_PLUGIN_ROOT/tests" -P "$JT_PLUGIN_ROOT/tests/lib" "$f" 2>&1)"
  if [ -z "$out" ]; then
    jt_ok "shellcheck ${f#"$JT_PLUGIN_ROOT"/}"
  else
    jt_fail "shellcheck ${f#"$JT_PLUGIN_ROOT"/} -- $(printf '%s' "$out" | grep -c '^In ') finding(s): $(printf '%s' "$out" | grep -oE 'SC[0-9]+' | sort -u | tr '\n' ' ')"
  fi
done

jt_end
