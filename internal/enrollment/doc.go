// Package enrollment issues and redeems the one-time secrets that bring
// nodes and owner clients into a Pyre fleet.
//
//	enr, err := enrollment.NewStore(db, auth, clock, enrollment.Options{
//		HubURL: "https://hub:7443", HubCertSHA256: cert.Fingerprint,
//	}) // namespace "enrollment"
//	enr.Register(mux, enrollment.HandlerOptions{Logger: logger}) // POST PathEnrollExchange, PathPairExchange
//
// Node enrollment: the owner calls FleetAPI.CreateEnrollment (topology),
// which creates a ticket {node_id, pye_ secret, expiry, hub URL, pin}. The
// node redeems it once at contracts.PathEnrollExchange (ExchangeEnrollment
// on the node side) for a NodeCredential and stores it with
// SaveNodeCredential (0600; LoadNodeCredential refuses group/world-readable
// files, symlinks, and files owned by another user).
//
// Client pairing: FleetAPI.CreatePairing returns a PairingTicket whose URI is
// pyre://pair?hub=<url>&pin=<sha256>&id=<pairing_id>&code=<code>; the client
// parses it with ParsePairingURI and redeems it once with ExchangePairing.
// BootstrapOwnerClient mints the first owner credential locally on the hub.
//
// Secrets are stored only as SHA-256 hashes and compared in constant time;
// unknown IDs and wrong secrets get a uniform unauthorized error; tickets
// are invalidated after MaxFailedAttempts wrong secrets; exchange endpoints
// are rate limited per remote IP and globally; nothing secret is logged.
//
// SSH-assisted bootstrap: SSHBootstrapPlan builds the exact scp/ssh argv
// steps that copy pyred into a user-owned directory on a host reached
// through the user's existing SSH alias and run `pyred enroll` with the
// secret on stdin. It installs no service and never weakens host-key
// checking. Execute runs a plan through an injectable Runner, or prints it
// in dry-run mode.
package enrollment
