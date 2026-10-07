#!/bin/bash
# Driver (runs on the build Mac) for the real-host release update test (A16):
#
#   tests/e2e/release_update.sh build                 throwaway signing key + four signed test releases
#   tests/e2e/release_update.sh build-handoff [B U]   four releases for the in-product pyred update test:
#                                                     B (launcher, default rc8), U (update, default rc9),
#                                                     Unohub (starts but cannot reach its hub),
#                                                     Ucrash (pyred run exits non-zero at once)
#   tests/e2e/release_update.sh upload HOST PLATFORM   copy the host's tarballs + kit to HOST:~/.cache/$WUPD_NAME
#   tests/e2e/release_update.sh run HOST ARGS...       run tests/e2e/release_update_host.sh ARGS on HOST
#
# WUPD_NAME (default pyre-wupdates) names the work directory under ~/.cache
# on the host; WUPD_RELS (default "rc2 rc3 rc3bad proto2") lists the release
# directories to upload.
#
# Releases (packaging/release.sh, all signed with a throwaway Ed25519 key that
# is generated here and pinned on the test installs; never the owner's key):
#   0.1.0-rc2-test        baseline
#   0.1.0-rc3-test        the update
#   0.1.0-rc3bad-test     same tooling, but `go build -overlay` makes `pyred run`
#                         exit at once (forced health failure)
#   0.1.0-rc4proto2-test  same tooling, `-overlay` raises the protocol to 2..2
#                         (protocol-incompatible with an rc2/rc3 hub)
# The overlays are passed through GOFLAGS to `make dist`; nothing is committed.
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
OUT=${WUPD_OUT:?set WUPD_OUT to a scratch directory for the releases}
NAME=${WUPD_NAME:-pyre-wupdates}
RELS=${WUPD_RELS:-rc2 rc3 rc3bad proto2}
case "$NAME" in pyre-w*) ;; *) echo "WUPD_NAME must start with pyre-w" >&2; exit 2 ;; esac
cmd=${1:?build|build-handoff|upload|run}; shift
key() {
  mkdir -p "$OUT/key" "$OUT/overlay"
  K="$OUT/key/sign.key" A="$OUT/key/allowed-signers"
  if [ ! -f "$K" ]; then
    ssh-keygen -q -t ed25519 -N "" -C synthetic-wupdates-test -f "$K"
    read -r kt kb _ <"$K.pub"
    printf 'pyre-release namespaces="pyre-release" %s %s synthetic-wupdates-test\n' "$kt" "$kb" >"$A"
  fi
}
rel() { # DIR VERSION [GOFLAGS]
  rm -rf "${OUT:?}/$1"
  GOFLAGS="${3:-}" bash "$ROOT/packaging/release.sh" --version "$2" --out "$OUT/$1" --signing-key "$K" --allowed-signers "$A" >"$OUT/$1.build.log" 2>&1 ||
    { tail -n 30 "$OUT/$1.build.log"; exit 1; }
  echo "== $2 -> $OUT/$1"; grep -E "SHA256SUMS.sig|warning" "$OUT/$1.build.log" || true
}

case "$cmd" in
build)
  key
  # Forced health failure: pyred run refuses to start.
  python3 - "$ROOT/cmd/pyred/run.go" "$OUT/overlay/run_bad.go" <<'PY'
import sys
src, dst = sys.argv[1], sys.argv[2]
s = open(src).read()
old = "\tif err := fs.Parse(args); err != nil {\n\t\treturn err\n\t}\n\treturn runDaemon(o)\n"
assert s.count(old) == 1
s = s.replace(old, "\tif err := fs.Parse(args); err != nil {\n\t\treturn err\n\t}\n\t_ = o\n\treturn errors.New(\"SYNTHETIC test build 0.1.0-rc3bad-test: pyred run refuses to start (forced health failure)\")\n")
open(dst, "w").write(s)
PY
  printf '{"Replace":{"%s":"%s"}}\n' "$ROOT/cmd/pyred/run.go" "$OUT/overlay/run_bad.go" >"$OUT/overlay/bad.json"
  # Protocol 2..2.
  sed -e 's/^const ProtocolVersion = 1$/const ProtocolVersion = 2/' -e 's/^const MinProtocolVersion = 1$/const MinProtocolVersion = 2/' \
    "$ROOT/internal/contracts/doc.go" >"$OUT/overlay/doc_proto2.go"
  grep -q '^const MinProtocolVersion = 2$' "$OUT/overlay/doc_proto2.go"
  printf '{"Replace":{"%s":"%s"}}\n' "$ROOT/internal/contracts/doc.go" "$OUT/overlay/doc_proto2.go" >"$OUT/overlay/proto2.json"
  rel rc2 0.1.0-rc2-test
  rel rc3 0.1.0-rc3-test
  rel rc3bad 0.1.0-rc3bad-test "-overlay=$OUT/overlay/bad.json"
  rel proto2 0.1.0-rc4proto2-test "-overlay=$OUT/overlay/proto2.json"
  ;;
build-handoff)
  B=${1:-rc8} U=${2:-rc9}
  key
  # A daemon that starts and serves its socket but can never reach its hub.
  python3 - "$ROOT/cmd/pyred/run.go" "$OUT/overlay/run_nohub.go" <<'PY'
import sys
src, dst = sys.argv[1], sys.argv[2]
s = open(src).read()
old = "\t\tif hubURL == \"\" {\n\t\t\thubURL = cred.HubURL\n\t\t}\n"
assert s.count(old) == 1
s = s.replace(old, "\t\t// SYNTHETIC test build (nohub): the hub is unreachable.\n\t\thubURL = \"https://127.0.0.1:9\"\n")
open(dst, "w").write(s)
PY
  printf '{"Replace":{"%s":"%s"}}\n' "$ROOT/cmd/pyred/run.go" "$OUT/overlay/run_nohub.go" >"$OUT/overlay/nohub.json"
  # A daemon that exits non-zero at once (crash before health): only the
  # service manager's restart policy brings the launcher back to roll it back.
  python3 - "$ROOT/cmd/pyred/run.go" "$OUT/overlay/run_crash.go" <<'PY'
import sys
src, dst = sys.argv[1], sys.argv[2]
s = open(src).read()
old = "\terr := runDaemon(o)\n"
assert s.count(old) == 1
s = s.replace(old, "\t_ = runDaemon\n\terr := errors.New(\"SYNTHETIC test build (crash): pyred run exits at once\")\n")
open(dst, "w").write(s)
PY
  printf '{"Replace":{"%s":"%s"}}\n' "$ROOT/cmd/pyred/run.go" "$OUT/overlay/run_crash.go" >"$OUT/overlay/crash.json"
  rel "$B" "0.1.0-$B-test"
  rel "$U" "0.1.0-$U-test"
  rel "${U}nohub" "0.1.0-${U}nohub-test" "-overlay=$OUT/overlay/nohub.json"
  rel "${U}crash" "0.1.0-${U}crash-test" "-overlay=$OUT/overlay/crash.json"
  ;;
upload)
  HOST=${1:?host}; PLAT=${2:?platform}
  S="$OUT/stage-$HOST"
  rm -rf "$S"; mkdir -p "$S/$NAME/rel" "$S/$NAME/kit"
  for r in $RELS; do
    mkdir -p "$S/$NAME/rel/$r"
    cp "$OUT/$r"/pyre-*-"$PLAT".tar.gz "$OUT/$r/SHA256SUMS" "$OUT/$r/SHA256SUMS.sig" "$S/$NAME/rel/$r/"
  done
  cp "$OUT/key/allowed-signers" "$S/$NAME/rel/allowed-signers"
  cp "$ROOT/scripts/install/rehearsal-supervisor.sh" "$ROOT/tests/e2e/release_update_host.sh" "$ROOT/tests/e2e/synth_mcp.py" "$S/$NAME/kit/"
  ssh "$HOST" mkdir -p .cache
  scp -q -r "$S/$NAME" "$HOST:.cache/"
  echo "uploaded to $HOST:~/.cache/$NAME ($PLAT)"
  ;;
run)
  HOST=${1:?host}; shift
  ssh "$HOST" env WUPD_DIR=.cache/"$NAME" WUPD_BASE="${WUPD_BASE:-rc2}" WUPD_NO_NODE="${WUPD_NO_NODE:-0}" bash -l .cache/"$NAME"/kit/release_update_host.sh "$@"
  ;;
*) sed -n '2,20p' "$0"; exit 2 ;;
esac
