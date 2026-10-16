# shellcheck shell=bash
# Shared helpers for scripts/install/{install,uninstall,upgrade}.sh.
# Sourced, not executed. Compatible with macOS /bin/bash 3.2 and Linux bash.
#
# Layout (per user, never root):
#   <home>/.local/share/pyre/<version>/bin/{pyre,pyred,pyre-hub}   immutable version dirs
#   <home>/.local/share/pyre/current -> <version>                  default for new processes
#   <home>/.local/share/pyre/.previous                             version before the last switch
#   <home>/.local/share/pyre/<old version> -> current              pruned version (alias, see prune_versions)
#   <home>/.local/share/pyre/.pyre-install                         ownership marker
#   <home>/.local/bin/{pyre,pyred,pyre-hub} -> ../share/pyre/current/bin/<name>
#   macOS units:  <home>/Library/LaunchAgents/dev.jhh3.pyre.{pyred,hub}.plist
#   Linux units:  <home>/.config/systemd/user/{pyred,pyre-hub}.service
#
# Modes:
#   --root DIR            prefix every write with DIR (fake root); service
#                         manager commands are printed, never run
#   --no-service-manager  never run launchctl/systemctl; print them instead
#                         (also implied by --home other than $HOME, and by
#                         PYRE_INSTALL_NO_SERVICE_MANAGER=1)
#   --dry-run             write nothing; print every action

PYRE_MARKER="pyre-managed"
PYRE_LABEL_NODE="dev.jhh3.pyre.pyred"
PYRE_LABEL_HUB="dev.jhh3.pyre.hub"
PYRE_BINS="pyre pyred pyre-hub"

# Globals filled by parse_common_flag / setup_paths.
ROOT=""
NODE_STATE=""
HUB_DATA=""
DRY=0
NO_SM=${PYRE_INSTALL_NO_SERVICE_MANAGER:-0}
T_OS=""
T_ARCH=""
T_HOME=""
T_USER=""
HOST_OS=""

die() { echo "error: $*" >&2; exit "${DIE_CODE:-1}"; }
note() { echo "==> $*"; }
warn() { echo "warning: $*" >&2; }

host_os() {
  case "$(uname -s)" in
    Darwin) echo darwin ;;
    Linux) echo linux ;;
    *) echo unknown ;;
  esac
}

host_arch() {
  case "$(uname -m)" in
    arm64|aarch64) echo arm64 ;;
    x86_64|amd64) echo amd64 ;;
    *) uname -m ;;
  esac
}

# parse_common_flag "$@": consumes one common flag; sets SHIFT_N to the
# number of arguments consumed (0 if not a common flag).
parse_common_flag() {
  SHIFT_N=0
  case "$1" in
    --root) [ $# -ge 2 ] || die "--root needs a directory"; ROOT=${2%/}; SHIFT_N=2 ;;
    --dry-run) DRY=1; SHIFT_N=1 ;;
    --no-service-manager) NO_SM=1; SHIFT_N=1 ;;
    --os) [ $# -ge 2 ] || die "--os needs darwin|linux"; T_OS=$2; SHIFT_N=2 ;;
    --arch) [ $# -ge 2 ] || die "--arch needs arm64|amd64"; T_ARCH=$2; SHIFT_N=2 ;;
    --home) [ $# -ge 2 ] || die "--home needs a directory"; T_HOME=${2%/}; SHIFT_N=2 ;;
    --user) [ $# -ge 2 ] || die "--user needs a name"; T_USER=$2; SHIFT_N=2 ;;
  esac
}

setup_paths() {
  HOST_OS=$(host_os)
  [ -n "$T_OS" ] || T_OS=$HOST_OS
  [ -n "$T_ARCH" ] || T_ARCH=$(host_arch)
  [ -n "$T_HOME" ] || T_HOME=$HOME
  [ -n "$T_USER" ] || T_USER=$(id -un)
  case "$T_OS" in darwin|linux) ;; *) die "unsupported OS '$T_OS' (darwin|linux)" ;; esac
  case "$T_HOME" in /*) ;; *) die "--home must be absolute" ;; esac
  if [ -n "$ROOT" ]; then
    case "$ROOT" in /*) ;; *) die "--root must be absolute" ;; esac
    [ "$ROOT" != "/" ] || ROOT=""
  fi
  if [ -n "$ROOT" ] || [ "$T_HOME" != "$HOME" ] || [ "$T_OS" != "$HOST_OS" ]; then
    NO_SM=1
  fi
  if [ "$(id -u)" = 0 ] && [ -z "$ROOT" ] && [ "$DRY" = 0 ]; then
    die "run as the owning user, not root (Pyre services are per-user)"
  fi
  PREFIX="$T_HOME/.local/share/pyre"
  SHIM_DIR="$T_HOME/.local/bin"
  CONFIG_DIR="$T_HOME/.config/pyre"
  CLIENT_CONFIG="$CONFIG_DIR/client.json"
  if [ "$T_OS" = darwin ]; then
    STATE_DIR="$T_HOME/Library/Application Support/pyre"
    HUB_DATA_DEFAULT="$T_HOME/Library/Application Support/pyre-hub"
    LOG_DIR="$T_HOME/Library/Logs/pyre"
    UNIT_DIR="$T_HOME/Library/LaunchAgents"
    UNIT_NODE="$UNIT_DIR/$PYRE_LABEL_NODE.plist"
    UNIT_HUB="$UNIT_DIR/$PYRE_LABEL_HUB.plist"
    UNIT_SLICE=""
  else
    STATE_DIR="$T_HOME/.local/state/pyre"
    HUB_DATA_DEFAULT="$T_HOME/.local/share/pyre-hub"
    LOG_DIR="$T_HOME/.local/state/pyre/logs"
    UNIT_DIR="$T_HOME/.config/systemd/user"
    UNIT_NODE="$UNIT_DIR/pyred.service"
    UNIT_HUB="$UNIT_DIR/pyre-hub.service"
    UNIT_SLICE="$UNIT_DIR/pyre-runs.slice"
  fi
  NODE_STATE=${NODE_STATE:-$STATE_DIR}
  HUB_DATA=${HUB_DATA:-$HUB_DATA_DEFAULT}
}

# r PATH: the path as written on this machine (fake-root prefixed).
r() { printf '%s%s' "$ROOT" "$1"; }

# can_exec: the target platform's binaries can run here.
can_exec() { [ "$T_OS" = "$HOST_OS" ] && [ "$T_ARCH" = "$(host_arch)" ]; }

# act CMD...: run a filesystem command, or print it in dry-run mode.
act() {
  if [ "$DRY" = 1 ]; then
    echo "[dry-run] $*"
    return 0
  fi
  "$@"
}

# sm CMD...: run a service-manager command, or print it.
sm() {
  if [ "$DRY" = 1 ] || [ "$NO_SM" = 1 ]; then
    echo "[service-manager, not run] $*"
    return 0
  fi
  echo "+ $*"
  "$@"
}

sha256_of() {
  if command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{print $1}'
  else
    sha256sum "$1" | awk '{print $1}'
  fi
}

# verify_sums DIR SUMSFILE: every listed file under DIR matches.
verify_sums() {
  local dir=$1 sums=$2 line want file got n=0
  [ -f "$sums" ] || die "checksum file $sums not found"
  while IFS= read -r line; do
    [ -n "$line" ] || continue
    want=${line%% *}
    file=${line#* }
    file=${file# }
    file=${file#\*}
    [ -f "$dir/$file" ] || die "checksum: $dir/$file missing"
    got=$(sha256_of "$dir/$file")
    [ "$got" = "$want" ] || die "checksum mismatch for $file (expected $want, got $got)"
    n=$((n + 1))
  done <"$sums"
  [ "$n" -gt 0 ] || die "checksum file $sums is empty"
  echo "$n"
}

valid_version() {
  case "$1" in
    ""|current|bin|.*|*/*) return 1 ;;
  esac
  case "$1" in
    *[!A-Za-z0-9._+-]*) return 1 ;;
  esac
  return 0
}

# json_num KEY: first numeric value of "KEY": N from stdin (flat JSON only).
json_num() {
  tr -d '\n' | sed -n "s/.*\"$1\"[[:space:]]*:[[:space:]]*\([0-9][0-9]*\).*/\1/p" | head -n 1
}

# json_str KEY: first string value of "KEY": "V" from stdin (no escapes).
json_str() {
  tr -d '\n' | sed -n "s/.*\"$1\"[[:space:]]*:[[:space:]]*\"\([^\"]*\)\".*/\1/p" | head -n 1
}

xml_escape() {
  printf '%s' "$1" | sed -e 's/&/\&amp;/g' -e 's/</\&lt;/g' -e 's/>/\&gt;/g' -e 's/"/\&quot;/g'
}

# systemd_word ARG: one ExecStart word, double-quoted with systemd escapes.
systemd_word() {
  local s
  s=$(printf '%s' "$1" | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g' -e 's/%/%%/g' -e 's/\$/$$/g')
  printf '"%s"' "$s"
}

# render_template SRC DST: replaces @NAME@ with $R_NAME (exported) and drops
# blocks between "# BEGIN <tag>" and "# END <tag>" (or <!-- BEGIN/END -->)
# when R_DROP_<tag>=1.
render_template() {
  local src=$1 dst=$2 out
  out=$(awk '
    function dropping(line,   t) {
      if (match(line, /BEGIN [a-z_]+/)) {
        t = substr(line, RSTART + 6, RLENGTH - 6)
        if (ENVIRON["R_DROP_" t] == "1") { skip = t }
        return 1
      }
      if (match(line, /END [a-z_]+/)) { skip = ""; return 1 }
      return 0
    }
    {
      if ($0 ~ /(# |<!-- )(BEGIN|END) [a-z_]+/) { dropping($0); next }
      if (skip != "") next
      rest = $0; out = ""
      while (match(rest, /@[A-Z_]+@/)) {
        name = substr(rest, RSTART + 1, RLENGTH - 2)
        if (!(("R_" name) in ENVIRON)) { print "render: unset @" name "@" > "/dev/stderr"; exit 3 }
        out = out substr(rest, 1, RSTART - 1) ENVIRON["R_" name]
        rest = substr(rest, RSTART + RLENGTH)
      }
      print out rest
    }' "$src") || die "failed to render $src"
  if [ "$DRY" = 1 ]; then
    echo "[dry-run] write $dst:"
    printf '%s\n' "$out" | sed 's/^/    | /'
    return 0
  fi
  mkdir -p "$(dirname "$dst")"
  printf '%s\n' "$out" >"$dst.tmp.$$"
  chmod 644 "$dst.tmp.$$"
  mv -f "$dst.tmp.$$" "$dst"
}

is_managed() { [ -f "$1" ] && grep -q "$PYRE_MARKER" "$1"; }

# sync_app: on macOS keep $PREFIX/Pyre.app (the launchd program, at a fixed
# path) identical to the active version's Pyre.app, so macOS privacy grants,
# which belong to the signed bundle dev.jhh3.pyre at that path, survive
# upgrades and rollbacks. A running pyred keeps its open binary.
sync_app() {
  local src dst
  src=$(r "$PREFIX/current/Pyre.app") dst=$(r "$PREFIX/Pyre.app")
  if [ ! -d "$src" ]; then
    # The active version predates Pyre.app (a rollback): keep the launchd
    # path valid with that version's pyred in an ad-hoc signed bundle
    # (macOS grants do not carry over to it).
    [ -d "$dst" ] || return 0
    [ -x "$(r "$PREFIX/current/bin/pyred")" ] || return 0
    if [ "$DRY" = 1 ]; then echo "[dry-run] rebuild $dst from current/bin/pyred"; return 0; fi
    if [ "$("$dst/Contents/MacOS/pyred" version 2>/dev/null | head -1)" = "$("$(r "$PREFIX/current/bin/pyred")" version 2>/dev/null | head -1)" ] && \
      ! codesign -dv "$dst" 2>&1 | grep -q '^TeamIdentifier=[A-Z0-9]'; then return 0; fi
    rm -rf "$dst.new.$$" "$dst.old.$$"
    mkdir -p "$dst.new.$$/Contents/MacOS"
    cp "$(r "$PREFIX/current/bin/pyred")" "$dst.new.$$/Contents/MacOS/pyred"
    printf '%s\n' '<?xml version="1.0" encoding="UTF-8"?>' '<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">' \
      '<plist version="1.0"><dict><key>CFBundleIdentifier</key><string>dev.jhh3.pyre</string><key>CFBundleName</key><string>Pyre</string>' \
      '<key>CFBundleExecutable</key><string>pyred</string><key>CFBundlePackageType</key><string>APPL</string><key>LSUIElement</key><true/></dict></plist>' \
      >"$dst.new.$$/Contents/Info.plist"
    ! command -v codesign >/dev/null 2>&1 || codesign --force --sign - "$dst.new.$$" >/dev/null 2>&1 || true
    mv "$dst" "$dst.old.$$"; mv "$dst.new.$$" "$dst"; rm -rf "$dst.old.$$"
    warn "Pyre.app now runs $(cat "$(r "$PREFIX/current/VERSION")" 2>/dev/null || echo current) ad-hoc signed (it predates the signed bundle): macOS permissions must be granted again until you upgrade"
    return 0
  fi
  if [ "$DRY" = 1 ]; then echo "[dry-run] sync $src -> $dst"; return 0; fi
  if [ -d "$dst" ] && diff -rq "$src" "$dst" >/dev/null 2>&1; then return 0; fi
  rm -rf "$dst.new.$$" "$dst.old.$$"
  if command -v ditto >/dev/null 2>&1; then ditto "$src" "$dst.new.$$"; else cp -R "$src" "$dst.new.$$"; fi
  [ ! -d "$dst" ] || mv "$dst" "$dst.old.$$"
  mv "$dst.new.$$" "$dst"
  rm -rf "$dst.old.$$"
  note "Pyre.app -> $(cat "$(r "$PREFIX/current/VERSION")" 2>/dev/null || echo current) ($dst)"
}

# swap_symlink TARGET LINK: atomically point LINK at TARGET (relative).
swap_symlink() {
  local target=$1 link=$2 tmp
  if [ "$DRY" = 1 ]; then
    echo "[dry-run] ln -s $target $link (atomic replace)"
    return 0
  fi
  if [ -e "$link" ] && [ ! -L "$link" ]; then
    die "$link exists and is not a symlink; refusing to replace it"
  fi
  tmp="$link.new.$$"
  rm -f "$tmp"
  ln -s "$target" "$tmp"
  if [ "$HOST_OS" = darwin ]; then
    mv -fh "$tmp" "$link"
  else
    mv -fT "$tmp" "$link"
  fi
}

# is_alias VERSION: the version dir was pruned and is now a symlink alias
# (normally "<version> -> current"); it cannot be switched to.
is_alias() { [ -L "$(r "$PREFIX/$1")" ]; }

# prune_versions: after a successful switch, every installed version other
# than current and .previous is removed and left behind as the alias
# "<version> -> current". Paths written while it was current (Claude hooks,
# MCP commands and run environments of sessions started under it, e.g.
# .../share/pyre/<version>/bin/pyred) keep resolving, to the current binaries;
# processes already running from it keep their (unlinked) executable. The
# alias replaces the old manual workaround of symlinking removed versions.
# Rollback targets are never pruned: current and .previous always stay.
prune_versions() {
  local cur prev d v aside n=0
  cur=$(current_version || true)
  [ -n "$cur" ] || return 0
  is_alias "$cur" && return 0 # never prune through an aliased current
  prev=""
  [ ! -f "$(r "$PREFIX/.previous")" ] || prev=$(cat "$(r "$PREFIX/.previous")")
  for d in "$(r "$PREFIX")"/*; do
    [ -d "$d" ] && [ ! -L "$d" ] || continue
    v=$(basename "$d")
    valid_version "$v" || continue
    [ "$v" != "$cur" ] && [ "$v" != "$prev" ] || continue
    [ -f "$d/VERSION" ] || [ -x "$d/bin/pyred" ] || continue # Pyre version dirs only
    aside="$(r "$PREFIX")/.pruned-$v.$$"
    act mv "$d" "$aside"
    act ln -s current "$d"
    act rm -rf "$aside"
    n=$((n + 1))
    note "pruned $v (left as $PREFIX/$v -> current, so paths into it keep working)"
  done
  [ "$n" -gt 0 ] || note "nothing to prune (kept current $cur${prev:+ and previous $prev})"
  return 0
}

current_version() {
  local l
  l=$(r "$PREFIX/current")
  [ -L "$l" ] || return 1
  basename "$(readlink "$l")"
}

# sm_domain: the launchd domain for per-user agents.
sm_domain() { echo "gui/$(id -u)"; }

label_for() { if [ "$1" = hub ]; then echo "$PYRE_LABEL_HUB"; else echo "$PYRE_LABEL_NODE"; fi; }
unit_for() { if [ "$1" = hub ]; then echo "$UNIT_HUB"; else echo "$UNIT_NODE"; fi; }
sdunit_for() { if [ "$1" = hub ]; then echo pyre-hub.service; else echo pyred.service; fi; }

service_load() { # node|hub — first enable/start after the unit file is written
  local svc=$1
  if [ "$T_OS" = darwin ]; then
    sm launchctl bootout "$(sm_domain)/$(label_for "$svc")" || true
    sm launchctl bootstrap "$(sm_domain)" "$(unit_for "$svc")"
    sm launchctl enable "$(sm_domain)/$(label_for "$svc")"
  else
    sm systemctl --user daemon-reload
    sm systemctl --user enable "$(sdunit_for "$svc")"
    sm systemctl --user restart "$(sdunit_for "$svc")"
  fi
}

service_restart() { # node|hub — stopping pyred never stops runs (KillMode=process / AbandonProcessGroup)
  local svc=$1
  if [ -n "${PYRE_UPGRADE_RESTART_HOOK:-}" ]; then
    echo "+ $PYRE_UPGRADE_RESTART_HOOK restart $svc   (restart hook)"
    [ "$DRY" = 1 ] && return 0
    "$PYRE_UPGRADE_RESTART_HOOK" restart "$svc"
    return $?
  fi
  if [ "$T_OS" = darwin ]; then
    sm launchctl kickstart -k "$(sm_domain)/$(label_for "$svc")"
  else
    sm systemctl --user restart "$(sdunit_for "$svc")"
  fi
}

service_unload() { # node|hub — stop + disable
  local svc=$1
  if [ -n "${PYRE_UPGRADE_RESTART_HOOK:-}" ]; then
    echo "+ $PYRE_UPGRADE_RESTART_HOOK stop $svc   (restart hook)"
    [ "$DRY" = 1 ] || "$PYRE_UPGRADE_RESTART_HOOK" stop "$svc" || true
  fi
  if [ "$T_OS" = darwin ]; then
    sm launchctl bootout "$(sm_domain)/$(label_for "$svc")" || true
  else
    sm systemctl --user disable --now "$(sdunit_for "$svc")" || true
    sm systemctl --user daemon-reload
  fi
}

# service_installed node|hub: a Pyre-managed unit file exists.
service_installed() { is_managed "$(r "$(unit_for "$1")")"; }

# rotate_log FILE: keep one previous copy when FILE exceeds 10 MiB (launchd
# does not rotate StandardOutPath/StandardErrorPath).
rotate_log() {
  local f=$1 size
  [ -f "$f" ] || return 0
  size=$(wc -c <"$f" | tr -d ' ')
  if [ "$size" -gt 10485760 ]; then
    act mv -f "$f" "$f.1"
  fi
}

rotate_service_logs() {
  [ "$T_OS" = darwin ] || return 0
  local f
  for f in pyred.out.log pyred.err.log pyre-hub.out.log pyre-hub.err.log; do
    rotate_log "$(r "$LOG_DIR/$f")"
  done
}

# node_status_json: `pyred status --json` for the installed node, or empty.
node_status_json() {
  local bin
  bin=$(r "$PREFIX/current/bin/pyred")
  can_exec || return 1
  [ -x "$bin" ] || return 1
  "$bin" status --state "$(r "$NODE_STATE")" --json 2>/dev/null
}
