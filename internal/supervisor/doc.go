// Package supervisor holds node-side process supervision helpers used by the
// Pyre node daemon (pyred): a minimal client for Pyre-owned tmux servers
// (Tmux: session queries, attached clients, server PID, kill-only-ours),
// systemd transient-scope placement for the run server (scope.go), process
// liveness, resource sampling that separates Pyre's own overhead from agent
// processes, and bounded rotating log files.
//
// # Ownership model
//
// A node runs ONE Pyre-owned tmux server shared by all runs, on a private
// socket at <state>/tmux/pyre.sock (directory 0700; built by cmd/pyred
// buildAdapters and shared by the Claude, Codex and shell adapters). Each
// run is one tmux session on that server, named by its run ID; the
// persisted RunHandle records TmuxSocket and TmuxTarget (= run ID). The
// Codex adapter adds one "codex-as-<key>" session per Codex home for its
// shared app-server.
// The server lifecycle lives in internal/runtime/tmux: the config is written
// next to the socket (<state>/tmux/tmux.conf: exit-empty off,
// remain-on-exit on, bounded history; the user's ~/.tmux.conf is never
// read), and the server is started by a bare "start-server"
// (runtime/tmux.EnsureServer, once per socket) because a daemonized tmux
// server keeps the argv of the client that started it visible in ps for its
// whole life. Per-run environment, including the session token, reaches
// only the run's pane process: it is written to a private 0600 file that
// the pane launcher (runtime/tmux execenv.go) reads, deletes and applies
// before exec'ing the harness, so it is on no command line and not in the
// tmux session environment. Panes also inherit the server's (pyred's)
// environment. See docs/capabilities.md "Shared node layer".
//
// The tmux server double-forks and is reparented to init/launchd (or runs in
// its own systemd scope, below), so killing or restarting pyred never kills
// a run (runtime REPORT C1, verified on macOS and Linux). A restarted
// daemon re-establishes control from the persisted RunHandle (socket +
// target) and never launches a second process for the same run. Stopping a
// run terminates only its session (runtime/tmux TerminateSession), never the
// shared server.
//
// Tmux.Kill (kill-server) refuses any socket outside the configured owned
// root, so Pyre can never kill the user's default tmux server or another
// program's server. The daemon uses it only to clean up a per-run socket
// (<state>/runs/<run_id>/tmux.sock) that a failed Launch may have left
// behind (test and mock adapters use per-run servers); Tmux's other methods
// pass "-f /dev/null", which is ignored by an already-running server.
//
// # Service manager requirements (for WP20 packaging)
//
// The daemon itself is supervised by the platform's user service manager.
// The service definition MUST NOT kill the run processes when pyred stops,
// restarts, or is upgraded:
//
//   - systemd (Linux, user unit), default: the Pyre tmux server is started
//     in its own transient scope in pyre-runs.slice (scope.go,
//     runtime/tmux.EnsureServer; PYRE_SYSTEMD_SCOPE=require in the unit), so
//     pyred.service keeps the default KillMode=control-group and its own
//     MemoryHigh/MemoryMax/CPUQuota, and stop/restart never signals runs.
//     The scope changes the cgroup only: runs still inherit pyred's
//     environment, rlimits, NoNewPrivileges and mount namespace, so the unit
//     must not use NoNewPrivileges or namespace sandboxing. tmux builds with
//     systemd support additionally put each pane in a tmux-spawn-*.scope in
//     the same slice. Restart=on-failure, RestartSec=2s. Lingering
//     ("loginctl enable-linger") keeps the user manager, and so the slice,
//     alive across logout.
//   - systemd legacy (install.sh --no-scope, or systemd-run missing):
//     KillMode=process so only the pyred main process is signalled; runs
//     stay in pyred's cgroup, so the unit sets no limits; PYRE_SYSTEMD_SCOPE=off.
//   - launchd (macOS, LaunchAgent): AbandonProcessGroup=true so launchd does
//     not SIGKILL the process group when pyred exits; KeepAlive with
//     SuccessfulExit=false; ProcessType=Background is acceptable.
//     The tmux servers have already escaped the process group via setsid,
//     but AbandonProcessGroup is still required for children started
//     before the double fork completes.
//   - Both: run as the owning user (never root); the state directory and
//     sockets are created 0700/0600 by pyred; stdout/stderr may go to the
//     service manager's journal, but pyred also writes a bounded rotating
//     log (RotatingFile) under <state>/logs.
//   - Stop ordering: stopping pyred is not stopping runs. An explicit
//     "pyre session stop" (or uninstall with --stop-runs) is the only path
//     that terminates runs.
//
// Units are installed by scripts/install/install.sh from packaging/ (WP20).
package supervisor
