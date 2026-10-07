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

## Portable backup and fresh-machine restore

```bash
outfit backup create ~/Downloads/outfit-backup.outfit
outfit backup inspect ~/Downloads/outfit-backup.outfit
outfit backup restore ~/Downloads/outfit-backup.outfit
outfit doctor
```

The passphrase is never stored. Argon2id derives the encryption key and AES-256-GCM authenticates the payload. Downloaded binaries and generated skills are reproducible and therefore excluded; restore installs the destination platform's artifacts, re-verifies connections, preserves shared credential relationships, and regenerates the selected host skills.

Repository `.outfit/workspace.yaml` and its lock remain secret-free. They pin packages, one mode/method per connection, capabilities, operations, tool versions/checksums, and upstream skill refs/digests. A fresh clone uses `outfit apply`; a fresh computer can restore its backup first to avoid re-entering credentials.
