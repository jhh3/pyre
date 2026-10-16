# Shared contracts

Status: **proposed v0 semantics for parallel implementation**, not an implemented API. WP00 must convert these into a small versioned provisional schema and conformance fixtures. G0 validates or amends backend-dependent details after the runtime/account spikes and records the decision. Workers can build against the shared initial version immediately but must not independently declare incompatible interfaces stable.

These contracts define behavior at component boundaries. They do not prescribe internal frameworks, mandatory task roles, or a universal workflow. Read [architecture.md](architecture.md) for ownership and deployment decisions and [product-spec.md](product-spec.md) for R01–R14.

## 1. Conventions and scope

- Stable opaque IDs identify nodes, sessions, runs, projects, messages, documents, revisions, artifacts, and accounts. Human names are mutable aliases, never deduplication keys.
- Times are UTC timestamps for display and diagnosis. Durable sequence/cursor values determine ordering; wall-clock order across hosts is not reliable.
- Every mutating call has an `operation_id`, actor identity, and optional correlation/reference IDs. Repeating the same operation ID with the same body returns the existing result; reusing it with a different body returns a conflict.
- A successful durable-write response means committed storage, not completed downstream execution. Asynchronous work returns an operation/reference to observe.
- `project_id` is nullable for sessions, messages, notes, and artifacts. Standalone usage is a first-class conformance case.
- Credentials, raw tokens, environment secrets, private keys, and session authorization grants are not serializable application metadata.
- Errors use stable codes plus a human explanation, retryability, and relevant object reference. At minimum distinguish unavailable, unauthorized, conflict, unsupported, capacity exhausted, and outcome indeterminate.
- Adapter capabilities and schema/protocol versions are explicit. An unsupported action fails clearly; it must not silently degrade to unverified terminal typing.

Use a single schema owner and generated/shared types once implementation language is chosen. JSON is the default external interchange format; exact endpoint names and programming-language type names below are illustrative. [R01–R14]

## 2. Core records

| Record | Minimum fields and invariants |
|---|---|
| `Node` | `node_id`, name, platform, architecture, capabilities, current versions, last heartbeat, connection state, resource summary; offline never means idle |
| `Session` | `session_id`, provider/harness, optional `project_id`, title, creation actor/time, current run reference, lineage; persists across runs |
| `Run` | `run_id`, `session_id`, node, working-directory binding, runtime/version, environment generation, account binding reference, lifecycle observations, native resume/thread reference if supported |
| `Project` | `project_id`, name, brief document reference, repo/checkouts references, archive state; no mandatory board or task state |
| `DocumentRevision` | document ID, revision ID, parent/base revision, content digest, author, creation time, content or blob reference |
| `ArtifactVersion` | artifact ID, immutable version ID, content digest, size, media type, display filename, storage reference, author, optional project/session/run provenance |
| `ReviewComment` | comment ID, author, target document revision or artifact version, optional anchor, text, reply reference, timestamps |
| `AccountObservation` | account ID, provider, reporting node, observation time, source, confidence, eligibility, usage/reset hints; never a raw credential |
| `EnvironmentGeneration` | immutable profile revision, runtime versions, configuration references/digests, skills/plugins inventory, named connection grants; resolved locally before launch |

Provider-specific metadata belongs in namespaced adapter data rather than new universal mandatory fields. Keep account identity separate from device login/credential handle and from a run's fixed assignment. [R02, R05–R11]

## 3. Session actions

| Action | Required meaning |
|---|---|
| `list / inspect` | Return current observed state, source time, connection freshness, project/account references, and adapter capabilities |
| `start` | Idempotently create a session/run on an explicit node, or on a node selected by a caller-supplied rule; validate directory, environment, capacity, and account before launch |
| `read / watch` | Read bounded transcript/activity with cursors, provenance, and any known gaps; redact secrets at capture where feasible |
| `attach` | Return/establish an authenticated attach mechanism for this exact run; state whether it is native terminal, shared backend UI, or read-only view |
| `send` | Persist an addressed message and return its delivery state; normal messages queue according to adapter input availability |
| `steer` | Explicit immediate steering only where supported; distinct from queued mail and traceable to the actor |
| `answer` | Answer a specific pending request/approval ID for its originating run; validate it is still current |
| `interrupt / stop` | Explicitly interrupt current agent work or stop a run, respectively; report observed outcome separately from command acceptance |
| `resume` | Create a new run from a validated continuation/checkpoint reference, preserving lineage; expose whether continuation is native or a reconstructed handoff |

Launch requests include `operation_id`, `node_id`, harness/provider, a directory or project checkout binding, optional project ID, requested environment profile, and account policy. The default account policy is eligible configured subscription for that provider; crossing providers is never implicit.

Define run observations narrowly: starting, running, waiting for input, waiting for approval, exiting, exited, failed, or unknown. Include a reason and freshness. A status may be unknown because the observer lost contact; that does not mutate the remote process into a failure. Completion of a user goal is an explicit result/report, not equivalent to process exit or a quiet terminal.

Native subagent observations can include parent/child references if provided by the harness, but no implementation may require exposing them as independent fleet sessions. [R02, R03, R08, R12]

## 4. Runtime adapter boundary

Each adapter declares supported capabilities, implemented actions, tested harness versions, and platform limitations. The minimum boundary is:

```text
probe() -> version, capabilities, health
launch(resolved_launch) -> run_handle
observe(run_handle, cursor?) -> bounded events + next cursor
deliver(run_handle, message_id, input) -> accepted | rejected | indeterminate
reconcile(run_handle, message_id) -> evidence | unknown
attach(run_handle, actor) -> attach descriptor
interrupt(run_handle) -> command receipt
stop(run_handle) -> command receipt
checkpoint/resume(...) -> continuation reference, if supported
answer(run_handle, request_id, answer) -> receipt, if supported
```

`resolved_launch` contains local credential references and resolved executable/configuration paths; these are not returned to general clients. Attach descriptors are short-lived and authorized; do not log embedded access tokens. A controller reconnecting to a live run must not launch a duplicate process.

The adapter must document how it knows input was accepted, how it handles busy runtimes, whether it supports input idempotency, and what happens if it crashes between delivery and recording the receipt. If it cannot demonstrate safe automatic delivery in a state, queue the message or report unsupported. [R02, R04, R08, R14]

## 5. Message envelope and delivery

Proposed envelope:

```json
{
  "schema_version": 1,
  "message_id": "opaque-message-id",
  "operation_id": "opaque-origin-operation-id",
  "sender": {"kind": "session", "id": "opaque-session-id"},
  "recipient": {"kind": "session", "id": "opaque-session-id"},
  "project_id": null,
  "thread_id": null,
  "in_reply_to": null,
  "mode": "queue",
  "body": {"text": "Review the published artifact and reply with findings."},
  "attachments": [{"artifact_version_id": "opaque-version-id"}],
  "created_at": "2026-10-04T12:00:00Z"
}
```

An authenticated session cannot claim a different sender. A delegated actor records both its actual identity and any authorized on-behalf-of relationship. Client timestamps are informational. Attachments reference authorized published artifacts; a filesystem path alone is not transferable.

Keep independent facts rather than one misleading progress enum:

| Field | Values / interpretation |
|---|---|
| Hub storage | pending locally, stored, rejected |
| Runtime delivery | queued, attempting, accepted, rejected, indeterminate |
| Recipient acknowledgement | none, explicit acknowledgement reference, reply reference |
| Work outcome | absent, or explicit result reference; separate from mail delivery |

Receipts contain message ID, recipient session/run ID when bound, attempt ID, observer, durable event ID/time, and evidence category. Runtime acceptance means a supported input interface accepted the message, not that an LLM read, understood, or completed it. Acknowledgement is not required for every informational message.

### Required delivery rules

1. Persist the origin outbox before reporting local acceptance. Persist hub storage before reporting `stored`. Persist the recipient inbox before acknowledging node delivery.
2. Deduplicate message storage by message ID. The same ID with different content is a conflict.
3. Claim a delivery attempt durably before invoking the runtime. Bind it to a particular run when the attempt begins.
4. Serialize input delivery per run when required by the adapter. Preserve hub-accepted order per recipient unless an explicit steering action intervenes. Display any gap rather than inventing global ordering.
5. Record runtime receipt evidence. A runtime idempotency key may support safe retry; absence of evidence never proves non-execution.
6. Crash after possible acceptance without a reconcilable receipt produces `indeterminate`. Surface it in CLI, TUI, Android, and agent tools.
7. A replaced run may consume clearly unattempted queued mail. Previously accepted or indeterminate messages retain their original run binding and require reconciliation before any reissue.
8. An explicit reissue creates a new message referencing the earlier one and records who accepted the duplication risk. Transport retries of the original ID must not become new work.
9. Reconnection exchanges cursors and outstanding IDs, reconciles durable state, and then resumes delivery with bounded backoff/concurrency.
10. Retention/expiry is explicit. Expired queued messages receive a terminal delivery explanation; they must not disappear without a record.

The system promises durable records, observable delivery, and deduplicated storage. It does not promise exactly-once agent actions. [R04, R14]

## 6. Events, cursors, and recovery

A durable event contains an event ID, originating component/node ID, originating monotonic sequence, object reference, kind, payload version, and timestamp. The hub assigns a stream cursor on ingest. The cursor orders the shared view by ingestion; it does not assert causal order between hosts.

Consumers persist their last committed cursor. Repeating an event is safe; skipping an acknowledged-but-unapplied event is not. A cursor older than retention returns a reset-required response and snapshot reference instead of silently continuing from the present. Event handlers must not treat event delivery as permission to repeat an external side effect.

Node heartbeat/status reports are replaceable observations. Message receipts, document revisions, artifact publications, account assignments, and approvals are durable facts. Loss of a heartbeat does not delete durable objects or assert that running processes stopped.

Recovery fixtures must include hub restart, node restart with runtime alive, network partition, duplicate send, stale cursor, lost runtime receipt, process exit, and a host returning after a replacement run was created elsewhere. Reconciliation cannot create two current runs accidentally; an explicit concurrent run/fork gets a separate identity or a visible conflict. [R01, R04, R14]

## 7. Documents, artifacts, and reviews

The shared material API needs only these initial operations:

- Project create/read/update/archive; optional repo/checkout bindings and session association.
- Document read by revision/current head; propose successor using `base_revision`; list revision history.
- Artifact begin upload, upload bytes, verify/finalize, retrieve manifest/content, list versions.
- Review comment create/list/reply and optional resolution, referencing a stable version.

Document update commits only if the current head matches the caller's base revision. Return the current revision and preserve the caller's proposal on conflict. Do not introduce automatic content merging until it can preserve user reviewability.

Artifact publication is atomic from the reader's perspective: readers see either no version or a complete verified version. Preserve original names for display but use safe internal paths. Require content digest and size. Downloads check authorization and support useful Android media handling. Preview HTML or other active content in a sandbox without management-app credentials or privileged bridge access. A new version never changes the bytes at an old version's reference.

Standalone artifacts may be associated with a session and later a project. Reviews remain attached to the reviewed version after newer versions appear. A checklist, if added, is an optional project document or lightweight structure; it does not gate session launch or messaging. [R05, R06, R13]

## 8. Hive boundary

Expose capture/link, search, read, write note/handoff, and retrieve launch context through an adapter. Exact commands should follow the inspected Hive implementation, not a parallel invented memory store.

Each memory item has provenance, source revision or session/run references where available, visibility/project association, timestamps, and sync freshness. Search responses link back to evidence and include truncation/staleness. Hive may index authoritative project documents but cannot silently change their current revision.

Launch context is bounded and records the brief revision and retrieval references included. Message passing does not depend on embedding generation or background enrichment. Failures in indexing must not block runtime input or launch unless the caller explicitly required unavailable context. [R07, R14]

## 9. Rota and Outfit boundaries

### Rota

Conceptual actions: list account observations, choose eligible account for provider/policy, assign account to run, report usage/limit events, obtain a local credential handle, and plan validated fallback.

Selection returns account ID, provider, observation freshness, reason, and assignment reference. It does not return a secret. An assignment is a local execution binding, not proof that provider quota is reserved. Credentials are resolved by the executing node. Concurrent sessions must not mutate one shared active-login file.

Fallback returns one of: continuation possible with eligible same-provider account; checkpoint/handoff available but native resume unsupported; waiting for account/reset; or intervention required. The old run's in-flight external effects must be reconciled before automatically replaying its last instruction. Account exhaustion produces a durable inbox/notification event without deleting session state.

A Claude resume continues natively on any account when the conversation is on the resuming node. That covers the same account, the accounts' shared transcript store, the default login (`~/.claude`), and any profile. Before launch, the adapter copies the transcript and its sidecar dir into the new run's config home; the original is left in place. When a session's working directory was under a temp root (`/tmp`, `$TMPDIR`) and a reboot removed it, the resume recreates it empty (mode 0700). Only the directory's contents are lost; the conversation is not.

### Outfit

Conceptual actions: inventory current machine state, resolve profile for node/project/session, list named connections, grant/revoke access, prepare environment generation, diff/apply managed changes, and report drift. A connection descriptor declares local credential provisioning and/or remote invocation support. Remote invocations authenticate the session and connection grant; the caller receives results without copying the provider credential into the model context.

Environment preparation produces resolved local paths plus a nonsecret manifest. Configuration precedence and ownership must be frozen once per harness: provider defaults → adopted user configuration → explicit managed profile → explicit session overrides, with secret resolution separate. No worker may implement a second independent merge strategy. Unsupported conflicting settings return a diff/conflict instead of silently overwriting user configuration.

Managed runtime/plugin updates produce a new generation, retain rollback data, and affect new runs by default. Existing runs retain their recorded generation. A connection invocation validates the current session grant at use time; revocation does not rely solely on removing a label from cached metadata. [R08–R11]

## 10. Input ownership, identity, and mobile

Authenticate owner clients, enrolled nodes, and session agents separately. A node reports only for runs it owns. Session tools default to the session's provisioned fleet/project/connection scope; broad owner-authorized access is supported without embedding owner credentials in the model's prompt.

An input lease for conflicting raw terminal writes contains actor, run, expiry/renewal, and takeover record. All clients show the lease. Losing a transport does not instantly imply another client may safely type; lease expiry or explicit takeover resolves it. Native attachment does not block supported structured message delivery or inbox polling. Raw writes queue when they would collide; structured delivery follows the adapter's declared busy/turn semantics.

Approval requests contain request ID, session/run, request kind, explanation/options, creation/expiry, and resolution record. Only one valid resolution commits. Stale Android links or duplicate taps return the existing result or an expired/conflict explanation. No client answers an approval by sending generic text to a different pending question.

Notifications reference stable objects and carry a grouping key and severity/category. Delivery is best effort and independently observable. The durable Inbox remains the source of pending attention. Push payloads contain minimal metadata; details require authenticated fetch. Mobile tests include background operation, hub unavailable, Twingate disconnected/reconnected, repeated taps, and a replaced run. [R02, R09, R12, R13]

## 11. Required shared fixtures before broad parallel implementation

The schema/contracts owner publishes small examples that every implementation reuses:

1. Standalone session launch and a project-associated launch.
2. Node disconnected with a live run and stale fleet status.
3. Message queued, runtime accepted, replied, rejected, and indeterminate.
4. Duplicate operation with identical body and conflicting body.
5. Human input lease with conflicting raw writes queued, structured peer mail continuing, and explicit takeover.
6. Document update conflict and immutable artifact publication/review.
7. Fresh, stale, exhausted, and unknown account availability.
8. Same-provider fallback with native resume and with handoff-only capability.
9. Pending/resolved/expired approval deep link.
10. Environment drift, staged update, rollback, and protocol-version mismatch.

Mock adapters drive UI and service development; real Mac/Linux harness tests verify conformance. Any change to these semantics requires updating fixtures, affected consumers, and the implementation decision record in the same integration change. The goal is enough shared structure to parallelize safely, with implementation details free to evolve.

## 12. Contract revision v1 (implemented by WP00)

The semantics above are frozen as **protocol v1** in Go under `internal/contracts/`. The Go types are the single source of truth; `schemas/pyre-v1.schema.json` (`make schemas`) and the ten fixture bundles in `tests/fixtures/contracts/` (`make fixtures`) are generated from them, and CI fails if the committed copies drift.

| Concern | Location |
| --- | --- |
| IDs (`<prefix>_<ULID>`), errors and HTTP status mapping | `ids.go`, `errors.go` |
| Records: nodes, sessions/runs, leases/approvals, messages/receipts, projects/documents/artifacts/comments/notes, accounts/grants/assignments, environments, events/attention | `fleet.go`, `sessions.go`, `interaction.go`, `messaging.go`, `projects.go`, `accounts.go`, `environment.go`, `events.go` |
| Hub API (one interface for hub, mock, HTTP client, daemon proxy) | `api.go` |
| HTTP binding: `POST /v1/<op>` JSON RPC plus upload/content/preview/event-stream/node/terminal endpoints | `routes.go` |
| Node control protocol (WebSocket frames, command/report durability) | `nodeproto.go` |
| Runtime adapter boundary and capability strings | `runtime.go` |
| Typed node actions (inventory, environment, updates, connections, accounts, memory) | `actions.go` |
| Fixture bundle format | `fixtures.go` |

Key v1 rules: the authenticated principal travels in the request context, never the body; every mutation carries `operation_id`; message delivery keeps storage, runtime-delivery, acknowledgement and outcome as separate fields; an attempt binds a message to one run permanently; `indeterminate` is resolved only by evidence or an explicit `messages.resolve`/`messages.reissue`; approvals are answered by `(approval_id, run_id)`; configuration precedence is `provider_default → adopted_user → managed_global → managed_project → session` with one merge implementation (`internal/configuration`). Domain stores own namespaced migrations through `internal/storage/sqlitex`, so no worker edits a shared migration registry.

**v1.2 (additive; protocol version unchanged):** `attention.get`, `artifacts.preview_url` (short-lived one-time preview links), `connections.check_grant` (hub-authoritative use-time connection grant decision for nodes), the `account.changed` event and `notes.list` `sync_states` filter. See decision I09.

**v1.3 (additive):** account fleet lease (`accounts.lease.acquire/renew/release`, `CredRotaCheckout`, `Account.per_machine_independent`). See decision I13.

**v1.4 (additive):** `CommandResult.interim` and `commands.get` for async node action results. See decision I14. `nodes.action` with action `connection.invoke` may omit `node_id`: the hub checks the calling session's grant (`forbidden` when denied), then routes to a node with verified `remote_invoke` availability, preferring the session's own node and then the most recent verification; no verified holder yields `unavailable`. Every other action still requires `node_id`.

**v1.5 (additive):** node report kind `run_snapshot` so a node re-sends its recent runs and sessions when the hub's committed report watermark is behind its own (hub restored from an older backup). See decision I15.

**v1.6 (additive):** Claude doorbell. Capability `input.monitor`; evidence kinds `claude.tool.inbox_read` (acceptance and consumption: the recipient run read the message with the pyre inbox tool) and `claude.monitor.notified` (informational; never acceptance); classifiers `AcceptanceEvidence`, `ConsumptionEvidence`, `PullEvidence`; `RunHandle.Data["caps_unavailable"]` (`HandleCapsUnavailable`) removes capabilities from one run. A receipt for pull evidence carries an attempt id minted by the recipient node; hubs still accept an empty `attempt_id`. See decision I16.

**HTTP binding note (no version bump):** a node daemon answering from its read cache while the hub is unreachable sets the response header `Pyre-Freshness: <Freshness JSON>` (source `node-cache:<node>`, stale, `observed_at` = last confirmation from the hub). Hub responses never carry it. See decision I17.

**HTTP binding note (no version bump):** a node daemon whose hub link is down answers `unavailable` with the response header `Pyre-No-Retry: 1`; clients must not retry that answer with backoff (the daemon itself is reachable). A client that cannot reach the daemon still retries. Receipt times are stamped by the hub on its own clock; a node's own observation time is kept in the receipt detail as `node_observed_at=<RFC3339>`. See decision I19.

**HTTP binding note (no version bump):** client transport failures are `unavailable` errors with `details.unreachable` = `hub` or `local_daemon` (plus `details.socket`). See decision I20.

**Memory hits (no version bump):** `MemoryHit.session_ref` is the Pyre session ID (`ses_…`) when the answering node can map the harness's native session to a Pyre-launched run (provenance then also carries `pyre_session`/`pyre_run` and the session's project); otherwise it is the harness's native session ID.

**Protocol check at exchange (no version bump):** public exchange endpoints (`/v1/pair/exchange`, `/v1/enroll/exchange`) check `X-Pyre-Protocol` before redeeming; an incompatible peer gets `protocol_mismatch` (HTTP 426) and the one-time code stays valid.

**v1.7 (additive):** `AccountView.by_node` carries the newest observation of an account from each node that holds a grant for it. See decision I22.

**Operational retention (no version bump):** the hub deletes finished node commands 7 days after completion and idempotency (retry-protection) records after 30 days; a retry of an older mutation runs again. Nodes prune their command journal after 30 days. A message's `bound_run_id` is released only when every attempt on that run was deferred with nothing submitted.

**Search (additive, contracts/search.go):** node action `transcript.search` (a node searches its own runs' transcripts in a local FTS5 index) and hub operation `search.query` (fans out to connected nodes in parallel, adds Hive memory, notes, docs and artifacts, ranks by relevance and recency, and names nodes not searched). Served through an optional `SearchAPI`; older hubs answer 404 (unsupported). Node principals are refused.

**accounts.update (additive, contracts/accounts_update.go):** owner-only change of an account's label or disabled flag; never touches its rota binding. `pyre accounts rename <account> <label>`.

**Token accounts (additive, contracts/accounts_token.go):** credential kind `oauth-token`; hub ops `accounts.set_token`/`accounts.remove_token` (owner), `accounts.tokens` (metadata only, never the token), `accounts.token.fetch` (node principals allowed for that account; never offered on the local node API); node registration source `"token"`; `account.observe` `force` flag; observation source `claude:messages-headers` (unified rate-limit headers from a minimal Messages request). See decision I23.

**Repos and worktrees (additive, contracts/repos.go):** hub operations `repos.list` (fuzzy, recently used first, across nodes) and `sessions.worktrees`; node report kinds `repos` and `session_worktrees`; node actions `repos.reindex`, `worktree.create`, `worktree.scan` and `worktree.remove` (the node re-checks at removal: never the main checkout, never uncommitted work, never an unmerged and unpushed branch). `start_session` accepts `repo`/`repos`/`fresh`/`branch`. See quick launch.

**Folded token accounts (additive):** `accounts.token.fetch` and `accounts.tokens` accept a folded account's ID and answer for the surviving account (its `account_id`); node reports naming a folded ID are recorded under the survivor, and its grant reports are dropped. `AddTokenResult.attached` says the token joined an existing account. See decision I23.

**Machine affinity (no wire change):** automatic account selection may also weigh other machines' active assignments, read via `accounts.list`, as a soft preference; the assignment reason explains it. See decision I25.

**Own login identity (additive, contracts/fleet.go):** `HarnessInfo.login {email, provider_ref, plan}` reports the identity of a node's own harness login, read without writing the login files and never carrying a token. An enabled account whose `rota_ref` matches that email and has no Pyre login on the node shows as one "own login" card across those machines and counts as ready; its usage comes only from rate-limit updates Pyre's own runs receive. See decision I26.

**Child reports (additive, v1.7):** `StartSessionRequest.report_to_parent` (bool, optional): when the creator is a session (default true) the hub messages the parent session from the child when a turn ends (last assistant message ≤1500 chars, skipped if the child already messaged the parent that turn), when input is needed, and on exit or failure; recorded as reserved tag `report:parent` or `report:none`; nothing is sent to an archived or ended parent; at most 12 reports per child per 10 minutes. Children get `PYRE_PARENT_SESSION_ID` and `PYRE_REPORT_TO_PARENT`.

**sessions.start by a session:** the parent is set from the principal. A child started without `project_id` joins the parent's project. An explicit `""` (CLI `--project none`) keeps it outside any project.

**sessions.update by session principals:** a session may rename any session (owner decision 2026-10-06; automatic titling still defers to a person's title). It may archive or unarchive only itself and sessions it started (created_by or lineage parent); otherwise `forbidden`.

**v1.8 (additive):** machine and per-run resource stats in `NodeResources` (`cpu_pct`, `load5`, `load15`, `swap_total_mb`, `swap_used_mb`, `mem_pressure` normal|warn|critical, `mem_pressure_some10`, `disks[]` `DiskUsage{path,roles,total_mb,free_mb}`, `runs[]` `RunUsage{run_id,session_id,procs,rss_mb,cpu_pct,top{pid,name,rss_mb},observed_at}`), sent in status heartbeats (replaceable, never queued). `Run.usage` is filled by the hub for live runs in session views from the node's last status; nodes do not set it in run reports.

**v1.9 (additive):** `Run.activity {text, since}`: a node-derived "doing now" phrase (≤40 chars, never secrets, file names only), present only while the run is running; `Observation.doing` feeds it.

**fs.mkdir (additive, contracts/fs.go):** node action that creates a folder, only inside the user's home (symlinks leaving home are refused); used by the new-session panel and `pyre new <path> --mkdir`.

**Read receipts (additive):** evidence `pyre.read` (counts as acceptance) and hub op `messages.mark_read`, called when the recipient session reads a message (get_message, inbox, wait_reply via MCP or CLI) and recorded also when it acks or replies; the hub then cancels any pending paste on the node (`cancel_delivery` with `read`). Owner reads do not count.

**Plugin-prompt delivery (additive):** capability `input.plugin_prompt` and acceptance evidence `claude.plugin.prompt_submit`: Pyre's per-run Claude plugin module claims a spooled message and queues it with Claude Code's `$.prompt.submit` between turns (no paste, no input lease); paste stays the fallback when the module heartbeat is absent. Message headers carry the send time: `[pyre-msg:<id> · sent 3:05 PM PT (8h ago)] from session:<title>`.

**Repo catalog and clone (additive):** hub op `repos.catalog` (each machine's repo index merged with the owner's GitHub repos, deduped by normalized remote, ranked used-in-Pyre > contributed > checked out); report kind `github_repos` (`GitHubRepoIndex`, fetched hourly by nodes via their own `gh` login, no token stored); node actions `repo.clone` (async, into the node's repo-root layout, never overwrites) and `repos.github`; hub migration `hub_repos` v2. `pyred --no-github-catalog` disables the GitHub fetch.

**Archive-when-done and icons (additive):** `UpdateSessionRequest.when_done` (archive a live session once its turn ends: reserved tag `archive:when-done`, graceful stop, then archive) and MCP `archive_session` with no session = self; MCP `archive_project` (`with_sessions`); `Session.icon` (one emoji, at most 2 cells) set via `UpdateSessionRequest.icon`, MCP `set_session_icon` or `rename_session`; audit actions `session.archive_scheduled`, `session.archive_canceled`, `session.icon_set`; hub migration namespace `hub_session_icon` v1.

**Owner activity (additive):** `Session.owner_active_at` is when the owner last sent the session a message or reply (`messages.send` to a session, including ask decisions the owner answered), typed into it through the terminal relay, or took its run's input lease. Only an owner principal counts (a session acting on the owner's behalf, the system and auto-resolved asks do not). The hub writes it off the request path, at most once a minute per session (in-memory throttle plus a stored-value guard), and emits `session.updated` when it changes; `sessions.list`, `sessions.get` and `sessions.get_many` return it. Clients use it to mark recently engaged sessions. Hub migration namespace `hub_session_owner_active` v1.

**Reparenting (additive):** `UpdateSessionRequest.parent` (session id; "" or "none" = top level) moves a session under a new parent; `Lineage.reparented_from`/`reparented_at` record it and `Session.Parent()` returns the current parent (created_by stays the creator). Pending child reports retarget in the same transaction; both sides get a notice message; audit action `session.reparented`. Permissions: owner any; a session may move itself, sessions it currently parents, and sessions in its own project. MCP `reparent_session` (pre-approved; `sessions` for bulk; `parent` required).

**Run.state_since (additive):** when the run entered its current state; set by the node on each state change and kept while unchanged; the hub falls back to ended_at/started_at for older nodes. The TUI/CLI "FOR" column shows time in state (working 4m, idle 25m, 2h ago).

**sessions.get_many (additive, contracts/sessions_many.go):** `POST /v1/sessions.get_many {ids}` → `{items: [SessionView], missing: [ids]}`; at most 200 ids (more is invalid), items in request order, empty and duplicate ids dropped, archived sessions included, same authorization as `sessions.get`. Optional `SessionBatchAPI`; Hello feature `sessions.get_many`; a hub without it answers 404 (unsupported). The TUI refreshes only the sessions an event names with one call per debounce window; the full list is a safety net (≥5 min while the event stream is connected). See decision I27.

**inbox.wait (additive, node-local only, contracts/inboxwait.go):** `inbox.wait {session_id, epoch, after_generation, wait_seconds}` → `{epoch, generation, woken}` on pyred's local API (the hub never serves it). pyred wakes a session's waiters when a `deliver` command for it arrives and on every hub reconnect, and returns after at most 60 s; a changed epoch (pyred restart) or a newer generation returns at once. The Claude doorbell (`pyred inbox-watch`) waits locally, then makes one non-waiting `messages.list`; against an older pyred (404) it long-polls the hub as before. See decision I27.

**Terminal fit (additive):** `TerminalOpenCommand.fit`, hub feature and node capability `terminal.fit`. On `/v1/terminal` a read-only ticket's auth frame may set `"fit": true`: the node attaches with `tmux attach -r -f ignore-size` at the window's own size and the server sends `{"type":"resize","cols","rows"}` first and on every window change; client resize frames are ignored for fitted streams. The TUI's previews and Grid tiles for remote sessions use the relay (fitted tiles only when both hub and node advertise `terminal.fit`); interactive attach keeps the ssh descriptor (multiplexed with ControlMaster). `PYRE_TUI_PREVIEW=ssh` forces the old path.

**Codex systemError and capacity retry (no wire change):** a Codex thread whose last turn failed (`systemError`) is reported `waiting_input` with reason `last turn failed: <error>`; a failure at model capacity is continued automatically after 2/5/15/30 minutes (budget reset by a completed turn) and audited as `node.capacity_retry` (automation action `capacity_retry`).

**command.result payloads and retention (behaviour change, no version bump):** the `command.result` event omits a result larger than 1 KiB (the command record keeps it for `commands.get` and the waiting caller). A finished command's payload/result over 1 KiB is compacted after `CommandCompactAfter` (default 24 h); finished commands are deleted after 7 days; all hub prunes run in ≤500-row transactions. Hub migration namespace `hub_retention` v1 (partial index `commands_finished`, `operations_created`). `pyre-hub compact --data DIR [--dry-run] [--no-vacuum]` runs retention and VACUUM offline; `serve` holds `<data>/hub.lock`.

**Compact agent tool output (behaviour change, MCP only):** the read tools `inbox`, `get_message`, `list_sessions`, `get_session`, `read_session`, `list_approvals`, `list_projects`, `list_nodes`, `search` and `get_digest` return compact text by default (one line per item; ids kept; receipts, cursors and capabilities omitted; listing bodies cut at ~160 chars without splitting paths/links, extra refs in `[refs: …]`; `get_message` never cuts the body) and declare no `outputSchema`; `format: "json"` returns the previous structured result. The MCP `inbox` defaults to unacked mail. Session lines mark "(this machine)" so agents can tell whether a recipient shares their filesystem; long reports go as a file path (same machine) or `pyre publish` + `attach` (another machine). The Claude doorbell recognizes reads from the `[pyre-msg:<id>]` marker as well as JSON.

**Delivery status (additive):** `MessageView.delivery_status {state, cause, summary, since, final}` (hub fills it on `messages.send` and `messages.get`, not on lists; clients derive it with `msgstatus.Derive` from the message and the recipient's session for older hubs) and `WaitReplyResult.delivery`. Summaries say why mail is not getting through (recipient working, last turn failed, machine disconnected, approval pending, input held, ended, archived); "delivered"/"read" only with acceptance evidence. `wait_reply` returns early (conflict) when no reply can come without action (recipient ended/archived, message refused/expired/cancelled). The hub sends a sending session one notice (system principal `stuckmail`, batched per sender, ≤1 per 10 min, each message once) for mail queued >10 minutes because the recipient ended, was archived, failed its last turn, is unknown or its machine is disconnected.

**Failure attention (behaviour change, no wire change; decision I32):** the attention deriver also consumes `run.observed` and `session.updated`. `run.ended` with state failed raises nothing for a session whose `Session.Parent()` is set (the hub's child report tells the parent, also for a child tagged `report:none`; launch failures render "<title> could not start: …" without a transcript read); for an owner-started session whose reason is "no eligible <provider> … (cooldown/limit/exhausted)" (`runfail.Capacity`) it joins the one active item of group `capacity:<provider>` (kind `failure`, no `session_id` or `project_id`, object = newest session, `count` = sessions, title "<Codex|Claude> out of capacity: N launches waiting for the reset", summary lists session titles; members in `attention_members`, attention migration v2); other failures keep `failure:<session>`. A session's `failure` and `account_exhausted` items are withdrawn, and it leaves capacity groups, when it is archived (`session.updated` archived) or a later run reaches a live state (running, waiting_input, waiting_approval, waiting_on_background; `run.started` in state starting no longer resolves them); any `run.ended` withdraws its `account_exhausted` items raised before. Hub maintenance sweeps open items the events missed (archived, live, under a parent, account items older than the run's end) in batches of 100 sessions, one short transaction per changed session. `SessionView.flag`: when the current run failed at launch (state failed, reason `launch …`), `failure` and `account_exhausted` items flag at level `message` (reason `failed` / `reauth`), never `needs_you`. `message` items carry `summary` = the newest message body, escapes/control characters stripped, whitespace collapsed, ≤300 runes (empty summaries of open items are backfilled once per hub process); push payloads are unchanged (`PushDataKeys`, generic title).

**Asks (additive, contracts/asks.go):** hub ops `asks.create`, `asks.list`, `asks.get`, `asks.answer` (owner only, batch: each item commits on its own as `<operation_id>:<index>`) and `asks.withdraw` (asker or owner); optional `AsksAPI`, Hello feature `asks`, 404 (unsupported) on older hubs; pyred forwards them for sessions. Create rules: topic key normalized to a lowercase slug (≤120); title ≤120; context required: markdown, at least `AskMinContext` (300) and at most 16000 characters for a new ask, rejected with `invalid_argument` naming what to cover (what is decided and why now, what is known or tried, what each option leads to, what happens by default; a follow-up merged into an open ask may be short); 2–4 options with unique ids, each with a `detail` (new asks); the default must be one of them. A child session's ask is forwarded to its parent as a queue message (`redirected`); an open ask on the same (project or `owner`, topic) absorbs a repeat as an update (`merged`, no new owner mail); a topic answered, defaulted or expired within 7 days returns that ask (`decided`) unless `reopen`; an open ask in the scope whose title+options are ≥0.6 trigram-Jaccard similar (FTS5 BM25 candidates) yields `possible_duplicate` unless `confirm_new`. Deadline: `deadline_seconds` or 1 h; one landing in the owner's quiet hours (default 22:00–08:00 America/Los_Angeles) moves to their end (`morning`) unless urgent. The ask reaches the owner as a message from the session (thread = ask id) and as one attention item of kind `ask` (group `ask:<id>`, flags the session "needs you" until decided; the carrying message raises no item); `Ask.attention_id` is filled on read. At the deadline maintenance applies the default (`defaulted`) or expires it (`expired`, by system principal `asks`); answer and deadline race on `state='open'` (the loser gets conflict). The asker gets one queue message per batch replying to the ask's message (`[pyre ask decided] <title>: owner chose b — <label>`); archiving a session withdraws its open asks. Messages to the owner from a session with a parent are refused (`forbidden`: "only top-level sessions message the owner; send this to your parent …"). Hub migration namespace `hub_asks` v1.

**Held answers (additive, contracts/webinbox.go, Hello feature `asks.hold`):** owner-only ops `asks.hold` (an `AnswerAskItem`, validated like `asks.answer`; replaces an earlier held answer for that ask and restarts the quiet window), `asks.unhold` (`not_found` when nothing is held for the ask: already sent or decided), `asks.held` (the queue, oldest hold first, with `sends_at` = newest hold + `AskBatchQuiet` (5 min), absent when empty) and `asks.flush` (send now; the response is `asks.answer`'s). Held answers live in the hub (`ask_held`, migration namespace `hub_ask_held` v1), so every surface sees one queue. Hub maintenance (every 10 s) sends the whole queue once the quiet window has passed, through the `asks.answer` path as the owner principal that held each answer (one combined decision message per asking session; per-item operation ID `op_askheld-<ask>-<hold time>`, so a crash replays instead of answering twice). Before applying a deadline default, maintenance sends the held answers of the due asks, so the owner's choice wins. Any decision (answer from another surface, deadline, withdrawal, archive) deletes the ask's held answer in the same transaction. Every queue change emits `ask.held_changed` (payload `HeldAnswers`, object = the ask). pyred forwards the ops; the mock applies the window lazily on asks calls.

**Project pages (additive, contracts/webinbox.go, Hello feature `projects.page`):** `projects.set_page` / `projects.get_page`. The page is the project's document with slug `page` (title "Project page", markdown, ≤256 KiB). `set_page` is allowed for the owner and for a top-level session whose project is the target (`project_id` defaults to the caller's project; children and other projects get `forbidden`; no project → `invalid_argument`); the first write creates the document, each later one is a new head revision written directly (no proposal step). With `base_revision_id` the write is a compare-and-swap (`conflict`, details `current_head`); without it the write lands on the current head (one retry if another write won the race). `get_page` returns `{project_id}` with no revision when no page exists. No separate event: clients refresh on `document.revised` whose `project_id` is the project (the hub's document store refs the document; the mock refs the revision). MCP `update_project_page` / `get_project_page` (pre-approved for Claude) and `pyre project page [show|set --file F|-]`.

**Waiting on you (additive, attention kind `waiting`):** when a top-level session's turn ends (its current run goes running → waiting_input; turns are counted in `child_turns` as for children, `Session.Parent()` empty) the hub records a check (hub migration namespace `hub_waiting` v1, table `lead_waits`, one row per session) and its child-report worker reads the last assistant message through the node (transcript tail, same bound as a child report) outside any transaction, then runs `askdetect.OwnerQuestion`. A question, when the session has no open ask it created or merged during that turn and the turn is still its latest, raises or refreshes one attention item per session: kind `waiting`, group `waiting:<session>`, object = session, `session_id`/`project_id` set, severity normal, title "Waiting on you: <question first line, ≤100 runes>", summary ≤300 runes of the message around the question (sanitized like a message excerpt). A repeat updates the active item (count, updated_at, title, summary); once resolved, the same item reopens rather than a new one being created. Resolution: the owner engaging with the session (typing, taking its input lease, messaging it, reading its message) marks it handled; the deriver withdraws it on `ask.changed` open (created or merged) for the session, `run.observed`/`run.started` running, `run.ended` (each only for an item last updated at or before the event) and `session.updated` archived; a raise that lost the race with the next turn is withdrawn at once. `SessionView.flag`: level `needs_you`, reason `waiting`, rank between `ask` and the session's own flag, kept while read. Push: generic title "Pyre: a lead is waiting on you", quiet hours and per-kind prefs apply, at most one push per session per hour (later reopenings record `disabled` with that reason; the window is in memory, so a hub restart allows one more). Sessions with a parent never get one.
