#!/bin/bash
# Failure-path test for release signatures in scripts/install/install.sh
# (first install) and scripts/install/upgrade.sh.
#
#   packaging/test-release-integrity.sh [--release DIR]
#
# Takes the host platform's tarball from a built release (default
# dist/release, from packaging/release.sh) and re-signs copies of its
# SHA256SUMS with throwaway keys generated here (never the owner's key).
# Everything happens in scratch HOMEs (--home, no service manager).
# install.sh: a tampered SHA256SUMS, a tampered tarball, a signature by
# another key, a missing signature and a signature with no trusted key are
# refused with exit 3 and install nothing; a good signature installs and
# pins the key, after which a conflicting --allowed-signers is refused.
# upgrade.sh: a tampered SHA256SUMS, a signature by another key, a missing
# signature once a key is pinned, a conflicting --allowed-signers and an
# unverifiable --from directory are refused with exit 3 and leave `current`
# unchanged; a good signature passes and pins the key.
set -uo pipefail
ROOT=$(cd "$(dirname "$0")/.." && pwd)
REL="$ROOT/dist/release"
while [ $# -gt 0 ]; do
  case "$1" in
    --release) REL=${2:?}; shift 2 ;;
    *) sed -n '2,18p' "$0"; exit 2 ;;
  esac
done
case "$(uname -s)" in Darwin) os=darwin ;; Linux) os=linux ;; *) echo "unsupported OS" >&2; exit 2 ;; esac
case "$(uname -m)" in arm64|aarch64) arch=arm64 ;; *) arch=amd64 ;; esac
TB=$(ls "$REL"/pyre-*-"$os-$arch".tar.gz 2>/dev/null | grep -v "/pyre-tools-" | head -n 1)
[ -n "$TB" ] && [ -f "$REL/SHA256SUMS" ] || { echo "no $os-$arch tarball + SHA256SUMS in $REL (run packaging/release.sh first)" >&2; exit 2; }
command -v ssh-keygen >/dev/null || { echo "ssh-keygen required" >&2; exit 2; }

W=$(mktemp -d "${TMPDIR:-/tmp}/pyre-sigtest.XXXXXX")
trap 'rm -rf "$W"' EXIT
H="$W/home"
mkdir -p "$H"
NS=pyre-release
for k in trusted other; do
  ssh-keygen -q -t ed25519 -N "" -C "synthetic-$k" -f "$W/$k.key"
  read -r kt kb _ <"$W/$k.key.pub"
  printf 'pyre-release namespaces="pyre-release" %s %s synthetic-%s\n' "$kt" "$kb" "$k" >"$W/$k.allowed"
done
mkrel() { # NAME SIGNKEY|none
  mkdir -p "$W/$1"
  cp "$TB" "$W/$1/"
  cp "$REL/SHA256SUMS" "$W/$1/SHA256SUMS"
  if [ "$2" != none ]; then ssh-keygen -Y sign -q -f "$W/$2.key" -n "$NS" "$W/$1/SHA256SUMS" </dev/null; fi
}
mkrel good trusted
mkrel wrongkey other
mkrel unsigned none
mkrel tampered trusted
echo "0000000000000000000000000000000000000000000000000000000000000000  pyre-evil.tar.gz" >>"$W/tampered/SHA256SUMS"
B=$(basename "$TB")
# A tarball whose bytes changed after signing (SHA256SUMS and signature untouched).
mkrel tampered-tarball trusted
printf 'synthetic tampering\n' >>"$W/tampered-tarball/$B"

INST="$ROOT/scripts/install"
fails=0 n=0

# ---- install.sh (first install, each case in a fresh scratch home) ----
iexpect() { # WANT_EXIT DESCRIPTION -- install.sh args...
  local want=$1 desc=$2 got hh
  shift 3
  n=$((n + 1))
  hh="$W/ihome$n"
  mkdir -p "$hh"
  bash "$INST/install.sh" --home "$hh" "$@" >"$W/case$n.log" 2>&1
  got=$?
  if [ "$got" != "$want" ]; then
    echo "FAIL $n  exit $got (want $want)  install: $desc"; sed 's/^/       /' "$W/case$n.log"; fails=$((fails + 1)); return
  fi
  if [ "$want" = 3 ] && [ -e "$hh/.local/share/pyre" -o -e "$hh/.config/pyre/release-allowed-signers" ]; then
    echo "FAIL $n  refused but left files behind  install: $desc"; fails=$((fails + 1)); return
  fi
  echo "ok   $n  exit $got  install: $desc"
}
iexpect 3 "tampered SHA256SUMS (--allowed-signers)" -- --tarball "$W/tampered/$B" --allowed-signers "$W/trusted.allowed"
iexpect 3 "tampered tarball, signature intact (--allowed-signers)" -- --tarball "$W/tampered-tarball/$B" --allowed-signers "$W/trusted.allowed"
grep -q "tarball checksum mismatch" "$W/case$n.log" || { echo "FAIL     no checksum-mismatch message"; fails=$((fails + 1)); }
iexpect 3 "tampered tarball even with --allow-unsigned" -- --tarball "$W/tampered-tarball/$B" --allowed-signers "$W/trusted.allowed" --allow-unsigned
iexpect 3 "signature by another key (--allowed-signers)" -- --tarball "$W/wrongkey/$B" --allowed-signers "$W/trusted.allowed"
iexpect 3 "signature by another key even with --allow-unsigned" -- --tarball "$W/wrongkey/$B" --allowed-signers "$W/trusted.allowed" --allow-unsigned
iexpect 3 "unsigned release" -- --tarball "$W/unsigned/$B" --allowed-signers "$W/trusted.allowed"
iexpect 3 "signed by a key the source checkout does not trust (no --allowed-signers)" -- --tarball "$W/good/$B"
iexpect 0 "unsigned release with --allow-unsigned (nothing pinned)" -- --tarball "$W/unsigned/$B" --allow-unsigned
[ ! -e "$W/ihome$n/.config/pyre/release-allowed-signers" ] || { echo "FAIL     an unsigned install pinned a key"; fails=$((fails + 1)); }
iexpect 0 "good signature with --allowed-signers: verified and pinned" -- --tarball "$W/good/$B" --allowed-signers "$W/trusted.allowed"
GH="$W/ihome$n"
grep -q "release signature verified" "$W/case$n.log" || { echo "FAIL     no verification note"; fails=$((fails + 1)); }
cmp -s "$GH/.config/pyre/release-allowed-signers" "$W/trusted.allowed" || { echo "FAIL     key not pinned by the verified install"; fails=$((fails + 1)); }
n=$((n + 1))
bash "$INST/install.sh" --home "$GH" --tarball "$W/good/$B" --allowed-signers "$W/other.allowed" >"$W/case$n.log" 2>&1
if [ $? = 3 ]; then echo "ok   $n  exit 3  install: --allowed-signers conflicting with the pin"; else echo "FAIL $n  install: conflicting --allowed-signers accepted"; fails=$((fails + 1)); fi
n=$((n + 1))
bash "$INST/install.sh" --home "$GH" --tarball "$W/good/$B" >"$W/case$n.log" 2>&1
if [ $? = 0 ] && grep -q "(pinned)" "$W/case$n.log"; then echo "ok   $n  exit 0  install: re-install verified against the pin"; else echo "FAIL $n  install: re-install against the pin"; sed 's/^/       /' "$W/case$n.log"; fails=$((fails + 1)); fi

# ---- upgrade.sh (one scratch home, baseline install from the unsigned copy: nothing pinned) ----
bash "$INST/install.sh" --tarball "$W/unsigned/$B" --allow-unsigned --home "$H" >"$W/install.log" 2>&1 || { cat "$W/install.log"; echo "baseline install failed" >&2; exit 1; }
CUR0=$(readlink "$H/.local/share/pyre/current")
PIN="$H/.config/pyre/release-allowed-signers"
expect() { # WANT_EXIT DESCRIPTION -- upgrade.sh args...
  local want=$1 desc=$2 got cur
  shift 3
  n=$((n + 1))
  bash "$INST/upgrade.sh" --home "$H" --service none --skip-hub-check "$@" >"$W/case$n.log" 2>&1
  got=$?
  cur=$(readlink "$H/.local/share/pyre/current")
  if [ "$got" = "$want" ] && [ "$cur" = "$CUR0" ]; then
    echo "ok   $n  exit $got  $desc"
  else
    echo "FAIL $n  exit $got (want $want), current=$cur  $desc"; sed 's/^/       /' "$W/case$n.log"; fails=$((fails + 1))
  fi
}
pinned() { if [ -f "$PIN" ]; then echo yes; else echo no; fi; }

expect 0 "unsigned release, nothing pinned: accepted with a warning" -- --tarball "$W/unsigned/$B"
grep -q "unsigned release and no pinned release key" "$W/case$n.log" || { echo "FAIL     missing unsigned warning"; fails=$((fails + 1)); }
expect 0 "signed release, nothing pinned: accepted, signature NOT verified (warning)" -- --tarball "$W/good/$B"
grep -q "signature was NOT verified" "$W/case$n.log" || { echo "FAIL     missing not-verified warning"; fails=$((fails + 1)); }
expect 3 "tampered SHA256SUMS (--allowed-signers)" -- --tarball "$W/tampered/$B" --allowed-signers "$W/trusted.allowed"
expect 3 "signature by another key (--allowed-signers)" -- --tarball "$W/wrongkey/$B" --allowed-signers "$W/trusted.allowed"
[ "$(pinned)" = no ] || { echo "FAIL     a failed verification pinned a key"; fails=$((fails + 1)); }
expect 0 "good signature with --allowed-signers: verified and pinned" -- --tarball "$W/good/$B" --allowed-signers "$W/trusted.allowed"
[ "$(pinned)" = yes ] || { echo "FAIL     key not pinned after a verified upgrade"; fails=$((fails + 1)); }
grep -q "release signature verified" "$W/case$n.log" || { echo "FAIL     no verification note"; fails=$((fails + 1)); }
expect 3 "unsigned release while a key is pinned" -- --tarball "$W/unsigned/$B"
expect 0 "unsigned release while pinned with --allow-unsigned" -- --tarball "$W/unsigned/$B" --allow-unsigned
expect 3 "signature by another key (pinned)" -- --tarball "$W/wrongkey/$B"
expect 3 "wrong key even with --allow-unsigned" -- --tarball "$W/wrongkey/$B" --allow-unsigned
expect 3 "tampered SHA256SUMS (pinned)" -- --tarball "$W/tampered/$B"
expect 3 "--allowed-signers conflicting with the pin" -- --tarball "$W/wrongkey/$B" --allowed-signers "$W/other.allowed"
mkdir -p "$W/extracted"
tar -xzf "$TB" -C "$W/extracted"
expect 3 "--from directory while a key is pinned" -- --from "$W/extracted/${B%.tar.gz}"
expect 0 "good signature (pinned)" -- --tarball "$W/good/$B"
echo "$((n - fails))/$n cases passed"
[ "$fails" = 0 ]
