#!/usr/bin/env bash
# jhh3-mem — tests/t-capture.sh   SPEC §14.2 (CAP-1 .. CAP-15)
# Plain bash; each case builds a throwaway store under $(mktemp -d).
set -uo pipefail

_t_src="${BASH_SOURCE[0]}"
case "$_t_src" in */*) _t_dir="${_t_src%/*}" ;; *) _t_dir="." ;; esac
JT_SUITE=capture
# shellcheck source=./lib/e2-harness.sh
. "$_t_dir/lib/e2-harness.sh"
trap jt_cleanup EXIT INT TERM

SID='11111111-2222-3333-4444-555555555555'

# ── CAP-1 ─ capture transcript-small.jsonl ──────────────────────────────────
jt_store
out="$(jt_capture "$JT_FIXTURES/transcript-small.jsonl" "$SID" "$JT_TMP" other 2>&1)"
rc=$?
jt_eq CAP-1 0 "$rc" 'exit code'
jt_eq CAP-1 '' "$out" 'stdout+stderr silent'
jt_eq CAP-1 1 "$(jt_sql 'SELECT count(*) FROM entries;')" 'row count'
jt_eq CAP-1 'Flag registry keeps rooms across reconnect' \
  "$(jt_sql 'SELECT title FROM entries;')" 'title from aiTitle'
_cap_ver="$(grep -oE '^JM_VERSION="?[0-9.]+' "$JT_PLUGIN_ROOT/lib/common.sh" | grep -oE '[0-9.]+')"
jt_eq CAP-1 "live|session|deterministic|jhh3-mem/${_cap_ver}" \
  "$(jt_sql "SELECT origin||'|'||kind||'|'||capture_mode||'|'||producer FROM entries;")" 'classification'
jt_eq CAP-1 2 "$(jt_sql 'SELECT n_mod FROM entries;')" 'n_mod'
jt_eq CAP-1 2 "$(jt_sql 'SELECT n_read FROM entries;')" 'n_read'
jt_eq CAP-1 'npm' "$(jt_sql 'SELECT commands FROM entries;')" 'command heads'
jt_eq CAP-1 'flagregistrykeepsroomsacrossreconnect' \
  "$(jt_sql 'SELECT dedup_key FROM entries;')" 'dedup_key'
jt_eq CAP-1 '1.62' "$(jt_sql 'SELECT durability FROM entries;')" 'durability 1.20x1.35x1.00'
jt_eq CAP-1 2 "$(jt_sql 'SELECT count(*) FROM entry_file;')" 'entry_file rows'
jt_eq CAP-1 'ok' "$(jt_sql "SELECT outcome FROM ops_log WHERE event='session_end';")" 'ops_log'
jt_eq CAP-1 'ok' "$(jt_sql "INSERT INTO entries_fts(entries_fts) VALUES('integrity-check'); SELECT 'ok';")" 'fts integrity'
jt_eq CAP-1 1 "$(jt_sql "SELECT count(*) FROM entries_fts WHERE entries_fts MATCH 'reconnect';")" 'fts searchable'

# ── CAP-2 ─ re-run unchanged is a zero-row no-op ────────────────────────────
jt_capture "$JT_FIXTURES/transcript-small.jsonl" "$SID" "$JT_TMP" other >/dev/null 2>&1
jt_eq CAP-2 0 $? 'exit code'
jt_eq CAP-2 1 "$(jt_sql 'SELECT count(*) FROM entries;')" 'still one row'
jt_eq CAP-2 'noop' "$(jt_sql 'SELECT outcome FROM ops_log ORDER BY id DESC LIMIT 1;')" 'ops_log noop'
jt_eq CAP-2 0 "$(jt_sql 'SELECT rows_written FROM ops_log ORDER BY id DESC LIMIT 1;')" 'rows_written'

# ── CAP-3 ─ a grown transcript supersedes the older row ─────────────────────
grown="$JT_TMP/grown.jsonl"
cp "$JT_FIXTURES/transcript-small.jsonl" "$grown"
{
  printf '%s\n' '{"type":"assistant","timestamp":"2026-08-17T10:05:00.000Z","cwd":"/repo","message":{"role":"assistant","content":[{"type":"tool_use","name":"Edit","input":{"file_path":"/repo/lib/flags/rooms.ts"}}]}}'
  printf '%s\n' '{"type":"user","promptSource":"typed","timestamp":"2026-08-17T10:05:10.000Z","cwd":"/repo","message":{"role":"user","content":"One more pass on rooms.ts please."}}'
  printf '%s\n' '{"type":"assistant","timestamp":"2026-08-17T10:06:00.000Z","cwd":"/repo","message":{"role":"assistant","content":[{"type":"text","text":"rooms.ts updated as well."}]}}'
} >>"$grown"
jt_capture "$grown" "$SID" "$JT_TMP" clear >/dev/null 2>&1
jt_eq CAP-3 2 "$(jt_sql "SELECT count(*) FROM entries WHERE uid='$SID';")" 'two rows for the uid'
jt_eq CAP-3 '1|0' \
  "$(jt_sql "SELECT group_concat(superseded,'|') FROM (SELECT superseded FROM entries WHERE uid='$SID' ORDER BY source_bytes);")" \
  'older superseded, newer live'

# ── CAP-4 ─ a real ~4 MB transcript ─────────────────────────────────────────
big="$JT_TMP/big.jsonl"
# shellcheck disable=SC2012  # sorting by SIZE is the point; find cannot do it
real="$(LC_ALL=C ls -S "$HOME"/.claude/projects/*/*.jsonl 2>/dev/null | head -1)"
if [ -n "$real" ] && [ "$(stat -c%s "$real" 2>/dev/null || stat -f%z "$real" 2>/dev/null || echo 0)" -gt 1000000 ]; then
  cp "$real" "$big"
else
  # Synthetic stand-in: same shape, ~4 MB, so the case still runs on a clean host.
  {
    printf '%s\n' '{"type":"user","promptSource":"typed","timestamp":"2026-08-17T09:00:00.000Z","cwd":"/repo","message":{"role":"user","content":"Synthetic large transcript."}}'
    pad="$(head -c 9000 /dev/zero | tr '\0' 'x')"
    for i in $(seq 1 420); do
      printf '{"type":"assistant","timestamp":"2026-08-17T09:00:0%d.000Z","cwd":"/repo","message":{"role":"assistant","content":[{"type":"tool_use","name":"Edit","input":{"file_path":"/repo/src/mod%d.ts","new_string":"%s"}}]}}\n' \
        $((i % 10)) "$i" "$pad"
    done
    printf '%s\n' '{"type":"ai-title","aiTitle":"Synthetic large transcript capture"}'
    printf '%s\n' '{"type":"assistant","timestamp":"2026-08-17T09:10:00.000Z","cwd":"/repo","message":{"role":"assistant","content":[{"type":"text","text":"All modules updated."}]}}'
  } >"$big"
fi
jt_store
t0=$(date +%s%N)
jt_capture "$big" 'aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee' "$JT_TMP" other >/dev/null 2>&1
rc=$?
ms=$(((  $(date +%s%N) - t0 ) / 1000000 ))
jt_eq CAP-4 0 "$rc" 'exit code'
if [ "$ms" -lt 400 ]; then jt_ok CAP-4 "completed in ${ms}ms (< 400)"; else jt_no CAP-4 "took ${ms}ms (>= 400)"; fi
jt_ne CAP-4 '' "$(jt_sql 'SELECT files_touched FROM entries;')" 'files_touched'
if [ "$(jt_sql 'SELECT n_mod FROM entries;')" -gt 0 ]; then jt_ok CAP-4 'n_mod > 0'; else jt_no CAP-4 'n_mod == 0'; fi

# ── CAP-5 ─ truncated transcript still yields a row ─────────────────────────
jt_store
jt_capture "$JT_FIXTURES/transcript-truncated.jsonl" 'cafe0000-0000-0000-0000-000000000001' "$JT_TMP" other >/dev/null 2>&1
jt_eq CAP-5 0 $? 'exit code'
jt_eq CAP-5 1 "$(jt_sql 'SELECT count(*) FROM entries;')" 'row written from valid lines'
jt_has CAP-5 "$(cat "$JT_LOG" 2>/dev/null)" 'degraded	reason=partial_transcript' 'log line'

# ── CAP-6 ─ zero valid JSON lines ───────────────────────────────────────────
jt_store
out="$(jt_capture "$JT_FIXTURES/transcript-novalid.jsonl" 'cafe0000-0000-0000-0000-000000000002' "$JT_TMP" other 2>&1)"
jt_eq CAP-6 1 $? 'worker rc 1 (it failed, and said so)'
jt_eq CAP-6 '' "$out" 'silent'
jt_eq CAP-6 0 "$(jt_sql 'SELECT count(*) FROM entries;')" 'no rows'
jt_has CAP-6 "$(cat "$JT_LOG" 2>/dev/null)" 'reason=no_valid_json' 'log line'
# The exit-0 guarantee belongs to the hook, not the worker: prove it at the hook.
jt_store
pl6="$JT_TMP/p6.json"
jt_payload "$pl6" 'cafe0000-0000-0000-0000-000000000002' "$JT_FIXTURES/transcript-novalid.jsonl" "$JT_TMP" other
out="$(bash "$JT_SESSION_END" <"$pl6" 2>"$JT_TMP/e6")"
jt_eq CAP-6 0 $? 'hook exit code 0'
jt_eq CAP-6 '' "$out$(cat "$JT_TMP/e6")" 'hook stdout+stderr empty'

# ── CAP-7 ─ trivial session is skipped ──────────────────────────────────────
jt_store
jt_capture "$JT_FIXTURES/transcript-trivial.jsonl" 'cafe0000-0000-0000-0000-000000000003' "$JT_TMP" other >/dev/null 2>&1
jt_eq CAP-7 0 $? 'exit code'
jt_eq CAP-7 0 "$(jt_sql 'SELECT count(*) FROM entries;')" 'no row'
jt_has CAP-7 "$(cat "$JT_LOG" 2>/dev/null)" 'noop	reason=trivial_session' 'log line'

# ── CAP-8 ─ promptSource filtering ──────────────────────────────────────────
jt_store
jt_capture "$JT_FIXTURES/transcript-small.jsonl" "$SID" "$JT_TMP" other >/dev/null 2>&1
req="$(jt_sql 'SELECT request FROM entries;')"
jt_has    CAP-8 "$req" 'Fix the flag registry'   'typed prompt present'
jt_has    CAP-8 "$req" 'Also add a regression'   'second typed prompt present'
jt_hasnt  CAP-8 "$req" 'command-name'            'slash-command noise'
jt_hasnt  CAP-8 "$req" 'local-command-stdout'    'command stdout noise'
jt_hasnt  CAP-8 "$req" 'slashcommandnoise'       'command stdout body'

# ── CAP-9 ─ --dry-run prints the row and writes nothing ─────────────────────
jt_store
out="$(jt_capture "$JT_FIXTURES/transcript-small.jsonl" "$SID" "$JT_TMP" other --dry-run 2>/dev/null)"
jt_eq CAP-9 0 $? 'exit code'
jt_has CAP-9 "$out" 'Flag registry keeps rooms across reconnect' 'row printed'
jt_has CAP-9 "$out" 'durability    1.62' 'scalars printed'
if [ -f "$JT_DB" ]; then jt_no CAP-9 'db was created'; else jt_ok CAP-9 'no db written'; fi

# ── CAP-10 ─ 60 distinct edited files ───────────────────────────────────────
jt_store
many="$JT_TMP/many.jsonl"
{
  printf '%s\n' '{"type":"user","promptSource":"typed","timestamp":"2026-08-17T12:00:00.000Z","cwd":"/repo","message":{"role":"user","content":"Touch sixty files."}}'
  for i in $(seq -w 1 60); do
    printf '{"type":"assistant","timestamp":"2026-08-17T12:00:01.000Z","cwd":"/repo","message":{"role":"assistant","content":[{"type":"tool_use","name":"Edit","input":{"file_path":"/repo/src/f%s.ts"}}]}}\n' "$i"
  done
  printf '%s\n' '{"type":"ai-title","aiTitle":"Sixty file sweep"}'
} >"$many"
jt_capture "$many" 'cafe0000-0000-0000-0000-000000000010' "$JT_TMP" other >/dev/null 2>&1
jt_eq CAP-10 60 "$(jt_sql 'SELECT n_mod FROM entries;')" 'n_mod counts all 60'
jt_eq CAP-10 40 "$(jt_sql "SELECT length(files_touched)-length(replace(files_touched,char(10),''))+1 FROM entries;")" 'stored list is 40 lines'
jt_eq CAP-10 40 "$(jt_sql 'SELECT count(*) FROM entry_file;')" 'entry_file rows'

# ── CAP-11 ─ every field at 2x its CHECK limit ──────────────────────────────
jt_store
huge="$JT_TMP/huge.jsonl"
big_title="$(head -c 400 /dev/zero | tr '\0' 'T')"
big_prompt="$(head -c 4000 /dev/zero | tr '\0' 'P')"
big_text="$(head -c 8000 /dev/zero | tr '\0' 'O')"
{
  printf '{"type":"user","promptSource":"typed","timestamp":"2026-08-17T13:00:00.000Z","cwd":"/repo","message":{"role":"user","content":"%s"}}\n' "$big_prompt"
  for i in $(seq 1 30); do
    printf '{"type":"assistant","timestamp":"2026-08-17T13:00:01.000Z","cwd":"/repo","message":{"role":"assistant","content":[{"type":"tool_use","name":"Edit","input":{"file_path":"/repo/%s/deeply/nested/path/segment/file%s.ts"}}]}}\n' \
      "$(head -c 200 /dev/zero | tr '\0' 'd')" "$i"
    printf '{"type":"assistant","timestamp":"2026-08-17T13:00:02.000Z","cwd":"/repo","message":{"role":"assistant","content":[{"type":"tool_use","name":"Bash","input":{"command":"cmd%s-%s --flag"}}]}}\n' \
      "$i" "$(head -c 40 /dev/zero | tr '\0' 'c')"
  done
  printf '{"type":"ai-title","aiTitle":"%s"}\n' "$big_title"
  printf '{"type":"assistant","timestamp":"2026-08-17T13:00:09.000Z","cwd":"/repo","message":{"role":"assistant","content":[{"type":"text","text":"%s"}]}}\n' "$big_text"
} >"$huge"
out="$(jt_capture "$huge" 'cafe0000-0000-0000-0000-000000000011' "$JT_TMP" other 2>&1)"
jt_eq CAP-11 0 $? 'exit code'
jt_eq CAP-11 '' "$out" 'silent'
jt_eq CAP-11 1 "$(jt_sql 'SELECT count(*) FROM entries;')" 'insert succeeded'
jt_eq CAP-11 '200|2000|4000|600' \
  "$(jt_sql "SELECT length(title)||'|'||length(request)||'|'||length(outcome)||'|'||length(commands) FROM entries;")" \
  'title/request/outcome/commands exactly at their limits'
# files_touched is line-structured: jm_truncate_bytes cuts at the last newline at
# or before the cap, so it lands on the largest whole-path prefix — never a half
# path. Under the cap, and within one path of it.
ft="$(jt_sql 'SELECT length(files_touched) FROM entries;')"
if [ "$ft" -le 4000 ] && [ "$ft" -gt 3700 ]; then
  jt_ok CAP-11 "files_touched $ft <= 4000, cut on a path boundary"
else
  jt_no CAP-11 "files_touched length $ft"
fi
jt_eq CAP-11 0 "$(jt_sql "SELECT count(*) FROM entries WHERE files_touched LIKE '%'||char(10);")" 'no trailing empty path'
jt_hasnt CAP-11 "$(cat "$JT_LOG" 2>/dev/null)" 'CHECK' 'no CHECK error reached the log'

# ── CAP-12 ─ a 65 MiB transcript ────────────────────────────────────────────
jt_store
if [ "${JHH3_MEM_TEST_BIG:-1}" = '0' ]; then
  jt_skip CAP-12 'JHH3_MEM_TEST_BIG=0'
else
  vast="$JT_TMP/vast.jsonl"
  pad="$(head -c 60000 /dev/zero | tr '\0' 'z')"
  {
    printf '%s\n' '{"type":"user","promptSource":"typed","timestamp":"2026-08-17T14:00:00.000Z","cwd":"/repo","message":{"role":"user","content":"Enormous transcript."}}'
    for i in $(seq 1 1140); do
      printf '{"type":"assistant","timestamp":"2026-08-17T14:00:01.000Z","cwd":"/repo","message":{"role":"assistant","content":[{"type":"tool_use","name":"Edit","input":{"file_path":"/repo/src/v%s.ts","new_string":"%s"}}]}}\n' "$i" "$pad"
    done
    printf '%s\n' '{"type":"ai-title","aiTitle":"Enormous transcript capture"}'
    printf '%s\n' '{"type":"assistant","timestamp":"2026-08-17T14:30:00.000Z","cwd":"/repo","message":{"role":"assistant","content":[{"type":"text","text":"Finished the enormous run."}]}}'
  } >"$vast"
  vs="$(stat -c%s "$vast" 2>/dev/null || stat -f%z "$vast")"
  jt_capture "$vast" 'cafe0000-0000-0000-0000-000000000012' "$JT_TMP" other >/dev/null 2>&1
  jt_eq CAP-12 0 $? 'exit code'
  if [ "$vs" -gt 67108864 ]; then jt_ok CAP-12 "fixture is $vs bytes (> 64 MiB)"; else jt_no CAP-12 "fixture only $vs bytes"; fi
  jt_eq CAP-12 1 "$(jt_sql 'SELECT count(*) FROM entries;')" 'row written'
  jt_has CAP-12 "$(cat "$JT_LOG" 2>/dev/null)" 'reason=transcript_too_large' 'log line'
  jt_has CAP-12 "$(jt_sql 'SELECT end_reason FROM entries;')" '+truncated' 'end_reason suffix'
  jt_eq CAP-12 "$vs" "$(jt_sql 'SELECT source_bytes FROM entries;')" 'source_bytes is the full size'
  rm -f "$vast"
fi

# ── CAP-13 / CAP-14 ─ backfill sweep ────────────────────────────────────────
jt_store
bdir="$JT_TMP/proj"
mkdir -p "$bdir"
for i in 1 2 3 4 5; do
  sed "s/Flag registry/Backfill session $i/" "$JT_FIXTURES/transcript-small.jsonl" \
    >"$bdir/bf00000$i-0000-0000-0000-00000000000$i.jsonl"
done
# three stale (older than 600 s), two fresh
touch -t 202311140000 "$bdir/bf000001-0000-0000-0000-000000000001.jsonl" \
  "$bdir/bf000002-0000-0000-0000-000000000002.jsonl" \
  "$bdir/bf000003-0000-0000-0000-000000000003.jsonl"
# The sweep never bootstraps (SessionEnd owns that), so seed the store first.
jt_capture "$JT_FIXTURES/transcript-small.jsonl" 'seed0000-0000-0000-0000-000000000000' "$JT_TMP" other >/dev/null 2>&1
n="$(bash "$JT_CAPTURE" --backfill "$bdir" --max 3 --seconds 10 2>/dev/null)"
jt_eq CAP-13 3 "$n" 'captured count'
jt_eq CAP-13 4 "$(jt_sql 'SELECT count(*) FROM entries;')" 'rows (seed + 3)'
jt_eq CAP-13 3 "$(jt_sql "SELECT count(*) FROM entries WHERE end_reason='backfill';")" 'end_reason'
jt_eq CAP-13 0 "$(jt_sql "SELECT count(*) FROM entries WHERE uid LIKE 'bf000004%' OR uid LIKE 'bf000005%';")" 'fresh siblings untouched'
jt_eq CAP-13 0 "$(bash "$JT_CAPTURE" --backfill "$bdir" --max 3 --seconds 10 2>/dev/null)" 'idempotent second sweep'
jt_eq CAP-14 0 "$(JHH3_MEM_BACKFILL=0 bash "$JT_CAPTURE" --backfill "$bdir" --max 3 --seconds 10 2>/dev/null)" 'JHH3_MEM_BACKFILL=0'

# ── CAP-15 ─ durability floor for an ungrounded live session ────────────────
# shellcheck source=../lib/capture.sh
JM_CAPTURE_SOURCED='' . "$JT_PLUGIN_ROOT/lib/capture.sh"
jt_eq CAP-15 '0.84' "$(jm_capture_durability session 0 0 0)" 'session, no files'
jt_eq CAP-15 '1.62' "$(jm_capture_durability session 1 0 0)" 'session, files modified'
jt_eq CAP-15 '1.38' "$(jm_capture_durability session 0 1 0)" 'session, files read'
jt_eq CAP-15 '0.29' "$(jm_capture_durability session 0 0 1)" 'ephemeral penalty'
jt_eq CAP-15 '0.56' "$(jm_capture_durability decision 0 1 1)" 'SPEC §5.3 worked example'
jt_eq CAP-15 '2.03' "$(jm_capture_durability bugfix 1 1 0)" 'SPEC §5.3 upper bound'
jt_eq CAP-15 '0.21' "$(jm_capture_durability discovery 0 0 1)" 'SPEC §5.3 lower bound'

# ── extra ─ the hook guard end to end ───────────────────────────────────────
jt_store
pl="$JT_TMP/payload-end.json"
jt_payload "$pl" "$SID" "$JT_FIXTURES/transcript-small.jsonl" "$JT_TMP" other
out="$(bash "$JT_SESSION_END" <"$pl" 2>"$JT_TMP/err")"
jt_eq HOOK 0 $? 'guard exit code'
jt_eq HOOK '' "$out" 'guard stdout empty'
jt_eq HOOK '' "$(cat "$JT_TMP/err")" 'guard stderr empty'
jt_eq HOOK 1 "$(jt_sql 'SELECT count(*) FROM entries;')" 'row written by the hook'
jt_eq HOOK 'other' "$(jt_sql 'SELECT end_reason FROM entries;')" 'reason from the payload'

jt_summary
