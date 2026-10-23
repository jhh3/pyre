#!/bin/bash
# Enroll a remote machine as a Pyre node over an existing ssh alias.
# Dry run by default: prints the exact scp/ssh plan and creates no ticket.
#
#   bootstrap-node.sh --ssh ALIAS [--name NAME] [--hub-url URL]
#                     [--dist DIR | --pyred FILE] [--target OS/ARCH] [--probe]
#                     [--remote-dir DIR] [--execute]
#
# Wraps `pyre enroll-node --name NAME --ssh ALIAS --bootstrap [--execute]`
# (internal/enrollment SSHBootstrapPlan). The plan:
#   1. ssh ALIAS 'umask 077 && mkdir -p ~/.local/share/pyre/bin'
#   2. scp <pyred for the target> ALIAS:.local/share/pyre/bin/.pyred.new
#   3. ssh ALIAS 'chmod 700 ... && mv -f ... && pyred version'
#   4. ssh ALIAS 'pyred enroll --hub URL --pin SHA --node-id ID
#      --credential-file ~/.local/share/pyre/node-credential.json --secret-stdin'
#      (the one-time secret travels on ssh stdin, never in argv)
# Host keys are verified by ssh as usual. Nothing is installed as a service.
# A Windows target (--target windows/ARCH, or found by --probe) gets the
# PowerShell form of the same steps, installing bin\pyred.exe and
# node-credential.json in %LOCALAPPDATA%\Programs\Pyre (install.ps1's prefix).
#
# --hub-url is the hub URL as reachable FROM THE NODE (for a hub behind an
# ssh -L forward this is the node-side forward, e.g. https://127.0.0.1:7443).
# --dist DIR picks DIR/pyre-*-<os>-<arch>/pyred (a `make dist` tree, or a
# release tree with bin/pyred; pyred.exe for Windows). --target OS/ARCH names
# the platform; --probe asks the host instead (`ssh ALIAS uname -sm`, else a
# PowerShell probe of PROCESSOR_ARCHITECTURE; read-only).
#
# After enrollment, install the service on the node (see docs/operations.md):
#   copy a release tarball, then on the node:
#   scripts/install/install.sh --tarball pyre-<v>-<os>-<arch>.tar.gz --service node --name NAME --ssh-alias ALIAS
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)

usage() { sed -n '2,31p' "$0" | sed 's/^# \{0,1\}//'; exit 2; }
ALIAS="" NAME="" HUB_URL="" DIST="" PYRED="" TARGET="" PROBE=0 REMOTE_DIR="" EXECUTE=0
PYRE=${PYRE:-pyre}
while [ $# -gt 0 ]; do
  case "$1" in
    --ssh) ALIAS=${2:?}; shift 2 ;;
    --name) NAME=${2:?}; shift 2 ;;
    --hub-url) HUB_URL=${2:?}; shift 2 ;;
    --dist) DIST=${2:?}; shift 2 ;;
    --pyred) PYRED=${2:?}; shift 2 ;;
    --target) TARGET=${2:?}; shift 2 ;;
    --probe) PROBE=1; shift ;;
    --remote-dir) REMOTE_DIR=${2:?}; shift 2 ;;
    --execute) EXECUTE=1; shift ;;
    -h|--help) usage ;;
    *) echo "unknown argument: $1" >&2; usage ;;
  esac
done
[ -n "$ALIAS" ] || usage
case "$ALIAS" in *[!A-Za-z0-9._@-]*|-*) echo "error: --ssh must be a plain alias from your ssh config" >&2; exit 2 ;; esac
[ -n "$NAME" ] || NAME=$ALIAS

if [ -z "$PYRED" ]; then
  if [ -z "$TARGET" ] && [ "$PROBE" = 1 ]; then
    ssh -o BatchMode=yes "$ALIAS" exit 0 >/dev/null 2>&1 || { echo "error: cannot reach $ALIAS over ssh (ssh -o BatchMode=yes $ALIAS exit 0 failed)" >&2; exit 1; }
    um=$(ssh -o BatchMode=yes "$ALIAS" uname -sm 2>/dev/null || true)
    case "$um" in
      "Linux x86_64") TARGET=linux/amd64 ;; "Linux aarch64") TARGET=linux/arm64 ;;
      "Darwin arm64") TARGET=darwin/arm64 ;; "Darwin x86_64") TARGET=darwin/amd64 ;;
      MINGW*|MSYS*|CYGWIN*|Windows*|*_NT*|"")
        # Windows OpenSSH (cmd.exe or PowerShell login shell: no uname).
        wa=$(ssh -o BatchMode=yes "$ALIAS" powershell -NoProfile -NonInteractive -Command '$env:PROCESSOR_ARCHITECTURE' 2>/dev/null | tr -d '\r' || true)
        case "$wa" in
          AMD64) TARGET=windows/amd64 ;; ARM64) TARGET=windows/arm64 ;;
          *) echo "error: unsupported remote platform '${um:-no uname}${wa:+ / $wa}'" >&2; exit 1 ;;
        esac ;;
      *) echo "error: unsupported remote platform '$um'" >&2; exit 1 ;;
    esac
    echo "==> $ALIAS is $TARGET"
  fi
  if [ -n "$TARGET" ]; then
    [ -n "$DIST" ] || DIST="$HERE/../../dist"
    os=${TARGET%/*}; arch=${TARGET#*/}
    exe=""
    [ "$os" != windows ] || exe=.exe
    for d in "$DIST"/pyre-*-"$os-$arch"; do
      if [ -f "$d/bin/pyred$exe" ]; then PYRED="$d/bin/pyred$exe"; elif [ -f "$d/pyred$exe" ]; then PYRED="$d/pyred$exe"; fi
    done
    [ -n "$PYRED" ] || { echo "error: no pyred for $TARGET under $DIST (run make dist or packaging/release.sh)" >&2; exit 1; }
  fi
fi

set -- enroll-node --name "$NAME" --ssh "$ALIAS" --bootstrap
case "$TARGET" in windows/*) set -- "$@" --target-os windows ;; ?*) set -- "$@" --target-os unix ;; esac
[ -z "$PYRED" ] || set -- "$@" --pyred "$PYRED"
[ -z "$HUB_URL" ] || set -- "$@" --hub-url "$HUB_URL"
[ -z "$REMOTE_DIR" ] || set -- "$@" --remote-dir "$REMOTE_DIR"
if [ "$EXECUTE" = 1 ]; then
  set -- "$@" --execute
  echo "==> creating a one-time ticket and running the plan against $ALIAS"
else
  echo "==> dry run (no ticket, nothing contacted); add --execute to enroll $ALIAS"
fi
echo "+ $PYRE $*"
"$PYRE" "$@"
if [ "$EXECUTE" = 1 ] && [ "${TARGET%%/*}" = windows ]; then
  cat <<EOF
==> next on $ALIAS (requires owner authorization for a real host):
    1. copy pyre-<v>-$(printf %s "$TARGET" | tr / -).zip, SHA256SUMS, SHA256SUMS.sig (and the tools zip for -WithTools)
    2. powershell -NoProfile -ExecutionPolicy Bypass -File scripts\install\install.ps1 -Zip <zip> -AllowedSigners <file> -Service node -Name $NAME -SshAlias $ALIAS
       (uses %LOCALAPPDATA%\Programs\Pyre\node-credential.json written by the bootstrap)
    3. verify from here: pyre nodes show $NAME
EOF
elif [ "$EXECUTE" = 1 ]; then
  cat <<EOF
==> next on $ALIAS (requires owner authorization for a real host):
    1. copy a release tarball for its platform and the SHA256SUMS file
    2. scripts/install/install.sh --tarball <tarball> --service node --name $NAME --ssh-alias $ALIAS
       (uses ~/.local/share/pyre/node-credential.json written by the bootstrap)
    3. Linux: loginctl enable-linger \$USER so runs survive logout
    4. verify from here: pyre nodes show $NAME
EOF
fi
