# integrations/rota — read-only Rota inventory

Pyre keeps [Rota](https://github.com/jhh3/rota) working as it is today and reads its inventory
for display and account mapping. Pyre-managed sessions never run on a Rota login. Evidence is in
the accounts report §4 and decision I08. The node side
of account management (profiles, grants, selection, usage, fallback) is in
[`internal/accounts`](../../internal/accounts).

## What this adapter does

| Function | Reads | Never does |
|---|---|---|
| `Detect` | Resolves `Options.RotaPath`, else the rota bundled with this Pyre install (`install.sh --with-tools`, `<prefix>/tools/rota/bin/rota`), else `rota` on PATH, reads the script file to fingerprint it (`sha256` revision, optional `__version__`). With `RunHelp`, runs **`rota --help`** and nothing else. | Run any other rota command |
| `Scan` | Directory listings only: `~/.claude/accounts/*.json` file **names** (dotfiles excluded) and `$CODEX_HOME/accounts/<name>/` directory names that contain an `auth.json` (checked with `lstat`) | Open a snapshot file. Each snapshot is a live login (tokens). |
| `ReadUsageCache` | `~/.claude/accounts/.usage-cache.json`, keeping only utilization windows and `fetchedAt` | Treat a hint as capacity. Hints are always stale, low confidence, eligibility `unknown`, and display-only. |
| `SuggestMappings` / `ApplyMapping` | Matches snapshot names to Pyre accounts by `rota_ref` (confirmed) or by label (suggested; the owner confirms). | Grant access or copy anything. A mapping is only a label. |

**The usage cache contains no credentials.** This was checked against the rota source at
`eb9f458`: `cache_store()` writes `{name: {"fetchedAt": ms, "data": <usage response>}}`.
`data` is the `/api/oauth/usage` response, which holds utilization, reset times and the
extra-usage credit total. Pyre still drops everything except the windows.

**Commands Pyre never runs:** `rota` (no arguments), `rota usage`, `list`, `whoami`,
`switch`, `save`, `add`, `reauth`, `export`, `import`, `remove`, and all `rota codex ...`
commands. The reasons, from the rota source:

- `switch` rewrites the live login. A running default-profile Claude session takes the new
  identity on its next turn (E8).
- `usage` (and plain `rota`) refreshes expired tokens and writes them back, including to the
  live login.
- `list` and `whoami` read the live keychain secret.
- `export` prints secrets.

`--help` was verified in `main()` to print the module docstring and exit. Pyre runs it with an
exact argv, a throwaway `HOME`, a minimal `PATH` and a 5 s timeout.

## Migration path

1. **Nothing changes for the default login.** Rota keeps managing `~/.claude` and `~/.codex`
   (the ambient login). `rota switch` still works for terminals you start by hand.
2. **Pyre-managed sessions use isolated profiles.** Each (account, node, harness) gets
   `~/.local/state/pyre/profiles/<account-id>/<harness>/` with its own fresh device login,
   created by the command `CreateProfile` prints:
   - `CLAUDE_CONFIG_DIR=<dir> claude auth login --claudeai`
   - `CODEX_HOME=<dir> codex -c 'cli_auth_credentials_store="file"' login --device-auth`

   Profiles are never seeded by *copying* a rota snapshot. A copied login logs out whichever
   copy refreshes second (E2/E5).
   **Owner-directed alternative: rota checkout.** Pyre can use the accounts rota already holds
   by *transferring* one into a profile for the duration of use (grant kind `rota-checkout`).
   A lock next to the snapshot and a hub fleet lease make the profile the single owner. Rotated
   tokens are written back into the snapshot, and the credential moves back at check-in. The
   live default account is never checked out. It needs rota's
   `pyre-integration` branch (lock-aware) installed on the node.
3. **Map names.** `Scan` lists rota's snapshot names. `SuggestMappings` proposes links to Pyre
   accounts. When the owner confirms one, `Account.RotaRef` is set to the rota name. The
   provider identity (`Account.ProviderRef`) is bound separately from the profile's own local
   login (`accounts.BindIdentity`), never from a rota snapshot.
4. **Ambient runs stay possible.** A session launched with account policy `ambient` uses the
   node's default (rota-managed) login, is labelled `credential_kind=ambient`, and never gets
   automatic fallback. If you `rota switch` while such a session runs, it changes identity
   on its next turn. Pyre reports ambient runs as unisolated.
5. **Rollback.** Remove `~/.local/state/pyre/profiles/<account>/` after `claude auth logout` /
   `codex logout` *inside that profile*. Rota and the default login were never touched, so
   there is nothing to restore.

## Rota upstream note

Rota's README and docstring said "running sessions keep their old identity" after
`rota switch`. The accounts spike measured the opposite for plaintext credentials (E8). For
the macOS keychain the change is inferred to land within about 30 s. The corrected text is
committed on branch `pyre-integration` in the rota checkout (`pyre-deps/rota`, not pushed).

## Tests

`go test ./integrations/rota/` uses a stand-in `rota` script that leaves a marker file if it
is called with anything other than `--help`. Snapshot files are `chmod 000`, which proves names
come from directory listings only. A synthetic usage cache covers both response formats.
`PYRE_ROTA_REAL=1 go test -run TestRealNodeReadOnly ./integrations/rota/` inventories the real
machine read-only and logs counts only.
