# Operations runbook

How to build, install, run, upgrade, back up and remove Pyre on a personal
Mac/Linux fleet. Every command here exists in this repository. Where a step is
not implemented yet it says so. The real-host evidence for these procedures
is kept outside this repository ([evidence](evidence/README.md)).

No service has been installed on the owner's real machines. The
install/upgrade/uninstall and backup/restore procedures were rehearsed in
scratch homes, fake roots and loopback hubs on a Mac, and the signed-release
install, upgrade, rollback and tool-update paths were run on server and laptop
in isolated user-level prefixes with the hub and node started from the
rendered units by a test supervisor. The
[rollout plan](#live-rollout-plan-proposal-not-executed) at the end needs the
owner's authorization step by step.

## Deployment shape

```text
 Android app ──HTTPS (pinned)──┐
 pyre CLI/TUI ──HTTPS (pinned)─┤
                               ▼
                    pyre-hub (one, always-on host)
                    SQLite + artifact blobs + TLS identity
                               ▲
          node channel (WebSocket over the same HTTPS port, pinned)
                               │
     pyred (one per machine, user service) ── Pyre-owned tmux server (own socket;
                                              Linux: own systemd scope in pyre-runs.slice)
                                              Claude / Codex / shell panes
```

- **Hub** (`pyre-hub serve`): one per fleet on an always-on host. Owns the
  shared directory, messages, project documents, artifacts, attention items and
  enrollment. Listens on one HTTPS port (default `:7443`) for the API, the node
  channel and pairing/enrollment exchanges. An optional second port
  (`--preview-listen`) serves isolated artifact previews.
- **Node** (`pyred run`): one per machine, run as a per-user service. Runs live
  in a Pyre-owned tmux server on a private socket (`<state>/tmux/pyre.sock`),
  never the user's default tmux server. Stopping or upgrading pyred does not
  stop runs: on Linux the tmux server runs in its own systemd scope in
  `pyre-runs.slice`, outside `pyred.service`; on macOS the LaunchAgent uses
  `AbandonProcessGroup`. Nodes dial out to the hub; nothing dials into a node
  except `ssh` for remote attach.
- **Clients**: `pyre` (CLI/TUI) on any machine, the Android app. All pin the
  hub's self-signed certificate by SHA-256 fingerprint. With a pin the hostname
  is not checked, so the same hub can be reached by several names or through
  forwards.

### How clients and nodes reach the hub

1. **Direct TLS** (preferred): the hub port is reachable over the LAN or
   Twingate. Hub URL `https://<hub-name>:7443`. Twingate provides reachability
   only; Pyre still authenticates every device with its own token. Cloud
   nodes reach the hub at its Tailscale address the same way
   ([cloud nodes](#cloud-nodes-gcp-over-tailscale)).
2. **ssh local forward** when the hub port is not reachable (as in
   `tests/e2e/two_host.sh`):
   `ssh -N -o ExitOnForwardFailure=yes -L 127.0.0.1:7443:127.0.0.1:7443 <hub-alias>`,
   then use `https://127.0.0.1:7443` as the hub URL on that machine. A node
   behind such a forward is enrolled with `--hub-url https://127.0.0.1:7443`.
3. **ssh reverse forward** for a node that can only be reached from a machine
   that already reaches the hub (`two_host.sh add-mac`):
   `ssh -N -R 127.0.0.1:7443:127.0.0.1:7443 <node-alias>`.

Forwards are a development convenience: Pyre ships no unit that keeps a
forward alive. Android needs direct HTTPS reachability (for example Twingate
on the phone); it cannot use an ssh forward.

The hub binds all interfaces by default (`--listen :7443`). On a shared
network bind it to the Twingate/LAN address or to `127.0.0.1` behind forwards.

## Prerequisites

| Where | Requirement |
| --- | --- |
| Every node | `tmux` on the service PATH (runs cannot start without it); the Claude Code and/or Codex CLIs you intend to run; a user account (no root needed) |
| Linux nodes and a Linux hub | systemd user manager; `loginctl enable-linger <user>` so the service and its runs survive logout |
| macOS nodes | a logged-in user session (LaunchAgents run only while the user is logged in; use auto-login on a headless Mac) |
| Hub host | stable disk; a reachable HTTPS port; `curl` for the restore check |
| Build machine | Go 1.27.1 (`~/sdk/go1.27.1`, picked up by the Makefile); nothing else. Nodes and the hub need no Go |
| Android | JDK 17+, Android SDK 35 to build; see [android/README.md](../android/README.md) |
| Backup key machine (not the hub) | `age-keygen` from the age tools (Homebrew/apt `age`, or `go install filippo.io/age/cmd/age-keygen@v1.3.2`) to create `hub-backup.key`. It is not on a stock Mac or Ubuntu. Without it, use an `ssh-ed25519` key pair as the recipient and identity instead |
| Optional | `gpg` only for gpg-encrypted backups (age encryption is built into `pyre-hub`); the `age` CLI only to decrypt a backup without Pyre |

The install scripts need only `bash` (macOS `/bin/bash` 3.2 works), coreutils,
`tar`, `shasum`/`sha256sum`, and `plutil` on macOS. The rehearsal scripts also
use `python3` and `curl`.

## Paths

| | macOS | Linux |
| --- | --- | --- |
| Binaries | `~/.local/share/pyre/<version>/bin/{pyre,pyred,pyre-hub}` | same |
| Active version | `~/.local/share/pyre/current -> <version>` (`.previous` records the last one) | same |
| Shims | `~/.local/bin/{pyre,pyred,pyre-hub} -> ../share/pyre/current/bin/*` | same |
| Node state (db, socket, runs, logs, node credential, profiles) | `~/Library/Application Support/pyre` | `~/.local/state/pyre` (or `$XDG_STATE_HOME/pyre`) |
| Hub data (db, artifacts, TLS key, owner credential) | `~/Library/Application Support/pyre-hub` (installer default) | `~/.local/share/pyre-hub` (installer default) |
| CLI config (owner token, 0600) | `~/.config/pyre/client.json` | same (or `$PYRE_CONFIG`) |
| Node service | `~/Library/LaunchAgents/dev.jhh3.pyre.pyred.plist` | `~/.config/systemd/user/pyred.service` + `pyre-runs.slice` |
| Run placement | process group abandoned by launchd | scope `pyre-tmux-<hash>.scope` in `pyre-runs.slice`; record in `<state>/tmux/placement` |
| Hub service | `~/Library/LaunchAgents/dev.jhh3.pyre.hub.plist` | `~/.config/systemd/user/pyre-hub.service` |
| Service logs | `~/Library/Logs/pyre/*.log` (stdout/stderr) and `<state>/logs/pyred.log` (rotating, 5 MiB x 3) | `journalctl --user -u pyred -u pyre-hub` and `<state>/logs/pyred.log` |
| Account profiles | `~/.local/state/pyre/profiles/<account>/<harness>/` (`$PYRE_PROFILE_ROOT`; an existing `<state>/profiles` keeps being used) | same |
| Shared transcript store of the profiles (session histories and transcripts) | `shared/` next to the profile root: `~/.local/state/pyre/shared` by default. Every Claude profile (token, rota, device login) links `projects/ todos/ file-history/ plans/ tasks/ session-env/ shell-snapshots/ history.jsonl` to `shared/claude/`, so a session resumes natively under another account; pyred merges a profile's own copies into it at start (renamed aside to `<name>.pyre-migrating`, moved in, name clashes kept as `<name>.from-<account>-<profile>`) | same |
| CLI local state (`pyre artifact get` verified cache, document drafts) | `~/.local/state/pyre/{cache,drafts}` (`$PYRE_STATE_DIR`), outside the node state directory | `~/.local/state/pyre/{cache,drafts}`, inside the node state directory |
| Android signing key | `~/.config/pyre/pyre-release.jks` + `android-signing.properties` (never in Git or in hub backups) | — |

`pyre enroll-node --bootstrap` writes `~/.local/share/pyre/bin/pyred` and
`~/.local/share/pyre/node-credential.json` on the target (Windows:
`%LOCALAPPDATA%\Programs\Pyre\bin\pyred.exe` and
`%LOCALAPPDATA%\Programs\Pyre\node-credential.json`). The installer
(`install.sh`, `install.ps1`) recognises that layout and points the service
at the bootstrap credential.

## Build and release

```sh
make build                                   # bin/{pyre,pyred,pyre-hub,pyre-mockhub} for this machine
packaging/release.sh --version 0.1.0         # make dist + checksummed tarballs in dist/release/
```

`packaging/release.sh` cross-builds darwin/linux x arm64/amd64 with
`CGO_ENABLED=0 -trimpath`, verifies `dist/SHA256SUMS`, and writes one
deterministic tarball per platform plus `dist/release/SHA256SUMS`. Two builds
of the same commit and version produce identical tarballs. Layout:
[packaging/README.md](../packaging/README.md). Tag the commit first so the
version is meaningful; a build from a dirty tree says so in `BUILDINFO`.

Verify a download: `shasum -a 256 -c --ignore-missing SHA256SUMS` (macOS) or
`sha256sum -c --ignore-missing SHA256SUMS` (Linux) in the directory holding the
tarballs. `SHA256SUMS` lists all four platforms; without `--ignore-missing` the
tarballs you did not download fail the check (exit 1).
`install.sh --tarball` first verifies `SHA256SUMS.sig` (`ssh-keygen -Y`,
namespace `pyre-release`) against `--allowed-signers FILE`, the pinned
`~/.config/pyre/release-allowed-signers`, or the source checkout's
`packaging/release-allowed-signers`, pins a verified key, then checks the
tarball against the verified `SHA256SUMS` and every extracted file against the
tarball's own `SHA256SUMS`. Details and refusal cases:
[packaging/README.md](../packaging/README.md#release-signing).

## Install

```sh
scripts/install/install.sh --tarball pyre-<v>-<os>-<arch>.tar.gz --allowed-signers packaging/release-allowed-signers --dry-run
scripts/install/install.sh --tarball pyre-<v>-<os>-<arch>.tar.gz --allowed-signers packaging/release-allowed-signers   # verify, pin, binaries + shims
scripts/install/install.sh --tarball ... --service node --name server --ssh-alias server
scripts/install/install.sh --tarball ... --service hub --hub-url https://hub:7443 --hub-listen 192.0.2.10:7443
```

The scripts come from a source checkout or from the extracted release tarball
(the install copies only the binaries, so keep one of them for `upgrade.sh`,
`uninstall.sh` and the backup scripts). On a host without a checkout: copy the
tarball, `SHA256SUMS`, `SHA256SUMS.sig` and `packaging/release-allowed-signers`
from your trusted checkout into one directory, verify by hand
([packaging/README.release.md](../packaging/README.release.md), which is the
tarball's `README.md`), extract the tarball, and run
`scripts/install/install.sh --tarball ../<tarball> --allowed-signers
../release-allowed-signers` from the extracted directory.

Behaviour:

- Signature first: a tampered `SHA256SUMS`, a signature by another key or a
  tarball that does not match the signed digest is refused (exit 3) before
  anything is extracted; an unsigned or unverifiable release needs
  `--allow-unsigned`. Run from a source checkout, the checkout's
  `packaging/release-allowed-signers` is trusted; run from an extracted
  tarball, pass `--allowed-signers` (or pin the key first).
- Per user, never root. Writes only under `~/.local/share/pyre`, the three
  shims in `~/.local/bin`, the unit file, the node state directory and the log
  directory.
- Idempotent: the same version with identical binaries is a no-op; an
  unchanged unit is not reloaded. A version directory is immutable: different
  bytes under an existing version are refused (exit 3).
- Refuses (exit 3, nothing changed) when a shim name is taken by a non-Pyre
  file, when `~/.local/share/pyre` holds foreign files, or when a unit file
  without the `pyre-managed` marker is in the way. Other commands in
  `~/.local/bin` (`hive`, `outfit`, `rota`, ...) are never touched.
- Shell rc files are edited only with `--edit-path` (one marked block in
  `~/.zshrc` or `~/.bashrc`, removed by uninstall). Otherwise it prints the
  `export PATH=...` line.
- The service PATH is rendered at install time from the directories of
  `tmux`, `claude`, `codex`, `hive`, `outfit`, `rota` and `node` as found by
  the installing shell, plus system defaults. launchd and systemd start with a
  minimal PATH, so install from a shell where those tools resolve (or pass
  `--path`). The installer warns when tmux/claude/codex are missing. Use an
  interactive login shell on the machine: a non-interactive `ssh host cmd`
  (even `bash -l`) skips the rc files that add Homebrew or nvm. In the rc4
  smoke test this left out nvm's `codex` on server, and both `tmux`
  (`~/homebrew/bin`) and `codex` on laptop.
- Health check: `pyred version --json`, `pyre version`, `pyre-hub version` of
  the installed binaries.
- Value flags take `--flag VALUE` or `--flag=VALUE` (also in `upgrade.sh` and
  `uninstall.sh`), so `--pyred-arg=--trust-dir=DIR` works.
- Node service options: `--state DIR --credential FILE --name NAME
  --ssh-alias ALIAS --max-runs N`, and `--pyred-arg ARG` (repeatable) for any
  other `pyred run` flag, e.g. `--pyred-arg --rota-checkout` or
  `--pyred-arg --trust-dir --pyred-arg DIR` (never a secret).
  `--permissions ask|bypass` is short for `--pyred-arg --permissions
  --pyred-arg MODE`. With bypass (in either form) the installer also records
  that the owner accepted Claude's one-time bypass-permissions warning:
  `skipDangerousModePermissionPrompt: true` merged into
  `~/.claude/settings.json` (created 0600 when absent; other keys and the
  file mode kept; a file that is not a JSON object is left alone with a
  warning; needs python3 to merge an existing file). Without it the node's
  first bypass Claude launch fails with "the owner never accepted Claude's
  bypass-permissions warning on this node". Bypass is the owner's standing
  choice on his nodes; `--permissions ask` writes nothing. `--no-integrations`
  (`pyred run --no-integrations`) turns off more than Hive/Outfit: the node then
  has no accounts or rota checkout, no connections, no environment profiles
  (`pyre env`) and no tool updates (`pyre update ls` answers `unsupported: node
  action "update.list" is not available on this node: pyred runs with
  --no-integrations, …`). Hub options: `--hub-data DIR --hub-listen
  ADDR --hub-url URL --preview-listen ADDR`. Linux caps: `--memory-max V
  --memory-high V --cpu-quota V` (pyred defaults 512M/1G/200%, hub 1G/200%),
  `--runs-memory-high V --runs-cpu-weight N` for `pyre-runs.slice` (defaults
  85% and 90), `--no-scope` for the legacy node unit, `--no-harden` drops the
  hub sandbox.
- Test modes: `--root DIR` (fake root), `--home DIR`, `--os linux --arch amd64`
  (render for another platform), `--no-service-manager` (print
  launchctl/systemctl instead of running them; implied by any fake root/home).

Service definitions (templates in `packaging/`):

- launchd: `AbandonProcessGroup=true`, `KeepAlive{SuccessfulExit=false}`,
  `ThrottleInterval=10`, `ExitTimeOut=20`, `ProcessType=Standard` (Background
  throttling would be inherited by agent runs), `NumberOfFiles=8192`.
  Loaded with `launchctl bootstrap gui/$UID <plist>`; restarted with
  `launchctl kickstart -k gui/$UID/dev.jhh3.pyre.pyred`.
- systemd node (default, scope mode): pyred starts its tmux server with
  `systemd-run --user --scope --quiet --collect --slice=pyre-runs.slice
  --unit=pyre-tmux-<hash>.scope -- tmux ... start-server`, so every run
  (agents, shells, Codex app-servers) lives in that scope; tmux builds with
  systemd support also put each pane in a `tmux-spawn-*.scope` in the same
  slice. `pyred.service` therefore keeps the default `KillMode=control-group`
  and caps pyred alone (`MemoryHigh=512M`, `MemoryMax=1G`, `CPUQuota=200%`,
  `TasksMax=512`, `ManagedOOMPreference=avoid`); `Restart=on-failure`,
  `RestartSec=2s`, start-limit 10 per 5 min, `LimitNOFILE=8192`,
  `TimeoutStopSec=30s` (launchd `ExitTimeOut` 30). On SIGTERM pyred refuses
  new launches and waits up to 20 s for in-flight launches to record their
  run handle; a launch still in flight after that (or after a crash/kill) is
  reconciled on the next start: adopted if its tmux session or process
  exists, failed with "launch interrupted … nothing ran — relaunch to retry"
  when the run dir proves the harness was never started (no
  `launch-started` marker), otherwise left `unknown`. A `start_run` whose
  command was still executing when pyred stopped, before the launching
  record committed (account placement, probes), left nothing behind: on
  the next start pyred reports that command failed ("launch interrupted by
  a pyred restart; the run never started") instead of indeterminate, so the
  hub fails the run and the parent gets its report; when the run does
  exist, the command's result is the run as recovered. The hub backs this
  up for older nodes: a run that stays `unknown` for 2 minutes while its
  node is connected and the node's latest hello inventory leaves it out
  (although the node already held its launch) ends: `failed` when its
  launch was indeterminate, otherwise `exited` ("run gone: its node no
  longer reports it"). Runs the node still lists (orphaned, outcome
  unknown on the node too) stay `unknown`. Pyre never relaunches
  an interrupted launch by itself (a live session whose process died is a
  different case: see Crash resume). `ManagedOOMPreference=avoid` is honoured for systemd-oomd
  memory-pressure kills of user units because the unit's cgroup and the
  monitored `user@UID.service` have the same owner (systemd 259; swap kills
  ignore it). On server (2026-10-06) oomd killed pyred.service at 529 MB: its
  RSS was ~60 MB, the rest was reclaimable dentry/inode slab charged to it by
  `git status` children of the worktree tracker (fixed: cached, budgeted
  checks) and page cache of an 800 MB pyred.db bloated by stored
  `read_transcript` results (fixed: 10-minute retention for read-only
  command records). Constant reclaim at `MemoryHigh` made it the top pgscan
  candidate. `pyre-runs.slice` (rendered next to the unit)
  carries `MemoryHigh=85%` (reclaim pressure, no kill) and `CPUWeight=90`.
  Because "-" is hierarchy in slice names, the slice sits at
  `user@UID.service/pyre.slice/pyre-runs.slice`. The unit sets
  `PYRE_SYSTEMD_SCOPE=require`: if the scope cannot be created, pyred refuses
  to start runs instead of putting them in its own (killed, capped) cgroup.
  A scope moves processes to another cgroup and nothing else: runs still
  inherit pyred's environment, rlimits, `NoNewPrivileges` and mount
  namespace, so the unit has no `NoNewPrivileges`, `ProtectSystem`,
  `ProtectHome` or `PrivateTmp`. Placement is recorded in
  `<state>/tmux/placement` (how the server was started, its pid and cgroup).
- systemd node, legacy (`--no-scope`, or when `systemd-run` is missing):
  `KillMode=process`, runs in pyred's cgroup, no caps, `PYRE_SYSTEMD_SCOPE=off`.
  Switching a running legacy node to scope mode is refused while its cgroup
  still holds runs (they would be killed by the restart); stop them first or
  pass `--force-scope-switch`.
- `PYRE_SYSTEMD_SCOPE` outside the installed unit: `auto` (default; scope only
  when pyred itself runs as a systemd user service and `systemd-run --user
  --scope` works, else the server starts directly), `on` (try a scope even from
  a shell), `off`. `PYRE_SYSTEMD_SLICE` overrides the slice name.
- systemd hub: `MemoryMax=1G`, `CPUQuota=200%`, `TasksMax=256`,
  `NoNewPrivileges`, `PrivateTmp`, `ProtectSystem=strict`,
  `ProtectHome=read-only` with `ReadWritePaths=<hub data>`. In a user unit
  these need unprivileged user namespaces; if the unit fails with
  `status=226/NAMESPACE` or `218/CAPABILITIES`, reinstall with `--no-harden`.
  Caps apply only if the user manager has the memory/cpu controllers
  delegated (`systemctl --user show pyre-hub -p MemoryMax -p CPUQuotaPerSecUSec`).
- No unit contains a secret. pyred reads `<state>/node-credential.json`
  (0600); the hub reads its data directory (0700).

launchd does not rotate `StandardOutPath`/`StandardErrorPath`; install and
upgrade move a log over 10 MiB to `.1`. pyred writes only warnings and errors
to stderr (`pyred.err.log`); the full log is `<state>/logs/pyred.log`, which
pyred rotates itself (before the A20 soak every line went to both, and the
unrotated copy grew about 270 KB/h per node at one message per second).

Terminal keys in runs: with tmux 3.5 or newer the Pyre tmux server sets
`extended-keys on`, `extended-keys-format csi-u` and `terminal-features
'xterm*:extkeys'`, so Shift-Enter and other modified keys reach Claude Code
and Codex (tmux sends them only to applications that ask for them). tmux 3.4
and older keep the previous behaviour. `PYRE_TMUX_EXTENDED_KEYS=0` in pyred's
environment turns it off. Pyre's own deliveries are bracketed pastes and are
byte-identical with or without the setting (verified on tmux 3.6). A human
attached through a terminal whose `TERM` does not match `xterm*` may need
`set -as terminal-features '<TERM>:extkeys'` in that attach path.

## Hub

```sh
pyre-hub init  --data ~/.local/share/pyre-hub --hub-url https://<hub-name>:7443 \
               --tls-host <hub-name> --tls-host 127.0.0.1
pyre-hub serve --data ~/.local/share/pyre-hub --listen :7443 --hub-url https://<hub-name>:7443
```

`init` creates the database, a self-signed TLS identity and the first owner
credential, `<data>/owner-credential.json` (0600: `hub_url`,
`hub_cert_sha256`, `client_id`, `token`). It prints the TLS fingerprint; the
same value appears as `tls_sha256=` in the `serve` log. `--tls-host` names
matter only for clients that do not pin; every Pyre client pins.

Run it as a service with `install.sh --service hub --hub-data <data>
--hub-url <url> --hub-listen <addr>` after `init`. Health: `GET /healthz`
(unauthenticated, `{"ok":true}`), `pyre hello` (authenticated, prints hub id,
version and protocol range).

### Hub database size and write lock

`serve` holds `<data>/hub.lock` (flock); a second `serve` or a `compact` on the
same data directory refuses to start. Maintenance bounds `hub.db`: events keep
7 days or 200 000 entries; finished node commands are deleted 7 days after
completion and lose payloads/results over 1 KiB after 1 day; operation records
keep 30 days; audit 90 days / 100 000 entries. Every retention pass runs in
write transactions of at most 500 rows with a short pause between them, so it
never holds the write lock long. `command.result` events no longer copy
results over 1 KiB (the command record keeps them for `commands.get`).

A database that grew before this (2026-10-08: 5.5 GB, 2.9 GB of it finished
transcript reads) shrinks once offline:

```sh
pyre-hub compact --data <data> --dry-run   # bytes per event kind / command kind; changes nothing
<stop the hub service>
pyre-hub compact --data <data>             # retention to completion, then VACUUM
<start the hub service>
```

VACUUM needs free disk space about the size of the remaining data. The first
start of this version builds the retention indexes (`commands_finished`,
`operations_created`, namespace `hub_retention`), which scans `commands` once.

The hub (and pyred) log `sqlite: write transaction held the lock` or
`sqlite: BEGIN waited for the write lock` with the caller when either exceeds
1 s, and `sqlite: nested write transaction` when code opens a second write
transaction on the same database inside one it holds (a self-deadlock; it fails
at once instead of after the 10 s busy timeout).

## Clients

First CLI (the owner's machine): copy `owner-credential.json` over ssh, then

```sh
pyre login --token - < owner-credential.json     # token, hub URL and pin from the file
pyre hello
```

`--hub` and `--pin` override the file's `hub_url` and `hub_cert_sha256` (e.g. when the CLI reaches the hub
under another name). A bare token on stdin works too (`… | pyre login --hub URL --pin FP --token -`).

Delete the copied file afterwards; the token now lives in
`~/.config/pyre/client.json` (0600, refused if group/world readable).
The same file may carry `web_url`, the web app (`pyre web serve`) whose
inbox the TUI links to (default `https://pyre-example.ngrok.app`;
`PYRE_WEB_URL` overrides it); `pyre login`/`pair --force` keep it.

More machines: `pyre pair-client --name laptop --kind cli` prints a one-time
`pyre://pair?...` link (it expires; treat it like a password); on the other
machine run `pyre pair` and paste the link at the prompt (or
`pyre pair < link.txt`). The client keeps the `--name` given to
`pair-client` (`pyre pair --name` overrides it). Secrets are never accepted as command-line
arguments, which other local processes can read: `pyre pair` refuses a link
argument and `pyre login` accepts only `--token -` or `PYRE_TOKEN`.

Android: `pyre pair-client --name phone --kind android`, then open or paste
the link in the app's pairing screen; it shows hub, pin and device name before
exchanging the code. List and revoke clients with `pyre clients` and
`pyre revoke client <id>`.

Downloads over a slow hub link: `pyre artifact get` prints progress to
stderr (at most every 5 s when stderr is not a terminal). An attempt that
receives no bytes for `--stall-timeout` (default 60s) is aborted and retried
up to 3 times with backoff, resuming with HTTP Range from `<file>.part` next
to the destination; a node's pyred forwards the Range to the hub, so only the
missing bytes cross the link. A get that still fails keeps `<file>.part`, and
rerunning the same command resumes from it. Size and sha256 are verified
before the file is renamed into place. There is no overall deadline.

## Nodes

Manual enrollment:

```sh
pyre enroll-node --name server --ssh server [--hub-url https://<hub-name>:7443]   # on a configured client; secret shown once
pyred enroll --hub https://<hub-name>:7443 --pin <sha256> \
  --node-id <node_id> --secret-stdin                        # on the node; paste the secret; writes <state>/node-credential.json
scripts/install/install.sh --tarball ... --service node --name server --ssh-alias server
```

`--ssh` records the alias other machines use for remote attach. `--hub-url`
sets the hub URL the ticket carries (the hub as reachable from the node; the
default is the hub's own `--hub-url`), so `pyre --json enroll-node ...
--hub-url URL` piped into `pyred enroll --ticket-stdin` or `install.ps1`
needs no separate `--hub`. `pyred enroll --credential-file PATH` writes
elsewhere; pass the same path to `install.sh --credential`.

ssh bootstrap (installs only the `pyred` binary and enrolls; no service):

```sh
scripts/install/bootstrap-node.sh --ssh laptop --target darwin/arm64 --dist dist/release-extracted   # dry run
scripts/install/bootstrap-node.sh --ssh laptop --probe --hub-url https://<hub-name>:7443 --execute
```

This wraps `pyre enroll-node --name N --ssh ALIAS --bootstrap [--execute]`.
The plan is `ssh mkdir`, `scp pyred`, `ssh mv + pyred version`, `ssh pyred
enroll --secret-stdin` with the one-time secret on stdin. Host keys are
verified by ssh as usual. Afterwards install the release and the service on
the node (the script prints the steps).

`--execute` first asks the target which OS it runs (`ssh ALIAS uname -s`:
Linux, Darwin, the BSDs are unix; MINGW, MSYS, CYGWIN, `Windows_NT` from
busybox-w32 or uutils, any `*_NT` are Windows; anything else or no uname is
settled by `powershell ... [Environment]::OSVersion.Platform`), before any
ticket exists; `--target-os unix|windows` skips the probe, and the dry run never
connects (it shows the unix steps unless `--target-os windows`). A Windows
target (Windows OpenSSH with cmd.exe or PowerShell as its shell) gets the
same four steps as `powershell -NoProfile -NonInteractive -EncodedCommand`
scripts: create `%LOCALAPPDATA%\Programs\Pyre\bin`, scp `pyred.exe` there,
move it into place and run `pyred.exe version`, then read the secret from
ssh stdin and pipe it to `pyred.exe enroll ... --secret-stdin`. Pass
`--pyred` a `pyred.exe` for the target's architecture (from the release zip;
`bootstrap-node.sh --probe` picks it from `--dist`), then run `install.ps1
-Service node` on the node: it uses that credential.

```sh
scripts/install/bootstrap-node.sh --ssh win-office --probe --dist dist/release-extracted --hub-url https://<hub-name>:7443 --execute
pyre enroll-node --name win-office --ssh win-office --bootstrap --execute --pyred pyre-<V>-windows-amd64/bin/pyred.exe --hub-url https://<hub-name>:7443
```

Check a node: `pyre nodes`, `pyre nodes show <name>`, `pyre inventory
<name>` (harnesses, tools, extensions, drift; `--node` works too); on the
node `pyred status` (link, live runs, queues, pyred overhead).

A new Linux node that will run with `--permissions bypass`: install it with
`install.sh ... --service node --permissions bypass` so the Claude
bypass-permissions acceptance is recorded (see Install); otherwise the first
bypass Claude launch there fails until you run `claude
--dangerously-skip-permissions` once and accept the warning.

While the hub is unreachable, clients on a node (CLI with `PYRE_SOCKET`,
agents, MCP tools) keep working through the node socket: this node's
sessions come from local state (marked stale), messages go to the durable
outbox, memory search/status are answered by the node's Hive, and project,
document, revision, comment, note and launch-context reads that this
principal made earlier on this node are answered from pyred's read cache.
Cached answers are the hub's last response; the CLI prints `hub
unavailable; showing the copy cached on node …, last confirmed …` on stderr
(MCP results carry the same note). Reads never made on the node, and every
write, report the outage. Node and connection timestamps come from the
hub's clock, so a node whose clock is off does not look fresher or older
than it is.

### Automatic cleanup (sessions, worktrees, temp folders)

The hub drives it; nodes do the file work (`internal/hub/lifecycle.go`,
`internal/daemon/lifecycle.go`, `internal/repoindex/autoclean.go`).

- **Finished children**: a session started by another session (never one
  the owner started) whose current run ended is archived once its ended
  report to the parent is settled (sent, skipped, or folded into the
  parent's next report; or there was none) and nobody touched it since the
  end: no new run, and the owner has not messaged, typed into or attached
  to it. The grace is five minutes after a clean exit (a recorded exit code
  of 0) and one hour after any other end (non-zero or missing exit code,
  failed launch or run). A run with a fallback plan (account switch, move
  between turns) is not finished. Audit: `session.archived` (automatic, by
  Pyre; reason "exited cleanly …" or "exited child (exit 143), report
  delivered, untouched for an hour"); unarchive it like any archived
  session. The archive itself sends the parent nothing: the ended report
  is already settled when it happens, a report whose child Pyre archived
  is never re-sent (only a parent that archived the child suppresses its
  reports: `archivedBy`, `reportParentArchivedDetail`), and a folded
  "also exited" line still rides along with the parent's next report
  unchanged.
- **Sessions under an archived one**: when the owner or a session
  archives a session (itself, one under it, or any on the owner's say),
  its live descendants (lineage, `Session.Parent`, through already
  archived ones, at most 32 levels) are archived with it
  (`internal/hub/archivecascade.go`). The walk starts once the session
  itself is archived (at once, or when its archive-when-done completes),
  so their reports go to an archived parent and are dropped. A descendant
  with a live run archives when done (idle: stopped at once; mid-turn:
  when its turn ends) and its own descendants follow when it is archived;
  an ended one is archived now. A descendant the owner started, or
  attached to, messaged or typed into since it was created is left alone
  with what it started. Audit: `session.archive_scheduled` or
  `session.archived` by Pyre, reason "archived with ancestor ses_…". The
  archive answer (`SessionView.archive_cascade`, MCP `archive_session`
  note) and the session's final report to its parent say "also archived
  3 sessions it started; left 1 the owner is using: …". Pyre's own
  archives (finished children, the nightly sweep) do not cascade. A
  session may archive any session under it without the owner's approval.
- **Session cleanup** (`session.cleanup` node action) follows every
  automatic archive:
  - stop processes in the terminal sessions of the session's runs (the
    pane PID each run was launched as is the session leader) whose
    environment carries the session's `PYRE_SESSION_ID` (SIGTERM, SIGKILL
    after 3 s; Linux and macOS). Daemons that detach into their own
    terminal session (tmux server, adb, gradle, language servers) are not
    matched. pyred drops `PYRE_SESSION_ID`, `PYRE_RUN_ID`,
    `PYRE_SESSION_TOKEN` and `PYRE_PARENT_SESSION_ID` from its own
    environment at startup.
  - remove worktrees whose pyre-created marker names the session. Pyre
    writes the marker, with a one-time claim token, when it creates fresh
    worktrees for a launch (`worktree.create`: quick launch, the TUI's new
    session, agents' `start_session` repo/fresh); once the session has
    started, the launcher sends `worktree.claim` with the token and the
    marker records the session. A marker that never names a session
    (claim lost, or created before this release) counts as the owner's.
    Worktrees without a marker, made by hand or by an agent with
    `git worktree add`, and the checkouts a run's `checkout` request creates
    at launch, are never removed automatically. A marked
    worktree is removed only when it is not the main checkout, not locked,
    not on the default branch (or main/master), has no uncommitted or
    untracked changes, has no ignored files other than rebuildable ones
    (node_modules, .venv, venv, target, __pycache__, dist, build, .next,
    .turbo, caches), its branch is merged into the default branch or
    contained in its own upstream (a detached HEAD must be on the default
    branch or a remote branch), and no live run's working directory or
    process's current directory is inside it (processes are listed again
    right before each removal). The local branch is deleted when merged,
    or when `git ls-remote` confirms the remote still has it; otherwise
    it is kept. Audit: `session.cleaned_up`.
- **Nightly sweep** (first hub pass between 03:30 and 05:00 in the owner's
  notification time zone, default America/Los_Angeles; a hub that is down
  then skips that night; recorded in `hub_meta` `lifecycle.last_sweep`):
  archive sessions with no live run whose launch failed over an hour ago
  or whose run ended over 24 h ago (sessions with a live child, a pending
  report or a scheduled archive are skipped), then `node.sweep` on every
  connected node: session cleanup for its archived sessions, orphan marked
  worktrees of archived sessions (the hub sends its unarchived sessions as
  `keep_sessions`; a session with a live run there is kept too) in the repositories
  of its repo index and of runs from the last 30 days (same rules, and
  nothing in the worktree's git metadata changed for 24 h), and session
  folders under `<state-dir>/tmp/claude` (Claude's `CLAUDE_CODE_TMPDIR`)
  with no file changed for 72 h; folders of live runs (any session ID their
  hooks reported, and the folder named after a live run's cwd) are kept.
  The owner gets one message per sweep from `system:cleanup`; nothing is
  sent when nothing happened.
- **Dry run first**: until a dry-run report has reached the owner (`hub_meta`
  `lifecycle.dry_run_reported`, written after the message is stored), every
  sweep archives and removes nothing and reports what it would do; real
  cleanup starts the night after.
- **Windows**: nodes do not list processes there, so "in use" means a live
  run's working directory only, and no leftover processes are stopped.
- Offline machines are listed as not checked; their sessions are still
  archived. Undo an archive with `pyre sessions unarchive` or resume the
  session.

### Restarting a node

Before rebooting a machine, drain it; after the reboot, resume what the
reboot interrupted:

```sh
pyre node drain server --minutes 10 --wait   # tag + tell every live session to reach a stopping point
sudo reboot                               # on server
pyre node resume server                      # once server shows connected in `pyre nodes`
```

`node drain` tags every non-archived session whose current run is on the
node and not ended `drained:<node_id>` and sends it an owner message to
finish or pause its current step, save work and end its turn (`--message`
replaces the text). Shell sessions are skipped, since a message would run
as a command. With `--wait` it polls every 15 s until no tagged session is
running, waiting on background work or starting; at the deadline the ones
still working get a final steered STOP NOW message (a normal message where
the runtime cannot steer). `--dry-run` lists what it would do.

`node resume` handles each tagged session: archived ones and runs that
ended deliberately (for example `stopped by pyre (/exit)`) lose the tag and
are left alone; runs pyred could not find after its restart (state reason
`exit not observed`) and failed resume launches are natively resumed, like
`pyre resume`, one every ~1.5 s; runs that are still live (a Codex run that
reconnected by itself) are not resumed. Every resumed or live session gets a
restart note telling it background jobs were stopped. A resume that fails or
does not come up within ~90 s keeps its tag and prints the run's state
reason; rerun the command to retry. `--all` resumes every ended tagged
session whatever the reason. Both commands are safe to rerun: tags are
idempotent and message IDs are stable.

The node must come back on its own: the hub and pyred are services that
start at boot (launchd on macOS; systemd user services on Linux need
`loginctl enable-linger <user>`, or nothing starts until that user logs in).

Since crash resume (below), pyred resumes the sessions a reboot ended by
itself when it starts again; for a session tagged `drained:<node_id>` it
leaves the note to `node resume`, which then finds the session live, sends
the owner's restart note under the same stable message ID and removes the
tag. A note pyred already stored under that ID counts as sent.

### Crash resume

A graceful pyred restart or upgrade leaves every run alive (runs live in
their own tmux server and scope, not in pyred's cgroup), so a run whose process is gone when pyred starts died
underneath it: a reboot, or an unclean stop of the whole user session (Oct
20 2026, 12:34 AM PT: server's systemd user manager was SIGKILLed and tmux and
every agent died with it; only Codex runs came back, because Codex recovery
restarts its own app-server). pyred records such a run as before
(`exited`, "run process not found after pyred restart (exit not
observed)"), and then resumes the session by itself
(`internal/daemon/crashresume.go`) when:

- the run's last recorded state was `running`, `waiting_input`,
  `waiting_on_background` or `waiting_approval`;
- its harness resumes natively and Codex recovery does not already handle it
  (Claude, agy; not Codex, shells or headless `pyre run` runs);
- no stop was requested for it (pyred records `pyre.stop_requested` before
  it asks the adapter to stop and removes it again when the stop fails, so a
  crash between a stop and the observed exit does not bring it back), and the
  session is not archived;
- it is not itself a crash resume that died again within 10 minutes (the
  run pyred starts carries `pyre.crash_resumed_at`), so a crash loop resumes
  each session once;
- the hub, asked with the node's own credential (a session token revoked by
  an archive says nothing), knows the session, it is not archived and its
  current run is still the dead one (not resumed elsewhere meanwhile). Any
  other answer (not found, refused) skips it. While the hub cannot be
  reached (link down, unavailable, timeout) the session waits, rechecked
  every 5 s and when the link comes up, for up to 10 minutes; then it is left
  ended rather than resumed blind. A child waits while its parent does. A
  node without a hub (standalone) resumes from its own record;
- no stop for the dead run arrived after the restart. The hub queues a stop
  sent while the node was down for the run it still thought current (the
  dead one) and delivers it once the link is up: pyred then cancels that
  session's resume, or stops the run that already replaced it (once it is
  up, if it is still starting). Stop commands expire on the hub after its
  `StopTTL` (30 minutes): a node offline longer than that never receives the
  stop, and the session is resumed; stop it again. A hub that refuses the
  node's own credential is reported as a node error, and nothing is resumed.

The resume is a node-local native resume on the same account (falling back
to the best eligible one when it is unusable, or the ambient login when the
run had no assignment), exactly like an account-fallback continuation: the
hub sees a new run with the dead one as its previous run. It never replays
the interrupted instruction and never falls back to a handoff. Sessions
without a parent go first, then their children, one launch every ~1.5 s;
node run capacity applies. A resume that fails is logged (pyred's log and
the node's diagnostics errors) and leaves the session ended, as before: `pyre resume` or
`pyre node resume` can still bring it back.

Once the new run is up, the session gets one message from itself (stable
ID `msg_resumed-<session>-<run>`, the same scheme `pyre node resume` uses;
held in the node outbox while the hub is unreachable):

> [from Pyre] Pyre restarted after a crash: your previous process died at
> about <time PT> and Pyre resumed you at <time PT>. Anything you had running
> in the background (watchers, Monitors, dev servers, tunnels, shells) is
> gone: check your state (git status, PRs, files) and restart what you need.
> If you were started by another session, check in with it before
> continuing. No reply needed.

The death time is the later of the run's last recorded observation and
pyred's own alive stamp (written every 30 s), so it is when pyred last saw
the machine healthy, not an exact time of death. A parent still gets the
child's ordinary `ended` report for the dead run.

## Cloud nodes (GCP over Tailscale)

`pyre node create --cloud gcp` makes an on-demand Google Cloud VM and enrolls
it as an ordinary node. Everything runs on the machine where you type it,
with your own `gcloud` (its active account and configuration; Pyre stores no
service-account key). Code: `internal/cloud`, `internal/cli/cmd_cloud.go`.
Tested with fakes only; the first real create is an owner-approved rollout
step.

```sh
pyre node create --cloud gcp --name dev1                       # e2-standard-8, 200 GB, Ubuntu 24.04
pyre node create --cloud gcp --name gpu1 --gpu nvidia-l4:1 --spot
pyre node create --cloud gcp --name win1 --os windows --rdp     # Windows Server 2025, RDP over the tailnet
pyre node create --cloud gcp --name win-build --os windows --toolchain node@24.21.0 --toolchain bun@1.3.14,rust
pyre nodes --cloud                  # VM state, type, uptime, estimated $/h now and while running
pyre node stop dev1                 # power off, disk kept (refuses with live runs unless --force)
pyre node start dev1                # boots with a fresh Tailscale key, waits for the node
pyre node idle dev1 60              # idle auto-stop minutes (0 = off), live within a minute
pyre node delete dev1               # VM + disks + hub enrollment (refuses with live runs or unpushed work)
pyre node rdp win1 --reset-password --open
pyre node image bake [--os windows] # image family so later creates boot in about a minute
```

Every VM is `pyre-<name>`, labelled `pyre-node=<name>`, `pyre-managed=true`
and `pyre-os=<os>` (its boot disk too); listing and cleanup use only these
labels and refuse instances without `pyre-managed=true`. Builder VMs of
`image bake` carry `pyre-role=bake` and are deleted after the image is made.

### Cloud configuration

`cloud.json` beside the client config (`~/.config/pyre/cloud.json`, or
`$PYRE_CLOUD_CONFIG`); every field is optional and flags override it:

```json
{
  "hub_url": "https://server.<tailnet>.ts.net:7443",
  "gcp": {"project": "<project>", "zone": "<zone>", "machine_type": "e2-standard-8", "disk_gb": 200,
          "spot": false, "idle_stop_minutes": 30, "user": "pyre", "service_account": "",
          "harnesses": ["claude", "codex", "gh"], "pyred_args": [],
          "toolchains": ["electron"],
          "windows_installers": [], "windows_gpu_driver": ""},
  "release": {"url": "gs://<bucket>/pyre/<version>/", "version": "<version>", "with_tools": false},
  "tailscale": {"tag": "tag:pyre-node", "oauth_client_id": "<id>",
                "oauth_client_secret_file": "/Users/<you>/.config/pyre/tailscale-oauth-secret"}
}
```

Project and zone default to `gcloud config get-value project` and
`compute/zone`. `release.url` is a directory holding
`pyre-<v>-linux-<arch>.tar.gz` (and `pyre-<v>-windows-amd64.zip` for Windows),
`SHA256SUMS` and `SHA256SUMS.sig`, as `packaging/release.sh` writes them;
upload with `gcloud storage cp dist/release/{pyre-<v>-linux-amd64.tar.gz,SHA256SUMS,SHA256SUMS.sig} gs://<bucket>/pyre/<v>/`.
The release key the VM verifies with is your pinned
`~/.config/pyre/release-allowed-signers` (or `release.allowed_signers`).
Secrets are never flags: the Tailscale OAuth client secret comes from
`TAILSCALE_OAUTH_CLIENT_SECRET` or a 0600 file, a pre-made key from
`PYRE_TAILSCALE_AUTHKEY` or `tailscale.auth_key_file` (single use: one
create; `start` then needs another).

### Bootstrap flow and secrets

1. The CLI refuses early (name taken on the hub or in the project, no hub
   URL, no release key, no Tailscale key source), ensures the firewall rule
   `pyre-deny-ingress` (DENY all inbound from 0.0.0.0/0 to tag `pyre-node`,
   priority 100; firewall rules cannot carry labels, so its name and
   description mark it), mints a one-time hub enrollment ticket (TTL 30 min)
   and one Tailscale auth key (single use, ephemeral, pre-authorized, tagged
   `tag:pyre-node`, 15 min).
2. It writes the startup script, the non-secret `pyre-config` JSON and the
   two secrets to 0600 files in a private 0700 temp dir and creates the VM
   with `--metadata-from-file` (secrets never in argv or logs; the dir is
   removed afterwards). The VM gets no service account at all, except
   `devstorage.read_only` when the release is on `gs://` or a Windows node
   has `gcp.windows_installers` (set
   `gcp.service_account` to one with only `roles/storage.objectViewer` on the
   bucket to narrow it further).
3. First boot (`startup-script`, root, every boot, idempotent, output on the
   serial console: `gcloud compute instances get-serial-port-output
   pyre-<name> --zone <z>`): base packages, the lingering node user `pyre`,
   Tailscale (`tailscale up --auth-key=file:<0600 tmpfs file> --hostname
   pyre-<name> --ssh`), the release download, `ssh-keygen -Y verify` of
   `SHA256SUMS.sig` (namespace `pyre-release`) and the tarball checksum
   before extracting (as `install.sh` does), `pyred enroll --secret-stdin`
   (secret on stdin), the developer toolchains (below), node/Claude
   Code/Codex when missing, then
   `install.sh --tarball … --allowed-signers … --service node --name <name>
   --ssh-alias pyre@pyre-<name>` (which verifies again). Secret files are
   deleted at once; the script never enables tracing.
4. The CLI waits for the node to connect (default 15 min, Windows 60; the
   Windows enrollment ticket also lives 60 min), then
   removes `pyre-enroll-secret` and `pyre-ts-authkey` from the metadata and
   revokes the minted key, also after a timeout. Until then any process on
   the VM could read them from the metadata server; both are single use and
   short-lived for that reason.

`pyre node image bake` runs the same script in bake mode on a throwaway
builder (toolchain and verified release; no tailnet login, enrollment or
credential; machine-id cleared; Windows runs GCESysprep), waits for it to
power off, creates an image in family `pyre-node` / `pyre-node-windows` and
deletes the builder. `create` uses that family automatically when it exists.

### Developer toolchains

`gcp.toolchains` in `cloud.json` and `--toolchain` on `pyre node create` /
`pyre node image bake` (repeatable, comma-separated also works) choose what
the boot script installs, so a node can build a project the way its CI
does. Entries are ids, ids with a version, or presets:

| id | Windows | Linux |
|---|---|---|
| `node@lts`, `node@<major>`, `node@<x.y.z>` (`node` = `node@lts`) | official zip (`node-v<x.y.z>-win-x64.zip`) into `C:\Program Files\PyreTools\node\…`, first on the machine PATH; checked against nodejs.org `SHASUMS256.txt` | official tarball into `/opt/pyre-tools/node/v<x.y.z>`, `/opt/node` and `/usr/local/bin/{node,npm,npx,corepack}` links; `SHASUMS256.txt` |
| `pnpm`, `pnpm@<x.y.z>` | `corepack enable pnpm` (Node 25+: corepack from npm first); `COREPACK_HOME=C:\ProgramData\pyre\corepack` (dev user can write), `COREPACK_ENABLE_DOWNLOAD_PROMPT=0`; a repo's `packageManager` field picks the pnpm version at first use; with a version, `corepack install -g pnpm@<v>` | same, shims in `/usr/local/bin`; with a version, prepared for the node user |
| `bun`, `bun@<x.y.z>` | release `bun-windows-x64.zip` of tag `bun-v<x.y.z>` (bare `bun`: latest), checked against that release's `SHASUMS256.txt`; `bun.exe`/`bunx.exe` in `PyreTools\bun\<v>` | `bun-linux-x64.zip` / `-aarch64`, same check; `/usr/local/bin/bun`, `bunx` |
| `rust`, `rust@<channel or x.y.z>` | `rustup-init.exe` (checked against its `.sha256`), `--default-host x86_64-pc-windows-msvc`, machine-wide `RUSTUP_HOME`/`CARGO_HOME` under `C:\ProgramData\pyre\rust` (dev user can write), `cargo\bin` on the machine PATH; adds `vs-buildtools` | `rustup-init` (`.sha256`) as the node user (`~/.cargo`, linked into `~/.local/bin` for pyred's service PATH); `build-essential` for the linker |
| `cmake`, `cmake@<x.y.z>` | Chocolatey `cmake` (`ADD_CMAKE_TO_PATH=System`, `--version` when given) | distro `cmake` (a version applies on Windows only) |
| `llvm`, `llvm@<x.y.z>` | Chocolatey `llvm`; `LIBCLANG_PATH=C:\Program Files\LLVM\bin` for bindgen | distro `llvm clang libclang-dev` |
| `python`, `python@<x.y.z>` | python.org installer (default 3.12.8), Authenticode publisher Python Software Foundation | distro `python3 python3-venv python3-pip python-is-python3` |
| `vs-buildtools` | VS 2022 Build Tools, C++ workload with recommended components (Windows SDK); Authenticode publisher Microsoft | `build-essential` |
| `git` | latest Git for Windows, checked against the GitHub release asset digest | distro `git` |
| `gh` | latest gh MSI, GitHub release asset digest | GitHub's signed apt repository |
| `parsec` (Windows only) | Parsec host (`parsec-windows.exe /silent /shared`), Authenticode publisher Parsec; sign in once as the dev user over RDP | refused |

Presets: `default` (what nodes always got: Windows `git, node@lts, python,
vs-buildtools`; Linux `git, python`), `electron` (`node@lts, pnpm, python,
git, gh`, plus `vs-buildtools` on Windows) and `media` (`electron` plus
`parsec`, Windows). Rules: nothing configured means `default`; the
`--toolchain` flags are added after the configured list; a later entry for
the same tool replaces an earlier one (`--toolchain node@24.21.0` overrides
`electron`'s `node@lts`); `pnpm` adds `node@lts` when no node is listed;
`rust` on Windows adds `vs-buildtools`; the `gh` harness adds `gh`.
Versions are validated by the CLI before anything is created (`node`
takes `lts`, a major or an exact `x.y.z`; the others an exact `x.y.z`).
Claude Code and Codex still get node 22 (Linux) or LTS (Windows) when no
toolchain installed one.

Every id is idempotent and skipped when the requested version is present
(`node@lts` and bare ids accept any installed version). The scripts run the
list on every boot (Linux: first boot after enrollment, later boots before
the ready line; Windows: before the later-boot exit), so a failed install is
retried by a reboot; in node mode a failure is logged
(`WARNING: toolchain … failed`) and the node still comes up, in bake mode it
fails the bake. Nothing secret is involved or logged. Installs are
machine-wide: on Windows the machine PATH and environment change, which the
dev user's sessions (and pyred, started at logon) see from the next logon;
the first boot reboots before that logon anyway.

Trust: Node, Bun and rustup downloads are checked against SHA-256 files
the vendor publishes beside them (same origin, over TLS: this catches
corruption and mirror tampering, not a compromised vendor site; Node's and
Bun's GPG signatures are not checked). Git and gh use the SHA-256 digest
GitHub's release API reports for the asset. VS Build Tools, Python, Parsec
and the NVIDIA driver must carry a valid Authenticode signature from their
publisher. CMake and LLVM come from Chocolatey community packages, as the
CI's `choco install cmake` / `choco install llvm`: Chocolatey's moderators
review the package and the package checks the checksum its maintainer
embedded for the upstream download, so the trust root is the community
package maintainer plus `community.chocolatey.org/install.ps1` (fetched over
TLS, unverified) — use an image bake you inspect if that is not enough.
Windows Python's installer and Git for Windows' latest-release lookup carry
no vendor checksum beyond those signatures/digests.

The owner's Electron app (acme/video-editor, built on GitHub's
`windows-2025` runner) needs this list on its Windows build node; the app's
`.nvmrc` sets the node version (keep them in step), pnpm comes from the
repo's `packageManager` field through corepack, and `git` is added because
the base Windows image has none:

```json
"gcp": {"toolchains": ["node@24.21.0", "pnpm", "bun@1.3.14", "rust", "cmake", "llvm", "python", "vs-buildtools", "git"]}
```

then, on the node, `pnpm install --frozen-lockfile` and `pnpm run build`
(the NSIS installer comes from `scripts/build-desktop-artifact.ts`). A
Windows create with Build Tools, Rust and LLVM takes well over half an hour,
so bake an image with the same list once
(`pyre node image bake --os windows --toolchain …`, or with `gcp.toolchains`
set) and creates boot ready; the default Windows image is Windows Server
2025 Datacenter (`windows-2025`, as the CI runner), `--image` overrides it.

### Cloud lifecycle and cost

- **Idle auto-stop** runs on the node (`pyre-idle-stop.timer`, every
  minute): when `pyred status` reports no live runs and nobody is logged in
  for `pyre-idle-minutes` (instance metadata, default 30; `pyre node idle`
  changes it), it powers the VM off from inside. GCE shows a guest power-off
  as TERMINATED with the disk kept, like `gcloud compute instances stop`.
  This needs no cloud credential on the VM (stopping itself through the API
  would need a compute scope that also reaches every other VM) and works
  while the CLI machine sleeps. If pyred does not answer but its tmux server
  has sessions, the node stays up.
- **Stop/start**: stop keeps the disk; Claude/Codex sessions resume from
  their transcripts after start. At power-off the node logs out of the
  tailnet (the ephemeral device disappears); `start` mints a fresh key,
  passes it the same way and removes it after the node connects. Spot VMs
  stop (not delete) on preemption.
- **Delete** refuses while runs are live or a session-created worktree has
  uncommitted changes or a branch neither merged nor pushed (the worktree
  removal rules: a fresh scan when the node is online, else the last
  report). Main checkouts are not covered by that scan. `--force` deletes
  anyway and lists what it overrode. Delete removes the VM with its disks
  and any leftover disk labelled `pyre-node=<name>`, revokes the hub node
  and drops the local snapshot.
- **Cost**: `pyre nodes --cloud` shows estimated $/h from built-in list
  prices (e2-standard-8 ≈ $0.27/h; spot ≈ 35% of that; disk ≈ $0.10/GB-month,
  also while stopped; Windows adds ≈ $0.046/vCPU-h). `prices` in `cloud.json`
  overrides an entry. The TUI Machines detail shows a "Cloud" line from the
  snapshot `cloud-nodes.json` the cloud commands write beside the client
  config (the TUI never runs gcloud).

### Hub reachability over Tailscale (owner steps on server)

Clients and nodes pin the hub certificate by SHA-256 and do not check the
hostname (`transport.PinnedTLSConfig`), so cloud nodes reach the existing
hub at its tailnet address with the existing pin. The certificate is not
reissued and enrolled nodes are unaffected.

1. Install Tailscale on server (`curl -fsSL https://tailscale.com/install.sh | sh`)
   and `sudo tailscale up` (owner login; the hub needs no tag).
2. Tailnet policy (admin console, Access controls): define the tag, let it
   reach only the hub port, and let your devices reach the nodes:

   ```json
   "tagOwners": {"tag:pyre-node": ["autogroup:admin"]},
   "acls": [
     {"action": "accept", "src": ["tag:pyre-node"], "dst": ["server:7443"]},
     {"action": "accept", "src": ["autogroup:member"], "dst": ["tag:pyre-node:*"]}
   ],
   "ssh": [{"action": "accept", "src": ["autogroup:member"], "dst": ["tag:pyre-node"], "users": ["pyre"]}]
   ```

   (`server` as a `"hosts"` alias or its tailnet IP; keep the rules your
   devices already need.)
3. OAuth client (Settings, OAuth clients): scope Auth Keys (write), tag
   `tag:pyre-node`. Keep the secret in a 0600 file or
   `TAILSCALE_OAUTH_CLIENT_SECRET` on the CLI machine.
4. Hub listen address: the hub listens on `--listen` (install default
   `:7443`, all interfaces, which includes `tailscale0`: nothing to change).
   If it is bound to the LAN address, add the tailnet address with
   `pyre-hub serve --also-listen <tailnet-ip>:7443` (installer:
   `install.sh … --service hub --hub-also-listen <tailnet-ip>:7443`) and
   restart the hub (owner-approved rollout). Check from a tailnet device:
   `curl -k https://<server tailnet name>:7443/healthz`.
5. Set `hub_url` in `cloud.json` to `https://<server MagicDNS name or tailnet
   IP>:7443`. The hub's own `--hub-url` stays the LAN URL; only cloud nodes
   use the tailnet one (their credential records it).
6. GCP: `gcloud auth login` (owner account), `gcloud config set project
   <project>` and `compute/zone`, enable the Compute Engine API, create the
   release bucket and upload a signed release. The `default` network keeps
   its rules; `pyre-deny-ingress` overrides `default-allow-ssh` and
   `default-allow-rdp` for Pyre nodes.

### Accounts on cloud nodes

Claude token accounts (`claude setup-token`, I23) reach a new node within
3 minutes with no action. Codex needs one device login per machine:
`pyre accounts login --node <name> [--register]` (link and code on your
terminal; the credential stays on the VM). It survives stop/start and is
lost with delete.

### Windows nodes

`--os windows` uses Windows Server 2025 (`windows-2025`; `--image
windows-2022` for 2022) and the `windows-startup-script-ps1` metadata key. The script (SYSTEM, every
boot) creates the dev user `pyre` with a random password stored only as the
Winlogon LSA secret (never plaintext in the registry, never off the VM) and
turns on auto-logon, so pyred, started by its Task Scheduler logon task,
runs in an interactive session and can launch GUI (Electron) apps. It
installs Tailscale (MSI, unattended, key from a file), the developer
toolchains (default: Git for Windows, Node.js LTS, Python, Visual Studio
Build Tools with the C++ workload; see Developer toolchains), gh, Claude
Code and Codex, the owner's installers and, on GPU nodes, the NVIDIA
driver; and verifies the release zip like
Linux (`ssh-keygen -Y verify` of the Windows OpenSSH client, then the
checksum). Pyre is a per-user install, so SYSTEM does not run `install.ps1`
itself: it stages the zip, `SHA256SUMS(.sig)`, the allowed signers,
`install.ps1` and, for the first enrollment only, the ticket file (JSON
`node_id`, `secret`, `hub_url`, `hub_cert_sha256`) in
`C:\ProgramData\pyre-install`, ACL'd to the dev user and SYSTEM only, and
registers the one-shot task `pyre-first-install`: at the dev user's logon,
as that user, interactive token, not elevated. The task runs
`install.ps1 -Zip <zip> -AllowedSigners <file> -Service node -Name <name>
-HubUrl <url> -Restart -EditPath -EnrollTicketFile <ticket>` (installing into
the user's `%LOCALAPPDATA%` and registering pyred's logon task for that
user), deletes the ticket file when `install.ps1` returns (success or
failure), logs to `%LOCALAPPDATA%\Pyre\first-install.log` (no secrets) and
unregisters itself (SYSTEM removes a leftover at the next boot and reports
the outcome on the serial console). The first boot reboots once so
auto-logon fires it. A new `release_version` takes the same path without a
ticket (`install.ps1` skips enrollment once `node-credential.json` exists)
and runs at once when the user is already logged on. `pyre node image bake
--os windows` only installs Tailscale, the toolchains and the owner's
installers and verifies the release: no user, task or ticket. Idle auto-stop is a SYSTEM scheduled task
that asks the dev user's pyred (`pyred status --json --state <user state>`)
for live runs; an active RDP session counts as busy.

**GPU (media-test) nodes.** `--os windows --gpu nvidia-l4:1` (G2,
`g2-standard-8`) or `--gpu nvidia-tesla-t4:1` (N1) attaches the NVIDIA RTX
Virtual Workstation variant of the GPU (`--accelerator=type=nvidia-l4-vws,count=1`),
which carries the GRID license the Windows graphics driver needs; GPUs
without a vWS variant (A100, V100) are refused for Windows. The estimate
adds about $0.20/GPU-hour for the license (`prices["gpu-license:vws"]`
overrides it). When an NVIDIA PCI device is present the boot script
installs the GRID driver from Google's public driver bucket
(`gs://nvidia-drivers-us-public/GRID/`, read anonymously over HTTPS: the
newest `vGPU<n>/…_grid_win…server20xx….exe`, or `gcp.windows_gpu_driver`,
an `https://` URL or an object in that bucket; set it if the anonymous
bucket listing is refused or to pin a driver), checks the download against
the object's MD5 in the bucket and NVIDIA's Authenticode signature, runs it
with `-s -noreboot -noeula`, and reboots once (the first boot's reboot, or
an extra one on a later boot); it is skipped once `nvidia-smi` exists. It
also sets the Remote Desktop policies "Use hardware graphics adapters for
all Remote Desktop Services sessions" (`bEnumerateHWBeforeSW=1`) and H.264/AVC
444 with hardware encoding, so GPU apps render over RDP. The `parsec`
toolchain (or the `media` preset) installs the Parsec host for smoother
viewing over Tailscale; sign in to Parsec yourself once over RDP. Images
baked on the CPU builder carry no GPU driver: GPU nodes install it at
first boot.

**Owner installers.** `gcp.windows_installers` runs installers Pyre must
not fetch itself (vendor downloads behind registration forms, licensed
software): upload the file to the project's private bucket and list it:

```json
"windows_installers": [{"name": "davinci-resolve",
  "gcs_uri": "gs://<bucket>/installers/DaVinci_Resolve_<v>_Windows.exe",
  "sha256": "<sha256 of the file>", "args": ["/i", "/q", "/noreboot"]}]
```

The boot script downloads each with the VM's read-only storage identity
(setting the list is what gives a Windows VM the `devstorage.read_only`
scope), checks the SHA-256, runs it silently (`.msi` through `msiexec /i …
/qn /norestart` plus `args`; an `.exe` with `args` as given, unquoted) and
records `installer-<name>-<hash prefix>.done`, so it runs once per file
(a new hash runs again). Failures are retried on the next boot (node mode)
or fail the bake. Installers also run in Windows bakes. `sha256sum
<file>` (Linux) or `shasum -a 256 <file>` (macOS) gives the hash.

**Two Windows machines.** The toolchain list and the installers come from
the config file, so keep one file per machine role (shared settings such as
`hub_url`, `release` and `tailscale` repeated in both):

```sh
# ~/.config/pyre/cloud.json        gcp.toolchains = the Vortex list above, no installers
# ~/.config/pyre/cloud-media.json  gcp.toolchains = ["media"],
#                                  gcp.windows_installers = [the davinci-resolve entry above]

# win-build: CPU, builds the Electron app like CI
pyre node create --cloud gcp --name win-build --os windows --type e2-standard-8 --rdp

# win-media: L4 GPU (RTX vWS, g2-standard-8), media preset, Resolve from the private bucket
PYRE_CLOUD_CONFIG=~/.config/pyre/cloud-media.json \
  pyre node create --cloud gcp --name win-media --os windows --gpu nvidia-l4:1 --rdp
pyre node rdp win-media --reset-password --open   # then sign in to Parsec and Resolve once
```

win-build gets no storage identity (unless the release is on `gs://`);
win-media gets the read-only storage scope for the installer. Stop
win-media when idle: an L4 vWS node costs several times a CPU node.

Remote Desktop (`--rdp`) is reachable from tailnet addresses only: no VPC
rule opens 3389 (`pyre-deny-ingress` blocks it from the internet) and the
Windows firewall's Remote Desktop rules are limited to `100.64.0.0/10` and
`fd7a:115c:a1e0::/48`. `pyre node rdp <name>` prints the node's tailnet
address (from the local `tailscale status`) and writes/opens a `.rdp` file
without credentials (macOS: Windows App). `--reset-password` runs
`gcloud compute reset-windows-password` and prints the password once; Pyre
never logs or stores it. For the dev user the new password is also handed
to the VM's auto-logon refresher through the `pyre-autologon-password`
metadata key, removed again after 20 s.

## Account profiles

One isolated profile per (account, node, harness), created by a fresh login
inside it (decision I08). pyred's profile root is `~/.local/state/pyre/profiles`
on both OSes (override with `PYRE_PROFILE_ROOT`; an install that already has
`<state>/profiles` keeps using it). Never
copy a credential file between profiles or machines: a copied login logs the
loser out, and Codex refreshes twice on a shared home. Pyre and Rota never
refresh harness-owned grants; `rota switch` and seeding from Rota snapshots
are forbidden for managed sessions.

Create or re-authenticate a profile interactively with
`spikes/accounts/real-profile-check.sh` (user-run; it never prints tokens and
checks that the default login is unchanged):

```sh
spikes/accounts/real-profile-check.sh claude-login <account>
spikes/accounts/real-profile-check.sh codex-login <account>
```

The script defaults to `~/.local/state/pyre/profiles`, which matches pyred on
both OSes (set `PYRE_PROFILE_ROOT` for both if you use another root, or point
it at `<state>/profiles` on an older install). Repeat on every node
that should run that account. `pyre accounts` shows eligibility, usage age,
grants per node and dead/expiring grants. Uninstall never deletes profiles
unless `--purge --include-profiles`.

**Claude first-run screens.** Claude opens its onboarding (the theme picker
"Choose the text style that looks best with your terminal", then "Security
notes") whenever the profile's `.claude.json` lacks `hasCompletedOnboarding`;
the theme's location does not matter (Claude 2.1.304 keeps it in
`settings.json` and drops a default `"dark"` from `.claude.json` when it
saves, so a missing `theme` key there is normal). pyred adds
`hasCompletedOnboarding: true` (and, for older Claude, `theme: "dark"`) to a
profile's `.claude.json` before every launch, never changing a value that is
there; a file it cannot read, parse or write is left alone and logged once
(`first-run answers not added to ...`). If the onboarding still appears, the
adapter answers it on screen before the session starts: one Enter on the theme
picker (keeps the highlighted theme; change it later with `/theme`) and one on
the security notes, audited once per run as `node.onboarding_answered`. A
screen still shown is pressed again only after 3 s (at most 3 times), so a slow
redraw never turns an extra Enter into an answer to the next screen; the login
method, API-key and workspace-trust steps are never answered by this. A run
with no hook event at all, no input box and no recognised dialog 90 s after
launch is reported as `waiting_approval` "needs human: Claude first-run screen
(attach to answer it)" when the screen shows Claude's "Welcome to Claude Code"
onboarding banner (attach and answer it); otherwise its reason reads "Claude
has not started (no SessionStart after 90 s)".

### Rota accounts (rota checkout)

To run sessions on accounts rota already holds on a node (the lock-aware
rota from `pyre-integration` must be installed there):

```sh
pyred run ... --rota-checkout [--rota-independent acct_a,acct_b]   # on the node
pyre accounts register --node laptop --provider anthropic --rota work-max [--label "Work Max"] [--independent]
pyre accounts checkouts --node laptop
pyre accounts checkin "Work Max" --node laptop                   # refused while runs use it
```

`register` creates (or reuses, by provider and rota name) the hub account,
registers it on the node (an isolated profile backed by the snapshot; the
node's live default login is never used) and records the snapshot's
nonsecret identity as the account's provider ref. A checkout moves the login
into the profile for the duration of use and writes rotated tokens back to
rota; the hub fleet lease keeps one node at a time unless the account is
per-machine independent.

Availability of a checked-in rota account: Claude snapshots are probed with
the snapshot's access token (never refreshed). A Codex snapshot cannot be
probed without starting Codex on rota's home, so automatic selection verifies
it at checkout instead: the login is moved into the profile, the usage probe
runs there, and the run is admitted only on a fresh eligible result;
otherwise the account is checked straight back in and keeps the reported
reset. A snapshot whose access token expired at rest (an idle account) is
"eligible, unverified": it ranks below verified accounts, and when chosen it
is checked out, the harness refreshes its own login inside the profile
(Claude: its TUI started briefly with no prompt and hooks off; Codex: the
app-server's `account/read` with `refreshToken`), the refreshed login is
written back to rota, and the usage probe decides before the run exists. An
account found at its limit is checked back in and selection moves on or
waits, so the first prompt is never sent to it. Pyre never refreshes a login
rota still holds. If the harness cannot refresh, automatic selection skips
the account for 30 minutes (explicit `--account` still launches). After a
limit's reset the account is probed at the next selection, without waiting
for its poll interval.

Check-in also waits while any process is still bound to the profile through
`CLAUDE_CONFIG_DIR` or `CODEX_HOME` (Linux, from `/proc/<pid>/environ`). A
helper that outlives the run, such as Hive's detached enrichment worker that
calls `claude -p` after `SessionEnd`, would otherwise use, and possibly
refresh, a login that has already moved back to rota. The checkout shows
`pending_checkin` with the process IDs; the sync loop keeps writing rotated
tokens back and checks in when they exit. macOS does not expose other
processes' environments reliably, so there only the adapters' own busy checks
apply.
The daemon itself also holds the profile while it hands an ended Codex run's
rollout to Hive (`hive mem capture`, in the background); check-in follows
the capture.

### Token accounts (`claude setup-token`)

A rota login is a refresh family: a copy used on two machines logs one of
them out. A long-lived token from `claude setup-token` (`sk-ant-oat01-…`) is
not. The same token runs on every machine at once. Make such an account
once from any client:

```sh
claude setup-token                                   # on any machine, once; copy the token
pyre accounts add-token personal --provider anthropic --keychain-service pyre-claude-token-test   # macOS keychain item (account "pyre")
pbpaste | pyre accounts add-token personal --provider anthropic                                  # or on stdin
pyre accounts add-token personal --provider anthropic --node linux-box --node mac-mini         # only these nodes
pyre accounts tokens                                 # token set · added Oct 6 · last verified 3m ago
pyre accounts --refresh                              # probe token accounts' usage now
pyre accounts remove-token personal
```

How it works:

- **Storage.** The hub keeps the only copy, encrypted at rest with an age X25519 key in `<hub data>/secrets/account-tokens.key` (0600). No read API returns it. Hub backups hold only the ciphertext, not the key, so after a restore on another machine you run `add-token` again (or copy the key file yourself).
- **Delivery.** A node fetches the token over its authenticated TLS link (`accounts.token.fetch`, allowed only for node credentials and only for the account's allowed nodes). It fetches when a run is bound to the account or its usage is probed, and keeps the token in memory only, refetching after 10 minutes. During a hub outage it reuses the in-memory copy for up to 12 hours.
- **Launch.** The run gets the token only through its private environment file (I18), as `CLAUDE_CODE_OAUTH_TOKEN`, with its own isolated profile. That profile is composed as usual (I22) and holds no credential file. `ANTHROPIC_API_KEY` and the other login variables are removed. The token is never in a start command, `run.json`, the node DB, the journal, events or delivery records.
- **Concurrency.** Runs on several nodes may use the token at the same time, so no lease is needed. The account is marked per-machine independent.
- **Usage.** Usage comes from one minimal Messages request: Haiku 4.5, `max_tokens` 1, about 8 input tokens and 1 output token per probe. Its `anthropic-ratelimit-unified-*` headers carry 5-hour and weekly utilization, reset and status. The token has inference scope only, so `/api/oauth/usage` answers 403. A node probes at most every 10 minutes per account while the account runs there. It also probes at selection when the reading is older than 10 minutes, and on `pyre accounts --refresh` (at most once a minute). An idle account shows its last reading as "ready · checked on use".
- **Failures.** HTTP 401/403 means the token was revoked or expired. The account shows *needs re-auth* ("run `claude setup-token` and `pyre accounts add-token` again") and is never selected; `add-token` with a new token revives it on every node. HTTP 429, or unified status `rejected`, means the account is at its limit until the reported reset. Switching and handoff then work as for any account.
- **Preference.** When a token account and another login (such as rota) belong to the same person, matched by label, rota name or provider identity, selection prefers the token account. The cards and `pyre accounts` tag each account's kind (`· token`, `· rota`).
- **Machine spread.** Automatic selection keeps machines on separate accounts where it can. This is a soft preference: it never makes a run fail and never picks an at-limit or ineligible account while a usable one exists. Explicit `--account` and Prefer lists come first.
  - A machine stays on its *home* account, the one it is using now or used last, while that account is below 80% on the 5-hour window and below 70% on the weekly window.
  - Otherwise it takes the account with the most headroom that no other machine is using. Only when every eligible account with room is in use elsewhere does it share: fewest other machines first, then headroom.
  - When all accounts are near a limit, the most headroom wins.
  - *Most headroom* means the lowest binding window: the higher of the 5-hour and weekly use, then the lower weekly use, then the lower 5-hour use. An account at 97% weekly never wins because its 5-hour window is empty.
  - The node reads other machines' active runs from the hub (`accounts.list`) before each automatic choice: 2 s timeout, reused for 5 s. If the hub is unreachable, there is no spread for 30 s: only home stickiness and headroom, and the reason says "fleet view unavailable".
  - Two machines choosing at the same moment can land on the same account; nothing is reserved. Their next runs rebalance: the machine whose run started first keeps the account, and the other moves to a free one. Running sessions are never moved.
  - The assignment reason says why, e.g. "home account for mac; not used on other machines (5h 3% used, weekly 20%)". `pyre accounts` and the TUI show which machine holds an account ("in use by <session> on mac").
- **Moves between turns.** Every 2 minutes a node moves up to 3 sessions that are idle at the prompt off an account at 85% weekly or 90% 5-hour, so they do not hit the limit mid-step. The move is a native resume on the best eligible account below the near-limit thresholds, with no prompt: the session stays idle and its next message runs on the new account. Only safe moves happen: the run has read idle for at least a minute and no message for it is queued or was delivered in the last minute; the starter did not pin the account strictly; the harness can resume across accounts over the shared transcript store (Claude) and the successor may only be a native resume (never a handoff); at most one move per session per 30 minutes. If the new run fails to start, the session resumes on its original account and is not moved again for an hour; if that fails too, the move is reported as needing you. Each move runs under a 3-minute deadline and never blocks the limit fallback. Proactive moves do not count toward the switch circuit breaker. A stop that is not confirmed is checked again: an ended run resumes on its original account, a run that may still stop is reported as needing you, and claims a restarted pyred left behind are settled the same way at startup. A limit that arrives during a move is handled when the move lets go of the run. Runs that were active when a node upgraded to this version count as strictly pinned and are never moved. Rolling pyred back past this version needs a restore of the node database from before the upgrade: older binaries refuse the newer accounts schema (migration v4). The audit log shows "account switched" with a reason starting "proactive move between turns".
- **Reserve accounts.** `pyre accounts tier <account> reserve` (by ID, label, rota name or prefix, like `rename`; `--provider` when a label names accounts of several providers; `normal` undoes it) keeps an account for when the others run out. Automatic selection uses a reserve account only when every normal account of that provider is at its limit, unavailable, or near its limit: 5-hour or weekly window at 80% (what `pyre accounts` shows as "near limit"). Then the usual ranking decides, and the assignment reason starts "reserve account: no normal … account has room". `--account` and Prefer lists still pick a reserve account. Placement across machines prefers one where a normal account has room. If every normal account fails checkout for a launch, reserve accounts compete for the rest of it. Idle sessions that selection or a limit fallback put on a reserve account move back to a normal account between turns once one has room again (the rules of the moves above, at most one per sweep), never sessions started with that account named, never strict pins. Sessions already running when a node upgrades count as named only when they were explicit, preferred or strictly pinned, so the others on a newly reserved account drain too. Upgrade every pyred before reserving an account: an older node ignores the tier. `pyre accounts` shows "reserve" in NOTES; the TUI's Accounts view tags the row. The change is audited ("moved account … to the reserve tier"). Nodes read the tier from the hub within seconds; a node older than this version ignores it. Rolling pyred back past this version needs the node database from before the upgrade (migration v5).
- **Explicit accounts.** `--account X` (MCP `account`) uses X. When X cannot take the run (at its limit, needs re-auth, Codex home busy, checkout refused), the launch uses the best eligible account of the same provider instead, and the assignment reason starts `account "X" is unavailable (…)`. `--strict-account` (MCP `strict_account`) keeps X and fails as before. The TUI's account pick and fallback successors are strict.
- **Codex with no free account.** `pyre start --harness codex` asks the node (`account.select`, a dry run) before creating the session. With no free OpenAI account and no `--node`, it first tries the other connected Codex nodes: without `--cwd` the launch moves to the first with a free account; with `--cwd` the error names them. Otherwise it fails at once with the reason and the fix, and no session is created. The dry run and the launch share the own-login fallback, so the check never refuses what the launch would take. `--fallback-harness claude` (MCP `fallback_harness`) starts a Claude session instead, titled "Codex unavailable, Claude review: …", tagged `codex-unavailable`, with a first-prompt note. Use it for reviews and second opinions. A node that cannot answer the dry run is not blocked.

Codex access tokens (`--provider openai`, `CODEX_ACCESS_TOKEN`) register and show, but they are not selected for runs yet. Launch and usage in Codex's token mode are unverified.

A node that runs under an isolated `HOME` (a test node) but should use the
owner's rota passes `pyred run --rota-home /home/<owner>`: rota's snapshots
(`.claude/accounts`, `.codex/accounts`) and the live default logins that are
never checked out (`.claude.json`, `.codex/auth.json`) are then read from
that home instead of the daemon's own.

Testing only: `pyred run --accounts-test-mode` (or `PYRE_ACCOUNTS_TEST_MODE=1`)
enables `account.inject_limit` and sub-5-minute usage polls for verification
runs; `PYRE_ACCOUNTS_INJECT_RESET=3m` shortens the reset an injected limit
announces (default 1h). Never enable it on a production node.

## Upgrade and rollback

```sh
scripts/install/upgrade.sh --tarball pyre-<new>-<os>-<arch>.tar.gz     # auto-detects installed services
scripts/install/upgrade.sh --rollback                                 # back to .previous
scripts/install/upgrade.sh --rollback --to <version>
```

Sequence: verify the release signature against the pinned key -> stage the
new version beside the current one (checksums, immutable
version directory, `pyred version --json`) -> compare the new pyred's protocol
range with the hub's (`pyre hello --json` with the installed pyre, through
the client config, else
through the node credential; `--hub-protocol MIN:MAX` or `--skip-hub-check`
when the hub is unreachable) -> record live runs -> switch `current` ->
restart the service -> wait until `pyred status` reports the new version, a
connected hub link and at least as many live runs -> otherwise switch back,
restart and verify the previous version. A hub on the same host is restarted
before the node.

| Exit | Meaning |
| --- | --- |
| 0 | upgraded (or already current) |
| 3 | staging refused (foreign files, mutated version) |
| 4 | protocol ranges do not overlap: nothing switched, new version stays staged |
| 5 | new version unhealthy: rolled back; it is marked `<version>/.failed` and is not a rollback target |
| 6 | rollback also unhealthy: operator action needed (see troubleshooting) |

When the protocol rises, upgrade the hub first; nodes on an older protocol are
then rejected with an explicit protocol error rather than misread.
`--allow-run-loss` accepts a daemon that comes back with fewer live runs
(warning only). Runs are never relaunched by an upgrade.

Pruning: after a successful upgrade (and after `install.sh` activates a
version) every installed version other than `current` and `.previous` is
removed and left behind as the alias `<version> -> current`. Sessions started
under an old version have its absolute path in their Claude hooks, MCP
command and environment (`…/share/pyre/<version>/bin/pyred`); through the
alias those paths keep working and run the current binaries, so the earlier
manual "symlink the removed version" workaround is no longer needed.
Processes still running from a pruned version keep their executable.
`--no-prune` keeps every version. A pruned version is not a rollback target
(`--rollback --to` refuses it; reinstall it with `--tarball`, which replaces
the alias). `uninstall.sh` removes the aliases with the versions.

Second mechanism: `pyre update stage|activate|rollback|ls --node N --tool
claude|codex|pyred` manages versioned tools under `<state>/tools/` on a node
(staged, digest-checked, pinned per run). For `pyred` it updates the node
daemon in place without touching the release install above:

- `stage --tool pyred --source <release tarball>` needs `SHA256SUMS` and
  `SHA256SUMS.sig` beside the tarball and verifies them against the pinned
  release key (see Signing keys).
- `activate` checks the protocol range against the hub, switches
  `<state>/tools/pyred/current` and restarts the daemon a few seconds later
  (after the command result is sent): pyred execs the installed pyred (the
  launcher the service unit starts), which hands off (exec, same PID) to the
  active managed pyred. Runs are recovered, not restarted.
- The first boot of a newly activated pyred must reach its hub (standalone:
  start) within `PYRE_DAEMON_HANDOFF_TIMEOUT` (default 90s). Otherwise, or if
  it crashes before that, the launcher rolls back to the previous managed
  version or to itself, marks the version `failed` and reports
  `update.result failed_reverted`.
- `rollback --tool pyred` returns to the previous managed version, or to the
  launcher when there is none.
- A managed pyred is used only over the launcher version it was activated
  over. After `upgrade.sh` or `install.sh` installs another release, the
  managed activation is superseded (logged, marked previous): `upgrade.sh`
  stays authoritative, and nothing downgrades silently.
- The hub is not updated by this path; update it with `upgrade.sh`.

- Sources: an absolute path or `file://` (for example a Claude version from
  `~/.local/share/claude/versions/<v>`, copied, never moved), `https://` with
  `--digest`, or `npm:<package>@<version>` with `--digest` (the sha256 of the
  `npm pack` tarball; installed into an isolated prefix, never the global
  one). A wrong digest discards the candidate.
- `activate` re-runs the health check through `current`; a failing version is
  marked `failed` and the previous one stays active ("Running sessions were
  not affected"). `rollback` returns to the previous version. `ls` shows
  which runs still use each version.
- Runs on a Pyre-managed Claude get `DISABLE_AUTOUPDATER=1`: Claude's native
  updater would otherwise install into and repoint the owner's own Claude
  installation. Runs on the owner's external claude keep its updater.

Hub schema and rollback: an older hub refuses a database whose migration
namespace is newer than it knows. After the release that adds hub migration
`hub_childreport` v3 (column `child_reports.turn_ended_at`, the turn end of
an ended child report), rolling the hub back needs, with the hub stopped:
`sqlite3 <hub.db> "DELETE FROM schema_migrations WHERE namespace='hub_childreport' AND version=3"`.
The column can stay (older binaries do not read it). Before upgrading again,
drop it (`ALTER TABLE child_reports DROP COLUMN turn_ended_at`), or v3 fails
with "duplicate column name". The same applies to `hub_waiting_on` v2
(column `session_waits.quiet`):
`sqlite3 <hub.db> "DELETE FROM schema_migrations WHERE namespace='hub_waiting_on' AND version=2"`,
and `ALTER TABLE session_waits DROP COLUMN quiet` before upgrading again.

## Managed environment profiles

A profile is the desired, secret-free managed layer for a node's Claude and
Codex runs: settings, skills, plugins, MCP servers, hooks and connection IDs
(`internal/configuration/profile.go`). The node keeps two copies:

- **Active** (`<state>/env/profiles/...`): what new runs resolve.
- **Desired** (`<state>/env/desired/...`): written by `pyre env profile set`
  and waiting for review.

```sh
pyre env profile set --node N --file profile.json [--project P]   # store desired, print the plan
pyre env profile get --node N                                     # active, pending desired, applied generation
pyre env apply --node N --plan PLAN --digest sha256:...           # make it active, materialize home items
pyre env drift --node N
pyre env rollback --node N --generation GEN                       # earlier profile active again
```

- A desired profile reaches new runs only through `env apply` of its plan;
  the plan lists the live Claude/Codex sessions that keep their current
  generation (restart required). Running sessions are never changed: each
  keeps its generation's overlay files (`--mcp-config`, `--plugin-dir`).
- `env rollback` re-materializes the earlier generation's home items and makes
  its profile active again; it is refused while a managed item has local
  edits (fix or revert them first). User additions are reported as
  `unmanaged_addition` and preserved.
- Managed `claude.*` values and Claude hooks go into the generation's
  settings overlay, which the Claude adapter merges into the run's own single
  `--settings` file after Pyre's runtime hooks (`disableAllHooks` is
  refused). The owner's `~/.claude/settings.json` is never written (except
  keys a global profile lists in `persist`).
- Codex values, MCP servers, plugins and hooks become `-c` overlays that the
  Codex adapter applies as per-thread config of the run's thread on the
  shared app-server (re-sent on every resume, so a running thread keeps its
  generation). Nothing is written to any `CODEX_HOME`. Keys that overlap
  Pyre's per-run identity (`mcp_servers.pyre`,
  `shell_environment_policy.set.PYRE_*`) are refused at launch.
- A project profile (`--project P`) overrides the global (node) profile for
  that project's runs: its values win, and a same-name MCP server replaces
  the global definition. Project skills are delivered per run (Claude plugin
  overlay); Codex has no per-run skill overlay.
- Home-delivered skills (`"delivery": "home"`) are installed into the node's
  default Claude home (`~/.claude/skills`, or `pyred run --env-home DIR` for
  an isolated test node); account profiles see them through their linked
  `skills` directory.
- A profile file placed by hand in the active store takes effect for new runs
  without review; use `env profile set` instead.

## Connection credentials on a node (local_credential)

Pyre-provisioned connection credentials live in the node's OS store: the
macOS Keychain (default search list) or, on Linux, the Secret Service through
libsecret's `secret-tool` on the session bus (an unlocked gnome-keyring or
KeePassXC). There is no plaintext fallback. Select the store explicitly with
`pyred run --credential-backend auto|keychain|secret-service` and, on macOS,
`--credential-keychain FILE` to keep them in a dedicated keychain file
(also `PYRE_CREDENTIAL_BACKEND`, `PYRE_CREDENTIAL_KEYCHAIN`). The store CLIs
get the daemon's `HOME`, `DBUS_SESSION_BUS_ADDRESS` and `XDG_RUNTIME_DIR`.

Provisioning is owner-initiated through node actions (there is no CLI
command yet): `connection.provision_offer` on the recipient node (a one-time
X25519 key, 15 minutes at most), `connection.provision_seal` on the node
that holds the value (its own store reference), then
`connection.provision_accept` on the recipient, which stores the value and
records the variable name. Only ciphertext crosses the hub, and a replayed
ciphertext is refused. `connection.deprovision` removes the value and the
record. The node pushes a connections report after accept, deprovision and
`pyre connections verify`, also when Outfit is not installed on it.

`pyre connections verify CONN [--node N]` has pyred on each node run the real
check now (`outfit connector test NAME` for an Outfit connection, a store read
for a Pyre-provisioned credential) and prints each node's state and detail
(an Outfit that does not know the name reports `missing`, "connection ... is
not configured in Outfit on this node"). It exits 0 only when every node
reports `verified`, else 1 (with `--json` the results are still on stdout).
pyred runs in the user's login session, so `verified` means Pyre sessions on
that node can use it; an ssh login there can reach a different credential
store (Windows Credential Manager under an ssh key login, a locked macOS
keychain) and see another result.

A run receives the credential when the node's active environment profile
lists the connection and a grant pushed to that node covers the session at
launch (`pyre connections grant CONN --project P --node N`, or
`--session`). The no-grant default of decision I37 applies only to invoking
a connection (`connection_invoke`), never to provisioning: without a grant,
no credential is put into a run's environment. This holds for Claude and
Codex runs (through their generation) and plain shell runs (through the
default profile's connections). The value goes into the run's environment only; the
run's `adapter_data["pyre.connections"]` manifest lists the connection,
variable name and store reference. A revoke affects the next launch (a running
process keeps its environment); a locked or missing store fails a granted
launch with an actionable error.

## Copying Outfit connections to other machines

Every machine holds its own copy of the owner's Outfit connections, and
running sessions invoke the connections their machine holds (or another
machine holds, through `connection_invoke`) without a grant, unless the
owner manages that connection with grants (decision I37). To copy this
machine's connections, credentials included, to others:

```sh
scripts/outfit-sync.sh laptop devbox win-home      # ssh hosts
scripts/outfit-sync.sh --only github,linear laptop       # a subset
scripts/outfit-sync.sh win-home=win-home-pc               # ssh host = Pyre node name
```

It creates an Outfit backup here (a random one-time passphrase in a 0600
file, passed with `--passphrase-file`, never in argv) and, per target:
copies both files into a private temp directory, runs `outfit backup restore
--replace` (the backup and passphrase are deleted there as soon as it
returns), `outfit doctor` and `outfit skill list` (which skill folders were
written), checks each connection, and deletes the target's directory before
moving on. Per connection it prints one verdict:

- `FAILED NAME on HOST: not restored` when the restore did not report it
  ready (Outfit prints `done<TAB>NAME is ready` for each one it restored; a
  restore that exits non-zero without that line counts as failed). It is not
  checked further and never reported as verified.
- On a connected Pyre node: `verified NAME on NODE in Pyre sessions` or
  `FAILED NAME on NODE in Pyre sessions: STATE (DETAIL)`, from `pyre
  connections verify` (pyred runs the connector test in the login session),
  plus an informational `ssh login on HOST: NAME works` / `does not work
  there` from `outfit connector test` over ssh. On Windows the two often
  differ (Credential Manager), and Pyre sessions are what count.
- Without a Pyre node: `verified NAME on HOST in an ssh login` or `FAILED
  ...`, from `outfit connector test` over ssh.

Every `FAILED` connection is listed again in the summary (`FAILED
connections: host:name ...`) and makes the script exit 1.

Remote commands run in a non-login shell, so the script puts
`~/.local/bin`, `~/bin`, `~/go/bin`, `~/.cargo/bin`, `/opt/homebrew/bin` and
`/usr/local/bin` (Windows: `%USERPROFILE%\.local\bin`) in front of the
remote PATH: Outfit and the provider CLIs it runs (`gh`, `agent-slack`, ...)
are found there. `--outfit PATH` with a leading `~/` or `$HOME/` (quoted, so
your shell keeps it) means the target user's home. Ctrl-C stops the
run and cleans up; a cleanup that fails is reported with the path to remove
by hand. Hosts are validated (no leading `-`); an unreachable host is
reported and skipped.

Not copied, and listed in the final summary:

- Connections whose vault secret is missing here (`outfit doctor` shows
  `credential missing`; reconnect them with `outfit credential replace
  NAME`). The script then exits 1.
- Outfit-managed OAuth connections (Linear, Notion, Sentry, Slack hosted
  MCP, ...), unless `--include-oauth`. Copies of one refresh token on
  several machines invalidate each other, the source's included, when the
  provider rotates it; connect those on each machine.

A connection that cannot be restored on a target (a CLI with no build for
it, an expired account) is put back as it was and reported; the others
still restore and the script exits 1. Restore probes the target's vault
(store and delete a dummy secret) before verifying any account, so a
keychain that refuses writes stops it before anything is refreshed at a
provider; a vault failure in the middle stops the remaining connections.

The restore runs as a Pyre shell session on the target node (`--via pyre`,
the default when the node is connected). pyred runs inside the user's login
session there, so the macOS login keychain, the Linux Secret Service and the
Windows Credential Manager accept the writes. `--via ssh` runs it over ssh
instead: fine for Outfit's file vault or an already unlocked keychain. A
macOS login keychain over ssh usually refuses ("User interaction is not
allowed"), and so can Credential Manager under ssh key logins; there is no
clean non-interactive unlock (`security unlock-keychain` needs the login
password, `launchctl asuser` needs root), so use the Pyre route there.
Windows targets need PowerShell as the shell Pyre and ssh run.

On a Linux machine without a desktop secret service, choose Outfit's file
vault once with `outfit vault use file` (`outfit vault status` shows the
choice and where it comes from; `outfit doctor` prints it too). Nothing
switches the backend implicitly, and secrets stored under the other backend
stay readable.

## Uninstall

```sh
scripts/install/uninstall.sh                    # stop + disable services, remove units, shims, versions; keep data
scripts/install/uninstall.sh --stop-runs        # also kill Pyre-owned tmux servers (only <state>/tmux/pyre.sock)
scripts/install/uninstall.sh --purge            # print exactly what --purge would delete; deletes nothing
scripts/install/uninstall.sh --purge --yes      # delete it
```

`--purge` deletes node state, hub data, `~/.config/pyre/client.json`, the
bootstrap node credential, the macOS log directory and, on macOS, the CLI's
own state (`~/.local/state/pyre/{cache,drafts}`). Account profiles and the
`shared/` transcript store beside them (the session histories the profiles
link to) are kept, and listed as kept, unless `--include-profiles`. It never deletes `~/.config/pyre` as a directory (the
Android signing key lives there), Hive/Outfit/Rota data, `~/.claude`,
`~/.codex`, or the user's tmux server. It refuses while Pyre runs are alive
unless `--stop-runs`. Revoke the node on the hub too: `pyre revoke node <id>`.

What `--purge` leaves behind, so delete it by hand if you want it gone:

- **A `--remote-dir` bootstrap:** the node credential under that directory
  (only the default `~/.local/share/pyre/node-credential.json` is purged).
- **A CLI state directory moved with `$PYRE_STATE_DIR`** (only the default
  `~/.local/state/pyre`, or `$XDG_STATE_HOME/pyre`, is purged).

Run the scripts from the source checkout or the extracted release you
installed from. The install itself does not keep a copy.

## Backup and restore

```sh
scripts/backup/backup-hub.sh --data <hub-data> --out-dir <dir> --recipient age1... --verify
scripts/backup/restore-hub.sh --in <archive> --data <NEW-dir> --identity key.txt --check-listen 127.0.0.1:7444
```

Both scripts run `pyre-hub` from `PATH` (`--pyre-hub BIN` or `$PYRE_HUB_BIN`
otherwise), so `~/.local/bin` must be on `PATH`, which a non-interactive ssh
command may not provide. A post-quantum `age1pq1…` recipient is about 2,000
characters long; `--recipients-file F` is easier to handle than `--recipient`.

`pyre-hub backup` takes a transactionally consistent SQLite snapshot
(`VACUUM INTO`, checked with `PRAGMA integrity_check`), then the
content-addressed artifact blobs and the TLS identity, into a tar with a
digest manifest. It is safe while the hub serves. The snapshot is staged in
a temporary directory inside the hub data directory (same protection as the
live database) and removed afterwards; a crash can leave a
`<hub-data>/.pyre-backup-*` directory, which is safe to delete.

**Encryption is age** ([age-encryption.org/v1](https://age-encryption.org),
`filippo.io/age` v1.3.2), applied while the tar is produced: no plaintext
archive is ever written to disk. The output is `pyre-hub-<UTC>.tar.age`, 0600.
`pyre-hub backup` refuses to run without one of these modes unless
`--unencrypted` is given (with a warning):

| Mode | Flags | Decrypt |
| --- | --- | --- |
| Recipients (recommended) | `--recipient age1…` (X25519), `--recipient age1pq1…` (post-quantum ML-KEM-768 + X25519 hybrid) or `--recipient "ssh-ed25519 …"`; repeatable; `--recipients-file F` (one per line, `#` comments). Every recipient can decrypt | `--identity key.txt` (age identity file or an unencrypted OpenSSH private key) |
| Passphrase | `--passphrase-file F` or `$PYRE_BACKUP_PASSPHRASE` (age scrypt, work factor 2^18: about 1 s and 256 MiB on each backup and restore) | the same passphrase |

age refuses combinations that would weaken a mode: a passphrase cannot be
combined with recipients, and post-quantum `age1pq1…` recipients cannot be
mixed with X25519/SSH ones. Generate keys with the age tools, e.g.
`age-keygen -pq -o hub-backup.key` (public key: `age-keygen -y
hub-backup.key`), and keep the private key off the hub host. The scripts and
`pyre-hub backup` only ever take public keys.

Any age implementation decrypts the archive, so a restore does not need Pyre:

```sh
age -d -i hub-backup.key pyre-hub-<UTC>.tar.age | tar -t      # list
age -d pyre-hub-<UTC>.tar.age | tar -x -C <dir>               # passphrase: age prompts
```

`--verify` decrypts the finished archive again as a stream and checks every
digest against the manifest, writing nothing. In recipient mode the archive
gets one extra, ephemeral recipient for that check; its private key exists
only in the `pyre-hub` process's memory and is discarded, so `age-inspect`
shows one more recipient than you gave. `pyre-hub backup --out -` streams to
stdout (for piping into another tool; `--verify` needs a file).

`pyre-hub restore` detects the format from the archive's first bytes: age
(binary or ASCII armored), the legacy pyre passphrase format
(`PYRE-HUB-BACKUP-ENC/1`, written before age; still readable, never written),
or a plaintext tar. It decrypts as a stream, writes the files into a 0700
staging directory beside the target, checks every digest and `PRAGMA
integrity_check`, and only then renames the staging directory to the target
(which must not exist or be empty). Missing or wrong keys, truncation,
tampering and trailing data all fail before the target appears. `--in -`
reads stdin; `--verify-only` checks an archive without writing anything.

`backup-hub.sh --encrypt auto|recipients|passphrase|gpg|none` wraps this
and writes a `.sha256` beside the archive. `auto` picks recipients when
every `--recipient` is an age/SSH key, gpg when they are not, else the
passphrase if one is set, else none. `gpg` streams `pyre-hub backup --out -
--unencrypted` through a pipe into `gpg --encrypt` (`.tar.gpg`; nothing in
plaintext on disk; `--verify` there needs the secret key in the keyring).
`--encrypt none` warns: the archive contains the hub TLS private key and
private project data. `restore-hub.sh` passes `--identity` /
`--passphrase-file` to `pyre-hub restore` and pipes `.gpg` archives
through `gpg --decrypt`; it optionally starts the restored hub briefly to
check `/healthz`, and prints the reconnect steps:

- The TLS identity and the token hashes are restored: pins and existing
  client configs stay valid.
- Same hub URL: nodes reconnect on their own. New URL: start pyred with
  `--hub <new-url>` (or re-enroll) and re-run `pyre login --hub <new-url>
  --pin <pin> --token - --force` on clients; re-pair Android.
- Everything written after the backup is lost. A node whose acknowledged
  reports are newer than the backup reconnects with a `node report gap`
  warning in the hub log; those reports are not replayed. Instead the node
  sends a run snapshot (its most recent 1000 run records with their
  sessions): runs that ended after the backup show their final state rather
  than `unknown`, and sessions started after the backup reappear with their
  titles. Messages, approvals and project data written after the backup are
  not recovered.

Hardware/plugin identities (`age-plugin-yubikey` …) and passphrase-protected
SSH keys are not built in; use the age CLI on either side of a pipe:
`pyre-hub backup --data D --out - --unencrypted | age -r age1yubikey1… -o
X.tar.age` and `age -d -i yubikey-identity.txt X.tar.age | pyre-hub restore
--in - --data NEW`.

**Migrating from the pre-age format.** Existing backups stay restorable:
legacy `.tar` archives written with the built-in passphrase mode restore with
`--passphrase-file` (format detected from the header), `.tar.age` archives
written by the old script through the external `age` CLI restore with
`--identity`, and `.tar.gpg` archives as before. New backups are always age;
the script's output for passphrase mode changes from `.tar` to `.tar.age`,
and `--encrypt age` is now an alias for `--encrypt recipients` (no external
`age` binary needed). To move off the legacy format, take a new backup; to
re-encrypt an old archive, `pyre-hub restore` it into a scratch directory and
back that up (the archive is a full snapshot), then delete the scratch copy.

Not in the hub backup (keep separately, protected): node credentials and node
state, account profiles/logins, `owner-credential.json`, the Android signing
key and its passwords, `google-services.json`, the FCM service-account key.

Rehearse: `scripts/backup/rehearse-restore.sh` (scratch directory, loopback
ports, synthetic data). It restores a passphrase archive
end to end and also restores age X25519 and post-quantum recipient archives
(decrypting them with the stock age CLI too), a gpg archive, and, with
`--legacy-hub <old pyre-hub>`, a legacy-format archive. Suggested schedule:
daily `backup-hub.sh --recipient age1pq1… --verify` to a directory synced
off the hub host, keeping 14 days. No timer unit is shipped yet.

## Android

Build, sign, sideload and update are in [android/README.md](../android/README.md).
Operationally:

- Create the release key once (`keytool ...` in that README), keep
  `~/.config/pyre/pyre-release.jks` and its password backed up outside Git and
  outside hub backups. Losing it means future updates require uninstalling the
  app (losing its data).
- `android/gradlew -p android assembleRelease` signs automatically when
  `~/.config/pyre/android-signing.properties` exists; verify with
  `apksigner verify --print-certs`. Publish the APK's sha256 with the release.
- Install/update: `adb install -r app-release.apk` (same key keeps pairing,
  cache and outbox). Bump `-PpyreVersionCode`/`-PpyreVersionName` per release.
  The release certificate SHA-256 is
  `13:16:54:7A:73:D0:2C:07:F0:E2:F3:75:EE:0F:E3:ED:F9:D2:A0:9D:A4:67:F2:48:E7:5D:E3:C6:51:1F:AA:A0`
  (`bash android/scripts/release-verify.sh` prints it for a fresh build).
- 0.2.0 (versionCode 2) installs over 0.1.0-rc1 (versionCode 1) with the same
  key. On first start it migrates the pairing from EncryptedSharedPreferences
  to an Android Keystore key and DataStore, then deletes the old file. This is
  verified on the JVM only, not yet on a phone; if the pairing is lost, pair
  again.
- Pair with `pyre pair-client --kind android`. Re-pair after a hub URL change.

## Signing keys and their backups

| Key | Files | Where |
| --- | --- | --- |
| Release key (signs `SHA256SUMS`) | `~/.config/pyre/release-signing` (+ `.pub`); public half in `packaging/release-allowed-signers` | build Mac; backups in `~/.config/pyre/` on server and laptop (0600, dir 0700) |
| Android key | `~/.config/pyre/pyre-release.jks` + `android-signing.properties` | build Mac; backups in `~/.config/pyre/` on server and laptop (0600) |
| Hub backup identity (age) | owner-created `hub-backup.key` | off the hub host |

None of these is in Git or in a hub backup. `uninstall.sh --purge` never
deletes `~/.config/pyre`. The release key fingerprint is
`SHA256:PkV+Jks4fzjePVhK4cvgH21P6KqHNI/UWt9d9SuHf18`. Each installed host pins
its public key in `~/.config/pyre/release-allowed-signers`. Keep one more
offline copy of the private keys, for example in a password manager. Losing
the Android key forces every phone to uninstall and pair again. Losing the
release key means a new key plus a deliberate re-pin on every host.

## Push notifications

Default builds have no Firebase dependency. Without push, alerts reach the
phone through the app's optional hub-stream mode (a foreground service holding
`/v1/events/stream`; exempt the app from battery optimization) and the Inbox.

Real background push (FCM) needs, none of it in Git:

1. A Firebase project owned by the user with Cloud Messaging enabled and
   Android apps `dev.jhh3.pyre` (and `dev.jhh3.pyre.debug`).
2. `android/app/google-services.json` from that project at build time; the
   build then has an `fcm` flavor (`assembleFcmRelease`).
3. A service-account JSON key for the same project on the hub host (0600).

Start the hub with `pyre-hub serve ... --fcm-service-account <key.json>` (a
broken key fails at startup). Without the flag the hub runs with FCM disabled
("FCM not configured on hub"). Push verification requires the phone
backgrounded with the screen off; a foreground connection is not evidence.

## Resource expectations

Measured on an Apple M2 Pro (macOS, arm64), loopback hub, one connected node,
mock runtime, integrations off, no runs (`scripts/install/measure-idle.sh`,
60 s window after a 10 s settle):

| Process | CPU (one core) | RSS | Disk |
| --- | --- | --- | --- |
| pyre-hub idle | 0.63 % | 25 MiB | 0.8 MiB data after init |
| pyred idle, connected | 0.10 % | 15–20 MiB | 0.24 MiB state |
| pyred right after start (rehearsals) | — | 27 MiB | — |

The WP18 resilience baseline (branch `wp18-resilience`, not yet merged) adds
node-link numbers: idle RSS 14–22 MiB, about 8 000 reports/s burst, p99
command latency 38–59 ms, 5 000 queued reports drained in under 0.4 s after a
hub restart. Real Claude on server (2026-10-05): pyred adds about 5 ms per Claude
hook invocation on Linux and about 17 ms on macOS, about 27 ms of CPU per
turn, and stays under 32 MiB; Hive bridge indexing costs about 0.45 s per note
(`tests/resilience/BUDGETS.md`). Not measured yet: Codex hooks, many
concurrent real sessions. Measure with
`measure-idle.sh` (any host), `pyred status` (pyred versus agent CPU/RSS per
node), and `systemd-cgtop --user` on Linux. Agent processes dominate memory
(hundreds of MiB per Claude/Codex session), which is why they live in
`pyre-runs.slice` with reclaim pressure (`MemoryHigh`) rather than a hard cap,
while pyred itself is capped. On server (2026-10-04) pyred.service held 12 MiB
(`memory.current`) with two shells and one Claude run; the runs slice held
255–320 MiB.

### Background work limits

Pyre's own background work is bounded and yields to interactive calls (CLI
and MCP reads, send, attach, approvals, TUI refresh); none of the limits
depends on how loaded the machine is. Interactive requests run on their own
goroutines and never wait in a background queue. `pyred status --json`
reports current queue depths (`diagnostics.queues`) and the limits
(`diagnostics.limits`).

| Background work | Limit (default) | When full |
| --- | --- | --- |
| Hive subprocesses for capture and the note bridge | one shared slot (`--hive-background-slots 1`), nice +10 | wait for the slot; interactive memory calls never take one |
| Hive transcript capture queue | 32 transcripts (`--hive-capture-queue`), coalesced per transcript, 1 s between captures | dropped; Hive's sweep or the next capture picks the transcript up |
| Hive note bridge | 20 notes and 10 pages per pass, passes at least 2 s apart, backoff to 5 min | notes stay `pending` on the hub |
| Node read cache writes | 256 values (`--read-cache-queue`), one writer | not cached; the read itself is unaffected |
| Local message outbox (hub unreachable) | 1000 messages, 100 per flush, serial | `capacity_exhausted` to the sender (retryable); never dropped |
| Hub-link report outbox | 100 000 reports, 256 in flight | `capacity_exhausted`; never dropped |
| Run observation, deliveries, node actions | 4 / 4 / 4 concurrent | wait (poll), capacity_exhausted (actions) |
| Hub event streams | 256 open streams; one 200-event batch each; 10 s per frame | capacity_exhausted; a stalled subscriber is disconnected and resumes from its cursor |
| Push notifications (hub) | serial, 6 burst then 1 per 10 s, 2 s debounce | deferred, not dropped |
| Update staging | one at a time, 1 GiB per download | wait |

## Audit log

`pyre audit [--since 24h] [--kind account|session|fleet] [--session S] [--node N] [--account A] [--json]`
(MCP `audit_log`, read-only and pre-approved; TUI Activity view `<leader>l` / `:activity`, `a` from
Accounts, which also lists RECENT SWITCHES; Android: history icon on Accounts) shows what Pyre did on
its own and what changed in the fleet, one plain-English line each, e.g.
`14:05 server · switched 'Credit perf' from code (weekly 96%) to me (5h 10%) — limit reached`.
Recorded: account chosen per run (with the selection reason, affinity included), automatic switches and
"no eligible account", re-auth needed / valid again, cooldown start/end, tokens added/replaced/removed/
synced to a machine, logins added on a machine, accounts added/folded/renamed/disabled/enabled; sessions
started (by owner or by session X), stopped, interrupted, resumed/handoff, renamed, auto-titled,
archived, input taken over; machines enrolled/revoked/connected/disconnected, pyred and hub upgrades,
failed updates, folder-trust prompts, Claude first-run screens and Claude exit dialogs a node
answered by itself.
Entries are written in the same transaction as the change (hub mutations, node reports, and an
event-log append hook), hold IDs, names and short reasons only (no secrets, message bodies or
transcripts) and live in the hub table `audit_log` (namespace `hub_audit`). Retention: 90 days or
100k entries, whichever is smaller; `pyre-hub serve --audit-max-age 720h --audit-max-rows 20000`.

## Owner approvals for agent requests

Some operations stay owner-only: archiving a session the agent did not start
(or an owner-started session archiving itself), `pyre revoke node`, and
`pyre enroll-node`. An agent that runs one is not refused and does not hand
the owner a command to paste: the hub files the exact request as an ask
("Archive session …?", "Revoke machine …?", "Create an enrollment ticket for
…?") with Approve / Deny in the owner's inbox (web, TUI, Android, push), and the
agent sees `needs the owner's approval: filed as ask_…` (CLI exit 8, MCP
`archive_session` result with `approval_ask`). Repeats merge into the open
ask. Approve runs the request with the owner's authority (operation ID
`op_owneraction-<ask>`, so it runs once), logs it in the audit log as the
owner with "approved request from <session>", and messages the requesting
session the outcome; Deny changes nothing and tells the session, and an
unanswered request expires after 7 days. For an enrollment the agent runs the
same `pyre enroll-node` command again after approval to receive the one-time
secret, which never travels in a message. Pending and finished requests are
in the hub table `owner_actions`.

## Issues reported by sessions

Sessions report what goes wrong with Pyre itself (`pyre_bug`: a tool errors, a
session, report or state is wrong or stuck, a message is lost), with a machine
(`machine`: a missing tool, an expired login, a full disk, wrong config) or with
Pyre's own instructions (`guidance`), using MCP `report_issue` (pre-approved) or
`pyre issue file --kind K --title T [--body TEXT|- | --body-file F|-]
[--related ID]... [--node N]`. The agent guide tells them to do that and keep
working instead of messaging the owner. The hub stores each report with the
reporter (session, run, node, harness and version, the node's pyred version,
project), an automatic context block (the run's state and reason, cwd, and the
last 10 lines of that node's `pyred.log` naming the run, found via
`$PYRE_RUN_DIR`) and the evidence. Title, body, context, related IDs, notes and
links pass the shared credential redaction patterns (provider tokens such as
`ghp_`/`github_pat_`, `sk-ant-`/`sk-proj-`, Slack `xox?-`, AWS `AKIA`/`ASIA`,
JWTs and Bearer headers; `NAME_TOKEN=`/`*_API_KEY=` assignments; URL
`user:password@`; PEM private key blocks). A `--node` must be a known machine. Tables: `issues`, `issue_occurrences`, `issue_notes` (migration
namespace `hub_issues`).

Repeats fold into one issue: the fingerprint is the kind plus the title with IDs,
numbers, paths, URLs and timestamps removed (plus the node for machine issues);
a title that leaves fewer than 4 letters is matched on its whole lowercased text.
After 50 stored reports an issue only counts repeats (no new evidence rows),
except a report that reopens a fixed issue.
A repeat adds an occurrence with its own evidence; a repeat of a fixed issue
reopens it ("reported again after fix"); a wontfix issue stays closed; a
duplicate forwards to the issue it duplicates.

Triage: `pyre issue triage set <session>|self|off` (the owner, or the current
triage session handing the role on; `pyre issue triage show`). The triage
session gets one Pyre message per new issue (`[pyre issue] <kind>: <title> —
iss_… from <session> on <node> (pyre vX)`), one when a fixed issue is reported
again, and one when an open issue reaches 3, 10 and 50 reports. The title is
quoted and labeled as the reporting session's words; the body is not included.
A session's new issues past 10 in an hour are stored without a message, and the
triage session is told once that it is filing a lot. Nothing goes to
the owner's inbox. Without a triage session (or when it is archived) issues are
only stored. A triage session works the queue periodically:

- `pyre issue ls [--status open,triaged|all] [--kind K] [--node N]`, `pyre issue show <iss_…>`
  (MCP `list_issues`, `get_issue`; the owner and every session may read);
- correct the guidance or fix the bug, then
  `pyre issue close <iss_…> --status fixed|wontfix|duplicate --note "…" [--link PR] [--of iss_…]`;
- `pyre issue note <iss_…> "…" [--link URL] [--status triaged|open]`
  (MCP `update_issue`; only the owner and the triage session update issues).

Triage messages go through the `issue_notices` outbox in the same transaction
as the issue: sent right after commit, retried by hub maintenance (idempotent by
operation ID), dropped after 20 attempts or when the triage session is gone.

## Logs and diagnostics

- Node: `pyred status [--json]`; `<state>/logs/pyred.log`; macOS
  `~/Library/Logs/pyre/pyred.err.log`; Linux `journalctl --user -u pyred`.
- Hub: macOS `~/Library/Logs/pyre/pyre-hub.err.log`; Linux
  `journalctl --user -u pyre-hub`; or `pyre-hub serve --log-file FILE`
  (size-rotated, default 10 MiB x 5, 0600). The access log never contains
  tokens. `pyre-hub version --json` reports version, protocol range and
  platform.
- Service state: `launchctl print gui/$UID/dev.jhh3.pyre.pyred`;
  `systemctl --user status pyred pyre-hub`.
- Fleet: `pyre nodes`, `pyre sessions`, `pyre attention`, `pyre hello`.
- Clutter: ended sessions (exited, failed, no run) stay in `pyre sessions`
  until archived. `pyre sessions prune [--older-than 24h] [--node N]
  [--dry-run]` archives them, `pyre sessions archive <session>...` archives
  chosen ones (a live session must be stopped first, or `--force`),
  `pyre sessions unarchive <session>` restores one, and `pyre sessions ls
  --ended` / `--all` list only ended / also archived sessions. Archiving
  stops nothing and revokes the session's agent tokens. The TUI hides ended
  sessions by default (`.` shows them, `X` archives, `:archived` + `u`
  restores).
- Names: a session started without a title is named automatically (tag
  `title:auto`): its node reads the run's transcript while it is idle and
  reports Claude Code's own conversation title (`ai-title`, or
  `custom-title` after `/rename` inside Claude), Codex's thread name
  (`session_index.jsonl` / `thread_name_updated`), else a short title from
  the first user prompt (first sentence, at most six words, Pyre markers
  removed). It updates after the first turn and again when Claude's title
  appears. Shells keep the cwd name. Rename with `pyre sessions rename
  <session> <title...>`, `R` / `:rename <title>` / clicking the pane title
  in the TUI, or the session overflow menu on Android; a person's rename
  always wins and stops auto-naming. Agents may name their own session with
  the pre-approved MCP tool `rename_session` (tag `title:agent`); the hub
  refuses other sessions and titles the owner set.
- Catching up: `pyre digest [--since last|2h|1d] [--json]` summarizes what
  happened since this client last looked — one line per agent, needs you
  first, then finished work, then still working — with what changed (files,
  documents, artifacts, messages) and a `pyre://` deep link to the exact
  transcript moment or object. It is computed client-side from existing hub
  reads (no hub operation) and deterministically (first sentence of the
  agent's last message, counts from the transcript); no model runs unless
  `--summarize` asks a cheap one through your own harness (default `claude
  -p --model haiku`, override with `PYRE_DIGEST_SUMMARIZE_CMD`, prompt on
  stdin). The last-seen mark lives in `digest-state.json` beside the client
  config (12 h on first use); `--since last` moves it, an explicit `--since`
  or `--no-mark` does not. The TUI shows the same digest as its own
  view (`␣w`, `:digest`; esc marks it seen), agents get the
  read-only MCP tool `get_digest` (pre-approved), and Android shows a
  digest card at the top of Sessions with its own per-app mark.
- Exit codes of the CLI: `pyre exit-codes`.

## Sentinel

`pyred sentinel run` is a watchdog that does not depend on the hub. It
exists because of the Oct 20, 2026 outage: server's systemd user manager was
killed at 12:34 AM PT, taking pyre-hub, pyred, tmux and every tunnel with it,
and nothing noticed for 6.5 hours because every alert path ran through the
hub on server. Sentinels run on always-on machines (devbox, office-box2,
laptop, server, office-box1) as their own service,
separate from pyred. Code: `internal/sentinel`. The owner chose Slack DM alerts at
any hour, ssh-login recovery, and no external dead-man service.

**Every 60 s** each sentinel:

1. Checks its own network: a TCP dial to `1.1.1.1:443` / `8.8.8.8:53`, or
   else to its default gateway (an answer or a refusal both count). If that
   fails it votes `unknown` on everything and does not lead.
2. Probes every target except itself:
   - the hub's unauthenticated `GET /healthz` on each configured address
     (server: LAN and Tailscale), with the hub certificate pinned by
     `hub_pin`. Either address passing means the hub is up;
   - one ssh session (`BatchMode=yes`, `ConnectTimeout=10`, `/bin/sh -s`)
     that prints `systemctl --user is-system-running`, the manager's start
     time, `systemctl --user is-active` per configured unit (macOS:
     `launchctl list LABEL`), `nc -z` (or bash `/dev/tcp`) per configured
     port, the host's `~/.local/state/pyre/maintenance.json` and, when the
     host runs a sentinel, its `~/.local/state/pyre/sentinel/state.json`.
     Every `systemctl` call is wrapped in `timeout 5`: a frozen manager or
     D-Bus reads as "not answering" (DEGRADED), and a session that printed
     its first line but never finished is "ssh connects, probe hung", not
     "host off". The same session also prints the host's clock, and writes
     this sentinel's last state to the peer's
     `~/.local/state/pyre/sentinel/peers/<self>.json` (only when that
     directory exists), so a peer that can reach us but not the other way
     round is still heard. Nothing else on the target is changed;
   - public HTTPS endpoints: any response below 500 without an
     `ngrok-error-code` header counts as up (an offline ngrok endpoint
     answers 404 `ERR_NGROK_3200`).
3. Votes per target: `down` (every hub address fails; or ssh works and the
   user manager is not running; or ssh cannot connect and no hub address
   answers), `degraded` (a unit not active, a port closed, a public URL
   failing, the user manager or the probe not answering, or ssh failing
   while the hub answers), `up`, or `unknown` (own network failed; ssh
   refused this sentinel's key and there is no hub to check; or the
   target's `via` host is DOWN or failed ssh this round). When the target's
   own sentinel is reporting (fresh state, pulled or pushed) a failed ssh
   from here is a path problem: DEGRADED at most, never DOWN.
4. Reads the peers' states (the fresher of the one pulled over ssh and the
   one the peer pushed). Age is measured on one clock: the peer's clock at
   read time minus its stamp for a pulled state, the file's mtime on this
   machine for a pushed one; a stamp in the future is ignored. A peer whose
   state is older than 3 minutes, or whose own network check failed, is not
   live. Each state lists the sentinels its writer sees live, with their
   age and delivery status; a sentinel a live peer sees counts as live for
   choosing the leader even when this machine has no path to it (one hop),
   until the combined age passes 3 minutes, and a gossiped `notify_err`
   skips it as actor like a direct one.
5. Evaluates each target from the live votes, never counting the target's
   own sentinel: **DOWN** needs at least 2 down votes held for **3 minutes**;
   with only one live voter, its down vote is **DOWN (unconfirmed)** (same
   3 minutes). **DEGRADED** needs min(2, live voters) degraded-or-down votes,
   or the target's own sentinel not reporting, held for **10 minutes**.
   "Held" has hysteresis: a condition clears only after 2 minutes without
   it, so a crash-looping service (up 20 s of every 30) or packet loss still
   confirms. After a gap longer than 3 minutes with no evaluation (the
   sentinel stopped, or everyone abstained) the timers start over.
   When every live vote on a target is `unknown` nothing changes: grace
   timers keep running and an open incident stays open (no false
   "recovered").

   **Partitions are not DOWN.** A watcher that cannot reach a target at all
   (ssh does not connect and no hub address answers) does not vote on the
   host when another watcher reaches it, or when, with `sites` configured,
   every failing watcher is at one site other than the target's and the
   target's own sentinel is reporting. The evidence must be at least as new
   as the latest failure began, so at the start of a real outage a
   watcher's last good probe from before it does not count. The target is
   then judged by the watchers that reach it, and the failing path becomes
   one **link incident** (below). When every watcher fails, the same-site
   ones included, nothing changes: DOWN as before. Oct 22: the home uplink
   was congested (1–3 s RTT) and the office NUCs timed out on server while
   devbox, laptop and server itself were healthy; that was "server is
   DOWN", a reminder and "DOWN again, flapping", and is now one link
   message and one recovery.

   **Public-URL-only problems** (every bad vote is DEGRADED only because a
   public URL fails; the host, hub, units and ports are fine) need
   `public_grace` (15 minutes) instead of 10, and the target's own check of
   the URL must fail too: a sentinel probes its own host's `public`
   endpoints each round (`self_public` in its state), and when the target's
   fresh state shows those URLs working, it is the watchers' path to the
   edge (an ngrok EOF seen from the office), not an alert. A target that
   publishes no self-check (no sentinel, or an older version) gets the
   15-minute grace alone.
6. The **leader** is the first live sentinel in the configured rank order
   (devbox, office-box2, laptop, office-box1, server). It sends the
   messages and runs recovery for every target except its own host, which
   the next live sentinel handles. A sentinel whose last bot DM or token
   check (`auth.test`, every 10 minutes) failed publishes `notify_err` and
   is skipped while another live one can deliver. Every other sentinel mirrors the acting
   sentinel's incidents into its own state, and a new actor merges what
   live peers recorded (and drops incidents a peer closed), so a leader
   change repeats neither an alert nor a recovery attempt. A sentinel
   re-stamps its state at the start of each round and stamps it again when
   written, so a slow round does not make it look missing.

**Incidents** (one per target, ID `<target>-<UTC start>`): one alert when
confirmed (and one more if it escalates, e.g. DEGRADED to DOWN), reminders
30 minutes and 2 hours after the first alert, and one "recovered" message
once the target has been clear for 2 minutes. A target that fails again
within 30 minutes of "recovered" reopens the same incident (one "failing
again, flapping" message) and its next "recovered" waits for 30 minutes
of stability. Reminders and "needs manual help" go out only while the
current round is still bad (the confirmed level lags 2 minutes behind a
recovery). Every message is
self-contained, in Pacific time, e.g.

```text
Pyre: server is DOWN since 12:35 AM PT (confirmed by devbox, office-box2). Hub unreachable on LAN and Tailscale; ssh works, user manager not running (is-system-running: offline). Auto-recovery: ssh login (attempt 1 of 2) at 12:38 AM PT: user manager running.
```

**Link incidents** (ID `link-<key>-<UTC start>`, key the sorted site pair
such as `home~office`, or `network` without `sites`): the partitions of all
targets over one link are one incident, sent by the first live sentinel
that can deliver and mirrored like target incidents. It needs at least two
failing watchers and 10 minutes (`degraded_grace`; gaps under 5 minutes do
not reset it), then sends one message, e.g. `Pyre: The office ↔ home link
looks down since 6:14 AM PT: office-box1, office-box2 can't reach laptop
and server; laptop, server and devbox are fine.`, and one "link recovered"
once no partition has been seen for 30 minutes. No reminders, no flapping
notices; a target in a maintenance window is left out.

A change of a target's user-manager start time outside an incident and
outside maintenance sends one "user manager restarted" message. This is the
signal for an outage like Oct 20's: a sentinel's own ssh probe opens a PAM
session, and pam_systemd starts `user@UID.service` if it is missing, so a
killed manager is usually back within one probe, before DOWN can be
confirmed. Expect "restarted" rather than "DOWN" for that case. At most one
such message per target per hour (the next one counts the restarts in
between): with linger off, every probe login would restart it.

**Recovery** (acting sentinel only, after DOWN is confirmed, outside maintenance):
when the acting sentinel's own probe connects over ssh but the user manager is not
running, it opens one ssh session that runs `systemctl --user start
default.target` (the login itself starts `user@UID.service`). At most 2
attempts per incident, 4 minutes apart, each recorded in state.json before
it runs; then one "needs manual help" message. Never sudo, never a kill or
stop. `"no_recovery": true` on a target disables it.

**Maintenance windows.** `pyred maintenance 30m [--reason X]` (or
`--for 30m`) writes `~/.local/state/pyre/maintenance.json` on the machine
being worked on (`clear` / `--clear` ends it, no arguments shows it).
`pyred maintenance server 15m` sets it on another sentinel target from a
sentinel machine, over the sentinel config's ssh aliases and identity
(`pyred maintenance server clear` ends it). Sentinels read it over ssh and cache
it, so a window survives the host disappearing (a reboot). While a window is
active the leader sends nothing and recovers nothing for that host or its
public URLs, and grace periods count from the window's end: a problem that
began inside it alerts only if it is still there a full grace (3 minutes
DOWN, 10 DEGRADED, 15 public URL only) after the window ends or is cleared.
`scripts/install/upgrade.sh`
sets a 10-minute window right before it switches versions and restarts
services (not for a refused or dry-run upgrade; best effort, never shortens
a longer window), clears that window once the upgrade (or rollback) is
healthy (a window someone else set or lengthened is left alone), and
afterwards restarts `pyre-sentinel` if it is
installed so the sentinel runs the new version (never with
`PYRE_UPGRADE_RESTART_HOOK`, `--root`, `--home` or a foreign `--os`, so
rehearsals and tests leave the real one alone). `install.ps1` does not: Windows machines are not
sentinel targets.

**Alerts: Slack DM.** The transport is a bot DM: `conversations.open` with
`slack.user_id`, then `chat.postMessage`, using the bot token in
`slack.bot_token_file` (0600; `PYRE_SENTINEL_SLACK_TOKEN` overrides it). A bot
DM notifies; a message the owner's own account sends to his self-DM does
not, which rules out agent-slack (it runs on the owner's browser session) as
the primary path. The bot needs `chat:write` and `im:write` (an existing
workspace bot with both scopes works). Only a bot DM counts as delivered. When
the bot fails, `slack.agent_slack` (default off; the template sets
`agent-slack`) records each message once per sentinel (keyed by incident
and kind, not by its changing text) as the owner (a written record, it
does not notify), the alert stays unsent, the bot is retried every round,
and the sentinel reports `notify_err` so another one takes over. A post
that timed out after sending is retried (a possible duplicate, never a
silent miss) and is not copied to agent-slack. `pyred sentinel run
--dry-run` checks the token with `auth.test` (and the `chat:write` /
`im:write` scopes) without sending, and fails when it cannot deliver;
`scripts/install/sentinel.sh install` runs it first.

### Configuration

`~/.config/pyre/sentinel.json`, mode 0600 (refused otherwise). Start from
[packaging/sentinel/sentinel.example.json](../packaging/sentinel/sentinel.example.json).

| Field | Meaning |
|---|---|
| `self` | This machine's sentinel name (must be in `sentinels`). |
| `sentinels` | Rank order (first = leader). `ssh`: aliases tried in order. `via`: the target this machine's only path to that sentinel runs through (ProxyJump); while it is DOWN, that sentinel's silence is expected. Each sentinel is also watched as a target (ssh, user manager, its sentinel reporting) unless listed in `targets`. |
| `targets[]` | `name`, `ssh` aliases, `hub` (`url`, `label`), `hub_pin` (hub cert SHA-256, validated; without it `/healthz` skips verification), `units`, `tcp` (`host:port`, checked on the target), `public` (`url`, `label`), `via`, `no_recovery`. Unit names, ports and names must be plain words (they go into the remote script). |
| `ssh_identity` | Private key passed as `ssh -i` (a service has no ssh agent). |
| `slack` | `user_id` (U…), `bot_token_file`, `agent_slack`. |
| `network` | `public` addresses for the self-check, `no_gateway`. |
| `sites` | Optional `{"home": ["server", "devbox", "laptop"], "office": ["office-box1", "office-box2"]}`: names a link incident (`office ↔ home`) and lets a target's own sentinel count as evidence when only watchers at another site fail. A host is in at most one site. Older sentinels refuse a config with `sites`: upgrade first. |
| `timing` | `interval` 60s, `down_grace` 3m, `degraded_grace` 10m, `public_grace` 15m, `stale` 3m, `recover_grace` 2m, `recovery_retry` 4m, `reminders` [30m, 2h], `max_recovery_attempts` 2. |
| `state_dir` | Default `~/.local/state/pyre/sentinel` on every platform (peers read it at that path). |

Commands: `pyred sentinel run [--once] [--dry-run] [--json]` (`--dry-run`:
one read-only round that prints the votes and what would be sent; no
message, no recovery, no state written, nothing pushed), `pyred sentinel
status [--json]`, `pyred sentinel test-alert [--text T]`.

### Network paths (an example fleet)

| From \ to | server | devbox | laptop | office-box2 |
|---|---|---|---|---|
| devbox | LAN (hub LAN only; no Tailscale) | — | LAN | none direct: ProxyJump server (`via: server`) |
| laptop | LAN (hub LAN only) | LAN | — | none direct: ProxyJump server (`via: server`) |
| office-box2 | Tailscale only (hub Tailscale only) | `home.example.com:<port A>` | `home.example.com:<port B>` | — |
| server | — | LAN | LAN | Tailscale |

Here `home.example.com` is a hostname for the home's public address, and
the two ports are forwards on the home router to devbox's and laptop's ssh,
independent of server.

So with server down, devbox and laptop still confirm DOWN between them
and office-box2 still hears devbox (and defers to it) through the port
forwards; devbox hears office-box2 through what office-box2 pushes.
Installing Tailscale on devbox and laptop would remove the
ProxyJump dependency.

Check ssh trust between the sentinel hosts before installing: a path that
fails host-key verification or name resolution leaves that sentinel blind
to the target.

### Install (per machine; needs a Pyre release containing the sentinel)

Run as the owning user. Nothing here touches pyred, the hub or a run.

1. Upgrade Pyre on the machine (`scripts/install/upgrade.sh`); check
   `pyred sentinel` prints its usage.
2. ssh key for the sentinel:
   `ssh-keygen -t ed25519 -N '' -C "pyre-sentinel@$(hostname -s)" -f ~/.ssh/pyre_sentinel_ed25519`.
   Append its `.pub` to `~/.ssh/authorized_keys` on every target this
   sentinel watches, prefixed `restrict ` (no forwarding, no pty; commands
   and the PAM session still work). On a jump host (server above), for a
   sentinel that jumps through it to another target, use
   `restrict,port-forwarding,permitopen="<target address>:22" ` instead.
3. ssh aliases in `~/.ssh/config` (`User` your login on the targets), then
   accept each host key once: `ssh -i ~/.ssh/pyre_sentinel_ed25519 <alias> true`
   (check the fingerprint). One alias per target as the sentinel reaches it:
   `HostName` its LAN or Tailscale address; through a port forward,
   `HostName home.example.com`, `Port <port>` and `HostKeyAlias <target>` (so
   the key is checked as the target's); through a jump host, `ProxyJump
   <jump host>`.
   - A ProxyJump hop is a separate ssh that does not get the sentinel's
     `-i`, `BatchMode` or `ConnectTimeout`. Set them for the jump host in
     `~/.ssh/config` on the machines that jump through it:
     `Host server` / `IdentityFile ~/.ssh/pyre_sentinel_ed25519` /
     `BatchMode yes` / `ConnectTimeout 10` (plus its `HostName`).
4. Slack token: `install -m 600 /dev/null ~/.config/pyre/sentinel-slack-token`,
   then write the bot token into it, e.g. from your secret manager:
   `<print the bot token> | ssh <host> 'umask 077; cat > ~/.config/pyre/sentinel-slack-token'`.
5. Config: copy the example to `~/.config/pyre/sentinel.json`, `chmod 600`,
   set `self`, the ssh aliases this machine uses, and `slack.user_id`. On server
   keep the server target (it is skipped as self, but documents the checks).
6. `pyred sentinel run --dry-run` (read-only round: every target `up`; peers
   not yet installed show as missing; `slack ok`, verified with
   `auth.test`, nothing sent). `pyred sentinel test-alert` sends a real DM;
   use it only if delivery is in doubt.
7. `scripts/install/sentinel.sh install` (Linux: `pyre-sentinel.service`,
   `Restart=always`; macOS: `dev.jhh3.pyre.sentinel`, `KeepAlive`). Then
   `pyred sentinel status` a minute later: `leader devbox`, every
   installed sentinel live.

Install all sentinels within 10 minutes of each other, or each missing one
is reported DEGRADED. Remove: `scripts/install/sentinel.sh uninstall`
(config and state kept). Logs: `journalctl --user -u pyre-sentinel`,
macOS `~/Library/Logs/pyre/sentinel.err.log`.

### Limitations

- If every sentinel is down, or the leader's Slack path is down, nothing
  alerts. There is no external dead-man heartbeat (declined). server's own
  sentinel dies with server's user manager.
- A partition can elect two leaders (each sees the other as missing, also
  second-hand) and send duplicate messages, and in that case each may run
  its own recovery attempts; the incident merge only prevents repeats among
  sentinels that can read each other.
- A sentinel host that dies outright is DOWN only after its last state goes
  stale (3 minutes) plus quorum and grace: about 7 minutes.
- A Slack post that times out after sending is retried on the next round
  (a possible duplicate, never a silent miss); it does not fall back to
  agent-slack.
- The "user manager restarted" signal relies on
  `systemctl --user show --timestamp=unix -p UserspaceTimestamp` (systemd
  248+). The recovery and the probe-login effect were not exercised against
  a real dead manager (no deliberate kill was authorized); they follow
  pam_systemd(8).
- Alerts carry no session counts (the hub may be down when they are sent).

## Troubleshooting

**Stale or disconnected node.** `pyre nodes` shows `disconnected` with
`last_seen`; a disconnected node's runs show as unknown, never idle, and are
never reassigned. On the node: is the service running (`launchctl print`,
`systemctl --user status pyred`)? Does `pyred status` show `hub link
disconnected`? Check the hub URL from the node (`curl -sk
https://<hub>/healthz`), the ssh forward if one is used, and the pyred log for
TLS pin or auth errors. A revoked node logs 401s and stays disconnected; it
needs a new enrollment. On Linux, a node that dies at logout lacks
`loginctl enable-linger`.

**Indeterminate deliveries.** A message whose runtime outcome is unknown (the
connection dropped after submission) is never replayed automatically.
`pyre msg show <id>` shows stored/runtime/acknowledged facts and evidence.
Inspect the session (`pyre read <session>`), then record the outcome with
`pyre resolve-delivery <id> accepted|rejected --note "<what you checked>"`,
or resend deliberately with `pyre reissue <id> --accept-duplicate-risk`. Later
evidence settles it on its own: if the hook, the transcript or the agent's
inbox tool shows the message, the delivery becomes `accepted`, and
`resolve-delivery` then answers `conflict: delivery is accepted, not
indeterminate`.

Long messages into Claude (fixed after rc4): Claude shows only the last six
rows of a long draft, and a run pane keeps the size of the last terminal
attached to it, so in rc4 a message of a few hundred characters pasted into a
narrow pane scrolled the marker (`[pyre-msg:<id>] from …`) out of view, went
`indeterminate` and left the text unsent. Pyre now restores the run window to
its launch size (200×50) before pasting when no terminal is attached, and
recognises a paste whose marker is out of view when the input box shows
exactly the end of the message. It never presses Enter on text it cannot
recognise as its own, nor while a human is attached. When it cannot tell, the
delivery stays `indeterminate` with "Needs a human: …" in its reason, and the
session state says `needs human: Claude's input box holds Pyre's unsent draft
of <id>` while the draft is there. Attach, then press Enter to send it (the
prompt hook then settles the delivery as `accepted`) or Ctrl-C to clear it and
`pyre resolve-delivery <id> rejected --note "draft cleared"`. Never resend it.

**Dead or expiring grant.** `pyre accounts` marks the grant `dead` or
`expiring`; launches on that account fail fast instead of retrying. Log in
again inside that profile on that node (the profile script above). Do not copy
a working login from another profile or machine.

**Protocol mismatch.** `pyre hello` exits non-zero with "hub speaks protocol
N (min M); this pyre speaks ...: upgrade the older side"; a node on an
incompatible protocol is refused at connect and logs the protocol error.
Upgrade the hub first when the protocol rises, or roll the node back with
`upgrade.sh --rollback`. `upgrade.sh` refuses such a switch up front (exit 4).
The hub checks the protocol before it redeems a pairing or enrollment code
(`/v1/pair/exchange`, `/v1/enroll/exchange`), so an incompatible client or
node is refused without using the code. The Android app says which protocols
the hub supports, keeps its pairing, cached data and queued actions, and
resumes (stream, outbox replay) once the hub or app is updated or rolled back
([android/README.md](../android/README.md#protocol-mismatch)).

**Upgrade exit 6 (rollback unhealthy).** `current` points at the previous
version. Read the pyred log and service log; run the daemon in the foreground
(`~/.local/share/pyre/current/bin/pyred run --state <state>`) to see the
error; a broken state directory or credential is the usual cause. Runs are
still in their tmux servers and are recovered when a daemon starts.

**Service does not start.** launchd: `launchctl print
gui/$UID/dev.jhh3.pyre.pyred` (last exit status), `pyred.err.log`; KeepAlive
retries every 10 s. systemd: `journalctl --user -u pyred -n 50`; after 10
failures in 5 minutes systemd stops retrying (`systemctl --user reset-failed
pyred`). Hub sandbox failure (`226/NAMESPACE`): reinstall with `--no-harden`.

**Launch fails with "no systemd scope for the run server".** The installed
unit requires a scope (`PYRE_SYSTEMD_SCOPE=require`) and pyred could not
create one; the reason follows in the message (`systemd-run` missing, user bus
unreachable). Check `systemd-run --user --scope -- true` as the same user;
the bus is `$XDG_RUNTIME_DIR/bus` (package `dbus-user-session` on Debian/
Ubuntu). A failed probe is retried after 30 s. Fallback: reinstall with
`--no-scope`. To see where runs live: `cat <state>/tmux/placement`,
`systemd-cgls --user-unit pyre-runs.slice`, `systemctl --user status
pyre-runs.slice`. Stopping the slice (`systemctl --user stop pyre-runs.slice`)
stops every run on the node.

**Harness missing in the service.** `pyre nodes show` lists a harness as
unavailable although it works in your shell: the service PATH lacks it.
Re-run `install.sh --service node` from a shell where `command -v claude`
works, or pass `--path`.

**Hub restored from backup.** Expect the `node report gap` warning once per
node, followed by the node's `sent run snapshot` log line; data written after
the backup is gone except the run/session state the snapshot reconciles. Do not run the old and the
restored hub at the same time.

## State ownership and migration

- The hub owns the shared directory, project documents and versions,
  artifacts, message routing records and enrollment; each node owns its run
  records, outbox, runtime endpoints and native tool state.
- Hive keeps its vault and index authority; Outfit/Rota and the native stores
  keep credential custody. Pyre stores references, never secrets.
- Installing Pyre changes nothing in Hive, Outfit, Rota, `~/.claude`,
  `~/.codex`, the default tmux server or existing hooks. Adoption of existing
  configuration goes through `pyre env plan|apply|drift|rollback`, which
  preserves unmanaged content.
- Migrate one authority at a time: run Pyre-managed sessions beside existing
  ones, compare behaviour, keep rollback data, and only then disable a
  duplicate hook or service. Preserve dirty working trees (a deployed Outfit
  checkout may have local edits); never force-reset a checkout to install.
- Do not publish host inventories, ssh configuration, tokens, account emails
  or transcripts in this repository.

## Live rollout plan (proposal, not executed)

Targets: **server** (Linux, always-on) as hub and node; **this Mac** as owner
client and node; **laptop** (macOS) as node. Every step marked
**[authorize]** changes a real machine and needs the owner's explicit go.
Each phase ends with a check and has its own rollback. Nothing below stops an
existing tmux session, replaces a login, or edits Hive/Outfit/Rota.

**Phase 0: read-only inventory (no changes).** On each host:
`uname -sm`, `command -v tmux claude codex hive outfit rota`, `tmux -V`,
`ls -la ~/.local/bin ~/.local/share/pyre 2>/dev/null`, free disk, on server
`loginctl show-user $USER -p Linger` and the non-interactive PATH
(`ssh server 'echo $PATH; command -v claude codex tmux'`). Record a sanitized
summary. Go/no-go: tmux present; no name conflicts for `pyre`, `pyred`,
`pyre-hub`.

**Phase 1: release.** Tag a commit; `packaging/release.sh --version <v>`;
keep `dist/release/` and its `SHA256SUMS`. No host changes.

**Phase 2: hub on server.**
1. [authorize] Copy the linux-amd64 tarball and `SHA256SUMS` to server; run
   `install.sh --tarball ...` (binaries and shims only).
2. [authorize] `pyre-hub init --data ~/.local/share/pyre-hub --hub-url
   https://<server-name>:7443 --tls-host <server-name> --tls-host 127.0.0.1`.
3. [authorize] `install.sh --tarball ... --service hub --hub-url <url>
   --hub-listen <twingate-or-127.0.0.1>:7443`.
4. [authorize] `loginctl enable-linger $USER` (changes the user-manager
   lifetime policy on server).
5. Check from this Mac: `curl -sk https://<url>/healthz`, then client login
   (Phase 3). Take the first backup and restore it in a scratch directory on
   the Mac with `restore-hub.sh --check-listen`.
Rollback: `uninstall.sh` on server (keeps hub data; `--purge --yes` removes it);
disable lingering if it was off before.

**Phase 3: this Mac as client and node.**
1. [authorize] Copy `owner-credential.json` from server over ssh, `pyre login
   ... --token -`, delete the copy; `pyre hello`.
2. [authorize] `install.sh --tarball <darwin-arm64> --service node --name mac`
   after `pyre enroll-node --name mac` + `pyred enroll` (the LaunchAgent loads
   into the current GUI session).
3. Check: `pyre nodes` shows `mac` connected; start a shell session in a
   scratch directory (`pyre start --node mac --harness shell --cwd <scratch>`),
   attach, detach, `upgrade.sh` to the same version (no-op), restart the agent
   with `launchctl kickstart -k` and confirm the run survives.
Rollback: `uninstall.sh` on the Mac; `pyre revoke node <id>`; the client
config can stay or be purged.

**Phase 4: server as node.**
1. [authorize] `pyre enroll-node --name server --ssh server`, `pyred enroll` on server,
   then `install.sh --service node --name server --ssh-alias server` from an
   interactive server shell so the service PATH includes the user-local tools.
   Existing tmux sessions on server are untouched (Pyre uses private sockets); the
   workstream web service is unaffected. The unit and `pyre-runs.slice` are the
   ones validated with transient units on server; this
   step makes them persistent.
2. Check the installer's PATH warnings; `pyre nodes show server` must list the
   harnesses as available.
3. Check: `cat ~/.local/state/pyre/tmux/placement` names a
   `pyre-runs.slice/pyre-tmux-*.scope` cgroup; `systemctl --user restart pyred`
   with a shell run open leaves the run alive.
Rollback: `uninstall.sh` on server (node only; the hub keeps running).

**Phase 5: laptop as node.**
1. [authorize] `bootstrap-node.sh --ssh laptop --probe --hub-url <url>
   --execute`, then copy the darwin tarball and run `install.sh --service node
   --name laptop --ssh-alias laptop` on laptop. Confirm laptop has a
   logged-in session (or auto-login); otherwise the LaunchAgent does not run.
Rollback: `uninstall.sh` on laptop; `pyre revoke node <id>`.

**Phase 6: Android.** [authorize] Sideload the signed APK; `pyre pair-client
--kind android`; verify read/reply and hub-stream alerts with the screen off.
FCM only after the owner provisions Firebase and the hub runs with
`--fcm-service-account`.
Rollback: unpair in the app, `pyre revoke client <id>`, uninstall the APK.

**Phase 7: accounts.** [authorize, interactive] Fresh logins per account per
node with the profile script; verify identity isolation; no copied grants.
Rollback: `real-profile-check.sh remove <account>` (logs out of the profile
only).

**Phase 8: steady state.** Daily encrypted hub backups off server; upgrades with
`upgrade.sh` on one node first, then the rest; the hub first when the
protocol changes.

Fleet rollback at any point: `uninstall.sh` on each node and the hub (data
kept), revoke nodes and clients. Hive, Outfit, Rota, native logins and the
user's tmux sessions are unaffected because the install never modified them.

## Release checklist

- [acceptance.md](acceptance.md) cases at their required level, with real
  Mac/Linux and Android evidence attached; unrun cases marked unrun.
- `packaging/release.sh` from a tagged clean tree; publish `SHA256SUMS`, the
  APK sha256 and its signing-certificate digest.
- Install/upgrade/rollback/uninstall and backup/restore rehearsals re-run on
  the release build.
- Document unsupported native capabilities and the open items above (FCM hub
  push on real hardware).
- Runbook entries exist for service restart, account re-authentication,
  update rollback and Android re-pairing (this document).

## macOS privacy permissions (Pyre.app)

On a Mac, pyred runs from `~/.local/share/pyre/Pyre.app`. It is a background-only app (no Dock icon), signed with the Developer ID that built the release; see decision I31. macOS sees one app, **Pyre**, whatever the version. Grant permissions to it once:

- **System Settings → Privacy & Security → Full Disk Access**: click **+**, press ⌘⇧G and paste `~/.local/share/pyre/Pyre.app`. This covers external drives, Documents and Desktop, and other apps' data, which agents' tools (du, git, builds) touch.
- If you grant permissions through the prompts instead, approve the prompts that name **Pyre**.

Upgrades and rollbacks replace the bundle's contents in place and keep the same signed identity, so the grants stay. Entries left over from releases before I31, which show as `pyred` in the permission lists, can be removed.

Releases built without a signing identity print a warning and stay ad-hoc signed. Those builds lose the grants again.

The sentinel (dev.jhh3.pyre.sentinel) also runs Pyre.app's pyred, so it shares Pyre.app's **Local Network** permission ("Allow Pyre to find devices on local networks"): grant it once. Sentinel agents installed before this ran `~/.local/bin/pyred`, which resolves to a per-version path; macOS keys a bare binary's Local Network rule to that path, so every upgrade asked again (and a denial broke the sentinel's LAN ssh and hub checks). `upgrade.sh` repoints such an agent at Pyre.app on the next upgrade; the old `pyred` entries under Local Network can be removed.

## Node run defaults: permissions, default account, macOS profile credentials

- `pyred --permissions ask|bypass` (default `ask`) sets the permission mode of
  every Pyre-launched run on the node. `bypass` launches Claude with
  `--dangerously-skip-permissions` and starts Codex threads with
  `approval_policy=never` + `sandbox_mode=danger-full-access` (its attached
  TUI gets `--dangerously-bypass-approvals-and-sandbox`). Such runs advertise
  `permissions.bypass` (shown by `pyre sessions show` as `perms:` and in the
  TUI detail); the approval broker sees no requests. Override per launch with
  `pyre start --permissions ask|bypass` or the MCP `start_session`
  `permissions` field; a resume keeps the previous run's mode. A Claude bypass
  run is refused unless the owner accepted Claude's one-time warning on that
  node (`skipDangerousModePermissionPrompt: true` in `~/.claude/settings.json`,
  or the older `bypassPermissionsModeAccepted` in `~/.claude.json`); the run's
  settings then carry that key so the dialog never blocks.
- `pyred --default-account auto|subscription|ambient` (default `auto`) decides
  launches without `--account`/`--ambient`. `auto`: a node with no Pyre
  account (profile) for the harness runs on your own login there, exactly like
  running `claude`/`codex` yourself; a node with accounts selects an eligible
  subscription and waits when they are exhausted, never falling back to your
  login. The launch review says which (`your own login on mac (this node has
  no Pyre anthropic accounts)`).
- macOS rota checkouts keep the profile credential in the 0600
  `<profile>/.credentials.json`; Pyre never creates keychain items for
  profiles (an item created by the pyred LaunchAgent needed a keychain ACL
  approval before `claude` could read it, so launches hung on an invisible
  prompt with a blank pane). At start, pyred moves the credential of any
  `Claude Code-credentials-<sha256(profile dir)[:8]>` item an earlier version
  created into the file and deletes the item; a locked keychain defers this to
  the next start. The owner's `Claude Code-credentials` item and other config
  dirs' items are never touched.
- Every run's `TMPDIR` is its own folder on the node's disk,
  `<state>/tmp/runs/<run_id>` (0700), instead of `/tmp` (a RAM-backed tmpfs
  with a per-user quota on server and the office NUCs). Codex threads get it as
  a per-thread `shell_environment_policy.set.TMPDIR`. Claude's own
  `CLAUDE_CODE_TMPDIR` (`<state>/tmp/claude`) is unchanged. A `TMPDIR` the
  run's environment already sets (an environment profile, launch prep, the
  session's `--env-file`) is kept. `pyred --run-tmp-root DIR` moves the
  folders; `--run-tmp-root none` leaves runs the TMPDIR they inherit. Not
  set on Windows (its TEMP is already on disk) or when the root contains
  whitespace, as macOS's default state dir does (scripts expand `$TMPDIR`
  unquoted), so macOS runs keep launchd's `/var/folders` TMPDIR. `pyre run`
  (headless) runs keep the caller's. pyred removes a run's folder when its
  session is archived (`session.cleanup`, `node.sweep`) or, in the 10-minute
  maintenance pass, once the run ended over 24 h ago and no newer run of its
  session is going (at most 256 folders examined and 32 removed per pass;
  a folder no run record names goes after 24 h with no file changed). A
  folder any of the user's processes still holds (open file, cwd or
  executable: `/proc/*/{fd,cwd,exe}` on Linux, `lsof` on macOS) is kept;
  when that cannot be checked, nothing is removed.

## Extra harness arguments and adopting a Claude session

`pyre start` takes extra harness arguments after `--` (or one per
`--harness-arg`); the MCP `start_session` takes `harness_args`. They are an
allowlist per harness, checked by the CLI and again by the node:

| Harness | Allowed |
| --- | --- |
| claude | `--append-system-prompt-file`, `--append-system-prompt`, `--system-prompt-file`, `--channels`, `--dangerously-load-development-channels`, `--plugin-dir`, `--add-dir`, `--mcp-config`, `--settings`, `--agents`, `--agent` |
| codex, agy | none yet |
| shell | anything: the arguments are the shell's command line (`pyre start --harness shell --cwd D -- -lc 'cmd'`; on PowerShell nodes use `-NoProfile -NonInteractive -EncodedCommand <base64 UTF-16LE>`, since `-Command` text is requoted by Windows argument passing), passed unchanged by every node |

Pyre refuses the flags it controls itself (`--resume`, `--session-id`,
`--continue`, permission flags, `--model`/`--effort`, `-p`, `--bg`,
`--strict-mcp-config`, ...). The run records the arguments (`pyre sessions
show` prints `args:`); `pyre resume` and account switches relaunch with the
same ones. A machine whose pyred predates this (no `launch.harness_args` capability) is refused rather than given arguments it would pass unchecked. `pyre start --dry-run ...` prints the canonical arguments and the
Claude command line the node builds, without starting anything.

Paths (`--append-system-prompt-file`, `--system-prompt-file`, `--plugin-dir`,
`--add-dir`, `--settings`) must be absolute paths on the machine. The node
reads a `--settings` file before launch and refuses one that sets `env`,
`apiKeyHelper` (or the other credential helpers), `disableAllHooks`,
`forceLogin*` or `permissions.defaultMode`: those would override the account
binding, the run's permission mode or Pyre's hooks.

Notes for Claude:
- A session's `--append-system-prompt` text joins Pyre's agent guide in one
  flag (Claude keeps only the last one); `--append-system-prompt-file` works
  alongside it.
- `--dangerously-load-development-channels plugin:NAME@MARKETPLACE` shows a
  confirmation dialog at every Claude start. For a run launched with that flag
  the adapter confirms it ("I am using this for local development") while the
  run starts (before Claude's input box appears, within 2 minutes) and audits
  `node.dev_channels_accepted`; other runs leave the dialog for a human. The
  plugin and its marketplace must be in the account's profile: profiles copy
  `settings.json` (`enabledPlugins`, `extraKnownMarketplaces`) and
  `plugins/known_marketplaces.json`/`installed_plugins.json` from your own
  `~/.claude`, and keep every local marketplace pyred can read (the
  readability probe reads the folder and its `.claude-plugin/marketplace.json`;
  with Full Disk Access for Pyre.app a `~/Documents` marketplace is readable).
  An unreadable one is still left out and reported, as before.

To run an existing interactive Claude (started outside Pyre) as a Pyre
session, keep its conversation with `--native-resume <session id>`: stop the
original process first (two Claude processes must never write one
conversation), then start the session in the same directory with the same
arguments:

```
pyre start --node laptop --harness claude --cwd <the process's directory> \
  --permissions bypass --title Assistant --native-resume <claude session id> -- \
  --dangerously-load-development-channels plugin:example-bridge@example \
  --append-system-prompt-file /path/to/agent.md
```

The node looks for the transcript in your own `~/.claude` and the account
profiles and copies it into the bound account's profile (on nodes whose
profiles share `projects/`, into the shared store); an older copy from an
earlier adoption is refreshed when the newer transcript extends it. Without
the transcript the launch fails at once (`not on this node`); a conversation
written in the last 30 seconds or held open by a process is refused (`looks
live outside Pyre`). Runs whose extra arguments include `--settings`,
`--plugin-dir`, `--agents`, `--agent` or `--mcp-config` show `perms: ask +
custom (...)`: those can allow tools or add hooks beyond the recorded mode.

### Session environment variables: --env-file

`pyre start --env-file PATH` (MCP `start_session` `env_file`) gives the
harness process extra environment variables from a file on the node, without
the values leaving it: the node reads the file at the launch and at every
relaunch (`pyre resume`, account switches), and the run records only the path
(`pyre sessions show` prints `env file:`). The file is dotenv-style:

```
# ~/.config/pyre/agent.env (chmod 600)
AGENT_BRIDGE_TOKEN=...
AGENT_INTAKE_URL=http://127.0.0.1:8091
AGENT_WORKTREE=/path/to/agent/worktree
ANTHROPIC_API_KEY=
```

One `KEY=VALUE` per line, optional `export`, `#` comments, `'single'` or
`"double"` quoted values (one line each), each key once. PATH must be an
absolute local path on the node (no network share, nothing under `/dev`,
`/proc` or `/sys`); the file must be a regular file owned by the node's user
and private to it (`chmod 600`; on Windows no ACL entry for anyone else).
Refused, in any letter case: keys Pyre sets (`PYRE_*`, `HOME`, `PATH`,
`CLAUDE_CONFIG_DIR`, `USERPROFILE` and the other Windows home variables,
`TMUX`, `TMUX_PANE`, `CLAUDE_CODE_TMPDIR`) and, unless empty, login,
provider and config-home keys (`ANTHROPIC_*`, `CLAUDE_CODE_OAUTH_*`,
`CLAUDE_CODE_USE_*` such as Bedrock/Vertex/Foundry, `CODEX_*`,
`OPENAI_API_KEY`, `GEMINI_*`, `GOOGLE_API_KEY`,
`GOOGLE_APPLICATION_CREDENTIALS`, ...); an empty one such as
`ANTHROPIC_API_KEY=` unsets it, which keeps Claude on the account's
subscription login. Errors name the line and key, never the value;
`pyre start --dry-run --env-file PATH` checks a copy on the machine it runs on,
prints `KEY=<redacted>` and warns about `DISABLE_TELEMETRY` and
`CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC` (they turn off the Claude plugin
monitor that brings queued Pyre mail). Editing the file changes the next
relaunch, not the running process. A machine whose pyred predates this (no
`launch.env_file` capability) is refused.

The file is read again at every resume and account switch. When it has gone
missing or is no longer private, the resume fails, and so does an automatic
account switch (it then asks you, naming the run that recorded the file):
fix the file, or `pyre resume <session> --env-file /other/path` to replace it
or `--env-file none` to continue without one; later resumes keep the choice.

Not covered: a value the agent itself prints (running `env`, echoing the
token) reaches the transcript and screen like any other output; the node does
not redact env-file values from them. Key names appear in errors and the
node log, so do not put secrets before the `=`. On Windows the owner and ACL
checks read the path after opening it, not the handle. For Codex the values
sit in the run's private `thread-config.json` on the node (removed when the
run ends, left behind if pyred crashes), and MCP servers Codex starts do not
get them.

## Headless runs: pyre run

`pyre run [--project P] [--title T] [--account A] -- claude ARGS...` (or
`-- codex exec ARGS...`) runs a headless Claude or Codex command (`claude -p
...`, `codex exec ...`) as its own direct child, with stdin, stdout and stderr
passed through byte for byte and the child's exit code (or the signal that
ended it) as its own. It is a drop-in for programs that shell out to `claude
-p` (a Sentry triage job) or `codex exec` (a PR review loop's parallel
review script):

```
pyre run --title "sentry triage" -- claude -p "$PROMPT" --output-format json \
  --json-schema "$SCHEMA" --dangerously-skip-permissions --verbose --model sonnet
pyre run --title "pr review" -- codex exec --json --sandbox read-only --skip-git-repo-check \
  --cd "$REPO" -o "$LAST" -m gpt-6-luna -c model_reasoning_effort=xhigh - <"$PROMPT_FILE"
pyre run -- codex exec --json -o "$LAST" resume "$THREAD" - <"$FOLLOWUP_FILE"
```

- The machine's pyred binds the best eligible account the way placement does
  (weekly headroom first; `--account` pins one), and the command runs with that
  account's profile environment (`CLAUDE_CONFIG_DIR` or `CODEX_HOME`; a token
  account's variable); API keys and other logins of that harness in the
  caller's environment are removed, exactly as for Pyre-launched runs. Limited,
  exhausted or needs-reauth accounts are skipped the same way. Pyre adds
  nothing else: no agent guide, no Pyre MCP server, no hooks.
- Inside a Pyre session the command does not run as that session: the
  session's identity (`PYRE_SESSION_ID`, `PYRE_RUN_ID`, `PYRE_SESSION_TOKEN`,
  `PYRE_HARNESS`, `PYRE_RUN_DIR`, `PYRE_PROJECT_ID`, `PYRE_PARENT_SESSION_ID`,
  `PYRE_REPORT_TO_PARENT`, `PYRE_PERMISSIONS`, `PYRE_ENV_GENERATION`,
  `PYRE_CLAUDE_STATE_DIR`) is removed from the child's environment, also on
  the fallback path. A `pyre mcp` server the child's own config starts (an
  account profile's Codex config carries the owner's `[mcp_servers.pyre]`)
  or a hook it runs therefore never acts as the calling session, marks its
  turns or writes its hook state; with no session token it uses the
  machine's client login, as when codex runs outside Pyre. Machine-level
  variables (`PYRE_SOCKET`, `PYRE_NODE_ID`, `PYRE_HUB`, `PYRE_CLI`,
  `PYRE_TOKEN`) stay.
- The hub records a session (harness `claude` or `codex`, tag `pyre run`,
  `--project` when given, title `pyre run: <folder>` by default) and a headless
  run (adapter `claude-exec` or `codex-exec`). It ends `exited` with the
  command's exit code and the native id: Claude's session id, read from the
  JSON output (`--output-format json` or `stream-json`) or, failing that, from
  the one new transcript; Codex's thread id, read from the first `thread_id`
  of `codex exec --json` output (its `thread.started` event) or, failing that,
  for a new thread only, from the one thread created after the run started
  for its cwd under the bound `CODEX_HOME` (`sessions/<today or yesterday>`,
  by the rollout's `session_meta`; none with `--ephemeral`, or with `-C`
  naming another directory). A resumed command (`claude --resume ID`, `codex
  exec resume ID`) records the resumed conversation from the start. `pyre
  read` on a Codex run reads its rollout file (no app-server). `pyre sessions`,
  `pyre search` and `pyre read` show it, and `pyre resume` continues it as an
  interactive session. It never reports to a parent session and never raises
  owner attention. `pyre stop` signals the child.
- `--resume ID` in the command runs on the account that reaches that
  conversation. Profiles on server share one transcript store
  (`<profile>/projects -> ~/.local/state/pyre/shared/claude/projects`), so any
  account works there; a conversation only in one profile binds that account,
  one only in `~/.claude` the machine's own login.
- Codex: `codex exec resume ID` (a thread id, not a thread name or `--last`;
  a fork starts a new thread and gets no hint) does the same with rollouts: the profiles on server share
  `<profile>/sessions -> ~/.local/state/pyre/shared/codex/sessions`, so any
  account reaches the thread, and the run prefers the account a run on this
  node last ran it on (Codex cross-account resume is unproven); a rollout only
  in one profile binds that account, one only in `~/.codex` (or `$CODEX_HOME`
  of pyred) the machine's own login. With `--last` the newest thread of the
  bound account's `CODEX_HOME` is resumed (the shared store's newest on server).
- Codex commands must be `codex exec ...` with `exec` (or `e`) first; options
  for codex go after it (`codex exec -c k=v ...`, not `codex -c k=v exec`). An
  interactive `codex`, `codex resume` and other subcommands are refused with a
  usage error (exit 2): use `pyre start --harness codex` for those.
- When Pyre is **unavailable** (no pyred or one older than this feature, hub
  unreachable, node disconnected, setup timeout) or **declines** (no eligible
  account, the node's run capacity, a bad `--account`), the command runs exactly
  as given on the machine's own login after one stderr line: `pyre run: Pyre is
  unavailable: ...` or `pyre run: Pyre declined the run: ...`, naming the login
  used (`CLAUDE_CONFIG_DIR=...` or `~/.claude`). `--fallback fail`
  (`PYRE_RUN_FALLBACK=fail`) exits 75 without running instead. After an outage
  later calls skip the setup for 60 s. Setup waits at most 10 s
  (`--setup-timeout`, `PYRE_RUN_SETUP_TIMEOUT`). Measured overhead against an
  isolated hub and node on server: p50 22 ms, p95 75 ms per call.
- With no Pyre account bound (the node's own login), the caller's
  `CLAUDE_CONFIG_DIR` / `CODEX_HOME` is kept; a profile binding sets it. Only
  the harness's own login variables are removed (claude: `CLAUDE_*`,
  `ANTHROPIC_*`; codex: `CODEX_*`, `OPENAI_API_KEY`); other keys stay, so a
  codex keeps the caller's Anthropic key for its tools and vice versa. A
  Codex run on the node's own login records pyred's `CODEX_HOME` (else
  `~/.codex`) as its home: a caller that sets another `CODEX_HOME` still runs
  on it, but `pyre read` and the thread lookup then miss its rollout.
- The child dies with `pyre run`: forwarded signals, a Linux parent-death
  signal for SIGKILL, and on every platform the node stops a child whose
  `pyre run` vanished. A headless caller's child leads its own process group.
- Output to a pipe is relayed (and read for the session id); output to a file
  is passed as is, so a background process claude left behind never keeps
  `pyre run` waiting.
- The session is archived when the run ends (still searchable, readable and
  resumable); `--keep` leaves it listed.
- No hub login is needed on the machine. With no client config and no
  `PYRE_SESSION_TOKEN`, the CLI asks the local pyred to record the run with the
  node's own credential (`POST /v1/local/exec/start` on its socket, found at
  `$PYRE_SOCKET` or the default state path:
  `~/.local/state/pyre/pyred.sock`, `$XDG_STATE_HOME/pyre/pyred.sock`, macOS
  `~/Library/Application Support/pyre/pyred.sock`). The hub allows a node only
  this: a headless `claude` or `codex` session on itself, tagged `pyre run`,
  `report:none`, `archive:when-done`, with no parent, prompt or session token.
  `--project` and `--account` are resolved by the node the same way the CLI
  resolves them; `--keep` needs a login (the session is archived anyway, with
  one stderr line). A socket-only caller (`PYRE_SOCKET`, no token) takes the
  node path even when pyred holds `--owner-credential`.
  Do not pair an owner CLI login on a machine that runs untrusted prompts
  (devbox's Sentry triage): `pyre run` works without one.
- Rollout: upgrade the hub before the nodes. An older hub refuses a
  node-started `codex` run (`harness must be claude`; the CLI then falls back:
  declined), and an older pyred refuses any headless codex start (`not
  supported on this node`; declined, falls back). An older hub raises owner
  attention for a refused headless start (its run is recorded as failed), and
  does not restrict what a node credential starts. The CLI can go anywhere: a
  new CLI against an older pyred without a login falls back with "this
  machine's pyred cannot record a run itself".

## Windows nodes

A Windows 10 1809+/11 or Server 2019+ machine (amd64 or arm64) can be a node:
`pyred.exe` and `pyre.exe` run natively (not WSL) and launch Claude Code and
Codex as native Windows processes. The hub stays on macOS/Linux (`pyre-hub` is
not built for Windows).

**Install, upgrade, uninstall** (PowerShell, as the owning user, no
Administrator; the release zip with `SHA256SUMS` and `SHA256SUMS.sig` beside
it, `release-allowed-signers` from a trusted checkout):

```powershell
$I = "pyre-<V>-windows-amd64\scripts\install"   # extracted once from the zip
powershell -NoProfile -ExecutionPolicy Bypass -File $I\install.ps1 -Zip pyre-<V>-windows-amd64.zip -AllowedSigners release-allowed-signers -EditPath
# node + logon task, enrolled with a ticket (`pyre --json enroll-node --name win1` on any client):
Get-Content ticket.json | powershell -NoProfile -ExecutionPolicy Bypass -File $I\install.ps1 `
  -Zip pyre-<V>-windows-amd64.zip -AllowedSigners release-allowed-signers -Service node -Name win1 -HubUrl https://hub:7443
# upgrade: the same command with the new zip, plus -Restart
powershell -NoProfile -ExecutionPolicy Bypass -File $I\uninstall.ps1    # -Purge -Yes also deletes state
```

`install.ps1` mirrors `install.sh`: signature and checksum verification before
anything is extracted (`ssh-keygen -Y verify` from Windows OpenSSH 8.1+, else
the already installed `pyred.exe verify-release`, never a binary from the
candidate zip; exit 3 on refusal), immutable version directories, pruning,
`-DryRun`, `-Root DIR`. The enrollment ticket is read from stdin (or
`-EnrollTicketFile`) and handed to `pyred enroll --ticket-stdin --state DIR
[--hub URL]` on stdin, never on a command line; an existing
`node-credential.json` skips enrollment. `-HubUrl` is the hub as reachable from
this node. Manual enrollment without the installer pipes the ticket the same
way: `Get-Content ticket.json | pyred enroll --ticket-stdin [--hub URL]`. A node
enrolled over ssh with `pyre enroll-node --bootstrap --execute` (Nodes, above)
already has `%LOCALAPPDATA%\Programs\Pyre\node-credential.json`; install.ps1
uses it.

`-Permissions bypass` (or `-PyredArg --permissions=bypass`) runs the node
with bypass permissions and merges `skipDangerousModePermissionPrompt: true`
into `%USERPROFILE%\.claude\settings.json`, like `install.sh --permissions
bypass` (only a JSON `true` counts as accepted; the rewrite is parsed back and
compared, so a Windows PowerShell 5.1 round trip that would change arrays or
empty objects is not written; a file that cannot be written is left alone;
each case warns and the install goes on). (`powershell -File` passes `-PyredArg A,B` as the one string `A,B`;
install.ps1 splits a value whose comma-separated pieces all start with `--`,
so use the `=` form, or `-PyredArgsFile` with a JSON array. A repeated
`-PyredArg` is a PowerShell error.) stdin is read for a ticket only while the
node has no `node-credential.json`, so an upgrade over ssh does not wait on it.

**Outfit (`-WithTools`).** The Outfit Claude/Codex plugin's MCP server and
hooks run `outfit-core`. `-WithTools` installs the bundled Outfit for Windows
from `pyre-tools-<V>-windows-<arch>.zip` (beside `-Zip`, checked against the
same verified `SHA256SUMS`; or `-ToolsZip FILE`, `-ToolsFrom DIR`) into
`%LOCALAPPDATA%\Programs\Pyre\tools\outfit\bin\{outfit.exe,outfit-core.exe}`
(two identical binaries: Windows has no symlink shims). pyred puts that
directory on its runs' PATH, so Pyre sessions find `outfit-core` without a
PATH change; `-EditPath` adds it to the user PATH for other shells. An
`outfit`/`outfit-core` already on PATH (for example copies in
`%USERPROFILE%\.local\bin`) is left alone and the bundled copy is skipped
unless `-ToolsAlongside`; delete such copies first to switch to the bundled
one. Hive and Rota are not bundled for Windows. `uninstall.ps1` removes the
tools and their PATH entry.

**Layout:**

| Path | Contents |
| --- | --- |
| `%LOCALAPPDATA%\Programs\Pyre\<version>\bin\{pyre,pyred}.exe` | immutable version directories |
| `%LOCALAPPDATA%\Programs\Pyre\current` | directory junction to the active version (junctions need neither Administrator nor Developer Mode); pruned versions become junctions to `current`, so hook paths of older sessions keep resolving |
| `%LOCALAPPDATA%\Programs\Pyre\tools\outfit\bin` | bundled Outfit (`-WithTools`): `outfit.exe`, `outfit-core.exe` |
| `%LOCALAPPDATA%\Pyre` | node state: database, logs, runs, AF_UNIX socket, node credential (`pyred --state` overrides) |
| `%APPDATA%\Pyre` | config: `client.json`, pinned `release-allowed-signers`, `keys.toml` |

State and config directories get a protected owner-only DACL (the user and
SYSTEM, full control, inherited by everything below; the Windows equivalent of
0700/0600). `-EditPath` adds `...\Pyre\current\bin` to the user `PATH`
(`HKCU\Environment`; new shells see it).

**Why a logon task, not a Windows service.** Services run in session 0, which
has no interactive desktop: agents could not launch or test GUI desktop apps
(Electron windows, installers, screenshots). `pyred service install --state DIR
-- <run args>` registers a per-user Task Scheduler task with a logon trigger and
an interactive token, so pyred runs in the user's desktop session with the
user's environment (user and system `PATH`; pyred adds its own tool discovery:
npm global `%APPDATA%\npm`, nvm-windows `%NVM_SYMLINK%`, `%ProgramFiles%\nodejs`,
winget/scoop shims, Git for Windows). `pyred service status|start|stop|uninstall`
manage it.

**Restarts.** The task runs `pyred.exe supervise -- run ...`: a small
supervisor that runs `pyred run` as a child, restarts it after a crash with
backoff, and carries out the in-product update handoff (Windows has no
`exec`, so the supervisor starts the managed version instead of the launcher
replacing itself). The task itself repeats as a watchdog and ignores new
instances while one runs (logon trigger repeating every 5 minutes,
`MultipleInstancesPolicy=IgnoreNew`), so a dead supervisor comes back within
minutes. `pyred service stop` ends the task's supervisor; the daemon watches
the supervisor's process and shuts down cleanly. Stopping pyred does not stop
runs. A `pyred run` started by hand becomes its own supervisor on its first
update handoff.

**Session host.** Runs live in the Pyre session host (`pyred.exe
__pyre_termhost serve`), which must outlive pyred like a tmux server. pyred
first starts it with `CREATE_BREAKAWAY_FROM_JOB`; Task Scheduler runs the node
task in a job object that forbids breakaway, so pyred then registers (or
updates, since the pyred path changes on upgrade) a hidden on-demand task
`Pyre Session Host <user> <socket hash>` (interactive token, least privilege,
no time limit, ignore new instances, normal priority) whose action runs the
host through the install's `current` path, and starts it. The task instance has
its own job, so a pyred or supervisor restart leaves the sessions running. The
host's environment is handed over in a short-lived file in the private socket
directory, and `termhost.log` there records which path was taken.
`PYRE_TERMHOST_SPAWN=task` always uses the task, `=direct` never does (the host
then shares pyred's job and ends with it). A running host keeps its binary
until it is shut down; `pyred service uninstall` removes the session-host
tasks without ending a running host. Ending the task (`schtasks /End`) ends the
host and every session.

**Unattended GUI testing needs an interactive logon.** The task starts only
when the dev user logs on. For a test VM or lab PC that must come back after a
reboot without a person, configure auto-logon for that user:

- Sysinternals Autologon (`autologon.exe`) stores the password LSA-encrypted;
  or `HKLM\SOFTWARE\Microsoft\Windows NT\CurrentVersion\Winlogon`
  `AutoAdminLogon=1` with `DefaultUserName`/`DefaultDomainName` (avoid the
  plaintext `DefaultPassword` value).
- Trade-off: anyone with console or physical access gets that user's unlocked
  session, including its agent logins and the node credential. Use a dedicated
  low-value dev account on a dedicated VM (or a BitLocker-encrypted machine in a
  locked room), never your personal account. Screen lock on idle breaks GUI
  tests (a locked desktop cannot be driven), which is another reason to keep this
  to a VM with no other users.

**Network and local API.** pyred only dials out to the hub; it listens on no
TCP port, so no firewall rule is needed. The local API (CLI, hooks, agent
tools) is an AF_UNIX socket in the owner-only state directory, the same
transport as on macOS/Linux (Windows 10 1803+/Server 2019+). Codex's
app-server also listens on `unix://` (AF_UNIX) on Windows.

**Harness prerequisites** (`install.ps1` warns; nothing is installed for you):
Claude Code native install plus Git for Windows (`bash.exe`; set
`CLAUDE_CODE_GIT_BASH_PATH` if it is not in a standard place); the Codex CLI;
for Electron work Node.js (`node`, `npm`, `npx`: PATH, nvm-windows or
`%ProgramFiles%\nodejs`) and Visual Studio Build Tools with the C++ workload
(found through `vswhere.exe`) for native modules. A missing harness shows up as
unavailable in the node's probe rather than failing silently. pyred re-reads
the registry `PATH` at start (tools installed after logon), sets
`CLAUDE_CODE_GIT_BASH_PATH` for runs when it finds Git Bash and the variable is
unset, and runs the native `codex.exe` behind npm's `codex.cmd` shim. Claude
hooks are written in exec form (`command` + `args`, no shell). The doorbell
monitor is a shell command line and needs Git Bash: without it the run reports
`doorbell: disabled` and the probe says why.

**Not on Windows yet:** the OS credential store for Pyre-provisioned
connection credentials (`--credential-backend auto` refuses; the opt-in file
store works and is owner-only by DACL), ssh connection multiplexing for remote
attach, and anything that needs the terminal backend (W-winhost: run
spawn/capture/attach, pty-based TUI embedding and web-terminal relay).

**Still to verify on a real Windows machine** (not exercised by the
cross-compiled builds or the unix test suite):

- `install.ps1`/`uninstall.ps1` end to end on Windows PowerShell 5.1 and
  PowerShell 7 (signature paths, junction swap and pruning while a version runs,
  `-EditPath`, ticket on stdin).
- `pyred service install|start|stop|status|uninstall` against Task Scheduler as
  a standard user; restart after crash; watchdog; behaviour at logoff/reboot with
  auto-logon.
- Local API over AF_UNIX (CLI, hooks, doorbell) and the owner-only DACL checks
  on credential and config files.
- Claude Code and Codex runs (terminal backend, exec-form hooks, the doorbell
  monitor under Git Bash, Codex app-server over `unix://`, the minimum Codex
  version whose app-server supports `unix://` on Windows), GUI app launch from
  a run, `npm`/`npx electron`/node-gyp builds from a session.
- In-product update handoff through the supervisor; resource stats.
