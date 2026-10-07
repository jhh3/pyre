#!/bin/bash
# Reproducibility check: build the release twice and compare every artifact.
#
#   packaging/verify-repro.sh [--version V] [--keep DIR] [release.sh flags...]
#
# Build A uses the normal Go build cache; build B uses an empty GOCACHE, so B
# really recompiles everything. Each build starts from deleted
# dist/pyre-<V>-* directories and writes its tarballs to its own directory.
# The two dist/SHA256SUMS (all binaries) and every file in the two release
# directories (tarballs, SHA256SUMS, SHA256SUMS.sig if signed) must be
# byte-identical (cmp). Extra flags go to release.sh (default: --no-sign, so
# no key is needed; pass --signing-key FILE to also compare signatures).
# Exit 0 identical, 1 different or failed. dist/release/ is not touched.
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/.." && pwd)
VERSION="" KEEP="" PASS=()
SIGNFLAG=--no-sign
while [ $# -gt 0 ]; do
  case "$1" in
    --version) VERSION=${2:?}; shift 2 ;;
    --keep) KEEP=${2:?}; shift 2 ;;
    --signing-key) SIGNFLAG=""; PASS+=("$1" "${2:?}"); shift 2 ;;
    -h|--help) sed -n '2,15p' "$0"; exit 0 ;;
    *) PASS+=("$1"); shift ;;
  esac
done
[ -n "$VERSION" ] || VERSION=$(git -C "$ROOT" describe --tags --always --dirty 2>/dev/null || echo dev)
case "$VERSION" in *[!A-Za-z0-9._+-]*|"") echo "invalid version '$VERSION'" >&2; exit 2 ;; esac
WORK=$(mktemp -d "${TMPDIR:-/tmp}/pyre-repro.XXXXXX")
cleanup() { if [ -z "$KEEP" ]; then rm -rf "$WORK"; fi; }
trap cleanup EXIT
[ -z "$KEEP" ] || { mkdir -p "$KEEP"; rm -rf "$WORK"; WORK=$KEEP; }

build() { # NAME [env...]
  local name=$1; shift
  rm -rf "$ROOT"/dist/pyre-"$VERSION"-*
  mkdir -p "$WORK/$name"
  echo "==> build $name"
  env "$@" bash "$ROOT/packaging/release.sh" --version "$VERSION" --out "$WORK/$name/release" \
    ${SIGNFLAG:+"$SIGNFLAG"} ${PASS[@]+"${PASS[@]}"} >"$WORK/$name/build.log" 2>&1 ||
    { tail -n 30 "$WORK/$name/build.log" >&2; echo "build $name failed" >&2; exit 1; }
  # make dist hashes everything under dist/*/ (also dist/release, dist/android);
  # compare only this version's binaries.
  grep "  pyre-$VERSION-" "$ROOT/dist/SHA256SUMS" >"$WORK/$name/dist-SHA256SUMS"
}

build A PYRE_REPRO_BUILD=A
mkdir -p "$WORK/gocache"
build B PYRE_REPRO_BUILD=B GOCACHE="$WORK/gocache"

rc=0
if cmp "$WORK/A/dist-SHA256SUMS" "$WORK/B/dist-SHA256SUMS"; then
  echo "binaries: identical ($(wc -l <"$WORK/A/dist-SHA256SUMS" | tr -d ' ') files)"
else
  echo "binaries: DIFFERENT" >&2; diff "$WORK/A/dist-SHA256SUMS" "$WORK/B/dist-SHA256SUMS" >&2 || true; rc=1
fi
n=0
for f in "$WORK"/A/release/*; do
  b=$(basename "$f")
  if [ ! -f "$WORK/B/release/$b" ]; then echo "missing in build B: $b" >&2; rc=1; continue; fi
  if cmp -s "$f" "$WORK/B/release/$b"; then n=$((n + 1)); else echo "DIFFERENT: $b" >&2; rc=1; fi
done
for f in "$WORK"/B/release/*; do
  [ -f "$WORK/A/release/$(basename "$f")" ] || { echo "missing in build A: $(basename "$f")" >&2; rc=1; }
done
echo "release files identical: $n"
cat "$WORK/A/release/SHA256SUMS"
if [ "$rc" = 0 ]; then echo "==> reproducible: $VERSION builds A and B are byte-identical"; else echo "==> NOT reproducible" >&2; fi
exit "$rc"
