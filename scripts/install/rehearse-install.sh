#!/bin/bash
# Install / upgrade / rollback / uninstall rehearsal in fake homes and a fake
# root. Nothing is installed on this machine: every install uses --home
# <scratch> (which forces --no-service-manager) or --root <scratch>; the
# daemon is started by rehearsal-supervisor.sh from the rendered plist, never
# by launchctl. Hub, node and CLI state live in the scratch directory.
#
#   scripts/install/rehearse-install.sh [--dir WORKDIR] [--port N] [--skip-release]
#
# Releases are signed with a throwaway key generated here (never the owner's
# key) and installed with --allowed-signers, so the first install verifies the
# signature and pins that key.
#
# Cases: release build x2 (reproducibility) + a second version; refusal on a
# tampered tarball (install and upgrade); refusal on a
# foreign file; dry run; tarball install with a node service; idempotent
# re-install; --edit-path; upgrade with hub protocol check and restart;
# failing upgrade -> automatic rollback; incompatible protocol -> refused;
# manual rollback; run-loss guard; uninstall; purge (list, then --yes, then
# --include-profiles; CLI state and the shared transcript store);
# Linux render-only install/upgrade/uninstall in a fake root.
set -uo pipefail
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
W=/tmp/pyre-ops-install
PORT=17843 SKIP_RELEASE=0
while [ $# -gt 0 ]; do
  case "$1" in
    --dir) W=${2:?}; shift 2 ;;
    --port) PORT=${2:?}; shift 2 ;;
    --skip-release) SKIP_RELEASE=1; shift ;;
    *) sed -n '2,22p' "$0"; exit 2 ;;
  esac
done
V1=0.0.0-rehearsal.1 V2=0.0.0-rehearsal.2 V3=0.0.0-rehearsal.3-bad V4=0.0.0-rehearsal.4-proto
if [ -e "$W" ]; then
  [ -f "$W/.pyre-rehearsal" ] || { echo "refusing: $W exists and is not a rehearsal dir" >&2; exit 1; }
  if [ "$SKIP_RELEASE" = 1 ] && [ -d "$W/rel-1" ]; then
    find "$W" -mindepth 1 -maxdepth 1 ! -name 'rel-*' ! -name .pyre-rehearsal -exec rm -rf {} +
  else
    rm -rf "$W"
  fi
fi
mkdir -p "$W/logs"; chmod 700 "$W"; touch "$W/.pyre-rehearsal"
I="$ROOT/scripts/install"
FH="$W/home-mac"
export PYRE_REHEARSAL_HOME="$FH" PYRE_REHEARSAL_RUN="$W/run"
unset PYRE_HUB PYRE_HUB_PIN PYRE_SOCKET PYRE_TOKEN PYRE_SESSION_TOKEN
export PYRE_CONFIG="$FH/.config/pyre/client.json" PYRE_STATE_DIR="$W/cli-state"
STATE="$FH/Library/Application Support/pyre"
HUBPID=""
cleanup() {
  "$BASH" "$I/rehearsal-supervisor.sh" stop node >/dev/null 2>&1 || true
  if [ -n "$HUBPID" ]; then kill "$HUBPID" 2>/dev/null || true; fi
}
trap cleanup EXIT
FAILS=0 N=0
t0=$(date +%s)
log() { printf '[+%3ss] %s\n' "$(( $(date +%s) - t0 ))" "$*"; }
# case NAME EXPECTED_EXIT CMD...: run, save output, compare exit code.
case_() {
  local name=$1 want=$2 rc
  shift 2
  N=$((N + 1))
  local out; out=$(printf '%s/logs/%02d-%s.txt' "$W" "$N" "$name")
  { printf '$'; printf ' %q' "$@"; printf '\n'; } >"$out"
  "$@" >>"$out" 2>&1 </dev/null
  rc=$?
  echo "exit $rc" >>"$out"
  if [ "$rc" = "$want" ]; then log "PASS $name (exit $rc) -> ${out#$W/}"; else log "FAIL $name: exit $rc, want $want -> ${out#$W/}"; FAILS=$((FAILS + 1)); tail -n 15 "$out" | sed 's/^/      /'; fi
}
assert() { # NAME TEST...
  local name=$1; shift
  if "$@"; then log "PASS $name"; else log "FAIL $name"; FAILS=$((FAILS + 1)); fi
}
j() { python3 -c "import json,sys; d=json.load(open(sys.argv[1])); print($2)" "$1"; }
cur() { basename "$(readlink "$FH/.local/share/pyre/current")"; }
tree_sum() { (cd "$1" && find . -type f -o -type l | LC_ALL=C sort | while IFS= read -r f; do if [ -L "$f" ]; then echo "$f -> $(readlink "$f")"; else echo "$f $(shasum -a 256 "$f" | cut -c1-16)"; fi; done) | shasum -a 256 | cut -c1-16; }

# ---- releases (throwaway signing key, kept in rel-key/ for --skip-release) ----
SK="$W/rel-key/sign.key" SA="$W/rel-key/allowed-signers"
if [ ! -f "$SK" ]; then
  mkdir -p "$W/rel-key"
  ssh-keygen -q -t ed25519 -N "" -C synthetic-rehearsal -f "$SK"
  read -r kt kb _ <"$SK.pub"
  printf 'pyre-release namespaces="pyre-release" %s %s synthetic-rehearsal\n' "$kt" "$kb" >"$SA"
fi
if [ "$SKIP_RELEASE" = 0 ]; then
  log "release $V1 (twice, reproducibility) and $V2, signed with a throwaway key"
  case_ release-v1-a 0 "$BASH" "$ROOT/packaging/release.sh" --version "$V1" --out "$W/rel-1" --signing-key "$SK" --allowed-signers "$SA"
  case_ release-v1-b 0 "$BASH" "$ROOT/packaging/release.sh" --version "$V1" --out "$W/rel-1b" --signing-key "$SK" --allowed-signers "$SA"
  assert "release tarballs reproducible (identical SHA256SUMS)" cmp -s "$W/rel-1/SHA256SUMS" "$W/rel-1b/SHA256SUMS"
  assert "release signatures reproducible (identical SHA256SUMS.sig)" cmp -s "$W/rel-1/SHA256SUMS.sig" "$W/rel-1b/SHA256SUMS.sig"
  case_ release-v2 0 "$BASH" "$ROOT/packaging/release.sh" --version "$V2" --out "$W/rel-2" --signing-key "$SK" --allowed-signers "$SA"
fi
T1="$W/rel-1/pyre-$V1-darwin-arm64.tar.gz"; T2="$W/rel-2/pyre-$V2-darwin-arm64.tar.gz"
TL1="$W/rel-1/pyre-$V1-linux-amd64.tar.gz"; TL2="$W/rel-2/pyre-$V2-linux-amd64.tar.gz"
mkdir -p "$W/x1" "$W/x2"
tar -xzf "$T1" -C "$W/x1"; tar -xzf "$T2" -C "$W/x2"
X1="$W/x1/pyre-$V1-darwin-arm64"; X2="$W/x2/pyre-$V2-darwin-arm64"

# ---- fake homes ----
mkdir -p "$FH/.local/bin" "$FH/.config/pyre" "$STATE/profiles/acct-synthetic/claude"
for c in hive outfit rota; do printf '#!/bin/sh\necho synthetic %s\n' "$c" >"$FH/.local/bin/$c"; chmod 755 "$FH/.local/bin/$c"; done
echo "storeFile=/synthetic/not-a-key.jks" >"$FH/.config/pyre/android-signing.properties"
echo "synthetic profile marker" >"$STATE/profiles/acct-synthetic/claude/.keep"
printf '# synthetic zshrc\nalias ll="ls -l"\n' >"$FH/.zshrc"
OTHERS_BEFORE=$(cat "$FH/.local/bin/hive" "$FH/.local/bin/outfit" "$FH/.local/bin/rota" | shasum -a 256)
FC="$W/home-conflict"; mkdir -p "$FC/.local/bin"; printf '#!/bin/sh\necho not pyre\n' >"$FC/.local/bin/pyre"; chmod 755 "$FC/.local/bin/pyre"

log "tampered tarball, refusal, dry run"
mkdir -p "$W/tampered"
cp "$T2" "$W/rel-2/SHA256SUMS" "$W/rel-2/SHA256SUMS.sig" "$W/tampered/"
printf 'synthetic tampering\n' >>"$W/tampered/$(basename "$T2")"
TT="$W/tampered/$(basename "$T2")"
FT="$W/home-tampered"; mkdir -p "$FT"
case_ refuse-tampered-tarball 3 "$BASH" "$I/install.sh" --tarball "$TT" --home "$FT" --allowed-signers "$SA"
assert "tampered tarball: nothing installed, no key pinned" test ! -e "$FT/.local/share/pyre" -a ! -e "$FT/.config/pyre/release-allowed-signers"
case_ refuse-foreign-shim 3 "$BASH" "$I/install.sh" --tarball "$T1" --home "$FC" --allowed-signers "$SA"
assert "refusal left no Pyre files" test ! -e "$FC/.local/share/pyre"
case_ dry-run 0 "$BASH" "$I/install.sh" --tarball "$T1" --home "$FH" --service node --dry-run --allowed-signers "$SA"
assert "dry run wrote nothing" test ! -e "$FH/.local/share/pyre" -a ! -e "$FH/Library/LaunchAgents" -a ! -e "$FH/.config/pyre/release-allowed-signers"

# ---- hub + enrollment (scratch) ----
log "scratch hub on 127.0.0.1:$PORT ($V1)"
"$X1/bin/pyre-hub" init --data "$W/hub" --hub-url "https://127.0.0.1:$PORT" --tls-host 127.0.0.1 >/dev/null
"$X1/bin/pyre-hub" serve --data "$W/hub" --listen "127.0.0.1:$PORT" --hub-url "https://127.0.0.1:$PORT" >"$W/hub.log" 2>&1 &
HUBPID=$!
for _ in $(seq 1 100); do curl -sk --max-time 1 "https://127.0.0.1:$PORT/healthz" | grep -q ok && break; sleep 0.1; done
PIN=$(j "$W/hub/owner-credential.json" "d['hub_cert_sha256']")
j "$W/hub/owner-credential.json" "d['token']" | "$X1/bin/pyre" login --hub "https://127.0.0.1:$PORT" --pin "$PIN" --token - >/dev/null
"$X1/bin/pyre" enroll-node --name rehearsal-mac --json >"$W/ticket.json" 2>/dev/null
mkdir -p "$STATE"; chmod 700 "$STATE"
j "$W/ticket.json" "d['secret']" | "$X1/bin/pyred" enroll --hub "https://127.0.0.1:$PORT" --pin "$PIN" --node-id "$(j "$W/ticket.json" "d['node_id']")" --credential-file "$STATE/node-credential.json" --secret-stdin >/dev/null
rm -f "$W/ticket.json"

# ---- install v1 with the node service ----
log "install $V1 from the tarball with --service node"
case_ install-v1 0 "$BASH" "$I/install.sh" --tarball "$T1" --home "$FH" --service node --name rehearsal-mac --dev-mock --no-integrations --allowed-signers "$SA"
assert "install verified the signature and pinned the throwaway key" cmp -s "$FH/.config/pyre/release-allowed-signers" "$SA"
PL="$FH/Library/LaunchAgents/dev.jhh3.pyre.pyred.plist"
assert "current -> $V1" test "$(cur)" = "$V1"
assert "shims point into current" test "$(readlink "$FH/.local/bin/pyred")" = "../share/pyre/current/bin/pyred"
assert "plist lints" plutil -lint "$PL"
assert "plist has AbandonProcessGroup=true" test "$(plutil -extract AbandonProcessGroup raw "$PL")" = true
assert "plist KeepAlive.SuccessfulExit=false" test "$(plutil -extract KeepAlive.SuccessfulExit raw "$PL")" = false
# Token patterns live in a 0600 file: grep -f keeps them off command lines.
(umask 077; j "$STATE/node-credential.json" "d['token']" >"$W/node-token.pat")
assert "plist contains no token" sh -c '! grep -qF -f "$1" "$2"' _ "$W/node-token.pat" "$PL"
assert "hive/outfit/rota untouched" test "$(cat "$FH/.local/bin/hive" "$FH/.local/bin/outfit" "$FH/.local/bin/rota" | shasum -a 256)" = "$OTHERS_BEFORE"
assert "zshrc not edited without --edit-path" sh -c "! grep -q 'pyre PATH' '$FH/.zshrc'"
cp "$PL" "$W/logs/rendered-pyred.plist"
S1=$(tree_sum "$FH")
case_ install-v1-again-idempotent 0 "$BASH" "$I/install.sh" --tarball "$T1" --home "$FH" --service node --name rehearsal-mac --dev-mock --no-integrations
assert "re-install changed nothing" test "$(tree_sum "$FH")" = "$S1"
case_ install-edit-path 0 "$BASH" "$I/install.sh" --tarball "$T1" --home "$FH" --edit-path
assert "zshrc has one marked PATH block" test "$(grep -c '>>> pyre PATH >>>' "$FH/.zshrc")" = 1

log "start pyred from the rendered plist (test double for launchd)"
bash "$I/rehearsal-supervisor.sh" start node >"$W/logs/supervisor-start.txt" 2>&1
ok=0
for _ in $(seq 1 100); do
  if "$FH/.local/share/pyre/current/bin/pyred" status --state "$STATE" --json 2>/dev/null | grep -q '"link":"connected"'; then ok=1; break; fi
  sleep 0.2
done
assert "pyred (launched from plist args, env -i) connected to the hub" test "$ok" = 1
"$FH/.local/share/pyre/current/bin/pyred" status --state "$STATE" >"$W/logs/status-v1.txt" 2>&1
export PYRE_UPGRADE_RESTART_HOOK="$I/rehearsal-supervisor.sh"

# ---- tampered upgrade -> refused before staging ----
log "upgrade to the tampered $V2 tarball -> refused"
case_ upgrade-tampered-refused 3 "$BASH" "$I/upgrade.sh" --tarball "$TT" --home "$FH" --service node
assert "current still $V1, $V2 not staged" test "$(cur)" = "$V1" -a ! -e "$FH/.local/share/pyre/$V2"

# ---- upgrade v1 -> v2 ----
log "upgrade $V1 -> $V2"
case_ upgrade-v2 0 "$BASH" "$I/upgrade.sh" --tarball "$T2" --home "$FH" --service node --health-timeout 30
assert "current -> $V2" test "$(cur)" = "$V2"
assert ".previous = $V1" test "$(cat "$FH/.local/share/pyre/.previous")" = "$V1"
"$FH/.local/share/pyre/current/bin/pyred" status --state "$STATE" >"$W/logs/status-v2.txt" 2>&1

# ---- failing upgrade -> automatic rollback ----
log "failing build $V3 -> automatic rollback"
B3="$W/bad-3"; mkdir -p "$B3/bin"
cp "$X2/bin/pyre" "$X2/bin/pyre-hub" "$B3/bin/"; cp "$X2/bin/pyred" "$B3/bin/pyred.real"
cat >"$B3/bin/pyred" <<EOF
#!/bin/bash
# SYNTHETIC failing build for the rehearsal: passes version --json, refuses to run.
case "\${1:-}" in
  version) echo '{"version":"$V3","protocol":1,"min_protocol":1}' ;;
  run) echo "synthetic failure: this build refuses to run" >&2; exit 1 ;;
  *) exec "$B3/bin/pyred.real" "\$@" ;;
esac
EOF
chmod 755 "$B3/bin/pyred"; echo "$V3" >"$B3/VERSION"
case_ upgrade-bad-rolls-back 5 "$BASH" "$I/upgrade.sh" --from "$B3" --allow-unsigned --home "$FH" --service node --health-timeout 12
assert "current back to $V2" test "$(cur)" = "$V2"
assert "$V3 marked failed" test -f "$FH/.local/share/pyre/$V3/.failed"
assert ".previous still $V1 (failed build is not a rollback target)" test "$(cat "$FH/.local/share/pyre/.previous")" = "$V1"

# ---- incompatible protocol -> refused before switching ----
log "incompatible protocol build $V4 -> refused"
B4="$W/bad-4"; mkdir -p "$B4/bin"
cp "$X2/bin/pyre-hub" "$B4/bin/"
cp "$X2/bin/pyre" "$B4/bin/pyre.real"
cat >"$B4/bin/pyre" <<EOF
#!/bin/bash
# SYNTHETIC protocol-99 CLI: the protocol-1 hub refuses its hello (as a real one does).
case "\${1:-}" in
  hello) echo "pyre: protocol_mismatch: client protocol 99 not supported; hub supports 1..1" >&2; exit 6 ;;
  *) exec "$B4/bin/pyre.real" "\$@" ;;
esac
EOF
chmod 755 "$B4/bin/pyre"
cat >"$B4/bin/pyred" <<EOF
#!/bin/bash
# SYNTHETIC build that claims protocol 99 for the rehearsal.
case "\${1:-}" in
  version) echo '{"version":"$V4","protocol":99,"min_protocol":99}' ;;
  *) echo "synthetic" >&2; exit 1 ;;
esac
EOF
chmod 755 "$B4/bin/pyred"; echo "$V4" >"$B4/VERSION"
case_ upgrade-protocol-mismatch-refused 4 "$BASH" "$I/upgrade.sh" --from "$B4" --allow-unsigned --home "$FH" --service node
assert "current still $V2" test "$(cur)" = "$V2"
case_ upgrade-explicit-hub-protocol-refused 4 "$BASH" "$I/upgrade.sh" --from "$B4" --allow-unsigned --home "$FH" --service node --hub-protocol 1:1

# ---- manual rollback ----
log "manual rollback $V2 -> $V1"
case_ rollback-manual 0 "$BASH" "$I/upgrade.sh" --rollback --home "$FH" --service node --health-timeout 30
assert "current -> $V1" test "$(cur)" = "$V1"

# ---- run-loss guard (mock runs are in-process and do not survive a restart) ----
log "run-loss guard: a live mock run, then upgrade"
"$X1/bin/pyre" start --node rehearsal-mac --harness mock --cwd "$W" --title run-loss-guard --json >"$W/logs/session.json" 2>&1
sleep 2
"$FH/.local/share/pyre/current/bin/pyred" status --state "$STATE" --json >"$W/logs/status-before-guard.json"
log "live runs before: $(j "$W/logs/status-before-guard.json" "d['live_runs']")"
case_ upgrade-detects-run-loss 5 "$BASH" "$I/upgrade.sh" --tarball "$T2" --home "$FH" --service node --health-timeout 8
assert "current back to $V1 after the run-loss rollback" test "$(cur)" = "$V1"
"$X1/bin/pyre" start --node rehearsal-mac --harness mock --cwd "$W" --title run-loss-allowed --json >"$W/logs/session2.json" 2>&1
sleep 2
case_ upgrade-allow-run-loss 0 "$BASH" "$I/upgrade.sh" --tarball "$T2" --home "$FH" --service node --health-timeout 8 --allow-run-loss
assert "current -> $V2" test "$(cur)" = "$V2"

# ---- bootstrap wrapper (dry run only) ----
log "bootstrap-node.sh dry run (no ticket, no ssh)"
mkdir -p "$W/xl"; tar -xzf "$TL1" -C "$W/xl"
case_ bootstrap-dry-run 0 env PYRE="$X1/bin/pyre" "$BASH" "$I/bootstrap-node.sh" --ssh rehearsal-alias --name rehearsal-linux --target linux/amd64 --dist "$W/xl" --hub-url https://127.0.0.1:7443

# ---- uninstall ----
log "uninstall (keep data), purge list, purge --yes"
case_ uninstall 0 "$BASH" "$I/uninstall.sh" --home "$FH"
assert "plist removed" test ! -e "$PL"
assert "shims removed" test ! -e "$FH/.local/bin/pyre" -a ! -L "$FH/.local/bin/pyre"
assert "version dirs removed" test ! -e "$FH/.local/share/pyre"
assert "node state kept" test -f "$STATE/node-credential.json" -a -f "$STATE/pyred.db"
assert "hive/outfit/rota untouched" test "$(cat "$FH/.local/bin/hive" "$FH/.local/bin/outfit" "$FH/.local/bin/rota" | shasum -a 256)" = "$OTHERS_BEFORE"
assert "PATH block removed from zshrc" sh -c "! grep -q 'pyre PATH' '$FH/.zshrc' && grep -q 'synthetic zshrc' '$FH/.zshrc'"
assert "pyred stopped" sh -c "! test -f '$W/run/node.pid'"
cp -R "$FH/Library/Logs/pyre" "$W/logs/service-logs"
# macOS: the CLI's own state and the default profile root with its shared/
# transcript store live in ~/.local/state/pyre, outside the node state dir.
CS="$FH/.local/state/pyre"
mkdir -p "$CS/cache" "$CS/drafts" "$CS/profiles/acct-synthetic-2/claude" "$CS/shared/projects/synthetic"
echo "synthetic cache entry" >"$CS/cache/blob"
echo "synthetic draft" >"$CS/drafts/doc.md"
echo "synthetic profile marker" >"$CS/profiles/acct-synthetic-2/claude/.keep"
echo '{"synthetic":true}' >"$CS/shared/projects/synthetic/session.jsonl"
case_ purge-without-yes 1 "$BASH" "$I/uninstall.sh" --home "$FH" --purge
assert "purge without --yes deleted nothing" test -f "$STATE/pyred.db" -a -f "$PYRE_CONFIG" -a -f "$CS/cache/blob"
assert "purge list names the CLI state and reports the kept shared store" sh -c "grep -q '$CS/cache' '$W/logs/'*-purge-without-yes.txt && grep -q '$CS/drafts' '$W/logs/'*-purge-without-yes.txt && grep -q 'kept: $CS/shared (transcripts' '$W/logs/'*-purge-without-yes.txt"
case_ purge-yes 0 "$BASH" "$I/uninstall.sh" --home "$FH" --purge --yes
assert "state deleted" test ! -e "$STATE/pyred.db" -a ! -e "$STATE/node-credential.json"
assert "client config deleted" test ! -e "$PYRE_CONFIG"
assert "CLI cache and drafts deleted" test ! -e "$CS/cache" -a ! -e "$CS/drafts"
assert "profiles kept" test -f "$STATE/profiles/acct-synthetic/claude/.keep" -a -f "$CS/profiles/acct-synthetic-2/claude/.keep"
assert "shared transcript store kept with the profiles" test -f "$CS/shared/projects/synthetic/session.jsonl"
assert "android signing properties kept" test -f "$FH/.config/pyre/android-signing.properties"
case_ purge-include-profiles 0 "$BASH" "$I/uninstall.sh" --home "$FH" --purge --yes --include-profiles
assert "profiles and shared store deleted with --include-profiles" test ! -e "$STATE/profiles" -a ! -e "$CS/profiles" -a ! -e "$CS/shared"
assert "empty CLI state dir removed" test ! -e "$CS"
assert "android signing properties still kept" test -f "$FH/.config/pyre/android-signing.properties"

# ---- Linux paths, render only, in a fake root ----
log "Linux render-only (fake root)"
unset PYRE_UPGRADE_RESTART_HOOK
LR="$W/linux-root"; LH=/home/pyre
case_ linux-install-node 0 "$BASH" "$I/install.sh" --root "$LR" --os linux --arch amd64 --home "$LH" --user pyre --tarball "$TL1" --service node --name server-test --ssh-alias server --allowed-signers "$SA" --pyred-arg --rota-checkout --pyred-arg --trust-dir --pyred-arg "/home/pyre/work dir"
case_ linux-install-hub 0 "$BASH" "$I/install.sh" --root "$LR" --os linux --arch amd64 --home "$LH" --user pyre --tarball "$TL1" --service hub --hub-url https://hub.example.invalid:7443 --hub-listen :7443
case_ linux-install-hub-no-harden 0 "$BASH" "$I/install.sh" --root "$W/linux-root-2" --os linux --arch amd64 --home "$LH" --user pyre --tarball "$TL1" --service hub --no-harden --memory-max 512M --allowed-signers "$SA"
case_ linux-install-node-capped 0 "$BASH" "$I/install.sh" --root "$W/linux-root-2" --os linux --arch amd64 --home "$LH" --user pyre --tarball "$TL1" --service node --memory-max 8G --cpu-quota 400%
cp "$LR$LH/.config/systemd/user/pyred.service" "$W/logs/rendered-pyred.service"
cp "$LR$LH/.config/systemd/user/pyre-hub.service" "$W/logs/rendered-pyre-hub.service"
cp "$W/linux-root-2$LH/.config/systemd/user/pyre-hub.service" "$W/logs/rendered-pyre-hub-no-harden.service"
cp "$W/linux-root-2$LH/.config/systemd/user/pyred.service" "$W/logs/rendered-pyred-capped.service"
cp "$LR$LH/.config/systemd/user/pyre-runs.slice" "$W/logs/rendered-pyre-runs.slice"
U="$LR$LH/.config/systemd/user/pyred.service"
assert "pyred.service carries the --pyred-arg words (quoted)" grep -q '"--rota-checkout" "--trust-dir" "/home/pyre/work dir"' "$U"
assert "pyred.service (scope mode) uses the default KillMode" sh -c "! grep -q '^KillMode=' '$U'"
assert "pyred.service requires the run scope" grep -qx 'Environment=PYRE_SYSTEMD_SCOPE=require' "$U"
assert "pyred.service caps pyred (MemoryMax=1G, CPUQuota=200%)" sh -c "grep -qx 'MemoryMax=1G' '$U' && grep -qx 'CPUQuota=200%' '$U'"
assert "pyred.service has no namespace sandbox or NoNewPrivileges (inherited by runs)" sh -c "! grep -Eq '^(ProtectSystem|ProtectHome|PrivateTmp|NoNewPrivileges)=' '$U'"
assert "pyre-runs.slice rendered (MemoryHigh=85%, CPUWeight=90)" sh -c "grep -qx 'MemoryHigh=85%' '$LR$LH/.config/systemd/user/pyre-runs.slice' && grep -qx 'CPUWeight=90' '$LR$LH/.config/systemd/user/pyre-runs.slice'"
case_ linux-install-node-legacy 0 "$BASH" "$I/install.sh" --root "$W/linux-root-3" --os linux --arch amd64 --home "$LH" --user pyre --tarball "$TL1" --service node --no-scope --allowed-signers "$SA"
UL="$W/linux-root-3$LH/.config/systemd/user/pyred.service"
cp "$UL" "$W/logs/rendered-pyred-legacy.service"
assert "legacy pyred.service: KillMode=process, scope off, no caps, no slice" sh -c "grep -qx 'KillMode=process' '$UL' && grep -qx 'Environment=PYRE_SYSTEMD_SCOPE=off' '$UL' && ! grep -Eq '^(MemoryMax|CPUQuota)=' '$UL' && test ! -e '$W/linux-root-3$LH/.config/systemd/user/pyre-runs.slice'"
assert "pyre-hub.service is sandboxed and capped" sh -c "grep -qx 'ProtectSystem=strict' '$LR$LH/.config/systemd/user/pyre-hub.service' && grep -qx 'MemoryMax=1G' '$LR$LH/.config/systemd/user/pyre-hub.service'"
case_ linux-upgrade 0 "$BASH" "$I/upgrade.sh" --root "$LR" --os linux --arch amd64 --home "$LH" --tarball "$TL2"
assert "linux current -> $V2" test "$(basename "$(readlink "$LR$LH/.local/share/pyre/current")")" = "$V2"
case_ linux-rollback 0 "$BASH" "$I/upgrade.sh" --root "$LR" --os linux --arch amd64 --home "$LH" --rollback
assert "linux current -> $V1" test "$(basename "$(readlink "$LR$LH/.local/share/pyre/current")")" = "$V1"
case_ linux-uninstall 0 "$BASH" "$I/uninstall.sh" --root "$LR" --os linux --home "$LH"
assert "linux units and slice removed" test ! -e "$LR$LH/.config/systemd/user/pyred.service" -a ! -e "$LR$LH/.config/systemd/user/pyre-hub.service" -a ! -e "$LR$LH/.config/systemd/user/pyre-runs.slice"

log "secrets check"
(umask 077; j "$W/hub/owner-credential.json" "d['token']" >"$W/tokens.pat"; cat "$W/node-token.pat" >>"$W/tokens.pat")
if grep -rlF -f "$W/tokens.pat" "$W/logs" "$FH/Library/Logs" >/dev/null 2>&1; then log "FAIL a token appears in a log"; FAILS=$((FAILS + 1)); else log "PASS owner and node tokens absent from all case logs and service logs"; fi
log "done: $N cases, $FAILS failure(s); logs in $W/logs"
exit "$FAILS"
