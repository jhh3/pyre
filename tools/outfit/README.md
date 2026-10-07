# Outfit

> Part of [Pyre](../../README.md), in `tools/outfit`. Optional: Pyre works with or without it, and with a separately installed copy. Install the bundled copy with `scripts/install/install.sh --with-tools` (an existing install is detected and left alone), or build it with `make tools`. It stays its own Go module (`github.com/jhh3/outfit`); from the Pyre root, `go -C tools/outfit build ./cmd/outfit` or `go -C tools/outfit test ./...`.

Outfit is a local-first connector runtime for people and coding agents. Its `outfit` command gives Codex and Claude Code one coherent way to use local connector credentials, tools, generated skills, and automatic pasted-link context without a hosted account or control plane.

Outfit is under active construction. The primary first-run experience is:

```bash
outfit setup
outfit                         # searchable terminal workbench
outfit plugin install --host both
```

Or install a standalone Outfit release with GitHub CLI authentication:

```bash
gh api repos/jhh3/outfit/contents/scripts/install.sh -H 'Accept: application/vnd.github.raw+json' | sh
outfit setup
outfit
outfit plugin install --host both
```

`outfit setup` reports four different states plainly: installed connector packages, active connections, recognized environment credentials, and CLIs found on `PATH`. Detection is never labeled as a connection. When a recommended CLI such as `gh` or `gws` is already authenticated, the workbench offers **Use existing login** as the first setup recipe and imports that verified account without opening another authorization flow or copying its secret. From the workbench, choose a provider and one complete setup recipe—execution mode plus compatible authentication—and finish one verified transaction. `outfit` installs or adopts the tested provider CLI when needed, stores managed credentials in the OS keychain, activates one mode, and synchronizes only the applicable Codex and Claude skills.

Existing MCP OAuth grants from Claude Code or Codex appear as setup recipes inside their provider, alongside native CLI and outfit-managed options. A transfer refreshes the grant, verifies the hosted MCP and discovers its tools in `outfit`, backs up the host files, then removes the old host credential and disables that exact duplicate MCP registration. If the provider rotates a refresh token during preflight, `outfit` first preserves the rotated value in its owning host record so a later verification failure leaves that host usable; it never disables or removes the old registration before success. The same flow is scriptable:

```bash
outfit connector imports
outfit connector adopt-mcp IMPORT_ID --name linear
```

Restart affected Claude Code or Codex sessions after a successful transfer; an already-running session may keep its existing MCP transport open until exit.

Built-in connector fields also declare familiar environment sources such as `GH_TOKEN`, `INTERCOM_ACCESS_TOKEN`, `SENTRY_ACCESS_TOKEN`, and `STRIPE_API_KEY`. The setup form preloads matching values behind a password mask, and the CLI reports only the variable name. You still choose the mode and explicitly connect before `outfit` stores or uses anything.

Stripe and Google Workspace are the complete managed references:

```bash
outfit connector add stripe --name stripe-test --mode cli \
  --credential-method managed-restricted-key

outfit connector add google-workspace --name workspace-me --mode cli \
	--credential-method native-login \
	--set GWS_SERVICES=gmail,drive,calendar
```

The TUI has four stable destinations: **Home** for health and next actions, **Connectors** for provider accounts and setup recipes, **Projects** for workspace access, and **System** for updates, global CLI/skill inventory, backups, and activity. Credentials, runtime, operations, and skills live under the account they serve instead of appearing as unrelated database tables. Press `:` for the command palette, `?` for contextual help, and `/` to search providers, account identities, operations, CLIs, and skills. The same actions remain scriptable:

```bash
outfit credential list
outfit tool catalog
outfit tool status
outfit skill list
outfit skill sync --host both
```

Then, in either harness:

```bash
outfit ls
outfit search 'pull request'
outfit github get-pull-request --owner jhh3 --repo outfit --number 1
outfit get 'https://github.com/jhh3/outfit/pull/1'
```

Every selected connection becomes a concise typed namespace. Agents and people can use schema flags, `KEY=value`, repeated `--arg`, a JSON object, a file, or stdin; the canonical generic form remains available for portable scripts:

```bash
outfit slack read-thread --channel C123 --ts 1712345678.123456
outfit github get-pull-request --json-args '{"owner":"jhh3","repo":"outfit","number":1}' --json
printf '%s' '{"owner":"jhh3","repo":"outfit","number":1}' | outfit run github get_pull_request --input - --json
outfit stripe customers-list --all --max-items 500 --json
```

Captured/non-TTY calls to `ls`, `search`, `describe`, `doctor`, `get`, and connector operations automatically return `outfit.jhh3.dev/result/v1`; interactive terminals stay human-readable, and `--json` forces the envelope explicitly. It includes connection/mode provenance, page and item counts, truncation, and structured failures. Results larger than 64 KiB are written to a private `0600` temp JSON file and replaced inline by `meta.spill` with its path, byte count, and SHA-256 digest. Set `OUTFIT_AGENT=0` to keep human text in a pipeline or `OUTFIT_AGENT_MAX_BYTES` to tune the spill boundary. Reads can follow a connector-declared or standard provider cursor with `--all`; bounded defaults prevent accidental unbounded context. Mutations remain explicit with `outfit CONNECTION OPERATION --write` or `outfit write CONNECTION OPERATION`.

The typed CLI is the primary agent interface. The local MCP facade stays intentionally small—`outfit_search`, `outfit_describe`, `outfit_run`, and `outfit_get_url`—so installing connectors does not flood an agent's tool list. The MCP dispatcher is read-only; mutations always use an explicit CLI `--write`. Connector-authored recipes point to the same preferred CLI entry points:

```bash
outfit workflow list
outfit workflow show slack read-a-linked-discussion
```

## Core invariants

- One declarative connector package drives setup, auth, selected mode, operations, skills, link enrichment, health, and conformance.
- A named connection has exactly one selected execution mode.
- `outfit`-managed secrets live in the OS credential store and are never echoed through `outfit`, the admin UI, generated skills, plugin files, or logs. MCP adoption makes a private `0700`/`0600` backup of the pre-existing host files before removing host custody.
- Managed provider CLIs live below `OUTFIT_HOME`, are checksum-verified, and never require a global package manager or `sudo`.
- Encrypted portable backups move selected connections and credentials between machines; repository workspaces remain secret-free.
- The typed local CLI is primary. The compact read-only MCP fallback calls the same runtime and policy implementation without duplicating every operation into the host tool list.
- Automatic link context defaults to heavy, remains read-only/bounded/fail-open, and is framed as untrusted external data; each connection can switch to light or off.
- Project workspace policy may narrow access but never manufacture a credential.
- A repository workspace is secret-free, digest-locked, and inert until each developer explicitly applies it locally.

See [the agent interface](docs/agent-interface.md), [the architecture](docs/architecture.md), [repository workspaces](docs/workspaces.md), [connector authoring](docs/connector-authoring.md), and [the threat model](docs/threat-model.md).

## Daily use

```bash
outfit connector catalog
outfit connector show github
outfit connector add github --mode cli --name github-work
outfit connector test github-work
outfit ls
outfit search 'pull request'
outfit describe github-work get_pull_request
outfit run github-work get_pull_request --help
outfit github-work get-pull-request --owner acme --repo app --number 42 --dry-run
outfit github-work get-pull-request --owner acme --repo app --number 42 --json
outfit get https://github.com/acme/app/pull/42
outfit log
```

`outfit admin` remains an optional loopback browser renderer. The terminal workbench is the primary interactive surface and both call the same connection/runtime services.

## Managed credentials, tools, and skills

```bash
outfit credential test CREDENTIAL_ID
outfit credential replace CONNECTION
outfit credential reauthenticate CONNECTION
outfit credential reuse CREDENTIAL_ID NEW_CONNECTION
outfit tool install gws
outfit tool update stripe-cli
outfit skill disable CONNECTION SOURCE PATH --host codex
outfit skill repair --host both
```

Managed GWS invocation uses an isolated temporary config directory and clears competing inherited Google credential sources. A Gmail-only connection installs `gws-shared` and Gmail skills for each selected harness, never Drive, Admin, or unrelated service skills. Raw provider commands still run through the selected connection boundary with `outfit exec CONNECTION -- ...`.

Create and restore an encrypted portable backup:

```bash
outfit backup create ~/Downloads/outfit-backup.outfit
outfit backup inspect ~/Downloads/outfit-backup.outfit
outfit backup restore ~/Downloads/outfit-backup.outfit
outfit doctor
```

See [managed connectors](docs/managed-connectors.md) for the full Stripe/GWS walkthrough, TUI controls, credential model, tool management, selective skills, and backup behavior.

For a reproducible repository environment:

```bash
outfit profile infer --write # or: outfit workspace init, then edit
outfit lock
git add .outfit/workspace.yaml .outfit/workspace.lock.yaml
outfit apply # prompts locally for any missing credential
```

The secret-free workspace declares connections, selected modes, operation allowlists, profiles, generated skills, and link rules. Changing either checked-in file invalidates the one-time local approval. The approved policy narrows CLI operations, compact MCP dispatch eligibility, generated skills, session context, and link enrichment together.

Connector Kit can import common provider contracts:

```bash
outfit kit import openapi ./openapi.yaml --key acme-api --auth-field TOKEN
outfit kit import postman ./collection.json --key acme-api --base-url https://api.acme.test/
outfit kit import cli ./acme-help.txt --key acme-cli --binary acme
outfit kit import mcp https://mcp.acme.test/mcp --key acme-mcp --auth-field TOKEN
outfit kit import graphql https://api.acme.test/graphql --key acme-graphql --auth-field TOKEN
```

Discovery credentials are never written into generated files. Every generated package passes the ordinary Connector Kit validator and still requires human review and trust.

Successful read results can optionally become a local, provenance-bearing knowledge cache:

```bash
outfit cache enable --ttl 24h
outfit search --cached 'database timeout'
outfit context inspect res_...
outfit cache purge --all
```

## Development

```bash
go test ./...
go test -race ./...
go vet ./...
go build ./cmd/outfit
scripts/accept-managed-connectors.sh
```

## License

MIT, as part of Pyre (see [LICENSE](../../LICENSE)). Third-party Go modules linked into `outfit` and their licenses are listed in Pyre's [NOTICE](../../NOTICE).
