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

**sessions.update by session principals:** a session may rename any session (owner decision 2026-10-06; automatic titling still defers to a person's title). It may archive or unarchive only itself and sessions it started (created_by or lineage parent); otherwise `forbidden`.

**v1.8 (additive):** machine and per-run resource stats in `NodeResources` (`cpu_pct`, `load5`, `load15`, `swap_total_mb`, `swap_used_mb`, `mem_pressure` normal|warn|critical, `mem_pressure_some10`, `disks[]` `DiskUsage{path,roles,total_mb,free_mb}`, `runs[]` `RunUsage{run_id,session_id,procs,rss_mb,cpu_pct,top{pid,name,rss_mb},observed_at}`), sent in status heartbeats (replaceable, never queued). `Run.usage` is filled by the hub for live runs in session views from the node's last status; nodes do not set it in run reports.

**v1.9 (additive):** `Run.activity {text, since}`: a node-derived "doing now" phrase (≤40 chars, never secrets, file names only), present only while the run is running; `Observation.doing` feeds it.

**fs.mkdir (additive, contracts/fs.go):** node action that creates a folder, only inside the user's home (symlinks leaving home are refused); used by the new-session panel and `pyre new <path> --mkdir`.
