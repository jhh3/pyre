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
| `--run-tmp-root DIR\|none` | Where each run's `TMPDIR` goes (`DIR/<run_id>`, 0700, removed after the session is archived or a day after the run ended). Default `<state>/tmp/runs`; `none` leaves runs the TMPDIR they inherit. A TMPDIR set by the run's environment profile or env file wins. See [operations](operations.md#node-run-defaults-permissions-default-account-macos-profile-credentials). |
| `--trust-dir DIR` (repeatable) | Claude and Codex may trust workspaces under `DIR` automatically, so a new folder does not stall on "Do you trust this folder?". Default: none. A Claude run with bypass permissions trusts its folder wherever it is. |
| `--permissions ask\|bypass` | `ask` (default) relays approval prompts to the TUI and phone. `bypass` runs Claude with `--dangerously-skip-permissions` and Codex without approvals or sandbox. Override per launch with `pyre new/start --permissions`. |
| `--default-account auto\|subscription\|ambient` | `auto` (default) uses a Pyre account when the node has one for that provider, else your own login. `subscription` always uses a Pyre account. `ambient` always uses your own login. |
| `--repo-roots DIRS` | Comma-separated folders scanned for git repos (default `~/code,~/src,~/Documents/code,~/projects`). |
| `--max-runs N` | Cap concurrent sessions on this machine. |
| `--ssh-alias NAME` | The alias other machines use for full-screen remote attach. |
| `--no-integrations` | Turn off the optional integrations (Hive memory, Outfit, Rota, environments, tool updates). |

`pyred run --help` lists the rest.

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
`~/.config/pyre/client.json`.

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
