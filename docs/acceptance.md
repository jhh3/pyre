# Acceptance and verification

This document defines observable acceptance for the product. Passing document validation or a mocked interface test is not evidence that native harness behavior, account switching, mobile push, or failure recovery works.

## Verification levels

1. **Contract tests:** isolated fixtures/fakes; validate schemas, errors, persistence, and transitions deterministically.
2. **Adapter tests:** real CLI/backend binaries where possible, isolated homes and simulated model/provider endpoints; preserve the user's default environment.
3. **Integrated pilot:** real Mac and Linux nodes plus Android, explicitly selected test profiles and projects, and small controlled agent tasks. Use injected limit events to exercise fallback; do not burn subscriptions merely to reach a limit.
4. **Operational rehearsal:** update/rollback, backup/restore, node/hub outage, and resource envelope under representative concurrency.

Each acceptance result records code/dependency versions, platform, fixture/profile, commands or interaction steps, observed outcome, and evidence path. Redact secrets and private transcript content. An unrun case is marked unrun, not passed based on a related unit test.

## Cases

| Case | Requirements | Test and pass condition |
| --- | --- | --- |
| A01 | R01, R14 | Enroll an isolated Mac node and Linux node using the chosen SSH/Twingate-compatible path. Reconnect both and restart a client. Machine identities remain stable, stale/offline timestamps are accurate, and no duplicate nodes appear. |
| A02 | R02, R12 | Launch Claude and Codex remotely, attach from another machine, type a follow-up, detach, close the management UI, and reattach. Verify the intended execution/thread, cwd, and transcript continuity; no second live engine is accidentally created. Test each harness independently. |
| A03 | R02, R03 | A parent session invokes native subagents and also launches a peer on another node through Pyre tools. Both mechanisms work. Trace parent/child relationships only where actually exposed; unsupported independent child attachment is reported accurately. |
| A04 | R03, R04 | Two heterogeneous peers exchange a message, reply reference, and artifact reference. The CLI, terminal, and Android show consistent sender/recipient/thread identity and stored/runtime-accepted/acknowledged distinctions. Leave a native terminal attached and verify structured peer messaging or supported inbox polling continues while conflicting raw writes queue. |
| A05 | R04, R14 | Send while a recipient node is disconnected. Restart the sender service and hub, reconnect the recipient, and confirm the message remains durable and is reconciled without a duplicate agent invocation. Distinguish local outbox acceptance from hub acceptance. |
| A06 | R04, R14 | Crash the wrapper or sever the connection after native submission but before acknowledgement. The system reports an indeterminate outcome, reconciles when evidence appears, and does not automatically replay uncertain work. Exercise cancellation and resume that returns a replacement backend ID. |
| A07 | R05, R02 | Start a standalone session without a project, use its full basic controls, then associate it with a project. Also create a project with zero repos, one with multiple repos, and both shared-checkout/separate-worktree sessions. No mandatory task lifecycle is introduced. |
| A08 | R05, R06 | Edit the same project document from two sessions starting at one revision. One publish succeeds; the conflicting edit remains recoverable with an explicit conflict. The published revision is readable from another node and Android. |
| A09 | R06, R13 | Publish text, an image/PDF, and another arbitrary file; disconnect the producing node. Retrieve the exact published bytes through the hub and Android, comment on a version, publish a successor, and preserve provenance/review links. A failed upload is never labeled published. Test HTML isolation. |
| A10 | R07, R05 | Capture a synthetic session in Hive, publish a decision/handoff, and retrieve it from another node/new session with provenance and freshness. Preserve local query functionality during a hub outage. Bound initial brief size; verify historical evidence is not silently promoted into authoritative docs. |
| A11 | R08, R02 | Run two sessions using different accounts of the same provider on one machine, then across two machines. Check actual runtime identity using supported signals. Switching/recovering one session must not change the other's identity or the user's default login. Repeat on Mac and Linux. Negative check: a copied grant is never created (a copied login logs out the loser — see the accounts report). |
| A12 | R08, R04, R13 | Inject a verified limit/failure signal, checkpoint, select another eligible same-provider account, and recover using the supported native path. Preserve session identity/history or explicitly disclose handoff limitations. Do not replay unknown tool work. If no account is eligible, retain work and notify; no implicit provider/API-billing fallback. |
| A13 | R08, R14 | Simulate concurrent refresh requests, stale usage, expired grants, and a lost refresh response. Preserve the correct grant owner, avoid credential-file races, label unknown health, and back off. No token values reach logs or shared project data. Include the Codex shared-home double refresh (prevented by the one-process lease) and usage probes that never refresh. |
| A14 | R09 | Make a synthetic Outfit connection available on a second enrolled node under its chosen scope. Verify availability before use, preserve credential custody, test revoke/reauth, and ensure credentials are absent from manifests, messages, generated skills, and artifacts. |
| A15 | R10, R09 | Inventory an existing skill/plugin/config setup, preview adoption, materialize global and project overlays for both harnesses, and detect drift. Preserve unmanaged customizations and prevent duplicate hooks/MCP registration. Report restart-required changes honestly. |
| A16 | R11, R14 | Stage a CLI/backend/plugin update while a session runs. That session retains its selected executable/config revision. Start a new session on the updated version, force a health-check failure, roll back, and reject incompatible hub/node/mobile protocols clearly. |
| A17 | R12, R01, R04 | Use the TUI entirely from the keyboard at narrow and normal widths. Search, preview, attach/detach, send, open a project, and inspect account/machine health. Slow nodes do not freeze navigation; repeated events do not lose selection or overwhelm rendering. |
| A18 | R13, R06 | Install a signed APK on Android. Pair over HTTPS, launch on a selected node, read/reply, resolve a question/approval, review an artifact, and receive actionable screen-off notifications. Resolve an approval elsewhere and confirm the phone withdraws it. Verify update with the same signing key preserves app data. |
| A19 | R14, R01, R06 | Stop/restart the hub, restart a node service, simulate a lost node, and restore hub metadata/files from backup. Existing local executions continue where supported, shared functions show degraded status, cached documents/memory remain readable, and no live ownership is falsely reassigned. |
| A20 | R14, R07, R11 | Run representative concurrent sessions with capture/enrichment, event bursts, repeated outages, and bounded logs. Measure incremental Pyre CPU/RSS, queue sizes, latency, and hook overhead. Resource limits throttle background work and preserve interactive responsiveness. |

## Gate evidence

The implementation plan owns the precise gate-to-work-package mapping. These are the minimum behavioral constraints:

- **G0 backend/contract decision:** demonstrate the core A02 handoff behavior and A11 account isolation with the selected native versions; exercise A06 against the adapter design; record HAPI adopt/extend/reject evidence. Contract fixtures and capability reporting are ready for independent workers.
- **G1 operational backbone:** the backend and CLI portions of A01–A06 pass, with a real two-node pilot for connectivity, native control, and messaging. Artifact publication and finished TUI/Android assertions in A04 belong to G2/G3. Do not replace native tests with only mock tests.
- **G2 integrated workspace:** the backend and CLI portions of A07–A16 pass, including artifact publication, durable attention events, and real same-provider recovery where supported. Finished client presentation and external notification delivery are verified at G3. Document any requirement that remains blocked; project/memory/credential migration has a reversible path.
- **G3 usable release:** A17–A20 and the entire earlier suite pass at their required level; Android and real Mac/Linux evidence are attached. Deliver installable artifacts and operating instructions.

If an integration spike invalidates these gate labels or sequencing, the lead may amend the plan and this document together. Do not lower the final acceptance bar by moving an essential feature to an unlabeled backlog.

## Failure injection design

Give adapters injectable clocks, transport disconnects, fake limit/refresh responses, and crash points around durable writes/native dispatch. Use stable operation IDs across retries. Test ambiguous outcomes, not only clean failures before anything happened.

Test a completed native operation whose response is lost, a send arriving at a different turn than expected, an exited agent leaving a shell prompt, an approval resolved by another client, a stale document revision, partial artifact upload, and a plugin update that requires restart. No test may blindly inject text into a default user tmux pane.

## Performance budgets

Establish measurement methods and runtime baselines during G0. Measure frontends and integrations as their implementations arrive; do not invent guarantees for interfaces that do not exist yet. Record cold/warm TUI responsiveness, backend event latency, incremental per-node CPU/RSS, hook duration, memory indexing throughput, and artifact transfer behavior. Set explicit measured limits before G3 and fail regressions against them. Resource accounting separates Pyre overhead from agent/model processes.

## Release evidence

Keep synthetic/reproducible evidence under the release or integration report: exact build refs, platform matrix, executed case IDs, links to test output, screenshots/recordings of core flows, known limitations, backup/restore results, and artifact checksums. Private credentials, APK signing keys, and raw personal logs belong outside the repository.
