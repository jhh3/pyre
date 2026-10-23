#!/bin/bash
# Build and stage the bundled tools (Hive, Outfit, Rota) for each platform.
#
#   packaging/build-tools.sh [--version V] [--out DIR] [--platforms "darwin/arm64 linux/amd64 ..."]
#
# Writes one directory per platform, DIR/pyre-tools-<V>-<os>-<arch>/:
#   outfit/bin/outfit   Go binary (own module tools/outfit; CGO_ENABLED=0, -trimpath)
#   hive/               the Hive plugin tree (bin/hive launcher, Python lib, vendored
#                       jhh3-mem and ws); needs python3 >= 3.11, sqlite3, git, zstd
#   rota/bin/rota       single-file Python CLI; needs python3 >= 3.9
#   VERSION, BUILDINFO, NOTICE (the tools' third-party components)
# windows/<arch> platforms carry Outfit only, as outfit/bin/outfit.exe and an
# identical outfit/bin/outfit-core.exe (the command the Outfit plugin's MCP
# server and hooks run; Windows has no symlink shims). Hive and Rota are
# POSIX launchers and are not bundled for Windows.
# Only git-tracked files are copied (no __pycache__, no local state), with
# normalised modes, so release.sh can tar the result reproducibly.
# Default DIR: dist/tools. Default platforms: the four macOS/Linux release
# platforms (release.sh adds windows/amd64 and windows/arm64).
set -euo pipefail
umask 022
ROOT=$(cd "$(dirname "$0")/.." && pwd)
VERSION="" OUT="" PLATFORMS="darwin/arm64 darwin/amd64 linux/amd64 linux/arm64"
while [ $# -gt 0 ]; do
  case "$1" in
    --version) VERSION=${2:?}; shift 2 ;;
    --out) OUT=${2:?}; shift 2 ;;
    --platforms) PLATFORMS=${2:?}; shift 2 ;;
    *) sed -n '2,20p' "$0"; exit 2 ;;
  esac
done
[ -n "$VERSION" ] || VERSION=$(git -C "$ROOT" describe --tags --always --dirty 2>/dev/null || echo dev)
case "$VERSION" in *[!A-Za-z0-9._+-]*|"") echo "invalid version '$VERSION'" >&2; exit 2 ;; esac
[ -n "$OUT" ] || OUT="$ROOT/dist/tools"
mkdir -p "$OUT"
OUT=$(cd "$OUT" && pwd)  # absolute: go -C resolves a relative -o against tools/outfit
GOBIN=${GO:-go}
if [ -z "${GO:-}" ] && [ -x "$HOME/sdk/go1.27.1/bin/go" ]; then GOBIN="$HOME/sdk/go1.27.1/bin/go"; fi  # same rule as the Makefile
COMMIT=$(git -C "$ROOT" rev-parse --short=12 HEAD 2>/dev/null || echo unknown)
EPOCH=${SOURCE_DATE_EPOCH:-$(git -C "$ROOT" log -1 --format=%ct 2>/dev/null || echo 0)}
DATE=$(TZ=UTC date -r "$EPOCH" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || TZ=UTC date -d "@$EPOCH" +%Y-%m-%dT%H:%M:%SZ)
OUTFIT_VERSION=$(awk '/^## [0-9]/ {print $2; exit}' "$ROOT/tools/outfit/CHANGELOG.md")
[ -n "$OUTFIT_VERSION" ] || OUTFIT_VERSION=dev
OUTFIT_LDFLAGS="-s -w -X main.version=$OUTFIT_VERSION -X main.commit=pyre-$COMMIT -X main.date=$DATE"

# copy_tracked SRC_SUBDIR DEST: git-tracked files only, modes 0755/0644.
copy_tracked() {
  local src=$1 dest=$2 f rel
  git -C "$ROOT" ls-files -z -- "$src" | while IFS= read -r -d '' f; do
    rel=${f#"$src"/}
    [ -f "$ROOT/$f" ] || continue  # deleted in the working tree
    mkdir -p "$dest/$(dirname "$rel")"
    if [ -x "$ROOT/$f" ]; then install -m 0755 "$ROOT/$f" "$dest/$rel"; else install -m 0644 "$ROOT/$f" "$dest/$rel"; fi
  done
}

mkdir -p "$OUT"
for p in $PLATFORMS; do
  os=${p%/*} arch=${p#*/}
  d="$OUT/pyre-tools-$VERSION-$os-$arch"
  rm -rf "${d:?}"
  exe=""
  [ "$os" != windows ] || exe=.exe
  mkdir -p "$d/outfit/bin"
  echo "==> outfit $OUTFIT_VERSION $os/$arch"
  CGO_ENABLED=0 GOOS=$os GOARCH=$arch GOFLAGS=-mod=readonly "$GOBIN" -C "$ROOT/tools/outfit" build -trimpath \
    -ldflags "$OUTFIT_LDFLAGS" -o "$d/outfit/bin/outfit$exe" ./cmd/outfit
  chmod 0755 "$d/outfit/bin/outfit$exe"
  install -m 0644 "$ROOT/tools/outfit/README.md" "$d/outfit/README.md"
  if [ "$os" = windows ]; then
    install -m 0755 "$d/outfit/bin/outfit.exe" "$d/outfit/bin/outfit-core.exe"
  else
    mkdir -p "$d/hive" "$d/rota/bin"
    copy_tracked tools/hive/plugins/hive "$d/hive"
    install -m 0644 "$ROOT/tools/hive/README.md" "$d/hive/README.md"
    install -m 0644 "$ROOT/tools/hive/docs/hive.toml.example" "$d/hive/hive.toml.example"
    install -m 0755 "$ROOT/tools/rota/rota" "$d/rota/bin/rota"
    install -m 0644 "$ROOT/tools/rota/README.md" "$d/rota/README.md"
  fi
  echo "$VERSION" >"$d/VERSION"
  {
    echo "pyre-tools $VERSION ($os/$arch), commit $COMMIT"
    echo "outfit: $OUTFIT_VERSION, $("$GOBIN" version | awk '{print $3}'), CGO_ENABLED=0 go build -trimpath -ldflags '$OUTFIT_LDFLAGS'"
    if [ "$os" = windows ]; then
      echo "outfit-core.exe: a copy of outfit.exe (the Outfit plugin's command)"
      echo "hive, rota: not bundled for Windows (POSIX launchers)"
    else
      echo "hive: $(sed -n 's/.*"version": *"\([^"]*\)".*/\1/p' "$ROOT/tools/hive/plugins/hive/.claude-plugin/plugin.json" | head -n 1) (Python source)"
      echo "rota: Python source"
    fi
  } >"$d/BUILDINFO"
  {
    if [ "$os" = windows ]; then echo "Pyre bundled tools (Outfit) $VERSION"; else echo "Pyre bundled tools (Hive, Outfit, Rota) $VERSION"; fi
    echo "MIT License, see LICENSE in the Pyre repository."
    echo
    echo "Third-party Go modules compiled into outfit (go version -m):"
    "$GOBIN" version -m "$d/outfit/bin/outfit$exe" | awk '$1 == "dep" {print "  " $2 " " $3}' | sort -u
    echo
    sed -n '/^Bundled tools/,$p' "$ROOT/NOTICE"
  } >"$d/NOTICE"
  echo "==> $d"
done
