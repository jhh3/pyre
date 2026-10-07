#!/bin/bash
# Remove a per-user Pyre installation: stop/disable Pyre-managed services,
# remove their unit files, the shims and the version directories (and the
# bundled tools of install.sh --with-tools, with their shims).
#
#   uninstall.sh [--stop-runs] [--purge [--include-profiles] [--yes]]
#                [--state DIR] [--hub-data DIR]
#                [--root DIR] [--home DIR] [--os darwin|linux]
#                [--no-service-manager] [--dry-run]
#
# Data is kept unless --purge: node state (database, outbox, run records,
# logs, node credential), hub data (database, artifacts, TLS key, owner
# credential), the client config ~/.config/pyre/client.json and launchd logs.
# --purge prints exactly what it would delete and deletes only with --yes.
# Account profiles (~/.local/state/pyre/profiles, or <state>/profiles on
# older installs, or $PYRE_PROFILE_ROOT: fresh harness logins) and the
# shared/ transcript store beside them (session histories the profiles link
# to) are kept even with --purge unless --include-profiles. On macOS --purge
# also deletes the CLI's own state (~/.local/state/pyre/{cache,drafts}),
# which lies outside the node state directory there. ~/.config/pyre is never
# deleted as a directory: it may hold the Android signing key.
#
# Stopping pyred does not stop runs. --stop-runs kills only Pyre-owned tmux
# servers (<state>/tmux/pyre.sock and legacy <state>/runs/*/tmux.sock); the user's own tmux
# server is never touched.
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
# shellcheck source=lib.sh
. "$HERE/lib.sh"

usage() { sed -n '2,26p' "$0" | sed 's/^# \{0,1\}//'; exit 2; }
STOP_RUNS=0 PURGE=0 PROFILES=0 YES=0
while [ $# -gt 0 ]; do
  parse_common_flag "$@"
  if [ "$SHIFT_N" -gt 0 ]; then shift "$SHIFT_N"; continue; fi
  case "$1" in
    --stop-runs) STOP_RUNS=1; shift ;;
    --purge) PURGE=1; shift ;;
    --include-profiles) PROFILES=1; shift ;;
    --yes) YES=1; shift ;;
    --state) NODE_STATE=${2:?}; shift 2 ;;
    --hub-data) HUB_DATA=${2:?}; shift 2 ;;
    -h|--help) usage ;;
    *) echo "unknown argument: $1" >&2; usage ;;
  esac
done
setup_paths

# Pyre-owned tmux sockets of runs that may still be alive.
run_sockets() {
  local s
  for s in "$(r "$NODE_STATE")"/tmux/pyre.sock "$(r "$NODE_STATE")"/runs/*/tmux.sock; do
    [ -S "$s" ] && echo "$s"
  done
  return 0
}

live_sockets=""
if command -v tmux >/dev/null 2>&1; then
  while IFS= read -r s; do
    [ -n "$s" ] || continue
    if tmux -S "$s" list-sessions >/dev/null 2>&1; then live_sockets="$live_sockets
$s"; fi
  done <<EOF
$(run_sockets)
EOF
fi

if [ "$PURGE" = 1 ]; then
  # Build the exact deletion list before changing anything.
  LIST=""
  add() { if [ -e "$1" ] || [ -L "$1" ]; then LIST="$LIST
$1"; fi; }
  # pyred's default profile root (decision I08) may lie outside the state
  # dir; the profiles' shared transcript store is "shared" beside it.
  PROFILE_ROOT=${PYRE_PROFILE_ROOT:-$T_HOME/.local/state/pyre/profiles}
  PROFILE_ROOT=${PROFILE_ROOT%/}
  SHARED_ROOT=$(dirname "$PROFILE_ROOT")/shared
  if [ -d "$(r "$NODE_STATE")" ]; then
    for e in "$(r "$NODE_STATE")"/* "$(r "$NODE_STATE")"/.[!.]*; do
      [ -e "$e" ] || continue
      if [ "$PROFILES" = 0 ]; then
        case "$(basename "$e")" in profiles|shared) continue ;; esac
      fi
      add "$e"
    done
  fi
  add "$(r "$HUB_DATA")"
  add "$(r "$CLIENT_CONFIG")"
  add "$(r "$PREFIX/node-credential.json")"
  [ "$T_OS" != darwin ] || add "$(r "$LOG_DIR")"
  # The CLI's own state (pyre artifact get cache, document drafts) is
  # ~/.local/state/pyre on every OS: inside the node state dir on Linux,
  # outside it on macOS.
  CLI_STATE=$T_HOME/.local/state/pyre
  if [ "$T_HOME" = "$HOME" ] && [ -n "${XDG_STATE_HOME:-}" ]; then CLI_STATE=$XDG_STATE_HOME/pyre; fi
  if [ "$CLI_STATE" != "$NODE_STATE" ]; then
    add "$(r "$CLI_STATE/cache")"
    add "$(r "$CLI_STATE/drafts")"
  fi
  KEPT=""
  for d in "$NODE_STATE/profiles" "$NODE_STATE/shared" "$PROFILE_ROOT" "$SHARED_ROOT"; do
    [ -d "$(r "$d")" ] || continue
    case "$KEPT
" in *"
$d
"*) continue ;; esac
    case "$d" in
      "$NODE_STATE"/*) [ "$PROFILES" = 1 ] && continue ;; # deleted by the loop above
      *) if [ "$PROFILES" = 1 ]; then add "$(r "$d")"; continue; fi ;;
    esac
    KEPT="$KEPT
$d"
  done
  echo "--purge will delete:"
  if [ -n "$LIST" ]; then printf '%s\n' "$LIST" | sed '/^$/d; s/^/    /'; else echo "    (nothing found)"; fi
  printf '%s\n' "$KEPT" | sed '/^$/d' | while IFS= read -r d; do
    case "$d" in
      "$SHARED_ROOT"|"$NODE_STATE/shared") echo "kept: $d (transcripts the account profiles link to; add --include-profiles to delete)" ;;
      *) echo "kept: $d (account logins; add --include-profiles to delete)" ;;
    esac
  done
  echo "never deleted: other files in $CONFIG_DIR (e.g. the Android signing key), Hive/Outfit/Rota data, ~/.claude, ~/.codex, tmux"
  if [ -n "$live_sockets" ] && [ "$STOP_RUNS" = 0 ]; then
    echo "refusing: Pyre runs are still alive (their state would be orphaned):"
    printf '%s\n' "$live_sockets" | sed '/^$/d; s/^/    /'
    echo "stop them with 'pyre stop <session>' or re-run with --stop-runs"
    exit 3
  fi
  if [ "$YES" = 0 ]; then
    echo "nothing deleted; re-run with --purge --yes to delete the paths above"
    exit 1
  fi
fi

# ---- services ----
for svc in node hub; do
  u=$(r "$(unit_for "$svc")")
  if [ -e "$u" ]; then
    if is_managed "$u"; then
      note "stopping and disabling the $svc service"
      service_unload "$svc"
      act rm -f "$u"
      note "removed $(unit_for "$svc")"
      if [ "$svc" = node ] && [ "$STOP_RUNS" = 0 ]; then note "pyred stopped; runs keep running in their own tmux servers (use --stop-runs to stop them)"; fi
    else
      warn "$(unit_for "$svc") is not Pyre-managed; left alone"
    fi
  fi
done
if [ -n "$UNIT_SLICE" ] && [ -e "$(r "$UNIT_SLICE")" ]; then
  if is_managed "$(r "$UNIT_SLICE")"; then
    # Removing the slice file does not stop running scopes; they keep the
    # slice loaded until they exit (or --stop-runs below).
    act rm -f "$(r "$UNIT_SLICE")"
    sm systemctl --user daemon-reload
    note "removed $UNIT_SLICE"
  else
    warn "$UNIT_SLICE is not Pyre-managed; left alone"
  fi
fi

if [ "$STOP_RUNS" = 1 ] && [ -n "$live_sockets" ]; then
  printf '%s\n' "$live_sockets" | sed '/^$/d' | while IFS= read -r s; do
    note "stopping Pyre-owned tmux server $s"
    act tmux -S "$s" kill-server || true
  done
fi

# ---- shims ----
for b in $PYRE_BINS; do
  s=$(r "$SHIM_DIR/$b")
  if [ -L "$s" ]; then
    case "$(readlink "$s")" in
      *share/pyre/current/bin/$b) act rm -f "$s"; note "removed shim $SHIM_DIR/$b" ;;
      *) warn "$SHIM_DIR/$b is not a Pyre shim; left alone" ;;
    esac
  fi
done
for t in hive outfit rota; do # bundled tools (install.sh --with-tools); other installs are left alone
  s=$(r "$SHIM_DIR/$t")
  if [ -L "$s" ] && [ "$(readlink "$s")" = "../share/pyre/tools/$t/bin/$t" ]; then act rm -f "$s"; note "removed shim $SHIM_DIR/$t"; fi
done

# ---- version directories ----
P=$(r "$PREFIX")
if [ -d "$P" ]; then
  if [ ! -f "$P/.pyre-install" ] && [ ! -f "$P/bin/pyred" ]; then
    warn "$PREFIX has no .pyre-install marker; not touching it"
  else
    for e in "$P"/*; do
      [ -e "$e" ] || [ -L "$e" ] || continue
      n=$(basename "$e")
      if [ -L "$e" ] && [ "$(readlink "$e")" = current ]; then # a pruned version's alias
        act rm -f "$e"
        continue
      fi
      case "$n" in
        node-credential.json) continue ;;
        current) act rm -f "$e" ;;
        tools)
          if [ -f "$e/.pyre-tools" ]; then act rm -rf "$e"; note "removed $PREFIX/tools (bundled tools)"; else warn "$PREFIX/tools has no .pyre-tools marker; left alone"; fi ;;
        bin)
          # left by `pyre enroll-node --bootstrap`: only pyred and its temp file
          if [ -z "$(find "$e" -mindepth 1 ! -name pyred ! -name .pyred.new)" ]; then act rm -rf "$e"; else warn "$PREFIX/bin has non-Pyre files; left alone"; fi ;;
        *)
          if [ -f "$e/VERSION" ] || [ -x "$e/bin/pyred" ]; then act rm -rf "$e"; note "removed $PREFIX/$n"; else warn "$PREFIX/$n is not a Pyre version directory; left alone"; fi ;;
      esac
    done
    act rm -f "$P/.previous" "$P/.pyre-install"
    for e in "$P"/.staging-*; do [ -e "$e" ] && act rm -rf "$e"; done
    if [ "$DRY" = 0 ]; then rmdir "$P" 2>/dev/null && note "removed $PREFIX" || true; fi
  fi
fi

# ---- PATH block added by install.sh --edit-path ----
for rc in "$(r "$T_HOME/.zshrc")" "$(r "$T_HOME/.bashrc")"; do
  if [ -f "$rc" ] && grep -q '# >>> pyre PATH >>>' "$rc"; then
    if [ "$DRY" = 1 ]; then echo "[dry-run] remove the pyre PATH block from $rc"; else
      sed '/# >>> pyre PATH >>>/,/# <<< pyre PATH <<</d' "$rc" >"$rc.pyre-tmp" && cat "$rc.pyre-tmp" >"$rc" && rm -f "$rc.pyre-tmp"
      note "removed the pyre PATH block from $rc"
    fi
  fi
done

# ---- purge ----
if [ "$PURGE" = 1 ]; then
  printf '%s\n' "$LIST" | sed '/^$/d' | while IFS= read -r p; do
    act rm -rf "$p"
    echo "deleted $p"
  done
  for d in "$NODE_STATE" "${CLI_STATE:-}"; do
    if [ -n "$d" ] && [ -d "$(r "$d")" ] && [ -z "$(ls -A "$(r "$d")")" ]; then act rmdir "$(r "$d")"; fi
  done
else
  note "kept data: $NODE_STATE, $HUB_DATA, $CLIENT_CONFIG (use --purge to delete)"
fi
note "uninstalled"
