# Managed connectors

`outfit` is the local workstation for connector discovery, credentials, provider CLIs, agent skills, project policy, and portable restore. Run `outfit` in a terminal to open the workbench; every action also has a scriptable command.

## One connection, one mode, one credential method

A connection never means “all supported transports are on.” It selects exactly one execution mode—CLI, HTTP, or MCP—and one compatible credential method. The connector package ranks the alternatives and explains its recommendation. Reconfiguration verifies the candidate account before switching, so a failed mode or credential change leaves the prior connection usable.

Each provider page shows its active accounts first. Selecting an account shows the chosen execution recipe, authentication method, safe identity, credential fingerprint and custody, verification time, agent operations, managed CLI, and installed skills in one place. Testing, atomic replacement, deliberate reuse, and last-consumer destruction never display stored plaintext.

Refreshable hosted-MCP OAuth grants discovered in Claude Code and Codex appear as provider setup recipes. Transfer is ordered: refresh and live MCP verification, `outfit` vault commit, host-file backup, then exact-endpoint credential removal and disablement in the old hosts. A host mutation failure rolls the new `outfit` connection back. Linear, Notion, Sentry, and Slack have blessed hosted-MCP import modes; other standards-compliant hosted MCPs can be imported after OAuth metadata discovery.

## Managed CLIs and skills

Provider detail shows the CLI and skills needed by that account. System → **CLIs** searches the full tool registry and shows three distinct states: not installed, found globally on `PATH` but not managed, or installed/adopted by `outfit` with exact version/platform/source/path. Press `u` to install or update the provider-tested pin, or `a` to verify and deliberately adopt the discovered binary. The corresponding commands include `outfit tool install`, `status`, `update`, `use`, `adopt`, `remove`, `repair`, and `gc`.

System → **Skills** shows generated and upstream skills by connector and harness, including immutable source ref, digest, path, and install location. Optional upstream skills may be enabled or disabled without rotating the credential. System → **Updates** combines tested CLI and compatible skill updates. Restart an agent session after changing skills; current sessions do not hot-load filesystem additions.

## Stripe

Recommended: Stripe CLI 1.50.4 with a `outfit`-managed restricted test key.

```bash
outfit connector add stripe --name stripe-test --mode cli \
  --credential-method managed-restricted-key
outfit explain stripe-test customers_list
outfit stripe-test customers-list --limit 3
outfit stripe-test customers-list --all --max-items 500 --json
outfit stripe-test logs-tail       # live output; Ctrl-C stops it
```

`outfit` downloads the official platform archive, verifies its published SHA-256, and injects `STRIPE_API_KEY` only into the selected child. A competing inherited key cannot select another account. Native Stripe login remains an alternative method: on a fresh machine Outfit opens Stripe's browser approval, waits for the CLI's returned completion step, completes it through the same pinned binary, and verifies the account before committing the connection. Dashboard links are test/live bound before lookup.

Reads default to test mode. Add `--live` to any typed read (get, list, search, retrieve) to read live-mode data; it needs no `--write`:

```bash
outfit stripe subscriptions-get --subscription-id sub_123 --live
outfit exec stripe -- get /v1/subscriptions/sub_123 --live        # raw reads need no --write either
outfit exec stripe -- subscriptions retrieve sub_123 --live
```

With native Stripe login, `--live` makes the CLI use its profile's live key (Stripe keeps it in the OS keyring; if the CLI says the live key "needs to be re-configured", run `stripe login` again). A managed-key connection stores an optional second key, `STRIPE_LIVE_API_KEY` (an `rk_live_` restricted key with read permissions is best), read from the environment of setup or `outfit credential replace`, never from argv:

```bash
STRIPE_API_KEY=rk_test_... STRIPE_LIVE_API_KEY=rk_live_... outfit credential replace stripe
```

Setup and `outfit connector test` verify both keys. Without a stored live key, `--live` fails with a message naming the missing field instead of reading test data. Mutations stay CLI-only with `--write` in both modes. A Stripe error such as `resource_missing` is a failed call (`ok: false`, `meta.status: 404`, `error.provider_code: resource_missing`); in test mode it often means the object exists only in live mode.

Set `OUTFIT_STRIPE_TEST_KEY` and run `scripts/accept-managed-connectors.sh` to include a bounded real-account read in release acceptance.

## Google Workspace

Recommended for one human account: pinned GWS 0.22.5 with its native encrypted login. Outfit reuses a healthy profile or runs `gws auth setup --login` on a fresh machine. Outfit-managed browser OAuth is the alternative when Outfit should own the refresh grant and mint a short-lived token for each command; exported Workspace credentials JSON, a pre-obtained access token, and service-account JSON are separate choices.

```bash
outfit connector add google-workspace --name workspace-me --mode cli \
	--credential-method outfit-oauth \
	--set GWS_SERVICES=gmail,drive,calendar
```

Managed methods create an isolated temporary GWS config directory for each invocation, remove conflicting inherited token/credentials/ADC sources, materialize any credential file as `0600`, and delete it when the command exits. `outfit exec workspace-me -- gws arguments...` is unnecessary—the selected binary is implicit:

```bash
outfit workspace-me auth-status
outfit exec workspace-me -- gmail users messages list --params '{"userId":"me","maxResults":5}'
```

The pinned GWS source includes a large skill corpus. `outfit` verifies the immutable archive digest, validates every declared `SKILL.md`, and installs only paths whose required services are selected. A Gmail-only connection gets shared and Gmail skills in Codex and Claude, never Drive/Admin/Chat skills. Google Drive, Docs, Sheets, Slides, and Calendar links resolve only when their service capability is selected.

## Discord

Recommended: the Discord v10 REST API with an `outfit`-managed **bot** token owned by the server owner. User tokens (self-bots) violate Discord's Terms of Service and are not supported. In the Developer Portal open your application → Bot → Reset Token, enable the Message Content Intent (and Server Members Intent for `members`/`search_members`), and invite the bot with View Channels and Read Message History, plus Send Messages, Send Messages in Threads, Create Public Threads, Add Reactions, and Pin Messages if it should answer.

```bash
outfit connector add discord --name vortex-discord   # prompts for the token, or reads DISCORD_BOT_TOKEN
outfit vortex-discord guilds
outfit vortex-discord activity --guild 123456789012345678
outfit vortex-discord messages --channel 234567890123456789 --after 345678901234567890 --limit 100
outfit vortex-discord reply --channel 234567890123456789 --message 345678901234567890 \
  --nonce r345678901234567890 --content 'Thanks! (automated helper) The developer has been notified.' --write
```

Every request carries `Authorization: Bot …` and the required `User-Agent: DiscordBot (…)`. `activity` returns every channel and active thread with `last_message_id` in one call so a monitor fetches `messages --after <cursor>` only where something changed. Only message types 0 (default) and 19 (reply) are user posts; joins (7), pins (6), thread notices (18, 21), and poll results (46) are system notices. Writes need `--write`; `allowed_mentions` defaults to pinging nobody (`reply` notifies only the replied-to author), and `nonce` makes a retried send return the earlier message instead of posting twice. 429 responses are retried after Discord's `retry_after` (bounded). There is no gateway connection: monitoring is polling, and slash commands can be registered but not answered. discord.com message and channel links resolve through `outfit get`.

Polls, moderation, server management, and the escape hatch (connector 1.1.0):

```bash
outfit vortex-discord create-poll --channel 234567890123456789 --question 'Which day works?' \
  --answers '["Friday","Saturday"]' --duration 48 --write
outfit vortex-discord message --channel 234567890123456789 --message <poll message id>   # poll.results.answer_counts
outfit vortex-discord poll-voters --channel 234567890123456789 --message <id> --answer 1
outfit vortex-discord end-poll --channel 234567890123456789 --message <id> --write
outfit vortex-discord timeout-member --guild 123456789012345678 --user 456789012345678901 --until 60 \
  --reason 'Spam (automated helper, per owner policy)' --write          # --until 0 clears
outfit vortex-discord api-get --path /guilds/123456789012345678/emojis --query '{"limit":10}'
outfit vortex-discord api-call --method PATCH --path /guilds/123456789012345678/members/@me \
  --body '{"nick":"Helper"}' --reason 'Owner request' --write
```

- Polls: `create_poll` (answers as plain strings or poll answer objects; duration in hours, 1–768), `poll_voters`, `end_poll`; `message`/`messages` return the full `poll` object including `results`. `send_message` and `execute_webhook` also accept a raw `poll` object.
- Moderation (pass `reason`; it is URL-encoded into `X-Audit-Log-Reason`): `timeout_member`, `kick_member`, `ban_member`, `unban_member`, `bans`, `add_member_role`, `remove_member_role`, `audit_log`, `bulk_delete`, plus `reason` on `delete_message`, `update_thread`, `pin`, `unpin`.
- Server management: `create_channel`, `edit_channel` (including forum `available_tags`), `delete_channel`, `scheduled_events`, `create_scheduled_event`, `invites`, `create_invite`, `webhooks`, `execute_webhook`, `commands`, `register_command`, `delete_command` (`application` = the bot user ID from `me`).
- Escape hatch: `api_get` (read, GET only) and `api_call` (destructive class, `--write`; POST/PUT/PATCH/DELETE) take a path relative to `/api/v10` plus separate `query`/`body`. Paths with a scheme, host, `//`, `..`, `?`, `#`, backslashes, or encoded separators are rejected before any request; the origin, token, User-Agent, and 429 handling are fixed. `api_call` acts with the bot's full permissions (Administrator if granted), so the `--write` gate and the owner's own policy are the only limits.

## Portable backup and fresh-machine restore

```bash
outfit backup create ~/Downloads/outfit-backup.outfit
outfit backup inspect ~/Downloads/outfit-backup.outfit
outfit backup restore ~/Downloads/outfit-backup.outfit
outfit doctor
```

The passphrase is never stored. Argon2id derives the encryption key and AES-256-GCM authenticates the payload. Downloaded binaries and generated skills are reproducible and therefore excluded; restore installs the destination platform's artifacts, re-verifies connections, preserves shared credential relationships, and regenerates the selected host skills.

Repository `.outfit/workspace.yaml` and its lock remain secret-free. They pin packages, one mode/method per connection, capabilities, operations, tool versions/checksums, and upstream skill refs/digests. A fresh clone uses `outfit apply`; a fresh computer can restore its backup first to avoid re-entering credentials.
