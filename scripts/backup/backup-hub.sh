#!/bin/bash
# Back up a Pyre hub: a consistent SQLite snapshot, the published artifact
# blobs and the hub TLS identity, in one digest-manifested archive
# (`pyre-hub backup`). Safe while the hub is serving.
#
#   backup-hub.sh --data DIR [--out-dir DIR] [--encrypt auto|recipients|passphrase|gpg|none]
#                 [--recipient R]... [--recipients-file F]... [--passphrase-file F]
#                 [--verify] [--pyre-hub BIN]
#
# THE ARCHIVE CONTAINS THE HUB TLS PRIVATE KEY AND PRIVATE PROJECT DATA
# (documents, messages, artifacts, client/node token hashes). Store it
# encrypted and off the hub host.
#
# Encryption is built into pyre-hub (age format, streamed: no plaintext
# archive is ever written). Any age implementation can decrypt the result:
#   age -d -i key.txt pyre-hub-<UTC>.tar.age | tar -t
#   recipients: --recipient age1... (X25519), age1pq1... (post-quantum
#               hybrid; cannot be mixed with classic keys) or an ssh-ed25519/
#               ssh-rsa public key line; and/or --recipients-file F.
#               Public keys only: this script never creates, stores or reads
#               a private key. ("age" is accepted as an alias.)
#   passphrase: age scrypt, from --passphrase-file F or $PYRE_BACKUP_PASSPHRASE
#               (age -d prompts for it).
#   gpg:        --recipient KEYID|EMAIL; pyre-hub streams the tar straight
#               into `gpg --encrypt` through a pipe (nothing on disk).
#   none:       an explicit plaintext archive, with a warning.
# auto: recipients when every --recipient is an age/ssh key (gpg when they
# are not and gpg is installed); else passphrase when one is set; else none.
# --verify decrypts the finished archive as a stream and checks every
# digest without writing anything (recipient mode uses an ephemeral
# in-memory key added for the check; gpg mode needs the secret key in the
# keyring). The database integrity check runs on every snapshot.
#
# NOT in this backup (keep separately): node credentials and node state,
# harness account logins/profiles, the owner credential file written by
# `pyre-hub init` (client tokens stay valid: their hashes are in the
# database), the Android signing key, Firebase files.
#
# Output: <out-dir>/pyre-hub-<UTC>.tar.age (.tar.gpg, or .tar for none) and a
# .sha256 beside it.
set -euo pipefail
umask 077

usage() { sed -n '2,40p' "$0" | sed 's/^# \{0,1\}//'; exit 2; }
DATA="" OUT_DIR="." ENC=auto VERIFY=0 HUBBIN=${PYRE_HUB_BIN:-pyre-hub} PASSFILE=""
RECIPIENTS=() RFILES=()
while [ $# -gt 0 ]; do
  case "$1" in
    --data) DATA=${2:?}; shift 2 ;;
    --out-dir) OUT_DIR=${2:?}; shift 2 ;;
    --encrypt) ENC=${2:?}; shift 2 ;;
    --recipient) RECIPIENTS+=("${2:?}"); shift 2 ;;
    --recipients-file) RFILES+=("${2:?}"); shift 2 ;;
    --passphrase-file) PASSFILE=${2:?}; shift 2 ;;
    --verify) VERIFY=1; shift ;;
    --pyre-hub) HUBBIN=${2:?}; shift 2 ;;
    -h|--help) usage ;;
    *) echo "unknown argument: $1" >&2; usage ;;
  esac
done
[ -n "$DATA" ] || usage
[ -f "$DATA/hub.db" ] || { echo "error: $DATA/hub.db not found" >&2; exit 1; }
sha() { if command -v shasum >/dev/null 2>&1; then shasum -a 256 "$1"; else sha256sum "$1"; fi; }
is_age_recipient() { case "$1" in age1*|ssh-ed25519\ *|ssh-rsa\ *) return 0 ;; *) return 1 ;; esac; }
have_pass() { [ -n "$PASSFILE" ] || [ -n "${PYRE_BACKUP_PASSPHRASE:-}" ]; }

[ "$ENC" = age ] && ENC=recipients
if [ "$ENC" = auto ]; then
  all_age=1
  for r in ${RECIPIENTS[@]+"${RECIPIENTS[@]}"}; do is_age_recipient "$r" || all_age=0; done
  if [ ${#RFILES[@]} -gt 0 ] || { [ ${#RECIPIENTS[@]} -gt 0 ] && [ $all_age = 1 ]; }; then ENC=recipients
  elif [ ${#RECIPIENTS[@]} -gt 0 ] && command -v gpg >/dev/null 2>&1; then ENC=gpg
  elif [ ${#RECIPIENTS[@]} -gt 0 ]; then echo "error: recipients are not age/ssh keys and gpg is not installed" >&2; exit 2
  elif have_pass; then ENC=passphrase
  else ENC=none; fi
fi
case "$ENC" in
  recipients)
    [ ${#RECIPIENTS[@]} -gt 0 ] || [ ${#RFILES[@]} -gt 0 ] || { echo "error: --encrypt recipients needs --recipient or --recipients-file" >&2; exit 2; } ;;
  gpg)
    command -v gpg >/dev/null 2>&1 || { echo "error: gpg is not installed" >&2; exit 1; }
    [ ${#RECIPIENTS[@]} -gt 0 ] || { echo "error: --encrypt gpg needs --recipient" >&2; exit 2; } ;;
  passphrase)
    have_pass || { echo "error: --encrypt passphrase needs --passphrase-file or \$PYRE_BACKUP_PASSPHRASE" >&2; exit 2; } ;;
  none) ;;
  *) echo "error: --encrypt must be auto, recipients, passphrase, gpg or none" >&2; exit 2 ;;
esac
if [ -n "$PASSFILE" ] && [ "$ENC" != passphrase ]; then
  echo "error: --passphrase-file conflicts with --encrypt $ENC (an age passphrase must be the only recipient)" >&2; exit 2
fi
echo "encryption: $ENC"

mkdir -p "$OUT_DIR"
STAMP=$(date -u +%Y%m%dT%H%M%SZ)
BASE="$OUT_DIR/pyre-hub-$STAMP.tar"
VERIFYARG=()
if [ "$VERIFY" = 1 ]; then VERIFYARG=(--verify); fi
OUT=""
cleanup() { if [ -n "$OUT" ] && [ -f "$OUT.partial" ]; then rm -f "$OUT.partial"; fi; }
trap cleanup EXIT

# A passphrase in the environment must not leak into a recipient/gpg/none
# backup's flag checks (pyre-hub refuses ambiguous combinations).
case "$ENC" in
  recipients)
    OUT="$BASE.age"
    args=()
    for r in ${RECIPIENTS[@]+"${RECIPIENTS[@]}"}; do args+=(--recipient "$r"); done
    for f in ${RFILES[@]+"${RFILES[@]}"}; do args+=(--recipients-file "$f"); done
    env -u PYRE_BACKUP_PASSPHRASE "$HUBBIN" backup --data "$DATA" --out "$OUT" "${args[@]}" ${VERIFYARG[@]+"${VERIFYARG[@]}"} ;;
  passphrase)
    OUT="$BASE.age"
    args=()
    if [ -n "$PASSFILE" ]; then args=(--passphrase-file "$PASSFILE"); fi
    "$HUBBIN" backup --data "$DATA" --out "$OUT" ${args[@]+"${args[@]}"} ${VERIFYARG[@]+"${VERIFYARG[@]}"} ;;
  gpg)
    OUT="$BASE.gpg"
    args=()
    for r in "${RECIPIENTS[@]}"; do args+=(--recipient "$r"); done
    # Plaintext exists only inside the pipe; gpg writes ciphertext.
    echo "note: pyre-hub's UNENCRYPTED warning below refers to the stream piped into gpg"
    env -u PYRE_BACKUP_PASSPHRASE "$HUBBIN" backup --data "$DATA" --out - --unencrypted \
      | gpg --batch --yes --encrypt "${args[@]}" --output "$OUT.partial"
    mv "$OUT.partial" "$OUT"
    echo "encrypted with gpg to ${RECIPIENTS[*]} (streamed through a pipe; no plaintext on disk)"
    if [ "$VERIFY" = 1 ]; then
      gpg --batch --quiet --decrypt "$OUT" | env -u PYRE_BACKUP_PASSPHRASE "$HUBBIN" restore --in - --verify-only
    fi ;;
  none)
    OUT="$BASE"
    env -u PYRE_BACKUP_PASSPHRASE "$HUBBIN" backup --data "$DATA" --out "$OUT" --unencrypted
    if ! tar -tf "$OUT" | head -n 1 | grep -qx MANIFEST.json; then
      echo "error: $OUT does not start with MANIFEST.json" >&2; exit 1
    fi
    if [ "$VERIFY" = 1 ]; then "$HUBBIN" restore --in "$OUT" --verify-only; fi
    echo "WARNING: $OUT is NOT encrypted. It contains the hub TLS private key and private data." >&2
    echo "WARNING: encrypt it before it leaves this machine; it is mode 0600." >&2 ;;
esac
case "$ENC" in
  recipients|passphrase)
    if [ "$(head -c 21 "$OUT")" != "age-encryption.org/v1" ]; then
      echo "error: $OUT is not an age file" >&2; exit 1
    fi ;;
esac
(cd "$(dirname "$OUT")" && sha "$(basename "$OUT")") >"$OUT.sha256"
echo "backup: $OUT"
echo "sha256: $(cut -d' ' -f1 "$OUT.sha256")"
echo "not included: node credentials, account logins, owner-credential.json, Android signing key, Firebase files"
