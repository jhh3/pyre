#!/bin/bash
# Hub backup/restore rehearsal with synthetic data, fully isolated in a
# scratch directory (own hub data, node state, client config, gpg home; high
# loopback ports; mock runtime). Never touches ~/.config/pyre, the user's
# tmux server, ~/.gnupg or real credentials.
#
#   scripts/backup/rehearse-restore.sh [--bin DIR] [--dir WORKDIR] [--port-a N] [--port-b N] [--no-encrypt]
#                                      [--age-bin DIR] [--legacy-hub BIN]
#
#  1. init + serve hub A, log the CLI in, enroll a --dev-mock node, start a mock session
#  2. create a project (brief), a document, an artifact and a message through the CLI
#  3. back up while hub A is serving (backup-hub.sh --verify, built-in age
#     passphrase encryption; this archive is the one restored in step 5).
#     Also, each restored into its own scratch dir: age recipients (synthetic
#     X25519 and post-quantum keys; also decrypted with the stock age CLI),
#     gpg (throwaway key in WORKDIR/gnupg) and the legacy pre-age format
#     (written by --legacy-hub, an older pyre-hub). age-keygen/age come from
#     PATH, --age-bin, or `go install filippo.io/age/cmd/...@v1.3.2` into
#     WORKDIR; a format whose tool is unavailable is reported as SKIP.
#  4. write more data after the backup (message, artifact version) - expected to be lost
#  5. stop node + hub A; restore-hub.sh into a NEW dir; serve it on port B
#  6. log in against B with the same owner token, restart the node with --hub B
#  7. verify hub id, node identity (no duplicate), project, document revision,
#     artifact bytes/digest (downloaded with --no-cache), message; report-seq gap
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
W=/tmp/pyre-ops-restore
BIN="" PA=17743 PB=17744 ENCRYPT=1 AGEBIN="" LEGACY_HUB=""
while [ $# -gt 0 ]; do
  case "$1" in
    --bin) BIN=${2:?}; shift 2 ;;
    --dir) W=${2:?}; shift 2 ;;
    --port-a) PA=${2:?}; shift 2 ;;
    --port-b) PB=${2:?}; shift 2 ;;
    --no-encrypt) ENCRYPT=0; shift ;;
    --age-bin) AGEBIN=${2:?}; shift 2 ;;
    --legacy-hub) LEGACY_HUB=${2:?}; shift 2 ;;
    *) sed -n '2,25p' "$0"; exit 2 ;;
  esac
done
if [ -e "$W" ]; then
  [ -f "$W/.pyre-rehearsal" ] || { echo "refusing: $W exists and is not a rehearsal dir" >&2; exit 1; }
  rm -rf "$W"
fi
mkdir -p "$W"; chmod 700 "$W"; touch "$W/.pyre-rehearsal"
if [ -z "$BIN" ]; then make -C "$ROOT" build >/dev/null; BIN=$ROOT/bin; fi
P="$BIN/pyre"; D="$BIN/pyred"; H="$BIN/pyre-hub"
unset PYRE_HUB PYRE_HUB_PIN PYRE_SOCKET PYRE_TOKEN PYRE_SESSION_TOKEN PYRE_SESSION_ID
export PYRE_CONFIG="$W/client-a.json" PYRE_STATE_DIR="$W/cli-state"
A=https://127.0.0.1:$PA; B=https://127.0.0.1:$PB
PIDS=""
cleanup() {
  for p in $PIDS; do kill "$p" 2>/dev/null || true; done
  if [ -d "$W/gnupg" ] && command -v gpgconf >/dev/null 2>&1; then GNUPGHOME="$W/gnupg" gpgconf --kill gpg-agent 2>/dev/null || true; fi
}
trap cleanup EXIT
t0=$(date +%s)
log() { printf '[+%3ss] %s\n' "$(( $(date +%s) - t0 ))" "$*"; }
j() { python3 -c "import json,sys; d=json.load(open(sys.argv[1])); print($2)" "$1"; }
check() { if [ "$2" = "$3" ]; then log "PASS $1 ($2)"; else log "FAIL $1: got '$2' want '$3'"; FAILS=$((FAILS + 1)); fi; }
FAILS=0
wait_health() {
  for _ in $(seq 1 100); do
    curl -sk --max-time 1 "$1/healthz" 2>/dev/null | grep -q '"ok":true' && return 0
    sleep 0.1
  done
  echo "hub at $1 did not come up" >&2; return 1
}
wait_node() { # wait until `pyre nodes` shows the node connected
  for _ in $(seq 1 100); do
    "$P" nodes --json >"$W/nodes.poll.json" 2>/dev/null || true
    if python3 - "$W/nodes.poll.json" <<'PY'
import json,sys
try:
    d=json.load(open(sys.argv[1]))
except Exception:
    sys.exit(1)
items=d.get("items", d) if isinstance(d, dict) else d
sys.exit(0 if any((n.get("connection") or {}).get("state")=="connected" for n in items) else 1)
PY
    then return 0; fi
    sleep 0.2
  done
  echo "node did not connect" >&2; cat "$W/nodes.poll.json" >&2; return 1
}

log "binaries: $("$H" version)"
log "step 1: init + serve hub A on $A"
"$H" init --data "$W/hub-a" --hub-url "$A" --tls-host 127.0.0.1 >"$W/init.txt"
"$H" serve --data "$W/hub-a" --listen "127.0.0.1:$PA" --hub-url "$A" >"$W/hub-a.log" 2>&1 &
HA=$!; PIDS="$PIDS $HA"
wait_health "$A"
PIN=$(j "$W/hub-a/owner-credential.json" "d['hub_cert_sha256']")
j "$W/hub-a/owner-credential.json" "d['token']" | "$P" login --hub "$A" --pin "$PIN" --token - >/dev/null
"$P" hello --json >"$W/hello-a.json"
HUB_ID=$(j "$W/hello-a.json" "d['hub_id']")
log "hub A $HUB_ID, pin ${PIN:0:16}..."

"$P" enroll-node --name rehearsal-node --json >"$W/ticket.json" 2>/dev/null
NODE_ID=$(j "$W/ticket.json" "d['node_id']")
mkdir -p "$W/node"
j "$W/ticket.json" "d['secret']" | "$D" enroll --hub "$A" --pin "$PIN" --node-id "$NODE_ID" --credential-file "$W/node/node-credential.json" --secret-stdin >/dev/null
rm -f "$W/ticket.json"
"$D" run --state "$W/node" --name rehearsal-node --no-integrations --dev-mock >"$W/node-a.log" 2>&1 &
NP=$!; PIDS="$PIDS $NP"
wait_node
log "node $NODE_ID connected to A"

log "step 2: synthetic objects"
"$P" start --node rehearsal-node --harness mock --cwd "$W" --title restore-rehearsal --json >"$W/session.json"
SESSION=$(j "$W/session.json" "d['session']['session']['session_id']")
"$P" project new restore-rehearsal --brief "Synthetic brief for the restore rehearsal." --json >"$W/project.json"
PROJECT=$(j "$W/project.json" "d['project_id']")
"$P" doc new --project restore-rehearsal --title "Rehearsal notes" --slug rehearsal-notes --content "synthetic document body, revision 1" --json >"$W/doc.json"
DOC=$(j "$W/doc.json" "d['document']['document_id']")
REV=$(j "$W/doc.json" "d['revision']['revision_id']")
head -c 4096 /dev/urandom | base64 >"$W/artifact-v1.txt"
"$P" publish "$W/artifact-v1.txt" --project restore-rehearsal --title restore-artifact --json >"$W/publish-v1.json"
ART=$(j "$W/publish-v1.json" "d['artifact']['artifact_id']")
V1=$(j "$W/publish-v1.json" "d['version']['version_id']")
V1_DIGEST=$(j "$W/publish-v1.json" "d['version']['digest']")
V1_LOCAL=$(shasum -a 256 "$W/artifact-v1.txt" | cut -d' ' -f1)
sleep 1
"$P" send "$SESSION" "synthetic message before the backup" --json >"$W/m1.json"
M1=$(j "$W/m1.json" "d['envelope']['message_id']")
sleep 3
"$P" msg show "$M1" --json >"$W/m1-before.json"
log "session $SESSION project $PROJECT doc $DOC@$REV artifact $ART@$V1 ($V1_DIGEST) message $M1"
check "artifact digest equals local sha256" "${V1_DIGEST#sha256:}" "$V1_LOCAL"

log "step 3: backup while hub A is serving"
unset PYRE_BACKUP_PASSPHRASE
printf 'synthetic rehearsal passphrase\n' >"$W/backup-passphrase"
ENC_ARGS=(--encrypt passphrase --passphrase-file "$W/backup-passphrase")
RESTORE_ARGS=(--passphrase-file "$W/backup-passphrase")
if [ "$ENCRYPT" = 0 ]; then ENC_ARGS=(--encrypt none); RESTORE_ARGS=(); fi
bash "$ROOT/scripts/backup/backup-hub.sh" --data "$W/hub-a" --out-dir "$W/backups" "${ENC_ARGS[@]}" --verify --pyre-hub "$H" >"$W/backup.txt" 2>&1
cat "$W/backup.txt"
ARCHIVE=$(sed -n 's/^backup: //p' "$W/backup.txt")
[ -f "$ARCHIVE" ] || { echo "no archive" >&2; exit 1; }
if [ "$ENCRYPT" = 1 ]; then
  check "main archive is an age file" "$(head -c 21 "$ARCHIVE")" "age-encryption.org/v1"
  if grep -a -q -F "synthetic document body" "$ARCHIVE"; then log "FAIL plaintext visible in $ARCHIVE"; FAILS=$((FAILS + 1)); else log "PASS no plaintext in the encrypted archive"; fi
fi
PLAIN_LEFT=$(find "$W/backups" "$W/hub-a" -name '*.partial' -o -name '.pyre-backup-*' -o -name '*.tar' | wc -l | tr -d ' ')
check "no plaintext/partial/staging files left by backups" "$PLAIN_LEFT" "$([ "$ENCRYPT" = 0 ] && echo 1 || echo 0)"

# Extra formats: each archive is restored into its own scratch dir (not served).
extra_restore() { # name archive [restore-hub args...]
  local name=$1 arc=$2; shift 2
  if bash "$ROOT/scripts/backup/restore-hub.sh" --in "$arc" --data "$W/extra-$name" "$@" --pyre-hub "$H" >"$W/restore-$name.txt" 2>&1 \
     && [ "$(python3 -c "import sqlite3,sys; print(sqlite3.connect('file:'+sys.argv[1]+'?mode=ro', uri=True).execute(\"select value from hub_meta where key='hub_id'\").fetchone()[0])" "$W/extra-$name/hub.db")" = "$HUB_ID" ]; then
    log "PASS $name archive restores (digests, integrity, hub id)"
  else
    log "FAIL $name archive restore"; sed 's/^/    /' "$W/restore-$name.txt"; FAILS=$((FAILS + 1))
  fi
}
if [ "$ENCRYPT" = 1 ]; then
  # age recipients (X25519, then post-quantum hybrid).
  KEYGEN="" AGECLI=""
  for d in "$AGEBIN" "$W/agebin"; do
    if [ -n "$d" ] && [ -x "$d/age-keygen" ]; then KEYGEN=$d/age-keygen; AGECLI=$d/age; fi
  done
  if [ -z "$KEYGEN" ] && command -v age-keygen >/dev/null 2>&1; then KEYGEN=$(command -v age-keygen); AGECLI=$(command -v age || true); fi
  if [ -z "$KEYGEN" ] && command -v go >/dev/null 2>&1; then
    if GOBIN="$W/agebin" GOFLAGS= go install filippo.io/age/cmd/...@v1.3.2 >"$W/age-install.log" 2>&1; then KEYGEN=$W/agebin/age-keygen; AGECLI=$W/agebin/age; fi
  fi
  if [ -n "$KEYGEN" ]; then
    "$KEYGEN" -o "$W/age-x25519.key" 2>/dev/null
    "$KEYGEN" -pq -o "$W/age-pq.key" 2>/dev/null
    for kind in x25519 pq; do
      REC=$("$KEYGEN" -y "$W/age-$kind.key")
      mkdir -p "$W/backups-$kind"
      if bash "$ROOT/scripts/backup/backup-hub.sh" --data "$W/hub-a" --out-dir "$W/backups-$kind" --encrypt recipients --recipient "$REC" --verify --pyre-hub "$H" >"$W/backup-$kind.txt" 2>&1; then
        ARC=$(sed -n 's/^backup: //p' "$W/backup-$kind.txt")
        log "age $kind backup: $ARC"
        extra_restore "age-$kind" "$ARC" --identity "$W/age-$kind.key"
        if [ -x "$AGECLI" ]; then
          FIRST=$("$AGECLI" -d -i "$W/age-$kind.key" "$ARC" | tar -tf - | head -n 1)
          check "stock age CLI ($("$AGECLI" --version)) decrypts the $kind archive: age -d | tar -t first entry" "$FIRST" "MANIFEST.json"
        fi
      else
        log "FAIL age $kind backup"; sed 's/^/    /' "$W/backup-$kind.txt"; FAILS=$((FAILS + 1))
      fi
    done
  else
    log "SKIP age recipient rehearsal: no age-keygen (PATH, --age-bin, go install)"
  fi
  # gpg, streamed through a pipe.
  if command -v gpg >/dev/null 2>&1; then
    export GNUPGHOME="$W/gnupg"; mkdir -p "$GNUPGHOME"; chmod 700 "$GNUPGHOME"
    gpg --batch --quiet --passphrase '' --quick-gen-key "Pyre rehearsal (synthetic) <rehearsal@pyre.invalid>" default default never 2>/dev/null
    log "gpg: throwaway key in $GNUPGHOME (synthetic, deleted with WORKDIR)"
    mkdir -p "$W/backups-gpg"
    if bash "$ROOT/scripts/backup/backup-hub.sh" --data "$W/hub-a" --out-dir "$W/backups-gpg" --encrypt gpg --recipient rehearsal@pyre.invalid --verify --pyre-hub "$H" >"$W/backup-gpg.txt" 2>&1; then
      ARC=$(sed -n 's/^backup: //p' "$W/backup-gpg.txt")
      check "gpg backup leaves only the .gpg and .sha256" "$(ls "$W/backups-gpg" | wc -l | tr -d ' ')" "2"
      extra_restore gpg "$ARC"
    else
      log "FAIL gpg backup"; sed 's/^/    /' "$W/backup-gpg.txt"; FAILS=$((FAILS + 1))
    fi
  else
    log "SKIP gpg rehearsal: gpg not installed"
  fi
  # Legacy pre-age format, written by an older pyre-hub.
  if [ -n "$LEGACY_HUB" ]; then
    if PYRE_BACKUP_PASSPHRASE="synthetic legacy passphrase" "$LEGACY_HUB" backup --data "$W/hub-a" --out "$W/legacy.tar" >"$W/backup-legacy.txt" 2>&1; then
      check "legacy archive header" "$(head -c 21 "$W/legacy.tar")" "PYRE-HUB-BACKUP-ENC/1"
      printf 'synthetic legacy passphrase\n' >"$W/legacy-passphrase"
      extra_restore legacy "$W/legacy.tar" --passphrase-file "$W/legacy-passphrase"
    else
      log "FAIL legacy backup with $LEGACY_HUB"; sed 's/^/    /' "$W/backup-legacy.txt"; FAILS=$((FAILS + 1))
    fi
  else
    log "SKIP legacy-format rehearsal: no --legacy-hub (unit tests cover the reader)"
  fi
fi

log "step 4: writes after the backup (expected to be lost by the restore)"
"$P" send "$SESSION" "synthetic message after the backup" --json >"$W/m2.json"
M2=$(j "$W/m2.json" "d['envelope']['message_id']")
echo "synthetic v2 $(date +%s)" >"$W/artifact-v2.txt"
"$P" publish "$W/artifact-v2.txt" --artifact "$ART" --json >"$W/publish-v2.json"
V2=$(j "$W/publish-v2.json" "d['version']['version_id']")
sleep 3
log "post-backup message $M2, artifact version $V2"
"$D" status --state "$W/node" --json >"$W/node-status-a.json"

log "step 5: stop node and hub A; restore into a new dir; serve on $B"
kill "$NP"; wait "$NP" 2>/dev/null || true
kill "$HA"; wait "$HA" 2>/dev/null || true
bash "$ROOT/scripts/backup/restore-hub.sh" --in "$ARCHIVE" --data "$W/hub-b" ${RESTORE_ARGS[@]+"${RESTORE_ARGS[@]}"} --check-listen "127.0.0.1:$PB" --hub-url "$B" --pyre-hub "$H" >"$W/restore.txt" 2>&1
cat "$W/restore.txt"
"$H" serve --data "$W/hub-b" --listen "127.0.0.1:$PB" --hub-url "$B" >"$W/hub-b.log" 2>&1 &
HB=$!; PIDS="$PIDS $HB"
wait_health "$B"

log "step 6: reconnect the CLI (same owner token) and the node (--hub $B)"
export PYRE_CONFIG="$W/client-b.json" PYRE_STATE_DIR="$W/cli-state-b"
j "$W/hub-a/owner-credential.json" "d['token']" | "$P" login --hub "$B" --pin "$PIN" --token - >/dev/null
"$D" run --state "$W/node" --hub "$B" --name rehearsal-node --no-integrations --dev-mock >"$W/node-b.log" 2>&1 &
NP=$!; PIDS="$PIDS $NP"
wait_node
log "node reconnected to B"
sleep 3

log "step 7: verify"
"$P" hello --json >"$W/hello-b.json"
check "hub id preserved" "$(j "$W/hello-b.json" "d['hub_id']")" "$HUB_ID"
"$P" nodes --json >"$W/nodes-b.json"
check "single node, same id" "$(j "$W/nodes-b.json" "','.join(n['node_id'] for n in d['items'])")" "$NODE_ID"
"$P" project show "$PROJECT" --json >"$W/project-b.json"
check "project name" "$(j "$W/project-b.json" "d.get('project', d).get('name')")" "restore-rehearsal"
"$P" doc show "$DOC" --json >"$W/doc-b.json" 2>/dev/null
check "document head revision" "$(j "$W/doc-b.json" "d['revision']['revision_id']")" "$REV"
check "document content" "$(j "$W/doc-b.json" "d['revision']['content']")" "synthetic document body, revision 1"
"$P" artifact versions "$ART" --json >"$W/versions-b.json"
check "artifact versions after restore" "$(j "$W/versions-b.json" "','.join(v['version_id'] for v in d['items'])")" "$V1"
"$P" artifact get "$V1" --no-cache -o "$W/artifact-v1.restored" >/dev/null
check "restored artifact bytes sha256" "$(shasum -a 256 "$W/artifact-v1.restored" | cut -d' ' -f1)" "$V1_LOCAL"
"$P" msg show "$M1" --json >"$W/m1-after.json"
check "pre-backup message present" "$(j "$W/m1-after.json" "d.get('envelope', d).get('message_id')")" "$M1"
if "$P" msg show "$M2" --json >"$W/m2-after.json" 2>"$W/m2-after.err"; then
  log "NOTE post-backup message $M2 still visible (unexpected)"; FAILS=$((FAILS + 1))
else
  log "PASS post-backup message absent after restore (expected loss): $(head -c 160 "$W/m2-after.err")"
fi
if "$P" artifact show "$V2" --json >/dev/null 2>&1; then log "NOTE post-backup artifact version still visible (unexpected)"; FAILS=$((FAILS + 1)); else log "PASS post-backup artifact version absent (expected loss)"; fi
"$P" sessions show "$SESSION" --json >"$W/session-b.json" 2>/dev/null || true
log "session after restore: $(j "$W/session-b.json" "d['session']['session_id'] + ' run ' + d['run']['state'] + ' (' + d['run'].get('state_reason', '') + ')'" 2>/dev/null || echo unreadable)"
"$D" status --state "$W/node" --json >"$W/node-status-b.json"
log "node status: link $(j "$W/node-status-b.json" "d['diagnostics']['link']"), live runs $(j "$W/node-status-b.json" "d['live_runs']"), pyred RSS $(j "$W/node-status-b.json" "d['diagnostics']['resources'].get('pyre_rss_mb')") MB"
GAP=$(grep -c "node report gap" "$W/hub-b.log" || true)
log "hub B log 'node report gap' lines: $GAP"
grep "node report gap" "$W/hub-b.log" | sed 's/^/    /' || true
grep -i -E "level=(WARN|ERROR)" "$W/hub-b.log" | grep -v "node report gap" | head -n 10 | sed 's/^/    hub-b: /' || true
log "secrets check: grep for the owner token in logs and outputs"
# Pattern file under the excluded hub-a dir: grep -f keeps the token off argv.
(umask 077; j "$W/hub-a/owner-credential.json" "d['token']" >"$W/hub-a/token.pat")
if grep -rl --exclude=owner-credential.json --exclude=client-a.json --exclude=client-b.json --exclude-dir=hub-a --exclude-dir=hub-b --exclude-dir='extra-*' --exclude-dir=gnupg --exclude-dir='backups*' --exclude-dir=node -F -f "$W/hub-a/token.pat" "$W" >/dev/null 2>&1; then
  log "FAIL owner token found in a log/output file"; FAILS=$((FAILS + 1))
else
  log "PASS owner token not present in logs or command outputs"
fi
log "done: $FAILS failure(s); workdir $W"
exit "$FAILS"
