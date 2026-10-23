# Changelog

## Unreleased

- Stripe connector 2.1.0 (matches jhh3/outfit#12): every typed read takes `--live` (no `--write`); native login passes `--live` to the CLI, a managed-key connection stores an optional `STRIPE_LIVE_API_KEY` (environment only, verified at setup), and `--live` without one fails instead of reading test data. `outfit exec stripe -- get <path>` and `<resource> retrieve|list|search` are reads with or without `--live`; `post`, `delete`, `create`, `update`, `cancel`, `-d`/`--data`, `trigger`, `listen`, host/key/profile flags, and anything unknown need `--write`. The Stripe CLI's `{"error": ...}` output (exit 0) is now `ok: false` with the upstream status and the new `error.provider_code`. Connector Kit: `whenArgument` / `unlessArgument` credential bindings and CLI executor `errorField` / `errorStatus`.
- Connector validation no longer rejects a fixed HTTP `method` outside GET/POST/PUT/PATCH/DELETE, so OpenAPI-imported HEAD and OPTIONS operations validate again; only argument-chosen methods (`methodArgument` + `methods`) are held to that list (matches jhh3/outfit#10).
- `outfit exec github -- api ...` no longer requires `--write` for reads: a REST GET/HEAD with no fields or body, or a `graphql` call whose only document is a query, is classified as a read (endpoints such as `repos/OWNER/REPO/branches/main/protection` have no typed operation). Only `Accept` and `X-GitHub-Api-Version` headers and relative endpoints qualify; any other method, field, `--input`, header, full URL, or unrecognised flag still needs `--write`, which also closes `api -X DELETE status` passing the verb rule as a read.
- Fleet copies: `backup restore` probes the vault before touching anything, restores each connection on its own (a failure is put back and listed; a vault failure or Ctrl-C stops the rest; exit non-zero), keeps a replaced connection's old secret until the restore commits, handles shared credentials under `--replace`, skips validating built-in packages this machine already has, and no longer trips over removed connection rows. Integer schema checks accept whole JSON numbers, so packages round-tripped through a backup (discord, linear, slack, stripe) validate. `backup create` leaves out connections whose vault secret is missing and exits non-zero unless `--allow-missing`; `--exclude-oauth`; `--passphrase-file` for create and restore. `doctor` reports `credential missing` and the vault backend. Windows managed tool installs find `stripe.exe` / `gws.exe`. The vault reads secrets from either backend; `outfit vault use file|keyring` / `outfit vault status` choose and show where new secrets go.
- Discord connector 1.1.0: native polls (`create_poll`, `poll_voters`, `end_poll`; results stay in `message`), moderation with URL-encoded audit-log reasons (`timeout_member`, `kick_member`, `ban_member`, `unban_member`, `bans`, `add_member_role`, `remove_member_role`, `audit_log`, `bulk_delete`), channel/event/invite/webhook/slash-command management, and `api_get` / `api_call` for any other v10 endpoint on the fixed origin. Skill guidance now skips system messages (only types 0 and 19 are user posts) and requires reasons and owner policy for moderation.
- HTTP connectors can bind an optional argument to a header (`argumentHeaders`, with URL encoding), expand plain-string array items through `itemTemplates`, accept relative times (`relativeTimeArguments`), and opt into a validated raw path below the fixed base URL with argument-chosen query, body, and method (`pathArgument`, `queryArgument`, `bodyArgument`, `methodArgument` + `methods`). Raw paths reject schemes, hosts, `//`, dot segments, queries, fragments, and encoded separators; a read using them must be GET-only.

- Add a built-in Discord connector for the owner's own bot token (never a user token): server, channel, thread, forum, message, pin, member, role, and reaction reads; a composite `activity` snapshot for cheap polling; explicit `--write` replies, threads, forum posts, reactions, pins, and typing with `allowed_mentions` defaulting to nobody; and discord.com message/channel link context.
- HTTP connectors can declare fixed executor headers (such as Discord's mandatory User-Agent), fixed query values, and string-only body arguments; JSON-looking arguments now keep large integers exactly.
- HTTP execution honours 429 responses: it waits the provider's `retry_after`, `Retry-After`, or `X-RateLimit-Reset-After` (at most 30 s) and retries up to three times before reporting the delay.
- Connector Kit supports composite read operations (`compose`) that run several read steps through one credential and project items to compact fields.

## 0.10.7

- Evaluate the exact staged candidate through real Codex and Claude command traces, requiring safe live inspection and literal selected-mode routing instead of scoring only the final JSON answer.
- Add a credential-free matrix for every blessed link-capable execution mode and agent recovery evals for expired OAuth, missing scopes, ambiguity, disabled enrichment, unavailable CLIs, and throttling.
- Execute all 22 declared blessed link resource types through provider-shaped selected-mode fixtures, with a mechanical coverage gate that fails when a new resolver lacks evidence.
- Correct link recovery guidance to use the shipped credential, connector, and catalog commands.

## 0.10.6

- Make live Codex and Claude routing evals resumable with atomic per-trial checkpoints, bounded host retries, preserved attempt history, and explicit timeout, process-exit, malformed-output, and runner diagnostics.
- Exercise GitHub pull-request context, complete Linear issues, Intercom conversation timelines, and Stripe payment failures through provider-shaped selected-mode execution fixtures alongside the existing Slack, Notion, Sentry, and Google Workspace coverage.

## 0.10.5

- Keep the published-release smoke assertion aligned with connection provenance in rendered linked-resource context.

## 0.10.4

- Exercise blessed connectors through provider-shaped execution fixtures, including Slack pagination, recursive Notion content, Sentry evidence, the pinned GWS boundary, expired OAuth, and fresh-install failure states.
- Make local state safe under simultaneous CLI, hook, and MCP startup: serialize first-run migrations across processes, retry bounded SQLite contention, and avoid writes for unchanged built-in packages.
- Compile CLI aliases, examples, defaults, enums, generated skills, SessionStart guidance, and raw `outfit exec` fallback policy from one audited Connector Kit operation contract.
- Audit every built-in URL pattern and selected mode, fail closed on multi-account ambiguity, expose exact recovery diagnostics, preserve connection provenance, and distinguish unsupported short links without following redirects.
- Give opaque GWS operations typed object schemas and provider-shaped examples so agents can form valid Gmail, Drive, Calendar, and Sheets calls without guessing a generic params blob.

## 0.10.3

- Hill-climb real Codex and Claude routing from 75% to 100% on the connector benchmark by making `outfit get URL` the first-class link route across SessionStart context, generated skills, search, and describe.
- Make typed operations outrank raw provider CLI escape hatches, including selectively installed GWS skills, while retaining `outfit exec` for genuinely unlisted capabilities.
- Validate eval commands through Outfit's credential-free planner, add repeated cold-start trials and baseline rescoring, and require exact user task values in proposed commands.
- Accept agent-friendly operation arguments: canonical or friendly operation aliases, camel/snake key spellings, common short aliases, omitted `_id`, and a bare value for a sole required input.

## 0.10.2

- Keep the managed-connector acceptance harness on explicit human output while the release smoke separately verifies automatic agent JSON.

## 0.10.1

- Update the published-release smoke test for automatic agent JSON and verify the explicit human-output override used by shell pipelines.

## 0.10.0

- Automatically return the stable agent envelope for captured `ls`, `search`, `describe`, `doctor`, `get`, and typed operation calls while preserving human-readable interactive output and explicit `OUTFIT_AGENT=0` control.
- Spill oversized CLI and compact-MCP results to integrity-addressed, user-private temp JSON files instead of flooding model context; generated and packaged skills teach agents to consume the receipt.
- Add real Codex/Claude routing evals and a no-regression baseline/candidate scorer for measurable skill and prompt hill climbing.

## 0.9.0

- Make the typed `outfit` CLI the blessed human and agent interface across plugin guidance, SessionStart context, generated connector skills, operation help, workflows, and documentation.
- Replace the per-connection, per-operation MCP tool explosion with four stable tools: `outfit_search`, `outfit_describe`, `outfit_run`, and `outfit_get_url`.
- Keep local MCP deliberately read-only; all connector mutations now require explicit CLI `--write` intent.

## 0.8.2

- Complete native CLI browser handoffs declaratively through Connector Kit: Outfit opens the trusted HTTPS approval URL, runs the returned continuation through the same pinned executable without a shell, and verifies the account before committing it.
- Make Stripe native login work from a fresh terminal or TUI setup instead of discarding Stripe CLI's non-interactive browser continuation.
- Surface the approval URL and verification code as live CLI/TUI setup progress, so the same flow works in a remote or headless terminal where Outfit cannot launch a local browser.
- Preserve, redact, and report provider stdout when a verification command exits unsuccessfully, so failures such as an unauthenticated Stripe profile are actionable instead of blank.

## 0.8.0

- Compile one effective Connector Kit operation schema from authored JSON Schema plus HTTP/CLI templates, so concise CLI commands, canonical CLI commands, typed MCP tools, help, generated skills, workflows, validation, and runtime cannot drift.
- Add per-connection typed namespaces such as `outfit slack read-thread --channel … --ts …` while preserving `outfit run` / `outfit write`, schema flags, `KEY=value`, repeated `--arg`, JSON objects, files, and stdin.
- Add stable `outfit.jhh3.dev/result/v1` JSON and MCP structured-result envelopes with provenance, structured corrective errors, and centrally bounded cursor pagination; declare Slack and Stripe pagination contracts.
- Add `outfit get URL` and the `outfit_get_url` MCP tool for immediate heavy/light selected-mode link retrieval, plus `outfit workflow list/show` for connector-authored recipes with exact CLI and MCP entry points.
- Teach generated and stable plugin skills to prefer typed commands, inspect operation help, use URL retrieval and workflow recipes, and retain all portable fallback invocation forms.

## 0.7.2

- Accept schema-backed operation flags such as `--channel-id` and concise unambiguous aliases such as Slack's `--channel` and `--ts`; operation-level `--help` now renders the exact required flags while `--arg KEY=value` remains portable.
- Persist rotated OAuth access and refresh grants atomically for every consumer, journal superseded vault entries for cleanup, and turn provider `invalid_grant` responses into an actionable `outfit credential reauthenticate CONNECTION` recovery.
- Preserve imported host OAuth callbacks and requested scopes for providers whose OAuth clients are actually transferable; classify Slack's host-owned client-secret grant honestly and recommend the authenticated agent-slack CLI instead.
- Teach generated connector skills to request operation-specific CLI help instead of guessing flags from raw provider schemas.

## 0.7.1

- Add `outfit connector show` with every supported mode, recommendation, executor, credential source, provider setup location, exact host-OAuth import commands, and safe environment detection.
- Repair managed-tool receipts after the `cw` state directory is adopted, so existing pinned tools such as `gws` resolve from their new Outfit path.
- Make fresh setup honest and actionable: native `gh` and `gws` logins can bootstrap themselves, generated-only hosted MCP methods direct users through Claude/Codex import instead of opening an impossible credential form, and the TUI explains every credential field without revealing detected values.
- Reject provider-level Slack and Linear errors even when their APIs return HTTP 200, and correct the blessed connector instructions and verification contracts across GitHub, Google Workspace, Intercom, Linear, Notion, Sentry, Slack, and Stripe.
- Install the Codex plugin from the dedicated `outfit` marketplace identity so retired `cw@personal` configuration cannot block the renamed plugin.

## 0.7.0

- Rename the standalone product and CLI from `cw` to **Outfit**: `outfit` now owns setup, connector execution, skills, link context, plugins, workspaces, backups, and local administration.
- Migrate an existing local `cw` state directory, database, keychain records, backups, and generated skills into Outfit on first use, then remove the retired executables during installation.

## 0.6.0

- Make automatic pasted-link context explicitly configurable per connection as `heavy`, `light`, or `off`, with heavy as the default in the TUI, CLI, session context, and prompt hooks.
- Inject the richest bounded primary provider payload in heavy mode instead of reducing fetched resources to a short excerpt; keep light mode within the original compact prompt budget.
- Add declarative, read-only heavy follow-up reads and bounded recursive traversal to Connector Kit, including recursive Notion blocks, Sentry events, Linear discussion/context, GitHub PR files/discussion and workflow jobs, and a bounded Google Sheets value window.
- Prove the full Notion page body reaches real hook context and keep depth-aware caches from serving a shallow result to a heavy request.

## 0.5.3

- Generate compact, mode-aware connector skills that route agents through `outfit search` and authoritative `outfit describe` schemas instead of copying entire hosted-MCP manuals into context.
- Add connector-specific workflows for every blessed connector while retaining selectively installed upstream GWS service skills.
- Report the complete generated and upstream host-skill inventory after synchronization.

## 0.4.2

- Keep the Codex and Claude plugin manifests aligned with the native-CLI import release.

## 0.4.1

- Detect authenticated native CLI profiles during connector setup and prefer importing them over asking for duplicate credentials.
- Reuse existing `gws` and `gh` authentication without opening another login flow; `gws` native login is now the recommended Workspace credential method.
- Retry transient GWS discovery-schema failures while preserving exact operation-contract validation.

## 0.4.0

- Clear setup inventory that distinguishes connector packages, active connections, detected environment credentials, unmanaged PATH binaries, and transferable host OAuth.
- Transactional Claude Code/Codex MCP adoption: refresh and live-verify OAuth under `outfit`, back up host files, then remove host custody and disable the duplicate exact endpoint, with rollback on failure.
- Declarative credential-field environment sources and blessed hosted-MCP import modes for Linear, Notion, Sentry, and Slack.
- First-class Imports workbench plus CLI discovery/adoption commands and one-key adoption of existing PATH tools.

## 0.3.0

- Searchable Bubble Tea terminal workbench with connector, connection, credential, CLI, skill, update, workspace, backup, and activity views.
- `v1alpha2` connector and workspace contracts separating one active execution mode from one credential method and generic runtime bindings.
- First-class reusable credential records, verified atomic replacement, last-consumer cleanup, OS-keychain custody, and portable Argon2id/AES-GCM backup and restore.
- Checksum-pinned managed CLI installer/updater/adopter/version selector for exact provider binaries under `OUTFIT_HOME`, plus a 25-tool searchable registry.
- Complete Stripe CLI reference with managed restricted-key or native-login custody, focused read/write/stream operations, authored workflows, and environment-aware dashboard links.
- Complete GWS 0.22.5 reference with isolated managed credential/token/service-account and native-login methods, selected service policy, Google link lookup, and immutable upstream skill ingestion.
- Selective GWS skills: each active connection installs only `gws-shared`, selected service skills, and workflows whose full capability set is granted, equivalently for Codex and Claude.
- Live CLI stream output/cancellation, unified application service, tool/skill management commands, and fresh-home real-archive/runtime/skill/backup acceptance harness.

## 0.2.0

- Secret-free repository workspaces with deterministic package, mode, operation, skill, and link-rule locks plus explicit local approval and apply.
- Deterministic Connector Kit importers for OpenAPI, Postman, CLI help/completion, live MCP discovery, and live GraphQL introspection.
- Shared redacted execution plans exposed through `outfit explain` and `outfit run`/`outfit write --dry-run` without credential or provider access.
- Evidence-backed automatic project profile inference with an explicit lock-and-approve boundary.
- Opt-in SQLite FTS5 resource cache with TTL, provenance, arguments, expiry-aware search, inspection, and purge.
- Workspace policy enforced across CLI execution, local MCP, generated skills, prompt links, session context, cache access, diagnostics, audit output, and admin inventory.

## 0.1.0

- Standalone Go runtime with no hosted service application or hosted control-plane dependency.
- Local SQLite metadata, OS-keychain secret custody, encrypted-file fallback, durable cleanup outbox, and local receipts.
- One selected HTTP, CLI, or MCP mode per named connection.
- Fixed-origin/DNS-pinned HTTP, argv-only CLI, and official Go SDK MCP execution.
- Live MCP tool discovery with conservative mutation classification and generated skills.
- Bounded, read-only pasted-link context for GitHub, Slack, Linear, Sentry, Intercom, and Notion.
- Codex and Claude Code marketplace plugin, session/prompt hooks, local MCP facade, and project profiles.
- Loopback connector workbench and Connector Kit scaffold/validation/conformance/security diff.
