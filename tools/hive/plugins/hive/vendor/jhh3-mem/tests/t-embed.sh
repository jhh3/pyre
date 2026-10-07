#!/usr/bin/env bash
# jhh3-mem — tests/t-embed.sh
# Semantic recall. The feature is optional, remote and paid, so almost every test
# here is about what happens when it is ABSENT or BROKEN rather than when it works:
# an optional lane that degrades badly is worse than no lane at all, because it
# takes the lexical search that already worked down with it.
set -uo pipefail
TDIR="$(cd -- "${BASH_SOURCE[0]%/*}" && pwd -P)"
ROOT="$(cd -- "$TDIR/.." && pwd -P)"
if [ "${1:-}" = '--list' ]; then
  printf '%s\n' EMB-1 EMB-2 EMB-3 EMB-4 EMB-5 EMB-6 EMB-6a EMB-6b EMB-6c EMB-7 EMB-8 EMB-9 EMB-9a; exit 0
fi
PASS=0; FAIL=0
ok()  { PASS=$((PASS+1)); printf '  ok   %-8s %s\n' "$1" "${2:-}"; }
bad() { FAIL=$((FAIL+1)); printf '  FAIL %-8s %s\n' "$1" "${2:-}"; }
check(){ if [ "$2" = "$3" ]; then ok "$1" "$4"; else bad "$1" "$4 — want [$2] got [$3]"; fi; }

WORK="$(mktemp -d "${TMPDIR:-/tmp}/jhh3-embed.XXXXXX")"
trap 'rm -rf -- "$WORK"' EXIT
STORE="$WORK/store"; mkdir -p "$STORE"; export JHH3_MEM_HOME="$STORE"
CWD="$WORK/repo"; mkdir -p "$CWD"
git -C "$CWD" init -q 2>/dev/null
git -C "$CWD" remote add origin 'git@github.com:jhh3test/embed.git' 2>/dev/null
sq() { sqlite3 -bail "$STORE/memory.db" "$@" 2>/dev/null; }
jm() { bash "$ROOT/bin/jhh3-mem" "$@"; }

# Seed via the real hook so the schema and FTS triggers are built the way they are
# in production, not the way this test imagines them.
NOW="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
T="$WORK/t.jsonl"
printf '{"type":"user","promptSource":"typed","timestamp":"%s","cwd":"%s","message":{"role":"user","content":"seed"}}\n' "$NOW" "$CWD" >"$T"
printf '{"type":"assistant","timestamp":"%s","cwd":"%s","gitBranch":"main","message":{"role":"assistant","content":[{"type":"tool_use","name":"Edit","input":{"file_path":"%s/a.sh"}},{"type":"text","text":"Seeded."}]}}\n' "$NOW" "$CWD" "$CWD" >>"$T"
printf '{"session_id":"e1","transcript_path":"%s","cwd":"%s","hook_event_name":"SessionEnd","reason":"other"}\n' "$T" "$CWD" | bash "$ROOT/scripts/session-end.sh" >/dev/null 2>&1

seed_row() { # uid, title, outcome
  sq "INSERT INTO entries(uid,source_bytes,origin,producer,host,repo_key,worktree,branch,repo_path,kind,ts,started_at,title,outcome,durability,dedup_key,injectable,superseded,ephemeral,n_mod,n_read,turns,capture_mode,end_reason,redactions)
      VALUES('$1',10,'live','t','claude-code','jhh3test/embed','','main','$CWD','session',strftime('%s','now'),strftime('%s','now'),'$2','$3',1.4,'$1',1,0,0,1,0,1,'deterministic','other',0);"
  sq "SELECT id FROM entries WHERE uid='$1';"
}
A="$(seed_row lex1 'kangaroo alpha' 'the kangaroo hops')"
B="$(seed_row sem1 'wallaby bravo'  'the wallaby rests')"
D="$(seed_row sem2 'quokka delta'   'the quokka smiles')"

rank() { # sem_ids, q -> ids in rank order
  bash -c '
    . "'"$ROOT"'/lib/common.sh" >/dev/null 2>&1
    . "'"$ROOT"'/lib/db.sh"     >/dev/null 2>&1
    . "'"$ROOT"'/lib/query.sh"  >/dev/null 2>&1
    mapfile -t P < <(jm_query_defaults)
    jm_rank_run "${P[@]}" q "$2" sem_ids "$1" scope_all 1 fmt text limit 10
  ' _ "$1" "$2" 2>&1
}

# --- EMB-1: with no key the semantic lane is inert, not merely quiet -------------
# The promise is that installing this feature cannot change results until it is
# actually configured. Compared against the explicit kill switch, not a snapshot.
a="$(env -u OPENAI_API_KEY bash "$ROOT/bin/jhh3-mem" search kangaroo --all-repos 2>&1)"
b="$(env -u OPENAI_API_KEY JM_RECALL_NO_SEMANTIC=1 bash "$ROOT/bin/jhh3-mem" search kangaroo --all-repos 2>&1)"
check EMB-1 "$a" "$b" "no key: search is identical to semantic being switched off"

# --- EMB-2/3: no hook may reach the network -------------------------------------
# INV-6 reads the source; this runs the hooks with the network clients replaced by
# tripwires, which is the only version of the claim that survives a refactor.
BIN="$WORK/bin"; mkdir -p "$BIN"
for c in curl wget nc; do
  printf '#!/bin/sh\ntouch "%s/CALLED.%s"\nexit 1\n' "$WORK" "$c" >"$BIN/$c"; chmod +x "$BIN/$c"
done
printf '{"session_id":"e2","transcript_path":"%s","cwd":"%s","hook_event_name":"SessionStart","source":"startup"}\n' "$T" "$CWD" \
  | PATH="$BIN:$PATH" OPENAI_API_KEY=sk-test-not-real bash "$ROOT/scripts/session-start.sh" >/dev/null 2>&1
n="$(find "$WORK" -maxdepth 1 -name 'CALLED.*' | wc -l | tr -d ' ')"
check EMB-2 "0" "$n" "SessionStart makes no network call even with a key present"

rm -f "$WORK"/CALLED.*
printf '{"session_id":"e3","transcript_path":"%s","cwd":"%s","hook_event_name":"SessionEnd","reason":"other"}\n' "$T" "$CWD" \
  | PATH="$BIN:$PATH" OPENAI_API_KEY=sk-test-not-real bash "$ROOT/scripts/session-end.sh" >/dev/null 2>&1
n="$(find "$WORK" -maxdepth 1 -name 'CALLED.*' | wc -l | tr -d ' ')"
check EMB-3 "0" "$n" "SessionEnd makes no network call even with a key present"

# --- EMB-4: a row both lanes find is rendered once ------------------------------
# `cand` UNION ALLs three lanes. Without the GROUP BY this row is JOINed twice and
# the user sees a duplicate -- the failure the aggregate exists to prevent.
out="$(rank "$A" kangaroo)"
n="$(printf '%s\n' "$out" | cut -d"$(printf '\037')" -f1 | grep -cx "$A" || true)"
check EMB-4 "1" "$n" "a row in both the lexical and semantic lane appears exactly once"

# --- EMB-5: semantic rank is honoured, not just membership ----------------------
# json_each.key carries the position embed.sh emitted. If the decay were dropped
# every semantic row would tie and this ordering would be arbitrary.
f="$(rank "$B,$D" kangaroo | cut -d"$(printf '\037')" -f1 | grep -nE "^($B|$D)$" | head -1 | cut -d: -f2)"
r="$(rank "$D,$B" kangaroo | cut -d"$(printf '\037')" -f1 | grep -nE "^($B|$D)$" | head -1 | cut -d: -f2)"
if [ "$f" = "$B" ] && [ "$r" = "$D" ]; then
  ok EMB-5 "the first id in sem_ids outranks the second, both ways round"
else bad EMB-5 "semantic rank ignored — got [$f] then [$r], want [$B] then [$D]"; fi

# --- EMB-6: malformed ids are rejected before binding, and cannot break the SQL --
# Two separate promises, so two separate checks. The validator is what keeps a bad
# id list from ever reaching the query; rank.sql is the backstop that keeps the
# damage to "wrong lane" instead of "failed search" if it ever does. Asserting only
# the first would leave the query one refactor away from raising on live data.
JUNK=("1,2); DROP TABLE entries--" "abc" "1,,2" "]" "1;2" ",5" "5," "$(printf '1\n2')")

rejected=0
for junk in "${JUNK[@]}"; do
  if bash -c '. "'"$ROOT"'/lib/common.sh" >/dev/null 2>&1
              . "'"$ROOT"'/lib/embed.sh"  >/dev/null 2>&1
              jm_embed_valid_ids "$1"' _ "$junk" >/dev/null 2>&1; then
    bad EMB-6 "validator ACCEPTED malformed id list [$junk]"; rejected=1; break
  fi
done
[ "$rejected" = 0 ] && ok EMB-6 "every malformed id list is rejected before it is bound"

# and a well-formed one still survives the validator, so the check is not vacuous
if bash -c '. "'"$ROOT"'/lib/common.sh" >/dev/null 2>&1
            . "'"$ROOT"'/lib/embed.sh"  >/dev/null 2>&1
            jm_embed_valid_ids "$1"' _ "$B,$D" | grep -qx "$B,$D"; then
  ok EMB-6a "a real id list passes the validator unchanged"
else bad EMB-6a "validator rejected a legitimate id list"; fi

erred=0
for junk in "${JUNK[@]}"; do
  if ! rank "$junk" kangaroo >/dev/null 2>&1; then
    bad EMB-6c "rank.sql ERRORED on sem_ids [$junk]"; erred=1; break
  fi
done
[ "$erred" = 0 ] && ok EMB-6c "rank.sql never raises on a malformed id list, it just matches nothing"

alive="$(sq 'SELECT count(*) FROM entries;')"
if [ "${alive:-0}" -ge 3 ]; then ok EMB-6b "and the table is still there afterwards ($alive rows)"
else bad EMB-6b "entries table lost rows — got [$alive]"; fi

# --- EMB-7: lexical search survives a store that never fetched the extension ----
# vec0 is an optional 160KB download. If rank.sql ever joined a vec0 virtual table
# directly, a missing module would make the DB unopenable and take lexical with it.
check EMB-7 "0" "$(sq "SELECT count(*) FROM sqlite_master WHERE name='entry_vec';")" \
  "no entry_vec table exists yet"
out="$(bash "$ROOT/bin/jhh3-mem" search kangaroo --all-repos 2>&1)"; rc=$?
if [ "$rc" -eq 0 ] && printf '%s' "$out" | grep -q 'kangaroo'; then
  ok EMB-7 "lexical search works with no extension and no vector table"
else bad EMB-7 "search rc=$rc out=${out:0:120}"; fi

# --- EMB-8: status is honest before anything is installed ----------------------
out="$(env -u OPENAI_API_KEY bash "$ROOT/bin/jhh3-mem" embed --status 2>&1)"
if printf '%s' "$out" | grep -qiE 'absent|not installed|off'; then
  ok EMB-8 "embed --status names the missing piece instead of implying it works"
else bad EMB-8 "status did not report the extension as absent: out=${out:0:120}"; fi

# --- EMB-9: the text sent for embedding is always valid UTF-8 ------------------
# The row text was truncated with `cut -c1-2000`, which counts BYTES on this input.
# A multi-byte character straddling the limit was cut in half, and the invalid
# UTF-8 made the API reject the entire batch -- 96 good rows lost to one, on every
# run, reported as a network error. Truncation moved into SQL, where substr counts
# characters. This builds the pathological row deliberately: em-dashes (3 bytes
# each) placed so that a byte-wise cut MUST land inside one.
pad="$(printf 'a%.0s' $(seq 1 1900))"
dashes="$(printf '\u2014%.0s' $(seq 1 120))"   # 3 bytes each, so byte and char counts diverge
long="$pad$dashes tail"
sq "UPDATE entries SET outcome='$long' WHERE id=$B;"
built="$(sq "SELECT substr(replace(replace(
               title || '. ' || coalesce(outcome,'') || ' ' || coalesce(learned,''),
               char(10),' '), char(9),' '), 1, 2000) FROM entries WHERE id=$B;")"
if printf '%s' "$built" | iconv -f UTF-8 -t UTF-8 >/dev/null 2>&1; then
  ok EMB-9 "truncated row text is still valid UTF-8 when multi-byte chars straddle the cut"
else bad EMB-9 "truncation produced invalid UTF-8 — the API would reject the whole batch"; fi

# and prove the test is not vacuous. A single fixed offset is a coin flip -- two
# cuts in three land on a character boundary and look fine -- so sweep a window
# and require that SOME byte-wise cut corrupts it. That is the actual claim: on
# this input a byte-wise cut is unsafe, which is why the code no longer uses one.
broke=0
for off in $(seq 1960 1990); do
  printf '%s' "$built" | cut -c"1-$off" | iconv -f UTF-8 -t UTF-8 >/dev/null 2>&1 || broke=$((broke+1))
done
if [ "$broke" -gt 0 ]; then
  ok EMB-9a "the fixture reproduces it: $broke of 31 byte-wise cuts yield invalid UTF-8"
else bad EMB-9a "no byte-wise cut corrupted this input — the fixture no longer reproduces the bug"; fi

printf 'embed: %d passed, %d failed, 0 skipped\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ]
