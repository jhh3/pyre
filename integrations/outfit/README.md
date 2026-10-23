# Outfit integration (WP12)

Pyre uses the user's existing [Outfit](https://github.com/jhh3/outfit) as the local authority for connector credentials, managed CLIs, and skills, and adds a fleet catalog of **metadata only**. This note is a sanitized inventory of Outfit (pinned `3c7577f`) plus the Pyre design. No credentials, keychain contents, or Outfit state files were read to write it.

## Observed deployments (names only)

| Host | Outfit | Notes |
|---|---|---|
| Mac (this checkout's host) | not on `PATH` | Tests use the fake CLI and binaries built from the deps clone with an isolated profile. |
| A Linux host | `~/.local/bin/outfit` → `outfit-core`, `outfit 0.10.7` built from a local checkout with uncommitted edits (not on GitHub `origin/main`). They were not touched. State lives in `~/.config/outfit`. `ls --json` exists; `tool status`, `skill list`, and `credential list` print text only. `secret-tool` is **not** installed (Outfit reaches the Secret Service over D-Bus directly). |

## Outfit facts that shape the adapter

**Commands.** `version`, `setup`, `tui`, `ls`, `search`, `describe`, `explain`, `run`/`write`, dynamic `outfit CONNECTION OPERATION`, `get URL`, `exec CONNECTION -- …`, `connector {catalog,show,install,add,remove,test,imports,adopt-mcp}`, `credential {list,test,reuse,replace|rotate|reauthenticate,remove}`, `tool {catalog,status,path,install,update,repair,use,adopt,remove,gc}`, `skill {catalog,list,sync,update,repair,enable,disable,diff,verify-source}`, `backup {create,inspect,restore}`, `workspace …`/`lock`/`apply`, `profile`, `context`, `session context`, `plugin`, `mcp`, `admin`, `doctor`, `log`, `cache`, `workflow`, `kit`.

**Machine-readable output.** `ls`, `search`, `describe`, `doctor`, `get`, and every operation emit `outfit.jhh3.dev/result/v1` (`{schema, ok, connection, connector, mode, operation, class, data, meta{pages,items,truncated,next_cursor,spill}, error{code,message,hint,correction,retryable}}`) when stdout is not a TTY, with `--json`, or with `OUTFIT_AGENT=1`. Results larger than `OUTFIT_AGENT_MAX_BYTES` (default 64 KiB, range 4 KiB–10 MiB) are spilled to a private `0600` temp file named by `meta.spill`. `tool status`, `skill list`, `credential list`, and `connector catalog` are text only. `ls --json` carries only name, connector, display, mode, status, and operation count.

**Side effects to avoid.** Every command that opens state runs schema migrations and upserts the built-in connector packages. `setup` and `doctor` also sweep pending secret deletions, so Pyre never calls them. A bare `outfit --help` (or any unknown first word) opens state to register dynamic connection commands. `connector test` records a verification timestamp; Pyre accepts that as Outfit's own bookkeeping. Outfit applies the nearest `.outfit/workspace.yaml` policy and refuses to run while one is unapproved, so Pyre runs it from `os.TempDir()`.

**Credential custody.** Secrets live in the OS credential store through `zalando/go-keyring`: service `dev.jhh3.outfit` (legacy `dev.jhh3.cw`), account a random `outfitsec_<hex>`, and the value a JSON map of fields. That means the macOS Keychain, or the Secret Service over D-Bus on Linux. `OUTFIT_VAULT_BACKEND=file` selects an AES-GCM file vault whose master key sits beside it; `outfit vault use file` records that choice in `vault/backend`, so later runs without the variable store new secrets there too; reads find a secret under either backend. SQLite holds only opaque refs. Credential methods declare a custody of `outfit`, `oauth` (Outfit-managed tokens), `executor` (a native CLI login such as `gh`), or `none`. Bindings inject a secret only into the child process: `process-env`, `stdin`, `temporary-file`, `temporary-directory`, `http-header`, `http-query`, or `mcp-header`. Secret fields may declare familiar environment variable names, such as `GH_TOKEN`. Outfit has no command that reveals a secret, and Pyre does not add one.

**Connections and modes.** A connection has exactly one selected execution mode (`cli`, `http`, or `mcp`) and one credential method. Operations are classed `read`, `write`, `admin`, or `destructive`. Mutations require an explicit human `--write`.

**Files.** `OUTFIT_HOME` defaults to `~/.config/outfit` and migrates a legacy `cw` directory. It holds `outfit.db`, `packages/`, `skills/`, `locks/`, `vault/`, and `tools/`. Repository policy lives in `.outfit/workspace.yaml` and `.outfit/workspace.lock.yaml`; both are secret-free and inert until `outfit apply` approves their digests. Harness skill roots come from `CODEX_HOME` and `CLAUDE_CONFIG_DIR`, falling back to `HOME`.

## What Pyre calls

| Purpose | Command | Notes |
|---|---|---|
| Detect | `outfit version` | Does not open state. Parses version and revision; `+working` marks a dirty build. |
| List connections | `outfit inventory --json`, falling back to `ls --json` | `inventory` is the proposed upstream change below. Older Outfit reports `unknown command`; the adapter remembers this and uses `ls`. |
| Check availability | `outfit connector test NAME` | Classified as `verified`, `missing`, `expired`, `locked_store`, or `unknown`, with redacted, bounded detail. |
| Invoke (reads) | `outfit run NAME OP --input - --json` | Args go as JSON on stdin, never in argv. `OUTFIT_AGENT=1`, spill bound ≤ `MaxOutput`. Spills are replaced by a marker and the temp file is removed. A timeout returns `outcome_indeterminate`. |
| Tool/skill inventory (WP14) | `inventory --json`, falling back to `tool status` + `skill list --host both` | Returned as `contracts.ToolVersion` and `contracts.ExtensionRef`. |

Every call has a timeout and an output cap, and runs in its own process group (killed on timeout). The child gets a minimal environment: `HOME`, `PATH`, `XDG_*`, the D-Bus session, `CODEX_HOME`/`CLAUDE_CONFIG_DIR`, on Windows the system and profile variables (`APPDATA`, `LOCALAPPDATA`, `USERPROFILE`, `SystemRoot`, `TEMP`/`TMP`, `PATHEXT`, `ProgramData`, `HOMEDRIVE`/`HOMEPATH`, ...), and, from pyred's environment, only `OUTFIT_HOME` and `OUTFIT_VAULT_BACKEND` (an allowlist). It never receives `PYRE_*` session tokens. Connection and operation names are validated so they cannot pose as flags.

## Pyre design

- **IDs.** `ConnectionIDFor(name) = "conn_" + name`, with `_` mapped to `:`. Every node and the hub derive the same ID without coordinating. One name means one logical connection, with availability tracked per node.
- **Modes.** A connection Outfit holds is `remote_invoke`: Outfit executes the call on the node that holds the credential, and the caller gets the result envelope. `local_credential` exists only where Pyre itself provisioned a value into the node's OS store. The session then receives an environment variable *name* plus a store *ref*, and the launch wrapper reads the value into `ResolvedLaunch.Env`.
- **Catalog (hub).** Lives in `internal/connections` (sqlitex namespace `connections`). It holds metadata, per-node availability from reports and checks, and grant tuples `(node?, project?, session?)` with granted and revoked generations. The empty tuple is an explicit owner-broad grant. Only owner or system principals can mutate. Revoking never broadens access.
- **Use-time recheck.** `connection.invoke` and `PrepareLaunch` call a `GrantChecker` on every use. On the hub this is `Catalog.Checker(sessionLookup)`; a node's checker asks the hub. Errors deny. A node-local mirror of pushed revocations adds an immediate local deny.
- **Stores** (`internal/credentials`):
  - macOS Keychain via `/usr/bin/security`. Service `pyre:<connection_id>`, account `<ENV_NAME>`. Values reach `security -i` only on stdin, base64-encoded. A measured line limit of about 4 KiB caps values at 2048 bytes.
  - Linux Secret Service via `secret-tool`, using stdin and stdout.
  - An explicit opt-in `0600` file store that refuses broader permissions.
  - Locked or unavailable stores return actionable errors. Pyre never falls back to plaintext.
- **Cross-node provisioning** is explicit and owner-initiated:
  1. The recipient creates an X25519 offer bound to the connection, variable, ref, and expiry.
  2. The source seals the value (HKDF-SHA256 + AES-256-GCM).
  3. The recipient accepts it once.

  Only ciphertext crosses the hub, so it is safe for the hub to persist it inside node commands.

## Proposed upstream Outfit change (prepared, not pushed)

Deps clone `pyre-deps/outfit` (a sibling checkout of this repository), branch `pyre-integration`, commit `8a0dbcc` on top of `3c7577f`:

- `outfit inventory [--json]` prints `outfit.jhh3.dev/inventory/v1`. It covers:
  - **Connections:** id, name, connector, mode, executor, credential method, custody, status, credential status, `last_verified_at`, declared environment variable **names**, capabilities, and operation count.
  - **Managed CLI receipts:** id, version, platform, path, sha256, and current/adopted flags.
  - **Installed skills.**

  It excludes values, vault refs, fingerprints, and account identities, and it honors workspace policy like `ls`.
- The change is `internal/app/inventory.go` plus `inventory_test.go`, a docs paragraph, and a **one-line** registration in `root.go`. Keeping `root.go` to one line limits conflicts with locally modified `root.go` checkouts. Reconcile it on such a host after the local edits are committed or dropped.
- Verification:
  - The new test passes. It puts a synthetic secret in the file vault and asserts the secret, the vault ref, the fingerprint, and the account name are all absent from the output.
  - `go vet` is clean.
  - The full `go test -race ./...` passed except `internal/toolchain TestInstallVerifiesAndAtomicallySelectsManagedTool`, which this change does not touch. That test fails only under parallel `-race` load and passes on rerun.
- Pyre's gated live test (`PYRE_TEST_REAL_OUTFIT`) passes against both the pinned build (no `inventory`, so it uses the `ls` fallback) and this branch. Both runs use a fully isolated `HOME`/`OUTFIT_HOME`/`CODEX_HOME`/`CLAUDE_CONFIG_DIR` and a file vault.

Further proposals, not implemented:

- An opt-in way for `connection.invoke` to request a write that carries recorded human intent. Today Outfit returns `write_confirmation_required`, which Pyre maps to `forbidden`.
- A read-only state open for `inventory` and `ls` that skips migrations and the built-in package upsert.

## Limitations

- An env-injected `local_credential` cannot be withdrawn from a process that is already running. Revocation applies to the next launch or invoke. Prefer `remote_invoke` when access must be revocable.
- With an Outfit that lacks `inventory`, availability stays `unknown` until `connection.check` runs (`pyre connections verify`). The node keeps the last check per connection, overlays it on its reports and pushes a report at once; the hub also records the check result. Routing requires `verified`.
- `connection.invoke` sent without `node_id` is routed by the hub (grant check, then the verified `remote_invoke` holder, preferring the session's node). The executing node re-checks the grant at use; a node may check grants for remote sessions only for connections it reports holding.
- Pyre can only redact credential *patterns* in Outfit diagnostics, because it does not know Outfit-held values. Outfit's own non-echo guarantee is the primary control.
- The Secret Service path needs `secret-tool`, which some Linux hosts lack. A D-Bus client would add a dependency, which needs owner approval.
