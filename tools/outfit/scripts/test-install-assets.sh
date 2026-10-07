#!/bin/sh
set -eu

assert_asset() {
  actual="$(OUTFIT_UNAME_S="$1" OUTFIT_UNAME_M="$2" OUTFIT_INSTALL_ASSET_ONLY=1 sh scripts/install.sh)"
  if [ "$actual" != "$3" ]; then
    echo "expected $3 for $1/$2, got $actual" >&2
    exit 1
  fi
}

assert_asset Darwin arm64 'outfit_*_darwin_arm64.tar.gz'
assert_asset Darwin x86_64 'outfit_*_darwin_amd64.tar.gz'
assert_asset Linux aarch64 'outfit_*_linux_arm64.tar.gz'
assert_asset Linux amd64 'outfit_*_linux_amd64.tar.gz'
