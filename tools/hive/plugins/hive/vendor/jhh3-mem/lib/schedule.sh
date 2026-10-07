#!/usr/bin/env bash
# jhh3-mem — lib/schedule.sh
#
# Installs the nightly consolidation job. This is the one place the plugin arranges
# for something to happen while it is not running, so it is also the one place that
# could quietly break the promise `uninstall --purge` makes.
#
# It does not break it: JM_CRON_TAG below is registered in jm_paths_manifest, the
# crontab lines are tagged with it, and `uninstall` strips them. "One command removes
# everything it ever created" keeps meaning everything -- including this.
#
# WHY CRON AND NOT A SYSTEMD TIMER. A user timer is the nicer mechanism and its
# Persistent=true would catch up runs missed while the machine was off, which suits a
# nightly job on a laptop. It also needs `loginctl enable-linger`, and on this host
# Linger=no -- so a user timer simply would not fire outside a login session, which on
# WSL is most of the time. cron is running (PID 217 at time of writing) and needs no
# such grant. If linger is ever enabled, --systemd switches to a timer.
#
# Missing a night is harmless by construction: `dream` is idempotent and `enrich` is
# resumable, so a skipped run costs nothing and the next one catches up.


# ── launcher shim ───────────────────────────────────────────────────────────
#
# The plugin lives at .../cache/<marketplace>/jhh3-mem/<version>/, and hosts DELETE
# the old version directory on upgrade (verified: plugins/jhh3 has shipped 0.1.0
# through 0.5.0 and only 0.5.0 is on disk). So any absolute path baked into a
# crontab line dies at the next update -- silently, because cron output goes to
# /dev/null.
#
# The shim is a stable indirection: ~/.local/bin/jhh3-mem resolves the newest
# installed copy at run time. cron points here, which survives upgrades, and it also
# puts the CLI on the user's own login PATH -- the plugin's bin/ is only added to the
# *agent's* PATH, so without this the command exists inside Claude Code and nowhere
# else.
#
# Like the crontab lines, it is registered in the paths inventory and removed by
# `uninstall --purge`. Nothing this plugin creates outside its own directory is
# allowed to be invisible to its own removal.
JM_SHIM="${JM_SHIM:-$HOME/.local/bin/jhh3-mem}"

# jm_shim_install — write the launcher. rc 0 ok · 1 could not write.
jm_shim_install() {
  local dir; dir="$(dirname -- "$JM_SHIM")"
  mkdir -p -- "$dir" 2>/dev/null || return 1
  cat > "$JM_SHIM" <<'SHIM' || return 1
#!/usr/bin/env bash
# jhh3-mem launcher — installed by `jhh3-mem setup`; removed by `uninstall --purge`.
# Resolves the newest installed plugin copy so this path survives plugin upgrades.
set -u
best=''
for c in "$HOME"/.claude/plugins/cache/*/jhh3-mem/*/bin/jhh3-mem          "$HOME"/.codex/plugins/cache/*/jhh3-mem/*/bin/jhh3-mem; do
  [ -x "$c" ] || continue
  if [ -z "$best" ]; then best="$c"; continue; fi
  # Newest by version directory, so a Claude and a Codex install cannot disagree.
  newer="$(printf '%s\n%s\n' "$(basename "$(dirname "$(dirname "$best")")")"                                "$(basename "$(dirname "$(dirname "$c")")")" | sort -V | tail -1)"
  [ "$newer" = "$(basename "$(dirname "$(dirname "$c")")")" ] && best="$c"
done
[ -n "$best" ] || { printf 'jhh3-mem: no installed plugin found under ~/.claude or ~/.codex\n' >&2; exit 3; }
exec "$best" "$@"
SHIM
  chmod 0755 "$JM_SHIM" 2>/dev/null || return 1
  return 0
}


# jm_shim_on_path — rc 0 when the shim's directory is on the CALLER's PATH.
#
# Checked and reported, because installing a launcher into a directory the user's
# shell never searches produces a command that exists and cannot be run -- which
# looks exactly like a broken install and is the more confusing failure.
jm_shim_on_path() {
  local dir; dir="$(dirname -- "$JM_SHIM")"
  case ":${PATH}:" in *":$dir:"*) return 0 ;; esac
  return 1
}

# jm_shim_remove — rc 0 always; absent is success.
jm_shim_remove() {
  [ -e "$JM_SHIM" ] || return 0
  # Only remove something we recognise as ours.
  grep -q 'jhh3-mem launcher' "$JM_SHIM" 2>/dev/null && rm -f -- "$JM_SHIM" 2>/dev/null
  return 0
}

JM_CRON_TAG='# jhh3-mem-schedule'
JM_SCHED_HOUR="${JM_SCHED_HOUR:-3}"

# The embed stage inside `catchup` and `nightly` needs OPENAI_API_KEY, and cron
# provides no environment -- so without help the scheduled embed runs, finds no
# key, and skips, every hour, forever. Enrich survives (the agent CLI's auth is
# file-based); the semantic lane silently decays. The fix: when this file exists
# and mentions the key, the cron lines SOURCE it at run time. The key itself is
# never written into the crontab -- the line only names the file the user already
# keeps it in, which preserves "the key is read from the environment and never
# stored" (SPEC 20).
JM_SCHED_ENV_FILE="${JHH3_MEM_SCHED_ENV_FILE:-$HOME/.env.private}"

# jm_schedule_status — echo 'cron', 'systemd', or 'none'.
jm_schedule_status() {
  if jm_have crontab && crontab -l 2>/dev/null | grep -qF "$JM_CRON_TAG"; then
    printf 'cron\n'; return 0
  fi
  if jm_have systemctl && systemctl --user list-unit-files jhh3-mem.timer >/dev/null 2>&1 \
     && systemctl --user is-enabled jhh3-mem.timer >/dev/null 2>&1; then
    printf 'systemd\n'; return 0
  fi
  printf 'none\n'
}

# _jm_sched_bin — absolute path to our own CLI, for a crontab line that has no $PATH.
_jm_sched_bin() {
  local d
  d="$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)" || return 1
  printf '%s/bin/jhh3-mem\n' "$d"
}

# jm_schedule_install DRY_RUN WITH_ENRICH
# rc 0 installed (or already present) · 1 failed · 3 no scheduler available
jm_schedule_install() {
  local dry="${1:-0}" with_enrich="${2:-0}" bin line cur
  # Point cron at the SHIM, never at $0: $0 is inside a versioned plugin directory
  # that the host deletes on upgrade, and a broken cron line writing to /dev/null is
  # a job that silently stops running.
  jm_shim_install || { printf 'jhh3-mem: could not install the launcher at %s\n' "$JM_SHIM" >&2; return 1; }
  bin="$JM_SHIM"

  jm_have crontab || { printf 'jhh3-mem: crontab is not installed; nothing to schedule with\n' >&2; return 3; }

  # A single line, absolute paths only: cron gives almost no environment, and the
  # store location must be explicit or the job would bootstrap a second empty store
  # under a different HOME. Output is discarded; the plugin's own log is the record.
  # ONE line, running `nightly`, which does enrich -> dream -> embed in order.
  #
  # This replaced a design where only `dream` was scheduled and `enrich` was opt-in
  # behind a flag. That default was wrong for what this plugin is FOR: the store
  # improving between sessions is the entire feature, and shipping it switched off
  # meant a machine could run for weeks accumulating raw rows that were never
  # distilled and, after 0.4.0, never vectorised -- so semantic search silently
  # decayed as the newest sessions stayed unsearchable by meaning. Opt-out, not
  # opt-in; `--no-enrich` is still there for a machine with no model access.
  #
  # Sequencing them inside one command also removes the old guess: two cron lines
  # an hour apart assumed dream finished within the hour, which is not a property
  # anyone was checking.
  # TWO lines, different cadences, because the work has two different shapes:
  #   hourly  catchup  enrich + embed -- incremental, no-op when idle. A session
  #                    finished at 10am is distilled and semantically searchable by
  #                    11am, not tomorrow. This is the line that makes the store
  #                    feel like it improves on its own.
  #   nightly nightly  the same two plus `dream`, which reconciles and compacts the
  #                    FTS index -- real work that has no business running hourly.
  local verb='nightly' hourly='catchup'
  if [ "$with_enrich" = '0' ]; then verb='nightly --no-enrich'; hourly=''; fi

  # See JM_SCHED_ENV_FILE above: with a key file, both commands run under a shell
  # that sources it, so the embed stage actually has a key under cron. Without one,
  # the lines are installed bare and embed skips -- announced below, not silent.
  local envfile=''
  if [ -r "$JM_SCHED_ENV_FILE" ] && grep -q 'OPENAI_API_KEY' "$JM_SCHED_ENV_FILE" 2>/dev/null; then
    envfile="$JM_SCHED_ENV_FILE"
  fi
  _jm_cron_cmd() {  # $1 = verb + args
    if [ -n "$envfile" ]; then
      printf "/bin/sh -c '. \"%s\" 2>/dev/null; \"%s\" %s'" "$envfile" "$bin" "$1"
    else
      printf '"%s" %s' "$bin" "$1"
    fi
  }
  # cron's default PATH is /usr/bin:/bin, and the model CLIs enrich dispatches to
  # (claude, codex) live in the shim's directory (~/.local/bin) — without this the
  # enrich stage fails "no model CLI found" on every scheduled run, to /dev/null.
  # Same defect class as the key: a scheduled stage that can only fail under cron.
  local cronpath; cronpath="$(dirname -- "$bin"):/usr/local/bin:/usr/bin:/bin"
  line="3 $JM_SCHED_HOUR * * * PATH=\"$cronpath\" JHH3_MEM_HOME=\"$JM_HOME\" $(_jm_cron_cmd "$verb") >/dev/null 2>&1 $JM_CRON_TAG"
  [ -n "$hourly" ] && line="$line"$'\n'"17 * * * * PATH=\"$cronpath\" JHH3_MEM_HOME=\"$JM_HOME\" $(_jm_cron_cmd "$hourly") >/dev/null 2>&1 $JM_CRON_TAG"

  if [ "$dry" = '1' ]; then
    printf 'would install:\n%s\n' "$line"
    return 0
  fi

  # Idempotent: drop any line we previously wrote, then append the current one, so
  # re-running after an upgrade rewrites rather than duplicates.
  cur="$(crontab -l 2>/dev/null | grep -vF "$JM_CRON_TAG" || true)"
  { [ -n "$cur" ] && printf '%s\n' "$cur"; printf '%s\n' "$line"; } | crontab - 2>/dev/null || {
    printf 'jhh3-mem: crontab refused the update\n' >&2; return 1; }

  # cron fields are MINUTE HOUR: writing them the other way round put the two jobs a
  # minute apart while this line claimed an hour, so enrich would have started before
  # dream finished on a large store.
  if [ "$with_enrich" = '0' ]; then
    printf 'scheduled: nightly at %02d:03 (dream + embed)' "$JM_SCHED_HOUR"
  else
    printf 'scheduled: catchup hourly at :17 (enrich + embed), full pass at %02d:03' "$JM_SCHED_HOUR"
  fi
  if [ -n "$envfile" ]; then
    printf ' — sourcing %s for the key\n' "$envfile"
  else
    printf '\n  %-22s ^ no OPENAI_API_KEY in %s: the embed stage will SKIP under cron\n' '' "$JM_SCHED_ENV_FILE"
    printf '  %-22s   (enrich and dream still run; pass --env-file to fix)\n' ''
  fi
  local envflag=0; [ -n "$envfile" ] && envflag=1
  jm_log schedule ok "mechanism=cron enrich=$with_enrich envfile=$envflag"
  return 0
}

# jm_schedule_remove — strip our lines. rc 0 always (absent is success).
jm_schedule_remove() {
  jm_have crontab || return 0
  crontab -l 2>/dev/null | grep -qF "$JM_CRON_TAG" || return 0
  local cur
  cur="$(crontab -l 2>/dev/null | grep -vF "$JM_CRON_TAG" || true)"
  if [ -n "$cur" ]; then printf '%s\n' "$cur" | crontab - 2>/dev/null
  else crontab -r 2>/dev/null; fi
  jm_log schedule ok 'removed'
  return 0
}
