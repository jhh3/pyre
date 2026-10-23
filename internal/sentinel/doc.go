// Package sentinel is the hub-independent node watchdog (`pyred sentinel`).
//
// A few always-on machines each run a sentinel. Every minute a sentinel
// checks its own network, then probes each watched host: the hub's
// unauthenticated /healthz on every configured address, one ssh session
// (BatchMode) that reads the systemd user manager's state, configured units,
// local TCP ports, the host's maintenance window and, when the host runs a
// sentinel too, that sentinel's state.json, and public HTTPS endpoints. The
// same ssh session drops this sentinel's own state into the peer's
// peers/ directory, so a peer that can be reached in only one direction is
// still heard.
//
// Votes and incidents live in ~/.local/state/pyre/sentinel/state.json. A
// target is DOWN only when two live sentinels (never the target itself)
// vote down for DownGrace; a lone live sentinel can only report "DOWN
// (unconfirmed)". DEGRADED (a unit, port, tunnel or sentinel missing) needs
// DegradedGrace; only public URLs failing needs PublicGrace and the
// target's own check of them failing too. Watchers that cannot reach a
// target that another watcher (or its own sentinel) shows healthy are a
// partition: one link message (links.go), not DOWN. Grace periods count
// from the end of the target's maintenance window. Each target has one actor, the first live sentinel in
// rank order other than the target (the leader, except for the leader's own
// host): it alone sends Slack DMs (one per incident, reminders, one
// "recovered") and runs the only automatic recovery, an ssh login that
// starts the user's default.target (at most MaxRecoveryAttempts per
// incident; never sudo, never a kill). Other sentinels mirror the actor's
// incidents so a new actor neither repeats an alert nor a recovery attempt.
//
// The hub is not involved: everything works with server (and its hub) down.
// If every sentinel is down nothing alerts; there is no external dead-man
// service.
package sentinel
