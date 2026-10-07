# State-of-the-art technology review

Status: review only; no code changes have been made. Baseline: `main` at `f3c2ad5`. Review date: 2026-10-04. Author: technology-review worker (branch `wp-sota-review`).

Scope: Go toolchain/CI/release, TUI, agent integration (MCP, Claude Code, Codex), storage/messaging, transport/security/backup, Android, packaging/ops. Each item is **current choice → current state of the art (sourced) → KEEP / UPGRADE / REPLACE**, with priority, effort and risk. Sources are numbered `[n]` and listed at the end with dates. Claims about Pyre's code come from reading the files named. Nothing here was run against a live system. Every recommendation below that changes runtime behaviour needs the validation listed in §10.

Priority: **P0** = violates a stated product guarantee or will break soon; do now. **P1** = clear product benefit, cheap or important; do next. **P2** = worthwhile, schedule. **P3** = optional or watch. Effort: S ≤ 1 day, M ≤ 1 week, L > 1 week (one worker).

## 1. Executive summary

| # | Area | Current | Recommendation | Pri | Effort |
|---|---|---|---|---|---|
| 1 | SQLite durability | `synchronous(NORMAL)` + WAL for every DB (`internal/storage/sqlitex/sqlitex.go:23`) | **UPGRADE** to `synchronous(FULL)` (at least hub + node message/outbox DBs). NORMAL in WAL mode "might roll back following a power loss or system crash" [S1], which contradicts "each hop commits before acknowledging" (architecture §7) | **P0** | S |
| 2 | CI actions | `actions/checkout` v4.4.0 + `setup-go` v5.5.0, both `runs.using: node20` | **UPGRADE** to checkout v7.0.1 (`3d3c42e5…`) / setup-go v7.0.0 (`b7ad1dad…`). GitHub is removing Node 20 from runners in fall 2026 [S2] | P1 | S |
| 3 | CI coverage | `go vet`, `go test -race`, generated-file check; no vuln scan, no dependency bot, no Android job | **UPGRADE**: add `govulncheck` [S3], Dependabot (gomod, gradle, github-actions), and an Android job (assembleDebug, unit tests, lint) | P1 | S |
| 4 | Claude idle/attached delivery | Bracketed paste into the Ink TUI, gated off while a human is attached; dev channel optional | **SPIKE then ADOPT**: use a Pyre session plugin (`--plugin-dir` [S4]) with a **plugin monitor** [S5][S6] as an inbox "doorbell". It can wake an idle session, or one with a human attached, without keystrokes; the agent then pulls mail through `pyre mcp`. Paste stays as the fallback | P1 | M |
| 5 | Backup encryption | Custom `PYRE-HUB-BACKUP-ENC/1` (scrypt N=2^15 + chunked AES-GCM); age/gpg path writes a plaintext tar to disk first | **REPLACE** with the `filippo.io/age` library v1.3.2 [S7]: passphrase (scrypt), X25519 and hybrid post-quantum recipients, streaming, so no plaintext temp file and any `age` CLI can decrypt. Keep a reader for the old format | P1 | S–M |
| 6 | Android terminal | Hand-written 167-line `VtScreen` (no colour, no wide/grapheme width, no mouse) | **REPLACE** with Termux `terminal-emulator` + `terminal-view` (Apache-2.0) [S8]. Alternative: xterm.js in a WebView | P1 | M |
| 7 | Android secret storage | `androidx.security:security-crypto:1.1.0` `EncryptedSharedPreferences` | **REPLACE**: every API was deprecated in 1.1.0 [S9]. Move to an Android Keystore AES-GCM key + DataStore (or `datastore-tink`), migrating on first read | P1 | S |
| 8 | Android toolchain | AGP 8.11.1, Gradle 8.14.3, Kotlin 2.1.21, Compose BOM 2025.05.00, OkHttp 4.12.0, compile/target SDK 35 | **UPGRADE**: AGP 9.3 (built-in Kotlin, new DSL, R8 full mode) [S10][S11], Gradle ≥9.5, Kotlin 2.4.20 [S12], BOM 2026.09.00 [S13], OkHttp 5.5.0 [S14], target 36. Finish before AGP 10 (late 2026) [S15] | P2 | M–L |
| 9 | TUI stack | Bubble Tea v1.3.10, Bubbles v1.0.0, Lip Gloss v1.1.0 | **UPGRADE** to v2 (`charm.land/*/v2`, stable since 2026-02-23/24; bubbletea v2.0.10, lipgloss v2.0.6) [S16][S17]. Gains: progressive keyboard (shift+enter), synchronized output, the faster renderer, OSC 52 and hyperlinks, declarative View | P2 | M |
| 10 | TUI search | Substring AND (`matchQuery`) and a 3-level score in compose | **UPGRADE**: ranked fuzzy matching with match highlighting (`sahilm/fuzzy`, already Bubbles' choice) | P2 | S |
| 11 | MCP server | Hand-rolled stdio, negotiates ≤ 2025-06-18, only `readOnlyHint` | **UPGRADE in place**: accept 2025-11-25, full annotations, `outputSchema` from the Go types. Add a test-only conformance suite using the official Go SDK v1.8 client [S18]. Do not move the stdio or channel servers to 2026-07-28 yet: Claude Code won't register a 2026-07-28 channel [S19] | P2 | S |
| 12 | Claude hooks | 12 events, command hooks | **UPGRADE**: add `PostToolUseFailure`, `PermissionDenied` and `PostCompact` [S20] for approval/state evidence. Use `claude auth status` JSON (`configDirectory`, `authMethod`, v2.1.268+) [S4] to verify profile binding | P2 | S |
| 13 | Codex app-server | Hand-written types; `turn/start`/`turn/steer` with `clientUserMessageId` | **KEEP** the design. Add a version-bound schema drift test from `codex app-server generate-json-schema` [S21] | P2 | S |
| 14 | Go module hygiene | `go 1.26.0` in go.mod, built with 1.27.1. Decision I02 still says "go 1.24" | **UPGRADE**: `go 1.27.0`, pin tools with `tool` directives, add golangci-lint v2, fix I02 | P2 | S |
| 15 | Release integrity | Deterministic tarballs + SHA256SUMS; updates verify a sha256 supplied by the owner | **UPGRADE**: Ed25519/minisign signature over SHA256SUMS, verified by `upgrade.sh`/`internal/updates`; CycloneDX SBOM; double-build reproducibility check. GitHub attestations are not available for user-owned private repos [S22] | P2 | S–M |
| 16 | Linux services | pyred `KillMode=process`; runs share pyred's cgroup | **UPGRADE**: start the Pyre tmux server in its own `systemd-run --user --scope` under a `pyre-runs.slice` [S23]. pyred returns to the default KillMode and can be capped separately | P2 | M |
| 17 | tmux terminal features | Isolated config: history, remain-on-exit, escape-time | **UPGRADE** for attached humans: `extended-keys`/`extended-keys-format csi-u` (tmux ≥3.5), `set-clipboard`, RGB. Gate this on the paste regression test, because csi-u breaks Claude multi-line paste [S24] | P2 | S |
| 18 | Storage engine | `modernc.org/sqlite` (pure Go) | **KEEP**. It is faster than the ncruces/wasm driver on the 2026 TPC-H runs [S25], and cgo-free cross-compiling is a requirement (I03) | — | — |
| 19 | Snapshots | `VACUUM INTO` | **KEEP**: consistent online snapshot. Litestream 0.5 [S26] is optional later | P3 | — |
| 20 | Messaging infra | SQLite outbox/inbox, no broker (D05/D06) | **KEEP**. JetStream would not remove the application-level "indeterminate" problem, and adds an operated component | — | — |
| 21 | WebSocket | `coder/websocket` v1.8.15 | **KEEP**: maintained, context-native. There is still no WebSocket in the standard library | — | — |
| 22 | Tokens | 256-bit prefixed random secrets, SHA-256 at rest, constant-time compare | **KEEP**: the same pattern as GitHub/Stripe tokens, with instant revocation. JWT/PASETO/macaroons add nothing for a single hub | P3 (checksum suffix) | — |
| 23 | Hub TLS | Self-signed P-256, exact leaf SHA-256 pin, TLS ≥1.2 | **KEEP**. Optionally pin the SPKI so the cert can be re-issued without re-pairing. TLS 1.2 floor is required because minSdk 26 < API 29 | P3 | S |
| 24 | OS secret stores | `security -i` over stdin; `secret-tool` | **KEEP**: cgo-free and keeps secrets out of argv. `go-keychain` needs cgo | — | — |
| 25 | Push | FCM HTTP v1 (stdlib JWT) + optional `remoteMessaging` foreground-service stream | **KEEP** (D10). FGS timeouts apply only to `dataSync`/`mediaProcessing` [S27]. UnifiedPush [S28] only if Play services are absent | P3 | — |
| 26 | Logging/metrics | `log/slog` text + redaction | **KEEP**; add a JSON handler flag. **No OpenTelemetry** at this scale. Go's flight recorder / goroutine-leak profile for diagnostics [S29] | P3 | S |
| 27 | Release tooling | `release.sh` (deterministic tar, NOTICE) | **KEEP** instead of GoReleaser [S30]; add #15. A Homebrew tap is not needed for a private fleet | — | — |

## 2. P0/P1 rationale in one place

1. **#1 SQLite FULL (P0).** "Stored" and "origin outbox committed" are product facts (architecture §7; AGENTS.md "Stored messages, runtime acceptance, and task completion are different facts"). With WAL+NORMAL, a commit the hub acknowledged can disappear after an OS crash or power loss on server. Then the sender's outbox has already dropped the message, and the hub has no record. Cost: one fsync per commit. Pyre writes on the order of tens of transactions per minute, which is negligible on SSD. On macOS nodes, also set `PRAGMA fullfsync=ON` for the node DB (outbox), because plain `fsync` does not flush the drive cache there [S1].
2. **#2/#3 CI (P1).** Pinned-by-SHA is already good practice; the pinned versions are just stale (Node 20). Dependabot keeps this review from going stale and is free for private repos.
3. **#4 Doorbell (P1).** Today a message to a Claude session is *queued* whenever a human has the terminal attached (capabilities.md, "Gate"), and delivery depends on screen parsing of the Ink TUI. Architecture §5 requires "Leaving a native terminal attached must not prevent peer coordination." Plugin monitors (Claude Code ≥ v2.1.105, experimental) turn each stdout line into a notification that can wake an idle session [S5][S6]. Pyre already pins Claude Code 2.1.289.
4. **#5 age (P1).** It replaces custom crypto with an audited, interoperable format. It removes the plaintext-on-disk window in `scripts/backup/backup-hub.sh` and the hard dependency on the `age`/`gpg` binaries being installed. Restores then work with any age implementation, including on a machine without Pyre.
5. **#6 terminal (P1).** The Android terminal is a product surface (Android must give session access). Claude Code and Codex TUIs depend on SGR colour, box drawing and wide glyphs, which `VtScreen` drops by design.
6. **#7 credential store (P1).** It holds the owner token. Pyre pins exactly the release that deprecated the library.

## 3. Go toolchain, modules, CI, release

**Current.** `go.mod`: `go 1.26.0`, no `toolchain` line. Makefile/CI build with Go 1.27.1. `CGO_ENABLED=0 -trimpath`. CI matrix ubuntu/macos, `-race`. `release.sh` produces deterministic tarballs (sorted entries, fixed mtime, `gzip -n`), SHA256SUMS, and a NOTICE listing modules.

**State of the art.**
- Go 1.27.0 was released 2026-08-19 and 1.27.1 on 2026-09-01 (1.26.8 is the supported previous line) [S31]. 1.27 brings generic methods, a standard-library `uuid` package, `encoding/json` backed by the v2 engine by default (`GOEXPERIMENT=nojsonv2` opts out), `crypto/mldsa`, ML-KEM-1024 in TLS, `stdversion` vet in `go test`, and a GA goroutine-leak profile [S29]. Pyre's toolchain is current; only the `go` directive lags.
- `govulncheck` reports only vulnerabilities in reachable code and is the Go team's recommended scanner [S3].
- Go builds are bit-for-bit reproducible given the same toolchain and flags [S32]. That makes "build twice and compare" a cheap CI check.
- GitHub artifact attestations persist only for public repos or Enterprise Cloud. User-owned private repos get "Feature not available" [S22]. Keyless Sigstore on the public instance would write the private repo's identity to the public Rekor log. For this private, single-owner product, an offline Ed25519 key (minisign format, or plain `crypto/ed25519`) is the right-sized provenance mechanism.
- GoReleaser v2.14+ handles SBOM, cosign and Homebrew casks [S30]. Its main benefit is public distribution, which Pyre does not need.

**Recommendations.**
- #2 (P1, S): bump actions to the SHAs above, keeping `persist-credentials: false` and `permissions: contents: read`. Risk: none functional. setup-go v6+ reads `toolchain` from go.mod [S33].
- #3 (P1, S): add a `govulncheck ./...` job (pinned via a go.mod `tool` directive or `golang/govulncheck-action@v1` by SHA). Add `.github/dependabot.yml` for `gomod`, `gradle` (`/android`), and `github-actions`, weekly and grouped. Add an `android` job (JDK 17, `./gradlew assembleDebug testDebugUnitTest lintDebug`, Gradle cache) without signing secrets.
- #14 (P2, S): `go 1.27.0` + `toolchain go1.27.1`. Add `tool` lines for `govulncheck`, `golangci-lint` v2 (staticcheck, errcheck, gosec in report-only mode at first) and `cyclonedx-gomod`. Run `go fix ./...` (1.26+ modernizers). Replace `github.com/google/uuid` where Pyre imports it directly with `uuid` (it stays indirect through invopop/jsonschema). Note the pre-release indirect `go.yaml.in/yaml/v4 v4.0.0-rc.2`. Correct decision I02 (it says `go 1.24`). Risk: json v2 stricter defaults apply only to the v2 API; run the full suite and conformance after the bump.
- #15 (P2, S–M): add to `release.sh`:
  - `SHA256SUMS.minisig` (or `.sig`), signed with an offline key stored outside the repo, like the Android keystore.
  - `sbom.cdx.json` per platform.
  - `make verify-repro`: two clean builds compared with `cmp`.

  `scripts/install/upgrade.sh` and `internal/updates` verify the signature against a public key pinned at install time before honouring the digest. Migration: accept unsigned artifacts for one release with a warning, then refuse them.

## 4. TUI stack and terminal features

**Current.** Bubble Tea v1.3.10 / Bubbles v1.0.0 / Lip Gloss v1.1.0 with termenv. Already present: a command palette (`:`/`ctrl+k`), `/` filter, vim navigation, `?` help, alt screen, and ANSI-16 colours. The ANSI-16 choice is the most theme-adaptive and should be kept as the base. Compose uses a `textarea`. Matching is substring AND.

**State of the art.**
- Charm shipped Bubble Tea, Bubbles and Lip Gloss v2 on 2026-02-23/24 under `charm.land/...` import paths. Patches through bubbletea v2.0.10 (2026-09-24) and lipgloss v2.0.6 (2026-08-11) [S16][S17]. Large projects such as GitLab's `glab` already track v2.0.x [S34]. v2 adds:
  - Progressive keyboard enhancements (Kitty protocol: key release, shift+enter, ctrl+m distinct) [S35].
  - Synchronized output (DEC mode 2026) [S36], grapheme width (mode 2027).
  - A real cursor; OSC 52 clipboard; hyperlinks.
  - Background-colour detection with `LightDark()`.
  - A declarative `View` struct replacing toggle commands.
  - The "Cursed" renderer with large redraw-performance gains.
  - Lip Gloss v2 `Canvas`/`Layer` compositing for overlays.
- Comparable tools converge on the same patterns:
  - Command palette + fuzzy finder with a live **preview** pane (Neovim Telescope).
  - Context-sensitive key hints in a footer (lazygit, k9s).
  - `:` resource command mode and `/` filter (k9s).
  - Config-defined sections and saved filters (gh-dash).
  - Shift+enter for newline and enter to send in compose (Claude Code, Codex CLI).

  Pyre's TUI already has the first and third. The gaps are ranked fuzzy results with highlighted matches, a preview pane in the palette/session list, and multi-line compose with shift+enter.
- Fuzzy ranking: `sahilm/fuzzy` (MIT; used by Bubbles' list filter) [S37] gives ranked results with matched indexes for highlighting. fzf's `src/algo` [S38] scores better but is an internal API of a CLI, so it is not a stable library dependency.

**Recommendations.**
- #9 (P2, M): migrate `internal/tui` and the `tuitest` harness to v2. Concrete UX wins:
  - shift+enter newline in compose; enter sends.
  - OSC 52 "copy attach command / session ID" that works over SSH.
  - OSC 8 links for artifact preview URLs.
  - Synchronized output, which removes tearing during frequent fleet refresh.
  - A `LightDark` palette only for the few non-ANSI accents.

  Risk: v1 → v2 API breaks across ~15 files; the screenshot pipeline (`102d91f`) needs re-baselining. Inside tmux, Kitty keyboard falls back to csi-u, so pair this with #17.
- #10 (P2, S): replace `matchQuery`/compose scoring with `fuzzy.FindFrom` (ranked, with highlight indexes), keeping the multi-term AND semantics for the session filter.
- #17 (P2, S): in `internal/runtime/tmux` `DefaultConfig`, add `extended-keys on`, `extended-keys-format csi-u`, `set-clipboard on`, and `terminal-features ',*:RGB:hyperlinks'`. Gate this on tmux ≥3.5 (the Mac has 3.4, server has 3.6) **and** on rerunning the paste-delivery suite: Claude Code issue #43167 reports multi-line paste losing newlines under csi-u [S24].

## 5. Agent integration: MCP, Claude Code, Codex

**MCP: current.** `internal/cli/mcp.go` is a 450-line stdio JSON-RPC server. It offers versions `2025-06-18`/`2025-03-26`/`2024-11-05` and handles cancellation. It returns `structuredContent` without an `outputSchema`, and tool errors as `isError` results. Tool order is deterministic. Annotations are `readOnlyHint` only.

**MCP: state of the art.**
- 2025-11-25 [S39] adds tool icons and tool-name guidance, URL-mode elicitation, experimental tasks, and JSON Schema 2020-12 as the default dialect. It also says validation errors should be *tool execution errors* (Pyre already does this).
- 2026-07-28 [S40] (final 2026-07-28) is the largest revision so far:
  - Stateless: no `initialize`; per-request `_meta` carries version and capabilities.
  - `server/discover`, which clients may use as a backward-compatibility probe on stdio.
  - `ping` and logging/setLevel removed; Roots, Sampling and Logging deprecated.
  - Tasks moved to an extension.
  - A 12-month minimum deprecation window for old revisions.
- The official Go SDK v1.7.0 implements 2026-07-28 and negotiates down to 2025-11-25 and earlier. v1.8.0 shipped 2026-09-14 [S18].
- Claude Code's v2 MCP runtime (default from v2.1.232) uses 2026-07-28 only with HTTP servers that support it. It **does not register a channel server that negotiates 2026-07-28** [S19]. Stdio servers that don't support it "connect on the earlier handshake".

**MCP: recommendation (#11, P2, S).** Keep the hand-rolled server for now. It is small and stdio-only, and its legacy handshake is what Claude Code's channel and stdio paths require today. Upgrade it:
- Add `2025-11-25` to `mcpVersions`.
- Add `title`, `destructiveHint`, `idempotentHint` and `openWorldHint` to every tool (`operation_id` tools are idempotent; `stop_session`/`reissue_message` are destructive).
- Emit `outputSchema` from the result types with the already-vendored `invopop/jsonschema`.
- Keep returning `-32601` for `server/discover`, which is the correct "legacy only" signal.

Add `internal/cli/mcp_conformance_test.go`, which drives `pyre mcp` with the official Go SDK client (test-only dependency) at 2025-06-18 and 2025-11-25. **Revisit REPLACE** (adopting the go-sdk server) when Claude Code negotiates 2026-07-28 for stdio and channels; check quarterly. Benefit: protocol churn is absorbed upstream. Risk: a new dependency, plus `MCPGODEBUG` escape hatches that are scheduled for removal.

**Claude Code: current.** Each run gets `--settings` hooks (12 events, command type), `--mcp-config=` for `pyre mcp`, paste delivery with a `[pyre-msg:id]` marker, a PermissionRequest broker, and an optional dev channel (`runtime/channels`).

**Claude Code: state of the art (verified against current docs).**
- **Hooks** [S20]. There are now 33 events: `PostToolUseFailure`, `PermissionDenied`, `PostToolBatch`, `PostCompact`, `TaskCompleted`, `CwdChanged`, `FileChanged`, `PreModelSwitch`/`PostModelSwitch`, `Elicitation`, and others. Hook types are `command`, `http`, `mcp_tool`, `prompt` and `agent`. `async`/`asyncRewake` hooks run in the background, and exit code 2 surfaces stderr to Claude as a system reminder.
- **Per-session plugins.** `--plugin-dir` loads a plugin directory or zip for one session only [S4]. A plugin can bundle hooks, MCP servers, skills and **monitors**.
- **Plugin monitors.** Experimental, v2.1.105+ [S5][S6]. A persistent command's stdout lines become notifications that can wake an idle session; the monitor inherits `CLAUDE_CODE_SESSION_ID`. Limits: interactive only, best-effort, auto-killed if noisy. They are skipped on Bedrock/Vertex/Foundry and when `DISABLE_TELEMETRY` or `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC` is set.
- **Channels** remain a research preview [S41][S42]: an allowlist or the dev flag, a consent dialog, and Team/Enterprise orgs must enable them. **Permission relay** (`claude/channel/permission`) lets a remote answer and the terminal race, with "whichever answer arrives first" applied and the other closed. That is exactly Pyre's C14 problem, where a broker hook keeps waiting after the human answers in the TUI. Relay is restricted to registered channel servers from v2.1.234.
- **Native background sessions.** `claude --bg`, `claude agents`/`attach`/`respawn`, and a `claude daemon` supervisor [S4]. These are single-host and Claude-only, so they do not replace Pyre. Watch for overlap. **Remote Control** drives a local Claude session from the Claude app; it complements the Pyre Android client but does not replace it (no Codex/fleet).

**Claude Code: recommendations.**
- #4 (P1, M), the doorbell spike. Generate a per-run plugin dir containing:
  - A monitor running `pyred inbox-watch --session $PYRE_SESSION_ID`, which prints one line per newly routed message (ID, sender, one-line subject), never the body.
  - The existing hooks, moved from `--settings` into the plugin (optional).
  - The `pyre mcp` server definition.

  The agent fetches the body with the `inbox` tool and acknowledges with `ack_message`. That tool call is structured evidence, captured by PostToolUse with the message ID. It is stronger than screen-parsed paste markers and works while a human is attached. Model the doorbell as a new capability `input.monitor` with its own evidence kind (`claude.monitor.notified` + `claude.tool.inbox_read`). Keep paste for "must start a turn now". Never count the doorbell line itself as acceptance.

  Unknowns to measure:
  - Does a notification start a turn when idle with a human attached?
  - Behaviour during an approval dialog.
  - Rate limits.
  - Interaction with `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC` in managed profiles.
  - Feature-flag dependence.
- #12 (P2, S):
  - Register `PostToolUseFailure` and `PermissionDenied`, so approval "effective" can be decided on a denial or failure as well as on PostToolUse.
  - Register `PostCompact`, so transcript cursors survive compaction.
  - Validate each name on 2.1.289 first; unknown names can open a settings-error dialog (`hook.go` comment).
  - In the accounts adapter, verify a run's binding with `claude auth status` (JSON `configDirectory`, `authMethod`; v2.1.268+) instead of inferring it from files.
- P3: prototype permission relay through the existing `runtime/channels` bridge when the owner opts into channels. It would close C14 cleanly but inherits the preview's consent and policy gates.

**Codex: current/SOTA.** Pyre already uses the current design: a Pyre-owned `codex app-server` on a unix socket, `experimentalApi`, `turn/start` with `clientUserMessageId`, `turn/steer` with `expectedTurnId`, and a native TUI via `--remote`. The official docs confirm these methods and that `generate-json-schema`/`generate-ts` output "is specific to the Codex version you ran" [S21]. There are no official Go bindings. **#13 (P2, S):** add `tools/codexschema` + a test that runs `codex app-server generate-json-schema` for the installed version (skipped if absent) and diffs the fields Pyre decodes in `internal/runtime/codex/types.go`, so a Codex upgrade fails loudly instead of silently dropping fields.

## 6. Storage and messaging

- **Driver: KEEP `modernc.org/sqlite`.** On the 2026-03-29 TPC-H comparison, modernc was faster than ncruces/go-sqlite3 (wazero) on queries (SF1 Q1: 12.0 s vs 20.6 s), while ncruces generated data faster [S25]. mattn requires cgo, which conflicts with I03. Pyre's workload is small OLTP, where either pure-Go driver is fine; switching buys nothing.
- **Durability: UPGRADE (#1, P0).** Change the `sqlitex.Open` DSN to `synchronous(FULL)`, and add an option (or darwin default) for `fullfsync(1)`. Keep WAL, `busy_timeout`, `_txlock=immediate` and foreign keys (all current best practice). Add a test asserting `PRAGMA synchronous` = 2 on hub and node DBs. Also consider running `PRAGMA optimize` on close/periodically (SQLite's recommended maintenance) [S1].
- **Backups: KEEP `VACUUM INTO`.** It produces a transactionally consistent, compacted copy from a live WAL database without blocking writers for long. The online backup API restarts on concurrent writes unless it steps in a loop, so it brings no advantage at hub size. Litestream 0.5 (point-in-time recovery, transaction IDs) [S26] would cut RPO from snapshot interval to seconds. It is a separate process and v0.5 is young, so treat it as P3 and wait unless the owner wants continuous replication off-host.
- **Outbox/inbox vs NATS JetStream: KEEP D05/D06.** The transactional-outbox pattern on the same SQLite transaction as the domain write is the standard way to avoid dual writes. JetStream would add at-least-once streams but not runtime-acceptance truth: the indeterminate state comes from the harness, not the transport. Revisit only if multi-hub or high fan-out appears.

## 7. Transport and security

- **Hub TLS: KEEP.** Self-signed ECDSA P-256, an exact leaf SHA-256 pin delivered out of band at enrollment/pairing, `VerifyConnection` with constant-time compare: this is the standard TOFU-free pinning pattern for a private service behind Twingate. Go ≥1.24 negotiates hybrid X25519MLKEM768 by default between Pyre's Go peers. The TLS 1.2 floor is needed because Android minSdk 26 lacks TLS 1.3 before API 29. P3 option: pin the SPKI (public key) instead of the whole leaf, accepting both during a transition, so a cert can be re-issued (new SAN, expiry) without re-pairing every node and phone.
- **mTLS / WireGuard / SPIFFE: not now.** mTLS would make node credentials proof-of-possession, so a leaked bearer token alone would not authenticate. The cost is enrollment changes plus Keystore-backed client certs on Android. Twingate already provides the network layer (owner direction: no new mesh). Record it as P3.
- **Tokens: KEEP.** Prefixed 256-bit secrets, only SHA-256 stored, constant-time compare, purpose-bound one-time tickets, and a rate-limited enrollment exchange. A slow hash is unnecessary for 256-bit random secrets. Optional P3: add a CRC32 checksum suffix, as GitHub tokens do, so the redactor and secret scanners can tell real tokens from look-alikes offline.
- **WebSocket: KEEP `coder/websocket`.** It is the actively maintained, context-first successor to nhooyr.io/websocket. Pyre's code already uses its unix-socket dialing for Codex.
- **Rate limiting:** present for unauthenticated enrollment exchange. P3: add a small per-principal token bucket (`golang.org/x/time/rate`) on authenticated write RPCs, to stop a runaway agent loop from flooding the hub.
- **Secret storage: KEEP.** `security -i` with values on stdin (never argv) and `secret-tool` keep builds cgo-free and secrets out of `ps`. `go-keychain`/`zalando/go-keyring` would add cgo (macOS) or D-Bus code for no gain.
- **Backup encryption: REPLACE with age (#5, P1).**
  - Current: a careful STREAM-style construction (per-chunk nonce counter, final flag, header as associated data), scrypt N=2^15 (~32 MiB). The external age/gpg path writes `--unencrypted` plaintext to disk first.
  - age v1.3.2 (2026-08-29, BSD-3) [S7]:
    - `ScryptRecipient`/`ScryptIdentity` with a stronger default work factor.
    - `X25519Recipient` and a post-quantum `HybridRecipient` (ML-KEM-768 hybrid, `age1pq1…`).
    - Streaming `Encrypt`/`Decrypt`, `DecryptReaderAt`, and `armor`.
  - Migration:
    1. `pyre-hub backup --encrypt passphrase|recipients FILE…` streams `tar → age.Encrypt` directly.
    2. Restore detects the age header, or the old magic for backward compatibility.
    3. `backup-hub.sh` stops writing plaintext for the age path; gpg stays as an external option.
    4. The restore rehearsal covers all three formats.

## 8. Android

**Current** (`android/gradle/libs.versions.toml`): AGP 8.11.1, Gradle 8.14.3, Kotlin 2.1.21, Compose BOM 2025.05.00, lifecycle 2.9.0, kotlinx-serialization 1.8.1, coroutines 1.10.2, OkHttp 4.12.0, security-crypto 1.1.0, firebase-messaging 24.1.0 (optional flavor), compile/target 35, minSdk 26, JDK 17, R8 minify + resource shrinking.

**State of the art.**

| Component | Current | Latest stable (date) | Notes |
|---|---|---|---|
| AGP | 8.11.1 | 9.3.0 (Jul 2026) [S11] | 9.0 (Jan 2026) [S10]: built-in Kotlin (drop `org.jetbrains.kotlin.android`), new DSL default, R8 full mode default, `android.defaults.buildfeatures.resvalues=false` (Pyre uses `resValue` → must enable), KGP ≥2.2.10. AGP 10 late 2026 removes opt-outs [S15] |
| Gradle | 8.14.3 | 9.5.1 (May 2026) / 9.6.0 [S43] | AGP 9.3 needs ≥9.5.0 |
| Kotlin | 2.1.21 | 2.4.20 (2026-09-07) [S12] | 2.4: stable context parameters, Gradle 9.5 compat |
| Compose BOM | 2025.05.00 | 2026.09.00 [S13] | Material3 1.4 stable; **M3 Expressive** needs 1.5.0-alpha. Stay on stable M3 (P3 watch) [S44] |
| OkHttp | 4.12.0 | 5.5.0 (2026-08-16) [S14] | 5.x: Android-specific artifact, Happy Eyeballs, ECH opt-in; MockWebServer moved to `mockwebserver3` |
| security-crypto | 1.1.0 | deprecated in 1.1.0 [S9] | Use a Keystore AES-GCM key + DataStore, or `datastore-tink` |
| target SDK | 35 | 36 (Android 16); 37 shipped 2026-06-16 [S45] | Sideload: no Play deadline, but 36 is AGP 9's default compile SDK |

**Background work: KEEP.** The `remoteMessaging` foreground service holding `/v1/events/stream` matches Android's own guidance for "a messaging service permanently connected with a websocket", and Android 15's 6 h/24 h timeout applies only to `dataSync`/`mediaProcessing` [S27]. FCM data-only + fetch remains D10. UnifiedPush (RFC 8030/8291 + VAPID; ntfy/Sunup distributors) [S28] is the de-Googled alternative. It needs a hub Web Push sender, so P3, and only if the phone lacks Play services.

**Terminal (#6, P1, M).** Options:
- **(a) Termux `terminal-emulator` + `terminal-view`** [S8]. Apache-2.0 modules: the main Termux app is GPLv3, so check each module's licence file when vendoring. A native view with full xterm-256/truecolor, wide characters, scrollback and mouse, plus proven soft-keyboard/IME handling. Feed it bytes from the existing `TerminalSocket`.
- **(b) xterm.js in a WebView** (MIT; VS Code's terminal). Best VT fidelity and WebGL rendering, but weaker Android IME/keyboard behaviour, plus a JS bridge.

Recommend (a), behind the existing `TerminalScreen` interface, and keep `VtScreen` until device-verified. Risk: the JitPack artifact pins a commit, so vendor it as source modules if JitPack availability is a concern.

**Credential store (#7, P1, S).** Generate an AES-256-GCM key in AndroidKeyStore (StrongBox when available; `setUnlockedDeviceRequired` optional). Encrypt the pairing record (`hub_url`, pin, owner token) into a DataStore/proto file. On first launch after the upgrade, read the old EncryptedSharedPreferences, write the new store, then delete the old file. Exclude both from backup (`data_extraction_rules.xml` already exists).

**Toolchain (#8, P2, M–L).** Order:
1. Gradle wrapper 9.5.1 → AGP 9.3 with `builtInKotlin`: remove the `kotlin-android` plugin, keep `kotlin.compose`/`serialization` plugins, `buildFeatures { resValues = true }`, migrate `kotlin { compilerOptions }`.
2. Kotlin 2.4.20.
3. BOM 2026.09.00 (+ lifecycle/activity/core bumps).
4. OkHttp 5.5.0 (`okhttp-android`; tests to `mockwebserver3`).
5. compile/target 36 (predictive back and edge-to-edge without the opt-out).
6. Check R8 full-mode keep rules for kotlinx-serialization models (strict full mode: `-keep class X` no longer implies constructors).

The signed release must install over the current one: same `applicationId` and key. P3: a Baseline Profile module for startup.

## 9. Packaging and operations

- **systemd (#16, P2, M).** Today `KillMode=process` keeps runs alive across pyred restarts, but systemd documents `process` as not recommended. It also means pyred's caps (`MemoryMax`, `CPUQuota`) apply to every agent, which is why hardening is off. SOTA: start the long-lived child tree in its own unit, `systemd-run --user --scope --unit=pyre-tmux-<node> --slice=pyre-runs.slice tmux -S … start-server` [S23]. Then:
  - pyred uses the default `KillMode=control-group` and gets modest hardening and limits.
  - `pyre-runs.slice` carries `MemoryHigh`/`CPUWeight` to protect the host.
  - `systemctl --user status pyre-runs.slice` shows run processes.
  - Upgrades no longer depend on a discouraged kill mode.

  Requires linger, which is already required. Fallback: today's behaviour when `systemd-run` or the user bus is unavailable. macOS: **KEEP** the LaunchAgent (`AbandonProcessGroup`, `ProcessType=Standard` are the right keys).
- **Self-update: KEEP** `internal/updates` (stage → health check → default-for-new-runs → rollback). It already follows the canary pattern; add signature verification (#15).
- **Release tooling: KEEP** `release.sh` instead of GoReleaser. It is already deterministic and small. GoReleaser's wins (casks, changelog, public signing) target public distribution.
- **Logging: KEEP** `log/slog` + `NewRedactingLogger`. P3: `--log-format json` for machine parsing. launchd `StandardOutPath` logs are not rotated (pyred has its own logfile ring); confirm the launchd stdout files stay small or point them at `/dev/null`.
- **Metrics/tracing: no OpenTelemetry.** For two to five nodes, the diagnostics view, slog and on-demand profiles are enough. If deeper debugging is needed, Go's `runtime/trace` flight recorder and the 1.27 goroutine-leak profile [S29] behind an owner-only diagnostics action cover it without a collector.

## 10. Other findings

- Decision log drift: I02 says `go 1.24`; go.mod says `1.26.0`. Two rows share ID **I10** (rota checkout and contract v1.3). Renumber the second.
- `docs/evidence/README.md` has no row for `review/`. The lead should add one (this worker owns only `docs/evidence/review/`).
- `android/app/build.gradle.kts` suppresses lint `OldTargetApi`/`NewerVersionAvailable`. With Dependabot this is fine, but keep `GradleDependency` visible in CI reports.
- `scripts/backup/backup-hub.sh` default `auto` falls back to the built-in passphrase format without recipients; after #5 that becomes age passphrase mode.

## 11. Implementation plan: independent work packages

`go.mod`/`go.sum`, `.github/`, `Makefile`, and `docs/decisions.md` are lead-owned: workers request edits. Android work is serialized inside `android/` (one owner at a time for `build.gradle.kts`/`libs.versions.toml`).

| WP | Scope (items) | Files (exclusive) | Depends | Validation required |
|---|---|---|---|---|
| **S-A Durability** (lead, P0) | #1 | `internal/storage/sqlitex/sqlitex.go` (+ test) | — | Unit test asserts `PRAGMA synchronous=2` (and `fullfsync=1` on darwin node DB). Full `go test -race ./...`. Hub write-latency spot check on server before/after (expect < 5 ms/commit). Record in decisions as I13 |
| **S-B CI hygiene** (lead, P1) | #2, #3, #14 | `.github/workflows/*.yml`, `.github/dependabot.yml`, `go.mod` (`go`/`toolchain`/`tool` lines), `.golangci.yml` | — | CI green on ubuntu+macos with node24 actions. govulncheck clean or triaged. Android job builds unsigned debug + unit tests + lint. `stdversion` vet passes |
| **S-C Backup on age** (P1) | #5 | `internal/hub/backup/*`, `scripts/backup/*`, `cmd/pyre-hub` backup flags | S-B (go.mod add `filippo.io/age`) | Round-trip tests: passphrase, X25519, hybrid PQ, multi-recipient. Old-format restore. Truncation/tamper rejected. No plaintext file ever created (test with a watched temp dir). `scripts/backup/rehearse-restore.sh` passes. Decrypt one archive with the stock `age` CLI |
| **S-D Claude doorbell spike** (P1) | #4 | `internal/runtime/claude/*` (plugin generation, evidence kinds), new `runtime/claudeplugin/` template, `cmd/pyred` `inbox-watch` subcommand, `docs/evidence/runtime/` new report | — | Real Claude 2.1.289 on Mac and server, isolated profile: idle wake; human attached (write client) wake; busy (queued until turn end?); approval dialog open; pyred restart (monitor reconnect, no duplicate lines); message storm (auto-kill threshold). Evidence: `inbox` tool call + ack via PostToolUse. Only then add capability `input.monitor` to contracts (lead) |
| **S-E MCP + hooks currency** (P2) | #11, #12 | `internal/cli/mcp.go`, `internal/cli/mcp_conformance_test.go`, `agents/tools/pyre-mcp-tools.json`, `internal/runtime/claude/hook.go`, `internal/accounts` auth-status probe | S-B (test-only go-sdk dep) | Conformance at 2025-06-18 and 2025-11-25 via the official Go SDK client. `tools/list` snapshot regenerated. Real Claude launch shows no settings-error dialog with the new hook names; an approval denial is classified via PermissionDenied/PostToolUseFailure |
| **S-F Codex schema drift** (P2) | #13 | `tools/codexschema/`, `internal/runtime/codex/schema_test.go` | — | Passes on Codex 0.160.0. Deliberately removing a field from a generated schema fails the test |
| **S-G Android security + terminal** (P1) | #6, #7 | `android/app/src/main/kotlin/dev/jhh3/pyre/{data/CredentialStore.kt,terminal/,ui/terminal/}`, terminal module dependency lines | — (before S-H) | Robolectric: migration from an EncryptedSharedPreferences fixture, then old file deleted. Device test on the owner's phone: pair, kill/restart, token survives. Terminal renders Claude Code and Codex TUIs (colour, box drawing, CJK/emoji width), soft keyboard + ctrl/esc keys, resize. `mockhub-it.sh` and `realhub-it.sh` pass |
| **S-H Android toolchain** (P2) | #8 | `android/build.gradle.kts`, `android/app/build.gradle.kts`, `android/gradle/**`, `android/gradle.properties`, test imports for mockwebserver3 | S-G merged | `assembleRelease` signed with the existing key installs **over** the current build. Unit tests, lint, both integration scripts, R8 release smoke (pairing, inbox, approval, artifact preview, terminal) on device |
| **S-I TUI v2** (P2) | #9, #10 | `internal/tui/**`, `internal/cli/tui.go`, screenshot pipeline baselines | S-B (go.mod charm.land v2) | tuitest suite. Screenshot review in Ghostty/iTerm2 direct, inside tmux 3.4 and 3.6, and over ssh: shift+enter newline, OSC 52 copy, OSC 8 link, no tearing on refresh. Fuzzy ranking unit tests with highlight indexes |
| **S-J Release integrity** (P2) | #15 | `packaging/release.sh`, `packaging/README*.md`, `scripts/install/upgrade.sh`, `internal/updates/*` | S-B | Two clean builds are byte-identical (`make verify-repro`). Signature verifies. A tampered SHA256SUMS or a wrong key blocks `upgrade.sh` and node staging. SBOM lists all `go version -m` modules. Key stored outside the repo |
| **S-K Linux scopes + tmux features** (P2) | #16, #17 | `packaging/systemd/*`, `internal/supervisor/*`, `internal/runtime/tmux/tmux.go` (`DefaultConfig`), `scripts/install/*` systemd parts | — | On server: `systemctl --user restart pyred` with live Claude/Codex runs → runs survive, and pyred cgroup no longer contains them. `systemd-cgls` shows `pyre-runs.slice`. Install/upgrade/uninstall rehearsal re-run. Paste-delivery real tests rerun with csi-u enabled on tmux 3.6; feature disabled on 3.4 |

Suggested sequencing: S-A and S-B first (hours, lead). Then S-C, S-D, S-E, S-F, S-G, S-I, S-J and S-K in parallel; S-H after S-G. S-D's report decides whether `input.monitor` becomes the default Claude delivery path (contract change by the lead).

## 12. What was not verified

- No recommendation was executed; versions are from upstream pages on the dates below. Plugin-monitor behaviour with a human attached, during dialogs, and under managed profiles is **unverified** (S-D exists to measure it).
- Termux module licences were taken from upstream statements [S8]; confirm the licence files when vendoring.
- The Claude Code docs mention a `monitors` key without a dedicated reference page in the fetched docs; the details come from secondary sources [S5][S6] and need confirming on 2.1.289.
- Sources marked "secondary" are third-party write-ups; primary docs were used wherever available.

## Sources (accessed 2026-10-04 unless noted)

- [S1] SQLite, PRAGMA synchronous / fullfsync / optimize — https://www.sqlite.org/pragma.html
- [S2] GitHub changelog, "Deprecation of Node 20 on GitHub Actions runners" (2025-09-19; Node 24 default from 2026-06-16, Node 20 removal fall 2026) — https://github.blog/changelog/2025-09-19-deprecation-of-node-20-on-github-actions-runners/ ; action runtimes checked at https://raw.githubusercontent.com/actions/checkout/11d5960a326750d5838078e36cf38b85af677262/action.yml and https://raw.githubusercontent.com/actions/setup-go/d35c59abb061a4a6fb18e82ac0862c26744d6ab5/action.yml ; tags via `git ls-remote` of actions/checkout and actions/setup-go
- [S3] Go, govulncheck tutorial — https://go.dev/doc/tutorial/govulncheck
- [S4] Claude Code CLI reference (`--plugin-dir`, `claude auth status`, `--bg`, `claude daemon`) — https://code.claude.com/docs/en/cli
- [S5] Plugin background monitors (secondary; v2.1.105) — https://www.agentpatterns.ai/tools/claude/plugin-background-monitors/
- [S6] "Claude Code plugin monitors" (secondary, 2026-08-21) — https://nikiforovall.blog/ai/2026/08/21/claude-code-plugin-monitors.html
- [S7] filippo.io/age v1.3.2 (2026-08-29) — https://pkg.go.dev/filippo.io/age ; v1.3 post-quantum recipients — https://www.x-cmd.com/blog/251230
- [S8] Termux app repository (terminal-emulator / terminal-view modules, Apache-2.0 per upstream notes) — https://github.com/termux/termux-app
- [S9] EncryptedSharedPreferences reference (deprecated) — https://developer.android.com/reference/androidx/security/crypto/EncryptedSharedPreferences ; "EncryptedSharedPreferences is Dead" (2026-08) — https://blog.includesecurity.com/2026/08/encryptedsharedpreferences-is-dead-heres-what-you-should-use-instead/
- [S10] AGP 9.0 release notes (Jan 2026) — https://developer.android.com/build/releases/agp-9-0-0-release-notes
- [S11] AGP 9.3.0 (Jul 2026) — https://developer.android.com/studio/releases/gradle-plugin
- [S12] Kotlin 2.4.0 released (2026-06/07); 2.4.20 (2026-09-07) — https://blog.jetbrains.com/kotlin/2026/06/kotlin-2-4-0-released/ , https://kotlinlang.org/docs/whatsnew2420.html
- [S13] Compose BOM (2026.09.00) — https://developer.android.com/develop/ui/compose/bom
- [S14] OkHttp changelog (5.5.0, 2026-08-16) — https://github.com/square/okhttp/blob/master/CHANGELOG.md
- [S15] AGP DSL/API migration timeline (AGP 10 late 2026) — https://developer.android.com/build/releases/gradle-plugin-roadmap
- [S16] Bubble Tea releases (v2.0.0 2026-02-23 … v2.0.10 2026-09-24) — https://github.com/charmbracelet/bubbletea/releases
- [S17] Lip Gloss releases (v2.0.0 2026-02-24 … v2.0.6 2026-08-11) — https://github.com/charmbracelet/lipgloss/releases
- [S18] MCP Go SDK releases (v1.7.0 2026-07-28: 2026-07-28 support + legacy negotiation; v1.8.0 2026-09-14) — https://github.com/modelcontextprotocol/go-sdk/releases
- [S19] Claude Code MCP docs (v2 runtime from v2.1.232; channel servers negotiating 2026-07-28 not registered) — https://code.claude.com/docs/en/mcp
- [S20] Claude Code hooks reference — https://code.claude.com/docs/en/hooks
- [S21] Codex app-server docs (methods, `experimentalApi`, version-bound schema generation) — https://learn.chatgpt.com/docs/app-server
- [S22] GitHub docs, artifact attestations (private repos require Enterprise Cloud) — https://docs.github.com/actions/security-guides/using-artifact-attestations-to-establish-provenance-for-builds
- [S23] systemd-run(1) (`--user --scope`) and tmux-under-systemd guidance — https://www.freedesktop.org/software/systemd/man/latest/systemd-run.html ; https://bbs.archlinux.org/viewtopic.php?id=194396
- [S24] Claude Code issue: multi-line paste loses newlines with tmux `extended-keys-format csi-u` — https://claudeissues.com/issue/43167-multi-line-paste-loses-newlines-when-tmux-extended-keys-format-is-csi-u ; tmux extended keys setup (secondary) — https://pi.dev/docs/latest/tmux
- [S25] go-sqlite-bench (TPC-H, 2026-03-29) — https://github.com/cvilsmeier/go-sqlite-bench
- [S26] Litestream v0.5.0 — https://fly.io/blog/litestream-v050-is-here/
- [S27] Android foreground-service timeouts (Android 15) — https://developer.android.com/develop/background-work/services/fg-service-timeout ; receiving messages reliably — https://developer.android.com/social-and-messaging/guides/communication/receiving-messages
- [S28] "5 years of UnifiedPush" (2026-01-08) — https://f-droid.org/2026/01/08/unifiedpush-5-years.html
- [S29] Go 1.27 release notes — https://go.dev/doc/go1.27
- [S30] GoReleaser v2.14 (2026-02-21) — https://www.goreleaser.com/blog/goreleaser-v2.14
- [S31] Go release history (1.27.0 2026-08-19; 1.27.1 2026-09-01) — https://go.dev/doc/devel/release
- [S32] Go blog, "Perfectly Reproducible, Verified Go Toolchains" (2023-08) — https://go.dev/blog/rebuild
- [S33] actions/setup-go README (node24, `toolchain` directive support in v6+) — https://github.com/actions/setup-go
- [S34] GitLab CLI MR updating `charm.land/bubbletea/v2` to v2.0.9 — https://gitlab.com/gitlab-org/cli/-/merge_requests/3793
- [S35] Kitty keyboard protocol — https://sw.kovidgoyal.net/kitty/keyboard-protocol/
- [S36] Synchronized output (mode 2026) spec — https://gist.github.com/christianparpart/d8a62cc1ab659194337d73e399004036
- [S37] sahilm/fuzzy — https://github.com/sahilm/fuzzy
- [S38] fzf algorithm source — https://github.com/junegunn/fzf/tree/master/src/algo
- [S39] MCP 2025-11-25 changelog — https://modelcontextprotocol.io/specification/2025-11-25/changelog
- [S40] MCP 2026-07-28 changelog — https://modelcontextprotocol.io/specification/2026-07-28/changelog ; release post — https://blog.modelcontextprotocol.io/tags/protocol/
- [S41] Claude Code channels (research preview) — https://code.claude.com/docs/en/channels
- [S42] Claude Code channels reference (`claude/channel`, permission relay, v2.1.234 restriction) — https://code.claude.com/docs/en/channels-reference
- [S43] Gradle 9.5.0 (2026-04-28), 9.5.1 (2026-05-12) — https://discuss.gradle.org/t/9-5-0/52029 ; current — https://services.gradle.org/versions/current
- [S44] Material 3 Expressive status (material3 1.4.0 stable vs 1.5.0-alpha, secondary) — https://www.skills.sh/talissonvitorino/kmp-ios-skills/m3-expressive
- [S45] Android 17 behavior changes (API 37, shipped 2026-06-16) — https://developer.android.com/about/versions/17/behavior-changes-17
