#!/usr/bin/env bash
# t-e2e.sh — the whole plugin, front to back, against a throwaway store:
# bootstrap -> capture a session -> inject it back -> recall it -> prove the
# footprint -> remove it and prove the removal.
#
# Every phase runs against $JHH3_MEM_HOME under mktemp; the real store at
# ~/.local/share/jhh3-mem is never touched.
#
# Phases whose component has not been written yet record PEND (named with the
# owning lane) instead of a false pass. `tests/run.sh --strict` turns PEND into
# a failure, which is the release gate.
#
# The final phase drives a real `claude --plugin-dir` session. It is opt-in
# (JHH3_MEM_TEST_LIVE=1) because it spawns a model session.
set -uo pipefail

_here="${BASH_SOURCE[0]}"
case "$_here" in */*) _d="${_here%/*}" ;; *) _d="." ;; esac
# shellcheck source=lib/harness.sh
. "$_d/lib/harness.sh"

jt_begin t-e2e

LIB="$JT_PLUGIN_ROOT/lib"
SCRIPTS="$JT_PLUGIN_ROOT/scripts"
CLI="$JT_PLUGIN_ROOT/bin/jhh3-mem"
MARKER="zqe2e$$"

# _lib CODE — run CODE with lib/common.sh + lib/db.sh sourced, in a subshell, so
# the library's own `readonly` constants can never leak between phases.
_lib() {
  bash -c '
    set -uo pipefail
    . "'"$LIB"'/common.sh"
    . "'"$LIB"'/db.sh"
    '"$1"'
  ' 2>&1
}

# _export_count DIR — how many ./jhh3-mem-final-export-*.jsonl files DIR holds.
_export_count() {
  local n=0 f
  for f in "$1"/jhh3-mem-final-export-*.jsonl; do
    [ -e "$f" ] && n=$((n + 1))
  done
  printf '%s\n' "$n"
}

# _sql QUERY — read-only query straight against the store, bypassing the
# library, so an assertion cannot be satisfied by the code it is testing.
_sql() {
  sqlite3 -readonly -batch -noheader "file:$JHH3_MEM_HOME/memory.db?mode=ro" "$1" 2>/dev/null
}

# Both must run in THIS shell: jt_store's `export JHH3_MEM_HOME` is what keeps
# every phase — including the purge — off the user's real store.
jt_store || {
  jt_fail "could not create a test store"
  jt_end
}
STORE="$JT_STORE"
jt_tmpdir || {
  jt_fail "could not create a work dir"
  jt_end
}
WORK="$JT_TMP"
jt_require_sandbox

TRANSCRIPT="$WORK/projects/-home-user-e2e/aaaaaaaa-1111-2222-3333-444444444444.jsonl"
mkdir -p -- "${TRANSCRIPT%/*}"
jt_make_transcript "$TRANSCRIPT" "$MARKER" "$JT_REPO_ROOT"
SID="aaaaaaaa-1111-2222-3333-444444444444"

sqlite3_ok=1
jt_case E2E-0 "environment"
if jt_need_cmd sqlite3 "the entire storage layer"; then
  ver="$(sqlite3 --version 2>/dev/null | awk '{print $1}')"
  fts="$(sqlite3 :memory: "CREATE VIRTUAL TABLE t USING fts5(x); SELECT 'yes';" 2>/dev/null)"
  jt_assert_eq "yes" "$fts" "sqlite3 $ver has FTS5"
else
  sqlite3_ok=0
fi
jt_assert_ne "" "$STORE" "throwaway store at $STORE"

# ---------------------------------------------------------------------- E2E-1 --
jt_case E2E-1 "first run bootstraps the store"
if [ "$sqlite3_ok" = 1 ]; then
  out="$(_lib 'jm_init_store && jm_db_bootstrap && jm_db_schema_version')"
  rc=$?
  jt_assert_rc "0" "$rc" "jm_init_store + jm_db_bootstrap"
  # Read the constant rather than hardcoding it: this assertion exists to catch a
  # store that bootstrapped at the wrong version, not to be re-edited every bump.
  _want_schema="$(grep -oE '^JM_SCHEMA_VERSION=[0-9]+' "$JT_PLUGIN_ROOT/lib/common.sh" | cut -d= -f2)"
  jt_assert_eq "$_want_schema" "$(printf '%s\n' "$out" | tail -1)" \
    "schema_version matches lib/common.sh JM_SCHEMA_VERSION"
  jt_assert_file "$STORE/memory.db" "memory.db created"
  jt_assert_eq "700" "$(stat -c%a "$STORE" 2>/dev/null || stat -f%Lp "$STORE" 2>/dev/null)" "store dir is 0700"
  jt_assert_eq "600" "$(stat -c%a "$STORE/memory.db" 2>/dev/null || stat -f%Lp "$STORE/memory.db" 2>/dev/null)" "memory.db is 0600"
  jt_assert_eq "wal" "$(_sql 'PRAGMA journal_mode;')" "journal_mode=wal"
  jt_assert_eq "ok" "$(_sql 'PRAGMA quick_check;')" "fresh DB passes quick_check"
else
  jt_skip "no sqlite3"
fi

# ---------------------------------------------------------------------- E2E-2 --
# UNI-1, both directions: everything the plugin created is in the manifest, and
# everything in the manifest is inside the store.
jt_case E2E-2 "paths manifest is exhaustive and self-contained"
manifest="$(_lib 'jm_paths_manifest')"
# 10, not 8: `embed` adds lib/ and lib/vec0.so. They are listed because the
# removal promise is only as good as this manifest -- an artifact absent here is
# one `uninstall --purge` would leave behind.
jt_assert_eq "10" "$(printf '%s\n' "$manifest" | grep -c .)" "manifest lists 10 paths"
outside="$(printf '%s\n' "$manifest" | grep -v "^$STORE" | tr '\n' ' ')"
jt_assert_empty "$outside" "every manifest path is inside \$JHH3_MEM_HOME"
missing=""
while IFS= read -r real; do
  [ -n "$real" ] || continue
  printf '%s\n' "$manifest" | grep -qxF "$real" || missing="$missing${real#"$STORE"/} "
done < <(find "$STORE" -mindepth 1 2>/dev/null)
jt_assert_empty "$missing" "every file actually on disk appears in the manifest"

# ---------------------------------------------------------------------- E2E-3 --
jt_case E2E-3 "no daemon, no listening socket"
stray="$(jt_stray_processes | tr '\n' ';')"
jt_assert_empty "$stray" "pgrep -fa jhh3-mem finds no process of ours"
if jt_need_cmd ss "socket inventory"; then
  sock="$(ss -ltnp 2>/dev/null | grep jhh3-mem | tr '\n' ';')"
  jt_assert_empty "$sock" "ss -ltnp shows no jhh3-mem listener"
  # The defect being inverted, by name and port.
  p37700="$(ss -ltn 2>/dev/null | grep -c ':37700')"
  jt_assert_eq "0" "$p37700" "nothing is listening on 127.0.0.1:37700"
fi

# ---------------------------------------------------------------------- E2E-4 --
jt_case E2E-4 "SessionEnd captures exactly one row, silently"
captured=0
if jt_need "$SCRIPTS/session-end.sh" "E-capture" && [ "$sqlite3_ok" = 1 ]; then
  errf="$WORK/end.err"
  out="$(jt_payload_end "$SID" "$TRANSCRIPT" "$JT_REPO_ROOT" other |
    bash "$SCRIPTS/session-end.sh" 2>"$errf")"
  rc=$?
  jt_assert_rc "0" "$rc" "guard exits 0"
  jt_assert_empty "$out" "stdout is empty (SessionEnd output is not context)"
  jt_assert_empty "$(cat "$errf")" "stderr is empty (a hook's stderr reaches the user's terminal)"
  n="$(_sql 'SELECT count(*) FROM entries;')"
  jt_assert_eq "1" "$n" "exactly one entry row"
  title="$(_sql "SELECT title FROM entries LIMIT 1;")"
  jt_assert_contains "$title" "$MARKER" "title came from the transcript's aiTitle"
  jt_assert_eq "live" "$(_sql "SELECT origin FROM entries LIMIT 1;")" "origin=live"
  jt_assert_eq "deterministic" "$(_sql "SELECT capture_mode FROM entries LIMIT 1;")" \
    "capture_mode=deterministic (no LLM in the hook path)"
  # SPEC CAP-8: verbatim slash-command noise must not be persisted.
  jt_assert_eq "0" "$(_sql "SELECT count(*) FROM entries WHERE request LIKE '%<command-name>%';")" \
    "untyped prompt noise is filtered out"
  captured=1
fi

# ---------------------------------------------------------------------- E2E-5 --
jt_case E2E-5 "re-running SessionEnd on an unchanged transcript is a no-op"
if [ "$captured" = 1 ]; then
  jt_payload_end "$SID" "$TRANSCRIPT" "$JT_REPO_ROOT" other |
    bash "$SCRIPTS/session-end.sh" >/dev/null 2>&1
  jt_assert_rc "0" "$?" "second run exits 0"
  jt_assert_eq "1" "$(_sql 'SELECT count(*) FROM entries;')" "still exactly one row (idempotent by uid)"
else
  jt_pend "needs a captured row (see E2E-4)"
fi

# ---------------------------------------------------------------------- E2E-6 --
jt_case E2E-6 "SessionStart on an empty store emits nothing"
if jt_need "$SCRIPTS/session-start.sh" "E-retrieval" && [ "$sqlite3_ok" = 1 ]; then
  empty_home="$WORK/empty-store"
  mkdir -p -- "$empty_home"
  chmod 0700 "$empty_home"
  errf="$WORK/start-empty.err"
  out="$(jt_payload_start "$SID" "$TRANSCRIPT" "$JT_REPO_ROOT" startup |
    JHH3_MEM_HOME="$empty_home" bash "$SCRIPTS/session-start.sh" 2>"$errf")"
  rc=$?
  jt_assert_rc "0" "$rc" "guard exits 0 with no DB"
  jt_assert_empty "$out" "zero bytes on stdout — no 'no memory found' banner"
  jt_assert_empty "$(cat "$errf")" "stderr is empty"
fi

# ---------------------------------------------------------------------- E2E-7 --
jt_case E2E-7 "SessionStart injects the captured session, under the cap"
if [ "$captured" = 1 ] && [ -f "$SCRIPTS/session-start.sh" ]; then
  errf="$WORK/start.err"
  outf="$WORK/start.out"
  jt_payload_start "$SID" "$TRANSCRIPT" "$JT_REPO_ROOT" startup |
    bash "$SCRIPTS/session-start.sh" >"$outf" 2>"$errf"
  rc=$?
  out="$(cat "$outf")"
  bytes="$(wc -c <"$outf" | tr -d ' ')"
  jt_assert_rc "0" "$rc" "guard exits 0"
  jt_assert_empty "$(cat "$errf")" "stderr is empty"
  if [ "$bytes" -gt 0 ]; then
    cap="${JHH3_MEM_INJECT_MAX_BYTES:-2400}"
    if [ "$bytes" -le "$cap" ]; then
      jt_ok "digest is $bytes bytes, within the $cap-byte cap"
    else
      jt_fail "digest is $bytes bytes, over the $cap-byte cap"
    fi
    jt_assert_contains "$out" "$MARKER" "the captured session appears in the digest"
    jt_assert_contains "$out" "jhh3-mem" "digest is wrapped in an identifying block"
    jt_assert_contains "$out" "not instructions" "digest carries the data-not-instructions marker"
    if [ "$(tail -c1 "$outf" | od -An -c | tr -d ' ')" = '\n' ]; then
      jt_ok "output ends on a line boundary (never cut mid-line)"
    else
      jt_fail "digest does not end with a newline (owner: E-retrieval, lib/render.sh) -- SPEC INJ-17 wants complete lines, and several SessionStart hooks compose into one attachment (SPEC 9.2), so an unterminated last line can run into the next hook's first line"
    fi
  else
    jt_pend "digest was empty — durability floor or repo scoping excluded the fixture row"
  fi
  # INJ-7: mid-session sources must not re-inject.
  for src in clear compact; do
    o="$(jt_payload_start "$SID" "$TRANSCRIPT" "$JT_REPO_ROOT" "$src" |
      bash "$SCRIPTS/session-start.sh" 2>/dev/null)"
    jt_assert_empty "$o" "source=$src emits nothing"
  done
else
  jt_pend "needs scripts/session-start.sh (E-retrieval) and a captured row"
fi

# ---------------------------------------------------------------------- E2E-8 --
jt_case E2E-8 "the CLI can find, show and account for what was stored"
if jt_need "$CLI" "E-core"; then
  out="$(bash "$CLI" version 2>&1)"
  jt_assert_rc "0" "$?" "jhh3-mem version"
  # Derive, don't pin: this assertion exists to catch the CLI and the manifest
  # disagreeing, not to be re-edited on every release.
  _ver="$(VER="$JT_PLUGIN_ROOT/.claude-plugin/plugin.json" python3 -c 'import json,os;print(json.load(open(os.environ["VER"]))["version"])')"
  jt_assert_contains "$out" "jhh3-mem $_ver" "version string matches the manifest"
  cli_paths="$(bash "$CLI" paths 2>/dev/null)"
  jt_assert_eq "$(printf '%s\n' "$manifest" | grep -c .)" \
    "$(printf '%s\n' "$cli_paths" | grep -c .)" "CLI paths agrees with the library manifest"
  if [ "$captured" = 1 ]; then
    hits="$(bash "$CLI" search "$MARKER" --limit 5 2>&1)"
    jt_assert_rc "0" "$?" "search exits 0 when there is a hit"
    jt_assert_contains "$hits" "$MARKER" "search finds the captured session"
    # REC-4: an injection attempt must not damage the store.
    bash "$CLI" search "'; DROP TABLE entries; --" >/dev/null 2>&1
    jt_assert_eq "1" "$(_sql 'SELECT count(*) FROM entries;')" \
      "entries survives a SQL-injection-shaped query"
    js="$(bash "$CLI" search "$MARKER" --json 2>/dev/null | head -1)"
    if [ -n "$js" ]; then
      jt_assert_eq "1" "$(_sql "SELECT json_valid('$(printf '%s' "$js" | sed "s/'/''/g")');")" \
        "--json emits valid JSON per line"
    fi
  fi
  doc="$(bash "$CLI" doctor 2>&1)"
  drc=$?
  jt_assert_rc "0" "$drc" "doctor on a healthy store"
  jt_assert_contains "$doc" "processes" "doctor reports its own process footprint"
fi

# ---------------------------------------------------------------------- E2E-9 --
jt_case E2E-9 "uninstall without --purge is a dry run"
jt_require_sandbox
if jt_need "$LIB/uninstall.sh" "E-lifecycle"; then
  out="$(cd "$WORK" && bash "$LIB/uninstall.sh" 2>&1)"
  jt_assert_rc "0" "$?" "dry run exits 0"
  jt_assert_file "$STORE/memory.db" "dry run removed nothing"
  jt_assert_contains "$out" "$STORE" "dry run printed the store path"
fi

# --------------------------------------------------------------------- E2E-10 --
jt_case E2E-10 "uninstall --purge --yes removes everything and proves it"
jt_require_sandbox
if [ -f "$LIB/uninstall.sh" ]; then
  before="$(_export_count "$WORK")"
  refuse="$(cd "$WORK" && bash "$LIB/uninstall.sh" --purge 2>&1)"
  rrc=$?
  jt_assert_ne "0" "$rrc" "--purge without --yes refuses (rc $rrc)"
  jt_assert_file "$STORE/memory.db" "refusal removed nothing"
  jt_assert_ne "" "$refuse" "refusal explains itself"

  out="$(cd "$WORK" && JHH3_MEM_CONFIRM="$STORE" bash "$LIB/uninstall.sh" --purge --yes 2>&1)"
  jt_assert_rc "0" "$?" "purge exits 0"
  jt_assert_no_file "$STORE" "store directory is gone"
  after="$(_export_count "$WORK")"
  if [ "$after" -gt "$before" ]; then
    jt_ok "final export written into CWD, not into the deleted directory"
  else
    jt_fail "no ./jhh3-mem-final-export-*.jsonl in CWD after purge"
  fi
  jt_assert_contains "$out" "pgrep" "purge printed re-checkable verification commands"
  # The claim the whole plugin is judged on.
  jt_assert_empty "$(jt_stray_processes | tr '\n' ';')" "no process survives the removal"
  # -mindepth 1: the harness's own mktemp dir is itself named jhh3-mem-test.*,
  # so searching from it without a mindepth matches the search root.
  jt_assert_empty "$(find "${STORE%/store}" -mindepth 1 -maxdepth 4 -name 'jhh3-mem*' 2>/dev/null | tr '\n' ' ')" \
    "find finds no leftover jhh3-mem* path where the store lived"
fi

# --------------------------------------------------------------------- E2E-11 --
# The only phase that proves the hooks are actually *registered* and fire: a
# real session with the plugin loaded from disk. Opt-in.
jt_case E2E-11 "live session with the plugin loaded (UNI-7, UNI-12)"
if [ "${JHH3_MEM_TEST_LIVE:-0}" != "1" ]; then
  jt_skip "set JHH3_MEM_TEST_LIVE=1 to drive a real \`claude --plugin-dir\` session"
elif ! jt_need "$SCRIPTS/session-end.sh" "E-capture"; then
  : # already recorded PEND
elif jt_need_cmd claude "the live session"; then
  live_home="$WORK/live-store"
  mkdir -p -- "$live_home"
  chmod 0700 "$live_home"
  logf="$WORK/live.log"
  (
    cd "$JT_REPO_ROOT" || exit 1
    JHH3_MEM_HOME="$live_home" timeout 300 claude -p 'Reply with exactly: PONG' \
      --plugin-dir "$JT_PLUGIN_ROOT" >"$logf" 2>&1
  )
  lrc=$?
  jt_assert_rc "0" "$lrc" "claude -p completed with the plugin loaded"
  # What must be proven here is that the hooks are *registered and fire* — not
  # that a row appeared. A one-turn `-p` session edits nothing, so SPEC 6.3.2's
  # skip rule (n_mod == 0, short outcome, empty request) correctly declines to
  # write: `noop reason=trivial_session` is a pass, and a row is a bonus.
  jt_assert_file "$live_home/memory.db" "SessionEnd fired and bootstrapped the store"
  oplog="$live_home/jhh3-mem.log"
  if [ -f "$oplog" ]; then
    logtxt="$(cat "$oplog")"
    jt_assert_contains "$logtxt" "session_end" "the ops log records the SessionEnd hook running"
    n="$(sqlite3 -readonly -batch -noheader "file:$live_home/memory.db?mode=ro" \
      'SELECT count(*) FROM entries;' 2>/dev/null)"
    case "$logtxt" in
      *trivial_session*) jt_ok "one-turn session correctly declined to write (rows=$n)" ;;
      *)
        if [ "${n:-0}" -gt 0 ]; then
          jt_ok "the SessionEnd hook wrote $n row(s) during a real session"
        else
          jt_fail "SessionEnd wrote nothing and did not log why -- log: $(printf '%.300s' "$logtxt")"
        fi
        ;;
    esac
  else
    jt_fail "no ops log at $oplog — the hook produced no trace at all"
  fi
  # UNI-12: nothing may outlive the session.
  sleep 5
  jt_assert_empty "$(jt_stray_processes | tr '\n' ';')" "no jhh3-mem process survives the session"
fi

jt_end
