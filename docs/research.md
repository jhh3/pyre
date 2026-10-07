# Research and reuse decisions

Snapshot: **2026-10-04**. This is the evidence behind the [product spec](product-spec.md), [architecture](architecture.md), and [integration plan](integrations.md). It records a design investigation, not a successfully deployed system.

## How to read the evidence

- **Source observation:** inspected implementation at the linked commit.
- **Documentation claim:** upstream describes the behavior; not independently demonstrated here.
- **Machine observation:** read-only inspection of the user's environment during planning; recheck before changing it.
- **Proposal:** behavior Pyre must implement and verify.
- **Unverified runtime:** a requirement whose feasibility or actual operation still needs a recorded experiment.

These distinctions matter: a transport accepting a message is not proof that a model acted on it, a usage sample is not a quota reservation, and documentation about local/remote control does not prove the user's existing native terminal workflow will feel good.

## Source baselines

Use these revisions to reproduce this investigation. Newer upstream revisions may be adopted after reviewing their changes and rerunning the relevant compatibility checks. Do not silently substitute `main` during implementation.

| Repository | Inspected revision | Intended relationship |
|---|---|---|
| [jhh3/hive](https://github.com/jhh3/hive/tree/a8d95a74783eb8e3d27748a2878ac6dddc6146b8) | `a8d95a74783eb8e3d27748a2878ac6dddc6146b8` | Retain memory and migrate overlapping coordination |
| [jhh3/outfit](https://github.com/jhh3/outfit/tree/3c7577f81f07759b1a12e4c328cc853302db6c6d) | `3c7577f81f07759b1a12e4c328cc853302db6c6d` | Retain connections, credentials, tools and skills; add fleet management |
| [jhh3/rota](https://github.com/jhh3/rota/tree/eb9f45812902d8f0b3c752334aa6705687fd44bd) | `eb9f45812902d8f0b3c752334aa6705687fd44bd` | Extend account inventory into isolated session assignment and recovery |
| [tiann/hapi](https://github.com/tiann/hapi/tree/86c88df93baf5d1f738dd4b202078bdf6dec376e) | `86c88df93baf5d1f738dd4b202078bdf6dec376e` | First candidate session backend and Android starting point |
| [MCP Agent Mail](https://github.com/Dicklesworthstone/mcp_agent_mail/tree/3fad5ec672869f81d2ca0a4c525dde004ac96f45) | `3fad5ec672869f81d2ca0a4c525dde004ac96f45` | Messaging and optional reservation precedent |
| [Gas Town](https://github.com/gastownhall/gastown/tree/649b832b7672bc7a2dbef26f5983aba6198b819b) | `649b832b7672bc7a2dbef26f5983aba6198b819b` | Durable work/identity and recovery precedent |
| [Beads](https://github.com/gastownhall/beads/tree/a93498c81f629da9a364cc9fd983bbab2dad0958) | `a93498c81f629da9a364cc9fd983bbab2dad0958` | Optional persistent work/dependency graph precedent |
| [Superset](https://github.com/superset-sh/superset/tree/01dff43f1e2b692ee40e5b5a3826ebc1cc6544f3) | `01dff43f1e2b692ee40e5b5a3826ebc1cc6544f3` | Headless host, terminal supervision and fleet-management comparison |
| [Happy](https://github.com/slopus/happy/tree/dafe9a51858f72c6f04a9337f3a93c87f603b5b7) | `dafe9a51858f72c6f04a9337f3a93c87f603b5b7` | Remote-agent CLI, encrypted sync and mobile comparison |

**Machine observations:** deployed checkouts on a user's machines can differ from the GitHub baselines above (for example an Outfit checkout with local modifications). Preserve and inspect local changes; GitHub HEAD is not evidence of deployed behavior. Do not copy credentials, private transcripts, or full machine inventories into this repository.

## HAPI: reuse candidate, subject to a compatibility gate

**Documentation claim:** HAPI already supplies a self-hosted SQLite hub, machine runners, remote agent spawning, REST/SSE clients, terminal wrappers, peer discovery/messaging, and native clients. This is enough useful infrastructure to justify a bounded integration experiment before building a replacement. [Architecture and runner guide](https://github.com/tiann/hapi/blob/86c88df93baf5d1f738dd4b202078bdf6dec376e/docs/guide/how-it-works.md)

**Source observation:** `pingPeer.ts`'s `sendMessage` posts `{ text: message }` without `localId`. **Documentation claim:** the fuller message contract uses `localId` for queued state, consume/cancel tracking and reconciliation; sends without it are stamped invoked on insertion. Therefore a successful `ping-peer` call is not sufficient evidence of durable end-to-end delivery for Pyre. Extend the adapter or upstream helper; do not build reliability around that shortcut. [Peer implementation](https://github.com/tiann/hapi/blob/86c88df93baf5d1f738dd4b202078bdf6dec376e/cli/src/modules/pingPeer/pingPeer.ts), [queue contract](https://github.com/tiann/hapi/blob/86c88df93baf5d1f738dd4b202078bdf6dec376e/docs/api/client-contract/pagination.md)

**Documentation claim:** the machine spawn request exposes agent, directory, model and runtime settings, but no account/profile selector. Machine availability checks do not validate the actual login. **Proposal:** resolve an account assignment before spawning and pass an opaque local profile reference through a tested adapter extension. A profile name must never stand in for account verification. [Machine REST contract](https://github.com/tiann/hapi/blob/86c88df93baf5d1f738dd4b202078bdf6dec376e/docs/api/client-contract/rest.md)

**Documentation claim:** Codex 0.154.0+ can share one app-server between the native terminal and HAPI clients. The launching terminal owns terminal-created execution lifetime; additional terminal attachment only detaches on exit. Each execution inherits its original authentication and `CODEX_HOME`; cold resume needs the same store and credentials. HAPI also documents native queue recovery and uncertain-write quarantine. These distinctions are essential when promising persistent sessions. [Shared Codex guide](https://github.com/tiann/hapi/blob/86c88df93baf5d1f738dd4b202078bdf6dec376e/docs/guide/codex-shared-sessions.md)

**Unverified runtime:** Claude's local/remote handoff, simultaneous human and agent messages, native subagents, session-specific credentials, and cross-account continuation on both operating systems. Do not infer Claude parity from Codex's shared runtime. The backend gate must demonstrate both. HAPI's general handoff guide describes mode switching; the actual installed versions and user experience are the acceptance target. [Handoff documentation](https://github.com/tiann/hapi/blob/86c88df93baf5d1f738dd4b202078bdf6dec376e/docs/guide/how-it-works.md#seamless-handoff)

**Documentation claim:** HAPI Android is Kotlin/Jetpack Compose and already has session creation, chat, attachments, questions and approvals. Private Firebase builds require matching client configuration and a hub service account; a Firebase-free build has no FCM push. Native Android rejects HTTP hub origins. Full remote terminal access currently belongs to the web client. **Proposal:** extend this app if the backend passes; start terminal access through a tested authenticated web route. [Android build/push instructions](https://github.com/tiann/hapi/blob/86c88df93baf5d1f738dd4b202078bdf6dec376e/android/README.md), [native client boundaries](https://github.com/tiann/hapi/blob/86c88df93baf5d1f738dd4b202078bdf6dec376e/docs/guide/native-apps.md)

**Source observation:** HAPI's root license is GNU AGPL version 3. Record original notices and component-specific licenses for any imported code. This document does not decide redistribution or derivative-work questions. Keep provenance explicit and review the exact files being reused when selecting the integration form. [License](https://github.com/tiann/hapi/blob/86c88df93baf5d1f738dd4b202078bdf6dec376e/LICENSE)

## Existing personal tools are part of the foundation

**Hive — source and documentation observations.** Hive already has memory capture, retrieval, private Git vault sync, handoffs, worktrees and workstreams. Its vendored `ws` includes mailboxes, reply obligations, channel cursors, terminal wakeups and terminal input. `deliver_unread` moves a message to the read directory when returning text; its “exactly-once” comment does not establish exactly-once model execution. Pyre must consolidate this existing delivery authority instead of adding a second competing inbox. Preserve its local retrieval and bounded background model. [Hive overview](https://github.com/jhh3/hive/blob/a8d95a74783eb8e3d27748a2878ac6dddc6146b8/README.md), [vendored coordination implementation](https://github.com/jhh3/hive/blob/a8d95a74783eb8e3d27748a2878ac6dddc6146b8/plugins/hive/vendor/ws/bin/ws)

**Outfit — documentation observations.** Outfit already declares named connections, OS credential-store custody, verified managed CLIs, selective skill generation, secret-free project policy and encrypted portable backups. Fleet provisioning should extend those boundaries. It must preserve existing logins, workspace policies and unrelated configuration rather than replacing every host's configuration with one global directory. [Overview and invariants](https://github.com/jhh3/outfit/blob/3c7577f81f07759b1a12e4c328cc853302db6c6d/README.md)

**Rota — source observation.** Both Claude and Codex switches replace the relevant shared live login state. The Codex switch implementation explicitly reports that already-running sessions retain their identity. **Documentation discrepancy:** the current README's Codex sections describe switching/snapshots, while a later limitations paragraph says those do not exist. The implementation supports them; reconcile the stale text during integration. Multi-machine refresh ownership remains a design problem, not a solved fleet feature. [Implementation](https://github.com/jhh3/rota/blob/eb9f45812902d8f0b3c752334aa6705687fd44bd/rota), [README](https://github.com/jhh3/rota/blob/eb9f45812902d8f0b3c752334aa6705687fd44bd/README.md)

## Other designs worth learning from

| Precedent | Evidence and useful idea | Pyre decision |
|---|---|---|
| MCP Agent Mail | **Documentation:** stable agent identities, threaded inboxes, searchable archives, attachments and voluntary file reservation leases. [README](https://github.com/Dicklesworthstone/mcp_agent_mail/blob/3fad5ec672869f81d2ca0a4c525dde004ac96f45/README.md) | Adopt the distinction between mail and work, explicit replies, inspectable history and optional reservations. Do not add another mailbox server by default. |
| Gas Town | **Documentation:** separates organizational coordination, agent identity and project implementation records. [Architecture](https://github.com/gastownhall/gastown/blob/649b832b7672bc7a2dbef26f5983aba6198b819b/docs/design/architecture.md) | Work and identity can survive execution replacement. Do not impose its named hierarchy on the user. |
| Beads | **Documentation:** durable dependency-aware work graph, claim/ready operations, JSON interfaces and synchronization. This pinned revision is Dolt-based. [README](https://github.com/gastownhall/beads/blob/a93498c81f629da9a364cc9fd983bbab2dad0958/README.md) | Provide optional work tracking; consider an adapter later. No Beads/Dolt dependency or mandatory task ticket for every session. |
| Codex app-server | **Official documentation:** structured thread/turn lifecycle, steering, approvals and account/rate-limit endpoints. [App-server reference](https://learn.chatgpt.com/docs/app-server) | Prefer supported protocol events over parsing screen text. Generate/version compatibility fixtures from the installed binary. |
| Claude channels | **Official documentation:** local MCP channel bridges can deliver events and expose reply tools. Custom channels are in research preview and need a development flag/consent; organization policy can still disable them. [Channel reference](https://code.claude.com/docs/en/channels-reference) | Evaluate as a capability-gated adapter; do not assume unattended availability or make it the only path. |
| NATS JetStream | **Official documentation:** durable storage, per-consumer progress, acknowledgement and redelivery. [JetStream guide](https://docs.nats.io/learn/jetstream/) | Learn the delivery semantics; use the selected hub's transactional outbox/inbox first. Introduce a broker only for an observed need. |

## Superset and Happy: viable components, different integration costs

**Superset — documentation claims:** the pinned revision supports a standalone `superset start --daemon`, workspace/agent management, automatic host updates with health checks and rollback, and a separate terminal daemon. Its systemd instructions explicitly preserve terminals across host-service restarts. A terminal-first client over this service is therefore plausible; disliking its desktop UI does not rule out its backend. Reusable patterns include lifecycle separation, worktree setup, versioned host discovery and update recovery. [Headless host guide](https://github.com/superset-sh/superset/blob/01dff43f1e2b692ee40e5b5a3826ebc1cc6544f3/apps/docs/content/docs/cli/host-server.mdx)

**Source observation:** `terminals send` resolves an organization/workspace/host and calls a terminal write API with text and a submit flag. That is useful terminal control, but this inspected path does not establish Pyre's runtime-acceptance/acknowledgement semantics. **Documentation claims:** standard remote discovery uses Superset authentication, organization membership and its relay; project setup creates cloud records. The documented mobile offering is iPhone. [Terminal command](https://github.com/superset-sh/superset/blob/01dff43f1e2b692ee40e5b5a3826ebc1cc6544f3/packages/cli/src/commands/terminals/send/command.ts), [remote access](https://github.com/superset-sh/superset/blob/01dff43f1e2b692ee40e5b5a3826ebc1cc6544f3/apps/docs/content/docs/remote-access.mdx), [README](https://github.com/superset-sh/superset/blob/01dff43f1e2b692ee40e5b5a3826ebc1cc6544f3/README.md)

**Proposal:** prefer HAPI's gated session/client integration first. Adopting Superset's standard deployment would add account/organization/relay assumptions and still require Pyre's Android and durable mail layers. Extracting its host service could fit, but the work to remove those dependencies needs a spike; it is not ruled out on desktop aesthetics.

**Happy — documentation claims:** Happy supplies Android/web clients, wrapped Claude/Codex sessions, encrypted sync, and `happy-agent` commands for machine discovery, remote spawning, messages, history and waiting. Its self-host package uses embedded PGlite and filesystem uploads without requiring PostgreSQL, Redis or S3. It is a credible small-infrastructure alternative. [Agent CLI](https://github.com/slopus/happy/blob/dafe9a51858f72c6f04a9337f3a93c87f603b5b7/packages/happy-agent/README.md), [self-host package](https://github.com/slopus/happy/blob/dafe9a51858f72c6f04a9337f3a93c87f603b5b7/packages/happy-server-self-host/README.md)

**Source observation:** the inspected agent client's `sendMessage` encrypts and emits a socket message without supplying a `localId` or returning a delivery receipt. This helper alone cannot satisfy Pyre's delivery contract. [Session client](https://github.com/slopus/happy/blob/dafe9a51858f72c6f04a9337f3a93c87f603b5b7/packages/happy-agent/src/session.ts)

**Proposal:** retain Happy as the closest alternative backend candidate. HAPI goes first because its documented shared Codex runtime, queue reconciliation and Kotlin client directly match the current implementation experiments. This is an evidence-based experiment order, not a verified reliability ranking. Happy still needs the same native-control, account-isolation and failure-delivery tests. Neither project's documentation demonstrates the complete Hive/Outfit/Rota integration.

Agent Deck remains excluded by explicit user preference.

## Decisions the initial experiments must settle

1. Can a wrapper preserve native Claude/Codex interaction while reliable, addressed messages arrive from agents and Android?
2. Can each runtime bind to an isolated subscription login on macOS and Linux, without changing another running session's identity?
3. Can a saved conversation continue under a different same-provider account, or must recovery create a linked successor with a handoff?
4. Does the user's Android reach an HTTPS hub over their actual Twingate setup, including after network changes and sleep?
5. Can hub and runner restarts preserve local work, pending input and truthful status without replaying uncertain side effects?

Record versions, commands, redacted results and limitations in a gate report. Read the [contracts](contracts.md) and [acceptance criteria](acceptance.md) before deriving implementations from these precedents. Research claims alone do not satisfy a gate.
