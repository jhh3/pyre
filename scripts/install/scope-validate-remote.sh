#!/bin/bash
# Build linux/amd64 Pyre binaries and test binaries, copy them with the
# install scripts/templates to <alias>:~/.cache/pyre-sk, and run
# scope-validate.sh there (transient user units only; see that script).
#
#   scripts/install/scope-validate-remote.sh <ssh-alias>
#
# Uses one Claude run with the host's default login (three tiny turns).
set -euo pipefail
ALIAS=${1:?usage: $0 <ssh-alias>}
case "$ALIAS" in *[!A-Za-z0-9._@-]*|-*) echo "bad alias" >&2; exit 2 ;; esac
R=$(cd "$(dirname "$0")/../.." && pwd)
GO=${GO:-}
if [ -z "$GO" ]; then
  if [ -x "$HOME/sdk/go1.27.1/bin/go" ]; then GO="$HOME/sdk/go1.27.1/bin/go"; else GO=go; fi  # same rule as the Makefile
fi
O=$(mktemp -d "${TMPDIR:-/tmp}/pyre-sk.XXXXXX")
trap 'rm -rf "$O"' EXIT
V=$(git -C "$R" describe --tags --always --dirty)
for b in pyre pyred pyre-hub; do
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 "$GO" -C "$R" build -trimpath -ldflags "-s -w -X github.com/jhh3/pyre/internal/buildinfo.Version=$V" -o "$O/bin/$b" "./cmd/$b"
done
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 "$GO" -C "$R" test -c -o "$O/bin/supervisor.test" ./internal/supervisor
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 "$GO" -C "$R" test -c -o "$O/bin/tmux.test" ./internal/runtime/tmux
cp "$R/scripts/install/keydump.py" "$R/scripts/install/scope-validate.sh" "$O/bin/"
mkdir -p "$O/src/scripts"
cp -R "$R/scripts/install" "$O/src/scripts/"
cp -R "$R/packaging" "$O/src/"
ssh -o BatchMode=yes "$ALIAS" mkdir -p .cache/pyre-sk
scp -q -r "$O/bin" "$O/src" "$ALIAS:.cache/pyre-sk/"
echo "==> $V copied to $ALIAS:~/.cache/pyre-sk; running scope-validate.sh"
rc=0
ssh -o BatchMode=yes "$ALIAS" bash -l .cache/pyre-sk/bin/scope-validate.sh || rc=$?
mkdir -p "$R/dist/scope-validate"
scp -q -r "$ALIAS:.cache/pyre-sk/out" "$R/dist/scope-validate/$ALIAS-$(date +%Y%m%dT%H%M%S)" || true
ssh -o BatchMode=yes "$ALIAS" rm -rf .cache/pyre-sk
exit "$rc"
