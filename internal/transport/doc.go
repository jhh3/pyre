// Package transport provides Pyre's authenticated connectivity: credential
// storage and verification, hub TLS identity and pinning, and the node
// control channel (contracts/nodeproto.go) on both sides.
//
// # Credentials (hub)
//
//	auth, err := transport.NewAuthStore(db, clock) // namespace "auth"
//
// AuthStore implements contracts.Authenticator and contracts.TokenIssuer.
// Tokens are 256-bit random strings tagged by prefix (pyc_ owner client,
// pyn_ node, pys_ session, pyt_ ticket; enrollment uses pye_/pyp_) and are
// stored only as SHA-256 hashes, compared in constant time, and revocable
// per principal (RevokePrincipal / RevokePrincipalTx + NotifyRevoked).
// Tickets are purpose-bound, expire, and redeem exactly once. Redact and
// NewRedactingLogger scrub secrets from strings and logs; every logger in
// this package and in enrollment is wrapped with redaction.
//
// # TLS
//
//	cert, err := transport.EnsureHubCert(dir, hosts) // self-signed ECDSA, key 0600, stable pin
//	srv.TLSConfig = cert.ServerTLSConfig()           // cert.Fingerprint goes into tickets/pairing URIs
//	tlsCfg, err := transport.PinnedTLSConfig(pin)    // client: accept exactly that leaf
//	tlsCfg := transport.CATLSConfig(nil)             // client: normal CA validation
//
// # Node channel, hub side
//
// Usually built through topology.Store.NodeServerConfig, which keeps the
// node registry current:
//
//	cfg, _ := fleet.NodeServerConfig(topology.HubHooks{Welcome: ..., OnReport: ..., OnCommandAck: ...})
//	cfg.Auth, cfg.HubID = auth, hubID
//	ns, err := transport.NewNodeServer(cfg) // http.Handler for contracts.PathNodeConnect; contracts.NodeChannel
//	ns.DisconnectOnRevoke(auth)
//	mux.Handle(contracts.PathNodeConnect, ns)
//	defer ns.Close()
//
// The server authenticates the bearer token (node principals only; the hello
// node_id must match), negotiates the protocol (mismatch: FrameError
// protocol_mismatch with an actionable message, then close), calls OnHello,
// sends Welcome and the returned pending commands, pings every
// HeartbeatInterval, re-authenticates the token every beat (revocation
// backstop), and closes on silence. Reports are passed to OnReport and
// acknowledged (ReportAck) with the contiguous committed watermark only
// after OnReport returns nil; duplicates are re-acked. A newer connection
// for the same node replaces the old one (OnDisconnect with Replaced=true).
//
// # Node channel, node side
//
//	link, err := transport.NewNodeLink(transport.NodeLinkConfig{
//		HubURL: cred.HubURL, NodeID: cred.NodeID, Token: cred.Token,
//		TLS: tlsCfg, DB: nodeDB, Hello: helloFn, Handler: commandHandler,
//	}) // namespace "nodelink" in the node's database
//	go link.Run(ctx)
//	link.Enqueue(ctx, contracts.ReportRun, run)  // durable, OriginSeq assigned
//	link.SendStatus(ctx, status)                 // replaceable, never queued
//
// NodeLink reconnects with bounded exponential backoff and jitter (maximum
// backoff only after a rejection: revoked credential or protocol mismatch),
// keeps a durable report outbox and resends everything after
// Welcome.CommittedReportSeq, and journals received commands before
// acknowledging them: duplicates are re-acked and never re-executed (a
// stored result is returned again), expired commands are recorded as
// failed/expired without execution, and a command interrupted by a crash is
// reported indeterminate rather than replayed. Expiry is judged on the hub
// clock: NodeLink.HubNow is an upper bound of it derived from the
// hello/welcome round trip, so a skewed node clock neither refuses valid
// commands nor starts ones the hub considers expired.
package transport
