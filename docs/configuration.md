# Configuration

Where Pyre keeps its files, the `pyred` flags worth knowing, key overrides, and how hub connections are
pinned. Procedures (upgrade, rollback, backup, uninstall) are in the [operations runbook](operations.md).

## Where things live

| What | Path |
|---|---|
| CLI config (hub URL, pin, your token; mode 0600) | `~/.config/pyre/client.json` (or `$PYRE_CONFIG`) |
| Key overrides (optional) | `~/.config/pyre/keys.toml` |
| Node state (database, sockets, logs, account profiles) | macOS `~/Library/Application Support/pyre`, Linux `~/.local/state/pyre` |
| Hub data | the `--data` directory you chose (database, artifacts, TLS key) |
| Binaries | `~/.local/share/pyre/<version>/`, shims in `~/.local/bin` |

The full path table is in [operations → paths](operations.md#paths).

## pyred flags

Pass them to the node service with `install.sh … --service node --pyred-arg <flag> [--pyred-arg <value>]`.

| Flag | What it does |
|---|---|
| `--worktree-root DIR` | Where new Pyre worktrees go (`DIR/<repo>/<branch>`); exported to every run as `PYRE_WORKTREE_ROOT`. Default `~/worktrees`; `""` puts them next to the repo as `<repo>-worktrees`. |
| `--run-tmp-root DIR\|none` | Where each run's `TMPDIR` goes (`DIR/<10-char id>`, 0700, short so Unix sockets under it fit; removed an hour after the session is archived, an hour after the run ended once a newer run of its session superseded it, or a day after the session's latest run ended; never while a process still uses it). Default `~/.ptmp` (`<state>/rt` for an unknown or long HOME; also on macOS when HOME has no whitespace); `none` leaves runs the TMPDIR they inherit. A TMPDIR set by the run's environment profile or env file wins. See [operations](operations.md#node-run-defaults-permissions-default-account-macos-profile-credentials). |
| `--trust-dir DIR` (repeatable) | Claude and Codex may trust workspaces under `DIR` automatically, so a new folder does not stall on "Do you trust this folder?". Default: none. A Claude run with bypass permissions trusts its folder wherever it is. |
| `--permissions ask\|bypass` | `ask` (default) relays approval prompts to the TUI and phone. `bypass` runs Claude with `--dangerously-skip-permissions` and Codex without approvals or sandbox. Override per launch with `pyre new/start --permissions`. |
| `--default-account auto\|subscription\|ambient` | `auto` (default) uses a Pyre account when the node has one for that provider, else your own login. `subscription` always uses a Pyre account. `ambient` always uses your own login. |
| `--repo-roots DIRS` | Comma-separated folders scanned for git repos (default `~/code,~/src,~/Documents/code,~/projects`). |
| `--max-runs N` | Cap concurrent sessions on this machine. |
| `--ssh-alias NAME` | The alias other machines use for full-screen remote attach. |
| `--no-integrations` | Turn off the optional integrations (Hive memory, Outfit, Rota, environments, tool updates). |
| `--run-memory-max V` | Linux: memory cap of each run's own systemd scope (the harness and everything it starts; a Codex app-server and its threads' commands share one). A share of RAM (`65%`, the default), a size (`16G`) or `off`. A run that reaches it is OOM-killed inside its scope (the largest process there, normally the runaway tool), not node-wide. |
| `--run-swap-max V` | Linux: swap cap of each run's scope: a share of swap (`25%`, the default), a size, `0` (no swap) or `off`. |
| `--run-memory-high V` | Linux: `MemoryHigh` (reclaim throttling) of each run's scope, as above. Default `off`: throttling stalls a runaway instead of ending it. |
| `--memory-guard on\|off` | The low-memory watchdog (default `on`). When the machine is nearly out of memory and swap it stops the largest process a Pyre run started (never a harness, pyred, tmux or a process outside Pyre's runs), audits it (`node.memory_guard_kill`) and tells the session. |
| `--memory-guard-available P` | Guard trigger: available memory below this share of RAM (default `5%`). |
| `--memory-guard-swap P` | Guard trigger on Linux: and more than this share of swap used (default `80%`; ignored without swap; macOS uses its critical pressure level instead). |

`pyred run --help` lists the rest. Memory protection (per-run scopes and the guard) is described in
[operations → memory protection](operations.md#memory-protection); `pyre nodes <node>` shows a node's settings on its
`memory cap:` line.

## keys.toml

```toml
# ~/.config/pyre/keys.toml: only what you list changes
leader = "space"
macos_option_arrows = "list"      # Terminal.app sends ESC b / ESC f for ⌥←/⌥→
[keys]
"sessions.attach" = ["enter", "o"]
"global.finder"   = ["<leader><leader>", "ctrl+p"]
```

Run `pyre keys` to see every action name and its effective keys. `PYRE_THEME=light|dark` forces a theme.
The [TUI design notes](../tests/tui/DESIGN.md) describe every view and key in depth.

## Hub URL and TLS pinning

The hub generates a self-signed certificate. Every client and node **pins** its SHA-256 fingerprint, so
hostnames don't matter: the same hub can be reached as `desktop.local`, a VPN IP, or `127.0.0.1` through an
ssh forward. Nodes dial out to the hub, and nothing dials into a node except ssh for full-screen attach.
Connection settings resolve in this order: `--hub`/`PYRE_HUB`, then `PYRE_SOCKET` (a local daemon), then
`~/.config/pyre/client.json`. Through `PYRE_SOCKET`, a call waits up to 30 s for a pyred that is restarting
(socket missing or refusing); `PYRE_SOCKET_WAIT` (a duration such as `10s`, or `0`) changes that.

When none of those is set (a plain ssh shell on a node, which has neither a session's `PYRE_SOCKET` nor a
client config), the CLI falls back to this machine's pyred socket at its default path, `<state dir>/pyred.sock`
(`$PYRE_STATE_DIR`, else `$XDG_STATE_HOME/pyre` or `~/.local/state/pyre` on Linux,
`~/Library/Application Support/pyre` on macOS, `%LOCALAPPDATA%\Pyre` on Windows), and prints
`pyre: using this machine's pyred (<path>)` on stderr. It uses the socket only if it is a socket (not a
symlink) owned by the current user, closed to group and others (pyred creates it 0600 inside its 0700 state
directory), answering, and pyred holds an owner credential (`pyred run --owner-credential`, shown as
`owner_credential` in `pyred status --json`); otherwise the "no hub configured" error names why it was not
used. Calls then act as the owner (`owner:local` in pyred's log), with pyred's owner credential for what it
forwards to the hub. This gives no access beyond what any process running as this user already has through
that socket. A pyred without an owner credential cannot act for you on the hub (it forwards a call without
a bearer, and the hub refuses it), so it is not used: in such a shell, pair (`pyre pair`) or run the command
from a Pyre session on the machine, whose `PYRE_SESSION_TOKEN` carries the session's identity.

## Security model

- **Secrets never go on a command line.** Pairing links, login tokens, enrollment secrets and account tokens
  are read from stdin or the keychain, never from argv, where other local processes could read them.
- **Each device has its own revocable credential** (`pyre clients`, `pyre revoke`). Credential files are
  0600, and Pyre refuses to use a client config that others can read.
- **Account tokens are encrypted at rest** on the hub. A node fetches a token over its pinned TLS link only
  when a run needs it, keeps it in memory, and passes it to the run through a private environment file.
- **Credentials are never copied between machines.** Each account gets an isolated profile per machine. Pyre
  never edits your own `~/.claude` or `~/.codex` login.
- **Your tmux is untouched.** Sessions run in Pyre-owned tmux servers on private sockets.
- **Releases are signed.** The installer pins the release key and refuses tampered or foreign-signed
  releases.

Reporting and the threat model: [SECURITY.md](../SECURITY.md) and the [architecture](architecture.md).
