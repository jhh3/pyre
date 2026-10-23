#!/bin/bash
# Install, remove or inspect the Pyre sentinel service on this machine
# (docs/operations.md "Sentinel").
#
#   sentinel.sh install [--config FILE] [--skip-check]
#   sentinel.sh uninstall
#   sentinel.sh status
#
# install: refuses unless the config exists and is 0600, runs one read-only
# probe round (`pyred sentinel run --dry-run`: no alert, no recovery, no
# state written), then installs and starts pyre-sentinel.service (Linux,
# systemd user unit) or dev.jhh3.pyre.sentinel (macOS LaunchAgent). Uses the
# pyred on ~/.local/bin (install.sh's shim); the macOS LaunchAgent runs the
# pyred inside ~/.local/share/pyre/Pyre.app instead, so its Local Network
# permission (granted to Pyre.app, not to a per-version path) survives
# upgrades. Never touches pyred.service.
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
PKG=$(cd "$HERE/../../packaging/sentinel" && pwd)
PYRED="$HOME/.local/bin/pyred"
APP_PYRED="$HOME/.local/share/pyre/Pyre.app/Contents/MacOS/pyred"
CONFIG="$HOME/.config/pyre/sentinel.json"
LABEL=dev.jhh3.pyre.sentinel
CHECK=1

die() { echo "error: $*" >&2; exit 1; }
note() { echo "==> $*"; }

cmd=${1:-}
[ $# -gt 0 ] && shift
while [ $# -gt 0 ]; do
  case "$1" in
    --config) CONFIG=${2:?}; shift 2 ;;
    --skip-check) CHECK=0; shift ;;
    *) die "unknown argument: $1" ;;
  esac
done

mode_of() { stat -c %a "$1" 2>/dev/null || stat -f %Lp "$1"; }

case "$(uname -s)" in
  Linux) OS=linux UNIT="$HOME/.config/systemd/user/pyre-sentinel.service" ;;
  Darwin) OS=darwin UNIT="$HOME/Library/LaunchAgents/$LABEL.plist" ;;
  *) die "unsupported OS (Linux, macOS)" ;;
esac

case "$cmd" in
  install)
    [ -x "$PYRED" ] || die "$PYRED not found: install Pyre first (install.sh)"
    { "$PYRED" sentinel 2>&1 || true; } | grep -q 'run|status|test-alert' || die "$PYRED has no sentinel command: upgrade Pyre first"
    [ -f "$CONFIG" ] || die "no config at $CONFIG (start from packaging/sentinel/sentinel.example.json)"
    case "$(mode_of "$CONFIG")" in 600|400) ;; *) die "$CONFIG must be mode 0600" ;; esac
    if [ "$CHECK" = 1 ]; then
      note "dry-run probe round (read-only)"
      "$PYRED" sentinel run --dry-run --config "$CONFIG"
    fi
    if [ "$OS" = linux ]; then
      mkdir -p "$(dirname "$UNIT")"
      install -m 0644 "$PKG/pyre-sentinel.service" "$UNIT"
      if [ "$CONFIG" != "$HOME/.config/pyre/sentinel.json" ]; then
        sed -i "s|^ExecStart=.*|ExecStart=%h/.local/bin/pyred sentinel run --config $CONFIG|" "$UNIT"
      fi
      systemctl --user daemon-reload
      systemctl --user enable --now pyre-sentinel.service
      systemctl --user --no-pager status pyre-sentinel.service | head -n 5 || true
    else
      mkdir -p "$(dirname "$UNIT")" "$HOME/Library/Logs/pyre"
      tmp=$(mktemp)
      prog=$PYRED
      if [ -x "$APP_PYRED" ]; then prog=$APP_PYRED; else echo "warning: no $APP_PYRED (Pyre predates it): macOS will ask for Local Network access again after each upgrade until you upgrade Pyre" >&2; fi
      sed -e "s|@PYRED@|$prog|g" -e "s|@HOME@|$HOME|g" "$PKG/$LABEL.plist.in" >"$tmp"
      if [ "$CONFIG" != "$HOME/.config/pyre/sentinel.json" ]; then
        sed -i '' "s|<string>run</string>|<string>run</string>\\
    <string>--config</string>\\
    <string>$CONFIG</string>|" "$tmp"
      fi
      install -m 0644 "$tmp" "$UNIT"; rm -f "${tmp:?}"
      launchctl bootout "gui/$(id -u)/$LABEL" 2>/dev/null || true
      launchctl bootstrap "gui/$(id -u)" "$UNIT"
      launchctl enable "gui/$(id -u)/$LABEL"
      launchctl print "gui/$(id -u)/$LABEL" | grep -E '^\s*(state|pid) =' || true
    fi
    note "installed; check in a minute with: $PYRED sentinel status"
    ;;
  uninstall)
    if [ "$OS" = linux ]; then
      systemctl --user disable --now pyre-sentinel.service 2>/dev/null || true
      rm -f "${UNIT:?}"; systemctl --user daemon-reload
    else
      launchctl bootout "gui/$(id -u)/$LABEL" 2>/dev/null || true
      rm -f "${UNIT:?}"
    fi
    note "removed the service (config and ~/.local/state/pyre/sentinel kept)"
    ;;
  status)
    if [ "$OS" = linux ]; then
      systemctl --user --no-pager status pyre-sentinel.service | head -n 5 || true
    else
      launchctl print "gui/$(id -u)/$LABEL" 2>/dev/null | grep -E '^\s*(state|pid|last exit code) =' || echo "not loaded"
    fi
    "$PYRED" sentinel status --config "$CONFIG" || true
    ;;
  *) sed -n '2,13p' "$0" | sed 's/^# \{0,1\}//'; exit 2 ;;
esac
