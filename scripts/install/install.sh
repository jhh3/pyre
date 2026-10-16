#!/bin/bash
# Per-user Pyre install (idempotent). Never needs root; never edits shell rc
# files unless --edit-path; never overwrites files it does not own.
#
#   install.sh --from DIR | --tarball FILE [--sums SHA256SUMS] [--sig SHA256SUMS.sig] [--version V]
#              [--allowed-signers FILE] [--allow-unsigned]
#              [--service none|node|hub] [--no-activate] [--no-prune] [--edit-path]
#              [node: --state DIR --credential FILE --name NAME --ssh-alias A --max-runs N
#                     --dev-mock --no-integrations --pyred-arg ARG (repeatable)]
#              [hub:  --hub-data DIR --hub-listen ADDR --hub-also-listen ADDR (repeatable) --hub-url URL --preview-listen ADDR]
#              [--memory-max V --memory-high V --cpu-quota V --no-harden] [--path PATH]
#              [Linux node: --no-scope --runs-memory-high V --runs-cpu-weight N --force-scope-switch]
#              [--with-tools [--tools-tarball FILE | --tools-from DIR] [--tools-alongside]]
#              [--root DIR] [--home DIR] [--os darwin|linux] [--arch A]
#              [--no-service-manager] [--dry-run]
#
# --from DIR: a release directory (DIR/bin/pyred) or a `make dist` platform
# directory (DIR/pyred). Version: --version, else DIR/VERSION, else
# `pyred version --json`.
#
# Release signature (--tarball, before anything is extracted): SHA256SUMS
# beside the tarball must carry a valid `ssh-keygen -Y` signature
# (SHA256SUMS.sig, namespace pyre-release) from a trusted key: --allowed-signers
# FILE, else the pinned ~/.config/pyre/release-allowed-signers, else
# packaging/release-allowed-signers of the source checkout this script runs
# from (a release tarball does not ship one). A verified key is pinned when
# nothing is pinned yet. A bad signature or a tampered SHA256SUMS is always
# refused (exit 3); a missing signature, a missing SHA256SUMS or a signature
# with no trusted key to check it is refused unless --allow-unsigned. --from
# DIR cannot be signature-checked: refused once a key is pinned or given,
# unless --allow-unsigned.
#
# --pyred-arg ARG adds ARG to the node service's `pyred run` command line
# (e.g. --rota-checkout, --trust-dir DIR, --tool-path DIR); never a secret.
#
# Linux node default: pyred starts its tmux server (all runs) in transient
# scopes in pyre-runs.slice (written next to pyred.service), so pyred.service
# uses the default KillMode and its own caps (MemoryHigh 512M, MemoryMax 1G,
# CPUQuota 200%); the slice gets MemoryHigh 85% and CPUWeight 90. --no-scope
# renders the legacy unit (KillMode=process, runs in pyred's cgroup, no caps).
#
# After activating, older versions other than current and .previous are
# pruned: each is removed and left as the alias "<version> -> current", so
# hook paths of sessions started under it keep working (--no-prune keeps
# them). Reinstalling a pruned version replaces its alias.
#
# --with-tools also installs the bundled Hive, Outfit and Rota from
# pyre-tools-<V>-<os>-<arch>.tar.gz (default: beside --tarball, checked against
# the same verified SHA256SUMS) or from an unpacked directory (--tools-from;
# default: beside --from, e.g. `make tools` output in dist/tools/) into
# <prefix>/tools/<tool>, with shims in ~/.local/bin. A tool that is already
# installed (a non-Pyre ~/.local/bin/<tool>, another one on PATH, or for Hive a
# Claude plugin install) is skipped with a note and left untouched;
# --tools-alongside installs the bundled copy anyway, without a shim (Pyre's
# integrations prefer a bundled tool). Tools persist across upgrades; re-run
# with --with-tools to update them.
#
# Exit codes: 0 ok, 1 error, 2 usage, 3 refused (a non-Pyre file is in the way, or the
# release signature/checksum did not verify).
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
# shellcheck source=lib.sh
. "$HERE/lib.sh"
TEMPLATES="$HERE/../../packaging"

usage() { sed -n '2,59p' "$0" | sed 's/^# \{0,1\}//'; exit 2; }

FROM="" TARBALL="" SUMS="" SIG="" ALLOWED_ARG="" ALLOW_UNSIGNED=0 VERSION="" SERVICE=none ACTIVATE=1 EDIT_PATH=0 PRUNE=1
PYRED_ARGS=()
HUB_ALSO_LISTEN=()
CREDENTIAL="" NODE_NAME="" SSH_ALIAS="" MAX_RUNS="" DEV_MOCK=0 NO_INTEG=0
HUB_LISTEN=":7443" HUB_URL="" PREVIEW_LISTEN=""
MEM_MAX="" CPU_QUOTA="" HARDEN=1 SVC_PATH=""
SCOPE=1 MEM_HIGH="" RUNS_MEM_HIGH="85%" RUNS_CPU_WEIGHT=90 FORCE_SCOPE_SWITCH=0
WITH_TOOLS=0 TOOLS_TARBALL="" TOOLS_FROM="" TOOLS_ALONGSIDE=0

while [ $# -gt 0 ]; do
  parse_common_flag "$@"
  if [ "$SHIFT_N" -gt 0 ]; then shift "$SHIFT_N"; continue; fi
  case "$1" in
    --from) FROM=${2:?}; shift 2 ;;
    --tarball) TARBALL=${2:?}; shift 2 ;;
    --sums) SUMS=${2:?}; shift 2 ;;
    --sig) SIG=${2:?}; shift 2 ;;
    --allowed-signers) ALLOWED_ARG=${2:?}; shift 2 ;;
    --allow-unsigned) ALLOW_UNSIGNED=1; shift ;;
    --pyred-arg) PYRED_ARGS+=("${2:?}"); shift 2 ;;
    --version) VERSION=${2:?}; shift 2 ;;
    --service) SERVICE=${2:?}; shift 2 ;;
    --no-activate) ACTIVATE=0; shift ;;
    --no-prune) PRUNE=0; shift ;;
    --edit-path) EDIT_PATH=1; shift ;;
    --state) NODE_STATE=${2:?}; shift 2 ;;
    --credential) CREDENTIAL=${2:?}; shift 2 ;;
    --name) NODE_NAME=${2:?}; shift 2 ;;
    --ssh-alias) SSH_ALIAS=${2:?}; shift 2 ;;
    --max-runs) MAX_RUNS=${2:?}; shift 2 ;;
    --dev-mock) DEV_MOCK=1; shift ;;
    --no-integrations) NO_INTEG=1; shift ;;
    --hub-data) HUB_DATA=${2:?}; shift 2 ;;
    --hub-listen) HUB_LISTEN=${2:?}; shift 2 ;;
    --hub-also-listen) HUB_ALSO_LISTEN+=("${2:?}"); shift 2 ;;
    --hub-url) HUB_URL=${2:?}; shift 2 ;;
    --preview-listen) PREVIEW_LISTEN=${2:?}; shift 2 ;;
    --memory-max) MEM_MAX=${2:?}; shift 2 ;;
    --cpu-quota) CPU_QUOTA=${2:?}; shift 2 ;;
    --no-harden) HARDEN=0; shift ;;
    --no-scope) SCOPE=0; shift ;;
    --memory-high) MEM_HIGH=${2:?}; shift 2 ;;
    --runs-memory-high) RUNS_MEM_HIGH=${2:?}; shift 2 ;;
    --runs-cpu-weight) RUNS_CPU_WEIGHT=${2:?}; shift 2 ;;
    --force-scope-switch) FORCE_SCOPE_SWITCH=1; shift ;;
    --path) SVC_PATH=${2:?}; shift 2 ;;
    --with-tools) WITH_TOOLS=1; shift ;;
    --tools-tarball) TOOLS_TARBALL=${2:?}; WITH_TOOLS=1; shift 2 ;;
    --tools-from) TOOLS_FROM=${2:?}; WITH_TOOLS=1; shift 2 ;;
    --tools-alongside) TOOLS_ALONGSIDE=1; shift ;;
    -h|--help) usage ;;
    *) echo "unknown argument: $1" >&2; usage ;;
  esac
done
case "$SERVICE" in none|node|hub) ;; *) echo "--service must be none, node or hub" >&2; exit 2 ;; esac
setup_paths

# ---- source binaries ----
WORK=""
cleanup() { if [ -n "$WORK" ]; then rm -rf "$WORK"; fi; }
trap cleanup EXIT
# ---- release signature trust ----
RELEASE_NAMESPACE=pyre-release
PIN=$(r "$CONFIG_DIR/release-allowed-signers")
CHECKOUT_SIGNERS="$HERE/../../packaging/release-allowed-signers" # present in a source checkout only
refuse() { DIE_CODE=3 die "$*; nothing was installed"; }
# release_keys FILE: the "pyre-release" keys of an allowed-signers file ("type base64", sorted).
release_keys() { awk '$1 == "pyre-release" {for (i = 2; i <= NF; i++) if ($i ~ /^ssh-/) {print $i, $(i + 1); break}}' "$1" | sort -u; }
TRUST="" TRUST_FROM="" PIN_KEY=""
if [ -n "$ALLOWED_ARG" ]; then
  [ -f "$ALLOWED_ARG" ] || die "--allowed-signers $ALLOWED_ARG not found"
  [ -n "$(release_keys "$ALLOWED_ARG")" ] || die "$ALLOWED_ARG has no pyre-release key"
  if [ -f "$PIN" ] && [ "$(release_keys "$PIN")" != "$(release_keys "$ALLOWED_ARG")" ]; then
    refuse "--allowed-signers $ALLOWED_ARG differs from the pinned release key $PIN (to rotate the key, replace $PIN deliberately)"
  fi
  TRUST=$ALLOWED_ARG TRUST_FROM="--allowed-signers"
elif [ -f "$PIN" ]; then
  TRUST=$PIN TRUST_FROM="pinned"
elif [ -f "$CHECKOUT_SIGNERS" ] && [ -n "$(release_keys "$CHECKOUT_SIGNERS")" ]; then
  TRUST=$(cd "$(dirname "$CHECKOUT_SIGNERS")" && pwd)/release-allowed-signers TRUST_FROM="source checkout"
fi

if [ -n "$TARBALL" ]; then
  [ -f "$TARBALL" ] || die "tarball $TARBALL not found"
  [ -n "$SUMS" ] || { [ -f "$(dirname "$TARBALL")/SHA256SUMS" ] && SUMS="$(dirname "$TARBALL")/SHA256SUMS"; } || true
  WORK=$(mktemp -d "${TMPDIR:-/tmp}/pyre-install.XXXXXX")
  if [ -z "$SUMS" ]; then
    [ "$ALLOW_UNSIGNED" = 1 ] || refuse "no SHA256SUMS next to the tarball and no --sums: its provenance cannot be verified (pass --allow-unsigned only for a build you made yourself)"
    warn "no SHA256SUMS next to the tarball and no --sums: provenance not verified (--allow-unsigned)"
  else
    [ -f "$SUMS" ] || die "checksum file $SUMS not found"
    [ -n "$SIG" ] || SIG="$SUMS.sig"
    if [ -f "$SIG" ]; then
      if [ -z "$TRUST" ]; then
        [ "$ALLOW_UNSIGNED" = 1 ] || refuse "the release is signed ($SIG) but no release key is trusted here: pass --allowed-signers <packaging/release-allowed-signers from a trusted checkout> (it is then pinned), or verify by hand and pass --allow-unsigned"
        warn "the release signature was NOT verified: no trusted release key (--allow-unsigned)"
      else
        command -v ssh-keygen >/dev/null 2>&1 || refuse "ssh-keygen (OpenSSH >= 8.1) is required to verify the release signature"
        # Verify private copies; checksum the tarball against exactly the verified file.
        cp "$SUMS" "$WORK/SHA256SUMS.verified"
        cp "$SIG" "$WORK/SHA256SUMS.verified.sig"
        if ! ssh-keygen -Y verify -f "$TRUST" -I "$RELEASE_NAMESPACE" -n "$RELEASE_NAMESPACE" \
             -s "$WORK/SHA256SUMS.verified.sig" <"$WORK/SHA256SUMS.verified" >"$WORK/verify.out" 2>&1; then
          sed 's/^/    /' "$WORK/verify.out" >&2
          refuse "release signature verification FAILED for $SUMS ($TRUST_FROM key $TRUST): the checksums were modified or signed by another key"
        fi
        note "release signature verified: $(head -n 1 "$WORK/verify.out") ($TRUST_FROM)"
        SUMS="$WORK/SHA256SUMS.verified"
        [ "$TRUST_FROM" = pinned ] || [ -f "$PIN" ] || PIN_KEY=$TRUST
      fi
    else
      [ "$ALLOW_UNSIGNED" = 1 ] || refuse "the release is not signed (no $SIG): pass --allow-unsigned only for a build you made yourself"
      warn "installing an UNSIGNED release (--allow-unsigned)"
    fi
    want=$(awk -v f="$(basename "$TARBALL")" '{n=$2; sub(/^\*/, "", n); if (n == f) print $1}' "$SUMS")
    [ -n "$want" ] || refuse "$(basename "$TARBALL") is not listed in $SUMS"
    got=$(sha256_of "$TARBALL")
    [ "$got" = "$want" ] || refuse "tarball checksum mismatch (expected $want, got $got)"
    note "tarball checksum verified against SHA256SUMS"
  fi
  tar -xzf "$TARBALL" -C "$WORK"
  FROM=$(find "$WORK" -mindepth 1 -maxdepth 1 -type d | head -n 1)
  [ -n "$FROM" ] || die "tarball has no top-level directory"
fi
[ -n "$FROM" ] || die "--from DIR or --tarball FILE is required"
if [ -z "$TARBALL" ]; then
  case "$TRUST_FROM" in
    pinned|--allowed-signers)
      [ "$ALLOW_UNSIGNED" = 1 ] || refuse "--from $FROM cannot be signature-checked and a release key is trusted ($TRUST_FROM); install from the signed --tarball, or pass --allow-unsigned"
      warn "installing an unverified directory although a release key is trusted (--allow-unsigned)" ;;
    *) note "--from $FROM: a directory carries no release signature (local build)" ;;
  esac
fi
FROM=$(cd "$FROM" && pwd)
if [ -x "$FROM/bin/pyred" ]; then SRC_BIN="$FROM/bin"; elif [ -x "$FROM/pyred" ]; then SRC_BIN="$FROM"; else die "no pyred binary under $FROM"; fi
for b in $PYRE_BINS; do [ -f "$SRC_BIN/$b" ] || die "missing $SRC_BIN/$b"; done
if [ -f "$FROM/SHA256SUMS" ] && [ "$SRC_BIN" = "$FROM/bin" ]; then
  n=$(cd "$FROM" && verify_sums "$FROM" "$FROM/SHA256SUMS")
  note "release contents verified ($n files, $FROM/SHA256SUMS)"
fi
if [ -z "$VERSION" ] && [ -f "$FROM/VERSION" ]; then VERSION=$(tr -d ' \n' <"$FROM/VERSION"); fi
if [ -z "$VERSION" ] && can_exec; then VERSION=$("$SRC_BIN/pyred" version --json | json_str version); fi
[ -n "$VERSION" ] || die "cannot determine the version: pass --version"
valid_version "$VERSION" || die "invalid version '$VERSION'"

note "Pyre $VERSION for $T_OS/$T_ARCH -> $PREFIX${ROOT:+ (fake root $ROOT)}"

# ---- bundled tools: locate and verify before anything is installed ----
if [ "$WITH_TOOLS" = 1 ]; then
  tname="pyre-tools-$VERSION-$T_OS-$T_ARCH"
  if [ -z "$TOOLS_TARBALL$TOOLS_FROM" ]; then
    if [ -n "$TARBALL" ]; then
      if [ -f "$(dirname "$TARBALL")/$tname.tar.gz" ]; then TOOLS_TARBALL="$(dirname "$TARBALL")/$tname.tar.gz"; fi
    else
      for c in "$(dirname "$FROM")/$tname" "$(dirname "$FROM")/tools/$tname"; do
        if [ -d "$c" ]; then TOOLS_FROM=$c; break; fi
      done
    fi
  fi
  [ -n "$TOOLS_TARBALL$TOOLS_FROM" ] || die "--with-tools: no $tname.tar.gz beside the tarball (or $tname/ beside --from); pass --tools-tarball FILE or --tools-from DIR"
  if [ -n "$TOOLS_TARBALL" ]; then
    [ -f "$TOOLS_TARBALL" ] || die "tools tarball $TOOLS_TARBALL not found"
    if [ -n "$SUMS" ]; then
      want=$(awk -v f="$(basename "$TOOLS_TARBALL")" '{n=$2; sub(/^\*/, "", n); if (n == f) print $1}' "$SUMS")
      [ -n "$want" ] || refuse "$(basename "$TOOLS_TARBALL") is not listed in $SUMS"
      got=$(sha256_of "$TOOLS_TARBALL")
      [ "$got" = "$want" ] || refuse "tools tarball checksum mismatch (expected $want, got $got)"
      note "tools tarball checksum verified against SHA256SUMS"
    elif [ -n "$TARBALL" ]; then
      warn "tools tarball provenance not verified (--allow-unsigned)"
    else
      note "--tools-tarball with --from: not verified (local build)"
    fi
    [ -n "$WORK" ] || WORK=$(mktemp -d "${TMPDIR:-/tmp}/pyre-install.XXXXXX")
    mkdir -p "$WORK/tools"
    tar -xzf "$TOOLS_TARBALL" -C "$WORK/tools"
    TOOLS_FROM=$(find "$WORK/tools" -mindepth 1 -maxdepth 1 -type d | head -n 1)
    [ -n "$TOOLS_FROM" ] || die "tools tarball has no top-level directory"
  else
    [ -d "$TOOLS_FROM" ] || die "--tools-from $TOOLS_FROM is not a directory"
    note "--tools-from $TOOLS_FROM: a directory carries no release signature (local build)"
  fi
  TOOLS_FROM=$(cd "$TOOLS_FROM" && pwd)
  if [ -f "$TOOLS_FROM/SHA256SUMS" ]; then
    n=$(cd "$TOOLS_FROM" && verify_sums "$TOOLS_FROM" "$TOOLS_FROM/SHA256SUMS")
    note "tools contents verified ($n files, $TOOLS_FROM/SHA256SUMS)"
  fi
fi

# ---- refuse to clobber non-Pyre files ----
refused=0
for b in $PYRE_BINS; do
  s=$(r "$SHIM_DIR/$b")
  if [ -L "$s" ]; then
    case "$(readlink "$s")" in *share/pyre/current/bin/$b) ;; *) echo "refusing: $SHIM_DIR/$b is a symlink to $(readlink "$s"), not a Pyre shim" >&2; refused=1 ;; esac
  elif [ -e "$s" ]; then
    echo "refusing: $SHIM_DIR/$b exists and is not a Pyre shim" >&2; refused=1
  fi
done
if [ -e "$(r "$PREFIX")" ] && [ ! -f "$(r "$PREFIX/.pyre-install")" ]; then
  # A bootstrap install (pyre enroll-node --bootstrap) leaves bin/pyred and
  # node-credential.json here; anything else is not ours.
  for e in "$(r "$PREFIX")"/* "$(r "$PREFIX")"/.[!.]*; do
    [ -e "$e" ] || continue
    case "$(basename "$e")" in bin|node-credential.json) ;; *) echo "refusing: $PREFIX contains $(basename "$e") and no .pyre-install marker" >&2; refused=1 ;; esac
  done
fi
if [ "$SERVICE" != none ]; then
  u=$(r "$(unit_for "$SERVICE")")
  if [ -e "$u" ] && ! is_managed "$u"; then echo "refusing: $(unit_for "$SERVICE") exists and is not Pyre-managed" >&2; refused=1; fi
fi
# Linux node: runs go to transient scopes in pyre-runs.slice unless --no-scope
# (or systemd-run is missing on this host).
if [ "$T_OS" = linux ] && [ "$SERVICE" = node ] && [ "$SCOPE" = 1 ]; then
  if [ "$T_OS" = "$HOST_OS" ] && [ -z "$ROOT" ] && ! command -v systemd-run >/dev/null 2>&1; then
    warn "systemd-run not found: using the legacy unit (KillMode=process, runs in pyred's cgroup)"
    SCOPE=0
  else
    s=$(r "$UNIT_SLICE")
    if [ -e "$s" ] && ! is_managed "$s"; then echo "refusing: $UNIT_SLICE exists and is not Pyre-managed" >&2; refused=1; fi
  fi
fi
[ "$refused" = 0 ] || DIE_CODE=3 die "nothing was changed; move the files above aside or remove them deliberately"

# ---- pin the release key that verified this release ----
if [ -n "$PIN_KEY" ]; then
  if [ "$DRY" = 1 ]; then
    echo "[dry-run] pin release key: install -m 0644 $PIN_KEY $PIN"
  else
    mkdir -p "$(dirname "$PIN")"
    chmod 700 "$(dirname "$PIN")" 2>/dev/null || true
    install -m 0644 "$PIN_KEY" "$PIN"
    note "pinned the release key in $CONFIG_DIR/release-allowed-signers ($TRUST_FROM); upgrades now require releases signed by it"
  fi
fi

# ---- install the version directory ----
VDIR="$PREFIX/$VERSION"
if is_alias "$VERSION"; then
  # A pruned version comes back as a real directory (the alias goes).
  note "$VERSION was pruned ($VDIR -> $(readlink "$(r "$VDIR")")); reinstalling it"
  act rm -f "$(r "$VDIR")"
fi
if [ -d "$(r "$VDIR")" ]; then
  same=1
  for b in $PYRE_BINS; do
    if ! cmp -s "$SRC_BIN/$b" "$(r "$VDIR/bin/$b")"; then same=0; fi
  done
  [ "$same" = 1 ] || DIE_CODE=3 die "$VDIR exists with different binaries; versions are immutable (remove it with uninstall.sh or use a new version)"
  note "$VERSION already installed (identical binaries)"
else
  act mkdir -p "$(r "$PREFIX")"
  act chmod 700 "$(r "$PREFIX")"
  STAGE="$(r "$PREFIX")/.staging-$VERSION-$$"
  act mkdir -p "$STAGE/bin"
  for b in $PYRE_BINS; do
    act cp "$SRC_BIN/$b" "$STAGE/bin/$b"
    act chmod 755 "$STAGE/bin/$b"
  done
  SRC_APP="$(dirname "$SRC_BIN")/Pyre.app"
  if [ -d "$SRC_APP" ]; then
    if command -v ditto >/dev/null 2>&1; then act ditto "$SRC_APP" "$STAGE/Pyre.app"; else act cp -R "$SRC_APP" "$STAGE/Pyre.app"; fi
  fi
  if [ "$DRY" = 0 ]; then
    for b in $PYRE_BINS; do
      [ "$(sha256_of "$SRC_BIN/$b")" = "$(sha256_of "$STAGE/bin/$b")" ] || die "copy of $b does not match its source"
      echo "$(sha256_of "$STAGE/bin/$b")  bin/$b"
    done >"$STAGE/SHA256SUMS"
    echo "$VERSION" >"$STAGE/VERSION"
  fi
  act mv "$STAGE" "$(r "$VDIR")"
  note "installed $VDIR"
fi
if [ "$DRY" = 0 ] && [ ! -f "$(r "$PREFIX/.pyre-install")" ]; then
  printf '%s\n%s\n' "$PYRE_MARKER: per-user Pyre installation (scripts/install/install.sh)" "Delete with scripts/install/uninstall.sh." >"$(r "$PREFIX/.pyre-install")"
fi

# ---- health check of the new binaries ----
if can_exec && [ "$DRY" = 0 ]; then
  vj=$("$(r "$VDIR/bin/pyred")" version --json) || die "pyred $VERSION failed its health check (version --json)"
  note "health: pyred version --json -> $vj"
  "$(r "$VDIR/bin/pyre")" version >/dev/null || die "pyre $VERSION failed its health check"
  "$(r "$VDIR/bin/pyre-hub")" version >/dev/null || die "pyre-hub $VERSION failed its health check"
else
  note "health check skipped (dry run, or $T_OS/$T_ARCH binaries cannot run on this host)"
fi

# ---- activate ----
PREV=$(current_version || true)
if [ "$ACTIVATE" = 1 ]; then
  if [ "$PREV" = "$VERSION" ]; then
    note "current already -> $VERSION"
  else
    if [ -n "$PREV" ] && [ "$DRY" = 0 ]; then echo "$PREV" >"$(r "$PREFIX/.previous")"; fi
    swap_symlink "$VERSION" "$(r "$PREFIX/current")"
    note "current -> $VERSION${PREV:+ (was $PREV)}"
  fi
  [ "$PRUNE" = 0 ] || prune_versions
  [ "$T_OS" != darwin ] || sync_app
else
  note "staged only (--no-activate); current stays ${PREV:-unset}"
fi

# ---- shims ----
act mkdir -p "$(r "$SHIM_DIR")"
for b in $PYRE_BINS; do
  s=$(r "$SHIM_DIR/$b")
  if [ -L "$s" ] && [ "$(readlink "$s")" = "../share/pyre/current/bin/$b" ]; then continue; fi
  act rm -f "$s"
  act ln -s "../share/pyre/current/bin/$b" "$s"
done
note "shims: $SHIM_DIR/{pyre,pyred,pyre-hub} -> ../share/pyre/current/bin/*"
if [ "$WITH_TOOLS" = 1 ]; then
  # Bundled tools live outside the version directories (<prefix>/tools), so
  # upgrades and pruning keep them. Existing installs are never replaced.
  TDIR="$PREFIX/tools"
  act mkdir -p "$(r "$TDIR")"
  for t in outfit hive rota; do
    src="$TOOLS_FROM/$t"
    if [ ! -x "$src/bin/$t" ]; then warn "$t is missing from $TOOLS_FROM; skipped"; continue; fi
    shim=$(r "$SHIM_DIR/$t") ours="../share/pyre/tools/$t/bin/$t" existing=""
    if [ -L "$shim" ] && [ "$(readlink "$shim")" = "$ours" ]; then
      :
    elif [ -e "$shim" ] || [ -L "$shim" ]; then
      existing="$SHIM_DIR/$t"
    elif [ "$T_OS" = "$HOST_OS" ] && [ -z "$ROOT" ] && [ "$T_HOME" = "$HOME" ]; then
      existing=$(command -v "$t" 2>/dev/null || true)
    fi
    if [ "$t" = hive ] && [ -z "$existing" ]; then
      for m in "$(r "$T_HOME")"/.claude/plugins/cache/*/hive/*/bin/hive; do
        if [ -x "$m" ]; then existing="the Claude plugin ${m#"$(r "$T_HOME")"/}"; break; fi
      done
    fi
    if [ -n "$existing" ] && [ "$TOOLS_ALONGSIDE" = 0 ]; then
      note "$t: existing install ($existing) left untouched and still used; bundled $t not installed (--tools-alongside adds it for Pyre without a shim)"
      continue
    fi
    stage="$(r "$TDIR")/.staging-$t-$$"
    act rm -rf "$stage"
    act cp -R "$src" "$stage"
    act rm -rf "$(r "$TDIR/$t")"
    act mv "$stage" "$(r "$TDIR/$t")"
    if [ -n "$existing" ]; then
      note "$t: installed $TDIR/$t for Pyre (its integration prefers it); $existing untouched, no shim"
    else
      if ! { [ -L "$shim" ] && [ "$(readlink "$shim")" = "$ours" ]; }; then act ln -s "$ours" "$shim"; fi
      note "$t: installed $TDIR/$t; shim $SHIM_DIR/$t"
    fi
  done
  if [ "$DRY" = 0 ]; then
    printf '%s\n%s\n' "$PYRE_MARKER: bundled tools (install.sh --with-tools), $(cat "$TOOLS_FROM/VERSION" 2>/dev/null || echo unknown)" "Delete with scripts/install/uninstall.sh." >"$(r "$TDIR/.pyre-tools")"
  fi
else
  others=""
  for c in hive outfit rota; do
    if [ -e "$(r "$SHIM_DIR/$c")" ]; then others="$others $c"; fi
  done
  [ -z "$others" ] || note "left untouched in $SHIM_DIR:$others"
fi

if [ "$EDIT_PATH" = 1 ]; then
  if [ "$T_OS" = darwin ]; then rc="$T_HOME/.zshrc"; else rc="$T_HOME/.bashrc"; fi
  if grep -q '# >>> pyre PATH >>>' "$(r "$rc")" 2>/dev/null; then
    note "PATH block already present in $rc"
  else
    if [ "$DRY" = 1 ]; then echo "[dry-run] append PATH block to $rc"; else
      # shellcheck disable=SC2016
      printf '\n# >>> pyre PATH >>>\nexport PATH="$HOME/.local/bin:$PATH"\n# <<< pyre PATH <<<\n' >>"$(r "$rc")"
    fi
    note "added $SHIM_DIR to PATH in $rc (marked block; uninstall.sh removes it)"
  fi
elif [ -z "$ROOT" ] && [ "$T_HOME" = "$HOME" ]; then
  case ":$PATH:" in
    *":$SHIM_DIR:"*)
      w=$(command -v pyre || true)
      if [ -n "$w" ] && [ "$w" != "$SHIM_DIR/pyre" ]; then warn "another pyre ($w) precedes $SHIM_DIR on PATH"; fi ;;
    *)
      note "$SHIM_DIR is not on PATH. Add it yourself, or re-run with --edit-path:"
      echo "    export PATH=\"\$HOME/.local/bin:\$PATH\"" ;;
  esac
fi

# ---- service ----
if [ "$SERVICE" = none ]; then
  note "no service requested (--service node|hub to install one)"
  exit 0
fi

# PATH for the service: launchd/systemd start with a minimal PATH, but pyred
# must find tmux, claude, codex and the Hive/Outfit/Rota commands.
if [ -z "$SVC_PATH" ]; then
  SVC_PATH="$SHIM_DIR"
  if [ "$T_OS" = "$HOST_OS" ]; then
    for tool in tmux claude codex hive outfit rota node; do
      p=$(command -v "$tool" 2>/dev/null || true)
      case "$p" in /*) d=$(dirname "$p"); case ":$SVC_PATH:" in *":$d:"*) ;; *) SVC_PATH="$SVC_PATH:$d" ;; esac ;; esac
    done
  fi
  if [ "$T_OS" = darwin ]; then base="/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"; else base="/usr/local/bin:/usr/bin:/bin"; fi
  for d in $(echo "$base" | tr ':' ' '); do case ":$SVC_PATH:" in *":$d:"*) ;; *) SVC_PATH="$SVC_PATH:$d" ;; esac; done
fi
if [ "$SERVICE" = node ] && { [ "$T_OS" = "$HOST_OS" ] && [ -z "$ROOT" ]; }; then
  for tool in tmux claude codex; do
    found=0
    for d in $(echo "$SVC_PATH" | tr ':' ' '); do [ -x "$d/$tool" ] && found=1; done
    if [ "$found" = 0 ]; then
      if [ "$tool" = tmux ]; then warn "tmux not found on the service PATH: pyred cannot start runs (install tmux)"; else warn "$tool not found on the service PATH ($SVC_PATH): that harness will be unavailable"; fi
    fi
  done
fi

CUR_BIN="$PREFIX/current/bin"
set --
if [ "$SERVICE" = node ]; then
  PYRED_PROG="$CUR_BIN/pyred"
  if [ "$T_OS" = darwin ] && [ -d "$(r "$PREFIX/Pyre.app")" ]; then PYRED_PROG="$PREFIX/Pyre.app/Contents/MacOS/pyred"; fi
  set -- "$PYRED_PROG" run --state "$NODE_STATE"
  if [ -z "$CREDENTIAL" ] && [ ! -f "$(r "$NODE_STATE/node-credential.json")" ] && [ -f "$(r "$PREFIX/node-credential.json")" ]; then
    CREDENTIAL="$PREFIX/node-credential.json"  # written by `pyre enroll-node --bootstrap`
    note "using the bootstrap credential $CREDENTIAL"
  fi
  [ -z "$CREDENTIAL" ] || set -- "$@" --credential "$CREDENTIAL"
  [ -z "$NODE_NAME" ] || set -- "$@" --name "$NODE_NAME"
  [ -z "$SSH_ALIAS" ] || set -- "$@" --ssh-alias "$SSH_ALIAS"
  [ -z "$MAX_RUNS" ] || set -- "$@" --max-runs "$MAX_RUNS"
  [ "$DEV_MOCK" = 0 ] || set -- "$@" --dev-mock
  [ "$NO_INTEG" = 0 ] || set -- "$@" --no-integrations
  for a in ${PYRED_ARGS[@]+"${PYRED_ARGS[@]}"}; do set -- "$@" "$a"; done
  if [ -z "$CREDENTIAL" ] && [ ! -f "$(r "$NODE_STATE/node-credential.json")" ]; then
    warn "no node credential yet: pyred will run standalone until you enroll it (pyre enroll-node, then pyred enroll)"
  fi
  act mkdir -p "$(r "$NODE_STATE")"
  act chmod 700 "$(r "$NODE_STATE")"
else
  set -- "$CUR_BIN/pyre-hub" serve --data "$HUB_DATA" --listen "$HUB_LISTEN"
  for a in ${HUB_ALSO_LISTEN[@]+"${HUB_ALSO_LISTEN[@]}"}; do set -- "$@" --also-listen "$a"; done
  [ -z "$HUB_URL" ] || set -- "$@" --hub-url "$HUB_URL"
  [ -z "$PREVIEW_LISTEN" ] || set -- "$@" --preview-listen "$PREVIEW_LISTEN"
  if [ ! -f "$(r "$HUB_DATA/hub.db")" ]; then
    warn "$HUB_DATA has no hub.db: run 'pyre-hub init --data \"$HUB_DATA\" --hub-url URL --tls-host HOST' before starting the service"
  fi
  [ -n "$HUB_URL" ] || warn "no --hub-url: enrollment tickets and pairing links will not carry a reachable URL"
fi

export R_VERSION="$VERSION" R_HOME="$T_HOME" R_LOG_DIR="$LOG_DIR"
if [ "$T_OS" = darwin ]; then
  R_PROGRAM_ARGUMENTS=""
  for a in "$@"; do R_PROGRAM_ARGUMENTS="$R_PROGRAM_ARGUMENTS    <string>$(xml_escape "$a")</string>
"; done
  export R_PROGRAM_ARGUMENTS="${R_PROGRAM_ARGUMENTS%
}"
  export R_LABEL; R_LABEL=$(label_for "$SERVICE")
  export R_PATH; R_PATH=$(xml_escape "$SVC_PATH")
  R_HOME=$(xml_escape "$T_HOME"); R_LOG_DIR=$(xml_escape "$LOG_DIR")
  if [ "$SERVICE" = node ]; then TPL="$TEMPLATES/launchd/$PYRE_LABEL_NODE.plist.in"; else TPL="$TEMPLATES/launchd/$PYRE_LABEL_HUB.plist.in"; fi
  if [ -n "$MEM_MAX$CPU_QUOTA" ]; then warn "--memory-max/--cpu-quota apply to systemd units only; launchd has no equivalent cgroup limit"; fi
  act mkdir -p "$(r "$LOG_DIR")"
  rotate_service_logs
else
  R_EXEC_START=""
  for a in "$@"; do R_EXEC_START="$R_EXEC_START $(systemd_word "$a")"; done
  export R_EXEC_START="${R_EXEC_START# }"
  export R_PATH; R_PATH=$(printf '%s' "$SVC_PATH" | sed 's/%/%%/g')
  if [ "$SERVICE" = node ]; then
    TPL="$TEMPLATES/systemd/pyred.service.in"
    if [ "$SCOPE" = 1 ]; then
      # Limits apply to pyred alone: runs live in pyre-runs.slice scopes.
      export R_DROP_legacy=1 R_MEMORY_HIGH="${MEM_HIGH:-512M}" R_MEMORY_MAX="${MEM_MAX:-1G}" R_CPU_QUOTA="${CPU_QUOTA:-200%}"
      export R_RUNS_MEMORY_HIGH="$RUNS_MEM_HIGH" R_RUNS_CPU_WEIGHT="$RUNS_CPU_WEIGHT"
    else
      export R_DROP_scope=1 R_MEMORY_HIGH="" R_MEMORY_MAX="" R_CPU_QUOTA=""
      if [ -n "$MEM_MAX$CPU_QUOTA$MEM_HIGH" ]; then warn "--no-scope: runs share pyred's cgroup, so --memory-max/--memory-high/--cpu-quota are not applied (they would cap agents)"; fi
    fi
  else
    TPL="$TEMPLATES/systemd/pyre-hub.service.in"
    export R_MEMORY_MAX="${MEM_MAX:-1G}" R_CPU_QUOTA="${CPU_QUOTA:-200%}"
    export R_HUB_DATA_RW; R_HUB_DATA_RW=$(systemd_word "$HUB_DATA")
    if [ "$HARDEN" = 0 ]; then export R_DROP_harden=1; fi
  fi
fi
UNIT=$(unit_for "$SERVICE")
slice_changed=0
if [ "$T_OS" = linux ] && [ "$SERVICE" = node ] && [ "$SCOPE" = 1 ]; then
  S=$(r "$UNIT_SLICE")
  if [ "$DRY" = 1 ]; then
    render_template "$TEMPLATES/systemd/pyre-runs.slice.in" "$S"
  else
    render_template "$TEMPLATES/systemd/pyre-runs.slice.in" "$S.rendered"
    if cmp -s "$S.rendered" "$S"; then rm -f "$S.rendered"; note "$UNIT_SLICE unchanged"; else mv -f "$S.rendered" "$S"; slice_changed=1; note "wrote $UNIT_SLICE"; fi
  fi
  # Switching a running legacy unit (KillMode=process) to scope mode: its
  # runs are still in pyred.service's cgroup and the new default KillMode
  # would kill them on the restart below.
  if [ "$NO_SM" = 0 ] && [ "$DRY" = 0 ] && grep -qx 'KillMode=process' "$(r "$UNIT")" 2>/dev/null; then
    cg=$(systemctl --user show pyred.service -p ControlGroup --value 2>/dev/null || true)
    if [ -n "$cg" ] && [ -f "/sys/fs/cgroup$cg/cgroup.procs" ] && [ "$(wc -l <"/sys/fs/cgroup$cg/cgroup.procs" | tr -d ' ')" -gt 1 ]; then
      if [ "$FORCE_SCOPE_SWITCH" = 1 ]; then
        warn "switching to scope mode with runs in pyred.service's cgroup (--force-scope-switch): they will be killed by the restart"
      else
        DIE_CODE=3 die "pyred.service (legacy KillMode=process) still holds runs in its cgroup; the scope-mode unit would kill them on restart. Stop the runs first (pyre stop <session>), keep the legacy unit with --no-scope, or pass --force-scope-switch"
      fi
    fi
  fi
fi
if [ "$DRY" = 1 ]; then
  render_template "$TPL" "$(r "$UNIT")"
  changed=1
else
  render_template "$TPL" "$(r "$UNIT").rendered"
  if [ "$T_OS" = darwin ] && command -v plutil >/dev/null 2>&1; then
    plutil -lint "$(r "$UNIT").rendered" >/dev/null || { rm -f "$(r "$UNIT").rendered"; die "rendered plist failed plutil -lint"; }
    note "plutil -lint: OK"
  fi
  if cmp -s "$(r "$UNIT").rendered" "$(r "$UNIT")"; then
    rm -f "$(r "$UNIT").rendered"
    changed=0
    note "$UNIT unchanged"
  else
    mv -f "$(r "$UNIT").rendered" "$(r "$UNIT")"
    changed=1
    note "wrote $UNIT"
  fi
fi
loaded=0
if [ "$NO_SM" = 0 ] && [ "$DRY" = 0 ]; then
  if [ "$T_OS" = darwin ]; then
    launchctl print "$(sm_domain)/$(label_for "$SERVICE")" >/dev/null 2>&1 && loaded=1
  else
    systemctl --user is-active --quiet "$(sdunit_for "$SERVICE")" && loaded=1
  fi
fi
if [ "$changed" = 0 ] && [ "$loaded" = 1 ]; then
  note "service already loaded with this unit; not restarted"
  [ "$slice_changed" = 0 ] || sm systemctl --user daemon-reload
else
  service_load "$SERVICE"
fi
if [ "$T_OS" = linux ]; then
  if [ "$NO_SM" = 0 ] && command -v loginctl >/dev/null 2>&1; then
    if [ "$(loginctl show-user "$T_USER" --property=Linger --value 2>/dev/null || echo no)" != yes ]; then
      warn "lingering is off for $T_USER: the service (and every run) stops at logout. Enable it: loginctl enable-linger $T_USER"
    fi
  else
    note "remember: loginctl enable-linger $T_USER (otherwise the service and its runs stop at logout)"
  fi
fi
if [ "$NO_SM" = 1 ] || [ "$DRY" = 1 ]; then
  note "service manager not invoked (fake root, foreign platform, --no-service-manager or --dry-run)"
fi
note "done"
