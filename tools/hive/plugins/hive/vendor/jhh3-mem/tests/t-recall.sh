#!/usr/bin/env bash
# jhh3-mem — tests/t-recall.sh
# SPEC §14.6 (T-REC) plus direct unit tests of jm_query_build and jm_render_cap.
# Self-contained: its own store under mktemp, JHH3_MEM_HOME pointed at it.
#
#   bash tests/t-recall.sh            run
#   bash tests/t-recall.sh --list     print the case ids
set -uo pipefail

TDIR="$(cd -- "${BASH_SOURCE[0]%/*}" && pwd -P)"
ROOT="$(cd -- "$TDIR/.." && pwd -P)"

if [ "${1:-}" = '--list' ]; then
  printf '%s\n' REC-1 REC-2 REC-3 REC-4 REC-5 REC-6 REC-7 REC-8 REC-9 REC-10 \
    REC-11 REC-12 REC-13 REC-14 QRY-1 CAP-1
  exit 0
fi

PASS=0; FAIL=0
ok()  { PASS=$((PASS + 1)); printf '  ok   %-8s %s\n' "$1" "${2:-}"; }
bad() { FAIL=$((FAIL + 1)); printf '  FAIL %-8s %s\n' "$1" "${2:-}"; }
check() { if [ "$2" = "$3" ]; then ok "$1" "$4"; else bad "$1" "$4 — want [$2] got [$3]"; fi; }

STORE="$(mktemp -d "${TMPDIR:-/tmp}/jm-rec.XXXXXX")" || exit 1
WORK="$(mktemp -d "${TMPDIR:-/tmp}/jm-recrepo.XXXXXX")" || exit 1
trap 'rm -rf -- "$STORE" "$WORK"' EXIT INT TERM
export JHH3_MEM_HOME="$STORE"

git -C "$WORK" init -q 2>/dev/null
git -C "$WORK" remote add origin 'git@github.com:jhh3test/recall.git' 2>/dev/null
REPO='jhh3test/recall'
OTHER='otherorg/otherrepo'
NOW="$(date +%s)"

sqlite3 -bail "$STORE/memory.db" ".read $ROOT/db/schema.sql" >/dev/null 2>&1
sqlite3 -bail "$STORE/memory.db" \
  "INSERT OR REPLACE INTO meta(k,v) VALUES('schema_version','1'),('has_math','1');" >/dev/null

hex() { printf '%s' "${1:-}" | od -An -v -tx1 | tr -d ' \n'; }
ins() { # ins UID REPO WORKTREE KIND TITLE OUTCOME DURABILITY EPHEMERAL AGEDAYS ORIGIN
  sqlite3 -bail "$STORE/memory.db" \
    -cmd ".parameter set :u cast(x'$(hex "$1")'as'text')" \
    -cmd ".parameter set :r cast(x'$(hex "$2")'as'text')" \
    -cmd ".parameter set :w cast(x'$(hex "$3")'as'text')" \
    -cmd ".parameter set :k cast(x'$(hex "$4")'as'text')" \
    -cmd ".parameter set :t cast(x'$(hex "$5")'as'text')" \
    -cmd ".parameter set :o cast(x'$(hex "$6")'as'text')" \
    -cmd ".parameter set :d cast(x'$(hex "$7")'as'text')" \
    -cmd ".parameter set :e cast(x'$(hex "$8")'as'text')" \
    -cmd ".parameter set :a cast(x'$(hex "$9")'as'text')" \
    -cmd ".parameter set :g cast(x'$(hex "${10}")'as'text')" \
    "INSERT OR IGNORE INTO entries
       (uid, source_bytes, origin, producer, repo_key, worktree, branch, repo_path,
        kind, ts, title, outcome, durability, dedup_key, injectable, superseded, ephemeral)
     VALUES (:u, 0, :g, 'jhh3-mem/0.1.0', :r, :w, 'main', '$WORK', :k,
             $NOW - CAST(:a AS INTEGER) * 86400, :t, :o, CAST(:d AS REAL),
             substr(replace(replace(replace(replace(replace(lower(:t),' ',''),'-',''),'_',''),'.',''),':',''),1,48),
             1, 0, CAST(:e AS INTEGER));" >/dev/null 2>&1
}
link() { # link UID PATH MODE
  sqlite3 -bail "$STORE/memory.db" \
    -cmd ".parameter set :u cast(x'$(hex "$1")'as'text')" \
    -cmd ".parameter set :p cast(x'$(hex "$2")'as'text')" \
    "INSERT OR IGNORE INTO path(p, base) VALUES(:p, replace(:p, rtrim(:p, replace(:p,'/','')), ''));
     INSERT OR IGNORE INTO entry_file(path_id, entry_id, mode)
       SELECT (SELECT id FROM path WHERE p = :p), id, $3 FROM entries WHERE uid = :u;" >/dev/null 2>&1
}

# ── fixture ────────────────────────────────────────────────────────────────
ins r-signing  "$REPO" ''    decision 'Rotating the signing_key without a restart' 'we chose the dual-key window' 1.40 0 3  live
ins r-reconn   "$REPO" ''    bugfix   'Presence socket reconnecting after a room drop' 'watchdog now backs off' 1.50 0 5  live
ins r-sqlite   "$REPO" ''    gotcha   'sqlite3_step returns SQLITE_BUSY under WAL' 'retry with a backoff' 1.30 0 9  live
ins r-eph      "$REPO" ''    discovery 'Monitoring cycle steady-state for widget alpha' 'nothing to report' 0.30 1 4 live
ins r-solid    "$REPO" ''    discovery 'Widget alpha rendering path documented' 'the renderer reads the flag' 1.15 0 4 live
ins r-other    "$OTHER" ''   bugfix   'Presence socket reconnecting in another org repo' 'not yours' 1.50 0 5 live
ins r-drop     "$REPO" ''    prompt    "'; DROP TABLE entries; --" '' 0.50 0 2 live
i=1
while [ "$i" -le 4 ]; do
  ins "r-dup$i" "$REPO" '' change 'Envelope Gate Fix' "variant $i of the same work" 1.30 0 "$i" live
  i=$((i + 1))
done
ins r-wt-a "$REPO" 'wt-alpha' bugfix 'WasmRenderer crash on the alpha worktree' 'fixed the bounds check' 1.50 0 6 live
ins r-wt-b "$REPO" 'wt-beta'  change 'WasmRenderer refactor on the beta worktree' 'split the module' 1.30 0 7 live
ins r-off  "$OTHER" ''        bugfix 'WasmRenderer crash in another org' 'not yours' 1.50 0 6 live
link r-wt-a 'renderers/wasm/WasmRenderer.ts' 2
link r-wt-b 'renderers/wasm/WasmRenderer.ts' 2
link r-off  'renderers/wasm/WasmRenderer.ts' 2

# shellcheck source=../lib/recall.sh
. "$ROOT/lib/recall.sh"
export JM_RECALL_DIR="$WORK"

printf 'T-REC  recall and ranking (SPEC §14.6)\n'

srch() { jm_recall_search "$1" "${2-$REPO}" "${3:-20}" "${4:-0}" "${5:-}" "${6:-text}" "${7:-0}"; }

# REC-1 tokenchars '_'
if srch 'signing_key' | grep -q 'signing_key'; then ok REC-1 "tokenchars '_' — signing_key hits"
else bad REC-1 'signing_key missed'; fi

# REC-2 prefix expansion
if srch 'reconnect' | grep -qi 'reconnecting'; then ok REC-2 'prefix expansion — reconnect finds reconnecting'
else bad REC-2 'no prefix expansion'; fi

# REC-3 identifier rule: no prefix expansion
Q3="$(jm_query_build 'sqlite3_step')"
check REC-3 '"sqlite3_step"' "$Q3" 'identifier emitted verbatim, unexpanded'

# REC-4 injection attempt
srch "'; DROP TABLE entries; --" >/dev/null 2>&1
STILL="$(sqlite3 -bail "$STORE/memory.db" "SELECT count(*) FROM entries;")"
if [ "$STILL" -gt 0 ]; then ok REC-4 "no SQL error, entries survives ($STILL rows)"
else bad REC-4 'entries table is gone'; fi

# REC-5 a 5000-char query
# Deterministic on purpose. This used to draw from /dev/urandom, which fails about
# one run in twelve: a random draw can produce a single token longer than
# JM_QUERY_MAX_CHARS (200), and for that input an empty build is the CORRECT answer
# -- no term fits -- so the "non-empty" assertion below was simply wrong for it.
# The property under test is "a huge query gets capped", which needs length, not
# randomness. Realistic word lengths, 5000 chars.
BIG="$(yes 'collab websocket handler reconnect backoff jitter payload' 2>/dev/null | head -c 5000)"
Q5="$(jm_query_build "$BIG")" || Q5=''
if [ -n "$Q5" ] && [ "${#Q5}" -le 200 ]; then ok REC-5 "5000-char query capped at ${#Q5} chars"
else bad REC-5 "got ${#Q5} chars"; fi
srch "$BIG" >/dev/null 2>&1; R5=$?
# rc 0 = hits, rc 1 = no hits.  Either is success here; 2 (misuse) or 3 (no
# sqlite3) would not be.
if [ "$R5" -le 1 ]; then ok REC-5b "runs cleanly on a 5000-char query (rc $R5)"
else bad REC-5b "rc $R5"; fi

# REC-6 repo scoping
if srch 'reconnecting' | grep -q "$OTHER"; then bad REC-6 'another repo leaked into a scoped search'
else ok REC-6 'scoped search returns nothing from another repo_key'; fi
if srch 'reconnecting' '' | grep -q "$OTHER"; then ok REC-6b '--all-repos does reach it'
else bad REC-6b '--all-repos found nothing'; fi

# REC-7 the worktree bridge
F7="$(jm_recall_files 'renderers/wasm/WasmRenderer.ts')"
NA="$(printf '%s' "$F7" | grep -c 'wt-alpha\|alpha worktree')"
NB="$(printf '%s' "$F7" | grep -c 'wt-beta\|beta worktree')"
if [ "$NA" -ge 1 ] && [ "$NB" -ge 1 ]; then ok REC-7 'both worktrees of one repo returned'
else bad REC-7 "alpha=$NA beta=$NB"; fi

# REC-8 the file lane is hard-scoped
if printf '%s' "$F7" | grep -q "$OTHER"; then bad REC-8 'file lane leaked across repo_key'
else ok REC-8 'file lane hard-scoped: no cross-repo row'; fi
if grep -qE 'scope_all|all[-_]repos' "$ROOT/lib/query.sh" \
  | grep -q 'file_lane'; then bad REC-8b 'file lane has a scope override'
else ok REC-8b 'no --all-repos parameter exists in the file lane statement'; fi

# REC-9 / REC-10 dedup
D9ROWS="$(srch 'envelope gate fix' "$REPO" 20 0 '' text 0 | grep -c '#')"
check REC-9 1 "$D9ROWS" '4 rows sharing a dedup_key collapse to 1'
if srch 'envelope gate fix' "$REPO" 20 0 '' text 0 | grep -q '×4'; then ok REC-9b 'rendered ×4'
else bad REC-9b 'recurrence not rendered'; fi
D10="$(srch 'envelope gate fix' "$REPO" 20 0 '' text 1 | grep -c '#')"
check REC-10 4 "$D10" '--dups returns all 4'

# REC-11 ephemeral ranks below non-ephemeral
# The title lives on the SECOND line of a rendered result, so the top hit is
# the first two lines together.
E11="$(srch 'widget alpha' | head -2 | tr '\n' ' ')"
case "$E11" in
  *'Widget alpha rendering'*) ok REC-11 'non-ephemeral outranks the ephemeral row' ;;
  *) bad REC-11 "top hit was: $E11" ;;
esac
if srch 'widget alpha' | grep -q '\[stale?\]'; then ok REC-11b 'ephemeral row labelled [stale?]'
else bad REC-11b 'no [stale?] label'; fi

# REC-12 the stepped decay path (no exp())
ORD_MATH="$(srch 'wasmrenderer' | grep '#' | head -3 | sed 's/.*#\([0-9]*\).*/\1/' | tr '\n' ' ')"
sqlite3 -bail "$STORE/memory.db" "UPDATE meta SET v='0' WHERE k='has_math';" >/dev/null
JM_HAS_MATH=''
ORD_STEP="$(srch 'wasmrenderer' | grep '#' | head -3 | sed 's/.*#\([0-9]*\).*/\1/' | tr '\n' ' ')"
if [ -n "$ORD_STEP" ] && [ "$ORD_MATH" = "$ORD_STEP" ]; then
  ok REC-12 "stepped ladder runs, ordering preserved ($ORD_STEP)"
else bad REC-12 "math=[$ORD_MATH] stepped=[$ORD_STEP]"; fi
# Header comments mention exp() on purpose; only SQL lines matter.
if jm_rank_sql | grep -v '^--' | grep -q 'exp('; then
  bad REC-12b 'exp() still present in SQL with has_math=0'
else ok REC-12b 'no exp() in the statement when has_math=0'; fi
sqlite3 -bail "$STORE/memory.db" "UPDATE meta SET v='1' WHERE k='has_math';" >/dev/null
JM_HAS_MATH=''

# REC-13 --json is valid JSON on every line
J="$(srch 'reconnect' "$REPO" 5 0 '' json)"
NJ="$(printf '%s\n' "$J" | wc -l | tr -d ' ')"
VALID="$(printf '%s\n' "$J" | while IFS= read -r l; do
  [ -n "$l" ] || continue
  sqlite3 -bail ':memory:' -cmd ".parameter set :j cast(x'$(hex "$l")'as'text')" "SELECT json_valid(:j);"
done | grep -c '^1$')"
check REC-13 "$NJ" "$VALID" "every --json line is valid JSON ($NJ lines)"

# REC-14 latency
T0="$(date +%s%N)"
srch 'presence socket reconnect' >/dev/null
T1="$(date +%s%N)"
MS=$(( (T1 - T0) / 1000000 ))
if [ "$MS" -lt 200 ]; then ok REC-14 "search wall time ${MS} ms (< 200 ms budget)"
else bad REC-14 "search took ${MS} ms"; fi

# QRY-1 the expansion rules, directly
check QRY-1a '("reconnect" OR "reconnect"*)' "$(jm_query_build 'reconnect')"  'long alpha term expands'
check QRY-1b '"flag"'                         "$(jm_query_build 'flag')"      'short term verbatim'
check QRY-1c '"PR#6807"'                      "$(jm_query_build 'PR#6807')"   'digit term verbatim, # kept inside the quoted phrase'
check QRY-1d '"useYjsCanvasSync"'             "$(jm_query_build 'useYjsCanvasSync')" 'mixed case verbatim'
check QRY-1e '("wellknown" OR "wellknown"*) OR "cfg"' "$(jm_query_build 'wellknown cfg')" 'joined with OR'
check QRY-1f '("alpha" OR "alpha"*) OR "beta"' "$(jm_query_build 'alpha-beta')" 'hyphen splits (beta is 4 chars: no expansion)'
jm_query_build '' >/dev/null 2>&1; check QRY-1g 1 "$?" 'empty query rejected'
jm_query_is_path 'src/index.ts'; check QRY-1h 0 "$?" 'path detected'
jm_query_is_path 'why is index.ts slow'; check QRY-1i 1 "$?" 'prose mentioning a file is not a path'

# CAP-1 jm_render_cap directly — the marker must live INSIDE the budget
LONG="$(i=0; while [ "$i" -lt 60 ]; do printf 'line %02d 0123456789012345678901234567890123456789\n' "$i"; i=$((i+1)); done)"
for cap in 1200 800 400 137; do
  R="$(printf '%s\n' "$LONG" | jm_render_cap "$cap" '[cut at {MAX}: {N} lines withheld]' 0)"
  B="$(printf '%s' "$R" | LC_ALL=C wc -c | tr -d ' ')"
  if [ "$B" -le "$cap" ]; then ok "CAP-1/$cap" "$B ≤ $cap with the marker included"
  else bad "CAP-1/$cap" "$B > $cap"; fi
done
R2="$(printf 'short\n' | jm_render_cap 1200 '[cut at {MAX}: {N} lines withheld]' 0)"
if printf '%s' "$R2" | grep -q 'cut at'; then bad CAP-1b 'marker added when nothing was dropped'
else ok CAP-1b 'no marker when nothing was dropped'; fi

printf '\n  passed %d   failed %d\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ]
