#!/usr/bin/env bash
# jhh3-mem — lib/enrich.sh
#
# Out-of-band LLM upgrade of deterministic rows: fills `learned`, `decided`, and
# `next_steps`, then flips capture_mode 'deterministic' -> 'enriched'.
#
# THIS FILE IS NEVER CALLED BY A HOOK. SPEC 1 (adjudication 1) settled that capture
# stays deterministic and enrichment happens out of band: SessionEnd hooks run in a
# shutdown path with a 1.5 s floor, and a model call there is not shippable. Run this
# from the CLI or a scheduler instead -- `jhh3-mem enrich`.
#
# It is also the ONLY file permitted to reach a model: INV-6 fails the build if any
# other shipped script names a credential or a network client. Nothing here reads or
# stores an API key -- the model is reached through whichever agent CLI is already
# installed and already authenticated, so this inherits the user's existing session
# rather than introducing a second credential path.
#
# Failure is per row and never fatal: a row whose call fails is marked
# 'enrich_failed' and skipped by later runs, so one bad transcript cannot wedge the
# queue and a partial run is always resumable.

JM_ENRICH_MAX_INPUT="${JM_ENRICH_MAX_INPUT:-6000}"   # bytes of row context sent per call
JM_ENRICH_WALL="${JM_ENRICH_WALL:-120}"              # per-row wall clock, seconds

# _jm_enrich_model_cmd — echo the argv prefix for a one-shot model call, or nothing.
#
# Deliberately ordered: an explicit override always wins, then whichever agent CLI is
# present. Both are one-shot, non-interactive, and already authenticated.
_jm_enrich_model_cmd() {
  if [ -n "${JHH3_MEM_ENRICH_CMD:-}" ]; then printf '%s\n' "$JHH3_MEM_ENRICH_CMD"; return 0; fi
  if jm_have claude; then printf 'claude -p\n'; return 0; fi
  if jm_have codex;  then printf 'codex exec\n'; return 0; fi
  return 1
}

# _jm_enrich_prompt TITLE REQUEST OUTCOME FILES COMMANDS — the per-row prompt.
#
# Asks for a strict JSON object so the reply can be parsed by sqlite3's json_extract
# rather than by regex over prose. Fields map 1:1 onto the columns being filled.
_jm_enrich_prompt() {
  cat <<PROMPT
You are distilling one past coding session into reusable knowledge for a future
session that has no memory of it.

The OUTCOME below is already stored and already searchable. Do NOT summarise it.
Your job is to capture what the outcome does NOT carry: the knowledge that would
save the next session from re-deriving something, and the decisions it should not
silently reverse.

Return ONLY a JSON object, no prose and no code fence, with exactly these keys:
  "learned"    - concrete, transferable facts: root causes, gotchas, constraints
                 discovered, "X does not work because Y", version-specific
                 behaviour, where the real problem turned out to live.
  "decided"    - choices made AND the reason, especially ones a future session
                 might undo without knowing why they were made.
  "next_steps" - work explicitly left undone.

Each value is one or two plain sentences, specific enough to act on. Prefer a
concrete detail ("the 8s timeout drops batches over ~90 inputs") over a general
one ("there were timeout issues"). Name the file, symbol, flag or number involved.

Return "" for a field ONLY when the session truly contains nothing of that kind.
A session that debugged, chose between options, or hit a surprise almost always
has a non-empty "learned" -- if you are about to return all three empty, look
again at the OUTCOME for the surprise or the cause that is stated there.

TITLE: ${1:-}
REQUEST: ${2:-}
OUTCOME: ${3:-}
FILES TOUCHED: ${4:-}
COMMANDS: ${5:-}
PROMPT
}

# _jm_enrich_json_only (filter) — emit the outermost {...} and nothing else.
# `claude -p` writes to stdout, and so does anything else in that process: an MCP
# server warning ("Client.listTools() called but server does not advertise tools
# capability") lands on the same stream as the reply. Requiring the WHOLE file to
# be valid JSON turned that into a row marked enrich_failed with no explanation.
_jm_enrich_json_only() {
  awk 'BEGIN{ RS="\0" }
       { s=$0
         i=index(s,"{"); if(i==0) exit 1
         j=0; for(k=length(s);k>0;k--) if(substr(s,k,1)=="}"){ j=k; break }
         if(j==0) exit 1
         printf "%s", substr(s,i,j-i+1) }'
}

# jm_enrich_run SELECTOR LIMIT DRY_RUN MODEL
#   SELECTOR : 'pending' (default) | 'all' | a repo_key
#   LIMIT    : max rows this run
#   DRY_RUN  : 1 to select and print without calling a model or writing
#   MODEL    : optional model hint appended to the CLI invocation
# stdout: one progress line per row. rc 0 ok · 1 partial · 2 bad args · 3 no model CLI
jm_enrich_run() {
  local selector="${1:-pending}" limit="${2:-20}" dry="${3:-0}" model="${4:-}"
  case "$limit" in '' | *[!0-9]*) return 2 ;; esac
  jm_require_sqlite || return 3

  local cmd=''
  if [ "$dry" != '1' ]; then
    cmd="$(_jm_enrich_model_cmd)" || {
      printf 'jhh3-mem: no model CLI found (looked for claude, codex).\n' >&2
      printf '          Set JHH3_MEM_ENRICH_CMD to override.\n' >&2
      jm_log enrich degraded 'reason=no_model_cli'
      return 3
    }
    [ -n "$model" ] && cmd="$cmd --model $model"
  fi

  # 'pending' is the resumable default: rows already enriched, already failed, or
  # never deterministic to begin with are all excluded, so re-running is a no-op
  # rather than a re-spend.
  local where
  case "$selector" in
    all)     where="capture_mode IN ('deterministic','enrich_failed')" ;;
    pending) where="capture_mode = 'deterministic'" ;;
    *)       where="capture_mode = 'deterministic' AND repo_key = '$(jm_sql_quote "$selector" | sed "s/^'//;s/'$//")'" ;;
  esac

  local ids
  ids="$(jm_db_read "SELECT id FROM entries WHERE $where AND superseded = 0 ORDER BY ts DESC LIMIT $limit;")" || return 1
  [ -n "$ids" ] || { printf 'nothing to enrich (selector=%s)\n' "$selector"; return 0; }

  local total=0 ok=0 failed=0 empty=0 id row title request outcome files cmds reply
  local stage; stage="$(jm_mkstage)" || return 1
  # shellcheck disable=SC2064  # expand $stage now, not at trap time
  trap "rm -rf -- '$stage' 2>/dev/null" RETURN

  while IFS= read -r id; do
    [ -n "$id" ] || continue
    case "$id" in *[!0-9]*) continue ;; esac
    total=$((total + 1))

    # One query per field, deliberately, replacing a single char(31)-joined SELECT
    # read back with `read -r title request outcome files cmds`. That was wrong twice
    # over, and silently:
    #
    #   * `read` stops at the first NEWLINE. `outcome` is multi-paragraph prose, so
    #     everything from its first line break onward -- and every field after it --
    #     was dropped. Every prompt this ever sent contained a title and a request
    #     and no session content, so the model correctly answered "nothing to add"
    #     and enrichment looked like it ran while distilling a heading.
    #   * `a || b` in SQL is NULL when EITHER side is NULL. The schema marks all six
    #     columns NOT NULL, so that could not actually fire today -- coalesce here
    #     keeps it that way if a migration ever relaxes one (ENR-10 guards the
    #     schema fact directly).
    #
    # Five small reads cost nothing next to the model call that follows them, and
    # neither failure mode can come back.
    title="$(jm_db_read   "SELECT coalesce(title,'')          FROM entries WHERE id = $id;")" \
      || { failed=$((failed+1)); continue; }
    request="$(jm_db_read "SELECT coalesce(request,'')        FROM entries WHERE id = $id;")" || request=''
    outcome="$(jm_db_read "SELECT coalesce(outcome,'')        FROM entries WHERE id = $id;")" || outcome=''
    files="$(jm_db_read   "SELECT coalesce(files_touched,'')  FROM entries WHERE id = $id;")" || files=''
    cmds="$(jm_db_read    "SELECT coalesce(commands,'')       FROM entries WHERE id = $id;")" || cmds=''

    if [ "$dry" = '1' ]; then
      printf '  [dry] %-6s %s\n' "$id" "$(printf '%s' "$title" | cut -c1-64)"
      ok=$((ok + 1))
      continue
    fi

    _jm_enrich_prompt "$title" "$request" "$outcome" "$files" "$cmds" \
      | head -c "$JM_ENRICH_MAX_INPUT" > "$stage/prompt.txt"

    # The model's reply is untrusted text on its way into a store that gets injected
    # into future sessions: redact it exactly like captured content, then defang it,
    # before it can reach a row.
    # shellcheck disable=SC2086  # $cmd is an argv PREFIX ("claude -p", "codex exec")
    # and must word-split; quoting it would look for a binary with a space in it.
    # See the guard in scripts/session-end.sh: without this, the call below is
    # captured as a session and the row it writes comes straight back here on the
    # next run -- an hourly loop that pays for a model call and an embedding to
    # record the fact that it made a model call.
    if ! JHH3_MEM_NO_CAPTURE=1 timeout -k 5 "$JM_ENRICH_WALL" $cmd < "$stage/prompt.txt" > "$stage/raw.txt" 2>"$stage/err.txt"; then
      printf '%s\n' "UPDATE entries SET capture_mode='enrich_failed' WHERE id=$id;" | jm_db_write >/dev/null 2>&1
      printf '  FAIL  %-6s %s\n' "$id" "$(head -c 60 "$stage/err.txt" 2>/dev/null | tr -d '\n')"
      failed=$((failed + 1))
      continue
    fi
    # jm_redact_file reports its redaction count on stdout; capture it so the
    # per-row progress line stays the only thing this loop prints.
    local _nred=0
    _nred="$(jm_redact_file "$stage/raw.txt" "$stage/red.txt" 2>/dev/null)" \
      || cp -- "$stage/raw.txt" "$stage/red.txt"

    local learned decided steps parsed
    _jm_enrich_json_only < "$stage/red.txt" > "$stage/obj.json" 2>/dev/null || : > "$stage/obj.json"
    parsed="$(jm_db_read "
      WITH r AS (SELECT readfile('$(jm_sql_quote "$stage/obj.json" | sed "s/^'//;s/'$//")') AS j)
      SELECT CASE WHEN json_valid(j)=1
        THEN coalesce(json_extract(j,'\$.learned'),'') || char(31) ||
             coalesce(json_extract(j,'\$.decided'),'') || char(31) ||
             coalesce(json_extract(j,'\$.next_steps'),'')
        ELSE '' END FROM r;" 2>/dev/null)"
    if [ -z "$parsed" ]; then
      printf '%s\n' "UPDATE entries SET capture_mode='enrich_failed' WHERE id=$id;" | jm_db_write >/dev/null 2>&1
      printf '  FAIL  %-6s reply was not the requested JSON object\n' "$id"
      failed=$((failed + 1))
      continue
    fi
    IFS=$'\x1f' read -r learned decided steps <<<"$parsed"

    # An all-empty reply is valid JSON, so the emptiness check above passes: the
    # joined string always contains its two separators. That is how three rows got
    # marked `enriched` holding nothing -- and, because `enriched` is terminal, they
    # would never be looked at again. Distinguish "the model had nothing to say"
    # from "the model said something", and never record the first as success.
    if [ -n "${JM_ENRICH_DEBUG:-}" ]; then
      cp -- "$stage/prompt.txt" "$JM_ENRICH_DEBUG/prompt-$id.txt" 2>/dev/null
      cp -- "$stage/raw.txt"    "$JM_ENRICH_DEBUG/raw-$id.txt"    2>/dev/null
    fi
    if [ -z "$learned$decided$steps" ]; then
      # hive: was enrich_empty — violates capture_mode CHECK (see dossier §8.1)
      printf '%s\n' "UPDATE entries SET capture_mode='enrich_failed' WHERE id=$id;" | jm_db_write >/dev/null 2>&1
      printf '  EMPTY %-6s model returned no content for any field\n' "$id"
      empty=$((empty + 1))
      continue
    fi

    if jm_db_param_write \
        "UPDATE entries SET learned=:l, decided=:d, next_steps=:n, capture_mode='enriched' WHERE id=$id;" \
        l "$(printf '%s' "$learned" | jm_defang)" \
        d "$(printf '%s' "$decided" | jm_defang)" \
        n "$(printf '%s' "$steps"   | jm_defang)"; then
      printf '  ok    %-6s %s\n' "$id" "$(printf '%s' "$title" | cut -c1-64)"
      ok=$((ok + 1))
    else
      printf '  FAIL  %-6s write rejected\n' "$id"
      failed=$((failed + 1))
    fi
  done <<<"$ids"

  jm_log enrich ok "total=$total ok=$ok empty=${empty:-0} failed=$failed dry=$dry"
  printf 'enriched %s of %s (%s empty, %s failed)\n' "$ok" "$total" "${empty:-0}" "$failed"
  [ "$failed" -gt 0 ] && return 1
  return 0
}
