#!/usr/bin/env bash
# jhh3-mem — lib/render.sh
# Row -> display line; visible truncation marker; line-boundary byte cut.
# SPEC §6.4, §9.1, §9.2.
#
# INVARIANTS ENFORCED HERE
#   - Every length decision is in BYTES, measured under LC_ALL=C.  The digest
#     separator is `·` (U+00B7, 2 bytes) and titles are routinely non-ASCII, so
#     character counting would under-count and blow the cap.
#   - jm_render_cap's output is ≤ MAX_BYTES *including* the marker it appends.
#     This is the P81 regression: reserving the marker after the cut yields
#     1234 bytes against a 1200 cap.
#   - The cut is always on a line boundary, so the model never sees half a row.
#   - Truncation is always VISIBLE: a shortened title ends in `…`, and a
#     shortened block ends in the marker line.  Silent truncation is how a
#     memory layer starts lying.
#
# shellcheck shell=bash

if [ -n "${JM_RENDER_SOURCED:-}" ]; then
  # shellcheck disable=SC2317  # reached when executed rather than sourced
  return 0 2>/dev/null || exit 0
fi
JM_RENDER_SOURCED=1

if [ -z "${JM_COMMON_SOURCED:-}" ]; then
  _jm_r_src="${BASH_SOURCE[0]}"
  case "$_jm_r_src" in */*) _jm_r_dir="${_jm_r_src%/*}" ;; *) _jm_r_dir="." ;; esac
  # shellcheck source=./common.sh
  . "$_jm_r_dir/common.sh"
  unset _jm_r_src _jm_r_dir
fi

# Per-line caps (SPEC §9.1).
JM_RENDER_TITLE_MAX=120
JM_RENDER_CONT_MAX=180
# SPEC §9.1 states "≤ 34 bytes" for the `MM-DD · KIND · wt:NAME` prefix, but its
# own §9.2 example prefix (`08-15 · SESSION · wt:collab-monitor`) is 37 bytes —
# the two are inconsistent.  40 is enforced here so the canonical example
# renders unchanged; only the worktree component is ever shortened, and the
# binding constraint (the total byte cap) is unaffected either way.
JM_RENDER_PREFIX_MAX=40
JM_RENDER_INDENT='  '
JM_RENDER_CONT_INDENT='      '
JM_RENDER_SEP=' · '
readonly JM_RENDER_TITLE_MAX JM_RENDER_CONT_MAX JM_RENDER_PREFIX_MAX
readonly JM_RENDER_INDENT JM_RENDER_CONT_INDENT JM_RENDER_SEP

# ── byte primitives ─────────────────────────────────────────────────────────

# jm_blen STRING — byte length.
jm_blen() {
  local LC_ALL=C
  local s="${1-}"
  printf '%s\n' "${#s}"
}

# jm_bcut STRING MAX — STRING truncated to ≤ MAX bytes on a UTF-8 boundary,
# with a visible `…` when anything was dropped.  The ellipsis is inside the
# budget, never added on top of it.
jm_bcut() {
  local s="${1-}" max="${2:-0}"
  case "$max" in '' | *[!0-9]*) max=0 ;; esac
  local LC_ALL=C
  [ "${#s}" -le "$max" ] && { printf '%s' "$s"; return 0; }
  [ "$max" -ge 6 ] || { printf ''; return 0; }
  # `…` is 3 bytes in UTF-8.
  local body
  body="$(printf '%s' "$s" | jm_truncate_bytes $((max - 3)))"
  printf '%s…' "$body"
}

# ── row rendering ───────────────────────────────────────────────────────────

# _jm_render_prefix TS KIND WORKTREE RECURRENCE
# `MM-DD · KIND[ ×N][ · wt:NAME]`, ≤ JM_RENDER_PREFIX_MAX bytes.  Only the
# worktree component is ever shortened: the date and the kind are the two
# things that make a memory line safe to read as history rather than as an
# instruction, and neither may be lost to a budget.
_jm_render_prefix() {
  local ts="${1:-0}" kind="${2:-other}" wt="${3:-}" rec="${4:-1}"
  local date kindu head
  date="$(jm_short_date "$ts")"
  kindu="$(printf '%s' "$kind" | LC_ALL=C tr '[:lower:]' '[:upper:]' | LC_ALL=C tr -cd '[:upper:]_')"
  [ -n "$kindu" ] || kindu='OTHER'
  head="${date}${JM_RENDER_SEP}${kindu}"
  case "$rec" in '' | *[!0-9]*) rec=1 ;; esac
  [ "$rec" -gt 1 ] && head="$head ×$rec"

  [ -n "$wt" ] || { printf '%s' "$head"; return 0; }
  wt="$(printf '%s' "$wt" | LC_ALL=C tr -cd 'A-Za-z0-9._-')"
  [ -n "$wt" ] || { printf '%s' "$head"; return 0; }

  local full="${head}${JM_RENDER_SEP}wt:${wt}"
  local LC_ALL=C
  if [ "${#full}" -le "$JM_RENDER_PREFIX_MAX" ]; then
    printf '%s' "$full"
    return 0
  fi
  local room=$((JM_RENDER_PREFIX_MAX - ${#head} - 6))  # ' · ' (4) + 'wt:' (3) - 1 for '…'
  if [ "$room" -lt 6 ]; then
    printf '%s' "$head"
    return 0
  fi
  printf '%s%swt:%s' "$head" "$JM_RENDER_SEP" "$(jm_bcut "$wt" "$room")"
}

# jm_render_row TS KIND WORKTREE TITLE RECURRENCE CONT
# One or two display lines (SPEC §9.2).  The continuation line is omitted
# entirely when empty — a "next: (none)" line costs tokens and says nothing.
# READ-TIME STRUCTURAL DEFANG (both row renderers).  jm_sanitize applies the
# same rules at write time, but 20,216 rows were migrated in BEFORE that rule
# existed and no amount of write-time care reaches them.  The digest's only
# structural boundary is its own `</jhh3-mem>` closing tag, so a stored title
# carrying that string would close the "data, not instructions" envelope early
# and hand the remainder to the model as trusted top-level prose.  This is the
# guarantee that must not depend on the provenance of the row.
jm_render_row() {
  local ts="${1:-0}" kind="${2:-other}" wt="${3:-}" title="${4:-}" rec="${5:-1}" cont="${6:-}"
  local prefix flat
  prefix="$(_jm_render_prefix "$ts" "$kind" "$wt" "$rec")"
  flat="$(printf '%s' "$title" | jm_flatten | jm_defang)"
  [ -n "$flat" ] || flat='(untitled)'
  printf '%s%s%s%s\n' "$JM_RENDER_INDENT" "$prefix" "$JM_RENDER_SEP" \
    "$(jm_bcut "$flat" "$JM_RENDER_TITLE_MAX")"

  [ -n "$cont" ] || return 0
  cont="$(printf '%s' "$cont" | jm_flatten | jm_defang)"
  [ -n "$cont" ] || return 0
  # The primary line can never begin with a role marker because it always
  # begins with the date, but the continuation line begins with row content.
  # jm_defang already handled mid-line markers; this catches the start of the
  # line, where it has no preceding whitespace to anchor against.
  case "$cont" in
    [Ss]ystem:* | [Aa]ssistant:* | [Uu]ser:* | [Hh]uman:*) cont="~$cont" ;;
  esac
  printf '%s%s\n' "$JM_RENDER_CONT_INDENT" "$(jm_bcut "$cont" "$JM_RENDER_CONT_MAX")"
  return 0
}

# jm_render_file_row PATH TS KIND TITLE RECURRENCE
# The slot-2 shape: `  lib/flags/registry.ts — 08-11 · BUGFIX ×3 · <title>`.
# The anchor path leads because the path is why this row is being shown at all.
jm_render_file_row() {
  local p="${1:-}" ts="${2:-0}" kind="${3:-other}" title="${4:-}" rec="${5:-1}"
  local prefix flat pshort
  prefix="$(_jm_render_prefix "$ts" "$kind" '' "$rec")"
  flat="$(printf '%s' "$title" | jm_flatten | jm_defang)"
  [ -n "$flat" ] || flat='(untitled)'
  pshort="$(jm_bcut "$(printf '%s' "$p" | jm_flatten | jm_defang)" 60)"
  printf '%s%s — %s%s%s\n' "$JM_RENDER_INDENT" "$pshort" "$prefix" "$JM_RENDER_SEP" \
    "$(jm_bcut "$flat" "$JM_RENDER_TITLE_MAX")"
}

# ── the byte cap ────────────────────────────────────────────────────────────

# jm_render_cap MAX_BYTES MARKER_TEMPLATE [EXTRA_DROPPED]   (filter)
#
# stdin  : the assembled block
# stdout : ≤ MAX_BYTES bytes, cut on a line boundary, with MARKER_TEMPLATE
#          appended as the final line iff anything was dropped — by this cut or
#          by the caller (EXTRA_DROPPED, e.g. rows a slot budget already shed).
#
# MARKER_TEMPLATE may contain `{N}` (lines withheld) and `{MAX}` (the cap).
#
# The marker is sized BEFORE the cut, using the total line count as the upper
# bound on the withheld count, so the digits can only shrink when the real count
# is substituted.  That is what makes `output ≤ MAX` provable rather than
# hopeful.
jm_render_cap() {
  local max="${1:-0}" tmpl="${2:-}" extra="${3:-0}"
  case "$max"   in '' | *[!0-9]*) return 2 ;; esac
  case "$extra" in '' | *[!0-9]*) extra=0 ;; esac

  local data
  data="$(cat; printf x)"
  data="${data%x}"

  local total kept dropped marker head
  total="$(printf '%s' "$data" | LC_ALL=C awk 'END { print NR }')"
  case "$total" in '' | *[!0-9]*) total=0 ;; esac

  local LC_ALL=C

  # An empty template means "cut, do not annotate" — used for the per-slot
  # budgets, where the single authoritative marker is appended later by the
  # caller over the assembled block.  Appending an empty marker here would emit
  # a blank line, which is 1 wasted byte and 0 information.
  if [ -z "$tmpl" ]; then
    if [ "${#data}" -le "$max" ]; then
      printf '%s' "$data"
      return 0
    fi
    # `$( )` strips EVERY trailing newline, and jm_truncate_bytes signals "I cut
    # on a line boundary" precisely by ending with one.  Without the sentinel
    # the case below misreads a complete final line as a partial one and drops
    # it — one line short at every budget, and empty when exactly one line fits.
    head="$(printf '%s' "$data" | jm_truncate_bytes "$max"; printf x)"
    head="${head%x}"
    case "$head" in
      '' | *$'\n') : ;;
      *) case "$head" in
           *$'\n'*) head="${head%$'\n'*}"$'\n' ;;
           *) head='' ;;
         esac ;;
    esac
    printf '%s' "$head"
    return 0
  fi

  # Fits, and the caller dropped nothing: emit verbatim, no marker (INJ-5).
  if [ "${#data}" -le "$max" ] && [ "$extra" -eq 0 ]; then
    printf '%s' "$data"
    return 0
  fi

  # Upper bound on the withheld count fixes the marker's maximum width.
  marker="$(_jm_render_marker "$tmpl" $((total + extra)) "$max")"
  local reserve=$((${#marker} + 1))

  if [ "${#data}" -le "$max" ]; then
    # Nothing to cut, but the caller shed rows: append the marker if it fits.
    if [ $((${#data} + reserve)) -le "$max" ]; then
      marker="$(_jm_render_marker "$tmpl" "$extra" "$max")"
      printf '%s' "$data"
      case "$data" in *$'\n' | '') : ;; *) printf '\n' ;; esac
      printf '%s\n' "$marker"
    else
      printf '%s' "$data"
    fi
    return 0
  fi

  local budget=$((max - reserve))
  if [ "$budget" -le 0 ]; then
    # No room for even one line plus the marker: emit nothing rather than a
    # bare "we hid everything" banner.  Silence costs zero tokens.
    return 0
  fi

  # Sentinel as above: preserve the trailing newline that marks a clean cut.
  head="$(printf '%s' "$data" | jm_truncate_bytes "$budget"; printf x)"
  head="${head%x}"
  # jm_truncate_bytes falls back to a UTF-8 boundary when no newline fits in
  # the budget.  For a multi-line block that would end mid-row, so back off to
  # the last complete line (INJ-17).  If not one whole line fits, emit nothing.
  case "$head" in
    '' | *$'\n') : ;;
    *) case "$head" in
         *$'\n'*) head="${head%$'\n'*}"$'\n' ;;
         *) head='' ;;
       esac ;;
  esac
  if [ -z "$head" ]; then
    return 0
  fi
  kept="$(printf '%s' "$head" | LC_ALL=C awk 'END { print NR }')"
  case "$kept" in '' | *[!0-9]*) kept=0 ;; esac
  dropped=$((total - kept + extra))
  [ "$dropped" -gt 0 ] || dropped=1

  marker="$(_jm_render_marker "$tmpl" "$dropped" "$max")"
  printf '%s' "$head"
  case "$head" in *$'\n') : ;; *) printf '\n' ;; esac
  printf '%s\n' "$marker"
  return 0
}

# _jm_render_marker TEMPLATE N MAX — substitute {N} and {MAX}.  Internal.
_jm_render_marker() {
  local tpl="${1:-}" n="${2:-0}" m="${3:-0}"
  tpl="${tpl//\{N\}/$n}"
  tpl="${tpl//\{MAX\}/$m}"
  printf '%s' "$tpl"
}
