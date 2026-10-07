#!/bin/bash
# Restore a Pyre hub backup into a NEW data directory, verify it and print
# the reconnect steps. Never writes into an existing non-empty directory.
#
#   restore-hub.sh --in FILE --data NEWDIR [--identity FILE]... [--passphrase-file F]
#                  [--check-listen 127.0.0.1:PORT] [--hub-url URL] [--pyre-hub BIN]
#
# `pyre-hub restore` detects the archive format from its first bytes and
# decrypts it as a stream (no plaintext archive on disk):
#   age (.tar.age, binary or armored): --identity FILE (an age identity file,
#       AGE-SECRET-KEY-1... or AGE-SECRET-KEY-PQ-1..., or an unencrypted
#       OpenSSH private key) for recipient backups, or --passphrase-file F /
#       $PYRE_BACKUP_PASSPHRASE for passphrase backups;
#   legacy pyre passphrase format (.tar starting PYRE-HUB-BACKUP-ENC/1, written
#       before the move to age): --passphrase-file F / $PYRE_BACKUP_PASSPHRASE;
#   plaintext .tar: nothing.
# .gpg archives are decrypted by gpg with the operator's keyring and piped
# into `pyre-hub restore --in -`. Plugin identities (age-plugin-yubikey...)
# and passphrase-protected SSH keys: run `age -d -i ID FILE | pyre-hub
# restore --in - --data NEWDIR` by hand.
# `pyre-hub restore` checks every file against the manifest and the database
# integrity before anything appears in NEWDIR.
#
# --check-listen starts the restored hub briefly on that address, waits for
# /healthz, prints its TLS fingerprint and stops it.
set -euo pipefail
umask 077

usage() { sed -n '2,25p' "$0" | sed 's/^# \{0,1\}//'; exit 2; }
IN="" DATA="" CHECK="" HUB_URL="" HUBBIN=${PYRE_HUB_BIN:-pyre-hub} PASSFILE=""
IDS=()
while [ $# -gt 0 ]; do
  case "$1" in
    --in) IN=${2:?}; shift 2 ;;
    --data) DATA=${2:?}; shift 2 ;;
    --identity) IDS+=(--identity "${2:?}"); shift 2 ;;
    --passphrase-file) PASSFILE=${2:?}; shift 2 ;;
    --check-listen) CHECK=${2:?}; shift 2 ;;
    --hub-url) HUB_URL=${2:?}; shift 2 ;;
    --pyre-hub) HUBBIN=${2:?}; shift 2 ;;
    -h|--help) usage ;;
    *) echo "unknown argument: $1" >&2; usage ;;
  esac
done
[ -n "$IN" ] && [ -n "$DATA" ] || usage
[ -f "$IN" ] || { echo "error: $IN not found" >&2; exit 1; }
if [ -d "$DATA" ] && [ -n "$(ls -A "$DATA")" ]; then
  echo "error: $DATA is not empty; restore into a new directory (the old one stays as your fallback)" >&2; exit 1
fi
if [ -f "$IN.sha256" ]; then
  want=$(cut -d' ' -f1 "$IN.sha256")
  if command -v shasum >/dev/null 2>&1; then got=$(shasum -a 256 "$IN" | cut -d' ' -f1); else got=$(sha256sum "$IN" | cut -d' ' -f1); fi
  [ "$want" = "$got" ] || { echo "error: $IN does not match $IN.sha256" >&2; exit 1; }
  echo "archive checksum matches $IN.sha256"
fi

PARENT=$(dirname "$DATA")
mkdir -p "$PARENT"
TMPD=$(mktemp -d "$PARENT/.pyre-restore-check.XXXXXX")
HUBPID=""
cleanup() {
  if [ -n "$HUBPID" ]; then kill "$HUBPID" 2>/dev/null || true; fi
  rm -rf "$TMPD"
}
trap cleanup EXIT

KEYARGS=(${IDS[@]+"${IDS[@]}"})
if [ -n "$PASSFILE" ]; then KEYARGS+=(--passphrase-file "$PASSFILE"); fi
case "$IN" in
  *.gpg)
    # Plaintext exists only inside the pipe.
    gpg --batch --quiet --decrypt "$IN" | "$HUBBIN" restore --in - --data "$DATA" ;;
  *)
    "$HUBBIN" restore --in "$IN" --data "$DATA" ${KEYARGS[@]+"${KEYARGS[@]}"} ;;
esac

FPR=""
if [ -n "$CHECK" ]; then
  "$HUBBIN" serve --data "$DATA" --listen "$CHECK" ${HUB_URL:+--hub-url "$HUB_URL"} >"$TMPD/check.log" 2>&1 &
  HUBPID=$!
  ok=0
  for _ in $(seq 1 50); do
    if curl -sk --max-time 2 "https://$CHECK/healthz" 2>/dev/null | grep -q '"ok":true'; then ok=1; break; fi
    sleep 0.2
  done
  FPR=$(sed -n 's/.*tls_sha256=\([0-9a-f:]*\).*/\1/p' "$TMPD/check.log" | head -n 1)
  kill "$HUBPID" 2>/dev/null || true; wait "$HUBPID" 2>/dev/null || true; HUBPID=""
  if [ "$ok" = 1 ]; then echo "restored hub answered /healthz on $CHECK (TLS sha256 $FPR)"; else
    echo "error: restored hub did not answer on $CHECK; log:" >&2; cat "$TMPD/check.log" >&2; exit 1; fi
fi

cat <<EOF

Restored into $DATA. Reconnect:
  1. Stop the old hub if it is still running (two hubs with one identity confuse nodes).
  2. Start the restored hub, e.g. as a service:
       scripts/install/install.sh --tarball <release tarball> --service hub --hub-data "$DATA" --hub-url ${HUB_URL:-https://<host>:7443}
     (--tarball, not --from: once a release key is pinned, install.sh refuses an unsigned --from directory)
     or in the foreground:
       pyre-hub serve --data "$DATA" --listen :7443 --hub-url ${HUB_URL:-https://<host>:7443}
  3. The TLS identity was restored, so the pin is unchanged${FPR:+ ($FPR)}: clients and nodes
     need no re-pairing. Tokens are in the database, so existing client configs keep working.
  4. Same hub URL: nodes reconnect by themselves (backoff up to ~60 s).
     New URL: on each node run pyred with --hub NEWURL (or re-enroll), and on each client
     'pyre login --hub NEWURL --pin <pin> --token - --force' (token from the old client config).
     Android: re-pair if the URL changed (pyre pair-client --kind android).
  5. Anything written after the backup was taken is gone: messages, document revisions,
     artifacts. A node whose acknowledged reports are newer than the backup reconnects with
     a "node report gap" warning in the hub log; those reports are not replayed.
  6. Check: pyre hello; pyre nodes; pyre project ls; pyre artifact ls.
Not restored by this archive: node credentials, account logins, owner-credential.json,
the Android signing key, Firebase files.
EOF
