# Architecture

`outfit` is one Go binary with five faces:

1. searchable terminal workbench and scriptable CLI;
2. optional loopback browser administration;
3. stdio MCP server for coding agents;
4. Codex and Claude Code lifecycle-hook adapter;
5. connector package compiler.

All faces use `internal/application` over the same connection, toolchain, skill, backup, state, runtime, and vault services. No daemon is required. SQLite stores connector packages, connection and credential metadata, one selected mode/method, tool discovery, resource context, receipts, local audit, and schema migrations. It stores opaque secret references, never provider secrets.

## Execution modes

A connector may offer HTTP, CLI, and MCP modes, but a connection selects exactly one. Candidate mode configuration is verified before an atomic switch. Failed setup does not disturb the active mode; a successful switch removes superseded credentials after commit.

- HTTP is fixed-origin and declarative.
- CLI executes trusted argv templates without a shell.
- MCP proxies a fixed stdio or Streamable HTTP server and preserves tool annotations.

## Credential methods and bindings

Execution mode chooses the operation surface; credential method chooses custody. A declarative binding maps that method to process environment, stdin, a `0600` temporary file, a temporary config directory, an HTTP header/query, or MCP authorization. Child processes receive an explicit environment and run directly without a shell. GWS uses this generic boundary to isolate native config and competing Google credential sources.

Connections reference first-class non-secret credential records. Several explicitly linked connections may reuse one record; the vault value is destroyed only after its final consumer is removed. Candidate replacement is verified before an atomic pointer switch, and failed cleanup retains a durable retry record.

## Managed tools and skills

`internal/toolchain` downloads fixed GitHub Release or HTTPS assets under `OUTFIT_HOME`, verifies the declared SHA-256, extracts only the intended binary, runs its version command, and atomically selects it. Runtime resolves the absolute managed path rather than silently using `PATH`.

`internal/skillmanager` composes generated connection skills with pinned upstream sources. It validates source digests and frontmatter, filters paths by the connection's granted capabilities and workspace operation policy, stages the full result, then activates it for Codex and Claude. Tool and skill provenance is visible in the TUI and CLI.

`internal/hostmcp` is the narrow migration boundary for MCP OAuth already held by Claude Code or Codex. Discovery returns safe metadata while keeping tokens in-memory only. Adoption refreshes and verifies through the ordinary connection/runtime path, persists any rotated refresh token back to its owning host record before further work, commits one `outfit` connection, rechecks the host grant for concurrent changes, then backs up and disables/removes the duplicate exact endpoint. Host mutation failure rolls the new connection back.

## Portable backups

Portable `.outfit` files use Argon2id and AES-256-GCM. The encrypted payload contains named connections, credential metadata and values, connector packages, tool pins, and skill preferences. Restore decrypts and validates before mutation, recreates candidates through the normal verified setup service, reinstalls platform-specific tools, regenerates skills, and rolls back completed connections after a later failure.

## Plugin shape

The repository is a marketplace for both hosts. `plugins/outfit` contains:

- `.codex-plugin/plugin.json`;
- `.claude-plugin/plugin.json`;
- `.mcp.json` pointing to `outfit-core mcp`;
- `hooks/hooks.json` for session and prompt context;
- a small stable `using-outfit` skill.

Enabled connector skills are generated before a session into the host's personal skill directory. SessionStart injects a compact live index; it does not mutate the plugin registry and pretend the active session discovered new skills.

## Repository workspaces

`outfit` walks from the current directory to the filesystem root and uses the nearest `.outfit/workspace.yaml`. Its lock pins package digests, one selected mode, the exact operation allowlist, active profile, skills, and link rules. Both files are safe to commit because they contain no credentials.

The files are inert until `outfit apply` records a local approval over both exact digests. Any manifest or lock edit invalidates that approval. Runtime startup also verifies installed package digests and connection modes against the lock. The same policy filters the CLI, local MCP facade, generated skills, SessionStart context, automatic link resolution, and local admin inventory.

## Exact execution plans

Runtime execution begins by building one validated plan. The plan expands fixed destinations and arguments, enforces required input, determines the executor and operation class, and substitutes named redaction markers for credential fields. `outfit explain` and `--dry-run` expose that same plan and stop before vault access or network, process, or MCP I/O.

## One invocation contract

Connector Kit compiles every operation into one effective JSON Schema. Explicit author schemas win; fixed HTTP paths, query/body templates, CLI argv/stdin templates, optional arguments, and pagination cursors contribute missing properties deterministically. That single contract drives:

- concise commands such as `outfit slack read-thread --channel … --ts …`;
- canonical commands such as `outfit run slack slack_read_thread --arg …`;
- JSON/file/stdin input for scripts;
- the compact read-only MCP search, describe, and dispatch facade;
- generated connector skills and authored workflow recipes;
- validation, dry-run planning, and runtime argument substitution.

The typed CLI is the blessed agent interface. The shared invocation service emits `outfit.jhh3.dev/result/v1` envelopes for CLI JSON and compact MCP structured content. Read operations can follow an explicit Connector Kit cursor contract or recognized standard MCP/GraphQL/REST cursor shapes. Pages stay separate rather than being silently reshaped, and page/item caps are enforced centrally. `outfit get URL` and `outfit_get_url` likewise share the selected-mode link resolver and heavy/light context policy. MCP intentionally exposes no mutation dispatcher or per-operation tools.

## Local knowledge cache

SQLite FTS5 indexes cached resource title and body while retaining connector, connection, operation, arguments, source URL, fetch time, expiry, and provenance. Ordinary operation caching and FTS search are opt-in. Expired evidence is excluded by default; reading stale evidence requires an explicit flag. `outfit cache purge --all` immediately deletes every cached resource and its FTS entry. Provider credentials are never cache fields.
