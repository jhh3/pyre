# Getting started

The detailed version of the README's quick start: what each step does, the options, the requirements, and
fixes for the usual first-day problems. The [operations runbook](operations.md) has the complete install,
upgrade, rollback, backup and uninstall procedures.

## Requirements

| | |
|---|---|
| Machines that run agents | macOS or Linux (arm64 or amd64), `tmux` on `PATH` (`brew install tmux`, `apt install tmux`; 3.5+ recommended for Shift-Enter in agents), the `claude` and/or `codex` CLIs. Linux services need a systemd user manager, plus `loginctl enable-linger $USER` so sessions survive logout. |
| Hub | Any always-on Mac or Linux machine reachable by your other devices, on one HTTPS port (default 7443). |
| Building | Go 1.27+ as `go` on `PATH` (or `make build GO=/path/to/go`), `make`, `git`. Android: JDK 17+ and Android SDK 36/37 ([details](../android/README.md#build)). |
| Phone | Android 8.0+ (API 26). |

Start with one machine. It runs both the **hub** (the shared directory) and a **node** (the daemon that runs
sessions). Steps 1–3 install both as per-user services; to look around first without installing any service,
see [try it without services](#try-it-without-services).

## Try it without services

Everything runs in the foreground in two terminals and stops with `ctrl+c`. No launchd/systemd unit is
written and nothing outside your home folder changes. The `shell` harness needs no Claude or Codex account.

```sh
make build
scripts/install/install.sh --from bin --allow-unsigned --service none   # binaries + ~/.local/bin links only
export PATH="$HOME/.local/bin:$PATH"

HUB=https://127.0.0.1:7443
pyre-hub init --data ~/.local/share/pyre-hub --hub-url $HUB --tls-host 127.0.0.1
pyre-hub serve --data ~/.local/share/pyre-hub --listen 127.0.0.1:7443 --hub-url $HUB    # terminal 1, leave running
```

In a second terminal (with the same `PATH` and `HUB`):

```sh
pyre login --token - < ~/.local/share/pyre-hub/owner-credential.json
pyre enroll-node --name desktop        # prints a one-time secret and the pyred enroll command
pyred enroll --hub $HUB --pin <fingerprint> --node-id <node_id> --secret-stdin    # paste the secret
pyred run --name desktop &             # the node, in the background of this shell (or a third terminal)
pyre nodes                             # desktop · connected · shell tmux …

pyre new ~/scratch shell --mkdir       # a plain shell session in a new folder
pyre                                   # the TUI; q quits, the session keeps running
```

To undo the trial: stop `pyre-hub` and `pyred` (`kill %1`, `ctrl+c`), then remove `~/.local/share/pyre-hub`,
`~/.local/share/pyre`, `~/.local/bin/{pyre,pyred,pyre-hub}`, `~/.config/pyre`, and the node state
(`~/Library/Application Support/pyre` on macOS, `~/.local/state/pyre` on Linux). Stopping `pyred` leaves its
private tmux server and sessions running; `pyre stop <session>` first, or `tmux -S <state>/tmux/pyre.sock
kill-server`. Continue with the steps below when you want services that start at login.

## 1. Build and start the hub

```sh
make build                                   # bin/pyre, bin/pyred, bin/pyre-hub
export PATH="$HOME/.local/bin:$PATH"         # where the installer puts the commands (add it to your shell rc,
                                             # or pass --edit-path to the installer)

HUB=https://my-desktop.local:7443            # a name or IP your other machines and phone can reach
bin/pyre-hub init --data ~/.local/share/pyre-hub --hub-url $HUB --tls-host my-desktop.local --tls-host 127.0.0.1
scripts/install/install.sh --from bin --allow-unsigned \
  --service hub --hub-data ~/.local/share/pyre-hub --hub-url $HUB
```

`init` prints the hub's TLS fingerprint and writes an owner credential to
`~/.local/share/pyre-hub/owner-credential.json`. The installer copies the binaries to `~/.local/share/pyre`,
adds `pyre`, `pyred` and `pyre-hub` to `~/.local/bin`, and registers a per-user service (launchd on macOS,
systemd `--user` on Linux) that starts now and at login. It never needs root. `--allow-unsigned` is required
only because you built the binaries yourself. Install a [signed release tarball](../packaging/README.md) with
`--tarball … --allowed-signers packaging/release-allowed-signers` instead. `--service none` installs only the
binaries; `--dry-run` prints what would change.

## 2. Log in the CLI

```sh
pyre login --token - < ~/.local/share/pyre-hub/owner-credential.json   # token, hub URL and pin from init
pyre hello                                   # checks the connection
```

The credential file carries the `--hub-url` you gave `init` and the TLS fingerprint; `--hub` and `--pin`
override them. On another computer, copy the file over, log in, and delete the copy.

## 3. Turn this machine into a node

```sh
pyre enroll-node --name desktop              # prints a one-time secret and the exact next command
pyred enroll --hub $HUB --pin <fingerprint> --node-id <node_id> --secret-stdin    # paste the secret
scripts/install/install.sh --from bin --allow-unsigned --service node --name desktop \
  --pyred-arg --trust-dir --pyred-arg ~/code   # optional: let agents start in ~/code without the trust prompt
pyre nodes                                   # desktop · connected
```

Run the installer from an interactive shell where `tmux`, `claude` and `codex` resolve. It records their
locations for the service. `pyre nodes` lists the harnesses the node found; `claude (unavailable)` means the
CLI was not on that `PATH`.

## 4. Add another machine

From your configured machine, over the ssh alias you already use:

```sh
pyre enroll-node --name laptop --ssh laptop --bootstrap            # dry run: shows every step
pyre enroll-node --name laptop --ssh laptop --bootstrap --execute  # copies pyred, enrolls (secret goes over ssh stdin)
```

If that machine has a different OS or CPU, run `make dist` first and add
`--pyred dist/pyre-<version>-<os>-<arch>/pyred`. Then copy the repository (or a release tarball) to that
machine and run `scripts/install/install.sh … --service node --name laptop` there. To use the CLI/TUI on
another computer, run `pyre pair-client --name laptop --kind cli` here and `pyre pair` there (paste the link
at the prompt).

## 5. Pair your phone

Build the app (`android/gradlew -p android assembleDebug`, see [android/README.md](../android/README.md)) and
install it. Then:

```sh
pyre pair-client --name phone --kind android     # prints a one-time pyre://pair link
```

Open the link on the phone, or tap **Paste link**. The app shows the hub and the certificate fingerprint
before it connects. The phone needs to reach the hub URL directly, for example on your LAN or through a VPN
such as Tailscale, WireGuard or Twingate.

## 6. Add a Claude account

```sh
claude setup-token | tail -1 | pyre accounts add-token personal --provider anthropic
pyre accounts                                    # 5-hour / weekly usage per account
```

Repeat for each subscription. Each token is read from stdin (or the macOS keychain with
`--keychain-service`) and is never printed. Codex runs use each machine's own `codex login` by default;
`pyre accounts login --help` covers device-code logins.

How the pool picks an account: each machine sticks to its "home" account while that account is under 80% on
its 5-hour window and under 70% weekly; otherwise it picks the account with the most headroom (the window closest
to its limit decides) that no other machine is using. Idle sessions on an account past 85% weekly move to one with
room between turns. When a Claude account hits its limit mid-session, Pyre hands the session to another Claude account and
says so. It never silently switches Claude work to Codex or to a paid API key. If no usable account is left,
the session waits for the reset and you get notified.

## 7. Start your first session

```sh
pyre new myrepo "add a --verbose flag and tests"     # by repo name, on the machine that has it
pyre                                                 # open the TUI
```

`pyre repos` lists what the nodes found. A node scans its repo roots 20 seconds after it starts and then every
10 minutes, so a repository cloned a moment ago may not be listed yet; start it by path meanwhile
(`pyre new ~/code/myrepo`). Claude is the default harness; add `codex` or `shell` after the name
(`pyre new myrepo shell`).

Or press `n` in the TUI and type `myrepo add a --verbose flag`. The launcher line is
`repo[+repo] [@machine] [claude|codex|agy|shell] first prompt…`; `ctrl+f` starts the session in a fresh git
worktree on a branch named after the prompt, and several repos (`api+web`) get side-by-side worktrees in one
folder. `pyre start --node … --cwd …` runs in any directory; without `--node` (and without `--cwd`) Pyre picks the least
loaded connected machine with the harness and says which. `pyre monitor add --every 10m --cmd '…'` wakes a
session only when a shell check's output changes (session lists show it as "watching N" on that session; Claude
Code sessions can use Claude's own Monitor tool instead; a Codex session can leave a command running in the
background and end its turn, and Pyre wakes it when the command finishes). `pyre keys` prints every key.

## Troubleshooting

**A new session sits at "Do you trust this folder?"**
Start `pyred` with `--trust-dir` for the folders you trust (see [configuration](configuration.md#pyred-flags)),
or answer once with `enter` in the session's terminal.

**On macOS, a session stays in "starting" with a blank pane, or repos under `~/Documents` are missing.**
macOS privacy protection guards `~/Documents`, `~/Desktop` and `~/Downloads`. The `pyred` background service
can't answer a privacy prompt nobody sees. Approve the prompt on that Mac once (System Settings → Privacy &
Security → Files and Folders), or keep repositories outside those folders and point `--repo-roots` at them.

**Copying text from the TUI doesn't reach my clipboard inside tmux.**
Pyre copies with OSC 52. Add `set -g set-clipboard on` to the `~/.tmux.conf` of the tmux you run `pyre` in.

**`⌥↑`/`⌥↓`/`⌥s` do nothing on a Mac.**
Make Option send Meta: in Terminal.app, Settings → Profiles → Keyboard → "Use Option as Meta key"; in iTerm2,
Profiles → Keys → Left Option key → "Esc+". `⌥←`/`⌥→` work without this (`macos_option_arrows` in
keys.toml).

**My agents keep stopping for permission prompts while I'm away.**
Answer them from the TUI (`a`) or the phone's inbox, which offers one tap **Allow / Deny**. For machines
where you accept the risk, run `pyred --permissions bypass`, or pass `--permissions bypass` for a single
launch.

**`pyred run` says the tmux socket path is too long.**
Unix sockets are limited to about 100 bytes. With a very long home folder or `--state`, pass a shorter
`--state` directory (or set `PYRE_STATE_DIR`).

**`pyre nodes` says a harness is unavailable, but it works in my shell.**
The service PATH was recorded at install time. Re-run the node install from an interactive shell where
`command -v claude` works, or pass `--path`.

**A node shows `disconnected`.**
On that machine, run `pyred status`, check the service (`launchctl print gui/$UID/dev.jhh3.pyre.pyred` or
`systemctl --user status pyred`), and run `curl -sk $HUB/healthz`. More cases are in
[operations → troubleshooting](operations.md#troubleshooting).
