#!/bin/bash
# Build checksummed, signed release tarballs from `make dist` (see packaging/README.md).
#
#   packaging/release.sh [--version V] [--skip-build] [--out DIR]
#                        [--signing-key FILE | --no-sign] [--allowed-signers FILE]
#                        [--no-sbom] [--no-tools] [--no-windows] [--strict]
#
# Steps: make dist VERSION=V (darwin/linux x arm64/amd64, CGO_ENABLED=0,
# -trimpath) -> verify dist/SHA256SUMS -> stage one directory per platform
# (with a CycloneDX SBOM per binary in sbom/) -> deterministic tar.gz (sorted
# entries, fixed mtime = commit time, numeric owner 0, no macOS metadata,
# gzip -n) -> <out>/SHA256SUMS over the tarballs -> <out>/SHA256SUMS.sig
# (`ssh-keygen -Y sign`, namespace pyre-release) verified against
# packaging/release-allowed-signers. pyre-mockhub (development only) is not shipped.
#
# Bundled tools: unless --no-tools, packaging/build-tools.sh also stages Hive,
# Outfit and Rota per platform and each becomes pyre-tools-<V>-<plat>.tar.gz
# (same deterministic tar, listed in the same signed SHA256SUMS). They are
# optional: install.sh installs them only with --with-tools. Unless
# --no-windows, pyre-tools-<V>-windows-<arch>.zip carries Outfit for Windows
# (outfit.exe and outfit-core.exe) for install.ps1 -WithTools.
#
# Windows: unless --no-windows, pyre.exe and pyred.exe are also built for
# windows/amd64 and windows/arm64 (same CGO_ENABLED=0 -trimpath -ldflags; no
# pyre-hub, which is unix-only; taken from dist/pyre-<V>-windows-<arch>/ when
# present) and packed as pyre-<V>-windows-<arch>.zip: deterministic zip (sorted
# entries, fixed DOS mtime = commit time in UTC, `zip -X`, no directory
# entries) holding bin/, scripts/install/{install,uninstall}.ps1, README.md,
# VERSION, BUILDINFO, NOTICE, sbom/ and an inner SHA256SUMS; listed in the
# same signed SHA256SUMS. The darwin/linux tarballs are unchanged.
#
# Signing key: --signing-key, else $PYRE_RELEASE_SIGNING_KEY, else
# ~/.config/pyre/release-signing when present (kept outside the repository).
# SBOM tool: $CYCLONEDX_GOMOD, else cyclonedx-gomod on PATH or in $(go env GOPATH)/bin.
# Without a key or the SBOM tool the release is built unsigned / without SBOMs
# and says so; --strict makes both mandatory (use it for real releases).
set -euo pipefail
umask 022
ROOT=$(cd "$(dirname "$0")/.." && pwd)
VERSION="" SKIP=0 OUT="" SIGNKEY=${PYRE_RELEASE_SIGNING_KEY:-} NOSIGN=0 NOSBOM=0 STRICT=0 TOOLS=1 WINDOWS=1
ALLOWED="$ROOT/packaging/release-allowed-signers"
NAMESPACE=pyre-release
while [ $# -gt 0 ]; do
  case "$1" in
    --version) VERSION=${2:?}; shift 2 ;;
    --skip-build) SKIP=1; shift ;;
    --out) OUT=${2:?}; shift 2 ;;
    --signing-key) SIGNKEY=${2:?}; shift 2 ;;
    --no-sign) NOSIGN=1; shift ;;
    --allowed-signers) ALLOWED=${2:?}; shift 2 ;;
    --no-sbom) NOSBOM=1; shift ;;
    --no-tools) TOOLS=0; shift ;;
    --no-windows) WINDOWS=0; shift ;;
    --strict) STRICT=1; shift ;;
    *) sed -n '2,36p' "$0"; exit 2 ;;
  esac
done
if [ "$NOSIGN" = 0 ] && [ -z "$SIGNKEY" ] && [ -f "$HOME/.config/pyre/release-signing" ]; then
  SIGNKEY="$HOME/.config/pyre/release-signing"
fi
if [ "$NOSIGN" = 1 ]; then SIGNKEY=""; fi
if [ "$STRICT" = 1 ]; then
  [ -n "$SIGNKEY" ] || { echo "--strict: no signing key (--signing-key, PYRE_RELEASE_SIGNING_KEY or ~/.config/pyre/release-signing)" >&2; exit 2; }
  [ "$NOSBOM" = 0 ] || { echo "--strict and --no-sbom are exclusive" >&2; exit 2; }
fi
if [ -n "$SIGNKEY" ]; then
  [ -f "$SIGNKEY" ] || { echo "signing key $SIGNKEY not found" >&2; exit 2; }
  [ -f "$ALLOWED" ] || { echo "allowed signers file $ALLOWED not found" >&2; exit 2; }
fi
[ -n "$VERSION" ] || VERSION=$(git -C "$ROOT" describe --tags --always --dirty 2>/dev/null || echo dev)
case "$VERSION" in *[!A-Za-z0-9._+-]*|"") echo "invalid version '$VERSION'" >&2; exit 2 ;; esac
[ -n "$OUT" ] || OUT="$ROOT/dist/release"
PLATFORMS="darwin-arm64 darwin-amd64 linux-amd64 linux-arm64"
sha() { if command -v shasum >/dev/null 2>&1; then shasum -a 256 "$@"; else sha256sum "$@"; fi; }
COMMIT=$(git -C "$ROOT" rev-parse HEAD 2>/dev/null || echo unknown)
EPOCH=${SOURCE_DATE_EPOCH:-$(git -C "$ROOT" log -1 --format=%ct 2>/dev/null || echo 0)}
GOBIN=${GO:-go}
if [ -z "${GO:-}" ] && [ -x "$HOME/sdk/go1.27.1/bin/go" ]; then GOBIN="$HOME/sdk/go1.27.1/bin/go"; fi  # same rule as the Makefile
STAMP=$(TZ=UTC date -r "$EPOCH" +%Y%m%d%H%M.%S 2>/dev/null || TZ=UTC date -d "@$EPOCH" +%Y%m%d%H%M.%S)
SBOMTOOL=""
if [ "$NOSBOM" = 0 ]; then
  if [ -n "${CYCLONEDX_GOMOD:-}" ]; then SBOMTOOL=$CYCLONEDX_GOMOD
  elif command -v cyclonedx-gomod >/dev/null 2>&1; then SBOMTOOL=$(command -v cyclonedx-gomod)
  elif gp=$("$GOBIN" env GOPATH 2>/dev/null) && [ -x "${gp%%:*}/bin/cyclonedx-gomod" ]; then SBOMTOOL="${gp%%:*}/bin/cyclonedx-gomod"
  fi
  if [ -z "$SBOMTOOL" ]; then
    if [ "$STRICT" = 1 ]; then echo "--strict: cyclonedx-gomod not found (go install github.com/CycloneDX/cyclonedx-gomod/cmd/cyclonedx-gomod@v1.10.0)" >&2; exit 2; fi
    echo "warning: cyclonedx-gomod not found; tarballs will carry no SBOM" >&2
  fi
fi
# The SBOM tool runs `go version -m`; make sure it finds the same toolchain.
GODIR=$(dirname "$(command -v "$GOBIN")")
SBOMVER=""
[ -z "$SBOMTOOL" ] || SBOMVER=$("$SBOMTOOL" version 2>/dev/null | awk '/^Version:/ {print $2}' | head -n 1)

# sbom_check BIN SBOM: every module `go version -m` lists for BIN is a component.
sbom_check() {
  local bin=$1 sbom=$2 path ver missing=0
  while read -r path ver; do
    if ! grep -q "\"pkg:golang/$path@$ver?type=module\"" "$sbom"; then
      echo "SBOM $sbom lacks $path@$ver" >&2; missing=1
    fi
  done < <("$GOBIN" version -m "$bin" | awk '
    $1 == "dep" { if (p != "") print p; p = $2 " " $3 }
    $1 == "=>"  { p = $2 " " $3 }   # a replaced module is listed as its replacement
    END { if (p != "") print p }')
  return "$missing"
}
DIRTY=""
if [ -n "$(git -C "$ROOT" status --porcelain 2>/dev/null)" ]; then DIRTY=" (working tree has uncommitted changes)"; fi

if [ "$SKIP" = 0 ]; then
  echo "==> make dist VERSION=$VERSION"
  make -C "$ROOT" dist VERSION="$VERSION"
fi
(cd "$ROOT/dist" && sha -c SHA256SUMS >/dev/null) || { echo "dist/SHA256SUMS verification failed" >&2; exit 1; }
echo "==> dist/SHA256SUMS verified"

mkdir -p "$OUT"
STAGE=$(mktemp -d "${TMPDIR:-/tmp}/pyre-release.XXXXXX")
trap 'rm -rf "$STAGE"' EXIT
# pack BASE NAME: deterministic tar.gz of BASE/NAME into $OUT/NAME.tar.gz.
pack() {
  local base=$1 name=$2
  (cd "$base/$name" && find . -type f ! -name SHA256SUMS | sed 's|^\./||' | LC_ALL=C sort | while IFS= read -r f; do sha "$f"; done >SHA256SUMS)
  find "$base/$name" -exec env TZ=UTC touch -t "$STAMP" {} +
  (cd "$base" && find "$name" -print | LC_ALL=C sort >"$base/$name.files")
  rm -f "$OUT/$name.tar.gz"
  if tar --version 2>/dev/null | grep -q 'GNU tar'; then
    (cd "$base" && tar --no-recursion --owner=0 --group=0 --numeric-owner --format=ustar -cf - -T "$name.files") | gzip -n -9 >"$OUT/$name.tar.gz"
  else
    (cd "$base" && COPYFILE_DISABLE=1 tar --no-recursion --uid 0 --gid 0 --uname root --gname root --no-xattrs --no-mac-metadata --format ustar -cf - -T "$name.files") | gzip -n -9 >"$OUT/$name.tar.gz"
  fi
  LIST="$LIST $name.tar.gz"
  echo "==> $OUT/$name.tar.gz"
}
# macOS code signing (darwin tarballs): a stable identity keeps macOS privacy
# grants (Full Disk Access, removable volumes, other apps' data) across
# releases; ad-hoc signatures change with every build, so each release looked
# like a new app. PYRE_MACOS_SIGN_IDENTITY picks the identity ("none" skips);
# default: the first "Developer ID Application" identity in the keychain.
# Signatures carry Apple's secure timestamp (they stay valid after the
# certificate expires), so darwin tarballs are not byte-reproducible.
MACSIGN=${PYRE_MACOS_SIGN_IDENTITY:-}
if [ -z "$MACSIGN" ] && [ "$(uname)" = Darwin ] && command -v security >/dev/null 2>&1; then
  MACSIGN=$(security find-identity -v -p codesigning 2>/dev/null | sed -n 's/.*"\(Developer ID Application: [^"]*\)".*/\1/p' | head -1)
fi
[ "$MACSIGN" != none ] || MACSIGN=""
if [ -n "$MACSIGN" ]; then echo "==> macOS signing identity: $MACSIGN"; else echo "==> macOS binaries stay ad-hoc signed (no signing identity; macOS permissions will not carry across releases)" >&2; fi
# darwin_app DIR SRC: Pyre.app (dev.jhh3.pyre) wrapping pyred, then sign the
# binaries and the bundle.
darwin_app() {
  local d=$1 src=$2 app="$1/Pyre.app"
  mkdir -p "$app/Contents/MacOS"
  install -m 0755 "$src/pyred" "$app/Contents/MacOS/pyred"
  sed -e "s/@VERSION@/$VERSION/g" "$ROOT/packaging/macos/Info.plist.in" >"$app/Contents/Info.plist"
  chmod 0644 "$app/Contents/Info.plist"
  [ -n "$MACSIGN" ] || return 0
  local b
  for b in pyre pyred pyre-hub; do
    codesign --force --timestamp --sign "$MACSIGN" --identifier "dev.jhh3.pyre.$b" "$d/bin/$b" || { echo "codesign $b failed" >&2; exit 1; }
  done
  codesign --force --timestamp --sign "$MACSIGN" "$app" || { echo "codesign Pyre.app failed" >&2; exit 1; }
  codesign --verify --strict "$app" && codesign --verify --strict "$d/bin/pyred" || { echo "codesign verification failed" >&2; exit 1; }
}

LIST=""
for plat in $PLATFORMS; do
  src="$ROOT/dist/pyre-$VERSION-$plat"
  [ -d "$src" ] || { echo "missing $src (did make dist run with VERSION=$VERSION?)" >&2; exit 1; }
  name="pyre-$VERSION-$plat"
  d="$STAGE/$name"
  mkdir -p "$d/bin" "$d/scripts/install" "$d/scripts/backup" "$d/packaging/launchd" "$d/packaging/systemd" "$d/packaging/sentinel"
  for b in pyre pyred pyre-hub; do install -m 0755 "$src/$b" "$d/bin/$b"; done
  case "$plat" in darwin-*) darwin_app "$d" "$src" ;; esac
  if [ -n "$SBOMTOOL" ]; then
    mkdir -p "$d/sbom"
    for b in pyre pyred pyre-hub; do
      # -noserial/-notimestamp keep the tarball reproducible; -short-purls
      # because v1.10.0 fills goos/goarch qualifiers from the host, not the binary.
      PATH="$GODIR:$PATH" "$SBOMTOOL" bin -json -std -noserial -notimestamp -short-purls \
        -version "$VERSION" -output "$d/sbom/$b.cdx.json" "$d/bin/$b"
      chmod 0644 "$d/sbom/$b.cdx.json"
      sbom_check "$d/bin/$b" "$d/sbom/$b.cdx.json" || { echo "SBOM for $plat/$b is incomplete" >&2; exit 1; }
    done
  fi
  for f in install.sh uninstall.sh upgrade.sh bootstrap-node.sh lib.sh sentinel.sh; do install -m 0755 "$ROOT/scripts/install/$f" "$d/scripts/install/$f"; done
  for f in backup-hub.sh restore-hub.sh; do install -m 0755 "$ROOT/scripts/backup/$f" "$d/scripts/backup/$f"; done
  install -m 0644 "$ROOT"/packaging/launchd/*.plist.in "$d/packaging/launchd/"
  install -m 0644 "$ROOT"/packaging/systemd/*.in "$d/packaging/systemd/"
  install -m 0644 "$ROOT"/packaging/sentinel/* "$d/packaging/sentinel/"
  sed -e "s/@VERSION@/$VERSION/g" -e "s/@PLATFORM@/$plat/g" "$ROOT/packaging/README.release.md" >"$d/README.md"
  echo "$VERSION" >"$d/VERSION"
  {
    echo "version: $VERSION"
    echo "platform: $plat"
    echo "commit: $COMMIT$DIRTY"
    echo "go: $("$GOBIN" version 2>/dev/null | awk '{print $3}')"
    echo "build: CGO_ENABLED=0 go build -trimpath -ldflags '-s -w -X github.com/jhh3/pyre/internal/buildinfo.Version=$VERSION'"
    if [ -n "$SBOMTOOL" ]; then echo "sbom: sbom/<binary>.cdx.json (CycloneDX 1.6, cyclonedx-gomod ${SBOMVER:-unknown})"; fi
    case "$plat" in darwin-*) echo "macos-signing: ${MACSIGN:-ad-hoc} (Pyre.app = dev.jhh3.pyre)" ;; esac
  } >"$d/BUILDINFO"
  if [ -f "$ROOT/LICENSE" ]; then install -m 0644 "$ROOT/LICENSE" "$d/LICENSE"; fi
  {
    echo "Pyre $VERSION"
    if [ -f "$ROOT/LICENSE" ]; then echo "See LICENSE."; else echo "No LICENSE file is present in the source repository; this is a private build. All rights reserved by the owner."; fi
    echo
    echo "Third-party Go modules compiled into these binaries (go version -m):"
    "$GOBIN" version -m "$src/pyred" "$src/pyre" "$src/pyre-hub" 2>/dev/null | awk '$1 == "dep" {print "  " $2 " " $3}' | sort -u
    if [ -f "$ROOT/NOTICE" ]; then echo; cat "$ROOT/NOTICE"; fi
  } >"$d/NOTICE"
  pack "$STAGE" "$name"
done
# pack_zip BASE NAME: deterministic zip of BASE/NAME into $OUT/NAME.zip (file
# entries only, sorted, DOS mtime = $STAMP in UTC, no extra fields).
pack_zip() {
  local base=$1 name=$2
  (cd "$base/$name" && find . -type f ! -name SHA256SUMS | sed 's|^\./||' | LC_ALL=C sort | while IFS= read -r f; do sha "$f"; done >SHA256SUMS)
  find "$base/$name" -exec env TZ=UTC touch -t "$STAMP" {} +
  rm -f "$OUT/$name.zip"
  (cd "$base" && find "$name" -type f -print | LC_ALL=C sort | TZ=UTC zip -X -D -9 -q "$OUT/$name.zip" -@)
  LIST="$LIST $name.zip"
  echo "==> $OUT/$name.zip"
}
if [ "$WINDOWS" = 1 ]; then
  command -v zip >/dev/null 2>&1 || { echo "zip (Info-ZIP) is required for the Windows artifacts (or pass --no-windows)" >&2; exit 2; }
  for arch in amd64 arm64; do
    plat="windows-$arch"
    name="pyre-$VERSION-$plat"
    src="$ROOT/dist/$name"
    if [ ! -f "$src/pyred.exe" ] || [ ! -f "$src/pyre.exe" ]; then
      # `make dist` covers darwin/linux only; build the same way here.
      src="$STAGE/winbuild/$name"
      mkdir -p "$src"
      echo "==> build $plat (pyre.exe, pyred.exe)"
      for b in pyre pyred; do
        CGO_ENABLED=0 GOOS=windows GOARCH=$arch "$GOBIN" -C "$ROOT" build -trimpath \
          -ldflags "-s -w -X github.com/jhh3/pyre/internal/buildinfo.Version=$VERSION" -o "$src/$b.exe" "./cmd/$b"
      done
    fi
    d="$STAGE/$name"
    mkdir -p "$d/bin" "$d/scripts/install"
    for b in pyre pyred; do install -m 0755 "$src/$b.exe" "$d/bin/$b.exe"; done
    if [ -n "$SBOMTOOL" ]; then
      mkdir -p "$d/sbom"
      for b in pyre pyred; do
        PATH="$GODIR:$PATH" "$SBOMTOOL" bin -json -std -noserial -notimestamp -short-purls \
          -version "$VERSION" -output "$d/sbom/$b.cdx.json" "$d/bin/$b.exe"
        chmod 0644 "$d/sbom/$b.cdx.json"
        sbom_check "$d/bin/$b.exe" "$d/sbom/$b.cdx.json" || { echo "SBOM for $plat/$b is incomplete" >&2; exit 1; }
      done
    fi
    for f in install.ps1 uninstall.ps1; do install -m 0644 "$ROOT/scripts/install/$f" "$d/scripts/install/$f"; done
    sed -e "s/@VERSION@/$VERSION/g" -e "s/@PLATFORM@/$plat/g" "$ROOT/packaging/README.release-windows.md" >"$d/README.md"
    echo "$VERSION" >"$d/VERSION"
    {
      echo "version: $VERSION"
      echo "platform: $plat"
      echo "commit: $COMMIT$DIRTY"
      echo "go: $("$GOBIN" version 2>/dev/null | awk '{print $3}')"
      echo "build: CGO_ENABLED=0 GOOS=windows GOARCH=$arch go build -trimpath -ldflags '-s -w -X github.com/jhh3/pyre/internal/buildinfo.Version=$VERSION'"
      if [ -n "$SBOMTOOL" ]; then echo "sbom: sbom/<binary>.cdx.json (CycloneDX 1.6, cyclonedx-gomod ${SBOMVER:-unknown})"; fi
    } >"$d/BUILDINFO"
    if [ -f "$ROOT/LICENSE" ]; then install -m 0644 "$ROOT/LICENSE" "$d/LICENSE"; fi
    {
      echo "Pyre $VERSION"
      if [ -f "$ROOT/LICENSE" ]; then echo "See LICENSE."; else echo "No LICENSE file is present in the source repository; this is a private build. All rights reserved by the owner."; fi
      echo
      echo "Third-party Go modules compiled into these binaries (go version -m):"
      "$GOBIN" version -m "$src/pyred.exe" "$src/pyre.exe" 2>/dev/null | awk '$1 == "dep" {print "  " $2 " " $3}' | sort -u
      if [ -f "$ROOT/NOTICE" ]; then echo; cat "$ROOT/NOTICE"; fi
    } >"$d/NOTICE"
    pack_zip "$STAGE" "$name"
  done
fi
if [ "$TOOLS" = 1 ]; then
  echo "==> bundled tools (packaging/build-tools.sh)"
  TP=""
  for plat in $PLATFORMS; do TP="$TP ${plat%-*}/${plat#*-}"; done
  GO="$GOBIN" SOURCE_DATE_EPOCH="$EPOCH" bash "$ROOT/packaging/build-tools.sh" --version "$VERSION" --out "$STAGE/tools" --platforms "${TP# }"
  for plat in $PLATFORMS; do
    [ -z "$DIRTY" ] || echo "commit: $COMMIT$DIRTY" >>"$STAGE/tools/pyre-tools-$VERSION-$plat/BUILDINFO"
    pack "$STAGE/tools" "pyre-tools-$VERSION-$plat"
  done
  if [ "$WINDOWS" = 1 ]; then
    # Windows tools: Outfit only (outfit.exe and outfit-core.exe), as a zip
    # for install.ps1 -WithTools.
    GO="$GOBIN" SOURCE_DATE_EPOCH="$EPOCH" bash "$ROOT/packaging/build-tools.sh" --version "$VERSION" --out "$STAGE/tools" --platforms "windows/amd64 windows/arm64"
    for arch in amd64 arm64; do
      [ -z "$DIRTY" ] || echo "commit: $COMMIT$DIRTY" >>"$STAGE/tools/pyre-tools-$VERSION-windows-$arch/BUILDINFO"
      pack_zip "$STAGE/tools" "pyre-tools-$VERSION-windows-$arch"
    done
  fi
fi
# shellcheck disable=SC2086
(cd "$OUT" && sha $LIST >SHA256SUMS)
echo "==> $OUT/SHA256SUMS"
cat "$OUT/SHA256SUMS"
rm -f "$OUT/SHA256SUMS.sig"
if [ -n "$SIGNKEY" ]; then
  # Ed25519 signatures are deterministic: re-signing the same SHA256SUMS gives
  # the same SHA256SUMS.sig, so signed releases stay reproducible.
  ssh-keygen -Y sign -q -f "$SIGNKEY" -n "$NAMESPACE" "$OUT/SHA256SUMS" </dev/null
  ssh-keygen -Y verify -f "$ALLOWED" -I "$NAMESPACE" -n "$NAMESPACE" -s "$OUT/SHA256SUMS.sig" <"$OUT/SHA256SUMS" >/dev/null ||
    { rm -f "$OUT/SHA256SUMS.sig"; echo "the signature does not verify against $ALLOWED: wrong signing key?" >&2; exit 1; }
  echo "==> $OUT/SHA256SUMS.sig (verified against $(basename "$ALLOWED"), key $(ssh-keygen -l -f "$SIGNKEY" 2>/dev/null | awk '{print $2}'))"
else
  echo "warning: release is UNSIGNED (no signing key); upgrade.sh refuses it on hosts that pinned the release key" >&2
fi
[ -z "$DIRTY" ] || echo "warning: built from a working tree with uncommitted changes" >&2
