#!/usr/bin/env bash
# t-packaging.sh — the manifests, the marketplace entry, and the repo
# conventions that have silently broken plugins in this repo before:
# misplaced files under .claude-plugin/, a bare marketplace `source`, a
# duplicated `version`, and a hook path that is not ${CLAUDE_PLUGIN_ROOT}-based.
#
# Covers SPEC UNI-11 (`claude plugin validate .` passes with no warnings) and
# the packaging half of SPEC 15.
set -uo pipefail

_here="${BASH_SOURCE[0]}"
case "$_here" in */*) _d="${_here%/*}" ;; *) _d="." ;; esac
# shellcheck source=lib/harness.sh
. "$_d/lib/harness.sh"

jt_begin t-packaging

MANIFEST="$JT_PLUGIN_ROOT/.claude-plugin/plugin.json"
MARKET="$JT_REPO_ROOT/.claude-plugin/marketplace.json"
HOOKS="$JT_PLUGIN_ROOT/hooks/hooks.json"

# ---------------------------------------------------------------------- PKG-1 --
jt_case PKG-1 "claude plugin validate . (repo root) — UNI-11"
if jt_need_cmd claude "the plugin validator ships with the Claude Code CLI"; then
  out="$(cd "$JT_REPO_ROOT" && claude plugin validate . 2>&1)"
  rc=$?
  jt_assert_rc "0" "$rc" "marketplace manifest validates"
  jt_assert_contains "$out" "Validation passed" "validator reports success"
  # UNI-11 requires no warnings, not merely a zero exit.
  warns="$(printf '%s\n' "$out" | grep -ci -E '\bwarn(ing)?\b')"
  jt_assert_eq "0" "$warns" "validator emitted no warnings"
fi

# ---------------------------------------------------------------------- PKG-2 --
jt_case PKG-2 "claude plugin validate <plugin dir>"
if jt_need "$MANIFEST" "E-manifests" && jt_need_cmd claude "plugin validator"; then
  out="$(cd "$JT_REPO_ROOT" && claude plugin validate ./plugins/jhh3-mem 2>&1)"
  rc=$?
  jt_assert_rc "0" "$rc" "plugin manifest validates"
  jt_assert_contains "$out" "Validation passed" "validator reports success"
  jt_assert_eq "0" "$(printf '%s\n' "$out" | grep -ci -E '\bwarn(ing)?\b')" \
    "validator emitted no warnings"
fi

# ---------------------------------------------------------------------- PKG-3 --
jt_case PKG-3 "only plugin.json lives in .claude-plugin/"
extra="$(find "$JT_PLUGIN_ROOT/.claude-plugin" -mindepth 1 -not -name 'plugin.json' 2>/dev/null | tr '\n' ' ')"
jt_assert_empty "$extra" "nothing but plugin.json under .claude-plugin/ (anything else silently breaks loading)"

# ---------------------------------------------------------------------- PKG-4 --
jt_case PKG-4 "components sit at the plugin root"
for d in hooks lib db; do
  jt_assert_file "$JT_PLUGIN_ROOT/$d" "$d/ is at the plugin root"
done
for d in bin scripts skills; do
  if [ -e "$JT_PLUGIN_ROOT/$d" ]; then
    jt_ok "$d/ is at the plugin root"
  else
    jt_pend "not implemented yet: $d/ (owner: $([ "$d" = skills ] && echo E-retrieval || echo E-core/E-capture))"
  fi
done

# ---------------------------------------------------------------------- PKG-5 --
# A hook whose script does not exist fails silently at session time. Resolve
# every ${CLAUDE_PLUGIN_ROOT} path in hooks.json against the real tree.
jt_case PKG-5 "every hook script path resolves"
if jt_need "$HOOKS" "E-manifests"; then
  # shellcheck disable=SC2016  # the literal string ${CLAUDE_PLUGIN_ROOT} is the pattern
  # The command is a single shell-parsed string with the path double-quoted inside
  # it (INV-9), so the JSON carries an escaped quote: bash \"${CLAUDE_PLUGIN_ROOT}/x.sh\".
  # Stop at the escaping backslash as well as the closing JSON quote.
  refs="$(grep -o '\${CLAUDE_PLUGIN_ROOT}[^"\\]*' "$HOOKS" | sort -u)"
  jt_assert_ne "" "$refs" "hooks.json references at least one script"
  while IFS= read -r ref; do
    [ -n "$ref" ] || continue
    real="$JT_PLUGIN_ROOT${ref#\$\{CLAUDE_PLUGIN_ROOT\}}"
    if [ -f "$real" ]; then
      jt_ok "hook target exists: ${ref#\$\{CLAUDE_PLUGIN_ROOT\}/}"
    else
      jt_pend "hook target not written yet: ${ref#\$\{CLAUDE_PLUGIN_ROOT\}/} (owner: E-capture / E-retrieval)"
    fi
  done <<<"$refs"
  # No path may be baked in relative or absolute — the plugin is copied to
  # ~/.claude/plugins/cache at install time.
  bad="$(grep -o '"args"[^]]*]' "$HOOKS" | grep -v 'CLAUDE_PLUGIN_ROOT' | tr '\n' ' ')"
  jt_assert_empty "$bad" "no hook arg bypasses \${CLAUDE_PLUGIN_ROOT}"
fi

# ---------------------------------------------------------------------- PKG-6 --
jt_case PKG-6 "marketplace entry"
if jt_need "$MARKET" "E-manifests"; then
  n="$(grep -c '"name"[[:space:]]*:[[:space:]]*"jhh3-mem"' "$MARKET")"
  jt_assert_eq "1" "$n" "jhh3-mem listed exactly once"
  jt_assert_eq "1" "$(grep -c '"source"[[:space:]]*:[[:space:]]*"\./plugins/jhh3-mem"' "$MARKET")" \
    "source is the explicit relative path ./plugins/jhh3-mem"
  if jt_need_cmd python3 "structural JSON validation"; then
    out="$(python3 - "$MARKET" "$MANIFEST" <<'PY' 2>&1
import json, sys
market, manifest = sys.argv[1], sys.argv[2]
m = json.load(open(market))
p = json.load(open(manifest))
entries = [e for e in m["plugins"] if e["name"] == "jhh3-mem"]
assert len(entries) == 1, f"expected 1 marketplace entry, got {len(entries)}"
e = entries[0]
assert e["source"] == "./plugins/jhh3-mem", e["source"]
assert "version" not in e, "marketplace entry must not carry version"
assert e.get("description"), "marketplace entry needs a description"
assert e.get("category") == "personal", e.get("category")
assert p["name"] == "jhh3-mem", p["name"]
assert p["version"], "plugin.json needs a version"
assert p["author"]["name"], "plugin.json needs an author name"
assert p["description"], "plugin.json needs a description"
print("ok " + p["version"])
PY
    )"
    rc=$?
    jt_assert_rc "0" "$rc" "both manifests parse and satisfy the structural contract"
    jt_assert_contains "$out" "ok " "structural check: $out"
  fi
fi

# ---------------------------------------------------------------------- PKG-7 --
jt_case PKG-7 "plugin identity matches its directory"
if jt_need "$MANIFEST" "E-manifests"; then
  name="$(sed -n 's/.*"name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$MANIFEST" | head -1)"
  jt_assert_eq "jhh3-mem" "$name" "plugin.json name matches the directory name"
  jt_assert_eq "jhh3-mem" "${JT_PLUGIN_ROOT##*/}" "directory is plugins/jhh3-mem"
fi

# ---------------------------------------------------------------------- PKG-8 --
# The README is a deliverable with a contract: it must carry the removal proof
# and the memory boundary, because those are the two claims a user has to be
# able to check without reading the code.
jt_case PKG-8 "README states the falsifiable claims"
if jt_need "$JT_PLUGIN_ROOT/README.md" "E-manifests"; then
  readme="$(cat "$JT_PLUGIN_ROOT/README.md")"
  jt_assert_contains "$readme" "jhh3-mem paths" "README points at the path manifest"
  jt_assert_contains "$readme" "uninstall --purge" "README documents the removal command"
  jt_assert_contains "$readme" "pgrep -fa jhh3-mem" "README prints a re-checkable no-daemon proof"
  jt_assert_contains "$readme" "MEMORY.md" "README states the built-in auto-memory boundary"
  jt_assert_contains "$readme" "PreToolUse" "README states the no-interception invariant"
  jt_assert_contains "$readme" "marketplace add Acme/agent-plugins" \
    "README re-adds the marketplace explicitly (known_marketplaces.json is stale)"
fi

# ---------------------------------------------------------------------- PKG-9 --
jt_case PKG-9 "skills follow the SKILL.md layout"
if [ -d "$JT_PLUGIN_ROOT/skills" ]; then
  bad=""
  while IFS= read -r d; do
    [ -n "$d" ] || continue
    [ -f "$d/SKILL.md" ] || bad="$bad${d##*/} "
  done < <(find "$JT_PLUGIN_ROOT/skills" -mindepth 1 -maxdepth 1 -type d 2>/dev/null)
  jt_assert_empty "$bad" "every skills/<name>/ has a SKILL.md"
  stray="$(find "$JT_PLUGIN_ROOT/skills" -maxdepth 1 -type f 2>/dev/null | tr '\n' ' ')"
  jt_assert_empty "$stray" "no flat-file commands/ style skills"
else
  jt_pend "not implemented yet: skills/memory-recall/SKILL.md (owner: E-retrieval)"
fi

# --------------------------------------------------------------------- PKG-10 --
jt_case PKG-10 "AGENTS.md documents the plugin"
AG="$JT_REPO_ROOT/AGENTS.md"
if jt_need "$AG" "repo"; then
  ag="$(cat "$AG")"
  jt_assert_contains "$ag" "jhh3-mem" "AGENTS.md mentions jhh3-mem"
  jt_assert_contains "$ag" "plugins/jhh3-mem/" "AGENTS.md lists the plugin directory"
  jt_assert_contains "$ag" "PreToolUse" "AGENTS.md records the no-interception invariant"
  jt_assert_contains "$ag" "MEMORY.md" "AGENTS.md records the built-in auto-memory boundary"
  # claude-mem is gone. It may be named as history, but never as a live source:
  # every surviving mention must say so on the same line.
  live_refs="$(grep -n 'claude-mem' "$AG" |
    grep -vi 'removed\|retired\|no longer\|archiv\|legacy\|replac' | tr '\n' ';')"
  jt_assert_empty "$live_refs" \
    "no line cites claude-mem as a live data source (the tool was removed)"
fi

jt_end
