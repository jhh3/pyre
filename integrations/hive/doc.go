// Package hive adapts the user's existing Hive memory system (the
// jhh3/hive Claude Code plugin and its vendored hive-mem store) to Pyre's
// memory contracts (contracts.MemoryStatus, MemorySearchResult, MemoryHit,
// Note sync and LaunchContext pointers).
//
// Hive stays the owner of capture, enrichment, ranking and vault sync. This
// package only uses Hive's supported, machine-readable interfaces:
//
//   - `hive search … --json` / `hive show <uid> --json`
//     (NDJSON emitted by the vendored hive-mem ranker) for retrieval;
//   - `hive note --kind K -- <text>` (the explicit write path, redacted and
//     sanitised by Hive) for publishing hub notes/handoffs/decisions;
//   - Hive's own state files (sync.push.stamp, tick.log, breaker.*) and a
//     strictly read-only (mode=ro, query_only) connection to the derived
//     SQLite index for freshness, provenance (hive_sessions) and as a
//     degraded local search path when the CLI is unavailable.
//
// Nothing here writes to the Hive index, vault, hooks, crontab or config
// directly. Every subprocess and query is bounded by a timeout and an
// output cap, and failures degrade to a reported status rather than
// blocking messaging or session launch.
package hive
