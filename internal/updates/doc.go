// Package updates manages versioned tools on a node: claude, codex, pyred/pyre
// and plugins ("plugin:<name>").
//
// Layout under the Pyre state dir:
//
//	<state>/tools/state.json                    version records (contracts.ToolVersion + source/exe)
//	<state>/tools/<tool>/<version>/...           immutable payload (binary, extracted archive, or npm prefix)
//	<state>/tools/<tool>/current -> <version>    default for NEW runs
//
// Stage fetches a candidate (local path, file:// or https:// URL, or
// "npm:<package>@<version>"), verifies its sha256 digest (required for
// network sources unless Options.AllowUnverified), installs it into its own
// version directory (npm packages go into an isolated --prefix; the user's
// global install is never touched) and runs a health check (`<tool>
// --version`, plus an optional smoke command). A failed check marks the
// version failed and removes its payload.
//
// Activate switches the `current` pointer atomically and re-runs the health
// check through it; on failure the pointer is reverted to the previous active
// version, the candidate is marked failed and an update.result event is
// reported. Activation never restarts or touches running sessions: the daemon
// pins each run to Pinned(tool).Path, a real path inside a version directory,
// so a running run keeps its executable across activations (InUseByRuns). At
// least one healthy previous version is kept while anything references it;
// Prune never removes the active version, a version in use, or the newest
// rollback target. Nothing upgrades automatically: there is no code path
// that stages or activates a version unless update.stage/update.activate is
// explicitly requested, and staging never changes the default.
//
// Activating pyred additionally checks protocol compatibility
// (CheckProtocolCompat) against the hub's supported range and refuses with an
// actionable ErrProtocolMismatch. The daemon self-swap is coordinated with
// WP06/WP20; PlanDaemonSwap documents the drain/restart/rollback steps.
//
// Rollout runs a canary node first and the remaining nodes afterwards with
// bounded concurrency; a failed canary stops the rollout, and a failure on
// one node never forces upgrades elsewhere.
package updates
