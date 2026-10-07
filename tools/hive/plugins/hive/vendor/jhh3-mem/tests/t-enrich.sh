#!/usr/bin/env bash
# jhh3-mem — tests/t-enrich.sh
# The out-of-band enrichment path. Uses a stub model so the suite is hermetic:
# no network, no credential, no dependence on which agent CLI is installed.
set -uo pipefail
TDIR="$(cd -- "${BASH_SOURCE[0]%/*}" && pwd -P)"
ROOT="$(cd -- "$TDIR/.." && pwd -P)"
if [ "${1:-}" = '--list' ]; then
  printf '%s\n' ENR-1 ENR-2 ENR-3 ENR-4 ENR-5 ENR-6 ENR-7 ENR-8 ENR-9 ENR-10 ENR-11 ENR-12 ENR-13; exit 0
fi
PASS=0; FAIL=0
ok()  { PASS=$((PASS+1)); printf '  ok   %-8s %s\n' "$1" "${2:-}"; }
bad() { FAIL=$((FAIL+1)); printf '  FAIL %-8s %s\n' "$1" "${2:-}"; }
check(){ if [ "$2" = "$3" ]; then ok "$1" "$4"; else bad "$1" "$4 — want [$2] got [$3]"; fi; }

WORK="$(mktemp -d "${TMPDIR:-/tmp}/jhh3-enrich.XXXXXX")"
trap 'rm -rf -- "$WORK"' EXIT
STORE="$WORK/store"; mkdir -p "$STORE"; export JHH3_MEM_HOME="$STORE"
CWD="$WORK/repo"; mkdir -p "$CWD"
git -C "$CWD" init -q 2>/dev/null
git -C "$CWD" remote add origin 'git@github.com:jhh3test/enrich.git' 2>/dev/null
NOW="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
sq() { sqlite3 -bail "$STORE/memory.db" "$@" 2>/dev/null; }
jm() { bash "$ROOT/bin/jhh3-mem" "$@"; }

T="$WORK/t.jsonl"
printf '{"type":"user","promptSource":"typed","timestamp":"%s","cwd":"%s","message":{"role":"user","content":"fix the race"}}\n' "$NOW" "$CWD" >"$T"
printf '{"type":"assistant","timestamp":"%s","cwd":"%s","gitBranch":"main","message":{"role":"assistant","content":[{"type":"tool_use","name":"Edit","input":{"file_path":"%s/a.sh"}},{"type":"text","text":"Fixed."}]}}\n' "$NOW" "$CWD" "$CWD" >>"$T"
printf '{"session_id":"e1","transcript_path":"%s","cwd":"%s","hook_event_name":"SessionEnd","reason":"other"}\n' "$T" "$CWD" | bash "$ROOT/scripts/session-end.sh" >/dev/null 2>&1

GOOD="$WORK/good.sh"; printf '#!/usr/bin/env bash\ncat >/dev/null\nprintf %s\n' \
  "'{\"learned\":\"flock is required\",\"decided\":\"serialize on the DDL lock\",\"next_steps\":\"add a test\"}\\n'" >"$GOOD"; chmod +x "$GOOD"
JUNK="$WORK/junk.sh"; printf '#!/usr/bin/env bash\ncat >/dev/null\nprintf %s\n' "'not json at all\\n'" >"$JUNK"; chmod +x "$JUNK"
BOOM="$WORK/boom.sh"; printf '#!/usr/bin/env bash\ncat >/dev/null\nexit 9\n' >"$BOOM"; chmod +x "$BOOM"

check ENR-1 "deterministic" "$(sq "select capture_mode from entries;")" "capture leaves the row deterministic"

# --- ENR-2: dry run selects but neither calls a model nor writes -----------------
out="$(JHH3_MEM_ENRICH_CMD="$BOOM" jm enrich --dry-run 2>&1)"
if printf '%s' "$out" | grep -q '\[dry\]' && [ "$(sq "select capture_mode from entries;")" = 'deterministic' ]; then
  ok ENR-2 "--dry-run selects without calling the model or writing"
else bad ENR-2 "dry run wrote or failed to select: $out"; fi

# --- ENR-3/4: the happy path ------------------------------------------------------
JHH3_MEM_ENRICH_CMD="$GOOD" jm enrich >/dev/null 2>&1
check ENR-3 "enriched" "$(sq "select capture_mode from entries;")" "a successful call flips capture_mode"
check ENR-4 "flock is required|serialize on the DDL lock|add a test" \
  "$(sq "select learned || '|' || decided || '|' || next_steps from entries;")" \
  "all three fields are written from the model's JSON"

# --- ENR-5: re-running is a no-op, not a re-spend ---------------------------------
out="$(JHH3_MEM_ENRICH_CMD="$BOOM" jm enrich 2>&1)"
if printf '%s' "$out" | grep -q 'nothing to enrich'; then
  ok ENR-5 "an already-enriched store is skipped (resumable, not re-spent)"
else bad ENR-5 "re-run tried to work: $out"; fi

# --- ENR-6/7: failure is per row and terminal, never a wedge ----------------------
sq "UPDATE entries SET capture_mode='deterministic';"
JHH3_MEM_ENRICH_CMD="$BOOM" jm enrich >/dev/null 2>&1
check ENR-6 "enrich_failed" "$(sq "select capture_mode from entries;")" "a failing model call marks the row, not the run"
sq "UPDATE entries SET capture_mode='deterministic';"
JHH3_MEM_ENRICH_CMD="$JUNK" jm enrich >/dev/null 2>&1
check ENR-7 "enrich_failed" "$(sq "select capture_mode from entries;")" "a non-JSON reply is rejected rather than stored"

# --- ENR-8: no model CLI at all is a clean rc 3, not a crash ----------------------
sq "UPDATE entries SET capture_mode='deterministic';"
# A PATH with coreutils and sqlite3 but neither agent CLI: both live outside
# /usr/bin (claude under ~/.local/share, codex under nvm), so this isolates the
# "no model available" branch without breaking the script itself.
( PATH=/usr/bin:/bin; unset JHH3_MEM_ENRICH_CMD; bash "$ROOT/bin/jhh3-mem" enrich >/dev/null 2>&1 )
check ENR-8 "3" "$?" "no model CLI available returns rc 3"

# --- ENR-9: the whole row reaches the model, newlines and all --------------------
# Every stub above discards the prompt (`cat >/dev/null`), so nothing in this suite
# could see WHAT was sent -- and what was sent was a title and nothing else. The
# fields were fetched with one char(31)-joined SELECT read back by `read -r`, which
# stops at the first newline; `outcome` is multi-paragraph, so it and every field
# after it vanished. The model was asked to distil a heading, correctly replied that
# there was nothing to add, and the row was marked enriched. A stub that captures
# the prompt is the only way this is visible.
CAP="$WORK/captured-prompt.txt"
SPY="$WORK/spy.sh"
{ printf '#!/usr/bin/env bash\n'
  printf 'cat > "%s"\n' "$CAP"
  printf 'printf %s\n' "'{\"learned\":\"L\",\"decided\":\"D\",\"next_steps\":\"N\"}\\n'"
} > "$SPY"; chmod +x "$SPY"

sq "UPDATE entries SET capture_mode='deterministic',
      outcome='FIRST LINE of the outcome.' || char(10) || char(10) || 'SECOND paragraph.' || char(10) || 'CANARY-TAIL-9',
      files_touched='a.sh b.sh', commands='make test';"
JHH3_MEM_ENRICH_CMD="$SPY" jm enrich >/dev/null 2>&1

if grep -q 'CANARY-TAIL-9' "$CAP" 2>/dev/null; then
  ok ENR-9 "the full multi-line outcome reaches the prompt"
else bad ENR-9 "outcome truncated at the first newline — prompt was $(wc -c <"$CAP" 2>/dev/null) bytes"; fi
if grep -q 'make test' "$CAP" 2>/dev/null && grep -q 'a.sh b.sh' "$CAP" 2>/dev/null; then
  ok ENR-9b "fields after the outcome survive too"
else bad ENR-9b "files/commands were dropped after the multi-line outcome"; fi

# --- ENR-10: the columns the prompt is built from are NOT NULL -------------------
# `a || b` is NULL when either side is NULL, so a char(31)-joined SELECT would blank
# the entire row if any column were nullable. Today the schema forbids it, which is
# WHY that concat was safe -- so assert the schema fact rather than a NULL round
# trip the database rejects outright. If a migration ever relaxes one of these, this
# fails and names the reason.
nn="$(sq "SELECT count(*) FROM pragma_table_info('entries')
            WHERE name IN ('outcome','learned','decided','next_steps','files_touched','commands')
              AND \"notnull\" = 1;")"
check ENR-10 "6" "$nn" "every column the prompt is built from is NOT NULL"

# --- ENR-11: an empty reply is not success ---------------------------------------
# All-empty JSON is still valid JSON, and the old guard only checked that the
# joined string was non-empty -- it always is, it carries two separators. Rows were
# marked `enriched` holding nothing, and `enriched` is terminal, so they were never
# revisited. It has to be distinguishable from a real enrichment.
EMPTYR="$WORK/empty.sh"
{ printf '#!/usr/bin/env bash\ncat >/dev/null\n'
  printf 'printf %s\n' "'{\"learned\":\"\",\"decided\":\"\",\"next_steps\":\"\"}\\n'"
} > "$EMPTYR"; chmod +x "$EMPTYR"
sq "UPDATE entries SET capture_mode='deterministic', learned='', decided='', next_steps='';"
JHH3_MEM_ENRICH_CMD="$EMPTYR" jm enrich >/dev/null 2>&1
got="$(sq "select capture_mode from entries;")"
if [ "$got" != 'enriched' ]; then
  ok ENR-11 "an all-empty reply is not recorded as enriched (got '$got')"
else bad ENR-11 "an all-empty reply was recorded as a successful enrichment"; fi

# --- ENR-12: the plugin does not capture its own model calls --------------------
# enrich reaches a model with `claude -p`, and that call is itself a session: hooks
# fire, a row is written, and the row is deterministic -- so it needs enriching,
# which makes another call, which writes another row. Nightly at limit 40 that was a
# slow leak; hourly at limit 500 it is a treadmill billing a model call and an
# embedding for rows that describe nothing but their own creation. Measured on the
# real store: one enrich-sized `claude -p` produced two rows.
GW="$(mktemp -d "${TMPDIR:-/tmp}/jhh3-selfcall.XXXXXX")"
GSTORE="$GW/store"; mkdir -p "$GSTORE"
GCWD="$GW/repo"; mkdir -p "$GCWD"
git -C "$GCWD" init -q 2>/dev/null
git -C "$GCWD" remote add origin 'git@github.com:t/selfcall.git' 2>/dev/null
GNOW="$(date -u +%Y-%m-%dT%H:%M:%SZ)"; GT="$GW/t.jsonl"
printf '{"type":"user","promptSource":"typed","timestamp":"%s","cwd":"%s","message":{"role":"user","content":"a session with real substance to it"}}\n' "$GNOW" "$GCWD" > "$GT"
printf '{"type":"assistant","timestamp":"%s","cwd":"%s","gitBranch":"main","message":{"role":"assistant","content":[{"type":"tool_use","name":"Edit","input":{"file_path":"%s/a.sh"}},{"type":"text","text":"Done."}]}}\n' "$GNOW" "$GCWD" "$GCWD" >> "$GT"
_gpay() { printf '{"session_id":"%s","transcript_path":"%s","cwd":"%s","hook_event_name":"SessionEnd","reason":"other"}' "$1" "$GT" "$GCWD"; }
_grows() { JHH3_MEM_HOME="$GSTORE" sqlite3 "$GSTORE/memory.db" "SELECT count(*) FROM entries;" 2>/dev/null || printf '0'; }

JHH3_MEM_HOME="$GSTORE" bash -c '_p="$1"; printf "%s" "$_p" | bash "$2/scripts/session-end.sh"' _ "$(_gpay s1)" "$ROOT" >/dev/null 2>&1
g1="$(_grows)"
JHH3_MEM_HOME="$GSTORE" JHH3_MEM_NO_CAPTURE=1 bash -c '_p="$1"; printf "%s" "$_p" | bash "$2/scripts/session-end.sh"' _ "$(_gpay s2)" "$ROOT" >/dev/null 2>&1
g2="$(_grows)"

if [ "${g1:-0}" -ge 1 ] && [ "$g1" = "$g2" ]; then
  ok ENR-12 "JHH3_MEM_NO_CAPTURE suppresses capture (a normal session still writes: $g1)"
elif [ "${g1:-0}" -lt 1 ]; then
  bad ENR-12 "fixture wrote no row at all — the test proves nothing"
else bad ENR-12 "the sentinel did not suppress capture ($g1 -> $g2)"; fi
rm -rf -- "$GW"

# --- ENR-13: and enrich actually sets it on the model call -----------------------
# ENR-12 proves the hook honours the sentinel; this proves the caller sends it.
# Without both halves the loop comes back the first time someone reorders this line.
if grep -qE 'JHH3_MEM_NO_CAPTURE=1[^|]*\$cmd' "$ROOT/lib/enrich.sh"; then
  ok ENR-13 "enrich exports the sentinel on the model invocation"
else bad ENR-13 "enrich no longer sets JHH3_MEM_NO_CAPTURE — the feedback loop is back"; fi

printf '\n  passed %-3s failed %s\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ]
