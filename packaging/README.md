# Packaging

Owned by WP20. Service templates, the release layout and the release build.
The operating runbook is [docs/operations.md](../docs/operations.md).

## Release layout

`packaging/release.sh [--version V] [--strict]` runs `make dist VERSION=V`,
verifies `dist/SHA256SUMS`, and writes to `dist/release/`:

```text
dist/release/
  SHA256SUMS                               sha256 of each tarball
  SHA256SUMS.sig                           ssh-keygen -Y signature of SHA256SUMS (namespace pyre-release)
  pyre-<V>-darwin-arm64.tar.gz
  pyre-<V>-darwin-amd64.tar.gz
  pyre-<V>-linux-amd64.tar.gz
  pyre-<V>-linux-arm64.tar.gz
  pyre-tools-<V>-<os>-<arch>.tar.gz        optional bundled tools, one per platform (--no-tools omits)
  pyre-<V>-windows-amd64.zip               Windows node + CLI (--no-windows omits)
  pyre-<V>-windows-arm64.zip
  pyre-tools-<V>-windows-<arch>.zip        Outfit for Windows (outfit.exe, outfit-core.exe; --no-tools or --no-windows omits)

pyre-<V>-<os>-<arch>/                      single top-level directory per tarball
  bin/pyre  bin/pyred  bin/pyre-hub        static, CGO_ENABLED=0, -trimpath, version stamped
  sbom/{pyre,pyred,pyre-hub}.cdx.json      CycloneDX 1.6 SBOM per binary
  scripts/install/{install,upgrade,uninstall,bootstrap-node,lib}.sh
  scripts/backup/{backup-hub,restore-hub}.sh
  packaging/launchd/*.plist.in  packaging/systemd/*.service.in
  README.md  VERSION  BUILDINFO  NOTICE  LICENSE
  SHA256SUMS                               sha256 of every other file in the directory

pyre-<V>-windows-<arch>/                   single top-level directory per zip
  bin/pyre.exe  bin/pyred.exe              CGO_ENABLED=0 GOOS=windows, -trimpath, version stamped (no pyre-hub)
  sbom/{pyre,pyred}.cdx.json
  scripts/install/{install,uninstall}.ps1
  README.md (from README.release-windows.md)  VERSION  BUILDINFO  NOTICE  LICENSE
  SHA256SUMS
```

`make dist` covers darwin/linux; release.sh builds the Windows binaries itself
with the same flags unless `dist/pyre-<V>-windows-<arch>/` already holds them.
The zips are deterministic like the tarballs: file entries only, sorted, DOS
mtime = commit time in UTC, `zip -X` (no extra fields), so release.sh needs
Info-ZIP `zip` unless `--no-windows`. Adding them changes only the release
`SHA256SUMS`; the darwin/linux tarballs are byte-identical to before.

Use `--strict` for real releases: it fails unless both the signing key and
the SBOM tool are available. Without it, a build that lacks either says so
and continues (rehearsals, CI).

- `pyre-mockhub` is built by `make dist` but not shipped.
- `pyre-tools-<V>-<os>-<arch>/` (from `packaging/build-tools.sh`, also
  `make tools`) holds `outfit/bin/outfit` (built from its own module in
  `tools/outfit`, CGO_ENABLED=0, -trimpath), `hive/` (the Hive plugin tree,
  `bin/hive` launcher) and `rota/bin/rota`, plus `VERSION`, `BUILDINFO`,
  `NOTICE` and `SHA256SUMS`. Only git-tracked files are copied. It is tarred
  the same deterministic way and listed in the same signed `SHA256SUMS`.
  `install.sh --with-tools` installs it into `~/.local/share/pyre/tools/`
  and links `~/.local/bin/outfit-core` (the Outfit plugin's command) to the
  bundled `outfit`. The `windows/<arch>` bundles hold Outfit only:
  `outfit/bin/outfit.exe` and an identical `outfit-core.exe` (no symlink
  shims on Windows); Hive and Rota are POSIX launchers and are not bundled.
  They are zipped like the Windows release zips, and `install.ps1
  -WithTools` installs them into `%LOCALAPPDATA%\Programs\Pyre\tools\`.
- `NOTICE` lists the third-party Go modules compiled in (`go version -m`).
  The build copies `LICENSE` (MIT) and `NOTICE` (third-party notices) from
  the repository root into every tarball.
- Tarballs are deterministic for a given commit and toolchain: sorted
  entries, mtime = commit time (`SOURCE_DATE_EPOCH` overrides), uid/gid 0,
  ustar, no macOS extended attributes, `gzip -n`.
- The Android APK is built and signed separately ([android/README.md](../android/README.md)).

## Release signing

GitHub artifact attestations are not available for a user-owned private
repository, and keyless Sigstore would publish the repository identity to a
public log. Pyre therefore signs with an offline Ed25519 key, using
`ssh-keygen -Y` (OpenSSH 8.1+, already on every Mac and Linux host, so no new
tool is needed):

| File | Where | Contents |
| --- | --- | --- |
| `~/.config/pyre/release-signing` | build machine, plus backups on server and laptop (0600, never in Git) | private key (OpenSSH Ed25519, no passphrase; `ssh-keygen -p -f` adds one, and then release.sh prompts) |
| `packaging/release-allowed-signers` | this repository | public key, principal and namespace `pyre-release` |
| `~/.config/pyre/release-allowed-signers` | every installed host | the pinned public key that upgrade.sh and pyred use |

Key fingerprint: `SHA256:PkV+Jks4fzjePVhK4cvgH21P6KqHNI/UWt9d9SuHf18` (ED25519, comment `pyre-release`).

`release.sh` signs `dist/release/SHA256SUMS` with the key it finds first:
`--signing-key FILE`, then `$PYRE_RELEASE_SIGNING_KEY`, then
`~/.config/pyre/release-signing`. `--no-sign` skips signing. The script then
verifies the new signature against `packaging/release-allowed-signers`, so a
wrong key fails the build. Ed25519 signatures are deterministic, so signing
does not break reproducibility.

Verification:

- **Manually:**
  `ssh-keygen -Y verify -f packaging/release-allowed-signers -I pyre-release -n pyre-release -s SHA256SUMS.sig < SHA256SUMS`,
  then `shasum -a 256 -c --ignore-missing SHA256SUMS` (Linux: `sha256sum`).
  `--ignore-missing` skips the platform tarballs you did not download;
  without it the check exits 1 for each of them.
- **`scripts/install/upgrade.sh`:** verifies before it stages anything. It
  pins the first verified `--allowed-signers FILE` in
  `~/.config/pyre/release-allowed-signers`. Once a key is pinned, it refuses
  (exit 3, `current` unchanged):
  - a tampered `SHA256SUMS`
  - a signature by any other key
  - a missing signature (allowed only with `--allow-unsigned`)
  - an unverifiable `--from` directory
  - an `--allowed-signers` file that differs from the pinned key

  When no key is pinned, it only warns (migration).
- **`internal/updates`** (`pyre update stage --tool pyred|pyre`): applies when
  the same pinned file exists (`$PYRE_RELEASE_ALLOWED_SIGNERS` overrides the
  path). The source must be a release tarball, either a local file or
  https, with `SHA256SUMS` and `SHA256SUMS.sig` beside it. The signature must
  verify, and the tarball must match the signed digest. A bad signature is
  refused even when unsigned sources are allowed.
- **`scripts/install/install.sh --tarball`** (first install) verifies the
  signature before it extracts anything. Trusted key, in this order:
  `--allowed-signers FILE`, the pinned
  `~/.config/pyre/release-allowed-signers`, or
  `packaging/release-allowed-signers` of the source checkout the script runs
  from (a release tarball does not ship one, so a script run from an extracted
  tarball needs `--allowed-signers` or a pin). A verified key is pinned when
  none is. It refuses (exit 3, nothing installed, nothing pinned):
  - a tampered `SHA256SUMS` or a signature by another key (even with
    `--allow-unsigned`)
  - a tarball whose bytes do not match the signed digest
  - an unsigned release, a missing `SHA256SUMS`, or a signed release with no
    trusted key to check it (allowed only with `--allow-unsigned`)
  - an `--allowed-signers` file that differs from the pin
  - `--from DIR` once a key is pinned or given (allowed only with
    `--allow-unsigned`)

  `upgrade.sh` hands install.sh its own verdict (the verified private copies
  and key, or `--allow-unsigned` where it accepted an unsigned or unpinned
  release), so staging re-checks exactly what upgrade.sh verified.

- **`scripts/install/install.ps1 -Zip`** (Windows) applies the same rules and
  exit codes, with the pin in `%APPDATA%\Pyre\release-allowed-signers`. It
  verifies with `ssh-keygen -Y verify` (Windows OpenSSH 8.1+), else with the
  already installed `pyred.exe verify-release` (the Go verifier of
  `internal/updates`), never with a binary from the candidate zip. With
  neither available it refuses unless `-AllowUnsigned`.

Key rotation: generate a new key, replace `packaging/release-allowed-signers`,
and replace the pinned file on each host deliberately. upgrade.sh never
replaces a pin on its own.

## SBOM

Each tarball carries `sbom/<binary>.cdx.json`. `cyclonedx-gomod` v1.10.0 is
the current CycloneDX Go tool. It reads the module list embedded in the
binary (`go version -m`) and is run as
`cyclonedx-gomod bin -json -std -noserial -notimestamp -short-purls`:

- `-noserial` and `-notimestamp` keep the SBOM reproducible.
- `-short-purls` is needed because v1.10.0 fills the purl `goos`/`goarch`
  qualifiers from the build host instead of the binary.

`release.sh` checks that every `dep` that `go version -m` reports appears in
the SBOM, and fails otherwise. Install the tool once with
`go install github.com/CycloneDX/cyclonedx-gomod/cmd/cyclonedx-gomod@v1.10.0`,
or point `$CYCLONEDX_GOMOD` at it.

## Reproducibility and integrity checks

```sh
packaging/verify-repro.sh --version v0.1.0-rc1        # two builds (second with an empty GOCACHE), cmp every artifact
packaging/test-release-integrity.sh                   # install.sh + upgrade.sh signature failure paths, throwaway keys, scratch HOMEs
go test ./internal/updates -run 'SSHSig|Signed|AllowedSigners'
```

`verify-repro.sh` writes its builds to a temporary directory and leaves
`dist/release/` alone (it does rewrite `dist/pyre-<V>-*` and
`dist/SHA256SUMS`). `make verify-repro` runs it (CI job "reproducible
build"); pass extra flags with `VERIFY_REPRO_ARGS="--signing-key FILE"`.
`make verify-repro-dist` is the quicker binaries-only check (`make dist`
twice; `REPRO_PLATFORMS="linux/amd64"` narrows it).

## Service templates

| Template | Rendered to | Notes |
| --- | --- | --- |
| `launchd/dev.jhh3.pyre.pyred.plist.in` | `~/Library/LaunchAgents/dev.jhh3.pyre.pyred.plist` | `AbandonProcessGroup`, `KeepAlive{SuccessfulExit=false}`, `ThrottleInterval 10`, `ProcessType Standard`, logs in `~/Library/Logs/pyre/` |
| `launchd/dev.jhh3.pyre.hub.plist.in` | `~/Library/LaunchAgents/dev.jhh3.pyre.hub.plist` | same supervision; runs only while the user is logged in |
| `systemd/pyred.service.in` | `~/.config/systemd/user/pyred.service` | scope mode (default): default `KillMode`, `PYRE_SYSTEMD_SCOPE=require`, `MemoryHigh=512M`, `MemoryMax=1G`, `CPUQuota=200%`, `TasksMax=512`; legacy block (`--no-scope`): `KillMode=process`, no caps. `Restart=on-failure`, `RestartSec=2s` |
| `systemd/pyre-runs.slice.in` | `~/.config/systemd/user/pyre-runs.slice` | slice for the tmux server scopes (all runs): `MemoryHigh=85%`, `CPUWeight=90`, no `MemoryMax` |
| `systemd/pyre-hub.service.in` | `~/.config/systemd/user/pyre-hub.service` | `MemoryMax=1G`, `CPUQuota=200%`, `TasksMax=256`, `ProtectSystem=strict`, `ProtectHome=read-only` + `ReadWritePaths=<hub data>` (`--no-harden` drops the sandbox) |

Placeholders are `@NAME@`; blocks between `# BEGIN tag` / `# END tag` are
dropped when the installer sets `R_DROP_tag=1`. Every rendered file carries
the `pyre-managed` marker; the installer never overwrites or removes a unit
file without it. No template contains a secret: the daemon and hub read their
credentials from 0600 files in their own state directories.

Windows has no template: `pyred service install` registers the per-user
logon task itself (see docs/operations.md, "Windows nodes").

Linux runs live outside `pyred.service`: pyred starts its tmux server with
`systemd-run --user --scope --slice=pyre-runs.slice` (internal/supervisor
scope.go, runtime/tmux `EnsureServer`), so the unit's cgroup limits apply to
pyred alone and a restart never signals runs. The node unit still has no
`ProtectSystem`, `ProtectHome`, `PrivateTmp` or `NoNewPrivileges`: a scope
moves processes to another cgroup, but the tmux server is forked from pyred
and inherits its namespaces and privilege flags, so those options would
still reach the agent sessions. Validated with transient units on a Linux
host (`scripts/install/scope-validate.sh`).
