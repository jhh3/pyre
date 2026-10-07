# Threat model

## Protected assets

- provider credentials and OAuth refresh material;
- local connection identity and selected mode;
- tool policy and approval class;
- private provider content returned by calls or link enrichment;
- local project/profile boundaries.

## Controls

- Secrets are stored in the OS credential store under random opaque account IDs.
- Managed child processes receive only the selected connection's declared bindings. Owned inherited variables are removed, argv never passes through a shell, and temporary credential files/config directories use private permissions and are deleted after the child exits.
- Provider binaries and upstream skill archives are pinned to immutable releases/commits and exact SHA-256 digests before activation.
- Portable backups derive an encryption key with Argon2id and authenticate all encrypted connection/credential data with AES-256-GCM. A wrong passphrase or modified file is rejected before state mutation.
- SQLite, logs, skills, plugin files, fixtures, argv, and exported profiles contain no secret values. MCP adoption preserves the pre-existing Claude/Codex files in a private `0700` backup directory with `0600` files before removing host custody; that backup has the same sensitivity as the original host credential store.
- HTTP and remote MCP origins are fixed; redirects remain same-origin; DNS is pinned and private address space is blocked unless explicitly opted in.
- Custom CLI packages require explicit trust and use direct argv execution without a shell.
- Operations are classified `read`, `write`, `admin`, or `destructive`; automatic context can call only reads.
- Provider text and discovered MCP descriptions are untrusted. Authored policy wins and rendered context is escaped inside a balanced trust frame.
- Link extraction is bounded by URL count, resource count, deadline, body bytes, and cache policy.
- Project workspace policy can only narrow locally enabled connections.
- Workspace manifests and locks contain no secrets, are inert until explicitly approved locally, and lose approval on any byte change.
- Dry-run and explain paths construct redacted plans without reading the vault or performing provider I/O.
- Searchable read-result caching is opt-in, local-only, TTL-bounded, and records provenance; it never stores provider credentials.
- Custom package updates show a semantic capability diff and require explicit `--approve-update`; untrusted packages can be inspected but cannot create a connection.

## Explicit non-goals

`outfit` cannot protect a secret from a compromised user account or kernel. It does prevent accidental disclosure through files, source control, command history, logs, generated agent context, and ordinary plugin operation.
