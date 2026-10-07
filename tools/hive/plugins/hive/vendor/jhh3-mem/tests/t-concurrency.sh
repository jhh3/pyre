#!/usr/bin/env bash
# jhh3-mem — tests/t-concurrency.sh   SPEC §14.7 (CON-1 .. CON-8)
set -uo pipefail

_t_src="${BASH_SOURCE[0]}"
case "$_t_src" in */*) _t_dir="${_t_src%/*}" ;; *) _t_dir="." ;; esac
JT_SUITE=concurrency
# shellcheck source=./lib/e2-harness.sh
. "$_t_dir/lib/e2-harness.sh"
trap jt_cleanup EXIT INT TERM

SMALL="$JT_PLUGIN_ROOT/tests/fixtures/transcript-small.jsonl"

# con_variant N OUT — a distinct transcript with its own title and edited file.
con_variant() {
  sed -e "s#Flag registry keeps rooms across reconnect#Concurrent capture number $1#" \
      -e "s#/repo/lib/flags/registry.test.ts#/repo/lib/flags/c$1.test.ts#" \
      "$SMALL" >"$2"
}

# ── CON-1 ─ 8 concurrent captures of DISTINCT transcripts ───────────────────
jt_store
jt_capture "$SMALL" 'seed0000-0000-0000-0000-000000000000' "$JT_TMP" other >/dev/null 2>&1
for i in 1 2 3 4 5 6 7 8; do con_variant "$i" "$JT_TMP/c$i.jsonl"; done
rcfile="$JT_TMP/rcs"
: >"$rcfile"
t0=$(date +%s%N)
for i in 1 2 3 4 5 6 7 8; do
  ( jt_capture "$JT_TMP/c$i.jsonl" "c0000000-0000-0000-0000-00000000000$i" "$JT_TMP" other >/dev/null 2>&1
    printf '%s\n' "$?" >>"$rcfile" ) &
done
wait
ms=$(( ( $(date +%s%N) - t0 ) / 1000000 ))
jt_eq CON-1 8 "$(wc -l <"$rcfile" | tr -d ' ')" 'all 8 finished'
jt_eq CON-1 0 "$(grep -cv '^0$' "$rcfile" || true)" 'zero non-zero exit codes'
jt_eq CON-1 9 "$(jt_sql 'SELECT count(*) FROM entries;')" 'seed + 8 rows'
jt_eq CON-1 'ok' "$(jt_sql 'PRAGMA quick_check;')" 'PRAGMA quick_check'
jt_eq CON-1 'ok' "$(jt_sql "INSERT INTO entries_fts(entries_fts) VALUES('integrity-check'); SELECT 'ok';")" 'fts integrity'
jt_ok CON-1 "8 concurrent captures in ${ms}ms"
jt_eq CON-1 0 "$(grep -c 'busy\|error' "$JT_LOG" || true)" 'no busy/error lines'

# ── CON-2 ─ 8 concurrent captures of the SAME transcript ────────────────────
jt_store
jt_capture "$SMALL" 'seed0000-0000-0000-0000-000000000000' "$JT_TMP" other >/dev/null 2>&1
: >"$rcfile"
for _ in 1 2 3 4 5 6 7 8; do
  ( jt_capture "$SMALL" 'same0000-0000-0000-0000-000000000001' "$JT_TMP" other >/dev/null 2>&1
    printf '%s\n' "$?" >>"$rcfile" ) &
done
wait
jt_eq CON-2 0 "$(grep -cv '^0$' "$rcfile" || true)" 'zero non-zero exit codes'
jt_eq CON-2 1 "$(jt_sql "SELECT count(*) FROM entries WHERE uid='same0000-0000-0000-0000-000000000001';")" 'exactly one row'
jt_eq CON-2 'ok' "$(jt_sql 'PRAGMA quick_check;')" 'PRAGMA quick_check'

# ── CON-3 ─ a held write lock past busy_timeout ─────────────────────────────
jt_store
jt_capture "$SMALL" 'seed0000-0000-0000-0000-000000000000' "$JT_TMP" other >/dev/null 2>&1
: >"$JT_LOG"
( printf 'BEGIN IMMEDIATE;\nSELECT 1;\n'; sleep 3; printf 'COMMIT;\n' ) | sqlite3 "$JT_DB" >/dev/null 2>&1 &
holder=$!
sleep 0.4
pl="$JT_TMP/p3.json"
jt_payload "$pl" 'busy0000-0000-0000-0000-000000000001' "$SMALL" "$JT_TMP" other
o="$(JHH3_MEM_BUSY_WRITE_MS=150 bash "$JT_SESSION_END" <"$pl" 2>"$JT_TMP/e3")"
rc=$?
kill "$holder" 2>/dev/null || true
wait "$holder" 2>/dev/null || true
jt_eq CON-3 0 "$rc" 'guard exit 0'
jt_eq CON-3 '' "$o$(cat "$JT_TMP/e3")" 'guard silent'
jt_has CON-3 "$(cat "$JT_LOG" 2>/dev/null)" 'busy' 'busy logged'
jt_has CON-3 "$(cat "$JT_LOG" 2>/dev/null)" 'rc=5' 'sqlite3 exit 5 recorded'
jt_eq CON-3 0 "$(jt_sql "SELECT count(*) FROM entries WHERE uid='busy0000-0000-0000-0000-000000000001';")" 'no partial row'
jt_eq CON-3 'ok' "$(jt_sql 'PRAGMA quick_check;')" 'db still healthy'

# ── CON-4 ─ kill -9 mid-transaction ─────────────────────────────────────────
jt_store
jt_capture "$SMALL" 'seed0000-0000-0000-0000-000000000000' "$JT_TMP" other >/dev/null 2>&1
before="$(jt_sql 'SELECT count(*) FROM entries;')"
fifo="$JT_TMP/txn.fifo"
rm -f "$fifo"; mkfifo "$fifo"
sqlite3 -bail "$JT_DB" <"$fifo" >/dev/null 2>&1 &
victim=$!
exec 9>"$fifo"
printf 'BEGIN IMMEDIATE;\n' >&9
printf "INSERT INTO entries(uid,source_bytes,origin,producer,repo_key,kind,ts,title) VALUES('killed-txn',1,'live','x','r','session',1,'Never Committed');\n" >&9
sleep 0.5
kill -9 "$victim" 2>/dev/null || true
exec 9>&-
wait "$victim" 2>/dev/null || true
rm -f "$fifo"
jt_eq CON-4 'ok' "$(jt_sql 'PRAGMA quick_check;')" 'PRAGMA quick_check'
jt_eq CON-4 0 "$(jt_sql "SELECT count(*) FROM entries WHERE uid='killed-txn';")" 'uncommitted row absent'
jt_eq CON-4 "$before" "$(jt_sql 'SELECT count(*) FROM entries;')" 'row count unchanged'
jt_capture "$SMALL" 'after000-0000-0000-0000-000000000001' "$JT_TMP" other >/dev/null 2>&1
jt_eq CON-4 0 $? 'the store is still usable afterwards'

# ── CON-5 ─ why -bail is mandatory ──────────────────────────────────────────
jt_store
jt_capture "$SMALL" 'seed0000-0000-0000-0000-000000000000' "$JT_TMP" other >/dev/null 2>&1
mk_script() {
  printf 'BEGIN IMMEDIATE;\n'
  printf "INSERT INTO entries(uid,source_bytes,origin,producer,repo_key,kind,ts,title) VALUES('%s-a',1,'live','x','r','session',1,'A');\n" "$1"
  printf 'INSERT INTO no_such_table(x) VALUES(1);\n'
  printf "INSERT INTO entries(uid,source_bytes,origin,producer,repo_key,kind,ts,title) VALUES('%s-b',1,'live','x','r','session',1,'B');\n" "$1"
  printf 'COMMIT;\n'
}
mk_script nobail | sqlite3 "$JT_DB" >/dev/null 2>&1 || true
jt_eq CON-5 2 "$(jt_sql "SELECT count(*) FROM entries WHERE uid LIKE 'nobail-%';")" 'without -bail: both good rows land'
mk_script withbail | sqlite3 -bail "$JT_DB" >/dev/null 2>&1 || true
jt_eq CON-5 0 "$(jt_sql "SELECT count(*) FROM entries WHERE uid LIKE 'withbail-%';")" 'with -bail: nothing lands'
# shellcheck disable=SC2016  # a grep pattern: $JM_DB must stay literal
jt_eq CON-5 0 "$(grep -c 'sqlite3 [^|]*-batch[^|]*"\$JM_DB"' "$JT_PLUGIN_ROOT/lib/capture.sh" || true)" \
  'capture.sh never opens the store without going through jm_db_write'

# ── CON-6 ─ a reader is never blocked by a writer ───────────────────────────
jt_store
jt_capture "$SMALL" 'seed0000-0000-0000-0000-000000000000' "$JT_TMP" other >/dev/null 2>&1
( printf 'BEGIN IMMEDIATE;\nSELECT 1;\n'; sleep 2; printf 'COMMIT;\n' ) | sqlite3 "$JT_DB" >/dev/null 2>&1 &
holder=$!
sleep 0.4
t0=$(date +%s%N)
n="$(sqlite3 -readonly -cmd '.timeout 1000' "file:$JT_DB?mode=ro" 'SELECT count(*) FROM entries;' 2>/dev/null)"
rms=$(( ( $(date +%s%N) - t0 ) / 1000000 ))
kill "$holder" 2>/dev/null || true
wait "$holder" 2>/dev/null || true
jt_eq CON-6 1 "$n" 'reader saw the committed row'
if [ "$rms" -lt 200 ]; then jt_ok CON-6 "reader returned in ${rms}ms"; else jt_no CON-6 "reader took ${rms}ms"; fi

# ── CON-7 ─ -shm removed while the DB is idle ───────────────────────────────
jt_store
jt_capture "$SMALL" 'seed0000-0000-0000-0000-000000000000' "$JT_TMP" other >/dev/null 2>&1
rm -f "$JT_DB-shm" "$JT_DB-wal"
n="$(sqlite3 -readonly -cmd '.timeout 1000' "file:$JT_DB?mode=ro" 'SELECT count(*) FROM entries;' 2>/dev/null)"
jt_eq CON-7 1 "$n" 'mode=ro read still succeeds'
jt_capture "$SMALL" 'shm00000-0000-0000-0000-000000000001' "$JT_TMP" other >/dev/null 2>&1
jt_eq CON-7 0 $? 'and a subsequent capture succeeds'
jt_eq CON-7 'wal' "$(jt_sql 'PRAGMA journal_mode;')" 'still WAL'

# ── CON-8 ─ ops_log is trimmed inside the capture transaction ───────────────
jt_store
jt_capture "$SMALL" 'seed0000-0000-0000-0000-000000000000' "$JT_TMP" other >/dev/null 2>&1
jt_sql "WITH RECURSIVE s(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM s WHERE i<6000)
        INSERT INTO ops_log(ts,event,outcome) SELECT 1,'filler','ok' FROM s;" >/dev/null
before="$(jt_sql 'SELECT count(*) FROM ops_log;')"
if [ "${before:-0}" -lt 6000 ]; then jt_no CON-8 "only $before filler rows"; else jt_ok CON-8 "$before rows before capture"; fi
jt_capture "$SMALL" 'trim0000-0000-0000-0000-000000000001' "$JT_TMP" other >/dev/null 2>&1
after="$(jt_sql 'SELECT count(*) FROM ops_log;')"
jt_eq CON-8 5000 "$after" 'trimmed to the last 5000'
jt_eq CON-8 'session_end' "$(jt_sql 'SELECT event FROM ops_log ORDER BY id DESC LIMIT 1;')" 'newest row survived'

jt_summary
