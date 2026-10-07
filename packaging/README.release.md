# Pyre @VERSION@ (@PLATFORM@)

Pyre runs Claude Code, Codex and shell sessions across your Mac/Linux machines,
with a shared hub for messages, project documents and artifacts.

| Path | Contents |
| --- | --- |
| `bin/pyre` | CLI and terminal UI |
| `bin/pyred` | per-machine node daemon |
| `bin/pyre-hub` | shared hub service |
| `scripts/install/` | `install.sh`, `upgrade.sh`, `uninstall.sh`, `bootstrap-node.sh` |
| `scripts/backup/` | `backup-hub.sh`, `restore-hub.sh` |
| `packaging/` | launchd and systemd user-unit templates used by `install.sh` |
| `sbom/` | CycloneDX 1.6 SBOM per binary (`cyclonedx-gomod bin`, from `go version -m`) |
| `SHA256SUMS` | checksums of every file in this directory |
| `BUILDINFO`, `VERSION`, `NOTICE`, `LICENSE` | build provenance, third-party notices, MIT license |

Verify the download first. The release `SHA256SUMS` is signed with the owner's offline
Ed25519 key (`SHA256SUMS.sig`, `ssh-keygen -Y`, namespace `pyre-release`). Take
`release-allowed-signers` from a trusted checkout of the source repository
(`packaging/release-allowed-signers`), never from the download itself. On a host
without a checkout, copy that one file over from a machine that has one (for
example `scp packaging/release-allowed-signers host:<download dir>/`). In the
directory that holds the tarball, `SHA256SUMS`, `SHA256SUMS.sig` and
`release-allowed-signers`:

```sh
ssh-keygen -Y verify -f release-allowed-signers -I pyre-release -n pyre-release \
  -s SHA256SUMS.sig < SHA256SUMS
shasum -a 256 -c --ignore-missing SHA256SUMS   # Linux: sha256sum -c --ignore-missing SHA256SUMS
```

`SHA256SUMS` lists all four platform tarballs. Without `--ignore-missing`, the
check reports the tarballs you did not download as `FAILED open or read` and exits 1.

Install for the current user (no root; nothing outside your home directory).
`install.sh --tarball` does the same signature check before it extracts anything and pins
the key in `~/.config/pyre/release-allowed-signers`; it refuses a tampered or unsigned
release. Later installs and `upgrade.sh` require releases signed by the pinned key.
Run these from this extracted directory (`tar -xzf pyre-@VERSION@-@PLATFORM@.tar.gz`
in the download directory, then `cd pyre-@VERSION@-@PLATFORM@`):

```sh
T=../pyre-@VERSION@-@PLATFORM@.tar.gz                  # with SHA256SUMS and SHA256SUMS.sig beside it
A=../release-allowed-signers                          # the copy from your trusted checkout
scripts/install/install.sh --tarball $T --allowed-signers $A --dry-run
scripts/install/install.sh --tarball $T --allowed-signers $A                  # binaries + shims
scripts/install/install.sh --tarball $T --allowed-signers $A --service node   # plus the pyred user service
scripts/install/install.sh --tarball $T --allowed-signers $A --with-tools     # plus Hive, Outfit and Rota
```

`--with-tools` needs `pyre-tools-@VERSION@-@PLATFORM@.tar.gz` beside the Pyre tarball
(checked against the same signed `SHA256SUMS`). It installs each tool into
`~/.local/share/pyre/tools/<tool>` with a shim in `~/.local/bin`, and skips any tool
that is already installed (another `hive`, `outfit` or `rota` on `PATH` or in
`~/.local/bin`, or Hive as a Claude plugin); `--tools-alongside` adds the bundled copy
for Pyre without a shim. Hive's hooks and crons are only set up by `hive install`.

The services are rendered with the `PATH` of the shell that runs `install.sh`. Run
it from an interactive login shell on that machine, where `command -v tmux claude
codex` all resolve. A plain `ssh host command` often lacks Homebrew or nvm
directories, and the installer then warns that tmux or a harness is missing.
Alternatively, pass `--path`.

Keep this directory, or the tarball, after installing. `upgrade.sh`,
`uninstall.sh` and the backup scripts run from here; the install itself copies
only the binaries.

`--from .` installs this extracted directory without a signature check (refused once a key
is pinned unless `--allow-unsigned`).

Requirements on every node: tmux, plus the Claude Code and/or Codex CLIs you
intend to run. The full runbook (hub setup, pairing, enrollment, upgrades,
backup/restore, troubleshooting) is `docs/operations.md` in the source repository.
