#!/usr/bin/env bash
# jhh3-mem — lib/recall.sh
# The explicit, on-demand retrieval path: `search`, `files`, `recent`, `show`.
# SPEC §5, §6.4, §10.1.
#
# This is the half of the design that the old tool did not have: memory the
# agent PULLS when it decides it needs it, instead of memory pushed into every
# session.  The failure policy is therefore the opposite of the hooks': the CLI
# fails LOUD with honest exit codes, because a diagnosis tool that swallows
# errors is worse than no tool.
#
# INVARIANTS ENFORCED HERE
#   - Read-only.  Nothing in this file writes to the database.
#   - Every agent- or user-supplied byte reaches sqlite3 as a BOUND PARAMETER.
#     There is no string interpolation into SQL anywhere in this file.
#   - The file lane is hard-scoped to the current repo_key with NO override
#     (SPEC §13.3): `src/index.ts` exists in many of 19 client orgs' repos.
#   - Legacy rows are labelled `[migrated]` and ephemeral rows `[stale?]`, so
#     provenance is visible rather than implied.
#
# shellcheck shell=bash

set -uo pipefail

if [ -n "${JM_RECALL_SOURCED:-}" ]; then
  # shellcheck disable=SC2317  # reached when executed rather than sourced
  return 0 2>/dev/null || exit 0
fi
JM_RECALL_SOURCED=1

_jm_c_src="${BASH_SOURCE[0]}"
case "$_jm_c_src" in */*) _jm_c_dir="${_jm_c_src%/*}" ;; *) _jm_c_dir="." ;; esac
if [ -z "${JM_QUERY_SOURCED:-}" ]; then
  # shellcheck source=./query.sh
  . "$_jm_c_dir/query.sh"
fi
if [ -z "${JM_RENDER_SOURCED:-}" ]; then
  # shellcheck source=./render.sh
  . "$_jm_c_dir/render.sh"
fi
unset _jm_c_src _jm_c_dir

JM_RECALL_HALF_LIFE=120
JM_RECALL_SCOPE_PENALTY='0.45'
JM_RECALL_TITLE_MAX=96
readonly JM_RECALL_HALF_LIFE JM_RECALL_SCOPE_PENALTY JM_RECALL_TITLE_MAX

# ── shared row rendering ────────────────────────────────────────────────────

# _jm_recall_flags ORIGIN EPHEMERAL — the provenance tags for one row.
_jm_recall_flags() {
  local origin="${1:-}" eph="${2:-0}" tags=''
  case "$origin" in
    legacy-*) tags=' [migrated]' ;;
  esac
  [ "$eph" = '1' ] && tags="$tags [stale?]"
  printf '%s' "$tags"
}

# _jm_recall_render_text MODE   (filter: 0x1F rows on stdin)
# MODE prose | file
_jm_recall_render_text() {
  local mode="${1:-prose}"
  local id uid ts kind rkey wt origin eph rec score title cont anchor
  local scope date tags rectag titles
  while IFS=$'\x1f' read -r id uid ts kind rkey wt origin eph rec score title cont anchor; do
    [ -n "${id:-}" ] || continue
    date="$(printf '%s' "$(jm_iso_local "$ts")" | LC_ALL=C cut -c1-10)"
    scope="$rkey"
    [ -n "${wt:-}" ] && scope="$rkey:$wt"
    tags="$(_jm_recall_flags "${origin:-}" "${eph:-0}")"
    rectag=''
    case "${rec:-1}" in '' | 1) : ;; *) rectag=" ×$rec" ;; esac
    # Same read-time structural defang the digest applies (lib/render.sh): CLI
    # output lands in the agent's context too, and migrated rows predate the
    # write-time rule.
    titles="$(printf '%s' "$title" | jm_flatten | jm_defang)"
    if [ "$mode" = 'file' ]; then
      printf '%7s  #%-6s %s  %-9s %s\n         %s%s%s\n' \
        "$score" "$id" "$date" "$kind$rectag" "$(jm_bcut "${anchor:-}" 70)" \
        "$(jm_bcut "$titles" "$JM_RECALL_TITLE_MAX")" "$tags" ''
    else
      printf '%7s  #%-6s %s  %-9s %s\n         %s%s\n' \
        "$score" "$id" "$date" "$kind$rectag" "$(jm_bcut "$scope" 46)" \
        "$(jm_bcut "$titles" "$JM_RECALL_TITLE_MAX")" "$tags"
    fi
    if [ -n "${cont:-}" ]; then
      printf '         %s\n' "$(jm_bcut "$(printf '%s' "$cont" | jm_flatten | jm_defang)" 150)"
    fi
  done
  return 0
}

# _jm_recall_emit FORMAT MODE ROWS — text or NDJSON, and the empty-result rc.
_jm_recall_emit() {
  local fmt="${1:-text}" mode="${2:-prose}" rows="${3:-}"
  [ -n "$rows" ] || return 1
  if [ "$fmt" = 'json' ]; then
    printf '%s\n' "$rows"
  else
    printf '%s\n' "$rows" | _jm_recall_render_text "$mode"
  fi
  return 0
}

# ── search ──────────────────────────────────────────────────────────────────

# jm_recall_search QUERY REPO_KEY_OR_EMPTY LIMIT SINCE_EPOCH KIND_OR_EMPTY FORMAT [DUPS]
#
# REPO_KEY empty  => --all-repos (other repos survive at scope_mult 0.45)
# DUPS '1'        => return every member of a dedup_key group, not just rank 1
#
# rc 0 results · 1 no results · 2 bad args · 3 no sqlite3
jm_recall_search() {
  [ $# -ge 6 ] || return 2
  local raw="${1:-}" repo="${2:-}" limit="${3:-20}" since="${4:-0}"
  local kind="${5:-}" fmt="${6:-text}" dups="${7:-0}"
  [ -n "$raw" ] || return 2
  case "$limit" in '' | *[!0-9]*) limit=20 ;; esac
  case "$since" in '' | *[!0-9]*) since=0 ;; esac
  jm_require_sqlite || return 3
  jm_db_present || return 1

  # A path-shaped query belongs to the file lane, which is hard-scoped and
  # cannot be reached with --all-repos (SPEC §5.5 step 5, §13.3).
  if jm_query_is_path "$raw"; then
    jm_recall_files "$raw"
    return $?
  fi

  local match
  match="$(jm_query_build "$raw")" || return 1

  local scope_all=0 dup_cap=1
  [ -n "$repo" ] || scope_all=1
  [ "$dups" = '1' ] && dup_cap=1000000

  local wt=''
  if [ -n "${JM_RECALL_WORKTREE:-}" ]; then wt="$JM_RECALL_WORKTREE"; fi

  # Semantic candidates, when the optional feature is installed AND a key is set.
  # Every failure here -- no extension, no key, offline, timeout, malformed reply --
  # leaves sem_ids empty, which makes this call byte-identical to the lexical-only
  # one. That is the whole safety property: semantic search can never make search
  # worse than it was, only wider.
  local sem_ids=''
  if [ "${JM_RECALL_NO_SEMANTIC:-0}" != '1' ] \
     && [ -r "${JM_LIB_DIR:-}/embed.sh" ]; then
    # shellcheck source=embed.sh
    . "${JM_LIB_DIR}/embed.sh" 2>/dev/null || true
    if command -v jm_embed_ready >/dev/null 2>&1 && jm_embed_ready; then
      local qvec
      qvec="$(jm_embed_query "$raw" 2>/dev/null)" || qvec=''
      if [ -n "$qvec" ]; then
        sem_ids="$(jm_vec_read "SELECT group_concat(id) FROM (
                     SELECT id FROM entry_vec WHERE e MATCH vec_f32('[$qvec]') AND k = 60);" 2>/dev/null)"
        # Validate before binding: this value traces back to an HTTP response, so it
        # is judged on its shape, not trusted for its provenance. rank.sql has its
        # own backstop, but the lane should never rely on it.
        sem_ids="$(jm_embed_valid_ids "$sem_ids" 2>/dev/null)" || sem_ids=''
      fi
    fi
  fi

  local rows rc=0
  rows="$(jm_rank_run \
    q "$match" \
    sem_ids "$sem_ids" \
    repo_key "$repo" \
    worktree "$wt" \
    scope_all "$scope_all" \
    scope_penalty "$JM_RECALL_SCOPE_PENALTY" \
    half_life "$JM_RECALL_HALF_LIFE" \
    durability_floor 0.0 \
    origin_filter '' \
    kind "$kind" \
    since_epoch "$since" \
    now "$(jm_now_epoch)" \
    limit "$limit" \
    uid_cap 1000000 \
    dup_cap "$dup_cap" \
    order_recency 0 \
    injectable_only 0 \
    fmt "$fmt")" || rc=$?
  if [ "$rc" -ne 0 ]; then
    jm_log search error "rc=$rc"
    return 1
  fi
  _jm_recall_emit "$fmt" prose "$rows"
}

# ── the file lane ───────────────────────────────────────────────────────────

# jm_recall_files PATH...
# HARD-SCOPED to the current repo_key.  There is deliberately no --all-repos
# here and no parameter that could introduce one (REC-8, SPEC §13.3).
# Env overrides for the CLI: JM_RECALL_FORMAT, JM_RECALL_LIMIT, JM_RECALL_DIR.
# rc 0 results · 1 none · 2 bad args · 3 no sqlite3
jm_recall_files() {
  [ $# -ge 1 ] || return 2
  jm_require_sqlite || return 3
  jm_db_present || return 1

  local fmt="${JM_RECALL_FORMAT:-text}" limit="${JM_RECALL_LIMIT:-20}"
  local dir="${JM_RECALL_DIR:-$PWD}"
  case "$limit" in '' | *[!0-9]*) limit=20 ;; esac

  local ident repo repo_path p rel paths=''
  ident="$(jm_repo_ident "$dir")"
  repo="${ident%%$'\t'*}"
  repo_path="$(printf '%s' "$ident" | LC_ALL=C cut -f4)"

  for p in "$@"; do
    [ -n "$p" ] || continue
    rel="$(jm_rel_path "$repo_path" "$p")"
    [ -n "$rel" ] || continue
    paths="${paths}${rel}"$'\n'
  done
  [ -n "$paths" ] || return 2
  paths="${paths%$'\n'}"

  local rows rc=0
  rows="$(jm_file_lane_run "$paths" \
    repo_key "$repo" \
    durability_floor 0.0 \
    origin_filter '' \
    kind '' \
    since_epoch 0 \
    limit "$limit" \
    uid_cap 1000000 \
    dup_cap 1 \
    injectable_only 0 \
    fmt "$fmt")" || rc=$?
  if [ "$rc" -ne 0 ]; then
    jm_log files error "rc=$rc"
    return 1
  fi
  _jm_recall_emit "$fmt" file "$rows"
}

# ── recent ──────────────────────────────────────────────────────────────────

# jm_recall_recent REPO_KEY_OR_EMPTY LIMIT FORMAT
# Reverse-chronological.  Unlike the digest this has no durability floor and no
# origin filter: `recent` is the "show me everything you kept" view.
# rc 0 · 1 none · 3 no sqlite3
jm_recall_recent() {
  local repo="${1:-}" limit="${2:-20}" fmt="${3:-text}"
  case "$limit" in '' | *[!0-9]*) limit=20 ;; esac
  jm_require_sqlite || return 3
  jm_db_present || return 1

  local scope_all=0
  [ -n "$repo" ] || scope_all=1

  local rows rc=0
  rows="$(jm_rank_run \
    q '' \
    repo_key "$repo" \
    worktree '' \
    scope_all "$scope_all" \
    scope_penalty "$JM_RECALL_SCOPE_PENALTY" \
    half_life 0 \
    durability_floor 0.0 \
    origin_filter '' \
    kind '' \
    since_epoch 0 \
    now "$(jm_now_epoch)" \
    limit "$limit" \
    uid_cap 1000000 \
    dup_cap 1000000 \
    order_recency 1 \
    injectable_only 0 \
    fmt "$fmt")" || rc=$?
  [ "$rc" -eq 0 ] || return 1
  _jm_recall_emit "$fmt" prose "$rows"
}

# ── show ────────────────────────────────────────────────────────────────────

# jm_recall_show ID_OR_UID [FORMAT]
# One entry, every non-empty field, labelled and verbatim: "show me exactly
# what you kept".  rc 0 · 1 not found · 2 bad args · 3 no sqlite3
jm_recall_show() {
  local sel="${1:-}" fmt="${2:-text}"
  [ -n "$sel" ] || return 2
  jm_require_sqlite || return 3
  jm_db_present || return 1

  local sql
  if [ "$fmt" = 'json' ]; then
    sql="SELECT json_object(
           'id', id, 'uid', uid, 'origin', origin, 'producer', producer,
           'repo_key', repo_key, 'worktree', worktree, 'branch', branch,
           'repo_path', repo_path, 'kind', kind, 'ts', ts,
           'date', strftime('%Y-%m-%dT%H:%M:%SZ', ts, 'unixepoch'),
           'title', title, 'request', request, 'outcome', outcome,
           'learned', learned, 'decided', decided, 'next_steps', next_steps,
           'facts', facts, 'concepts', concepts, 'files_touched', files_touched,
           'commands', commands, 'capture_mode', capture_mode,
           'end_reason', end_reason, 'turns', turns, 'redactions', redactions,
           'durability', durability, 'n_mod', n_mod, 'n_read', n_read,
           'ephemeral', ephemeral, 'injectable', injectable,
           'superseded', superseded, 'source_bytes', source_bytes)
         FROM entries WHERE uid = :sel OR CAST(id AS TEXT) = :sel
         ORDER BY id DESC LIMIT 1;"
  else
    sql="WITH e AS (SELECT * FROM entries WHERE uid = :sel OR CAST(id AS TEXT) = :sel
                    ORDER BY id DESC LIMIT 1),
              f AS (SELECT group_concat(p, char(10) || '                ') AS fl
                      FROM (SELECT pa.p AS p FROM entry_file ef
                              JOIN path pa ON pa.id = ef.path_id
                              JOIN e ON e.id = ef.entry_id
                             ORDER BY ef.mode DESC, pa.p LIMIT 40))
         SELECT 'id              ' || e.id           || char(10) ||
                'uid             ' || e.uid          || char(10) ||
                'origin          ' || e.origin       || '  (' || e.capture_mode || ')' || char(10) ||
                'when            ' || strftime('%Y-%m-%d %H:%M:%S', e.ts, 'unixepoch') || ' UTC' || char(10) ||
                'repo            ' || e.repo_key ||
                  (CASE WHEN e.worktree <> '' THEN '  wt:' || e.worktree ELSE '' END) ||
                  (CASE WHEN e.branch   <> '' THEN '  @'   || e.branch   ELSE '' END) || char(10) ||
                'kind            ' || e.kind ||
                  '   durability ' || printf('%.2f', e.durability) ||
                  '   n_mod ' || e.n_mod || '   n_read ' || e.n_read ||
                  '   turns ' || e.turns ||
                  '   redactions ' || e.redactions ||
                  (CASE WHEN e.ephemeral  = 1 THEN '   [stale?]'      ELSE '' END) ||
                  (CASE WHEN e.superseded = 1 THEN '   [superseded]'  ELSE '' END) ||
                  (CASE WHEN e.injectable = 0 THEN '   [search-only]' ELSE '' END) || char(10) ||
                'title           ' || e.title || char(10) ||
                (CASE WHEN e.request       <> '' THEN 'request         ' || replace(e.request,       char(10), char(10) || '                ') || char(10) ELSE '' END) ||
                (CASE WHEN e.outcome       <> '' THEN 'outcome         ' || replace(e.outcome,       char(10), char(10) || '                ') || char(10) ELSE '' END) ||
                (CASE WHEN e.learned       <> '' THEN 'learned         ' || replace(e.learned,       char(10), char(10) || '                ') || char(10) ELSE '' END) ||
                (CASE WHEN e.decided       <> '' THEN 'decided         ' || replace(e.decided,       char(10), char(10) || '                ') || char(10) ELSE '' END) ||
                (CASE WHEN e.next_steps    <> '' THEN 'next_steps      ' || replace(e.next_steps,    char(10), char(10) || '                ') || char(10) ELSE '' END) ||
                (CASE WHEN e.facts         <> '' THEN 'facts           ' || replace(e.facts,         char(10), char(10) || '                ') || char(10) ELSE '' END) ||
                (CASE WHEN e.concepts      <> '' THEN 'concepts        ' || e.concepts || char(10) ELSE '' END) ||
                (CASE WHEN e.commands      <> '' THEN 'commands        ' || e.commands || char(10) ELSE '' END) ||
                (CASE WHEN coalesce(f.fl, '') <> '' THEN 'files           ' || f.fl || char(10) ELSE '' END)
           FROM e LEFT JOIN f;"
  fi

  local out rc=0
  out="$(jm_db_param_read "$sql" sel "$sel")" || rc=$?
  [ "$rc" -eq 0 ] || return 1
  [ -n "$out" ] || return 1
  printf '%s\n' "$out"
  return 0
}
