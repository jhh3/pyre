#!/usr/bin/env bash
# jhh3-mem — tests/t-setup.sh
# `setup` (the one-command install/update path) and `schedule`.
#
# The crontab is STUBBED. These tests must never touch the developer's real crontab,
# and a stub also lets them assert on the exact line written -- which is where the
# only bug in this subsystem lived (cron fields are MINUTE HOUR; writing them the
# other way put dream and enrich a minute apart instead of an hour).
set -uo pipefail
TDIR="$(cd -- "${BASH_SOURCE[0]%/*}" && pwd -P)"
ROOT="$(cd -- "$TDIR/.." && pwd -P)"
if [ "${1:-}" = '--list' ]; then
  printf '%s\n' SET-1 SET-2 SET-3 SET-4 SCH-1 SCH-2 SCH-2b SCH-3 SCH-4 SCH-5 SCH-6 SCH-7 SCH-8 SCH-9 SCH-10; exit 0
fi
PASS=0; FAIL=0
ok()  { PASS=$((PASS+1)); printf '  ok   %-8s %s\n' "$1" "${2:-}"; }
bad() { FAIL=$((FAIL+1)); printf '  FAIL %-8s %s\n' "$1" "${2:-}"; }
check(){ if [ "$2" = "$3" ]; then ok "$1" "$4"; else bad "$1" "$4 — want [$2] got [$3]"; fi; }

WORK="$(mktemp -d "${TMPDIR:-/tmp}/jhh3-setup.XXXXXX")"
trap 'rm -rf -- "$WORK"' EXIT
export JHH3_MEM_HOME="$WORK/store"
# Isolate HOME: `setup` auto-detects ~/mem-archive and ~/.claude-mem, and without
# this the suite would import the developer's real 20k-row archive into a fixture.
export HOME="$WORK/home"; mkdir -p "$HOME"
export JM_SHIM="$WORK/home/.local/bin/jhh3-mem"
CRONFILE="$WORK/crontab.txt"; : > "$CRONFILE"

# --- crontab stub: reads/writes $CRONFILE, never the real user crontab ----------
mkdir -p "$WORK/bin"
cat > "$WORK/bin/crontab" <<STUB
#!/usr/bin/env bash
case "\${1:-}" in
  -l) cat "$CRONFILE" ;;
  -r) : > "$CRONFILE" ;;
  -)  cat > "$CRONFILE" ;;
  *)  [ -f "\${1:-}" ] && cat "\${1}" > "$CRONFILE" ;;
esac
exit 0
STUB
chmod +x "$WORK/bin/crontab"
export PATH="$WORK/bin:$PATH"
jm() { bash "$ROOT/bin/jhh3-mem" "$@"; }
sq() { sqlite3 -bail "$JHH3_MEM_HOME/memory.db" "$@" 2>/dev/null; }

# --- SET-1: fresh machine bootstraps -------------------------------------------
out="$(jm setup --no-schedule 2>&1)"
_sv="$(grep -oE '^JM_SCHEMA_VERSION=[0-9]+' "$ROOT/lib/common.sh" | cut -d= -f2)"
check SET-1 "$_sv" "$(sq "select v from meta where k='schema_version';")" "setup bootstraps the store to the current schema"

# --- SET-2: idempotent — re-running is the update path --------------------------
out="$(jm setup --no-schedule 2>&1)"
if printf '%s' "$out" | grep -q 'already at schema'; then
  ok SET-2 "a second run reports the store is current instead of rebuilding"
else bad SET-2 "not idempotent: $(printf '%s' "$out" | tr '\n' ' ' | cut -c1-90)"; fi

# --- SET-3: legacy import is once-only -----------------------------------------
LEG="$WORK/legacy.db"
# The full legacy shape migrate reads (observations + session_summaries +
# user_prompts + sdk_sessions); a lone observations table imports zero rows.
sqlite3 -batch "$LEG" <<'SQL' 2>/dev/null
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
INSERT INTO sdk_sessions VALUES (1,'acme-web');
INSERT INTO observations (id,memory_session_id,project,type,title,created_at,created_at_epoch)
  VALUES (1,'s1','acme-web','discovery','a legacy discovery','2026-01-01',1767225600),
         (2,'s1','acme-web','bugfix','a legacy bugfix','2026-01-02',1767312000);
INSERT INTO session_summaries (id,memory_session_id,project,request,created_at,created_at_epoch)
  VALUES (1,'s1','acme-web','do the thing','2026-01-01',1767225600);
SQL
out="$(jm setup --no-schedule --from "$LEG" 2>&1)"
n1="$(sq "select count(*) from entries where origin like 'legacy-%';")"
out="$(jm setup --no-schedule --from "$LEG" 2>&1)"
n2="$(sq "select count(*) from entries where origin like 'legacy-%';")"
if [ "${n1:-0}" -gt 0 ] && [ "$n1" = "$n2" ] && printf '%s' "$out" | grep -q 'already imported'; then
  ok SET-3 "a legacy archive imports once and is skipped thereafter ($n1 rows)"
else bad SET-3 "import not once-only: first=$n1 second=$n2"; fi

# --- SET-4: --dry-run writes nothing --------------------------------------------
before="$(sq 'select count(*) from entries;')"
jm setup --dry-run >/dev/null 2>&1
check SET-4 "$before" "$(sq 'select count(*) from entries;')" "--dry-run changes no rows"
check SET-4 "0" "$(grep -c . "$CRONFILE" 2>/dev/null)" "--dry-run writes no crontab line"

# --- SCH-1: install writes the jobs ----------------------------------------------
jm schedule --with-enrich >/dev/null 2>&1
check SCH-1 "2" "$(grep -cF '# jhh3-mem-schedule' "$CRONFILE")" "install writes one tagged line per job"

# --- SCH-2: THE regression. cron fields are MINUTE HOUR --------------------------
# The two jobs are now a nightly full pass and an hourly catch-up, which makes this
# assertion sharper than the old "hours are 3 and 4" version: the hourly line is
# `17 * * * *`, and writing the fields the other way round yields `* 17 * * *` --
# every minute of 5pm instead of once an hour. Minute must be numeric, hour must be
# the wildcard; reversing them cannot satisfy both.
hourly="$(grep -F 'catchup' "$CRONFILE" | head -1)"
_min="$(printf '%s' "$hourly" | awk '{print $1}')"
_hr="$(printf '%s' "$hourly" | awk '{print $2}')"
if printf '%s' "$_min" | grep -qE '^[0-9]+$' && [ "$_hr" = '*' ]; then
  ok SCH-2 "the hourly job is MINUTE=$_min HOUR=* (fields are MINUTE HOUR)"
else bad SCH-2 "hourly cron fields look reversed — minute=[$_min] hour=[$_hr]"; fi
check SCH-2b "3" "$(grep -F 'nightly' "$CRONFILE" | head -1 | awk '{print $2}')" \
  "the nightly job runs at hour 3"

# --- SCH-3: re-install rewrites rather than duplicating --------------------------
jm schedule --with-enrich >/dev/null 2>&1
check SCH-3 "2" "$(grep -cF '# jhh3-mem-schedule' "$CRONFILE")" "re-installing rewrites, it does not accumulate"

# --- SCH-4: it is listed in the inventory uninstall consumes ---------------------
if jm paths --annotated 2>/dev/null | grep -qi 'crontab'; then
  ok SCH-4 "the schedule appears in the paths inventory"
else bad SCH-4 "schedule is invisible to \`paths\` — uninstall could not promise to remove it"; fi

# --- SCH-5: remove strips every line we wrote ------------------------------------
jm schedule --remove >/dev/null 2>&1
check SCH-5 "0" "$(grep -cF '# jhh3-mem-schedule' "$CRONFILE")" "remove strips every tagged line"

# --- SCH-6: THE upgrade regression --------------------------------------------
# Hosts delete the old version directory on upgrade, so a crontab line naming
# .../jhh3-mem/<version>/bin/jhh3-mem stops working at the next update -- silently,
# because cron output goes to /dev/null. The line must name the stable shim.
# Assert the exact target rather than pattern-matching for a version segment: in a
# source checkout there is no /<version>/bin/ path to match, so a regex-based check
# passes even with the bug reintroduced. Naming the shim is the actual requirement.
jm schedule >/dev/null 2>&1
_target="$(grep -oE '"[^"]*/jhh3-mem" nightly' "$CRONFILE" | sed 's/" nightly//; s/^"//')"
check SCH-6 "$JM_SHIM" "$_target" "cron invokes the stable shim, not a versioned plugin path"

# --- SCH-7: and the shim actually resolves and runs -----------------------------
if [ -x "$JM_SHIM" ] && grep -q 'jhh3-mem launcher' "$JM_SHIM"; then
  ok SCH-7 "the launcher shim is installed and marked as ours"
else
  bad SCH-7 "no shim at $JM_SHIM"
fi

# --- SCH-8: a shim the shell cannot reach must SAY so ---------------------------
# Installing a launcher into a directory that is not on PATH yields a command that
# exists and cannot be run, which reads as a broken install. On a fresh machine
# ~/.local/bin frequently is not on PATH, so this is the common case, not the edge.
_off="$(PATH=/usr/bin:/bin jm setup --no-schedule 2>&1)"
if printf '%s' "$_off" | grep -q 'NOT on your PATH'; then
  ok SCH-8 "setup warns when the launcher's directory is not on PATH"
else
  bad SCH-8 "installed an unreachable launcher silently"
fi
# The fixture's shim dir is not on the suite's PATH by default, so put it there for
# the negative case rather than asserting silence from a genuinely unreachable shim.
_on="$(PATH="$(dirname "$JM_SHIM"):$PATH" jm setup --no-schedule 2>&1)"
if printf '%s' "$_on" | grep -q 'NOT on your PATH'; then
  bad SCH-8 "warned even though the directory IS on PATH"
else
  ok SCH-8 "and stays quiet when it is reachable"
fi

# --- SCH-9: cron lines source the key file, and never contain the key ------------
# cron provides no environment, so without this the embed stage inside catchup and
# nightly finds no OPENAI_API_KEY and skips, every hour, forever. With a key file,
# both lines run under a shell that sources it; the key itself must never appear in
# the crontab. Without one, the lines are installed bare (embed skips, announced).
printf 'export OPENAI_API_KEY=test-not-a-real-key\n' > "$WORK/env.private"
jm schedule --with-enrich --env-file "$WORK/env.private" >/dev/null 2>&1
if [ "$(grep -cF ". \"$WORK/env.private\"" "$CRONFILE")" = "2" ] \
   && ! grep -q 'test-not-a-real-key' "$CRONFILE" \
   && grep -q 'catchup' "$CRONFILE" && grep -q 'nightly' "$CRONFILE"; then
  ok SCH-9 "both lines source the env file; the key never lands in the crontab"
else bad SCH-9 "env-file cron lines wrong: $(cat "$CRONFILE")"; fi
jm schedule --with-enrich --env-file "$WORK/does-not-exist" >/dev/null 2>&1
check SCH-9 "0" "$(grep -c '/bin/sh -c' "$CRONFILE")" "no key file -> bare lines, no sourcing wrapper"
_warn="$(jm schedule --with-enrich --env-file "$WORK/does-not-exist" 2>&1)"
if printf '%s' "$_warn" | grep -q 'SKIP under cron'; then
  ok SCH-9 "and the skip is announced, not silent"
else bad SCH-9 "embed-will-skip warning missing: $_warn"; fi

# --- SCH-10: cron lines carry a PATH that can find the model CLIs ----------------
# cron's default PATH is /usr/bin:/bin; claude and codex live where the shim lives.
# Without an explicit PATH the enrich stage fails "no model CLI found" on every
# scheduled run, straight to /dev/null.
check SCH-10 "2" "$(grep -cF "PATH=\"$(dirname "$JM_SHIM"):" "$CRONFILE")" \
  "both cron lines put the shim's directory (where the model CLIs live) on PATH"

printf '\n  passed %-3s failed %s\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ]
