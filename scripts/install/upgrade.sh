#!/bin/bash
# Upgrade (or roll back) a per-user Pyre installation without stopping runs.
#
#   upgrade.sh --from DIR | --tarball FILE [--sums SHA256SUMS] [--sig SHA256SUMS.sig] [--version V]
#              [--allowed-signers FILE] [--allow-unsigned]
#              [--service auto|node|hub|none] [--state DIR]
#              [--hub-protocol MIN:MAX | --skip-hub-check] [--health-timeout SECS] [--allow-run-loss] [--no-prune]
#              [--root DIR] [--home DIR] [--os darwin|linux] [--no-service-manager] [--dry-run]
#   upgrade.sh --rollback [--to VERSION] [--service ...] [same common flags]
#
# Release signature (before anything is staged): the release SHA256SUMS must
# carry a valid `ssh-keygen -Y` signature (SHA256SUMS.sig, namespace
# pyre-release) from a key in the pinned allowed-signers file
# ~/.config/pyre/release-allowed-signers. The first verified
# --allowed-signers FILE (packaging/release-allowed-signers from a trusted
# checkout) is pinned there. Once a key is pinned (or --allowed-signers is
# given), an unsigned release, a tampered SHA256SUMS or a signature by another
# key is refused (exit 3) unless --allow-unsigned (unsigned only; a bad
# signature is always refused). Without a pinned key or --allowed-signers the
# signature cannot be checked: the release is accepted with a warning
# (migration; pin the key to close this).
#
# Sequence: stage the new version beside the current one (install.sh
# --no-activate: checksums, immutable version dir, `version --json` health
# check) -> check that the new pyred's protocol range overlaps the hub's ->
# record the running daemon's live runs -> switch `current` -> restart the
# service (pyred exits; its outbox and run handles are durable in pyred.db and
# the runs keep living in their tmux servers) -> wait until the new daemon
# reports the new version, is connected to the hub (if enrolled) and has
# recovered at least as many live runs -> on any failure switch `current`
# back, restart the previous daemon and verify it.
#
# After a successful upgrade, versions other than current and .previous are
# pruned and left as aliases "<version> -> current" (lib.sh prune_versions),
# so absolute hook paths of older sessions keep working; --no-prune keeps
# them. A pruned version is not a rollback target (reinstall it instead).
#
# Exit codes: 0 ok, 1 error, 2 usage, 3 refused by install.sh,
# 4 protocol incompatible (nothing switched), 5 failed and rolled back,
# 6 failed and rollback also failed (operator action needed).
#
# PYRE_UPGRADE_RESTART_HOOK=/path/to/exe (testing/manual supervision): called
# as `<hook> restart node|hub` instead of launchctl/systemctl.
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
# shellcheck source=lib.sh
. "$HERE/lib.sh"

usage() { sed -n '2,40p' "$0" | sed 's/^# \{0,1\}//'; exit 2; }
SRC_ARGS=()
COMMON=()
SERVICE=auto HUB_PROTO="" SKIP_HUB=0 TIMEOUT=45 ROLLBACK=0 TO="" ALLOW_RUN_LOSS=0 PRUNE=1
SRC_TARBALL="" SRC_FROM="" SRC_SUMS="" SIG="" ALLOWED_ARG="" ALLOW_UNSIGNED=0
RELEASE_NAMESPACE=pyre-release
while [ $# -gt 0 ]; do
  case "$1" in --*=*) set -- "${1%%=*}" "${1#*=}" "${@:2}" ;; esac # --flag=value
  parse_common_flag "$@"
  if [ "$SHIFT_N" -gt 0 ]; then
    COMMON+=("${@:1:$SHIFT_N}")
    shift "$SHIFT_N"; continue
  fi
  case "$1" in
    --tarball) SRC_TARBALL=${2:?}; shift 2 ;;
    --from) SRC_FROM=${2:?}; SRC_ARGS+=("$1" "$2"); shift 2 ;;
    --sums) SRC_SUMS=${2:?}; shift 2 ;;
    --version) SRC_ARGS+=("$1" "${2:?}"); shift 2 ;;
    --sig) SIG=${2:?}; shift 2 ;;
    --allowed-signers) ALLOWED_ARG=${2:?}; shift 2 ;;
    --allow-unsigned) ALLOW_UNSIGNED=1; shift ;;
    --state) NODE_STATE=${2:?}; COMMON+=("$1" "$2"); shift 2 ;;
    --service) SERVICE=${2:?}; shift 2 ;;
    --hub-protocol) HUB_PROTO=${2:?}; shift 2 ;;
    --skip-hub-check) SKIP_HUB=1; shift ;;
    --allow-run-loss) ALLOW_RUN_LOSS=1; shift ;;
    --no-prune) PRUNE=0; shift ;;
    --health-timeout) TIMEOUT=${2:?}; shift 2 ;;
    --rollback) ROLLBACK=1; shift ;;
    --to) TO=${2:?}; shift 2 ;;
    -h|--help) usage ;;
    *) echo "unknown argument: $1" >&2; usage ;;
  esac
done
setup_paths
# install.sh only accepts --state for node installs; keep it out of COMMON for staging.
STAGE_COMMON=()
i=0
while [ $i -lt ${#COMMON[@]} ] && [ ${#COMMON[@]} -gt 0 ]; do
  if [ "${COMMON[$i]}" = --state ]; then i=$((i + 2)); continue; fi
  STAGE_COMMON+=("${COMMON[$i]}"); i=$((i + 1))
done

OLD=$(current_version || true)
[ -n "$OLD" ] || die "no current Pyre installation under $PREFIX; use install.sh first"

# ---- which services restart ----
SERVICES=""
case "$SERVICE" in
  auto)
    if service_installed hub; then SERVICES="hub"; fi
    if service_installed node; then SERVICES="$SERVICES node"; fi ;;
  node|hub) SERVICES=$SERVICE ;;
  none) SERVICES="" ;;
  *) echo "--service must be auto, node, hub or none" >&2; exit 2 ;;
esac
SERVICES=${SERVICES# }
VERIFY=0
if [ -n "$SERVICES" ] && [ "$DRY" = 0 ] && { [ "$NO_SM" = 0 ] || [ -n "${PYRE_UPGRADE_RESTART_HOOK:-}" ]; } && can_exec; then VERIFY=1; fi

# ---- maintenance window, sentinel restart ----
# Sentinels watching this machine read ~/.local/state/pyre/maintenance.json
# over ssh and stay silent while "until" is in the future (docs/operations.md,
# "Sentinel"). Set only once services are really about to restart (not for a
# refused or dry-run upgrade). Best effort: a failure here never stops the
# upgrade, and a longer window already set (pyred maintenance --for 1h) is
# kept.
set_maintenance_window() {
  local f until now cur
  f=$(r "$T_HOME/.local/state/pyre/maintenance.json")
  until=$(date -u -d '+10 minutes' +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -v+10M +%Y-%m-%dT%H:%M:%SZ 2>/dev/null) || return 0
  now=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  cur=""
  [ ! -f "$f" ] || cur=$(sed -n 's/.*"until":"\([^"]*\)".*/\1/p' "$f" 2>/dev/null | head -n 1) || cur=""
  if [ -n "$cur" ] && [ "$(printf '%s\n%s\n' "$cur" "$until" | LC_ALL=C sort | tail -n 1)" = "$cur" ]; then return 0; fi
  mkdir -p "$(dirname "$f")" 2>/dev/null || return 0
  if printf '{"until":"%s","reason":"upgrade.sh from %s","set_at":"%s"}\n' "$until" "$OLD" "$now" 2>/dev/null >"$f.tmp.$$"; then
    mv -f "$f.tmp.$$" "$f" 2>/dev/null && MAINT_UNTIL=$until && note "maintenance window until $until (sentinels stay silent)"
  fi
  rm -f "$f.tmp.$$" 2>/dev/null
  return 0
}
MAINT_UNTIL=""
maybe_maintenance_window() { if [ -n "$SERVICES" ] && [ "$DRY" = 0 ]; then set_maintenance_window || true; fi; }
# clear_maintenance_window: after a healthy restart, end the window this
# run set (a window someone else set or lengthened since is left alone).
# Sentinels then count their grace periods from the window's end, so a
# service still settling does not alert at once.
clear_maintenance_window() {
  local f cur
  [ -n "$MAINT_UNTIL" ] || return 0
  f=$(r "$T_HOME/.local/state/pyre/maintenance.json")
  cur=$(sed -n 's/.*"until":"\([^"]*\)".*/\1/p' "$f" 2>/dev/null | head -n 1) || cur=""
  if [ "$cur" = "$MAINT_UNTIL" ] && rm -f "$f" 2>/dev/null; then note "maintenance window cleared"; fi
  MAINT_UNTIL=""
  return 0
}

# restart_sentinel: a running pyre-sentinel (scripts/install/sentinel.sh)
# runs the pyred shim (Linux) or Pyre.app's pyred (macOS), so after `current`
# moves it restarts onto the new version. Best effort; nothing when it is not
# installed. A macOS agent written before Pyre.app ran the shim, whose Local
# Network permission macOS keys to the per-version path (a new prompt after
# every upgrade): it is repointed at Pyre.app, which keeps one grant.
# Never in a test or staged install: a restart hook, --root, --home or a
# foreign --os means the real service is not ours to touch.
restart_sentinel() {
  [ "$DRY" = 0 ] && [ "$NO_SM" = 0 ] || return 0
  [ -z "${PYRE_UPGRADE_RESTART_HOOK:-}" ] && [ -z "$ROOT" ] && [ "$T_HOME" = "$HOME" ] && [ "$T_OS" = "$HOST_OS" ] || return 0
  if [ "$HOST_OS" = darwin ]; then
    local label=dev.jhh3.pyre.sentinel plist="$HOME/Library/LaunchAgents/dev.jhh3.pyre.sentinel.plist"
    local app="$PREFIX/Pyre.app/Contents/MacOS/pyred"
    launchctl print "gui/$(id -u)/$label" >/dev/null 2>&1 || return 0
    if [ -x "$app" ] && grep -q "<string>$HOME/.local/bin/pyred</string>" "$plist" 2>/dev/null; then
      sed "s|<string>$HOME/.local/bin/pyred</string>|<string>$app</string>|" "$plist" >"$plist.tmp.$$" &&
        mv -f "$plist.tmp.$$" "$plist" && note "sentinel now runs $app (one Local Network grant across upgrades)"
      rm -f "${plist:?}.tmp.$$"
      sm launchctl bootout "gui/$(id -u)/$label" || true
      # bootout returns before the job is gone (launchd allows it 20 s after
      # SIGTERM); bootstrap fails with EIO until then.
      local i=0
      while [ "$i" -lt 60 ] && launchctl print "gui/$(id -u)/$label" >/dev/null 2>&1; do sleep 0.5; i=$((i + 1)); done
      for i in 1 2 3; do
        sm launchctl bootstrap "gui/$(id -u)" "$plist" && return 0
        sleep 2
      done
      warn "the sentinel did not reload: run scripts/install/sentinel.sh install"
      return 0
    fi
    sm launchctl kickstart -k "gui/$(id -u)/$label" || true
  else
    systemctl --user is-enabled --quiet pyre-sentinel.service 2>/dev/null || return 0
    sm systemctl --user try-restart pyre-sentinel.service || true
  fi
  return 0
}

node_cred_file() {
  if [ -f "$(r "$NODE_STATE/node-credential.json")" ]; then echo "$(r "$NODE_STATE/node-credential.json")"
  elif [ -f "$(r "$PREFIX/node-credential.json")" ]; then echo "$(r "$PREFIX/node-credential.json")"; fi
}

# hub_hello BIN: `pyre hello --json` through the client config, else through
# the node credential (token passed in the child's environment only).
hub_hello() {
  local pyre=$1/pyre out cred
  if out=$("$pyre" hello --json 2>/dev/null); then echo "$out"; return 0; fi
  cred=$(node_cred_file)
  [ -n "$cred" ] || return 1
  PYRE_HUB=$(json_str hub_url <"$cred") PYRE_HUB_PIN=$(json_str hub_cert_sha256 <"$cred") \
    PYRE_TOKEN=$(json_str token <"$cred") "$pyre" hello --json 2>/dev/null
}

# wait_healthy SVC VERSION BEFORE_RUNS: 0 healthy; 2 daemon healthy but fewer
# live runs than before; 1 unhealthy.
# handed_off_from V WANT: the node runs V because the installed pyred WANT
# handed off to an in-product managed pyred V (pyre update --tool pyred,
# activated over WANT). Only a rollback to that launcher can match.
handed_off_from() {
  local h
  [ -n "${NODE_STATE:-}" ] || return 1
  h="$(r "$NODE_STATE/tools/pyred/handoff.json")"
  [ -f "$h" ] || return 1
  [ "$(json_str version <"$h")" = "$1" ] && [ "$(json_str base <"$h")" = "$2" ]
}

wait_healthy() {
  local svc=$1 want=$2 before=$3 deadline st v link runs need_link=0 hj short=0
  deadline=$(( $(date +%s) + TIMEOUT ))
  [ -z "$(node_cred_file)" ] || need_link=1
  while [ "$(date +%s)" -lt "$deadline" ]; do
    if [ "$svc" = node ]; then
      if st=$(node_status_json); then
        v=$(printf '%s' "$st" | json_str pyred)
        link=$(printf '%s' "$st" | json_str link)
        runs=$(printf '%s' "$st" | json_num live_runs)
        if { [ "$v" = "$want" ] || handed_off_from "$v" "$want"; } && { [ "$need_link" = 0 ] || [ "$link" = connected ]; }; then
          if [ "${runs:-0}" -ge "$before" ]; then
            note "health: pyred $v, hub link $link, live runs $runs (before: $before)"
            return 0
          fi
          short=1
        fi
      fi
    else
      if hj=$(hub_hello "$(r "$PREFIX/current/bin")"); then
        v=$(printf '%s' "$hj" | json_str hub_version)
        if [ "$v" = "$want" ]; then note "health: hub answers hello as $v"; return 0; fi
      fi
    fi
    sleep 1
  done
  if [ "$short" = 1 ]; then
    if [ "$ALLOW_RUN_LOSS" = 1 ]; then
      warn "pyred $v is healthy but reports ${runs:-0} live runs (before: $before); accepted (--allow-run-loss). Check: pyre sessions"
      return 0
    fi
    echo "pyred $v is connected but reports ${runs:-0} live runs after ${TIMEOUT}s (before: $before): runs were not recovered" >&2
    return 2
  fi
  echo "health check failed for $svc after ${TIMEOUT}s (version='${v:-?}' link='${link:-?}' runs='${runs:-?}', want version $want, runs >= $before)" >&2
  return 1
}

live_runs_now() {
  local st
  if st=$(node_status_json); then printf '%s' "$st" | json_num live_runs; else echo 0; fi
}

restart_all() { # VERSION BEFORE_RUNS -> 0 ok, 2 runs missing, 1 unhealthy
  local svc rc=0
  for svc in $SERVICES; do
    if [ "$svc" = node ]; then
      note "restarting pyred: it exits, runs stay in their tmux servers; the new daemon recovers each run handle without relaunching and resumes the report outbox"
    fi
    service_restart "$svc" || { echo "restart of $svc failed" >&2; return 1; }
    if [ "$VERIFY" = 1 ]; then
      wait_healthy "$svc" "$1" "$2" || rc=$?
      [ "$rc" = 0 ] || return "$rc"
    fi
  done
  return 0
}

switch_to() { # VERSION
  local from
  from=$(current_version || true)
  ! is_alias "$1" || die "$1 was pruned ($PREFIX/$1 is an alias of current); reinstall it with upgrade.sh --tarball/--from"
  [ -d "$(r "$PREFIX/$1")" ] || die "$PREFIX/$1 is not installed"
  if [ "$DRY" = 0 ] && [ -n "$from" ] && [ "$from" != "$1" ]; then echo "$from" >"$(r "$PREFIX/.previous")"; fi
  swap_symlink "$1" "$(r "$PREFIX/current")"
  note "current -> $1 (was ${from:-unset})"
  [ "$HOST_OS" != darwin ] || sync_app
}

# ---- rollback mode ----
if [ "$ROLLBACK" = 1 ]; then
  if [ -z "$TO" ]; then
    [ -f "$(r "$PREFIX/.previous")" ] || die "no previous version recorded; pass --to VERSION"
    TO=$(cat "$(r "$PREFIX/.previous")")
  fi
  [ "$TO" != "$OLD" ] || die "current is already $TO"
  BEFORE=$(live_runs_now)
  maybe_maintenance_window
  switch_to "$TO"
  rc=0; restart_all "$TO" "$BEFORE" || rc=$?
  restart_sentinel
  if [ "$rc" = 0 ]; then clear_maintenance_window; note "rolled back $OLD -> $TO"; exit 0; fi
  if [ "$rc" = 2 ]; then echo "rolled back to $TO, but not every run was recovered: check pyre sessions" >&2; exit 5; fi
  echo "rollback to $TO did not become healthy; switching back to $OLD" >&2
  switch_to "$OLD"
  rc=0; restart_all "$OLD" 0 || rc=$?
  restart_sentinel
  [ "$rc" = 0 ] || { echo "CRITICAL: neither $TO nor $OLD became healthy; inspect logs (docs/operations.md, troubleshooting)" >&2; exit 6; }
  exit 5
fi

# ---- release signature ----
[ -n "$SRC_TARBALL" ] || [ -n "$SRC_FROM" ] || die "--from DIR or --tarball FILE is required (or --rollback)"
[ -z "$SRC_TARBALL" ] || [ -z "$SRC_FROM" ] || die "--from and --tarball are exclusive"
PIN=$(r "$CONFIG_DIR/release-allowed-signers")
VERIFY_TMP=$(mktemp -d "${TMPDIR:-/tmp}/pyre-upgrade-verify.XXXXXX")
LOG="$VERIFY_TMP/install.log"
trap 'rm -rf "$VERIFY_TMP"' EXIT

refuse() { echo "error: $*; nothing staged, current unchanged ($OLD)" >&2; exit 3; }

# release_keys FILE: the "pyre-release" keys of an allowed-signers file ("type base64", sorted).
release_keys() { awk '$1 == "pyre-release" {for (i = 2; i <= NF; i++) if ($i ~ /^ssh-/) {print $i, $(i + 1); break}}' "$1" | sort -u; }

TRUST="" TRUST_FROM=""
if [ -n "$ALLOWED_ARG" ]; then
  [ -f "$ALLOWED_ARG" ] || die "--allowed-signers $ALLOWED_ARG not found"
  [ -n "$(release_keys "$ALLOWED_ARG")" ] || die "$ALLOWED_ARG has no pyre-release key"
  if [ -f "$PIN" ] && [ "$(release_keys "$PIN")" != "$(release_keys "$ALLOWED_ARG")" ]; then
    refuse "--allowed-signers $ALLOWED_ARG differs from the pinned release key $PIN (to rotate the key, replace $PIN deliberately)"
  fi
  TRUST=$ALLOWED_ARG TRUST_FROM="--allowed-signers"
elif [ -f "$PIN" ]; then
  TRUST=$PIN TRUST_FROM="pinned"
fi

if [ -n "$SRC_TARBALL" ]; then
  [ -f "$SRC_TARBALL" ] || die "tarball $SRC_TARBALL not found"
  [ -n "$SRC_SUMS" ] || SRC_SUMS="$(dirname "$SRC_TARBALL")/SHA256SUMS"
  [ -n "$SIG" ] || SIG="$SRC_SUMS.sig"
  if [ -f "$SIG" ] && [ -z "$TRUST" ]; then
    # Not worse than an unsigned release (migration), but say so loudly.
    warn "the release is signed ($SIG) but no release key is pinned, so the signature was NOT verified; pin the key with --allowed-signers <packaging/release-allowed-signers from a trusted checkout>"
    # install.sh would refuse a signature it cannot check: hand it a copy of
    # SHA256SUMS without the signature and the decision made here.
    SRC_ARGS+=(--tarball "$SRC_TARBALL" --allow-unsigned)
    if [ -f "$SRC_SUMS" ]; then
      mkdir -p "$VERIFY_TMP/unverified"
      cp "$SRC_SUMS" "$VERIFY_TMP/unverified/SHA256SUMS"
      SRC_ARGS+=(--sums "$VERIFY_TMP/unverified/SHA256SUMS")
    fi
  elif [ -f "$SIG" ]; then
    [ -f "$SRC_SUMS" ] || refuse "$SRC_SUMS not found next to the signature"
    command -v ssh-keygen >/dev/null 2>&1 || refuse "ssh-keygen (OpenSSH >= 8.1) is required to verify the release signature"
    # Verify private copies and stage from exactly the verified SHA256SUMS.
    cp "$SRC_SUMS" "$VERIFY_TMP/SHA256SUMS"
    cp "$SIG" "$VERIFY_TMP/SHA256SUMS.sig"
    if ! ssh-keygen -Y verify -f "$TRUST" -I "$RELEASE_NAMESPACE" -n "$RELEASE_NAMESPACE" \
         -s "$VERIFY_TMP/SHA256SUMS.sig" <"$VERIFY_TMP/SHA256SUMS" >"$VERIFY_TMP/verify.out" 2>&1; then
      sed 's/^/    /' "$VERIFY_TMP/verify.out" >&2
      refuse "release signature verification FAILED for $SRC_SUMS ($TRUST_FROM key $TRUST): the checksums were modified or signed by another key"
    fi
    note "release signature verified: $(head -n 1 "$VERIFY_TMP/verify.out") ($TRUST_FROM)"
    # install.sh re-verifies the same private copies against the same key.
    SRC_ARGS+=(--tarball "$SRC_TARBALL" --sums "$VERIFY_TMP/SHA256SUMS" --sig "$VERIFY_TMP/SHA256SUMS.sig" --allowed-signers "$TRUST")
    if [ "$TRUST_FROM" = "--allowed-signers" ] && [ ! -f "$PIN" ]; then
      if [ "$DRY" = 1 ]; then
        echo "[dry-run] pin release key: install -m 0644 $ALLOWED_ARG $PIN"
      else
        mkdir -p "$(dirname "$PIN")"
        chmod 700 "$(dirname "$PIN")" 2>/dev/null || true
        install -m 0644 "$ALLOWED_ARG" "$PIN"
        note "pinned the release key in $PIN; later upgrades require releases signed by it"
      fi
    fi
  else
    if [ -n "$TRUST" ]; then
      [ "$ALLOW_UNSIGNED" = 1 ] || refuse "the release is not signed (no $SIG) but a release key is trusted ($TRUST_FROM $TRUST); pass --allow-unsigned only for a build you made yourself"
      warn "installing an UNSIGNED release although a release key is pinned (--allow-unsigned)"
    else
      warn "unsigned release and no pinned release key: provenance rests on SHA256SUMS alone (pin a key with --allowed-signers)"
    fi
    SRC_ARGS+=(--tarball "$SRC_TARBALL" --allow-unsigned)
    [ -z "$SRC_SUMS" ] || [ ! -f "$SRC_SUMS" ] || SRC_ARGS+=(--sums "$SRC_SUMS")
  fi
else
  # --from DIR: an extracted directory has no release signature.
  if [ -n "$TRUST" ]; then
    [ "$ALLOW_UNSIGNED" = 1 ] || refuse "--from $SRC_FROM cannot be signature-checked and a release key is trusted ($TRUST_FROM); upgrade from the signed --tarball, or pass --allow-unsigned"
    warn "staging an unverified directory although a release key is pinned (--allow-unsigned)"
    SRC_ARGS+=(--allow-unsigned)
  fi
  [ -z "$SRC_SUMS" ] || SRC_ARGS+=(--sums "$SRC_SUMS")
fi

# ---- stage ----
note "staging (current $OLD)"
set +e
"$HERE/install.sh" "${SRC_ARGS[@]}" "${STAGE_COMMON[@]+"${STAGE_COMMON[@]}"}" --no-activate --service none 2>&1 | tee "$LOG"
rc=${PIPESTATUS[0]}
set -e
[ "$rc" = 0 ] || { echo "staging failed (install.sh exit $rc); current unchanged ($OLD)" >&2; exit "$rc"; }
NEW=$(sed -n 's/^==> Pyre \([^ ]*\) for .*/\1/p' "$LOG" | head -n 1)
[ -n "$NEW" ] || die "could not read the staged version"
if [ "$NEW" = "$OLD" ]; then note "already at $NEW"; exit 0; fi
NEWBIN=$(r "$PREFIX/$NEW/bin")

# ---- protocol compatibility ----
if can_exec && [ "$DRY" = 0 ]; then
  vj=$("$NEWBIN/pyred" version --json) || die "staged pyred $NEW fails version --json; current unchanged"
  NMAX=$(printf '%s' "$vj" | json_num protocol)
  NMIN=$(printf '%s' "$vj" | json_num min_protocol)
  NMIN=${NMIN:-$NMAX}
  HMIN="" HMAX=""
  if [ -n "$HUB_PROTO" ]; then
    HMIN=${HUB_PROTO%%:*}; HMAX=${HUB_PROTO##*:}
    src="--hub-protocol"
  elif [ "$SKIP_HUB" = 0 ]; then
    # Ask with the installed (current) pyre first: a staged pyre on another
    # protocol is refused by the hub's hello, which is exactly the case to
    # detect. The staged pyre is the fallback (e.g. no client config yet).
    if hj=$(hub_hello "$(r "$PREFIX/current/bin")") || hj=$(hub_hello "$NEWBIN"); then
      HMAX=$(printf '%s' "$hj" | json_num protocol)
      HMIN=$(printf '%s' "$hj" | json_num min_protocol)
      HMIN=${HMIN:-$HMAX}
      src="hub hello ($(printf '%s' "$hj" | json_str hub_version))"
    else
      die "cannot reach the hub to check protocol compatibility; current unchanged ($OLD). Pass --hub-protocol MIN:MAX or --skip-hub-check"
    fi
  fi
  if [ -n "$HMAX" ]; then
    if [ "$NMIN" -le "$HMAX" ] && [ "$HMIN" -le "$NMAX" ]; then
      note "protocol: pyred $NEW speaks $NMIN..$NMAX; hub supports $HMIN..$HMAX ($src): compatible"
    else
      if [ "$NMIN" -gt "$HMAX" ]; then act_msg="upgrade the hub to a build that supports protocol $NMIN first, or keep $OLD"; else act_msg="stage a pyred that supports protocol $HMIN or newer, or roll the hub back"; fi
      echo "protocol mismatch: pyred $NEW speaks $NMIN..$NMAX; hub supports $HMIN..$HMAX ($src). $act_msg. Not switched; $NEW stays staged, current is still $OLD." >&2
      exit 4
    fi
  else
    warn "hub protocol check skipped (--skip-hub-check)"
  fi
else
  note "protocol check skipped (dry run or foreign platform)"
fi

# ---- switch, restart, verify ----
BEFORE=0
if printf '%s\n' $SERVICES | grep -qx node; then
  BEFORE=$(live_runs_now)
  note "running daemon has $BEFORE live runs"
fi
PREV_BEFORE=""
[ ! -f "$(r "$PREFIX/.previous")" ] || PREV_BEFORE=$(cat "$(r "$PREFIX/.previous")")
maybe_maintenance_window
switch_to "$NEW"
if [ -z "$SERVICES" ]; then
  restart_sentinel
  note "no Pyre service to restart; new processes use $NEW"
  [ "$PRUNE" = 0 ] || prune_versions
  exit 0
fi
rc=0; restart_all "$NEW" "$BEFORE" || rc=$?
restart_sentinel
if [ "$rc" = 0 ]; then
  [ "$VERIFY" = 1 ] || note "restart not verified (service manager not invoked or dry run)"
  clear_maintenance_window
  note "upgraded $OLD -> $NEW (rollback: upgrade.sh --rollback)"
  [ "$PRUNE" = 0 ] || prune_versions
  exit 0
fi
echo "upgrade to $NEW failed its health check; rolling back to $OLD" >&2
[ "$DRY" = 1 ] || : >"$(r "$PREFIX/$NEW/.failed")"
switch_to "$OLD"
restart_sentinel
# The failed version must not become the rollback target.
if [ "$DRY" = 0 ]; then
  if [ -n "$PREV_BEFORE" ]; then echo "$PREV_BEFORE" >"$(r "$PREFIX/.previous")"; else rm -f "$(r "$PREFIX/.previous")"; fi
fi
rc=0; restart_all "$OLD" "$BEFORE" || rc=$?
if [ "$rc" = 0 ]; then
  echo "rolled back to $OLD; $NEW is marked failed ($PREFIX/$NEW/.failed)" >&2
  exit 5
fi
if [ "$rc" = 2 ]; then
  echo "rolled back to $OLD (healthy and connected), but runs were not recovered by either version: check pyre sessions" >&2
  exit 5
fi
echo "CRITICAL: rollback to $OLD did not become healthy either; inspect logs (docs/operations.md, troubleshooting)" >&2
exit 6
