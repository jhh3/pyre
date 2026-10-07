#!/usr/bin/env bash
# jhh3-mem — lib/note.sh
# The two EXPLICIT write paths: `note` (add one hand-authored memory) and
# `forget` (remove memories, FTS-consistently). SPEC §10.1.
#
# These are the only writes in the system that a human or an agent asks for by
# name. Everything else is the SessionEnd hook. They therefore follow the CLI
# policy, not the hook policy: they fail LOUD with honest exit codes and they
# say what they did.
#
# INVARIANTS ENFORCED HERE
#   - Every agent- or user-supplied byte reaches sqlite3 as a BOUND PARAMETER
#     (jm_db_param_write). There is no string interpolation into SQL here.
#   - Redaction is FAIL CLOSED, identical to capture: if sed fails, nothing is
#     written. A hand-authored note is exactly where a pasted credential shows
#     up, so this path may not be the lenient one.
#   - `forget` deletes through the `entries` table so the AFTER DELETE trigger
#     keeps entries_fts consistent; it never touches entries_fts directly.
#   - `forget` is a DRY RUN unless --yes. Deleting memory is a deliberate act.
#
# shellcheck shell=bash

if [ -n "${JM_NOTE_SOURCED:-}" ]; then
  # shellcheck disable=SC2317  # reached when executed rather than sourced
  return 0 2>/dev/null || exit 0
fi
JM_NOTE_SOURCED=1

if [ -z "${JM_CAPTURE_SOURCED:-}" ]; then
  _jm_n_src="${BASH_SOURCE[0]}"
  case "$_jm_n_src" in */*) _jm_n_dir="${_jm_n_src%/*}" ;; *) _jm_n_dir="." ;; esac
  # shellcheck source=./capture.sh
  . "$_jm_n_dir/capture.sh"
  unset _jm_n_src _jm_n_dir
fi

# The kinds a note may claim. `session` is deliberately absent: a note is not a
# session, and letting one masquerade as one would corrupt the one distinction
# the whole store is organised around.
JM_NOTE_KINDS='note decision bugfix gotcha security refactor change feature discovery other'

# jm_note_kind_ok KIND — rc 0 valid · 1 not.
jm_note_kind_ok() {
  local k="${1:-}" v
  for v in $JM_NOTE_KINDS; do
    [ "$k" = "$v" ] && return 0
  done
  return 1
}

# jm_note_run TEXT [KIND] [DIR]
# stdout: one confirmation line. rc 0 written · 1 failure · 2 misuse · 3 no sqlite3.
jm_note_run() {
  local text="${1:-}" kind="${2:-note}" dir="${3:-$PWD}"
  [ -n "$text" ] || { printf 'note: the note text is required\n' >&2; return 2; }
  if ! jm_note_kind_ok "$kind"; then
    printf 'note: --kind must be one of: %s\n' "$JM_NOTE_KINDS" >&2
    return 2
  fi
  jm_require_sqlite || { printf 'note: sqlite3 is not installed\n' >&2; return 3; }

  # A note is allowed to create the store: unlike SessionStart it is an explicit
  # act by the user, and refusing to record the first memory on a fresh machine
  # would be pedantry.
  local grc=0
  jm_db_gate || grc=$?
  case "$grc" in
    0) : ;;
    1)
      if ! jm_db_bootstrap; then
        printf 'note: could not create the store at %s (%s)\n' "$JM_DB" "$JM_DB_LAST_ERR" >&2
        return 1
      fi
      ;;
    2)
      printf 'note: the database schema is out of date — run `jhh3-mem upgrade` first\n' >&2
      return 1
      ;;
    *)
      printf 'note: the database is newer than this plugin (%s > %s); not writing\n' \
        "$(jm_db_schema_version)" "$JM_SCHEMA_VERSION" >&2
      return 1
      ;;
  esac

  local stage
  stage="$(jm_mkstage)" || { printf 'note: could not create a staging directory\n' >&2; return 1; }

  # ── redact BEFORE truncating, so a cut can never split a secret ──
  printf '%s\n' "$text" >"$stage/note.txt"
  chmod 0600 "$stage/note.txt" 2>/dev/null || true
  local red=0 rrc=0
  red="$(jm_capture_redact "$stage/note.txt" "$stage/note.red.txt")" || rrc=$?
  if [ "$rrc" -ne 0 ]; then
    rm -rf -- "$stage"
    printf 'note: redaction failed; nothing was written\n' >&2
    return 1
  fi
  red="$(_jm_cap_int "$red" 0)"

  local clean
  clean="$(jm_sanitize <"$stage/note.red.txt")"
  rm -rf -- "$stage"

  local title body
  title="$(printf '%s' "$clean" | jm_flatten | jm_truncate_bytes "$JM_CAP_LIM_TITLE" | jm_flatten)"
  [ -n "$title" ] || { printf 'note: the note is empty after sanitization\n' >&2; return 2; }
  # The full text goes to `outcome` when it says more than the title does; that
  # is the field the digest renders as the continuation line and the field
  # `search` prints under the hit.
  body=''
  if [ "$(printf '%s' "$clean" | jm_flatten)" != "$title" ]; then
    body="$(printf '%s' "$clean" | jm_truncate_bytes "$JM_CAP_LIM_OUTCOME")"
  fi

  local ident rk wt br rp
  ident="$(jm_repo_ident "$dir")"
  IFS=$'\x1f' read -r rk wt br rp <<<"$(_jm_cap_us "$ident")"

  local now uid dur
  now="$(jm_now_epoch)"
  uid="note:${now}:$(jm_sha256_str "$rk|$title" | cut -c1-12)"
  # A hand-authored note is the highest-quality content in the store and is
  # ranked accordingly (§5.3): kind `note` carries the second-highest prior.
  dur="$(jm_capture_durability "$kind" 0 0 0)" || dur='1.12'

  local sql
  sql="INSERT OR IGNORE INTO entries(
         uid, source_bytes, origin, producer, host, repo_key, worktree, branch, repo_path,
         kind, ts, started_at, title, request, outcome, capture_mode, end_reason,
         turns, redactions, durability, dedup_key, n_mod, n_read,
         ephemeral, injectable, superseded)
       VALUES(
         :uid, 0, 'noted', :producer, :host, :repo_key, :worktree, :branch, :repo_path,
         :kind, CAST(:ts AS INTEGER), CAST(:ts AS INTEGER), :title, '', :outcome,
         'noted', 'manual', 0, CAST(:red AS INTEGER), CAST(:dur AS REAL),
         substr(replace(replace(replace(replace(replace(lower(:title),' ',''),'-',''),'_',''),'.',''),':',''),1,48),
         0, 0, 0, 1, 0);"

  local wrc=0
  jm_db_param_write "$sql" \
    uid "$uid" producer "$JM_PRODUCER" host "$(jm_current_host)" repo_key "$rk" worktree "$wt" \
    branch "$br" repo_path "$rp" kind "$kind" ts "$now" \
    title "$title" outcome "$body" red "$red" dur "$dur" || wrc=$?
  if [ "$wrc" -ne 0 ]; then
    printf 'note: write failed (rc %s: %s)\n' "$wrc" "$JM_DB_LAST_ERR" >&2
    jm_log note error "rc=$wrc repo=$rk"
    return 1
  fi

  jm_db_upsert_ops note ok "repo=$rk" "uid=$uid" "redactions=$red" "rows=1" >/dev/null 2>&1 || true
  jm_log note ok "repo=$rk red=$red"
  printf 'noted  %s  [%s]  %s\n' "$rk" "$kind" "$title"
  [ "$red" -gt 0 ] && printf '       %s value(s) redacted before storage\n' "$red"
  return 0
}

# ── forget ──────────────────────────────────────────────────────────────────

# _jm_forget_since SPEC — epoch for `30d` / `12w` / `2026-01-31`, or empty.
_jm_forget_since() {
  local s="${1:-}" n unit
  case "$s" in
    '') return 1 ;;
    [0-9]*d | [0-9]*w | [0-9]*m | [0-9]*y)
      n="${s%[dwmy]}"; unit="${s##*[0-9]}"
      case "$n" in '' | *[!0-9]*) return 1 ;; esac
      case "$unit" in
        d) printf '%s\n' "$(( $(jm_now_epoch) - n * 86400 ))" ;;
        w) printf '%s\n' "$(( $(jm_now_epoch) - n * 604800 ))" ;;
        m) printf '%s\n' "$(( $(jm_now_epoch) - n * 2592000 ))" ;;
        y) printf '%s\n' "$(( $(jm_now_epoch) - n * 31536000 ))" ;;
      esac
      return 0
      ;;
    [0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9])
      n="$(date -d "$s" +%s 2>/dev/null || date -j -f '%Y-%m-%d %H:%M:%S' "$s 00:00:00" +%s 2>/dev/null)" || n=''
      case "$n" in '' | *[!0-9]*) return 1 ;; esac
      printf '%s\n' "$n"
      return 0
      ;;
  esac
  return 1
}

# jm_forget_run SESSION REPO BEFORE MATCHING YES
# stdout: what matched and (with YES=1) what was removed.
# rc 0 ok · 1 failure · 2 misuse · 3 no sqlite3.
jm_forget_run() {
  local session="${1:-}" repo="${2:-}" before="${3:-}" matching="${4:-}" yes="${5:-0}"
  jm_require_sqlite || { printf 'forget: sqlite3 is not installed\n' >&2; return 3; }
  jm_db_present || { printf 'forget: no memory store at %s\n' "$JM_DB" >&2; return 1; }

  if [ -z "$session" ] && [ -z "$repo" ] && [ -z "$before" ] && [ -z "$matching" ]; then
    printf 'forget: refusing to match everything — give at least one of --session/--repo/--before/--matching\n' >&2
    return 2
  fi

  local before_epoch=''
  if [ -n "$before" ]; then
    before_epoch="$(_jm_forget_since "$before")" || {
      printf 'forget: --before wants 30d / 12w / 6m / 1y or YYYY-MM-DD, not %s\n' "$before" >&2
      return 2
    }
  fi

  # Each predicate is inert unless its parameter is non-empty, so the same
  # statement text serves every combination and nothing is interpolated.
  local where="1=1"
  where="$where AND (:session = '' OR uid = :session)"
  where="$where AND (:repo = '' OR repo_key = :repo)"
  where="$where AND (:before = '' OR ts < CAST(:before AS INTEGER))"
  where="$where AND (:matching = '' OR instr(lower(title || ' ' || request || ' ' || outcome || ' ' || learned || ' ' || decided || ' ' || facts), lower(:matching)) > 0)"

  local -a params=(
    session "$session" repo "$repo" before "$before_epoch" matching "$matching"
  )

  local n
  n="$(jm_db_param_read "SELECT count(*) FROM entries WHERE $where;" "${params[@]}")" || {
    printf 'forget: could not read the store (%s)\n' "$JM_DB_LAST_ERR" >&2
    return 1
  }
  case "$n" in '' | *[!0-9]*) n=0 ;; esac

  if [ "$n" -eq 0 ]; then
    printf 'forget: nothing matched\n'
    return 0
  fi

  if [ "$yes" != '1' ]; then
    printf 'DRY RUN — nothing was removed. Re-run with --yes to delete.\n'
    printf 'would remove %s entr%s:\n' "$n" "$([ "$n" -eq 1 ] && printf 'y' || printf 'ies')"
    jm_db_param_read "SELECT '  ' || strftime('%Y-%m-%d', ts, 'unixepoch') || '  ' ||
                             substr(repo_key, 1, 28) || '  ' || substr(title, 1, 70)
                        FROM entries WHERE $where ORDER BY ts DESC LIMIT 20;" "${params[@]}" || true
    [ "$n" -gt 20 ] && printf '  ... and %s more\n' "$((n - 20))"
    return 0
  fi

  # entry_file goes with the entry through ON DELETE CASCADE (jm_db_param_write
  # sets foreign_keys=ON), entries_fts through the AFTER DELETE trigger, and the
  # now-unreferenced `path` rows through the sweep below — the one table nothing
  # else would ever reclaim.
  local wrc=0
  jm_db_param_write "BEGIN IMMEDIATE;
     DELETE FROM entries WHERE $where;
     DELETE FROM path WHERE id NOT IN (SELECT path_id FROM entry_file);
     COMMIT;" "${params[@]}" || wrc=$?
  if [ "$wrc" -ne 0 ]; then
    printf 'forget: delete failed (rc %s: %s)\n' "$wrc" "$JM_DB_LAST_ERR" >&2
    jm_log forget error "rc=$wrc rows=$n"
    return 1
  fi

  jm_db_upsert_ops forget ok "rows=$n" "repo=$repo" >/dev/null 2>&1 || true
  jm_log forget ok "rows=$n repo=$repo"
  printf 'removed %s entr%s\n' "$n" "$([ "$n" -eq 1 ] && printf 'y' || printf 'ies')"
  printf 'run `jhh3-mem doctor --deep` if you want the FTS index verified, or\n'
  printf '`sqlite3 -bail %s "VACUUM;"` to reclaim the file space.\n' "$JM_DB"
  return 0
}
