#!/usr/bin/env bash
# jhh3-mem — tests/t-uninstall.sh   SPEC §14.9 (UNI-1 .. UNI-12)
#
# The destructive path. Hard constraint 8 is "uninstall must be complete and
# provable", and the tool this replaces failed it: its uninstall left detached
# daemons and ~1.5 GB across six paths. So the assertions here are in two
# groups — what must be REMOVED, and what must be REFUSED.
#
# jt_require_sandbox runs before every destructive case. A test that can delete
# the user's real memory because an export silently failed is worse than no
# test, and that bug was live in this harness once.
set -uo pipefail

_t_src="${BASH_SOURCE[0]}"
case "$_t_src" in */*) _t_dir="${_t_src%/*}" ;; *) _t_dir="." ;; esac
# shellcheck source=./lib/harness.sh
. "$_t_dir/lib/harness.sh"

jt_begin t-uninstall

LIB="$JT_PLUGIN_ROOT/lib"
CLI="$JT_PLUGIN_ROOT/bin/jhh3-mem"

jt_need_cmd sqlite3 'the store is a sqlite database' || jt_end

# _seed — a store with one note and one captured session, plus a CWD to run in.
_seed() {
  jt_store
  jt_require_sandbox
  jt_tmpdir
  WORK="$JT_TMP"
  bash "$CLI" note "a memory worth exporting before deletion" >/dev/null 2>&1
  jt_make_transcript "$WORK/t.jsonl" unimarker "$JT_REPO_ROOT"
  jt_payload_end uni-1 "$WORK/t.jsonl" "$JT_REPO_ROOT" other |
    bash "$JT_PLUGIN_ROOT/scripts/session-end.sh" >/dev/null 2>&1
}

# ── UNI-1 ───────────────────────────────────────────────────────────────────
jt_case UNI-1 "the default is a dry run that removes nothing"
_seed
rows_before="$(sqlite3 -batch -noheader "$JHH3_MEM_HOME/memory.db" 'SELECT count(*) FROM entries;')"
out="$(cd "$WORK" && bash "$CLI" uninstall 2>&1)"; rc=$?
jt_assert_rc "0" "$rc" "dry run exits 0"
jt_assert_contains "$out" 'DRY RUN' "it says it is a dry run"
jt_assert_file "$JHH3_MEM_HOME/memory.db" "the database is still there"
jt_assert_eq "$rows_before" "$(sqlite3 -batch -noheader "$JHH3_MEM_HOME/memory.db" 'SELECT count(*) FROM entries;')" \
  "and still has every row"
jt_assert_empty "$(find "$WORK" -maxdepth 1 -mindepth 1 -name 'jhh3-mem-final-export*' | sed 's#.*/##')" \
  "a dry run writes no export"

# ── UNI-2 ───────────────────────────────────────────────────────────────────
jt_case UNI-2 "the dry run prints the generated path inventory, not a hand-written list"
n_manifest="$(bash -c '. "'"$LIB"'/common.sh"; jm_paths_manifest_annotated' | grep -c .)"
n_printed=0
while IFS= read -r line; do
  case "$line" in
    *'[exists]'* | *'[absent]'*) n_printed=$((n_printed + 1)) ;;
  esac
done <<<"$out"
jt_assert_eq "$n_manifest" "$n_printed" "every manifest row is printed with its state"
jt_assert_contains "$out" "$JHH3_MEM_HOME/memory.db" "the database path is named"
jt_assert_contains "$out" "${TMPDIR:-/tmp}/jhh3-mem." "the transient staging paths are named too"

# ── UNI-3 ───────────────────────────────────────────────────────────────────
jt_case UNI-3 "the verification block is real commands with live results"
jt_assert_contains "$out" 'verification (re-run any of these yourself)' "the block is present"
jt_assert_contains "$out" 'pgrep -fa jhh3-mem' "it shows the process probe"
jt_assert_contains "$out" 'ss -ltnp' "it shows the socket probe"
# Every line under pgrep must NAME the process; a bare PID is an unexplained
# claim in the one output whose whole purpose is being checkable.
bad=0
while IFS= read -r line; do
  case "$line" in
    '      '[0-9]*)
      # "<indent><pid> <command>" — a pid with nothing after it is the defect.
      rest="${line#      }"
      case "$rest" in
        *' '*) : ;;
        *) bad=$((bad + 1)) ;;
      esac
      ;;
  esac
done <<<"$out"
jt_assert_eq "0" "$bad" "no bare PID is printed without its command line"

# ── UNI-4 ───────────────────────────────────────────────────────────────────
jt_case UNI-4 "--purge without --yes refuses while the store holds rows"
out="$(cd "$WORK" && bash "$CLI" uninstall --purge 2>&1)"; rc=$?
jt_assert_ne "0" "$rc" "it refuses (rc $rc)"
jt_assert_contains "$out" 'refused' "and says so"
jt_assert_file "$JHH3_MEM_HOME/memory.db" "nothing was removed"

# ── UNI-5 ───────────────────────────────────────────────────────────────────
jt_case UNI-5 "a non-interactive --purge --yes still requires JHH3_MEM_CONFIRM"
out="$(cd "$WORK" && bash "$CLI" uninstall --purge --yes </dev/null 2>&1)"; rc=$?
jt_assert_ne "0" "$rc" "it refuses without the confirmation variable (rc $rc)"
jt_assert_contains "$out" 'JHH3_MEM_CONFIRM' "and names the variable it wants"
jt_assert_file "$JHH3_MEM_HOME/memory.db" "nothing was removed"

# ── UNI-6 ───────────────────────────────────────────────────────────────────
jt_case UNI-6 "a store that is not ours is refused even with every flag set"
jt_tmpdir
foreign="$JT_TMP/not-our-store"
mkdir -p "$foreign"
printf 'important\n' >"$foreign/somebody-elses-file"
out="$(cd "$WORK" && JHH3_MEM_HOME="$foreign" JHH3_MEM_CONFIRM="$foreign" \
  bash "$CLI" uninstall --purge --yes 2>&1)"; rc=$?
jt_assert_ne "0" "$rc" "it refuses a directory that holds none of our artifacts (rc $rc)"
jt_assert_file "$foreign/somebody-elses-file" "the foreign file is untouched"

# ── UNI-7 ───────────────────────────────────────────────────────────────────
jt_case UNI-7 "--purge --yes removes the whole store and exports first"
jt_require_sandbox
store="$JHH3_MEM_HOME"
out="$(cd "$WORK" && JHH3_MEM_CONFIRM="$store" bash "$CLI" uninstall --purge --yes 2>&1)"; rc=$?
jt_assert_rc "0" "$rc" "purge exits 0"
jt_assert_no_file "$store" "the store directory is gone"
jt_assert_empty "$(find "$store" 2>/dev/null)" "find over the store returns nothing"
exp="$(find "$WORK" -maxdepth 1 -mindepth 1 -name 'jhh3-mem-final-export*' | sed 's#.*/##')"
jt_assert_ne "" "$exp" "the final export landed in the CWD"
jt_assert_ne "" "$(head -c 1 "$WORK/$exp" 2>/dev/null)" "and it is not empty"
jt_assert_eq "1" "$(sqlite3 -batch -noheader ':memory:' \
  "SELECT json_valid('$(head -1 "$WORK/$exp" | sed "s/'/''/g")');")" \
  "each export line is valid JSON"
case "$(cd "$WORK" && ls -1)" in
  *jhh3-mem-final-export*) jt_ok "the export is NOT inside the directory that was deleted" ;;
  *) jt_fail "the export is not in the CWD" ;;
esac

# ── UNI-8 ───────────────────────────────────────────────────────────────────
jt_case UNI-8 "the removal report proves itself"
jt_assert_contains "$out" 'removed' "it reports what it removed"
jt_assert_contains "$out" 'verification' "and prints the verification block again"
jt_assert_contains "$out" '/plugin uninstall jhh3-mem@acme-plugins' "and the next step"
jt_assert_contains "$out" 'left alone' "and names what it deliberately did not touch"

# ── UNI-9 ───────────────────────────────────────────────────────────────────
jt_case UNI-9 "no process and no socket survives"
jt_assert_empty "$(jt_stray_processes)" "no jhh3-mem process is left running"
if jt_need_cmd ss 'socket probe'; then
  jt_assert_eq "0" "$(ss -ltnp 2>/dev/null | grep -c 'jhh3-mem' || true)" "no listening socket"
fi

# ── UNI-10 ──────────────────────────────────────────────────────────────────
jt_case UNI-10 "staging artifacts under TMPDIR are swept, not orphaned"
_seed
jt_require_sandbox
# Simulate what a SIGKILL leaves behind: a stage directory with the same shape
# a killed capture would have left, holding pre-redaction text.
orphan="$(mktemp -d "${TMPDIR:-/tmp}/jhh3-mem.XXXXXXXX")"
printf 'pre-redaction transcript text\n' >"$orphan/fields.tsv"
orphan_payload="$(mktemp "${TMPDIR:-/tmp}/jhh3-mem-payload.XXXXXXXX")"
jt_assert_file "$orphan" "an orphaned stage directory exists"
out="$(cd "$WORK" && bash "$CLI" uninstall 2>&1)"
jt_assert_contains "$out" 'staging artifact' "the dry run reports the orphan"
out="$(cd "$WORK" && JHH3_MEM_CONFIRM="$JHH3_MEM_HOME" bash "$CLI" uninstall --purge --yes 2>&1)"
jt_assert_no_file "$orphan" "purge removed the orphaned stage directory"
jt_assert_no_file "$orphan_payload" "purge removed the orphaned payload file"

# ── UNI-11 ──────────────────────────────────────────────────────────────────
jt_case UNI-11 "uninstall on an absent store is a clean no-op"
jt_tmpdir
export JHH3_MEM_HOME="$JT_TMP/never-existed"
out="$(cd "$WORK" && bash "$CLI" uninstall 2>&1)"; rc=$?
jt_assert_eq "2" "$rc" "rc 2 means 'nothing to remove'"
jt_assert_contains "$out" 'nothing to remove' "and it says exactly that"
out="$(cd "$WORK" && JHH3_MEM_CONFIRM="$JHH3_MEM_HOME" bash "$CLI" uninstall --purge --yes 2>&1)"; rc=$?
jt_assert_eq "2" "$rc" "purging an absent store is also a clean no-op"

# ── UNI-12 ──────────────────────────────────────────────────────────────────
jt_case UNI-12 "nothing is ever written outside the store directory"
_seed
jt_require_sandbox
for d in "$HOME/.claude/plugins/data" "$HOME/.cache/jhh3-mem" "$HOME/.npm/jhh3-mem"; do
  case "$d" in
    *jhh3-mem*) jt_assert_no_file "$d" "nothing at $d" ;;
    *) [ -e "$d/jhh3-mem" ] && jt_fail "created $d/jhh3-mem" || jt_ok "nothing at $d/jhh3-mem" ;;
  esac
done
jt_assert_empty "$(LC_ALL=C grep -rln 'CLAUDE_PLUGIN_DATA\|\.claude/plugins/data' \
  "$LIB" "$JT_PLUGIN_ROOT/scripts" "$JT_PLUGIN_ROOT/bin" 2>/dev/null |
  grep -v uninstall.sh || true)" \
  "no code path outside uninstall.sh even names the harness-managed data dir"

jt_end
