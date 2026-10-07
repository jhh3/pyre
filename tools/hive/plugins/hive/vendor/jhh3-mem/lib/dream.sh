#!/usr/bin/env bash
# jhh3-mem — lib/dream.sh
#
# Between-session consolidation. Runs from the CLI or a scheduler, never from a hook,
# and holds no state of its own: it starts, works over the store, and exits. That is
# the shape the sleep-time-compute literature converged on, and the opposite of the
# resident daemon this plugin exists to replace.
#
# It is CROSS-HOST BY CONSTRUCTION. Every step below reads rows, not transcripts, so
# a single pass consolidates Claude Code and Codex sessions together. Claude Code's
# own Auto Dream cannot do that -- it only ever sees Claude Code -- and Codex ships
# no equivalent. That is the one capability here neither host provides natively.
#
# WHAT IT DOES NOT DO. It never deletes an entry. SPEC 4.4 keeps entries indefinitely
# on purpose: a record that silently expires cannot be told apart from one that was
# never written. `forget` is the only reclamation path and it is explicit.
# It also does not derive semantic rules from repeated episodes -- consolidation here
# is mechanical (reconcile, recompute, compact, report), not inferential. Promoting
# episodes into reusable lessons is the obvious v2 and wants weeks of cross-host data
# before anyone guesses at its heuristics.

# jm_dream_run DRY_RUN VERBOSE
# stdout: a per-step report. rc 0 ok · 1 a step failed · 3 no sqlite3
jm_dream_run() {
  local dry="${1:-0}" verbose="${2:-0}"
  jm_require_sqlite || return 3
  local rc=0 n

  printf 'jhh3-mem dream%s\n\n' "$([ "$dry" = '1' ] && printf ' (dry run)')"

  # ---- 1. supersede reconciliation ---------------------------------------------
  # (uid, source_bytes) is unique, so a session captured more than once -- /clear,
  # resume, backfill, a manual `capture` -- leaves several rows per uid. Exactly the
  # largest transcript should survive as current. Capture maintains this already;
  # dream re-asserts it, because a crash between the insert and the supersede update
  # leaves the pair inconsistent and nothing else would ever notice.
  n="$(jm_db_read "
    SELECT count(*) FROM entries e
    WHERE e.superseded = 0
      AND EXISTS (SELECT 1 FROM entries o
                   WHERE o.uid = e.uid AND o.source_bytes > e.source_bytes);" 2>/dev/null)"
  printf '  %-34s %s\n' 'stale current rows' "${n:-0}"
  if [ "${n:-0}" -gt 0 ] && [ "$dry" != '1' ]; then
    printf '%s\n' "
      UPDATE entries SET superseded = 1
      WHERE superseded = 0
        AND EXISTS (SELECT 1 FROM entries o
                     WHERE o.uid = entries.uid AND o.source_bytes > entries.source_bytes);" | jm_db_write \
      >/dev/null || rc=1
  fi

  # ---- 2. durability recompute after enrichment ---------------------------------
  # durability is written at capture time from what was known then. A row that has
  # since been enriched carries real learned/decided prose, which is exactly the
  # signal the ranking is supposed to reward -- but nothing recomputed it, so an
  # enriched row still ranked as the bare deterministic row it used to be.
  #
  # A FLOOR, not a multiplier. dream is expected to run on a schedule, so any step
  # that scales a value would compound a little further on every pass; `max(x,1.30)`
  # reaches its fixed point after one run and the next pass reports nothing to do.
  n="$(jm_db_read "
    SELECT count(*) FROM entries
    WHERE capture_mode = 'enriched'
      AND (coalesce(learned,'') <> '' OR coalesce(decided,'') <> '')
      AND durability < 1.30;" 2>/dev/null)"
  printf '  %-34s %s\n' 'enriched rows under-ranked' "${n:-0}"
  if [ "${n:-0}" -gt 0 ] && [ "$dry" != '1' ]; then
    printf '%s\n' "
      UPDATE entries SET durability = max(durability, 1.30)
      WHERE capture_mode = 'enriched'
        AND (coalesce(learned,'') <> '' OR coalesce(decided,'') <> '')
        AND durability < 1.30;" | jm_db_write >/dev/null || rc=1
  fi

  # ---- 3. compaction ------------------------------------------------------------
  # FTS5 merges its b-tree segments on demand; doing it here means the next search
  # pays for none of it. Free, and the only step that touches the index.
  if [ "$dry" != '1' ]; then
    printf '%s\n' "INSERT INTO entries_fts(entries_fts) VALUES('optimize');" | jm_db_write >/dev/null 2>&1 || true
    printf '%s\n' "PRAGMA optimize;" | jm_db_write >/dev/null 2>&1 || true
  fi
  printf '  %-34s %s\n' 'fts index optimized' "$([ "$dry" = '1' ] && printf 'skipped' || printf 'yes')"

  # ---- 4. the cross-host report --------------------------------------------------
  printf '\n  store\n'
  local report
  report="$(jm_db_read "
    SELECT '    ' || host || '  ' || count(*) || ' entries, ' ||
           sum(capture_mode = 'deterministic') || ' awaiting enrich, ' ||
           sum(superseded) || ' superseded'
    FROM entries GROUP BY host ORDER BY host;" 2>/dev/null)"
  [ -n "$report" ] && printf '%s\n' "$report" || printf '    (empty)\n'

  if [ "$verbose" = '1' ]; then
    printf '\n  recurring topics (dedup_key seen 3+ times, any host)\n'
    jm_db_read "
      SELECT '    ' || count(*) || 'x  ' || substr(max(title),1,58)
      FROM entries WHERE superseded = 0 AND coalesce(dedup_key,'') <> ''
      GROUP BY dedup_key HAVING count(*) >= 3
      ORDER BY count(*) DESC LIMIT 10;" 2>/dev/null
  fi

  local pending
  pending="$(jm_db_read "SELECT count(*) FROM entries WHERE capture_mode = 'deterministic';" 2>/dev/null)"
  if [ "${pending:-0}" -gt 0 ]; then
    printf '\n  %s row(s) awaiting enrichment — run: jhh3-mem enrich\n' "$pending"
  fi

  jm_log dream ok "dry=$dry rc=$rc"
  return "$rc"
}
