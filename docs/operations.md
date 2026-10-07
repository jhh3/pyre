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
   only; Pyre still authenticates every device with its own token.
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
| Shared transcript store of the profiles (session histories and transcripts) | `shared/` next to the profile root: `~/.local/state/pyre/shared` by default | same |
| CLI local state (`pyre artifact get` verified cache, document drafts) | `~/.local/state/pyre/{cache,drafts}` (`$PYRE_STATE_DIR`), outside the node state directory | `~/.local/state/pyre/{cache,drafts}`, inside the node state directory |
| Android signing key | `~/.config/pyre/pyre-release.jks` + `android-signing.properties` (never in Git or in hub backups) | — |

`pyre enroll-node --bootstrap` writes `~/.local/share/pyre/bin/pyred` and
`~/.local/share/pyre/node-credential.json` on the target. The installer
recognises that layout and points the service at the bootstrap credential.

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
- Node service options: `--state DIR --credential FILE --name NAME
  --ssh-alias ALIAS --max-runs N`, and `--pyred-arg ARG` (repeatable) for any
  other `pyred run` flag, e.g. `--pyred-arg --rota-checkout` or
  `--pyred-arg --trust-dir --pyred-arg DIR` (never a secret). `--no-integrations`
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
  `TasksMax=512`); `Restart=on-failure`, `RestartSec=2s`, start-limit 10 per
  5 min, `LimitNOFILE=8192`. `pyre-runs.slice` (rendered next to the unit)
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

## Nodes

Manual enrollment:

```sh
pyre enroll-node --name server --ssh server                       # on a configured client; secret shown once
pyred enroll --hub https://<hub-name>:7443 --pin <sha256> \
  --node-id <node_id> --secret-stdin                        # on the node; paste the secret; writes <state>/node-credential.json
scripts/install/install.sh --tarball ... --service node --name server --ssh-alias server
```

`--ssh` records the alias other machines use for remote attach. `pyred enroll
--credential-file PATH` writes elsewhere; pass the same path to
`install.sh --credential`.

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

Check a node: `pyre nodes`, `pyre nodes show <name>`; on the node
`pyred status` (link, live runs, queues, pyred overhead).

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
  - A machine stays on its *home* account, the one it is using now or used last, while that account is below 80% on both the 5-hour and the weekly window.
  - Otherwise it takes the account with the most headroom that no other machine is using. Only when every eligible account below 80% is in use elsewhere does it share: fewest other machines first, then headroom.
  - When all accounts are at or above 80%, the most headroom wins, as before.
  - The node reads other machines' active runs from the hub (`accounts.list`) before each automatic choice: 2 s timeout, reused for 5 s. If the hub is unreachable, there is no spread for 30 s: only home stickiness and headroom, and the reason says "fleet view unavailable".
  - Two machines choosing at the same moment can land on the same account; nothing is reserved. Their next runs rebalance: the machine whose run started first keeps the account, and the other moves to a free one. Running sessions are never moved.
  - The assignment reason says why, e.g. "home account for mac; not used on other machines (5h 3% used, weekly 20%)". `pyre accounts` and the TUI show which machine holds an account ("in use by <session> on mac").

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

A run receives the credential when the node's active environment profile
lists the connection and a grant covers the session at launch (`pyre
connections grant CONN --project P --node N`, or `--session`): Claude and
Codex runs through their generation, plain shell runs through the default
profile's connections. The value goes into the run's environment only; the
run's `adapter_data["pyre.connections"]` manifest lists the connection,
variable name and store reference. A revoke affects the next launch (a running
process keeps its environment); a locked or missing store fails a granted
launch with an actionable error.

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
failed updates, folder-trust prompts and Claude exit dialogs a node answered by itself.
Entries are written in the same transaction as the change (hub mutations, node reports, and an
event-log append hook), hold IDs, names and short reasons only (no secrets, message bodies or
transcripts) and live in the hub table `audit_log` (namespace `hub_audit`). Retention: 90 days or
100k entries, whichever is smaller; `pyre-hub serve --audit-max-age 720h --audit-max-rows 20000`.

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
  or `--no-mark` does not. The TUI shows the same digest as a dismissible
  panel at the top of the Inbox (esc marks it seen), agents get the
  read-only MCP tool `get_digest` (pre-approved), and Android shows a
  digest card at the top of Sessions with its own per-app mark.
- Exit codes of the CLI: `pyre exit-codes`.

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
