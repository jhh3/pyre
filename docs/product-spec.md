# Product specification

Pyre connects one person's coding agents and terminal work across Mac and Linux machines. It provides a polished terminal interface, a scriptable interface for agents, and a sideloadable Android companion. The product must make remote work, communication, context, account selection, and environment maintenance feel like one coherent system.

## User and current environment

The user is one developer with a few Macs and Linux machines, already reached through SSH aliases over a private network (for example a VPN or mesh overlay); `server` stands for an always-on Linux machine. New sessions may start through a shared launcher; transparent takeover of arbitrary pre-existing terminal tabs is not a launch requirement.

The user runs Claude Code and Codex with subscriptions and has existing skills, plugins, and local customization. Three personal repositories provide foundations:

- [Hive](https://github.com/jhh3/hive): memory, workstreams, sessions, handoffs, and existing mail/channel concepts.
- [Outfit](https://github.com/jhh3/outfit): connector credentials, tool management, skills, and project environments.
- [Rota](https://github.com/jhh3/rota): account snapshots, usage visibility, and manual switching.

The user rejected Agent Deck and does not want its workflow recreated. The user is open to modifying or rewriting the three personal tools where justified, installing a daemon per machine, using a modest shared service, and building/sideloading an Android app.

## Product principles

1. **Capabilities before ceremony.** Humans and agents can discover, start, inspect, attach, message, and manage work without a mandatory workflow engine.
2. **Projects are useful and optional.** A project supplies shared context and reviewable output, not a condition of starting a session.
3. **Native work stays native.** Preserve the official agent terminal experience and native subagents. The management interface surrounds it.
4. **One coherent view.** A result, message, account state, or document should refer to the same identity across terminal, CLI, Android, and backend.
5. **Continuity is explicit.** Show stale state and uncertain outcomes. A reconnect must not manufacture completion or silently duplicate work.
6. **Use existing foundations.** Validate reusable implementations and learn from established designs before adding another custom mechanism.
7. **Small operating footprint.** Prefer existing hosts and a small number of processes. Resource-heavy indexing/enrichment is bounded background work.

These principles guide implementation. They are not a long prompt to inject into every agent session. The normal session receives a short tool guide and, when relevant, a concise project brief with retrieval pointers.

## Essential scenarios

### Work from any machine

Open Pyre on a laptop, see work on the laptop and server, preview a session, attach to its real terminal, answer a question, detach, and return to the fleet overview. Closing this UI does not end the execution it was viewing.

### Delegate without a prescribed hierarchy

Tell any session to launch work on a particular machine. It discovers capabilities, starts a peer, exchanges messages and results, and continues its own work. The user may instead launch everything manually or use one agent to supervise a goal. Native subagents work inside either session without registration ceremony.

### Coordinate through a project

Create a project with a brief, documents, repositories, and a few connections. Two sessions on different machines consult the same current brief, use separate worktrees or one chosen checkout, publish outputs, and respond to review comments. The user sees the documentation and artifacts from Android even after the producing machine sleeps.

### Do quick work with no project

Start an agent or shell in a directory, communicate with it, and later publish an output or associate it with a project. No placeholder project needs to be created.

### Continue through account limits

Launch using an eligible account for the chosen provider. See headroom and active assignments across the fleet. On a limit or unusable login, preserve work and use a supported recovery path on another configured subscription for the same provider. If none are usable, notify the user and retain a resumable session. Do not silently change Claude work into Codex work or start spending through an unconfigured API account.

### Maintain the fleet once

See installed agent/tool versions, skill/plugin configuration, and drift. Apply a versioned environment to a selected machine or project, stage updates, preserve active sessions, and roll back a failed update. Existing local customizations have a visible reconciliation path.

## Requirements

Each requirement is mandatory for the complete product unless a row explicitly calls a feature optional. Gates sequence delivery; they do not erase later requirements. See [acceptance.md](acceptance.md) for observable tests.

| ID | Requirement | Observable result |
| --- | --- | --- |
| R01 | Fleet connectivity | Enroll Mac/Linux nodes using existing access; show identity, connectivity, capabilities, health, and last-seen time; reconnect without duplicate machines. |
| R02 | Persistent sessions and native control | Start, list, preview, attach, detach, interrupt, stop, and resume managed Claude/Codex sessions from another machine; preserve native subagent behavior. |
| R03 | Agent-driven orchestration | Agents can discover machines/sessions, launch peers, inspect output, exchange messages, await replies/events, and publish results through the same underlying operations as the UI. No fixed coordinator hierarchy is required. |
| R04 | Reliable messaging | Direct and threaded messages, optional channels, durable IDs, offline queuing, replay-safe reconciliation, explicit delivery/acknowledgement state, sender identity, and visible uncertain outcomes. |
| R05 | Optional projects | Stable project identity with brief, machine-local paths, zero or more repositories, session associations, and environment references. Support shared checkout and separate worktree workflows. Standalone sessions retain full basic capability. |
| R06 | Shared documents, artifacts, and review | Versioned text, immutable published artifact versions, provenance, links, comments, previews/downloads, and conflict detection. Published output survives loss of access to the producing node. |
| R07 | Shared memory | Hive captures and retrieves relevant history across machines; new sessions can receive a bounded brief; explicit notes/handoffs become available promptly; freshness/provenance are visible. |
| R08 | Fleet account management | Rota-backed account health and usage, session-specific assignment, coordinated login/refresh state, same-provider selection/fallback, resumable recovery, and exhausted-pool notification. |
| R09 | Connections and credentials | Outfit-backed shared connection catalog, verified availability, credential provisioning or remote execution where appropriate, and clear machine/project scope. Secrets are not project content. |
| R10 | Configuration, skills, and plugins | Inventory existing customization; materialize a consistent global/project/session environment for both harnesses; detect conflicts/drift and report changes requiring restart. |
| R11 | Version and update management | Inventory and pin versions, stage and verify updates, preserve a running session's executable/config revision, handle backend/client compatibility, and provide rollback. |
| R12 | Terminal experience | Fast keyboard-driven navigation, search, previews, native attachment, readable attention states, project/document/artifact views, and accessible management views. |
| R13 | Android access and notifications | Buildable sideloaded app that can view/respond/launch/review/approve where supported, access published project content, and receive actionable background notifications. Terminal access may initially open the authenticated web terminal. |
| R14 | Resilience and resource safety | Sessions survive client and hub outages where their local runtime permits; state is recoverable; background work is bounded; backups, revocation, compatibility, and safe migration are exercised. |

## Projects and shared output

A project is a durable shared workspace, not necessarily a Git repository. It may link multiple code repositories, research folders, documents, remote services, and sessions. Its minimum required fields should be an ID and display name; all other structure is added as useful.

The project home provides a brief, documentation, artifacts, discussion/review, activity, and associated sessions. An optional checklist can help coordinate work, but completing a rigid task schema is never required to send a message or start a worker.

Project documents are authored/versioned knowledge. Hive memory is searchable historical evidence and derived context. Artifacts are published output. Avoid silently promoting every transcript or model-generated summary into authoritative project documentation.

Publishing has a durable receipt. The producer must distinguish a local file from a published version. Comments reference a specific version, and an agent can be notified of a review without replacing the artifact. HTML artifacts must be displayed as untrusted content isolated from the management application's credentials.

## Account and environment behavior

Account selection attaches to the session/run, not a machine-wide mutable default. Several simultaneous sessions may use different configured accounts. An account is a subscription identity; its per-device grants and refresh state are separate records.

Account telemetry is sampled and may be stale. Display timestamps and reset information when available. Scheduling heuristics can reduce collisions but cannot promise reserved provider quota or uninterrupted service. Failure recovery must distinguish a proven unexecuted prompt from a potentially executed operation.

Outfit prepares the environment for the selected session: tools, connection references, applicable skills/plugins, and configuration. Global preferences work for projectless sessions; project and session overlays can narrow or override supported settings. The UI explains which layer supplied a value without forcing the user to understand implementation paths.

## Scope boundaries

- First-class targets are macOS and Linux; the phone is Android. Windows/iOS may remain backend-supported but are not delivery requirements.
- Adoption of arbitrary pre-existing plain-terminal processes is deferred. Existing managed or resumable sessions should be handled when a verified adapter supports them.
- Moving a live process between machines is not required. Remote attachment is primary; cross-machine continuation may reconstruct a checkout and resume or hand off context through an explicit supported action.
- A multi-user organization, mandatory ticket tracker, public agent marketplace, Kubernetes deployment, custom terminal emulator, and general distributed filesystem are not prerequisites.
- Autonomous orchestration is enabled by tools and events. A permanently running LLM coordinator is optional, not infrastructure required to keep sessions alive.
- Native subagents are surfaced through parent relationships when exposed by the harness. Do not promise independent attachment or migration for every native child.

## Product acceptance

The product is complete only when a user can run the essential scenarios on real Mac/Linux nodes and a sideloaded Android device, with the failure cases and integration checks in [acceptance.md](acceptance.md) passing. A mock-only demo, a documentation bundle, or a session picker without the account/environment/project capabilities is an intermediate deliverable.
