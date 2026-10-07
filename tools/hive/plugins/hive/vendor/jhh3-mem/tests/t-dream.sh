#!/usr/bin/env bash
# jhh3-mem — tests/t-dream.sh
# Between-session consolidation. Every step is checked for three things: that it
# detects, that it actually writes, and that a second pass finds nothing. dream is
# expected to run on a schedule, so a step that is not idempotent compounds forever.
set -uo pipefail
TDIR="$(cd -- "${BASH_SOURCE[0]%/*}" && pwd -P)"
ROOT="$(cd -- "$TDIR/.." && pwd -P)"
if [ "${1:-}" = '--list' ]; then
  printf '%s\n' DRM-1 DRM-2 DRM-3 DRM-4 DRM-5 DRM-6 DRM-7; exit 0
fi
PASS=0; FAIL=0
ok()  { PASS=$((PASS+1)); printf '  ok   %-8s %s\n' "$1" "${2:-}"; }
bad() { FAIL=$((FAIL+1)); printf '  FAIL %-8s %s\n' "$1" "${2:-}"; }
check(){ if [ "$2" = "$3" ]; then ok "$1" "$4"; else bad "$1" "$4 — want [$2] got [$3]"; fi; }

WORK="$(mktemp -d "${TMPDIR:-/tmp}/jhh3-dream.XXXXXX")"
trap 'rm -rf -- "$WORK"' EXIT
STORE="$WORK/store"; mkdir -p "$STORE"; export JHH3_MEM_HOME="$STORE"
CWD="$WORK/repo"; mkdir -p "$CWD"
git -C "$CWD" init -q 2>/dev/null
git -C "$CWD" remote add origin 'git@github.com:jhh3test/dream.git' 2>/dev/null
NOW="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
sq() { sqlite3 -bail "$STORE/memory.db" "$@" 2>/dev/null; }
jm() { bash "$ROOT/bin/jhh3-mem" "$@"; }

T="$WORK/t.jsonl"
printf '{"type":"user","promptSource":"typed","timestamp":"%s","cwd":"%s","message":{"role":"user","content":"seed"}}\n' "$NOW" "$CWD" >"$T"
printf '{"type":"assistant","timestamp":"%s","cwd":"%s","gitBranch":"main","message":{"role":"assistant","content":[{"type":"tool_use","name":"Edit","input":{"file_path":"%s/a.sh"}},{"type":"text","text":"Seeded."}]}}\n' "$NOW" "$CWD" "$CWD" >>"$T"
printf '{"session_id":"d1","transcript_path":"%s","cwd":"%s","hook_event_name":"SessionEnd","reason":"other"}\n' "$T" "$CWD" | bash "$ROOT/scripts/session-end.sh" >/dev/null 2>&1

# --- DRM-1: clean store, nothing to do, rc 0 -------------------------------------
out="$(jm dream 2>&1)"; rc=$?
check DRM-1 "0" "$rc" "a clean store consolidates with rc 0"

# --- DRM-2: the report is per host ------------------------------------------------
sq "INSERT INTO entries(uid,source_bytes,origin,producer,host,repo_key,worktree,branch,repo_path,kind,ts,started_at,title,durability,dedup_key,injectable,superseded,ephemeral,n_mod,n_read,turns,capture_mode,end_reason,redactions)
    VALUES('cxu',10,'live','t','codex','jhh3test/dream','','main','$CWD','session',strftime('%s','now'),strftime('%s','now'),'codex row',1.4,'codexrow',1,0,0,1,0,1,'deterministic','other',0);"
out="$(jm dream 2>&1)"
if printf '%s' "$out" | grep -q 'codex' && printf '%s' "$out" | grep -q 'claude-code'; then
  ok DRM-2 "the report accounts for both hosts in one pass"
else bad DRM-2 "report missed a host: $(printf '%s' "$out" | tr '\n' ' ')"; fi

# --- DRM-3/4: supersede reconciliation detects, writes, and settles ---------------
sq "INSERT INTO entries(uid,source_bytes,origin,producer,host,repo_key,worktree,branch,repo_path,kind,ts,started_at,title,durability,dedup_key,injectable,superseded,ephemeral,n_mod,n_read,turns,capture_mode,end_reason,redactions)
    SELECT uid, source_bytes+5000, origin, producer, host, repo_key, worktree, branch, repo_path, kind, ts, started_at, title||' grown', durability, dedup_key, 1, 0, 0, n_mod, n_read, turns, capture_mode, end_reason, redactions FROM entries WHERE uid='d1';"
before="$(sq "select count(*) from entries where superseded=0 and uid='d1';")"
jm dream >/dev/null 2>&1
check DRM-3 "2|1" "$before|$(sq "select count(*) from entries where superseded=0 and uid='d1';")" \
  "two current rows for one session are reconciled to one"
# Capture, then match. `jm dream | grep -q` looks equivalent but is not: grep -q
# exits at the first hit, dream then dies on EPIPE, and `set -o pipefail` reports
# the pipeline as failed even though the assertion passed.
_d4="$(jm dream 2>&1)"
if printf '%s\n' "$_d4" | grep -qE 'stale current rows +0$'; then
  ok DRM-4 "a second pass finds nothing to reconcile"
else
  bad DRM-4 "supersede reconciliation is not idempotent"
fi

# --- DRM-5/6: the durability floor writes, and is a fixed point -------------------
sq "UPDATE entries SET capture_mode='enriched', learned='x', durability=0.90 WHERE uid='d1' AND superseded=0;"
jm dream >/dev/null 2>&1
check DRM-5 "1.3" "$(sq "select round(durability,2) from entries where uid='d1' and superseded=0;")" \
  "an enriched row is lifted to the floor"
jm dream >/dev/null 2>&1
check DRM-6 "1.3" "$(sq "select round(durability,2) from entries where uid='d1' and superseded=0;")" \
  "and stays there — a floor, not a multiplier"

# --- DRM-7: dream never deletes ----------------------------------------------------
n="$(sq 'select count(*) from entries;')"
jm dream >/dev/null 2>&1
check DRM-7 "$n" "$(sq 'select count(*) from entries;')" "consolidation never removes an entry"

printf '\n  passed %-3s failed %s\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ]
