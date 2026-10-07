#!/bin/bash
# Builds the release APK signed with the owner's key from
# ~/.config/pyre/android-signing.properties (never printed) and prints the
# signer certificate SHA-256, package/version and APK size.
#   bash android/scripts/release-verify.sh [-PpyreVersionCode=N -PpyreVersionName=X]
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
ANDROID="$(dirname "$HERE")"
SDK="$(sed -n 's/^sdk.dir=//p' "$ANDROID/local.properties" 2>/dev/null || true)"
SDK="${SDK:-${ANDROID_HOME:-$HOME/Library/Android/sdk}}"
BT="$(ls -d "$SDK"/build-tools/* | sort -V | tail -1)"
[ -f "$HOME/.config/pyre/android-signing.properties" ] || { echo "no signing properties"; exit 1; }
"$ANDROID/gradlew" -p "$ANDROID" --console=plain -q assembleRelease "$@"
APK="$ANDROID/app/build/outputs/apk/release/app-release.apk"
"$BT/apksigner" verify --print-certs "$APK" | grep -E "Signer #1 certificate (DN|SHA-256)"
"$BT/aapt2" dump badging "$APK" | grep -E "^package:|targetSdkVersion"
echo "APK size bytes: $(stat -f %z "$APK" 2>/dev/null || stat -c %s "$APK")"
echo "firebase entries: $(unzip -l "$APK" | grep -c firebase || true)"
