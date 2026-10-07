# Getting started

## 1. Install

Authenticate GitHub CLI for the private repository, then install the latest release:

```bash
gh auth status
gh api repos/jhh3/outfit/contents/scripts/install.sh -H 'Accept: application/vnd.github.raw+json' | sh
outfit setup
```

The installer places the same signed release binary at `~/.local/bin/outfit-core` and exposes `outfit` as a symlink. `outfit-core` is the stable executable name used by host plugin manifests.

## 2. Open the terminal workbench

Run `outfit` in an interactive terminal. Search by provider, account, use case, command, operation, CLI, or skill. Setup is presented as complete recipes so you choose a meaningful outcome such as **Use existing gws login**, **outfit-managed OAuth**, or **Import hosted MCP from Claude + Codex**, rather than separately manipulating internal mode and credential records.

```bash
outfit
```

The workbench has four destinations: **Home**, **Connectors**, **Projects**, and **System**. Home is an attention queue. Connectors groups accounts, credentials, execution, operations, CLIs, and skills under each provider. Projects carries repository policy. System carries updates, global CLI/skill inventory, backups, and activity. **Connected**, **detected**, **ready to import**, **checking**, **needs attention**, and **not configured** remain distinct states. Environment variables and host credentials are only discovery hints until you choose a recipe and verification succeeds. A connection has one selected mode and one selected credential method. A failed verification keeps the form and previous working connection intact.

An importable hosted-MCP account appears directly in that provider's **Ways to connect** list. It verifies the refresh grant and live MCP tool surface under `outfit` first. Only after success does it back up the host files, remove the old host credential, and disable the duplicate endpoint in each source host:

```bash
outfit connector imports
outfit connector adopt-mcp IMPORT_ID --name linear
```

Connector packages can declare conventional environment sources for individual credential fields. `outfit` shows the variable name but never its value, preloads it into the masked setup form, and still requires the normal explicit connect-and-verify step. A provider's account detail distinguishes a global binary found on `PATH` from one installed or deliberately adopted by `outfit`; the complete inventory lives under System → CLIs.

The first screen paints from local state immediately while authenticated CLI and host-MCP discovery continues in the background. Press `:` for navigation and global actions, `?` for contextual keyboard help, `/` to search the current workbench, and `r` to re-run discovery.

The same setup is scriptable:

```bash
outfit connector catalog
outfit connector show github
outfit connector add github --mode cli --name github-work
```

For a fresh managed reference, connect Stripe or GWS. `outfit` downloads the exact checksum-pinned CLI automatically:

```bash
outfit connector add stripe --name stripe-test --mode cli --credential-method managed-restricted-key
outfit connector add google-workspace --name workspace-me --mode cli \
	--credential-method outfit-oauth --set GWS_SERVICES=gmail,calendar
```

For one human account, native `gws` login is recommended: Outfit installs the pinned CLI, reuses an authenticated profile, or runs `gws auth setup --login` when setup is missing. Outfit-managed browser OAuth remains available when you want Outfit to hold the refresh grant and mint a short-lived token for each invocation; exported OAuth credentials, a pre-obtained access token, and service-account JSON are separate alternatives. Managed methods isolate GWS configuration for every child invocation. Only skills matching `GWS_SERVICES` are installed.

## 3. Install the coding-agent plugin

```bash
outfit plugin install --host both
outfit plugin sync --host both
```

Restart Codex or Claude Code. The plugin starts `outfit-core mcp`, adds compact active-connection context, and attempts bounded read-only enrichment for supported URLs in user prompts.

The session context is a compact `outfit ls`: each active account's provider, selected mode, callable operation count, and automatic-link depth. Connector skills are synchronized at the same boundary. Link enrichment defaults to `heavy`: outfit fetches the richest bounded read payload, performs any connector-declared follow-up reads, and injects that evidence before the agent starts thinking. In the workbench, open a connected account and press `l` to cycle `heavy → light → off` without disconnecting it, or use `outfit context mode CONNECTION heavy|light|off`. Light keeps compact metadata and an excerpt. Repository policy may further narrow enrichment with top-level `links`, connector-level `links`, or `linkRules` in `.outfit/workspace.yaml`.

Inspect or change the managed artifacts at any time without reconnecting:

```bash
outfit credential list
outfit tool status
outfit skill list
outfit skill update --host both
```

## 4. Author another connector

```bash
outfit kit new acme --kind http # or mcp / cli
cd acme
outfit kit validate .
outfit kit test .
outfit connector install . --trust
```

MCP packages discover their tools live. HTTP and CLI packages declare typed operations. All three compile into the same typed CLI, skills, link context, health, and admin surfaces. The local plugin keeps only a compact read-only MCP discovery/dispatch fallback instead of registering every operation as a host tool.

Once connected, each account is also a short typed command namespace. The following are equivalent and share the exact same Connector Kit schema and runtime:

```bash
outfit github-work get-pull-request --owner acme --repo app --number 42
outfit run github-work get_pull_request --owner acme --repo app --number 42
outfit run github-work get_pull_request --json-args '{"owner":"acme","repo":"app","number":42}'
```

Coding agents should run the same typed CLI command in a shell. Because harness shell output is captured, agent-facing commands automatically return the stable JSON envelope without an extra flag. Large results return a `meta.spill.path`; read that private JSON file for the complete result. When shell execution is unavailable, `outfit_run` can dispatch a read by connection and canonical operation name. `outfit get URL` / MCP `outfit_get_url` reads a supported pasted link through the currently selected connection mode, and `outfit workflow list` exposes connector-authored recipes without inventing another execution path.

## 5. Make the repository reproducible

```bash
outfit profile infer --write # or outfit workspace init and edit it yourself
outfit lock
git add .outfit/workspace.yaml .outfit/workspace.lock.yaml
outfit apply
```

Inference is a proposal, not authority. It reads a bounded set of local Git, dependency, config, and documentation signals and only suggests already-active local connections. Review the manifest and lock. `outfit apply` is the explicit local approval; it prompts for missing credentials and stores them outside the repository.

## 6. Inspect before execution

```bash
outfit github-work get-pull-request --help
outfit explain github-work get_pull_request --arg owner=acme --arg repo=app --arg number=42
outfit github-work get-pull-request --owner acme --repo app --number 42 --dry-run --json
```

Both show the exact executor plan and redacted credential boundary without opening the vault or calling the provider.

## 7. Opt into local resource recall

```bash
outfit cache enable --ttl 24h
outfit github-work get-pull-request --owner acme --repo app --number 42
outfit search --cached 'pagination bug'
outfit context inspect res_...
```

Only successful reads are captured. Search excludes expired entries unless requested, and every item retains its connector, operation, arguments, source, timestamps, and provenance.

## 8. Move to another machine

```bash
outfit backup create ~/Downloads/outfit-backup.outfit
# copy the encrypted file to the new machine
outfit setup
outfit backup restore ~/Downloads/outfit-backup.outfit
outfit doctor
```

The backup contains encrypted credential values and connection metadata, not downloaded binaries. Restore validates the complete archive, reinstalls the correct platform artifacts, verifies each account, and regenerates selected skills.
