# Integration and migration plan

Status: proposed implementation, informed by the **2026-10-04** source snapshot in [research](research.md). This document defines the changes needed around HAPI, Hive, Outfit and Rota. The [contracts](contracts.md) own wire/schema names; the [architecture](architecture.md) owns state authority.

## Integration boundaries

Pyre supplies one user-facing CLI/TUI and Android experience. It does not require rewriting every useful component into one language or binary. Adapters can invoke existing tools through explicit machine-readable interfaces, with bounded timeouts and version/capability negotiation.

| Component | Retain | Add or change | Keep out of its responsibility |
|---|---|---|---|
| HAPI candidate | Session transport, harness adapters, runners, native client foundation | Durable peer delivery path, logical/execution identity mapping, account-aware spawning and Pyre adapter | Project memory authority, fleet credential copying |
| Hive | Capture, search, provenance, private vault sync, handoffs, bounded background work | Shared project/session identifiers, immediate explicit note publishing, migrate legacy mail delivery | A second live inbox, mandatory project registration |
| Outfit | Credential custody, connections, generated skills, managed tools, project configuration | Enrolled-host provisioning, effective environment plans, plugin/CLI inventory and staged updates | Account quota prediction, shared plaintext secrets in the hub |
| Rota | Account inventory, provider-specific usage collection, refresh/error knowledge | Session-specific assignment, fleet usage views, coordinated login refresh, same-provider recovery | Global login switching underneath managed sessions |

Preserve existing source repositories and commands during migration. Implement changes in isolated clones/worktrees and reviewable branches. Do not reset, clean, force-push or overwrite a deployed checkout. Start by recording deployed commit, executable origin/version and dirty-file names; recheck deployed differences against the baselines in [research](research.md). Local working changes in a deployed checkout must be preserved and reconciled before it is updated. Store any private backups outside the source tree and repository history.

## HAPI backend adapter

### Prove the boundary before committing to a fork

Build the smallest adapter against the pinned HAPI revision. Keep backend IDs behind the logical session/execution mapping in [contracts](contracts.md). Clients must not depend on raw HAPI session IDs being stable across resume, reopen or account recovery. HAPI documents that some resume paths return a replacement ID.

The adapter must support remote launch, inspect, local/native attach, queue input, explicit steering when advertised, stop/interrupt, resume, questions/approvals and terminal access. A generic capability table is preferable to assuming Claude and Codex have identical controls. Native subagents remain harness-owned, but their progress/questions should remain visible through the parent.

**Concrete gaps found in source/docs:**

- The peer helper sends only text. Assign the canonical message ID once and map it to a durable backend `localId`. Retain the mapping across retries and execution replacement. Read stored/queued/consumed state from the backend; never upgrade an HTTP `ok` to “agent acknowledged.”
- The documented spawn API has no account profile. Add a local, opaque profile reference or implement a runner-side environment resolver. Do not accept arbitrary caller-supplied secret-bearing environment blobs. Record the resolved account identity and executable version in the execution record without storing tokens.
- HAPI distinguishes native Codex concurrent clients from older local/remote ownership. Preserve that distinction. Account/profile changes must be rejected on an existing execution unless the runtime explicitly supports them; otherwise use checkpoint and recovery.
- Terminal-created Codex execution lifetime follows its original terminal. Managed launches therefore need a daemon-owned wrapper/persistent terminal. Closing the TUI or detaching must not terminate the primary process.
- The current shared Codex guide rejects native `--profile` in that path. Do not implement account isolation merely by adding that flag. Test dedicated homes/config composition for the installed harness version.

These observations are supported by the pinned [peer helper](https://github.com/tiann/hapi/blob/86c88df93baf5d1f738dd4b202078bdf6dec376e/cli/src/modules/pingPeer/pingPeer.ts), [machine API](https://github.com/tiann/hapi/blob/86c88df93baf5d1f738dd4b202078bdf6dec376e/docs/api/client-contract/rest.md), and [Codex shared-session guide](https://github.com/tiann/hapi/blob/86c88df93baf5d1f738dd4b202078bdf6dec376e/docs/guide/codex-shared-sessions.md).

### Gate evidence

Run the same small workflow on this Mac and `server` with both providers. Record exact harness/HAPI versions and whether each result is simulated or real:

1. Start remotely, attach in a native terminal, detach, and continue working.
2. Exchange messages in both directions, including a native subagent task and a human reply.
3. Deliver while busy, while waiting for input, after hub reconnect, and after runner restart.
4. Repeat the same send ID after a dropped response; show one stored logical message and no automatic repetition of uncertain work.
5. Select one of two configured subscriptions without changing the other execution's account.
6. Demonstrate account recovery, or report the exact unsupported continuation path and a preserved, linked successor/handoff.
7. Reach the same session from Android over HTTPS/Twingate.

Evaluate isolated patches, maintenance burden and native interaction quality. If essential behavior requires pervasive modifications or remains unreliable, select direct native adapters behind the same Pyre contracts. Record that decision; do not give the same run two competing backend owners. A direct adapter for one harness can coexist with a HAPI adapter for another if that is the gate decision.

## One messaging authority

Pyre's addressed envelope and durable inbox/outbox are the coordination authority. A selected backend may own the native input queue, but it is an adapter delivery mechanism with a stored ID mapping, not a second independent mail system. This distinction is necessary whether HAPI is reused or replaced.

Hive's vendored `ws` already implements mail, channels, pending replies, hooks and `wake_pane`/terminal input. `deliver_unread` marks files read when returning their content, before any proof of model action. Keep legacy read state as an observation; do not import it as a model acknowledgement. [Existing implementation](https://github.com/jhh3/hive/blob/a8d95a74783eb8e3d27748a2878ac6dddc6146b8/plugins/hive/vendor/ws/bin/ws)

Migration sequence:

1. Inventory legacy mailbox IDs, project/stream aliases, unread messages, pending replies and channel cursors. Work from copies and produce counts before altering delivery.
2. Create deterministic import IDs and an alias map. Preserve sender, timestamps, thread/reply relationship and original body; mark legacy delivery evidence explicitly.
3. Import once using idempotent writes. Preserve unresolved reply obligations in the new inbox without forcing all sessions into an obligation workflow.
4. Turn legacy `ws` send/inbox commands into forwarding adapters for enrolled sessions. Disable their direct delivery/wakeup hooks for those sessions at the same cutover. Do not dual-deliver through both `send-keys` and the harness adapter.
5. Retain the original mailbox archive read-only and record a cutover watermark. Rollback must reconcile messages accepted since that watermark before re-enabling a legacy writer.

Use terminal input for explicit human terminal control; do not make unattended `send-keys` the default addressed-agent delivery mechanism. A message landing at a shell prompt can execute text rather than deliver it to an agent. When the supported adapter cannot accept input, retain the message, report the capability/state, and surface attention rather than guessing.

Test migration with duplicate imports, missing reply parents, disconnected senders, Unicode/attachments, channel fan-out and the crash window between runtime acceptance and acknowledgement. The service offers durable records and deduplication within its boundary, not exactly-once external effects. An uncertain send remains visible until reconciled or explicitly resolved.

## Hive: shared knowledge without forced context

Use the existing cited retrieval, transcript provenance and small resume briefs first. Preserve canonical vault data and rebuildable local indexes. Keep automatic capture/enrichment separate from promptly shared explicit notes and project documents. Record sync/index freshness in query responses so the UI and agents can see stale results. [Current operational model](https://github.com/jhh3/hive/blob/a8d95a74783eb8e3d27748a2878ac6dddc6146b8/README.md)

Add a compatibility interface for search, get, note, handoff and context lookup with stable project/session references. Return citations/source IDs and machine/time provenance. Project membership is optional; a standalone session can search global or explicitly scoped knowledge and publish an artifact without creating a project.

Project briefs, decisions and published artifacts are explicit shared records, versioned by the project service. Hive can index them, but must not become a second editable authority for the same document. A project brief should be small and link to details; do not inject every transcript, skill or historical note into every session.

Keep hooks bounded and fail-open; preserve capture-only hooks and bounded background enrichment. Do not install a second cron/service that performs the same sweep. Support the existing disable/breaker behavior through the daemon's health view. Validate capture continuity, cited cross-machine retrieval, offline reads, prompt publication of handoffs, recovery from an interrupted sync and resource consumption under a large import.

**Measured (2026-10-05, memconn):** in a Pyre-launched Claude session with the Hive plugin installed (isolated store), Hive's own `Stop` hook captures each turn (bounded worker, about 130 ms) and `SessionEnd` finalizes; model enrichment runs in a detached worker afterwards, never inside the hook. Search results for those transcripts carry the Pyre session, run and project (`session_ref`, `pyre_session=`, `pyre_run=`), resolved on the node from the run's native session ID. Hive 0.2.3 has no capture hook for Codex, and its `tick` sweep reads only `~/.claude/projects` and `~/.codex/sessions`, so Pyre captures Codex runs itself: when a Codex run ends (holding the run's profile so a rota check-in waits) and at idle boundaries of live runs, the node hands the rollout to `hive mem capture --transcript … --session <thread> --cwd …` from a bounded background queue. Codex 0.160 rollouts lack the `user_message`/`agent_message`/`exec_command` lines Hive 0.2.3's adapter reads (it stores them as "trivial"), so the node passes a private normalized copy that adds those lines; the shim can go once Hive reads the 0.160 format. Hive's SessionStart digest only includes entries with durability ≥ 0.9; a session that reads or changes no file scores 0.84 and is searchable but never recalled (memconn, follow-up).

## Outfit: connections and fleet environments

Use Outfit's current credential-store custody, selected connection modes, workspace policy and managed tool/skill functions as the local authority. Add a fleet catalog of **metadata**: connection/account labels, availability, approved project access, tool versions, skill/plugin revisions and host health. Private material does not belong in project documents, generated skills, messages or logs. [Existing invariants](https://github.com/jhh3/outfit/blob/3c7577f81f07759b1a12e4c328cc853302db6c6d/README.md)

A desired session environment combines machine defaults, explicit project overrides and session overrides. It should resolve to a reviewable effective plan and pinned digest. The plan selects existing connections, compatible tools and relevant skills. Adoption inventories existing installations first and preserves unrelated configuration. Managed sections need ownership markers and backups; edits outside those sections become visible drift rather than being erased.

Credentials can be provisioned to an enrolled machine when the user's configured scope permits it. Use encrypted transport and the destination's credential store, and track which host owns each grant. For device-bound or rotating grants, prefer a separate supported login on each machine or invoke the connection on its owning machine. Do not assume encrypted backup import is safe for simultaneous use of one refresh-token family. A locked or unavailable Linux credential store must produce an actionable error, not a silent plaintext fallback.

Extend managed tooling to Claude/Codex, Pyre, plugins and skill packages: exact versions/checksums where provided, compatibility declarations, staged install, health checks and rollback. Pin each running execution to its resolved executable and configuration digest. Updates affect future starts unless an explicit restart is selected. A failed rollout on one host should not force upgrades or stop healthy sessions elsewhere.

Test credential non-disclosure, two-host provisioning, revoked/expired grants, a locked credential store, existing plugin adoption, local edits, failed update rollback and coexistence with running sessions. Reconcile any locally modified deployed Outfit checkout before claiming deployed parity.

## Rota: account-aware sessions and recovery

The existing Rota switch path changes shared live login files; it does not move a running session to a new identity. The managed launcher must therefore call a new assignment/prepare interface, not `rota switch` against the user's ambient login. [Current implementation](https://github.com/jhh3/rota/blob/eb9f45812902d8f0b3c752334aa6705687fd44bd/rota)

Model three separate things: a provider subscription account, a particular machine's login/grant, and an execution's assignment. Multiple device logins can refer to one subscription and share its reported limits. Distinguish provider-wide usage snapshots from per-session usage; do not sum the same global snapshot once per machine. Unknown or stale usage is not unused capacity.

Provide selection and health responses containing eligibility, observed headroom, observation time, cooldown/reset hints and account identity. Fleet reservations are planning hints, not authoritative provider quota allocations. Start conservatively with short freshness windows and bounded polling; honor provider retry delays. Use fake responses to test exhaustion rather than intentionally burning real subscriptions.

The preparation step creates/chooses a supported isolated login environment. Preserve access to the required plugins, configuration and session history through explicit composition/import, not a symlink to one mutable global credential file. A Pyre run never writes the owner's files: files a harness may rewrite (Claude `settings.json`, `CLAUDE.md`, plugin registries; Codex `hooks.json`, `AGENTS.md`) are per-launch copies, directories (skills, agents, commands, plugin code) are mirrored as per-entry links, and harness-written state (`plugins/synced`, `skills/synced`, plugin `data`/`store`, Codex `skills/.system`) stays in the profile (evidence). Test keychain identity isolation on macOS and credential/home behavior on Linux. Rota's current CLI account support differs by deployed version; inventory it before migration.

Avoid concurrent refresh against a shared rotating grant. Prefer independent provider-supported device logins; when a grant must be shared, assign a refresh owner and a serialized, generation-checked update path that includes the harness's own refresh behavior. A Rota-only lock does not coordinate an unaware native CLI. If a provider cannot be used safely that way, refuse that sharing mode and request another device login. Never place refreshed tokens in ordinary sync Git history.

**Measured (2026-10-04, accounts report):** `rota switch` rewrites the live login and a running Claude session follows it on its next turn, so managed sessions never use the default profile and Pyre never invokes `rota switch`; a rota snapshot is a live login and must not seed a Pyre profile; `rota usage` can refresh tokens, so Pyre's usage probes call the provider endpoints with refresh disabled and report stale/unknown instead. Codex has no cross-process refresh lock: at most one Codex process per `CODEX_HOME`.

Recovery follows the user's policy: **another configured subscription for the same provider, then notify if none remain**. Do not enable API billing, spend credits, change providers, or substitute a model tier as implicit fallback. Preserve work, pending messages and account history. End/quiesce the old execution before its replacement takes ownership. Resume the native thread only when verified supported across accounts; otherwise create a linked successor with a handoff and make that limitation visible. Do not automatically repeat a tool call whose external effect is uncertain.

Test two parallel accounts on one host, one subscription through independent logins on two hosts, refresh races, stale usage, cooldown, exhausted pools, explicit account preference, process crashes during replacement and cross-account continuation for each harness. This can reduce interruptions, not guarantee uninterrupted execution or perfect limit prediction.

## Android and notifications

If HAPI passes the backend gate, extend its Kotlin/Compose client and share protocol fixtures. Keep a distinct app identity and stable release signing key for sideloaded updates. Signing secrets and Firebase service-account keys stay outside source and APK contents. A build without Firebase must remain useful and explicitly show push as unavailable. [Build and push configuration](https://github.com/tiann/hapi/blob/86c88df93baf5d1f738dd4b202078bdf6dec376e/android/README.md)

Add Pyre project documents/artifacts, fleet/account views, inbox threads and connection status. Support review and replies before attempting a native terminal renderer. For terminal access, launch a tested authenticated web view/browser route; do not leak bearer tokens in URLs or relax HTTPS validation. Test on the user's actual Android with Twingate, including after sleep and switching networks. [Current native/web boundary](https://github.com/tiann/hapi/blob/86c88df93baf5d1f738dd4b202078bdf6dec376e/docs/guide/native-apps.md)

Generate notification records from the durable event stream, with deduplication, quiet hours and deep links. Test stale approval actions, double-tapped replies, foreground/background/locked-device delivery and exhausted account alerts. Push is a wakeup/attention mechanism; the client fetches canonical current state before a control action. Configuration for FCM can be prepared with a Firebase-free build; real push acceptance requires the matching infrastructure and physical-device test.

## Completion evidence

Each integration change must report the original baseline, migration and rollback procedure, automated tests, real-machine checks and remaining unsupported capabilities. Keep third-party notices and version pins. A mocked adapter can unblock parallel UI development; it cannot satisfy native-terminal, credential-isolation, account-recovery or Android push acceptance. The [acceptance criteria](acceptance.md) remain the release boundary.
