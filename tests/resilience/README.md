# Resilience and failure-injection tests (WP18)

Black-box failure injection over the merged packages, following
`docs/acceptance.md` ("Failure injection design"). Everything runs on
loopback with temp directories, synthetic credentials and on-disk SQLite; no
tmux, no user configuration, no network beyond 127.0.0.1.

## Run

```sh
go test -race ./tests/resilience/...                 # ~2.5 min (real-stack scenarios build pyre-hub/pyred)
go test -race -short ./tests/resilience/...          # ~30 s, skips real-stack scenarios
go test ./tests/resilience -run TestResourceBudget   # skipped unless PYRE_RESOURCE=1
PYRE_RESOURCE=1 go test ./tests/resilience -run TestResourceBudget -v   # see BUDGETS.md
go test -v ./tests/resilience 2>&1 | grep -E 'EVIDENCE|DEFECT|PENDING'   # evidence lines
```

A skipped test is never a pass: `DEFECT WP18-n` skips are known bugs routed
to their owners (they fail as soon as the skip is removed), `PENDING WPnn`
skips name the package whose arrival enables the scenario.

## Harness

| piece | what it is |
|---|---|
| `faultnet/` | TCP fault proxy: pass, silent partition (blackhole), refuse/reset, per-chunk delay, cut-all, and a WebSocket frame mode that parses RFC 6455 frames in both directions so a filter can drop, duplicate, delay or cut on individual node-protocol frames (`report`, `report_ack`, `command`, `command_ack`, `welcome`, ...). |
| `refhub/` | Reference hub target implementing `refhub.NodeHub`: real `transport.AuthStore`, `enrollment`, `topology`, `transport.NodeServer` over an on-disk SQLite file, plus a minimal durable report/command store standing in for WP05. `Kill()` resets TCP connections and closes the DB without recording anything (crash); `Restart()` reopens the same files on the same port and runs startup recovery. |
| `testNode` (helpers_test.go) | Node daemon stand-in: `transport.NodeLink` over an on-disk SQLite file; `stop()` (process exit), `crash()` (DB closed under in-flight work, journal left "executing"), `restart()`. |
| `apiStack` (api_test.go) | `httpapi` server over a composite API — real `projects` + `artifacts` stores on SQLite, deterministic mock for sessions/messages/events — with a `lossyTransport` that lets the hub commit and then drops the response (lost acknowledgement). |
| `stack/` | Real local stack: builds `pyre-hub` and `pyred` from this module once per test process and runs them as separate processes in short `/tmp/pw18-*` dirs on free loopback ports (`pyre-hub init/serve/backup/restore`, `pyred enroll --secret-stdin`, `pyred run`). `Proc.Kill()` is SIGKILL, `Proc.Signal` any signal; PIDs feed the resource sampler. |
| `stack.RunTestDaemon` role | `PYRE_RESILIENCE_ROLE=pyred-test`: the test binary runs a daemon wired exactly like `pyred run` plus the WP07 **shell adapter** on an isolated tmux socket (`<state>/tmux/pyre.sock`) and a mock adapter with a crash-after-submit hook (`[wp18:crash-after-submit]` → SIGKILL after the runtime accepted input). Needed because `cmd/pyred` registers only the mock adapter today. |
| `TestMain` roles | The test binary re-executes itself as a NodeLink process (budget) or as the test daemon. |

### Targets

- Transport-level scenarios run against `refhub` (fast, deterministic, frame
  faults over plaintext WebSocket).
- Real-stack scenarios (`stack_*_test.go`) run the merged WP05 hub
  (`cmd/pyre-hub`) and WP06 daemon (`cmd/pyred`, with `--dev-mock` where a
  mock run is needed) as processes, with `faultnet` between pyred and the hub
  (TLS passes through byte-level faults). The shell scenarios use the real
  pyred and its native shell adapter. Only the crash-after-submit scenario
  still uses the test daemon role (it needs the injected crash point).
- Still to add: claude/codex crash points.

## Scenarios

### Covered now (passing)

| test | failure injected | asserted | acceptance |
|---|---|---|---|
| TestHubCrashRestartQueuedReportsAndCommands | node down while hub queues commands; hub crash while node keeps producing; hub crash in the middle of draining a 2 000-report burst | every report committed exactly once in origin order (contiguous seqs), local outbox durable while hub down, each command executed exactly once after restart, registry never duplicates the node, state accurate after reconnect | A05, A19 (node-channel level) |
| TestNodeCrashDuringCommandIsIndeterminate (graceful, hard-crash) | node exits / is killed while a command is executing | result `indeterminate` (`outcome_indeterminate`), handler invoked once, no replay, hub does not resend | A06 (node-channel level) |
| TestPartitionBlackholeDetectedAndHealed | silent partition (no RST) for 3 s | hub and node detect within heartbeat bounds, registry `disconnected` with stale `last_seen_at` (never idle), work queued on both sides flows once after heal, ≤ 25 reconnect attempts | A19, R14 |
| TestDuplicatedAndDelayedFrames | every report and command frame duplicated, some delayed | hub commits each report once, node executes each command once | contracts §5 rules 2/8, §6 |
| TestLostAcknowledgementsThenCut | report_acks and command_acks lost, then connection cut | no loss, no duplicate commit, commands once | "lost acknowledgement" |
| TestHubCommitFailureForcesResend | hub storage error on commit | connection closed unacked; node resends from watermark; contiguous | contracts §5 rule 1 |
| TestNodeOutboxBoundedDuringOutage | hub down, outbox at MaxOutbox | `capacity_exhausted` (never silent drop); drains and accepts after restart | storage pressure, A20 |
| TestCommandExpiresWhileNodeOffline | command expiry passes while node offline | failed/`expired`, zero executions | contracts nodeproto |
| TestLostResponseAfterCommitReturnsStoredResult | response dropped after commit for projects.create, documents.propose, upload chunk + finalize, messages.send | retry with same operation_id replays the stored result; exactly one record; published bytes identical | contracts §1, A05 |
| TestDoubledRequestsSameOperation | 8 concurrent copies of the same mutation | one record each; same op id + different body = conflict | contracts §1, §5 rule 2 |
| TestDelayedDuplicateDoesNotRegressState | stale copy of an earlier update after a later one | stored earlier result replayed; current state untouched | contracts §1 |
| TestStaleCursorResetRequired | watcher resumes from a cursor older than retention | `reset_required` with `snapshot_cursor`, no events handed past the gap, resume from snapshot works | contracts §6 |
| TestStoragePressureArtifacts | tiny per-version and total artifact limits, oversized chunk | `payload_too_large` / `capacity_exhausted` before bytes are accepted, nothing partial published, staged files cleaned, oversized chunk rejected then upload resumed | storage pressure, A09 |
| TestClientRetryBoundedUnderPersistentUnavailable | hub always 503 / 403 / bare proxy 502 | exactly MaxAttempts requests; Retry-After capped at RetryMax; non-retryable not retried | runaway retry |
| TestNodeLinkBackoffBounded (503, refused, flapping-after-welcome, 401) | persistent failure shapes | attempts within the exponential schedule; flapping does not reset backoff; 401 → `rejected` at max delay | runaway retry, R14 |
| faultnet/ unit tests | proxy modes and frame filtering | — | harness |

### Covered on the real stack (pyre-hub + pyred processes)

| test | failure injected | asserted | acceptance |
|---|---|---|---|
| TestStackA05MessageSurvivesHubRestartWhileRecipientDisconnected | recipient node cut off by proxy; message sent; hub SIGKILL + restart; sender retries same operation | message stays stored/undelivered across the crash, retry deduplicated, delivered to the runtime exactly once after reconnect, one inbox record | A05 |
| TestStackHubRestartWithQueuedCommandAndLiveRun | node cut off; stop command queued; hub SIGKILL + restart | live run reads stale (never failed) during the outage, queued stop executes once on reconnect, other run keeps its run id, one run per session, one node | A19 |
| TestStackDaemonCrashRecoversLiveShellRun | daemon SIGKILL with a live shell run (real tmux, isolated socket) | tmux server survives, run not marked terminal, restarted daemon recovers the same run id and tmux PID, no relaunch | A19, R14 |
| TestStackCrashAfterSubmitIsIndeterminate | daemon SIGKILL after the runtime accepted input, before the receipt | delivery `indeterminate`, bound to the original run, not retried/requeued, reissue needs `accept_duplicate_risk` | A06 |
| TestStackMockIndeterminateMarkerResolved | lost runtime receipt (`[mock:indeterminate]`) via real pyred | indeterminate + attention item, survives hub restart, resolved only explicitly | A06 |
| TestStackBackupRestoreRoundTrip | `pyre-hub backup` of a live hub, data dir deleted, `pyre-hub restore` into a fresh dir, served on the same address | old pin and owner token valid, node reconnects without re-enrollment, project/doc head/artifact bytes/message/run intact, new delivery accepted, one node | A19 |
| TestStackBackupArchiveSecrets | — | backup refuses to run without a passphrase; the archive is encrypted (no plaintext credentials or TLS key) | A19, R14 |
| TestStackQueuedFollowUpLatency | second message queued behind a busy run | follow-up delivered within 10 s of the first (≈ 2 s measured) | A04, A20 |
| TestResourceBudgetStack (PYRE_RESOURCE=1) | idle, 20 live runs, message burst | pyre-hub/pyred CPU/RSS/latency/log growth within BUDGETS.md limits | A20 |

Security scenarios on the real stack (revoked node, secret scan of the hub
data dir, node state dir and logs) live in `tests/security/stack_test.go`.

### Known defects (skipped, routed to owners)

| id | test | owner |
|---|---|---|
| WP18-1 (fixed, WP19) | TestWatchEventsBackoffUnderPersistentStreamError | WP03 `internal/client/watch.go` |
| WP18-4 (fixed, WP19) | TestStackBackupArchiveSecrets | WP05 `internal/hub/backup` / `cmd/pyre-hub` |
| WP18-6 (fixed, WP19) | TestStackQueuedFollowUpLatency | WP06 `internal/daemon` (+ `internal/messaging/delivery`) |

### Pending (skipped)

| test | waits for | planned assertion |
|---|---|---|
| TestPendingAdapterCrashPoints | claude/codex/shell adapters registered in `cmd/pyred` | crash points around durable writes/native dispatch on isolated tmux sockets |
| TestPendingAccountsThroughDaemon | pyred integrations configurable for an isolated HOME | A13 through `account.*` node actions; in-package coverage exists in `internal/accounts` (passing) |
| TestPendingMachineSleep | — (harness ready) | SIGSTOP/SIGCONT pyred beyond the heartbeat timeout: stale-not-idle, no duplicate run |

Also not yet covered: update download failure / partial configuration apply
(WP14), several nodes and long repeated-outage soak.

## Observations (not defects)

- `cmd/pyred` registers only the mock adapter (`cmd/pyred/adapters.go`); the
  merged WP07 shell/claude/codex adapters are not reachable through the
  shipped daemon yet (integration item).
- `sqlitex.Open` creates database files with the process umask (0644);
  `pyred run` and `pyre-hub` chmod them to 0600 afterwards, but any other
  caller must remember to (a 0600 create mode in sqlitex would be safer).
- `pyre-hub` logs one Info access line per API request to stderr without
  rotation (see BUDGETS.md).

- During a silent partition a node's reconnect attempt waits in the
  WebSocket handshake until `DialTimeout` (default 15 s), so after the
  network heals reconnection can take up to that long.
- The hub drops a silent connection from `Connected()` ~2 s before the
  registry records `disconnected` (bounded closing handshake).
- Artifact quota exhaustion uses `capacity_exhausted`, which is retryable by
  default, so `client.Publish` spends MaxAttempts on a full store before
  failing (bounded; the owner may prefer `Retryable=false` for quota).
