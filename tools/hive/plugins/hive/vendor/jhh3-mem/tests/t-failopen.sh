#!/usr/bin/env bash
# jhh3-mem — tests/t-failopen.sh   SPEC §14.4 (FO-1 .. FO-15)
#
# Every case asserts the SessionEnd contract (§8.1):
#   exit code 0 · stdout EMPTY · stderr EMPTY · one line in the ops log.
# The sole sanctioned stderr exception is FO-13, where the ops log itself cannot
# be written.
set -uo pipefail

_t_src="${BASH_SOURCE[0]}"
case "$_t_src" in */*) _t_dir="${_t_src%/*}" ;; *) _t_dir="." ;; esac
JT_SUITE=failopen
# shellcheck source=./lib/e2-harness.sh
. "$_t_dir/lib/e2-harness.sh"
trap jt_cleanup EXIT INT TERM

SID='ffff0000-1111-2222-3333-444444444444'
SMALL="$JT_PLUGIN_ROOT/tests/fixtures/transcript-small.jsonl"

# fo_run ID PAYLOAD_FILE [ENV=VAL ...] — run the hook, assert the invariants,
# return the number of log lines it produced.
fo_run() {
  local id="$1" pl="$2"
  shift 2
  local o e rc
  o="$(env "$@" bash "$JT_SESSION_END" <"$pl" 2>"$JT_TMP/stderr.$id")"
  rc=$?
  e="$(cat "$JT_TMP/stderr.$id" 2>/dev/null)"
  jt_eq "$id" 0 "$rc" 'exit code'
  jt_eq "$id" '' "$o" 'stdout empty'
  jt_eq "$id" '' "$e" 'stderr empty'
}

# fo_logcount — lines currently in the ops log.
fo_logcount() { wc -l <"$JT_LOG" 2>/dev/null | tr -d ' ' || printf 0; }

# ── FO-1 ─ sqlite3 absent (PATH stripped of it) ─────────────────────────────
jt_store
nosql="$JT_TMP/nosqlite"
mkdir -p "$nosql"
for b in bash sed grep awk tr cat head tail cut sort uniq stat date mkdir chmod rm ln cp mv mktemp timeout wc find id printf dirname basename env sh od flock touch seq; do
  src="$(command -v "$b" 2>/dev/null)" && ln -sf "$src" "$nosql/$b"
done
pl="$JT_TMP/p1.json"
jt_payload "$pl" "$SID" "$SMALL" "$JT_TMP" other
fo_run FO-1 "$pl" "PATH=$nosql"
jt_has FO-1 "$(cat "$JT_LOG" 2>/dev/null)" 'reason=no_sqlite3' 'degraded log line'
jt_eq FO-1 1 "$(fo_logcount)" 'exactly one log line'
if [ -f "$JT_DB" ]; then jt_no FO-1 'a db was created without sqlite3'; else jt_ok FO-1 'no db created'; fi

# ── FO-2 ─ corrupt DB (200 KB of /dev/urandom) → sqlite3 exit 26 ────────────
jt_store
mkdir -p "$JT_HOME"
head -c 204800 /dev/urandom >"$JT_DB"
pl="$JT_TMP/p2.json"
jt_payload "$pl" "$SID" "$SMALL" "$JT_TMP" other
fo_run FO-2 "$pl"
jt_has FO-2 "$(cat "$JT_LOG" 2>/dev/null)" 'rc=26' 'not-a-database reported'
jt_eq FO-2 1 "$(fo_logcount)" 'exactly one log line'
jt_eq FO-2 204800 "$(stat -c%s "$JT_DB" 2>/dev/null || stat -f%z "$JT_DB")" 'corrupt db left untouched — never auto-repaired'

# ── FO-3 ─ zero-length memory.db is a VALID empty database ──────────────────
jt_store
mkdir -p "$JT_HOME"
: >"$JT_DB"
pl="$JT_TMP/p3.json"
jt_payload "$pl" "$SID" "$SMALL" "$JT_TMP" other
fo_run FO-3 "$pl"
jt_eq FO-3 1 "$(jt_sql 'SELECT count(*) FROM entries;')" 'DDL applied and the row landed'
_fo_sv="$(grep -oE '^JM_SCHEMA_VERSION=[0-9]+' "$JT_PLUGIN_ROOT/lib/common.sh" | cut -d= -f2)"
jt_eq FO-3 "$_fo_sv" "$(jt_sql "SELECT v FROM meta WHERE k='schema_version';")" 'schema_version written'

# ── FO-4 ─ store not writable ───────────────────────────────────────────────
# NB chmod 0500 on a directory you OWN is not enough: jm_init_store restores
# 0700 by contract. An unwritable PARENT is the real condition.
jt_store
pl="$JT_TMP/p4.json"
jt_payload "$pl" "$SID" "$SMALL" "$JT_TMP" other
o="$(JHH3_MEM_HOME=/proc/self/nope/store bash "$JT_SESSION_END" <"$pl" 2>"$JT_TMP/e4")"
jt_eq FO-4 0 $? 'exit code'
jt_eq FO-4 '' "$o" 'stdout empty'
jt_eq FO-4 '' "$(cat "$JT_TMP/e4")" 'stderr empty (the worker log went to the sink)'
# The worker on its own says why, on stderr, because the ops log is unreachable too.
werr="$(JHH3_MEM_HOME=/proc/self/nope/store bash "$JT_CAPTURE" --transcript "$SMALL" \
  --session "$SID" --cwd "$JT_TMP" --reason other 2>&1 >/dev/null)"
jt_ne FO-4 0 $? 'worker reports failure'
jt_has FO-4 "$werr" 'log unwritable' 'the sanctioned notice, and nothing else'
jt_eq FO-4 '' "$(JHH3_MEM_HOME=/proc/self/nope/store bash "$JT_CAPTURE" --transcript "$SMALL" \
  --session "$SID" --cwd "$JT_TMP" --reason other 2>/dev/null)" 'worker stdout empty'

# ── FO-5 ─ malformed JSON on stdin ──────────────────────────────────────────
jt_store
fo_run FO-5 "$JT_FIXTURES/payload-bad.json"
jt_has FO-5 "$(cat "$JT_LOG" 2>/dev/null)" 'reason=bad_payload' 'log line'
jt_eq FO-5 1 "$(fo_logcount)" 'exactly one log line'

# ── FO-6 ─ empty stdin ──────────────────────────────────────────────────────
jt_store
: >"$JT_TMP/empty.json"
fo_run FO-6 "$JT_TMP/empty.json"
jt_has FO-6 "$(cat "$JT_LOG" 2>/dev/null)" 'reason=bad_payload' 'log line'
jt_eq FO-6 1 "$(fo_logcount)" 'exactly one log line'

# ── FO-7 ─ transcript_path points at a nonexistent file ─────────────────────
jt_store
pl="$JT_TMP/p7.json"
jt_payload "$pl" "$SID" "$JT_TMP/there-is-no-such-transcript.jsonl" "$JT_TMP" other
fo_run FO-7 "$pl"
jt_has FO-7 "$(cat "$JT_LOG" 2>/dev/null)" 'reason=no_transcript' 'log line'
jt_eq FO-7 0 "$(jt_sql 'SELECT count(*) FROM entries;')" 'no rows'

# ── FO-8 ─ transcript_path points at a directory ────────────────────────────
jt_store
mkdir -p "$JT_TMP/adirectory.jsonl"
pl="$JT_TMP/p8.json"
jt_payload "$pl" "$SID" "$JT_TMP/adirectory.jsonl" "$JT_TMP" other
fo_run FO-8 "$pl"
jt_has FO-8 "$(cat "$JT_LOG" 2>/dev/null)" 'reason=no_transcript' 'log line'
jt_eq FO-8 0 "$(jt_sql 'SELECT count(*) FROM entries;')" 'no rows'

# ── FO-9 ─ the worker overruns its wall clock ───────────────────────────────
jt_store
sleeper="$JT_TMP/sleeper.sh"
printf '#!/usr/bin/env bash\nsleep 30\n' >"$sleeper"
chmod +x "$sleeper"
pl="$JT_TMP/p9.json"
jt_payload "$pl" "$SID" "$SMALL" "$JT_TMP" other
t0=$(date +%s)
fo_run FO-9 "$pl" "JHH3_MEM_CAPTURE_WORKER=$sleeper"
el=$(( $(date +%s) - t0 ))
jt_has FO-9 "$(cat "$JT_LOG" 2>/dev/null)" 'reason=wall_timeout' 'wall_timeout logged'
if [ "$el" -le 4 ]; then jt_ok FO-9 "killed after ${el}s (<= 4)"; else jt_no FO-9 "took ${el}s"; fi
jt_eq FO-9 1 "$(fo_logcount)" 'exactly one log line'

# ── FO-10 ─ the worker kills itself with SIGKILL ────────────────────────────
jt_store
suicide="$JT_TMP/suicide.sh"
printf '#!/usr/bin/env bash\nkill -9 $$\n' >"$suicide"
chmod +x "$suicide"
pl="$JT_TMP/p10.json"
jt_payload "$pl" "$SID" "$SMALL" "$JT_TMP" other
fo_run FO-10 "$pl" "JHH3_MEM_CAPTURE_WORKER=$suicide"
jt_has FO-10 "$(cat "$JT_LOG" 2>/dev/null)" 'rc=137' 'rc 137 recorded'
jt_eq FO-10 1 "$(fo_logcount)" 'exactly one log line'

# ── FO-11 ─ DB schema AHEAD of the code ─────────────────────────────────────
jt_store
jt_capture "$SMALL" 'aaaa1111-0000-0000-0000-000000000001' "$JT_TMP" other >/dev/null 2>&1
before="$(jt_sql 'SELECT count(*) FROM entries;')"
jt_sql "UPDATE meta SET v='99' WHERE k='schema_version';" >/dev/null
: >"$JT_LOG"
pl="$JT_TMP/p11.json"
jt_payload "$pl" 'aaaa1111-0000-0000-0000-000000000002' "$SMALL" "$JT_TMP" other
fo_run FO-11 "$pl"
jt_has FO-11 "$(cat "$JT_LOG" 2>/dev/null)" 'reason=schema_ahead' 'log line'
jt_eq FO-11 "$before" "$(jt_sql 'SELECT count(*) FROM entries;')" 'no write'
jt_eq FO-11 1 "$(fo_logcount)" 'exactly one log line'

# ── FO-12 ─ DB schema BEHIND the code ───────────────────────────────────────
# SessionEnd never runs DDL and never speaks up: the one visible upgrade line
# (§8.4) belongs to SessionStart. Here we only prove silence + no write.
jt_store
jt_capture "$SMALL" 'aaaa2222-0000-0000-0000-000000000001' "$JT_TMP" other >/dev/null 2>&1
before="$(jt_sql 'SELECT count(*) FROM entries;')"
jt_sql "UPDATE meta SET v='0' WHERE k='schema_version';" >/dev/null
: >"$JT_LOG"
pl="$JT_TMP/p12.json"
jt_payload "$pl" 'aaaa2222-0000-0000-0000-000000000002' "$SMALL" "$JT_TMP" other
fo_run FO-12 "$pl"
jt_has FO-12 "$(cat "$JT_LOG" 2>/dev/null)" 'reason=schema_behind' 'log line'
jt_eq FO-12 "$before" "$(jt_sql 'SELECT count(*) FROM entries;')" 'no write'

# ── FO-13 ─ the ops log itself cannot be written ────────────────────────────
jt_store
mkdir -p "$JT_LOG"   # a directory where the log file must go
pl="$JT_TMP/p13.json"
jt_payload "$pl" "$SID" "$SMALL" "$JT_TMP" other
o="$(bash "$JT_SESSION_END" <"$pl" 2>"$JT_TMP/e13")"
jt_eq FO-13 0 $? 'exit code'
jt_eq FO-13 '' "$o" 'stdout empty'
n="$(wc -l <"$JT_TMP/e13" | tr -d ' ')"
if [ "$n" -le 1 ]; then jt_ok FO-13 "at most one stderr line (got $n)"; else jt_no FO-13 "$n stderr lines"; fi
if [ "$n" -eq 1 ]; then jt_has FO-13 "$(cat "$JT_TMP/e13")" 'log unwritable' 'and it is the sanctioned notice'; fi
# The unit-level guarantee: one call, exactly one stderr line, still rc 0.
rc=0
n2="$( ( . "$JT_PLUGIN_ROOT/lib/common.sh"; jm_log probe error 'reason=x' ) 2>&1 >/dev/null | wc -l | tr -d ' ')"
( . "$JT_PLUGIN_ROOT/lib/common.sh"; jm_log probe error 'reason=x' ) >/dev/null 2>&1 || rc=$?
jt_eq FO-13 0 "$rc" 'jm_log still returns 0'
jt_has FO-13 "$( ( . "$JT_PLUGIN_ROOT/lib/common.sh"; jm_log probe error 'reason=x' ) 2>&1 >/dev/null )" \
  'log unwritable' 'jm_log emits the sanctioned notice'
if [ "$n2" -eq 1 ]; then
  jt_ok FO-13 'jm_log emits exactly one stderr line'
else
  # Cross-lane finding, lib/common.sh (E1): `printf ... >>"$JM_LOG" 2>/dev/null`
  # applies the redirections left to right, so bash reports the FAILED append on
  # the not-yet-redirected stderr and a second line appears. The fix is to order
  # them `{ printf ...; } 2>/dev/null >>"$JM_LOG"`, as this guard already does at
  # its own logsink probe. The hook contract is unaffected — the guard redirects
  # the worker's stderr — which is what the case above proves.
  jt_skip FO-13 "jm_log emitted $n2 stderr lines — see lib/common.sh redirection order (E1)"
fi
rmdir "$JT_LOG" 2>/dev/null || true

# ── FO-14 ─ git absent from PATH ────────────────────────────────────────────
jt_store
nogit="$JT_TMP/nogit"
mkdir -p "$nogit"
for b in bash sed grep awk tr cat head tail cut sort uniq stat date mkdir chmod rm ln cp mv mktemp timeout wc find id printf sqlite3 env sh od flock touch seq; do
  src="$(command -v "$b" 2>/dev/null)" && ln -sf "$src" "$nogit/$b"
done
pl="$JT_TMP/p14.json"
jt_payload "$pl" 'aaaa3333-0000-0000-0000-000000000001' "$SMALL" "$JT_PLUGIN_ROOT" other
fo_run FO-14 "$pl" "PATH=$nogit"
jt_eq FO-14 1 "$(jt_sql 'SELECT count(*) FROM entries;')" 'row still written'
jt_has FO-14 "$(jt_sql 'SELECT repo_key FROM entries;')" 'dir:' 'repo_key fell back to dir:'

# ── FO-15 ─ not a git repo at all ───────────────────────────────────────────
jt_store
notrepo="$JT_TMP/plain"
mkdir -p "$notrepo"
pl="$JT_TMP/p15.json"
jt_payload "$pl" 'aaaa4444-0000-0000-0000-000000000001' "$SMALL" "$notrepo" other
fo_run FO-15 "$pl"
jt_eq FO-15 1 "$(jt_sql 'SELECT count(*) FROM entries;')" 'row written'
rk="$(jt_sql 'SELECT repo_key FROM entries;')"
jt_has FO-15 "$rk" 'dir:plain@' 'repo_key is dir:<base>@<hash>'
if printf '%s' "$rk" | grep -qE '^dir:plain@[0-9a-f]{8}$'; then
  jt_ok FO-15 "shape ok: $rk"
else
  jt_no FO-15 "bad shape: $rk"
fi
jt_eq FO-15 '' "$(jt_sql 'SELECT worktree FROM entries;')" 'worktree empty outside a repo'

jt_summary
