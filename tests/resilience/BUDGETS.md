# Resource budgets (A20) — measured baseline and proposed limits

Status: **baseline, macOS only.** Two measurements:

1. `TestResourceBudget` — the transport layer in isolation (WP04
   `transport.NodeLink` in its own process against `refhub`), plus the
   `httpapi` event stream + `client.WatchEvents`.
2. `TestResourceBudgetStack` — the **real `pyre-hub` and `pyred` processes**
   (main `c8bc980` merged into `wp18-resilience`), pyred with `--dev-mock` so
   agent processes are excluded by design; idle, idle with 20 live runs, and
   a message burst across 20 sessions.

Transcript capture with real harnesses, hooks (WP07 claude/codex), Hive
indexing, TUI and Android are **not** included; an unmeasured component has
no budget yet.

## How to run

```sh
PYRE_RESOURCE=1 go test ./tests/resilience -run TestResourceBudget -v
# optional JSON output:
PYRE_RESOURCE=1 PYRE_RESOURCE_OUT=/tmp/budget.json go test ./tests/resilience -run TestResourceBudget -v

PYRE_RESOURCE=1 go test ./tests/resilience -run TestResourceBudgetStack -v   # real processes, ~90 s
```

Runtime ≈ 25 s (transport) / ≈ 90 s (real stack). Each test fails if any measurement exceeds the limits in
`budgetLimits` / `stackBudgetLimits` (`budget_test.go`), so they double as
regression gates.

## Method

- The **node** runs in its own process (the test binary re-executed with
  `PYRE_RESILIENCE_ROLE=node`): a `NodeLink` over an on-disk SQLite database
  with a no-op command handler, logging at Info into a bounded ring. Its CPU
  and RSS are sampled by PID with `internal/diagnostics` (`ps` on macOS,
  `/proc` on Linux; `ps` TIME has 10 ms resolution). This isolates Pyre node
  overhead from agent runtimes, as A20 requires.
- The **hub** is `tests/resilience/refhub` (real `AuthStore`, `enrollment`,
  `topology`, `NodeServer` over on-disk SQLite, plus a minimal durable
  report/command store standing in for WP05) inside the test process, with
  the production heartbeat (15 s). Hub CPU is `getrusage` of that process and
  therefore includes the test's own polling.
- **Event stream**: an in-memory long-poll event source behind the real
  `httpapi` WebSocket stream, consumed by `client.WatchEvents` over loopback.
  Latency = receive time − publish timestamp in the payload (same clock).
- Phases: 20 s connected idle → 10 000-report burst → 200 queued commands →
  hub crash, 5 000 reports queued locally, hub restart, drain → paced and
  burst event streams.

## Environment of the baseline

| item | value |
|---|---|
| commit | `f738600` on `wp18-resilience` (base `71181f3`) |
| OS / arch | macOS 26.6.2, darwin/arm64 |
| CPU / RAM | Apple M2 Pro, 10 cores / 16 GiB |
| Go | go1.27.1, `CGO_ENABLED` default, pure-Go SQLite (modernc) |
| date | 2026-10-04 |
| load | developer laptop, other applications running |

## Measured (run 2 of 2; run 1 in parentheses where it differed)

| metric | measured | unit | proposed limit | note |
|---|---|---|---|---|
| node_idle_cpu_pct | 0 (< 0.05) | % of one core | **1.0** | 20 s window; below `ps` resolution |
| node_idle_rss_mb | 13.6 (21.7) | MiB | **64** | NodeLink + SQLite, Go runtime |
| node_idle_log_bytes_per_min | 0 | B/min | **2048** | Info level, connected idle |
| report_throughput_min_per_s | 8 381 (8 919) | reports/s | **≥ 500** | outbox → hub commit, 10 k burst |
| node_cpu_us_per_report | 232 | µs CPU/report | **1000** | enqueue + send + prune |
| hub_cpu_us_per_report | 98 | µs CPU/report | **1000** | includes test polling |
| node_burst_cpu_pct | 194 (200) | % of one core | — (informational) | ~1.2 s burst; bounded work, not a spin |
| node_burst_peak_rss_mb | 31.0 (33.0) | MiB | **128** | 100 ms sampling |
| node_peak_outbox | 10 000 | reports | — | whole burst enqueued before acks (default MaxOutbox 100 000) |
| node_log_bytes_per_report | 0 | B/report | **1** | no per-item logging at Info |
| hub_log_bytes_per_report | 0 | B/report | **1** | NodeServer at Info |
| node_db_bytes_per_report | 112 | B/report | **300** | DB high-water 1.1 MB after 10 k acked reports; WAL stays ≈ 4 MiB (SQLite auto-checkpoint, 1000 pages) |
| command_latency_p50_ms | 28 (33) | ms | — | 200 commands queued at once, 5 ms poll |
| command_latency_p99_ms | 38 (59) | ms | **500** | queue → result committed at hub |
| outage_drain_5k_s | 0.32 (0.38) | s | **20** | includes reconnect backoff after hub restart |
| event_latency_p50_ms | 0.09 | ms | — | paced 500 events/s |
| event_latency_p99_ms | 0.32 | ms | **250** | paced 500 events/s |
| event_burst_min_per_s | 97 055 (120 881) | events/s | **≥ 2000** | 5 000 events as fast as possible |
| event_burst_latency_p99_ms | 44 (36) | ms | — | includes queueing behind the burst |
| event_burst_cpu_pct | 286 | % of one core | — | server + client in one process |

## Real processes: pyre-hub + pyred (`TestResourceBudgetStack`)

Same environment as above; commit `26ae502` (main `c8bc980` merged). pyred
heartbeat 30 s, poll interval 2 s (idle backoff to 30 s), pyre-hub default
logging (Info access log on stderr). CPU/RSS by PID via `ps`.

| metric | measured (run 2; run 1) | unit | proposed limit | note |
|---|---|---|---|---|
| pyred_idle_cpu_pct | 0.07; 0.13 | % core | **1.0** | connected, no runs, 30 s |
| pyred_idle_rss_mb | 24.0; 23.4 | MiB | **96** | |
| hub_idle_cpu_pct | 0.4; 0.8 | % core | **1.0** | one node connected; closest to its limit |
| hub_idle_rss_mb | 26.8 | MiB | **128** | |
| pyred_idle20_cpu_pct | 0.10; 0.25 | % core | **3.0** | 20 live mock runs observed |
| hub_idle20_cpu_pct | 0.3; 0.75 | % core | — | |
| msg_accept_latency_p50_ms | 4.6; 14.9 | ms | — | owner send → `message.delivery` accepted event, idle recipient |
| msg_accept_latency_p99_ms | 10.2; 29.1 | ms | **2000** | 20 probes |
| msg_send_min_per_s | 117; 181 | msgs/s | **≥ 50** | hub stores sends, 8 concurrent clients |
| msg_accept_per_s | 3.2; 0.7 | msgs/s | — (blocked by WP18-6) | 2 resp. 5 queued messages per session: follow-ups wait for the poll backoff |
| msg_burst_accept_latency_p99_ms | 12 418; 125 933 | ms | — (blocked by WP18-6) | |
| pyred_cpu_ms_per_message | 9.9 | ms CPU/msg | **50** | idle-adjusted |
| hub_cpu_ms_per_message | 14.8 | ms CPU/msg | **50** | idle-adjusted, includes the event watcher |
| pyred_burst_peak_rss_mb | 32.1; 33.9 | MiB | **192** | |
| hub_burst_peak_rss_mb | 35.1; 36.9 | MiB | **256** | |
| hub_log_bytes_per_message | 296; 252 | B/msg | **400** | one Info access-log line per HTTP request; stderr is not rotated by pyre-hub |
| pyred_log_bytes_per_message | 0 | B/msg | **400** | file log rotates (5 MiB × 3) |
| hub_data_dir_mb | 6.5 | MiB | — | 20 sessions + 60 messages |
| pyred_state_dir_mb | 4.5 | MiB | — | WAL ≈ 4 MiB high-water |
| post-burst idle RSS pyred / hub | 31.4 / 35.6 | MiB | — | after the burst (no leak trend over this short run) |

Observations:

- Idle cost is low for both daemons. The hub's idle CPU (0.4–0.8 %) is the
  value closest to its proposed limit; re-measure with several nodes.
- Delivered-message throughput is not a Pyre CPU limit but a scheduling one:
  queued follow-ups used to wait for the backed-off poll (WP18-6, fixed: an
  accepted delivery makes the run's next observation due within
  Poll.Interval, and an idle observation with queued mail kicks delivery).
  The accept-throughput budget can now be measured.
- The hub logs every API request at Info (~300 B per message round trip);
  with chatty clients (TUI/Android polling) its stderr log grows without
  rotation unless the service manager rotates it — operations (WP20) should
  configure rotation or the hub should offer a rotating file sink.

## Timing characteristics observed in the resilience scenarios

These come from `go test ./tests/resilience -v` (EVIDENCE lines) with the
test heartbeat of 200 ms; production values scale with the 15 s heartbeat.

| behaviour | measured (test config) | production expectation |
|---|---|---|
| hub notices a silent node (blackhole) | 0.75 s (3 × 200 ms heartbeat) | 45–50 s (3 × 15 s timeout, checked every timeout/10; was 60–63 s measured with the per-interval check) |
| hub registry shows `disconnected` | 1.2 s (closing handshake bounded at 2 s after the drop) | 47–52 s |
| node notices a silent hub | 1.2 s (SilenceTimeout 1 s) | 45–50 s (3 × advertised heartbeat, checked every timeout/10) |
| reconnect attempts during a 3 s blackhole | 1 (the attempt hangs in the handshake until DialTimeout) | one attempt per DialTimeout (15 s) |
| node reconnect rate, persistent 503 / refused / flapping / 401 | 16–18 / 2 s at Initial 20 ms, Max 200 ms; 401 ≈ 1 per Max | transient failures: ≤ 1 per 10 s at steady state (default Max 10 s; the backoff resets after a connection that lasted 10 s, so repeated short hub outages no longer grow to ~40 s); rejections (401/403/protocol): ≤ 1 per 60 s |
| client RPC under persistent `unavailable` | exactly MaxAttempts (5), Retry-After capped at RetryMax | ≤ 5 requests per call |
| `client.WatchEvents` under a persistently failing stream | was 77 reconnects in 3 s (WP18-1, fixed: progress counts only after an event is delivered or the stream stays up 30 s) | ≤ 12 in 3 s at the same settings (enforced by the test) |
| real stack: revoked pyred reconnect attempts | 3 in 8 s | ≤ 1 per 60 s at steady state |
| real stack: second message queued behind a busy run | was 35 s after the first (WP18-6, fixed): now ≈ 2.1 s | ~2–6 s (poll interval; enforced ≤ 10 s by the test) |

## Real harness and Hive (fleet, 2026-10-05)

Three-node fleet plus an isolated Hive node, real Claude 2.1.289 on server
through rota checkout.
Hook overhead is measured by replaying the real hook payloads of the run
through its exact hook command (`tests/e2e/hookbench.py`, `/bin/sh -c`,
payload on stdin, 100 runs per event). Proposed limits are 3-8x the
measured values, like the rest of this file.

| metric | measured | unit | proposed limit | note |
|---|---|---|---|---|
| claude_hook_wall_ms_p95 (Linux) | 5.5-6.0 (was 19-22) | ms per invocation | **15** | server; was 18 ms p50 before pyred stopped linking the TUI (go-runewidth init) |
| claude_hook_wall_ms_p95 (macOS) | 18-23 (was 40-56) | ms per invocation | **40** | M2 Pro; includes process start (~10 ms) and fsync |
| pyred_start_ms_p50 (macOS) | 10 (was 33) | ms | **20** | `pyred version`; guarded by `TestPyredDoesNotLinkTheTUI` |
| pyred_cpu_ms_per_real_turn | 24-29 | ms CPU per Claude turn | **150** | transcript/hook-event observation, one Read tool call per turn |
| pyred_idle_cpu_pct_with_real_session | 0.10 | % core | **1.0** | one idle Claude session |
| pyred_rss_mb_with_real_session | 27-32 | MiB | **96** | agents (Claude + 3 shells) held 486 MiB, reported by `pyred status` |
| cached_read_latency_ms (hub down) | 91-185 | ms per CLI command | **500** | node read cache via the local socket; was 2.3-5.2 s before single-attempt proxying |
| hive_bridge_index_s_per_note | 0.45 | s per note | **2** | sequential `hive note`; 30 notes in 13.5 s |
| hive_bridge_pyred_cpu_ms_per_note | 2.7 | ms CPU per note | **20** | pyred side only (Hive subprocess CPU excluded) |

Hive's own plugin hooks are not Pyre overhead but run in the same sessions:
SessionStart 643 ms, UserPromptSubmit 117 ms, Stop 116 ms, SessionEnd 320 ms
(slowest command per event, server, isolated store).

## Multi-hour soak (fleet, 2026-10-05)

3 h 10 min on the three-node fleet (hub + pyred on server, pyred on the Mac and
laptop) at about one session-to-session message per second, with tickers,
artifact/doc writes, attach cycles, one real Claude session, and per hour a
hub restart, a pyred restart and a 2-minute network cut
(soak scripts not included in this repository). "Before" is the soak build; "after" is the same
270 MB hub database and load on the fixed build (`fleet-b29ac36`).

| metric | measured | unit | proposed limit | note |
|---|---|---|---|---|
| soak_messages_lost_or_duplicated | 0 of 11,201 | messages | **0** | hub, receipts and runtime transcripts; needed the deferred-delivery fix |
| hub_cpu_ms_per_message (idle-adjusted, 1 msg/s, 270 MB DB) | 20 after (85 before) | ms CPU/msg | **50** (existing) | before: receipt dedup missed its partial index and SQLite connections churned, so the cost grew with history (78 → 115 ms raw) |
| hub_idle_cpu_pct with 270 MB of history, 3 nodes, 19 sessions | 0.89 after (2.02 before) | % core | **1.0** (existing) | maintenance queries scanned the event log and dedup table every 10 s |
| pyred_cpu_ms_per_message (idle-adjusted, per fleet message) | server 6, Mac 20, laptop 6–9 | ms CPU/msg | **50** (existing) | Mac higher: fullfsync on every commit |
| pyred_rss_mb over 3 h | ≤ 42 (server), ≤ 40 (Mac), ≤ 33 (laptop) | MiB | **96** (existing) | growth only where `--dev-mock` holds transcripts (640 B/msg by design) |
| goroutines / open FDs slope | flat (abs slope ≤ 1/h, r ≤ 0.6) | per hour | **no trend** | per process lifetime, settled samples |
| hub_restart_reconnect_s | 1.2–8.7 | s | **30** | 12 s downtime, 3 nodes |
| pyred_restart_reconnect_s | 0.1 | s | **15** | shells and Claude survive; mocks resumed in 1.2–2.7 s |
| net_drop_reconnect_s | 1.1–3.5 | s after restore | **30** | 2-minute tunnel kill |
| silent_hub_detect_s (blackhole) | node 46.1, hub 46.5–47.0 | s | **45–50** (existing) | SIGSTOPped tunnel |
| net_blackhole_reconnect_s | 1.3 | s after restore | **75** | |
| backlog_drain_s_after_reconnect | ≤ 45.3 | s | **60** | every message stored during the fault accepted |
| msg_steady_latency_p99_s | 6.4 (p50 0.7) | s | **10** | stored → accepted, mail queued behind a busy mock; no drift (−0.24 s/h) |
| hub_stop_s with long-polls in flight | 1.2 after (10.0 before) | s | **3** | `TestServeStopsPromptlyWithLongPollInFlight` |
| hub_log_bytes_per_message (soak) | 264 | B/msg | **400** (existing) | rotating `--log-file` |
| pyred_stderr_bytes_per_message | warnings only (was a full duplicate of the rotated log) | B/msg | **≈ 0** | `TestLoggerKeepsInfoOutOfStderr` |
| hub_db_growth_mb_per_hour (1 msg/s) | ~80 | MiB/h | — (informational) | events (bounded at 200k ≈ 470 MB here), messages/receipts (permanent, ~3.2 KB/msg); finished commands and idempotency records now pruned after 7/30 days |
| codex_pyred_processes_per_turn | 0 | processes | **0** | JSON-RPC inside pyred; no Codex hooks or notify installed by Pyre |
| codex_pyred_cpu_ms_per_real_turn | 19–28 (idle-adjusted; 26–40 raw) | ms CPU/turn | **150** | real Codex 0.160.0 on server, 3 × 10 one-word turns |

## Soak round 2 (fleet, 2026-10-05)

Fixed build, the same fault schedule, real Claude and Codex, plus an isolated
Hive node.

| metric | measured | unit | proposed limit | note |
|---|---|---|---|---|
| soak_messages_lost_or_duplicated | 0 of 14,111 | messages | **0** | incl. 2,341 sent during a 90-min Mac outage (node outbox, drained in 27 min) |
| hub_cpu_pct at 1 msg/s, storm-free, 4 nodes | 3.4 (slope +0.09 %/h) | % core | — | round 1 before fixes: 7.8 → 11.5 |
| hub_idle_cpu_pct, 5 h of history, 4 nodes | 0.80 | % core | **1.0** (existing) | |
| hub_stop_s with long-polls in flight | 0.40–0.69 | s | **3** | |
| events_list_requests_per_node_after_pruning | ≤ 1 per reset (was ~200/s) | requests | **no storm** | `TestWatchNotes*` |
| hive_enrichment_wall_s (detached `claude -p`) | p50 6.1, max 22.5 | s | — (Hive's) | 0.85 s CPU, 260 MiB per `claude -p`; never inside a hook |
| hive_hook_ms (isolated store) | 104–204 (max 305) | ms | — (Hive's) | |
| rota_checkin_after_stop_with_enrichment_s | 12.8–34.6 | s | **≤ enrichment + 15** | documented wait for profile-bound processes |

Resource pressure: **not measured; do not run on owner machines.** The
memory phase on server made systemd-oomd kill an owner tmux scope (memory
pressure of `user@1000.service` above 50 %). `soak_pressure.sh` refuses to
start where systemd-oomd is active.

## Proposed limits — rationale

- Limits are 4–10× the measured values so slower CI hosts and Linux VMs pass,
  while a regression to per-item logging, an idle busy loop, unbounded
  outbox growth or a retry storm fails loudly.
- Idle CPU ≤ 1 % and idle RSS ≤ 64 MiB per node daemon are the interactive-
  responsiveness envelope for the user's laptop; the daemon will add runtime
  supervision (WP06) and must be re-measured against the same limits.
- Logging is budgeted per item and per idle minute rather than in absolute
  bytes: anything per-report at Info is a regression; operational lines
  (connect/disconnect) are allowed.
- Queue bounds that already exist and are enforced by tests:
  NodeLink `MaxOutbox` (default 100 000; `capacity_exhausted` beyond, see
  `TestNodeOutboxBoundedDuringOutage`), `SendWindow` 256 in flight,
  `DefaultMaxFrameBytes` 4 MiB, httpapi body 4 MiB / chunk 8 MiB, artifact
  `MaxBytes` / `MaxTotalBytes` (`TestStoragePressureArtifacts`), enrollment
  exchange rate limit 10 per IP burst.

## Not measured yet (remaining A20 scope)

- Linux nodes (server) — the sampler supports `/proc`, but no Linux run was
  authorized/available for this baseline.
- Hive model enrichment, TUI responsiveness (WP15) and Android. (Claude hooks, real transcript capture and Hive capture/indexing:
  see "Real harness and Hive" above.)
- Several nodes and repeated outages over hours: measured in the multi-hour
  soak above (fleet scripts), not as an automated `go test` budget.
- Codex hooks: none exist (see the soak section); Hive model enrichment and
  throttling under resource pressure remain unmeasured.
