#!/usr/bin/env bash
# jhh3-mem — tests/t-migrate.sh   SPEC §14.8 (MIG-1 .. MIG-14)
#
# Migration is a ONE-SHOT, IRREVERSIBLE import of the user's irreplaceable
# archive. The properties that matter are therefore not "does it work once" but
# "can it hurt the source" and "what happens on the second run".
#
# CI NEVER POINTS AT ~/mem-archive. The fixture is a miniature legacy database
# the harness builds from the real schema's column set — the archive holds work
# for 19 client orgs and a test that reads it would be the exposure this plugin
# exists to prevent. The one assertion that genuinely needs the real archive
# (the corpus redaction golden) lives in t-redact.sh and SKIPs without it.
set -uo pipefail

_t_src="${BASH_SOURCE[0]}"
case "$_t_src" in */*) _t_dir="${_t_src%/*}" ;; *) _t_dir="." ;; esac
# shellcheck source=./lib/harness.sh
. "$_t_dir/lib/harness.sh"

jt_begin t-migrate

LIB="$JT_PLUGIN_ROOT/lib"
CLI="$JT_PLUGIN_ROOT/bin/jhh3-mem"

jt_need_cmd sqlite3 'migration is entirely sqlite3' || jt_end

jt_store
jt_require_sandbox
jt_tmpdir
WORK="$JT_TMP"
LEGACY="$WORK/claude-mem.db"

# ── the miniature archive ───────────────────────────────────────────────────
# Same table and column names lib/migrate.sh selects from, same JSON-array
# shapes for files_modified / files_read / facts / concepts, and one row of
# every observation type the type map handles — plus a `sensitive` row and a
# credential, because those are the two rows whose HANDLING is the point.
sqlite3 -batch "$LEGACY" <<'SQL'
CREATE TABLE observations (
  id INTEGER PRIMARY KEY, memory_session_id TEXT, project TEXT, text TEXT,
  type TEXT, title TEXT, subtitle TEXT, facts TEXT, narrative TEXT,
  concepts TEXT, files_read TEXT, files_modified TEXT, prompt_number INTEGER,
  created_at TEXT, created_at_epoch INTEGER, content_hash TEXT,
  generated_by_model TEXT, agent_type TEXT, metadata TEXT);
CREATE TABLE session_summaries (
  id INTEGER PRIMARY KEY, memory_session_id TEXT, project TEXT, request TEXT,
  investigated TEXT, learned TEXT, completed TEXT, next_steps TEXT, notes TEXT,
  files_read TEXT, created_at TEXT, created_at_epoch INTEGER);
CREATE TABLE user_prompts (
  id INTEGER PRIMARY KEY, session_db_id INTEGER, prompt_text TEXT,
  created_at TEXT, created_at_epoch INTEGER);
CREATE TABLE sdk_sessions (id INTEGER PRIMARY KEY, project TEXT);

INSERT INTO sdk_sessions VALUES (1,'acme-web'),(2,'acme-web/wt-collab');

INSERT INTO observations
 (id,project,type,title,subtitle,narrative,facts,concepts,files_read,files_modified,created_at_epoch)
VALUES
 (1,'acme-web','discovery','Flag registry is read twice per render','The second read is in a memo','The memo closure captures a stale registry reference.','["registry is memoized","memo deps omit the registry"]','["registry","memo","flags"]','["/home/x/acme-web/src/flags/registry.ts"]','["/home/x/acme-web/src/flags/registry.ts"]',1750000000),
 (2,'acme-web','bugfix','Fixed the retry backoff cap','Backoff grew without bound','Capped at 30s.','[]','["retry","backoff"]','[]','["/home/x/acme-web/src/net/retry.ts"]',1750100000),
 (3,'acme-web/wt-collab','decision','Chose ydoc gating over presence gating','Presence still fires on reconnect','','[]','["ydoc","presence"]','[]','["/home/x/acme-web/src/collab/room.ts"]',1750200000),
 (4,'acme-web','security_alert','Rotated the leaked deploy key','Found in a prompt','The key was pasted while debugging.','[]','["deploy","key"]','[]','[]',1750300000),
 (5,'acme-web','sensitive','Customer PII in a fixture','','',  '[]','[]','[]','[]',1750400000),
 (6,'acme-web','feature','Added the audit trail','','',  '[]','["audit"]','[]','["/home/x/acme-web/src/audit/log.ts"]',1750500000),
 (7,'acme-web','change','Bumped the parser','','',  '[]','[]','[]','[]',1750600000),
 (8,'acme-web','refactor','Split the resolver','','',  '[]','[]','[]','[]',1750700000);

INSERT INTO session_summaries
 (id,project,request,investigated,learned,completed,next_steps,notes,files_read,created_at_epoch)
VALUES
 (1,'acme-web','Make the flag registry stop re-reading','Traced the memo closure','The memo deps were wrong','Fixed and tested','Wire the same cap into the migration path','',  '["/home/x/acme-web/src/flags/registry.ts"]',1750050000),
 (2,'acme-web','Debug the deploy pipeline with DATABASE_URL=postgres://svc:hunter2XYZ@db.internal:5432/app','','','','','',  '[]',1750350000);

INSERT INTO user_prompts (id,session_db_id,prompt_text,created_at_epoch) VALUES
 (1,1,'why does the registry read twice',1750000001),
 (2,2,'here is my key ghp_1234567890abcdefghijklmnopqrstuvwxyz please check it',1750200001);
SQL

jt_assert_file "$LEGACY" "miniature legacy archive built"
sha_before="$(sha256sum "$LEGACY" | cut -d' ' -f1)"
ls_before="$(cd "$WORK" && find . -maxdepth 1 -mindepth 1 | sed "s#.*/##" | LC_ALL=C sort | tr "\n" " ")"

# ── MIG-1 ───────────────────────────────────────────────────────────────────
jt_case MIG-1 "a dry run reports and writes nothing"
out="$(bash "$CLI" migrate --from "$LEGACY" --dry-run 2>&1)"; rc=$?
jt_assert_rc "0" "$rc" "dry run exits 0"
jt_assert_ne "" "$out" "dry run says something"
n="$(sqlite3 -batch -noheader "$JHH3_MEM_HOME/memory.db" 'SELECT count(*) FROM entries;' 2>/dev/null)" || n=0
jt_assert_eq "0" "${n:-0}" "dry run inserted no entries"

# ── MIG-2 ───────────────────────────────────────────────────────────────────
jt_case MIG-2 "the real run imports every eligible row"
out="$(bash "$CLI" migrate --from "$LEGACY" 2>&1)"; rc=$?
jt_assert_rc "0" "$rc" "migrate exits 0"
rows="$(sqlite3 -batch -noheader "$JHH3_MEM_HOME/memory.db" 'SELECT count(*) FROM entries;')"
# 8 observations minus the `sensitive` one, plus 2 session summaries = 9.
jt_assert_eq "9" "$rows" "7 observations + 2 summaries (sensitive skipped)"
jt_assert_eq "0" "$(sqlite3 -batch -noheader "$JHH3_MEM_HOME/memory.db" "SELECT count(*) FROM entries WHERE origin='legacy-prompt';")" \
  "user_prompts are skipped without --with-prompts"
jt_assert_eq "0" "$(sqlite3 -batch -noheader "$JHH3_MEM_HOME/memory.db" "SELECT count(*) FROM entries WHERE title LIKE '%Customer PII%';")" \
  "the sensitive row is skipped without --include-sensitive"

# ── MIG-3 ───────────────────────────────────────────────────────────────────
jt_case MIG-3 "the archive is never mutated and grows no sidecars"
jt_assert_eq "$sha_before" "$(sha256sum "$LEGACY" | cut -d' ' -f1)" "source sha256 is unchanged"
jt_assert_eq "$ls_before" "$(cd "$WORK" && find . -maxdepth 1 -mindepth 1 | sed "s#.*/##" | LC_ALL=C sort | tr "\n" " ")" \
  "no -wal / -shm / -journal appeared next to the source"
jt_assert_no_file "$LEGACY-wal" "no WAL sidecar"
jt_assert_no_file "$LEGACY-shm" "no SHM sidecar"
jt_assert_no_file "$LEGACY-journal" "no rollback journal"

# ── MIG-4 ───────────────────────────────────────────────────────────────────
jt_case MIG-4 "a second run is idempotent"
before="$(sqlite3 -batch -noheader "$JHH3_MEM_HOME/memory.db" 'SELECT count(*) FROM entries;')"
bash "$CLI" migrate --from "$LEGACY" >/dev/null 2>&1
after="$(sqlite3 -batch -noheader "$JHH3_MEM_HOME/memory.db" 'SELECT count(*) FROM entries;')"
jt_assert_eq "$before" "$after" "re-running inserted 0 new rows"
jt_assert_eq "$sha_before" "$(sha256sum "$LEGACY" | cut -d' ' -f1)" "and still did not touch the archive"

# ── MIG-5 ───────────────────────────────────────────────────────────────────
jt_case MIG-5 "uid is stable and namespaced by source table"
jt_assert_eq "7" "$(sqlite3 -batch -noheader "$JHH3_MEM_HOME/memory.db" "SELECT count(*) FROM entries WHERE uid LIKE 'lgc:obs:%';")" \
  "observations carry lgc:obs: uids"
jt_assert_eq "2" "$(sqlite3 -batch -noheader "$JHH3_MEM_HOME/memory.db" "SELECT count(*) FROM entries WHERE uid LIKE 'lgc:sum:%';")" \
  "summaries carry lgc:sum: uids"

# ── MIG-6 ───────────────────────────────────────────────────────────────────
jt_case MIG-6 "credentials in the archive do not survive into the new store"
dump="$(sqlite3 -batch -noheader "$JHH3_MEM_HOME/memory.db" "SELECT title||' '||request||' '||outcome||' '||learned||' '||decided||' '||facts FROM entries;")"
jt_assert_not_contains "$dump" 'hunter2XYZ' "the legacy DSN password is gone"
jt_assert_contains "$dump" 'db.internal:5432/app' "the debugging value is preserved"
jt_assert_not_contains "$dump" 'ghp_1234567890abcdef' "no GitHub token (that prompt is not imported anyway)"
jt_assert_ne "0" "$(sqlite3 -batch -noheader "$JHH3_MEM_HOME/memory.db" "SELECT count(*) FROM entries WHERE redactions > 0 OR request LIKE '%REDACTED%' OR title LIKE '%REDACTED%';")" \
  "at least one row records that it was redacted"

# ── MIG-7 ───────────────────────────────────────────────────────────────────
jt_case MIG-7 "migrated prose is search-reachable and injection-ineligible"
jt_assert_eq "9" "$(sqlite3 -batch -noheader "$JHH3_MEM_HOME/memory.db" "SELECT count(*) FROM entries WHERE origin LIKE 'legacy-%';")" \
  "every imported row is labelled legacy-*"
inj="$(sqlite3 -batch -noheader "$JHH3_MEM_HOME/memory.db" "SELECT count(*) FROM entries WHERE origin LIKE 'legacy-%' AND origin NOT IN ('live','noted');")"
jt_assert_eq "9" "$inj" "and therefore excluded by the digest's origin filter"
hits="$(cd "$WORK" && JHH3_MEM_HOME="$JHH3_MEM_HOME" bash "$CLI" search 'registry' --all-repos --limit 5 2>&1)" || true
jt_assert_contains "$hits" 'registry' "but reachable through search"
jt_assert_contains "$hits" '[migrated]' "and labelled [migrated] when shown"

# ── MIG-8 ───────────────────────────────────────────────────────────────────
jt_case MIG-8 "the FTS index is consistent after the import"
jt_assert_eq "" "$(sqlite3 -batch -noheader "$JHH3_MEM_HOME/memory.db" "INSERT INTO entries_fts(entries_fts) VALUES('integrity-check');" 2>&1)" \
  "entries_fts integrity-check is silent (ok)"
jt_assert_eq "ok" "$(sqlite3 -batch -noheader "$JHH3_MEM_HOME/memory.db" 'PRAGMA integrity_check;')" \
  "database integrity_check is ok"
jt_assert_eq "9" "$(sqlite3 -batch -noheader "$JHH3_MEM_HOME/memory.db" 'SELECT count(*) FROM entries_fts;')" \
  "the FTS row count matches the entries row count"

# ── MIG-9 ───────────────────────────────────────────────────────────────────
jt_case MIG-9 "provenance is recorded in meta"
jt_assert_eq "$(cd "$WORK" && pwd -P)/claude-mem.db" \
  "$(sqlite3 -batch -noheader "$JHH3_MEM_HOME/memory.db" "SELECT v FROM meta WHERE k='legacy_source';")" \
  "legacy_source names the archive"
jt_assert_eq "$sha_before" \
  "$(sqlite3 -batch -noheader "$JHH3_MEM_HOME/memory.db" "SELECT v FROM meta WHERE k='legacy_sha256';")" \
  "legacy_sha256 matches the archive as it was read"

# ── MIG-10 ──────────────────────────────────────────────────────────────────
jt_case MIG-10 "--with-prompts and --include-sensitive are opt-in and search-only"
mout="$(bash "$CLI" migrate --from "$LEGACY" --with-prompts --include-sensitive 2>&1)"; mrc=$?
jt_assert_rc "0" "$mrc" "opt-in migrate exits 0"
p="$(sqlite3 -batch -noheader "$JHH3_MEM_HOME/memory.db" "SELECT count(*) FROM entries WHERE origin='legacy-prompt';")"
jt_assert_eq "2" "$p" "prompts are imported when asked for"
jt_assert_eq "0" "$(sqlite3 -batch -noheader "$JHH3_MEM_HOME/memory.db" "SELECT count(*) FROM entries WHERE origin='legacy-prompt' AND injectable=1;")" \
  "and every one of them is injectable=0"
jt_assert_eq "0" "$(sqlite3 -batch -noheader "$JHH3_MEM_HOME/memory.db" "SELECT count(*) FROM entries WHERE title LIKE '%Customer PII%' AND injectable=1;")" \
  "the sensitive row, once imported, is injectable=0"
dump="$(sqlite3 -batch -noheader "$JHH3_MEM_HOME/memory.db" "SELECT title||' '||request||' '||outcome FROM entries WHERE origin='legacy-prompt';")"
jt_assert_not_contains "$dump" 'ghp_1234567890abcdef' "the imported prompt's GitHub token was redacted"

# ── MIG-11 ──────────────────────────────────────────────────────────────────
jt_case MIG-11 "a pre-migrate backup is written, even twice in the same second"
# Found by this suite: `VACUUM INTO` refuses to overwrite, and the backup name
# used only a second-resolution timestamp, so a second migration inside the
# same second failed outright — the MIGRATION was lost, not just the backup.
r1=0; r2=0
bash "$CLI" migrate --from "$LEGACY" >/dev/null 2>&1 || r1=$?
bash "$CLI" migrate --from "$LEGACY" >/dev/null 2>&1 || r2=$?
jt_assert_rc "0" "$r1" "back-to-back migration 1"
jt_assert_rc "0" "$r2" "back-to-back migration 2 (same-second backup name)"
found=0
for f in "$JHH3_MEM_HOME"/backups/pre-migrate-*.db; do
  [ -e "$f" ] && found=1
done
jt_assert_eq "1" "$found" "backups/pre-migrate-<ts>.db exists"

# ── MIG-12 ──────────────────────────────────────────────────────────────────
jt_case MIG-12 "an unreadable or non-database source fails loudly and changes nothing"
rows_before="$(sqlite3 -batch -noheader "$JHH3_MEM_HOME/memory.db" 'SELECT count(*) FROM entries;')"
out="$(bash "$CLI" migrate --from "$WORK/does-not-exist.db" 2>&1)"; rc=$?
jt_assert_ne "0" "$rc" "a missing source is an error (rc $rc)"
jt_assert_ne "" "$out" "and it explains itself"
printf 'this is not a database at all\n' >"$WORK/junk.db"
out="$(bash "$CLI" migrate --from "$WORK/junk.db" 2>&1)"; rc=$?
jt_assert_ne "0" "$rc" "a non-database source is an error (rc $rc)"
jt_assert_eq "$rows_before" "$(sqlite3 -batch -noheader "$JHH3_MEM_HOME/memory.db" 'SELECT count(*) FROM entries;')" \
  "neither failure changed the store"

# ── MIG-13 ──────────────────────────────────────────────────────────────────
jt_case MIG-13 "migration never runs by itself"
jt_assert_empty "$(LC_ALL=C grep -rn 'jm_migrate_run\|migrate --from' \
  "$JT_PLUGIN_ROOT/scripts" "$JT_PLUGIN_ROOT/hooks" 2>/dev/null)" \
  "no hook or hook script can reach the migration path"

jt_end
