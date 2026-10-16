#!/bin/bash
# Pyre cloud node bootstrap: the GCE startup-script of every VM made by
# `pyre node create` / `pyre node image bake`. It runs as root on every boot
# and is idempotent; its output goes to the serial console
# (gcloud compute instances get-serial-port-output <vm> --zone <z>).
#
# Non-secret settings come from the pyre-config metadata (JSON). The two
# secrets -- the one-time hub enrollment secret (pyre-enroll-secret) and the
# single-use ephemeral Tailscale auth key (pyre-ts-authkey) -- are read from
# metadata straight into 0600 files under /run/pyre-bootstrap (tmpfs), handed
# over by file or stdin only (never argv, never echoed), and deleted on exit.
# The CLI that created the VM removes both keys from the metadata once the
# node has connected; by then both are spent.
#
# Modes: node (join the tailnet, verify+install the release, enroll, start
# pyred) and bake (install everything except identity, then power off so the
# disk can become an image).
set -euo pipefail
set +x
umask 077
export DEBIAN_FRONTEND=noninteractive

log() { echo "pyre-bootstrap: $*"; }
die() { log "FAILED: $*"; exit 1; }

MD=http://metadata.google.internal/computeMetadata/v1/instance
md() { curl -fsS --retry 5 --retry-connrefused -H 'Metadata-Flavor: Google' "$MD/$1"; }
# md_secret KEY FILE: a secret goes from metadata into FILE, never to stdout.
md_secret() { curl -fsS --retry 5 --retry-connrefused -H 'Metadata-Flavor: Google' -o "$2" "$MD/attributes/$1" 2>/dev/null; }

RUN=/run/pyre-bootstrap
rm -rf "$RUN"
mkdir -m 700 -p "$RUN"
trap 'rm -rf "$RUN"' EXIT
STATE=/var/lib/pyre-bootstrap
mkdir -m 700 -p "$STATE" /etc/pyre

md attributes/pyre-config >"$RUN/config.json" || die "no pyre-config metadata"
cfg() { python3 -c 'import json,sys
v = json.load(open(sys.argv[1])).get(sys.argv[2], "")
if isinstance(v, bool): v = "1" if v else ""
elif isinstance(v, list): v = "\n".join(str(x) for x in v)
print(v)' "$RUN/config.json" "$1"; }

MODE=$(cfg mode); MODE=${MODE:-node}
NAME=$(cfg name)
PUSER=$(cfg user); PUSER=${PUSER:-pyre}
HUB_URL=$(cfg hub_url)
HUB_PIN=$(cfg hub_pin)
NODE_ID=$(cfg node_id)
REL_URL=$(cfg release_url); REL_URL=${REL_URL%/}
REL_VER=$(cfg release_version)
ALLOW_UNSIGNED=$(cfg allow_unsigned)
WITH_TOOLS=$(cfg with_tools)
TS_HOSTNAME=$(cfg ts_hostname)
TS_SSH=$(cfg ts_ssh)
SSH_ALIAS=$(cfg ssh_alias)
HARNESSES=$(cfg harnesses)
TOOLCHAINS=$(cfg toolchains)
case "$(uname -m)" in x86_64) ARCH=amd64 ;; aarch64 | arm64) ARCH=arm64 ;; *) die "unsupported arch $(uname -m)" ;; esac
log "mode=$MODE node=$NAME user=$PUSER release=$REL_VER arch=$ARCH toolchains=$(echo $TOOLCHAINS)"

# ---- base packages and the node user ----
if [ ! -f "$STATE/base.done" ]; then
  apt-get update -q
  apt-get install -y -q tmux git curl jq ca-certificates python3 openssh-client pciutils
  touch "$STATE/base.done"
fi
if ! id -u "$PUSER" >/dev/null 2>&1; then
  useradd -m -s /bin/bash "$PUSER"
  log "created user $PUSER"
fi
PHOME=$(getent passwd "$PUSER" | cut -d: -f6)
PUID=$(id -u "$PUSER")
loginctl enable-linger "$PUSER"
for _ in $(seq 1 30); do [ -S "/run/user/$PUID/bus" ] && break; sleep 1; done
UPATH="$PHOME/.local/bin:/usr/local/bin:/usr/bin:/bin"
# as_user CMD...: run as the node user with a clean, predictable environment.
as_user() { runuser -u "$PUSER" -- env -i HOME="$PHOME" USER="$PUSER" LOGNAME="$PUSER" PATH="$UPATH" \
  XDG_RUNTIME_DIR="/run/user/$PUID" DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/$PUID/bus" LANG=C.UTF-8 "$@"; }
printf 'PYRE_USER=%s\nPYRE_HOME=%s\n' "$PUSER" "$PHOME" >/etc/pyre/cloud.env
chmod 644 /etc/pyre/cloud.env

# ---- Tailscale (installed in both modes; joined only in node mode) ----
if ! command -v tailscale >/dev/null 2>&1; then
  curl -fsSL https://tailscale.com/install.sh -o "$RUN/tailscale-install.sh"
  sh "$RUN/tailscale-install.sh"
fi
systemctl enable --now tailscaled >/dev/null 2>&1 || true
mkdir -p /usr/local/lib/pyre
cat >/usr/local/lib/pyre/tailscale-logout.sh <<'EOF'
#!/bin/sh
# Power-off (not reboot): leave the tailnet so the ephemeral device is gone
# at once; `pyre node start` supplies a fresh single-use key.
if systemctl list-jobs 2>/dev/null | grep -q 'reboot.target'; then exit 0; fi
tailscale logout >/dev/null 2>&1 || true
EOF
chmod 755 /usr/local/lib/pyre/tailscale-logout.sh
cat >/etc/systemd/system/pyre-tailscale-logout.service <<'EOF'
[Unit]
Description=Pyre: log the ephemeral node out of the tailnet at power-off
After=tailscaled.service network-online.target
Wants=network-online.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/bin/true
ExecStop=/usr/local/lib/pyre/tailscale-logout.sh
TimeoutStopSec=20

[Install]
WantedBy=multi-user.target
EOF

# ---- idle auto-stop (powers the VM off; GCE then shows it TERMINATED with its disk kept) ----
cat >/usr/local/lib/pyre/idle-stop.sh <<'EOF'
#!/bin/bash
# Pyre idle auto-stop: no live runs and nobody logged in for
# pyre-idle-minutes (instance metadata, read every check; 0 disables).
set -uo pipefail
. /etc/pyre/cloud.env
MD=http://metadata.google.internal/computeMetadata/v1/instance/attributes
min=$(curl -fsS -H 'Metadata-Flavor: Google' "$MD/pyre-idle-minutes" 2>/dev/null || echo 30)
case "$min" in '' | *[!0-9]*) min=30 ;; esac
STAMP=/run/pyre-idle/since
mkdir -p /run/pyre-idle
if [ "$min" -eq 0 ]; then rm -f "$STAMP"; exit 0; fi
uid=$(id -u "$PYRE_USER")
st="$PYRE_HOME/.local/state/pyre"
busy=""
if out=$(runuser -u "$PYRE_USER" -- env HOME="$PYRE_HOME" "$PYRE_HOME/.local/bin/pyred" status --json --state "$st" 2>/dev/null); then
  live=$(printf '%s' "$out" | jq -r '.live_runs // 0' 2>/dev/null || echo 1)
  [ "${live:-1}" = 0 ] || busy="$live live run(s)"
elif runuser -u "$PYRE_USER" -- tmux -S "$st/tmux/pyre.sock" ls >/dev/null 2>&1; then
  busy="pyred not answering but its tmux server has sessions"
fi
[ -z "$(who)" ] || busy="${busy:+$busy; }a user is logged in"
if [ -n "$busy" ]; then rm -f "$STAMP"; exit 0; fi
[ -f "$STAMP" ] || date +%s >"$STAMP"
idle=$(( $(date +%s) - $(cat "$STAMP") ))
if [ "$idle" -ge $(( min * 60 )) ]; then
  msg="pyre-idle-stop: no live runs for $min minutes; powering off (disk kept; pyre node start resumes)"
  echo "$msg" >/dev/console 2>/dev/null || true
  logger -t pyre-idle-stop "$msg"
  systemctl poweroff
fi
EOF
chmod 755 /usr/local/lib/pyre/idle-stop.sh
cat >/etc/systemd/system/pyre-idle-stop.service <<'EOF'
[Unit]
Description=Pyre idle auto-stop check

[Service]
Type=oneshot
ExecStart=/usr/local/lib/pyre/idle-stop.sh
EOF
cat >/etc/systemd/system/pyre-idle-stop.timer <<'EOF'
[Unit]
Description=Pyre idle auto-stop (every minute)

[Timer]
OnBootSec=2min
OnUnitActiveSec=1min
AccuracySec=10s

[Install]
WantedBy=timers.target
EOF
systemctl daemon-reload

# ---- join the tailnet (node mode) ----
ts_state() { tailscale status --json 2>/dev/null | jq -r '.BackendState // ""' 2>/dev/null || true; }
if [ "$MODE" = node ]; then
  if [ "$(ts_state)" != Running ]; then
    md_secret pyre-ts-authkey "$RUN/ts.key" || die "not logged in to the tailnet and no pyre-ts-authkey in metadata (pyre node start supplies one)"
    set -- --auth-key="file:$RUN/ts.key" --hostname="$TS_HOSTNAME"
    [ -z "$TS_SSH" ] || set -- "$@" --ssh
    tailscale up "$@" || { rm -f "$RUN/ts.key"; die "tailscale up failed (expired or used key?)"; }
    rm -f "$RUN/ts.key"
    log "joined the tailnet as $TS_HOSTNAME"
  fi
  systemctl enable --now pyre-tailscale-logout.service >/dev/null
  ok=""
  for _ in $(seq 1 60); do
    # Reachability only (the pin is checked by pyred); no credential is sent.
    if curl -fsS -k --max-time 5 -o /dev/null "$HUB_URL/healthz"; then ok=1; break; fi
    sleep 2
  done
  [ -n "$ok" ] && log "hub reachable at $HUB_URL" || log "WARNING: hub $HUB_URL not reachable yet (Tailscale ACL? hub listen address?); pyred keeps retrying"
fi

# ---- developer toolchains (gcp.toolchains / --toolchain, resolved by the CLI) ----
# Each id installs idempotently (skipped when the requested version is
# present) where the node user's sessions and pyred's service PATH find it:
# /usr/local/bin (node, npm, corepack, pnpm, bun), distro packages (git,
# python, cmake, llvm, build-essential, gh from GitHub's signed apt repo) and
# the user's ~/.cargo with links in ~/.local/bin (rust). Downloads are
# checked against the vendor's published SHA-256: Node SHASUMS256.txt, Bun
# SHASUMS256.txt, rustup-init.sha256. Node mode: a failure is logged and the
# node comes up without that tool (every boot retries); bake mode: it fails
# the bake. Runs in a subshell with umask 022 so the user can read the tools.
TOOLS=/opt/pyre-tools
apt_install() { apt-get install -y -q "$@" || { apt-get update -q && apt-get install -y -q "$@"; }; }
# sum_for SUMS NAME: the SHA-256 a SHASUMS file lists for NAME.
sum_for() { awk -v f="$2" '{n=$2; sub(/^\*/, "", n); if (n == f) print $1}' "$1" | head -n 1; }
# check_sha FILE WANT WHAT
check_sha() {
  [ -n "$2" ] || { log "$3: no published SHA-256 to check against"; return 1; }
  [ "$(sha256sum "$1" | cut -d' ' -f1)" = "$2" ] || { log "$3: SHA-256 mismatch"; return 1; }
  log "$3: SHA-256 verified"
}
# tc_env NAME VALUE: for login shells (/etc/environment) and the user's
# systemd services (environment.d).
tc_env() {
  local f
  mkdir -p /etc/environment.d
  for f in /etc/environment /etc/environment.d/60-pyre-toolchains.conf; do
    touch "$f"
    if grep -q "^$1=" "$f"; then sed -i "s|^$1=.*|$1=$2|" "$f"; else echo "$1=$2" >>"$f"; fi
  done
}
tc_node() { # tc_node lts|MAJOR|X.Y.Z
  local want=${1:-lts} have ver ntb dir b NARCH=x64
  if [ "$ARCH" = arm64 ]; then NARCH=arm64; fi
  have=$(/usr/local/bin/node --version 2>/dev/null || true); have=${have#v}
  if [ -n "$have" ]; then
    case "$want" in
      lts) log "toolchain node@lts: v$have present"; return 0 ;;
      *.*.*) if [ "$have" = "$want" ]; then log "toolchain node@$want: present"; return 0; fi ;;
      *) case "$have" in "$want".*) log "toolchain node@$want: v$have present"; return 0 ;; esac ;;
    esac
  fi
  case "$want" in
    *.*.*) ver=v$want ;;
    *) ver=$(curl -fsS --retry 3 https://nodejs.org/dist/index.json | python3 -c 'import json,sys
w = sys.argv[1]
for r in json.load(sys.stdin):
    if (w == "lts" and r["lts"]) or r["version"].startswith("v" + w + "."):
        print(r["version"]); break' "$want") ;;
  esac
  [ -n "$ver" ] || { log "node@$want: no such release"; return 1; }
  ntb=node-$ver-linux-$NARCH.tar.xz
  curl -fsS --retry 3 -o "$RUN/node-SHASUMS256.txt" "https://nodejs.org/dist/$ver/SHASUMS256.txt"
  curl -fsS --retry 3 -o "$RUN/$ntb" "https://nodejs.org/dist/$ver/$ntb"
  check_sha "$RUN/$ntb" "$(sum_for "$RUN/node-SHASUMS256.txt" "$ntb")" "$ntb"
  dir=$TOOLS/node/$ver
  rm -rf "$dir"
  mkdir -p "$dir"
  tar -xJf "$RUN/$ntb" -C "$dir" --strip-components=1 --no-same-owner
  rm -f "$RUN/$ntb"
  # /opt/node: the current version (an unpinned copy from an older bootstrap is replaced).
  if [ -e /opt/node ] && [ ! -L /opt/node ]; then rm -rf /opt/node; fi
  ln -sfn "$dir" /opt/node
  for b in node npm npx corepack; do
    if [ -e "/opt/node/bin/$b" ]; then ln -sf "/opt/node/bin/$b" "/usr/local/bin/$b"; fi
  done
  log "toolchain node@$want: installed $ver ($dir, /usr/local/bin/node)"
}
tc_pnpm() { # tc_pnpm [X.Y.Z]: corepack; a repo's packageManager field picks the pnpm version
  local ver=${1:-}
  if [ ! -x /usr/local/bin/node ]; then tc_node lts; fi
  if [ ! -e /usr/local/bin/corepack ]; then
    /opt/node/bin/npm install -g -q corepack # Node 25+ no longer bundles it
    ln -sf /opt/node/bin/corepack /usr/local/bin/corepack
  fi
  if [ -e /usr/local/bin/pnpm ]; then
    log "toolchain pnpm: corepack shim present"
  else
    /usr/local/bin/corepack enable --install-directory /usr/local/bin pnpm
    log "toolchain pnpm: corepack enabled (/usr/local/bin/pnpm; the repo's packageManager field picks the version)"
  fi
  tc_env COREPACK_ENABLE_DOWNLOAD_PROMPT 0
  if [ -n "$ver" ]; then
    if [ -d "$PHOME/.cache/node/corepack/v1/pnpm/$ver" ]; then
      log "toolchain pnpm@$ver: present"
    else
      as_user env COREPACK_ENABLE_DOWNLOAD_PROMPT=0 corepack install -g "pnpm@$ver"
      log "toolchain pnpm@$ver: prepared for $PUSER (the default outside a repo with packageManager)"
    fi
  fi
}
tc_bun() { # tc_bun [X.Y.Z]: official release zip, SHASUMS256.txt-checked
  local ver=${1:-} have a base
  have=$(/usr/local/bin/bun --version 2>/dev/null || true)
  if [ -n "$have" ] && { [ -z "$ver" ] || [ "$have" = "$ver" ]; }; then log "toolchain bun: $have present"; return 0; fi
  if [ -z "$ver" ]; then
    ver=$(curl -fsSL --retry 3 https://api.github.com/repos/oven-sh/bun/releases/latest | python3 -c 'import json,sys; print(json.load(sys.stdin)["tag_name"].removeprefix("bun-v"))')
  fi
  a=bun-linux-x64
  if [ "$ARCH" = arm64 ]; then a=bun-linux-aarch64; fi
  base=https://github.com/oven-sh/bun/releases/download/bun-v$ver
  curl -fsSL --retry 3 -o "$RUN/bun-SHASUMS256.txt" "$base/SHASUMS256.txt"
  curl -fsSL --retry 3 -o "$RUN/$a.zip" "$base/$a.zip"
  check_sha "$RUN/$a.zip" "$(sum_for "$RUN/bun-SHASUMS256.txt" "$a.zip")" "bun $ver $a.zip"
  rm -rf "$RUN/bun-x"
  python3 -m zipfile -e "$RUN/$a.zip" "$RUN/bun-x"
  install -D -m 755 "$RUN/bun-x/$a/bun" "$TOOLS/bun/$ver/bun"
  ln -sf "$TOOLS/bun/$ver/bun" /usr/local/bin/bun
  ln -sf "$TOOLS/bun/$ver/bun" /usr/local/bin/bunx
  rm -rf "$RUN/bun-x" "$RUN/$a.zip"
  log "toolchain bun@$ver: installed ($TOOLS/bun/$ver, /usr/local/bin/bun)"
}
tc_rust() { # tc_rust [CHANNEL]: rustup for the node user, rustup-init.sha256-checked
  local ch=${1:-stable} triple u b rup="$PHOME/.cargo/bin/rustup"
  command -v cc >/dev/null 2>&1 || apt_install build-essential
  if [ -x "$rup" ]; then
    if as_user "$rup" toolchain list | grep "^$ch-" >/dev/null; then
      log "toolchain rust@$ch: present"
    else
      as_user "$rup" toolchain install "$ch" --profile default
      log "toolchain rust@$ch: installed"
    fi
    as_user "$rup" default "$ch" >/dev/null
  else
    case "$ARCH" in amd64) triple=x86_64-unknown-linux-gnu ;; *) triple=aarch64-unknown-linux-gnu ;; esac
    u=https://static.rust-lang.org/rustup/dist/$triple/rustup-init
    curl -fsS --retry 3 -o "$RUN/rustup-init" "$u"
    curl -fsS --retry 3 -o "$RUN/rustup-init.sha256" "$u.sha256"
    check_sha "$RUN/rustup-init" "$(cut -d' ' -f1 "$RUN/rustup-init.sha256")" rustup-init
    install -o "$PUSER" -g "$PUSER" -m 755 "$RUN/rustup-init" "$PHOME/.pyre-rustup-init"
    as_user "$PHOME/.pyre-rustup-init" -y --profile default --default-toolchain "$ch"
    rm -f "$PHOME/.pyre-rustup-init" "$RUN/rustup-init" "$RUN/rustup-init.sha256"
    log "toolchain rust@$ch: installed with rustup for $PUSER (~/.cargo)"
  fi
  # pyred's service PATH has ~/.local/bin, not ~/.cargo/bin.
  as_user mkdir -p "$PHOME/.local/bin"
  for b in "$PHOME"/.cargo/bin/*; do as_user ln -sf "$b" "$PHOME/.local/bin/${b##*/}"; done
}
tc_gh() { # GitHub's signed apt repository
  if command -v gh >/dev/null 2>&1; then log "toolchain gh: present"; return 0; fi
  mkdir -p -m 755 /etc/apt/keyrings
  curl -fsSL -o /etc/apt/keyrings/githubcli-archive-keyring.gpg https://cli.github.com/packages/githubcli-archive-keyring.gpg
  chmod 644 /etc/apt/keyrings/githubcli-archive-keyring.gpg
  echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" >/etc/apt/sources.list.d/github-cli.list
  apt-get update -q && apt-get install -y -q gh
  log "toolchain gh: installed (apt, cli.github.com)"
}
tc_apt() { # tc_apt ID VERSION CHECK PKG...: distro packages
  local id=$1 ver=$2 check=$3
  shift 3
  if [ -n "$ver" ]; then log "toolchain $id: version $ver applies on Windows; Linux uses the distro package"; fi
  if command -v "$check" >/dev/null 2>&1; then log "toolchain $id: present ($check)"; return 0; fi
  apt_install "$@"
  log "toolchain $id: installed $* (apt)"
}
# tc_one ID[@VERSION]: one toolchain, in a subshell (errexit, umask 022).
tc_one() (
  set -e
  umask 022
  name=${1%%@*} ver=""
  if [ "$name" != "$1" ]; then ver=${1#*@}; fi
  mkdir -p -m 755 "$TOOLS"
  case "$name" in
    node) tc_node "$ver" ;;
    pnpm) tc_pnpm "$ver" ;;
    bun) tc_bun "$ver" ;;
    rust) tc_rust "$ver" ;;
    gh) tc_gh ;;
    git) tc_apt git "" git git ;;
    python) tc_apt python "$ver" python python3 python3-venv python3-pip python-is-python3 ;;
    vs-buildtools) tc_apt vs-buildtools "" g++ build-essential ;; # the native C/C++ toolchain
    cmake) tc_apt cmake "$ver" cmake cmake ;;
    llvm) tc_apt llvm "$ver" clang llvm clang libclang-dev ;;
    *) log "toolchain $1: unknown id"; exit 1 ;;
  esac
)
install_toolchains() {
  local t rc failed=""
  while IFS= read -r t; do
    [ -n "$t" ] || continue
    set +e
    tc_one "$t"
    rc=$?
    set -e
    if [ "$rc" -ne 0 ]; then
      [ "$MODE" != bake ] || die "toolchain $t failed (exit $rc)"
      log "WARNING: toolchain $t failed (exit $rc); the node comes up without it (the next boot retries)"
      failed="$failed $t"
    fi
  done <<<"$TOOLCHAINS"
  log "toolchains: $(echo $TOOLCHAINS)${failed:+ (failed:$failed)}"
}

# ---- later boots: already installed and enrolled; pyred starts by itself (linger) ----
CRED="$PHOME/.local/state/pyre/node-credential.json"
if [ "$MODE" = node ] && [ -f "$STATE/installed-$REL_VER" ] && [ -f "$CRED" ]; then
  install_toolchains
  systemctl enable --now pyre-idle-stop.timer >/dev/null
  log "node $NAME ready (release $REL_VER already installed)"
  exit 0
fi

# ---- fetch and verify the signed release (exactly like install.sh) ----
fetch() { # fetch URL DEST
  case "$1" in
    gs://*)
      local rest=${1#gs://}; local bucket=${rest%%/*}; local obj=${rest#*/}
      local enc; enc=$(python3 -c 'import sys,urllib.parse; print(urllib.parse.quote(sys.argv[1], safe=""))' "$obj")
      md service-accounts/default/token | python3 -c 'import json,sys; print("Authorization: Bearer " + json.load(sys.stdin)["access_token"])' >"$RUN/gcs.hdr"
      curl -fsS --retry 3 -H "@$RUN/gcs.hdr" -o "$2" "https://storage.googleapis.com/storage/v1/b/$bucket/o/$enc?alt=media"
      rm -f "$RUN/gcs.hdr" ;;
    https://*) curl -fsS --retry 3 -o "$2" "$1" ;;
    *) die "release url must be gs:// or https://" ;;
  esac
}
CACHE="$PHOME/.cache/pyre-release/$REL_VER"
TB="pyre-$REL_VER-linux-$ARCH.tar.gz"
TOOLS_TB="pyre-tools-$REL_VER-linux-$ARCH.tar.gz"
cfg allowed_signers >/etc/pyre/release-allowed-signers
chmod 644 /etc/pyre/release-allowed-signers
install -d -o "$PUSER" -g "$PUSER" -m 700 "$PHOME/.cache" "$PHOME/.cache/pyre-release" "$CACHE"
DL="$RUN/release"; mkdir -p "$DL"
fetch "$REL_URL/$TB" "$DL/$TB"
fetch "$REL_URL/SHA256SUMS" "$DL/SHA256SUMS"
fetch "$REL_URL/SHA256SUMS.sig" "$DL/SHA256SUMS.sig" || [ -n "$ALLOW_UNSIGNED" ] || die "release is not signed (no SHA256SUMS.sig)"
[ -z "$WITH_TOOLS" ] || fetch "$REL_URL/$TOOLS_TB" "$DL/$TOOLS_TB"
UNSIGNED_FLAG=""
if [ -s /etc/pyre/release-allowed-signers ] && [ -f "$DL/SHA256SUMS.sig" ]; then
  ssh-keygen -Y verify -f /etc/pyre/release-allowed-signers -I pyre-release -n pyre-release \
    -s "$DL/SHA256SUMS.sig" <"$DL/SHA256SUMS" >"$RUN/verify.out" 2>&1 \
    || { cat "$RUN/verify.out"; die "release signature verification FAILED"; }
  log "release signature verified: $(head -n 1 "$RUN/verify.out")"
elif [ -n "$ALLOW_UNSIGNED" ]; then
  log "WARNING: release signature NOT verified (allow_unsigned)"
  UNSIGNED_FLAG=--allow-unsigned
else
  die "no release key configured: refusing an unverified release"
fi
want=$(awk -v f="$TB" '{n=$2; sub(/^\*/, "", n); if (n == f) print $1}' "$DL/SHA256SUMS")
got=$(sha256sum "$DL/$TB" | cut -d' ' -f1)
[ -n "$want" ] && [ "$got" = "$want" ] || die "tarball checksum mismatch or not listed in SHA256SUMS"
log "tarball checksum verified"
cp "$DL"/* "$CACHE/"
chown -R "$PUSER:$PUSER" "$CACHE"
EX="$RUN/extract"; mkdir -p "$EX"
tar -xzf "$DL/$TB" -C "$EX"
REL=$(find "$EX" -mindepth 1 -maxdepth 1 -type d | head -n 1)
[ -x "$REL/bin/pyred" ] || die "release has no bin/pyred"
chown -R "$PUSER:$PUSER" "$EX"
chmod 755 "$RUN" "$EX"

# ---- enroll (node mode, once; consumes the one-time ticket early) ----
if [ "$MODE" = node ] && [ ! -f "$CRED" ]; then
  as_user mkdir -p "$PHOME/.local/state/pyre"
  md_secret pyre-enroll-secret "$RUN/enroll.secret" || die "no pyre-enroll-secret in metadata (expired? re-create the node)"
  chown "$PUSER" "$RUN/enroll.secret"
  set -- --hub "$HUB_URL" --node-id "$NODE_ID" --credential-file "$CRED" --secret-stdin
  [ -z "$HUB_PIN" ] || set -- "$@" --pin "$HUB_PIN"
  as_user "$REL/bin/pyred" enroll "$@" <"$RUN/enroll.secret" || { rm -f "$RUN/enroll.secret"; die "enrollment failed"; }
  rm -f "$RUN/enroll.secret"
fi

# ---- toolchains and harnesses (skipped when present, e.g. on a baked image) ----
install_toolchains
want_h() { printf '%s\n' "$HARNESSES" | grep -qx "$1"; }
if lspci 2>/dev/null | grep -qi nvidia && ! command -v nvidia-smi >/dev/null 2>&1; then
  log "NVIDIA GPU found: installing drivers (ubuntu-drivers)"
  apt-get install -y -q ubuntu-drivers-common && ubuntu-drivers install --gpgpu || log "WARNING: GPU driver install failed"
fi
# Claude Code and Codex need node: 22 unless a toolchain installed one.
if { want_h claude || want_h codex; } && ! command -v node >/dev/null 2>&1; then
  tc_one node@22 # fatal (errexit), as before
fi
pkgs=""
# A harness counts as installed only when it runs: npm 11+ skips install
# scripts it was not told to allow, leaving Claude Code without its binary.
harness_works() { [ -x "$PHOME/.local/bin/$1" ] && as_user "$PHOME/.local/bin/$1" --version >/dev/null 2>&1; }
want_h claude && ! harness_works claude && pkgs="$pkgs @anthropic-ai/claude-code"
want_h codex && ! harness_works codex && pkgs="$pkgs @openai/codex"
if [ -n "$pkgs" ]; then
  # shellcheck disable=SC2086
  as_user npm install -g -q --prefix "$PHOME/.local" --allow-scripts=@anthropic-ai/claude-code,@openai/codex $pkgs
  log "installed$pkgs"
fi
# gh (a harness tool) is in the toolchain list: the CLI adds it.

# ---- install the release (and the node service) as the node user ----
set -- --tarball "$CACHE/$TB" --allowed-signers /etc/pyre/release-allowed-signers
[ -z "$UNSIGNED_FLAG" ] || set -- "$@" "$UNSIGNED_FLAG"
[ -z "$WITH_TOOLS" ] || set -- "$@" --with-tools
if [ "$MODE" = node ]; then
  set -- "$@" --service node --name "$NAME"
  [ -z "$SSH_ALIAS" ] || set -- "$@" --ssh-alias "$SSH_ALIAS"
  # --permissions bypass in gcp.pyred_args is the owner's consent for this
  # Pyre-made node: record the dev user's acceptance of Claude's warning.
  if cfg pyred_args | grep -qx -- '--permissions' && cfg pyred_args | grep -qx bypass; then
    sf="$PHOME/.claude/settings.json"
    as_user mkdir -p "$PHOME/.claude"
    if ! grep -q '"skipDangerousModePermissionPrompt": *true' "$sf" 2>/dev/null; then
      as_user python3 - "$sf" <<'PY'
import json,sys,os
p=sys.argv[1]
try: d=json.load(open(p))
except Exception: d={}
d["skipDangerousModePermissionPrompt"]=True
open(p,"w").write(json.dumps(d,indent=2)); os.chmod(p,0o600)
PY
      log "recorded the bypass-permissions acceptance for the node user"
    fi
  fi
  while IFS= read -r a; do [ -z "$a" ] || set -- "$@" --pyred-arg "$a"; done <<<"$(cfg pyred_args)"
fi
as_user bash "$REL/scripts/install/install.sh" "$@"

if [ "$MODE" = bake ]; then
  # Leave no identity in the image: no tailnet state, no node credential,
  # no machine-id; ssh host keys are regenerated by the guest environment.
  systemctl disable --now pyre-idle-stop.timer >/dev/null 2>&1 || true
  tailscale logout >/dev/null 2>&1 || true
  systemctl stop tailscaled || true
  rm -rf /var/lib/tailscale/* "$PHOME/.local/state/pyre/node-credential.json" "$STATE"/installed-*
  apt-get clean
  truncate -s 0 /etc/machine-id
  rm -f /var/lib/dbus/machine-id
  command -v cloud-init >/dev/null 2>&1 && cloud-init clean --logs >/dev/null 2>&1 || true
  log "bake complete; powering off"
  sync
  systemctl poweroff
  exit 0
fi

touch "$STATE/installed-$REL_VER"
systemctl enable --now pyre-idle-stop.timer >/dev/null
log "node $NAME ready (pyred running as $PUSER; idle auto-stop $(md attributes/pyre-idle-minutes 2>/dev/null || echo 30) min)"
