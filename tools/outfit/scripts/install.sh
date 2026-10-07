#!/bin/sh
set -eu
case "${OUTFIT_UNAME_S:-$(uname -s)}" in Darwin) os=darwin;; Linux) os=linux;; *) echo 'Use the release zip on Windows.' >&2; exit 1;; esac
case "${OUTFIT_UNAME_M:-$(uname -m)}" in x86_64|amd64) arch=amd64;; arm64|aarch64) arch=arm64;; *) echo 'Unsupported architecture.' >&2; exit 1;; esac
asset_pattern="outfit_*_${os}_${arch}.tar.gz"
if [ "${OUTFIT_INSTALL_ASSET_ONLY:-0}" = 1 ]; then
  printf '%s\n' "$asset_pattern"
  exit 0
fi
command -v gh >/dev/null 2>&1 || { echo 'gh is required to download this private repository release' >&2; exit 1; }
destination="${OUTFIT_INSTALL_DIR:-$HOME/.local/bin}"
mkdir -p "$destination"
temporary="$(mktemp -d)"; trap 'rm -rf "$temporary"' EXIT
if [ -n "${OUTFIT_VERSION:-}" ]; then
  gh release download "$OUTFIT_VERSION" --repo jhh3/outfit --pattern "$asset_pattern" --pattern checksums.txt --dir "$temporary"
else
  gh release download --repo jhh3/outfit --pattern "$asset_pattern" --pattern checksums.txt --dir "$temporary"
fi
archive=''
for candidate in "$temporary"/outfit_*_"$os"_"$arch".tar.gz; do
  [ -f "$candidate" ] || continue
  [ -z "$archive" ] || { echo "More than one release archive matched $asset_pattern" >&2; exit 1; }
  archive="$candidate"
done
[ -n "$archive" ] || { echo "No release archive matched $asset_pattern" >&2; exit 1; }
archive_name="$(basename "$archive")"
expected="$(awk -v name="$archive_name" '$2 == name { print $1 }' "$temporary/checksums.txt")"
[ -n "$expected" ] || { echo "No checksum found for $archive_name" >&2; exit 1; }
if command -v sha256sum >/dev/null 2>&1; then
  actual="$(sha256sum "$archive" | awk '{ print $1 }')"
elif command -v shasum >/dev/null 2>&1; then
  actual="$(shasum -a 256 "$archive" | awk '{ print $1 }')"
else
  echo 'sha256sum or shasum is required to verify the release' >&2
  exit 1
fi
[ "$actual" = "$expected" ] || { echo "Checksum verification failed for $archive_name" >&2; exit 1; }
tar -xzf "$archive" -C "$temporary"
install -m 0755 "$temporary/outfit" "$destination/outfit-core"
ln -sf outfit-core "$destination/outfit"
if [ -L "$destination/cw" ] && [ "$(readlink "$destination/cw")" = cw-core ]; then
  rm -f "$destination/cw"
fi
rm -f "$destination/cw-core"
echo "Installed outfit and outfit-core to $destination"
echo "Run: outfit setup && outfit plugin install"
