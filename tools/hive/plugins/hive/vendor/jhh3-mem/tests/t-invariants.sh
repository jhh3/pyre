#!/usr/bin/env bash
# t-invariants.sh — SPEC 14.1 (T-INV). These are build gates, not behaviour
# tests: each one is the machine-checkable inverse of a defect that got the
# previous memory tool removed.
#
# Not under `set -e`: an assertion must be able to fail without aborting.
set -uo pipefail

_here="${BASH_SOURCE[0]}"
case "$_here" in */*) _d="${_here%/*}" ;; *) _d="." ;; esac
# shellcheck source=lib/harness.sh
. "$_d/lib/harness.sh"

jt_begin t-invariants

HOOKS="$JT_PLUGIN_ROOT/hooks/hooks.json"
MANIFEST="$JT_PLUGIN_ROOT/.claude-plugin/plugin.json"
MARKET="$JT_REPO_ROOT/.claude-plugin/marketplace.json"

# _logical_lines FILE — emit `LINENO:TEXT` with backslash continuations joined
# and whole-line comments dropped, so a multi-line `sqlite3 … \` invocation is
# judged as one command and a commented-out example never trips a gate.
_logical_lines() {
  LC_ALL=C awk '
    function emit(s, n,   t) {
      t = s
      sub(/^[ \t]+/, "", t)
      if (t == "" || substr(t, 1, 1) == "#") return
      printf "%d:%s\n", n, s
    }
    BEGIN { buf = ""; start = 0 }
    {
      if (buf == "") start = NR
      cur = $0
      if (cur ~ /\\$/) { sub(/\\$/, "", cur); buf = buf cur " "; next }
      buf = buf cur
      emit(buf, start)
      buf = ""
    }
    END { if (buf != "") emit(buf, start) }
  ' "$1" 2>/dev/null
}

# Every shell source file the plugin ships (tests excluded: they legitimately
# talk about the very patterns being banned).
_shell_files() {
  find "$JT_PLUGIN_ROOT" -type f \
    \( -name '*.sh' -o -path "$JT_PLUGIN_ROOT/bin/*" \) \
    -not -path "$JT_PLUGIN_ROOT/tests/*" 2>/dev/null | sort
}

# ---------------------------------------------------------------------- INV-1 --
jt_case INV-1 "hooks.json registers no interception event"
if jt_need "$HOOKS" "E-manifests"; then
  banned="$(grep -c -E 'PreToolUse|PostToolUse|UserPromptSubmit|PreCompact|SubagentStop' "$HOOKS")"
  jt_assert_eq "0" "$banned" "no PreToolUse/PostToolUse/UserPromptSubmit/PreCompact/SubagentStop"
  events="$(grep -o -E '"(SessionStart|SessionEnd)"' "$HOOKS" | sort -u | tr '\n' ' ')"
  jt_assert_eq '"SessionEnd" "SessionStart" ' "$events" "only SessionStart and SessionEnd are declared"
  # The whole-plugin sweep: no other file may register an interception hook
  # either (a stray hooks fragment would be just as dangerous).
  stray="$(grep -rl -E '"(PreToolUse|PostToolUse|UserPromptSubmit)"' "$JT_PLUGIN_ROOT" \
    --include='*.json' 2>/dev/null | tr '\n' ' ')"
  jt_assert_empty "$stray" "no other JSON file in the plugin registers an interception hook"
fi

# ---------------------------------------------------------------------- INV-2 --
jt_case INV-2 "CLAUDE_PLUGIN_DATA appears nowhere in the plugin"
# Shipped shell only: README.md and SPEC.md name the variable in order to state
# that nothing uses it, and a naive tree-wide grep would flag that documentation.
_n2=0
for _f in $(_shell_files); do
  grep -q 'CLAUDE_PLUGIN_DATA' "$_f" 2>/dev/null && { _n2=$((_n2+1)); jt_note "refs CLAUDE_PLUGIN_DATA: ${_f#"$JT_PLUGIN_ROOT"/}"; }
done
jt_assert_eq "0" "$_n2" "no file references CLAUDE_PLUGIN_DATA (the store path is ours, not the host's)"

# ---------------------------------------------------------------------- INV-3 --
jt_case INV-3 "every non-readonly sqlite3 invocation in lib/ passes -bail"
_bad3=0
for _f in $(_shell_files); do
  while IFS= read -r _l; do
    # An invocation, not a mention: every real call in this codebase passes -batch.
    # `jm_have sqlite3`, `sqlite3 --version`, and "no_sqlite3" log strings are not calls.
    case "$_l" in
      *sqlite3*-batch*|*sqlite3*-cmd*)
        case "$_l" in *-readonly*|*mode=ro*) continue ;; esac
        case "$_l" in *-bail*) ;; *) _bad3=$((_bad3+1)); jt_note "missing -bail: ${_f#"$JT_PLUGIN_ROOT"/}:${_l%%:*}" ;; esac ;;
    esac
  done < <(_logical_lines "$_f")
done
jt_assert_eq "0" "$_bad3" "no sqlite3 write invocation can continue past an error"

# ---------------------------------------------------------------------- INV-4 --
jt_case INV-4 "every .import in lib/ uses --schema temp"
_bad4=0
for _f in $(_shell_files); do
  while IFS= read -r _l; do
    case "$_l" in
      *.import*) case "$_l" in *"--schema temp"*) ;; *) _bad4=$((_bad4+1)) ;; esac ;;
    esac
  done < <(_logical_lines "$_f")
done
jt_assert_eq "0" "$_bad4" "no .import can write outside the temp schema"

# ---------------------------------------------------------------------- INV-5 --
jt_case INV-5 "version lives in the plugin manifests only, and they agree"
_mv="$(MANIFEST="$MANIFEST" python3 -c 'import json,os;print(json.load(open(os.environ["MANIFEST"]))["version"])' 2>/dev/null)"
_cv="$(CX="$JT_PLUGIN_ROOT/.codex-plugin/plugin.json" python3 -c 'import json,os;print(json.load(open(os.environ["CX"]))["version"])' 2>/dev/null)"
_jv="$(grep -oE 'JM_VERSION=.?[0-9]+\.[0-9]+\.[0-9]+' "$JT_PLUGIN_ROOT/lib/common.sh" 2>/dev/null | head -1 | grep -oE '[0-9]+\.[0-9]+\.[0-9]+')"
_mkt="$(MARKET="$MARKET" python3 -c '
import json,os
d=json.load(open(os.environ["MARKET"]))
print(sum(1 for p in d["plugins"] if p.get("name")=="jhh3-mem" and "version" in p))' 2>/dev/null)"
jt_assert_eq "$_mv" "$_jv"  "JM_VERSION matches .claude-plugin/plugin.json"
jt_assert_eq "$_mv" "$_cv"  "the two manifests declare the same version"
jt_assert_eq "0"   "$_mkt"  "the marketplace entry carries no version key (the manifest wins)"

# ---------------------------------------------------------------------- INV-6 --
jt_case INV-6 "no credentials and no network client outside lib/enrich.sh and lib/embed.sh"
# lib/embed.sh joined enrich.sh on this list when semantic search landed: it holds
# OPENAI_API_KEY and calls curl. The list is a spelling of "which files are allowed
# to reach out", and it only means something alongside the property it protects --
# that NO HOOK can reach either one. That property is not provable by reading
# source, so it is proved by execution in t-embed (EMB-2, EMB-3), which runs both
# hooks with curl/wget/nc replaced by tripwires and a key exported. Widening this
# list without that pair of tests would be removing the invariant, not amending it.
# Judge USE, not mention. Two false positives this replaced, both real: `nc ` as a
# substring matched the word "async" in a comment, and the CLI's own --help text
# names OPENAI_API_KEY so the user knows what to set. Naming a variable in help is
# the opposite of a leak, and a check that punishes documentation gets satisfied by
# deleting documentation. So: a credential counts when it is DEREFERENCED or
# ASSIGNED, and a network client counts when it appears in COMMAND POSITION.
_bad6=0; _hits6=''
_cred_re='[$][{]?(ANTHROPIC|OPENAI)_API_KEY|(^|[[:space:]])(export[[:space:]]+)?(ANTHROPIC|OPENAI)_API_KEY='
_net_re='(^|[;&|(`]|[$][(])[[:space:]]*(curl|wget|nc|ssh)[[:space:]]'
for _f in $(_shell_files); do
  case "$_f" in */lib/enrich.sh | */lib/embed.sh) continue ;; esac
  while IFS= read -r _l; do
    _body="${_l#*:}"
    if printf '%s' "$_body" | grep -qE "$_cred_re"; then
      _bad6=$((_bad6+1)); _hits6="$_hits6 credential:${_f#"$JT_PLUGIN_ROOT"/}:${_l%%:*}"
    fi
    if printf '%s' "$_body" | grep -qE "$_net_re"; then
      _bad6=$((_bad6+1)); _hits6="$_hits6 network:${_f#"$JT_PLUGIN_ROOT"/}:${_l%%:*}"
    fi
  done < <(_logical_lines "$_f")
done
[ -z "$_hits6" ] || printf '       offenders:%s\n' "$_hits6" >&2
jt_assert_eq "0" "$_bad6" "only the two out-of-band paths may reach the network, and nothing else holds a key"

# ---------------------------------------------------------------------- INV-7 --
jt_case INV-7 "every hook entry declares an explicit timeout"
_to="$(HOOKS="$HOOKS" python3 -c '
import json,os
d=json.load(open(os.environ["HOOKS"]))
tot=miss=0
for ev in d["hooks"].values():
    for m in ev:
        for h in m["hooks"]:
            tot+=1
            if not isinstance(h.get("timeout"), int): miss+=1
print(f"{tot} {miss}")' 2>/dev/null)"
jt_assert_eq "2 0" "$_to" "both hooks bound their own wall clock"

# ---------------------------------------------------------------------- INV-9 --
jt_case INV-9 "both hooks use the portable, space-safe command form"
# The exec form (command:"bash", path in args[]) is Claude-Code-only: verified by
# probe against codex 0.147.0, a hooks.json using args[] installs cleanly and then
# SILENTLY NEVER FIRES. Codex reads the single-string `command` form (which is why
# plugins/acme works on both hosts). Both hosts shell-parse that string, so the
# path is double-quoted inside it -- that restores the space-safety the exec form
# gave us for free, which is the property INV-7 has always actually been about.
_inv7_ok=0
_inv7_quoted=0
for _c in $(HOOKS="$HOOKS" python3 -c '
import json,os
d=json.load(open(os.environ["HOOKS"]))
for ev in d["hooks"].values():
    for m in ev:
        for h in m["hooks"]:
            print(("ARGS" if "args" in h else "STR"))
'); do
  [ "$_c" = "STR" ] && _inv7_ok=$((_inv7_ok+1))
done
_inv7_quoted="$(grep -c '"command": "bash \\"\${CLAUDE_PLUGIN_ROOT}' "$HOOKS" || true)"
jt_assert_eq "2" "$_inv7_ok"     "both hooks use the single-string command form (portable to Codex)"
jt_assert_eq "2" "$_inv7_quoted" "both hook command strings double-quote the plugin-root path"

jt_case INV-8 "no listener, no daemon primitive"
bad=""
while IFS= read -r f; do
  [ -n "$f" ] || continue
  while IFS= read -r rec; do
    [ -n "$rec" ] || continue
    n="${rec%%:*}"
    line="${rec#*:}"
    if printf '%s\n' "$line" |
      grep -qE '(^|[;&|(]|\$\(|`)[[:space:]]*(nohup|setsid|disown|systemd-run|screen|tmux)([[:space:]]|$)|/dev/tcp/'; then
      bad="$bad${f#"$JT_PLUGIN_ROOT"/}:$n "
    fi
  done < <(_logical_lines "$f")
done < <(_shell_files)
jt_assert_empty "$bad" "no nohup/setsid/disown/systemd-run/screen/tmux/-dev-tcp anywhere"

jt_end
