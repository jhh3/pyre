# Security policy

## Reporting a vulnerability

Please **do not** open a public issue for security problems. Report them privately through GitHub: on the repository's **Security** tab, choose **Report a vulnerability**. This opens a private advisory that only the maintainers can see.

Include what you found, how to reproduce it, the affected version (`pyre version`, `pyred version`, `pyre-hub version`, or the Android app version), and the impact you expect. You should get an acknowledgement within a few days. Once a fix is released, we will credit you in the advisory unless you would rather stay anonymous.

Pyre is pre-1.0, so security fixes go into the latest release only.

## What Pyre protects, and how

Pyre runs coding agents on your machines and can type into their terminals, so a stolen Pyre credential is about as powerful as a shell on those machines. The design aims to keep those credentials narrow, revocable and out of reach of other local processes.

- **Transport.** All traffic between clients, nodes and the hub is HTTPS or WebSocket over TLS. The hub uses a self-signed certificate, and every client and node pins its SHA-256 fingerprint. A pin replaces CA and hostname checks, so neither DNS nor a network path can impersonate the hub.
- **Identity.** Every device (CLI, phone, node) and every agent session has its own bearer credential, issued through one-time pairing or enrollment codes. You can revoke any of them on its own (`pyre clients`, `pyre revoke`). A session's credential acts only as that session, so it cannot claim another identity.
- **No secrets in argv.** Pairing links, login tokens, enrollment secrets and account tokens are read from stdin or the macOS keychain, never from command-line arguments. The ssh bootstrap sends the enrollment secret on ssh stdin.
- **At rest.** Client and node credential files are written 0600, and Pyre refuses to use them when others can read them. The hub data directory is 0700. Subscription tokens added with `pyre accounts add-token` are encrypted on the hub with an age X25519 key kept beside the database. No read API returns them, and hub backups contain only the ciphertext.
- **In use.** A node fetches an account token only for a run that needs it, keeps it in memory, and passes it through the run's private environment file. A token never appears in a start command, the node database, logs or events. Each (account, machine, harness) gets an isolated profile, and Pyre never copies login files between machines or edits your own `~/.claude` / `~/.codex` login.
- **Isolation from your setup.** Sessions run in Pyre-owned tmux servers on private sockets, never your default tmux server.
- **Approvals.** With the default `--permissions ask`, harness approval prompts are relayed to you. An answer is bound to the exact request and run that asked for it, so a stale or replayed answer is refused. `--permissions bypass` turns that off for a node or a launch. Treat it as giving the agent your user account.
- **Artifact previews.** HTML artifacts can be served from a separate preview origin (`pyre-hub serve --preview-listen`). The Android app renders them in a locked-down WebView.
- **Releases.** Release tarballs are reproducible and signed with an offline Ed25519 key (`ssh-keygen -Y`, namespace `pyre-release`). The installer verifies the signature and pins the key before it extracts anything. The public key is in [`packaging/release-allowed-signers`](packaging/release-allowed-signers).

## Out of scope

- Anyone who already runs code as your user on a machine with a Pyre node or client. They can read your credential files and drive your agents.
- What an agent does with the permissions you give it, especially under `--permissions bypass`.
- Exposing the hub port to the public internet. It is designed for a LAN or VPN. Bind it to a private address (`pyre-hub serve --listen`).

More detail: [docs/architecture.md](docs/architecture.md) and the [operations runbook](docs/operations.md).
