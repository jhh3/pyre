// Package topology is the hub's node registry. It implements
// contracts.FleetAPI and records connection observations.
//
// Hub composition (all stores share the hub database):
//
//	auth, _  := transport.NewAuthStore(db, clock)
//	cert, _  := transport.EnsureHubCert(stateDir, hosts)
//	enr, _   := enrollment.NewStore(db, auth, clock, enrollment.Options{HubURL: url, HubCertSHA256: cert.Fingerprint})
//	fleet, _ := topology.NewStore(db, auth, enr, eventSink, clock) // namespace "fleet" (+ "ops")
//	fleet.ResetConnections(ctx) // once at startup: no connection survives a restart
//
//	cfg, _ := fleet.NodeServerConfig(topology.HubHooks{
//		Welcome:      hub.nodeWelcome,   // committed report watermark + commands with Seq > LastCommandSeq
//		OnReport:     hub.commitReport,  // idempotent by (node_id, origin_seq)
//		OnCommandAck: hub.markDelivered,
//	})
//	cfg.Auth, cfg.HubID = auth, hubID
//	ns, _ := transport.NewNodeServer(cfg) // also the hub's contracts.NodeChannel
//	ns.DisconnectOnRevoke(auth)
//	mux.Handle(contracts.PathNodeConnect, ns)
//	enr.Register(mux, enrollment.HandlerOptions{Logger: logger})
//
// Mutating FleetAPI calls require an owner (or system) principal in ctx and
// are idempotent by operation_id; enrollment secrets and pairing codes are
// never stored in the operations table (a replay rotates the secret while
// the ticket is unredeemed). Events are appended through the EventSink
// inside each write transaction: node.enrolled, node.status, node.revoked
// carry Node; node.connection carries NodeConnection.
//
// Connection state is never/connected/disconnected/revoked. ObserveHello
// updates the single row for a node_id (reconnects never duplicate nodes)
// under the connection's ID; ObserveDisconnect applies only for the current
// connection ID, recording last_seen_at as the last frame received.
// NodeFreshness reports anything but a recently observed connected node as
// stale; stale never implies idle.
package topology
