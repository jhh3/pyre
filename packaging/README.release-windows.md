# Pyre @VERSION@ (@PLATFORM@)

Pyre runs Claude Code, Codex and shell sessions across your machines, with a shared
hub for messages, project documents and artifacts. This archive is a Windows node:
the CLI/TUI and the node daemon. The hub (`pyre-hub`) runs on macOS or Linux only.

| Path | Contents |
| --- | --- |
| `bin\pyre.exe` | CLI and terminal UI |
| `bin\pyred.exe` | per-machine node daemon |
| `scripts\install\` | `install.ps1`, `uninstall.ps1` |
| `sbom\` | CycloneDX 1.6 SBOM per binary (`cyclonedx-gomod bin`, from `go version -m`) |
| `SHA256SUMS` | checksums of every file in this directory |
| `BUILDINFO`, `VERSION`, `NOTICE`, `LICENSE` | build provenance, third-party notices, license |

Requirements: Windows 10 1809+ / Windows 11 / Server 2019+ (AF_UNIX sockets),
the Claude Code (native install, with Git for Windows) and/or Codex CLIs you intend
to run. No Administrator rights are needed.

Verify the download first. The release `SHA256SUMS` is signed with the owner's offline
Ed25519 key (`SHA256SUMS.sig`, `ssh-keygen -Y`, namespace `pyre-release`). Take
`release-allowed-signers` from a trusted checkout of the source repository
(`packaging/release-allowed-signers`), never from the download itself. Put the zip,
`SHA256SUMS`, `SHA256SUMS.sig` and `release-allowed-signers` in one directory.
`install.ps1 -Zip` verifies the signature (Windows OpenSSH 8.1+ `ssh-keygen`, else an
already installed `pyred.exe verify-release`) and the zip checksum before it extracts
anything, and pins the key in `%APPDATA%\Pyre\release-allowed-signers`.

Extract the zip once to get the scripts, then, from the extracted directory in
PowerShell:

```powershell
$Z = "..\pyre-@VERSION@-@PLATFORM@.zip"     # with SHA256SUMS and SHA256SUMS.sig beside it
$A = "..\release-allowed-signers"            # the copy from your trusted checkout
powershell -NoProfile -ExecutionPolicy Bypass -File scripts\install\install.ps1 -Zip $Z -AllowedSigners $A -DryRun
powershell -NoProfile -ExecutionPolicy Bypass -File scripts\install\install.ps1 -Zip $Z -AllowedSigners $A -EditPath
# Node with the logon task, enrolled with a ticket from `pyre --json enroll-node --name NAME`:
Get-Content ticket.json | powershell -NoProfile -ExecutionPolicy Bypass -File scripts\install\install.ps1 `
  -Zip $Z -AllowedSigners $A -Service node -Name NAME -HubUrl https://hub.example:7443
```

Binaries go to `%LOCALAPPDATA%\Programs\Pyre\<version>\bin` with the junction
`current`; `-EditPath` puts `...\Pyre\current\bin` on your user PATH. Node state lives
in `%LOCALAPPDATA%\Pyre`, config in `%APPDATA%\Pyre`. The node runs as a per-user
scheduled task at logon in your desktop session, so agents can start and test GUI
apps; `pyred service status|stop|start` manages it. Upgrade by running `install.ps1`
with the new zip and `-Restart`; remove with `scripts\install\uninstall.ps1`.

The full runbook (hub setup, pairing, enrollment, auto-logon for unattended GUI
testing, upgrades, troubleshooting) is `docs/operations.md` ("Windows nodes") in the
source repository.
