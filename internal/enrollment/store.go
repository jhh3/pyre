package enrollment

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
	"github.com/jhh3/pyre/internal/transport"
)

// Migrations are the "enrollment" namespace migrations.
var Migrations = []sqlitex.Migration{{
	Namespace: "enrollment", Version: 1, Name: "tickets_pairings_clients", SQL: `
CREATE TABLE enroll_tickets (
	node_id         TEXT PRIMARY KEY,
	secret_hash     TEXT NOT NULL,
	name            TEXT NOT NULL,
	ssh_alias       TEXT NOT NULL DEFAULT '',
	created_by      TEXT NOT NULL,
	created_at      TEXT NOT NULL,
	expires_at      TEXT NOT NULL,
	redeemed_at     TEXT,
	failed_attempts INTEGER NOT NULL DEFAULT 0,
	invalidated_at  TEXT
);
CREATE TABLE pairings (
	pairing_id      TEXT PRIMARY KEY,
	code_hash       TEXT NOT NULL,
	client_name     TEXT NOT NULL DEFAULT '',
	kind            TEXT NOT NULL DEFAULT '',
	created_by      TEXT NOT NULL,
	created_at      TEXT NOT NULL,
	expires_at      TEXT NOT NULL,
	redeemed_at     TEXT,
	client_id       TEXT,
	failed_attempts INTEGER NOT NULL DEFAULT 0,
	invalidated_at  TEXT
);
CREATE TABLE clients (
	client_id  TEXT PRIMARY KEY,
	name       TEXT NOT NULL,
	kind       TEXT NOT NULL,
	created_at TEXT NOT NULL,
	revoked_at TEXT,
	pairing_id TEXT
);
`}}

// Defaults and limits.
const (
	DefaultEnrollTTL  = time.Hour
	DefaultPairingTTL = 10 * time.Minute
	MaxSecretTTL      = 24 * time.Hour
	// MaxFailedAttempts invalidates a ticket/pairing after this many wrong
	// secrets, so even a leaked ID cannot be brute-forced.
	MaxFailedAttempts = 5
)

// ClientKinds are the accepted owner client kinds.
var ClientKinds = map[string]bool{"cli": true, "android": true, "tui": true}

// Options configure what tickets advertise to nodes and clients.
type Options struct {
	HubURL        string // e.g. https://hub.example:7443
	HubCertSHA256 string // pin for a self-signed hub; empty for CA-issued certs
	RateLimit     RateLimitConfig
}

// NodeEnrollment is passed to the node-enrolled hook inside the redemption
// transaction.
type NodeEnrollment struct {
	NodeID     string
	Name       string
	SSHAlias   string
	Hostname   string
	Platform   string
	Arch       string
	EnrolledAt time.Time
}

// Store owns enrollment tickets, pairing tickets, and owner client records.
type Store struct {
	db    *sql.DB
	auth  *transport.AuthStore
	clock contracts.Clock
	opts  Options
	limit *limiter

	mu         sync.Mutex
	onEnrolled func(ctx context.Context, tx *sql.Tx, e NodeEnrollment) error
}

// NewStore migrates the "enrollment" namespace and returns the store. auth
// issues node and owner credentials inside redemption transactions.
func NewStore(db *sql.DB, auth *transport.AuthStore, clock contracts.Clock, opts Options) (*Store, error) {
	if auth == nil {
		return nil, errors.New("enrollment: auth store required")
	}
	if clock == nil {
		clock = contracts.SystemClock{}
	}
	if opts.HubCertSHA256 != "" {
		fp, err := transport.NormalizeFingerprint(opts.HubCertSHA256)
		if err != nil {
			return nil, err
		}
		opts.HubCertSHA256 = fp
	}
	if err := sqlitex.Migrate(context.Background(), db, Migrations...); err != nil {
		return nil, err
	}
	return &Store{db: db, auth: auth, clock: clock, opts: opts, limit: newLimiter(opts.RateLimit)}, nil
}

// SetNodeEnrolledHook registers fn, called inside the redemption
// transaction after the node credential is minted. topology.NewStore
// registers itself here to create the node record and emit node.enrolled
// atomically with redemption. A hook error aborts the redemption.
func (s *Store) SetNodeEnrolledHook(fn func(ctx context.Context, tx *sql.Tx, e NodeEnrollment) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onEnrolled = fn
}

// HubURL returns the configured hub URL.
func (s *Store) HubURL() string { return s.opts.HubURL }

// HubCertSHA256 returns the configured hub pin.
func (s *Store) HubCertSHA256() string { return s.opts.HubCertSHA256 }

func ttlOrDefault(seconds int, def time.Duration) time.Duration {
	if seconds <= 0 {
		return def
	}
	d := time.Duration(seconds) * time.Second
	if d > MaxSecretTTL {
		d = MaxSecretTTL
	}
	return d
}

func who(p contracts.Principal) string { return string(p.Kind) + ":" + p.ID }

func cleanName(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) > max {
		s = s[:max]
	}
	return s
}

// ---- node enrollment ----

// CreateTicketTx creates a one-time enrollment ticket for a new node ID. The
// secret is returned once; only its hash is stored.
func (s *Store) CreateTicketTx(ctx context.Context, tx *sql.Tx, req contracts.EnrollmentRequest, by contracts.Principal) (contracts.EnrollmentTicket, error) {
	name := cleanName(req.Name, 128)
	if name == "" {
		return contracts.EnrollmentTicket{}, contracts.Errorf(contracts.ErrInvalid, "node name is required")
	}
	secret, err := transport.NewSecret(transport.PrefixEnrollSecret)
	if err != nil {
		return contracts.EnrollmentTicket{}, err
	}
	now := s.clock.Now()
	exp := now.Add(ttlOrDefault(req.TTL, DefaultEnrollTTL))
	nodeID := contracts.NewID(contracts.PrefixNode)
	if _, err := tx.ExecContext(ctx, `INSERT INTO enroll_tickets(node_id, secret_hash, name, ssh_alias, created_by, created_at, expires_at) VALUES(?,?,?,?,?,?,?)`,
		nodeID, transport.HashSecret(secret), name, cleanName(req.SSHAlias, 256), who(by), sqlitex.FormatTime(now), sqlitex.FormatTime(exp)); err != nil {
		return contracts.EnrollmentTicket{}, err
	}
	return contracts.EnrollmentTicket{NodeID: nodeID, Secret: secret, ExpiresAt: exp, HubURL: s.opts.HubURL, HubCertSHA256: s.opts.HubCertSHA256}, nil
}

// RotateTicketSecretTx replaces the secret of an unredeemed, unexpired
// ticket and returns the ticket with the new secret. Idempotent replays of
// nodes.enroll use it because the original plaintext was never stored; the
// previous secret stops working.
func (s *Store) RotateTicketSecretTx(ctx context.Context, tx *sql.Tx, nodeID string) (contracts.EnrollmentTicket, error) {
	var exp string
	var redeemed, invalid sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT expires_at, redeemed_at, invalidated_at FROM enroll_tickets WHERE node_id=?`, nodeID).Scan(&exp, &redeemed, &invalid)
	if errors.Is(err, sql.ErrNoRows) {
		return contracts.EnrollmentTicket{}, contracts.Errorf(contracts.ErrNotFound, "enrollment ticket not found").WithRef(contracts.KindNode, nodeID)
	}
	if err != nil {
		return contracts.EnrollmentTicket{}, err
	}
	if redeemed.Valid {
		return contracts.EnrollmentTicket{}, contracts.Errorf(contracts.ErrConflict, "enrollment already redeemed; the node is enrolled").WithRef(contracts.KindNode, nodeID)
	}
	expAt, _ := sqlitex.ParseTime(exp)
	if invalid.Valid || !s.clock.Now().Before(expAt) {
		return contracts.EnrollmentTicket{}, contracts.Errorf(contracts.ErrExpired, "enrollment ticket expired or invalidated; create a new one").WithRef(contracts.KindNode, nodeID)
	}
	secret, err := transport.NewSecret(transport.PrefixEnrollSecret)
	if err != nil {
		return contracts.EnrollmentTicket{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE enroll_tickets SET secret_hash=? WHERE node_id=?`, transport.HashSecret(secret), nodeID); err != nil {
		return contracts.EnrollmentTicket{}, err
	}
	return contracts.EnrollmentTicket{NodeID: nodeID, Secret: secret, ExpiresAt: expAt, HubURL: s.opts.HubURL, HubCertSHA256: s.opts.HubCertSHA256}, nil
}

// errBadSecret is the uniform answer for unknown IDs and wrong secrets.
func errBadSecret() error {
	return contracts.Errorf(contracts.ErrUnauthorized, "invalid or unknown enrollment credentials")
}

// dummyHash keeps unknown-ID handling at the same cost as a real compare.
var dummyHash = transport.HashSecret("pyre-dummy")

// RedeemNode exchanges a node enrollment secret for a node credential,
// exactly once. Errors are uniform (unauthorized) unless the caller proved
// possession of the secret, in which case expired/conflict are reported.
func (s *Store) RedeemNode(ctx context.Context, ex contracts.NodeEnrollExchange) (contracts.NodeCredential, error) {
	if ex.NodeID == "" || !strings.HasPrefix(ex.Secret, transport.PrefixEnrollSecret) {
		transport.SecretMatches(dummyHash, ex.Secret)
		return contracts.NodeCredential{}, errBadSecret()
	}
	var cred contracts.NodeCredential
	var userErr error
	err := sqlitex.Tx(ctx, s.db, func(tx *sql.Tx) error {
		var hash, name, alias, exp string
		var redeemed, invalid sql.NullString
		var failed int
		err := tx.QueryRowContext(ctx, `SELECT secret_hash, name, ssh_alias, expires_at, redeemed_at, failed_attempts, invalidated_at FROM enroll_tickets WHERE node_id=?`, ex.NodeID).
			Scan(&hash, &name, &alias, &exp, &redeemed, &failed, &invalid)
		if errors.Is(err, sql.ErrNoRows) {
			transport.SecretMatches(dummyHash, ex.Secret)
			userErr = errBadSecret()
			return nil
		}
		if err != nil {
			return err
		}
		if !transport.SecretMatches(hash, ex.Secret) {
			userErr = errBadSecret()
			if redeemed.Valid || invalid.Valid {
				return nil
			}
			failed++
			if failed >= MaxFailedAttempts {
				_, err = tx.ExecContext(ctx, `UPDATE enroll_tickets SET failed_attempts=?, invalidated_at=? WHERE node_id=?`, failed, sqlitex.FormatTime(s.clock.Now()), ex.NodeID)
			} else {
				_, err = tx.ExecContext(ctx, `UPDATE enroll_tickets SET failed_attempts=? WHERE node_id=?`, failed, ex.NodeID)
			}
			return err
		}
		if redeemed.Valid {
			userErr = contracts.Errorf(contracts.ErrConflict, "enrollment ticket already redeemed; create a new ticket to re-enroll").WithRef(contracts.KindNode, ex.NodeID)
			return nil
		}
		if invalid.Valid {
			userErr = contracts.Errorf(contracts.ErrExpired, "enrollment ticket invalidated after repeated failures; create a new ticket").WithRef(contracts.KindNode, ex.NodeID)
			return nil
		}
		now := s.clock.Now()
		if expAt, _ := sqlitex.ParseTime(exp); !now.Before(expAt) {
			userErr = contracts.Errorf(contracts.ErrExpired, "enrollment ticket expired; create a new ticket").WithRef(contracts.KindNode, ex.NodeID)
			return nil
		}
		res, err := tx.ExecContext(ctx, `UPDATE enroll_tickets SET redeemed_at=? WHERE node_id=? AND redeemed_at IS NULL`, sqlitex.FormatTime(now), ex.NodeID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			userErr = contracts.Errorf(contracts.ErrConflict, "enrollment ticket already redeemed")
			return nil
		}
		tok, err := s.auth.IssueTx(ctx, tx, contracts.PrincipalNode, ex.NodeID, name)
		if err != nil {
			return err
		}
		s.mu.Lock()
		hook := s.onEnrolled
		s.mu.Unlock()
		if hook != nil {
			if err := hook(ctx, tx, NodeEnrollment{
				NodeID: ex.NodeID, Name: name, SSHAlias: alias,
				Hostname: cleanName(ex.Hostname, 256), Platform: cleanName(ex.Platform, 32), Arch: cleanName(ex.Arch, 32),
				EnrolledAt: now,
			}); err != nil {
				return err
			}
		}
		cred = contracts.NodeCredential{NodeID: ex.NodeID, Token: tok}
		return nil
	})
	if err != nil {
		return contracts.NodeCredential{}, err
	}
	if userErr != nil {
		return contracts.NodeCredential{}, userErr
	}
	return cred, nil
}

// ---- client pairing ----

// CreatePairingTx creates a one-time pairing ticket for an owner client.
func (s *Store) CreatePairingTx(ctx context.Context, tx *sql.Tx, req contracts.PairingRequest, by contracts.Principal) (contracts.PairingTicket, error) {
	if req.Kind != "" && !ClientKinds[req.Kind] {
		return contracts.PairingTicket{}, contracts.Errorf(contracts.ErrInvalid, "client kind must be cli, android, or tui")
	}
	code, err := transport.NewSecret(transport.PrefixPairingCode)
	if err != nil {
		return contracts.PairingTicket{}, err
	}
	now := s.clock.Now()
	exp := now.Add(ttlOrDefault(req.TTL, DefaultPairingTTL))
	id := contracts.NewID(contracts.PrefixPairing)
	if _, err := tx.ExecContext(ctx, `INSERT INTO pairings(pairing_id, code_hash, client_name, kind, created_by, created_at, expires_at) VALUES(?,?,?,?,?,?,?)`,
		id, transport.HashSecret(code), cleanName(req.ClientName, 128), req.Kind, who(by), sqlitex.FormatTime(now), sqlitex.FormatTime(exp)); err != nil {
		return contracts.PairingTicket{}, err
	}
	return s.pairingTicket(id, code, exp), nil
}

func (s *Store) pairingTicket(id, code string, exp time.Time) contracts.PairingTicket {
	t := contracts.PairingTicket{PairingID: id, Code: code, HubURL: s.opts.HubURL, HubCertSHA256: s.opts.HubCertSHA256, ExpiresAt: exp}
	t.URI = PairingURI{HubURL: t.HubURL, PinSHA256: t.HubCertSHA256, PairingID: id, Code: code}.String()
	return t
}

// RotatePairingCodeTx replaces the code of an unredeemed, unexpired pairing
// (idempotent replay of clients.pair).
func (s *Store) RotatePairingCodeTx(ctx context.Context, tx *sql.Tx, pairingID string) (contracts.PairingTicket, error) {
	var exp string
	var redeemed, invalid sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT expires_at, redeemed_at, invalidated_at FROM pairings WHERE pairing_id=?`, pairingID).Scan(&exp, &redeemed, &invalid)
	if errors.Is(err, sql.ErrNoRows) {
		return contracts.PairingTicket{}, contracts.Errorf(contracts.ErrNotFound, "pairing not found")
	}
	if err != nil {
		return contracts.PairingTicket{}, err
	}
	if redeemed.Valid {
		return contracts.PairingTicket{}, contracts.Errorf(contracts.ErrConflict, "pairing already redeemed")
	}
	expAt, _ := sqlitex.ParseTime(exp)
	if invalid.Valid || !s.clock.Now().Before(expAt) {
		return contracts.PairingTicket{}, contracts.Errorf(contracts.ErrExpired, "pairing expired or invalidated; create a new one")
	}
	code, err := transport.NewSecret(transport.PrefixPairingCode)
	if err != nil {
		return contracts.PairingTicket{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE pairings SET code_hash=? WHERE pairing_id=?`, transport.HashSecret(code), pairingID); err != nil {
		return contracts.PairingTicket{}, err
	}
	return s.pairingTicket(pairingID, code, expAt), nil
}

func errBadCode() error {
	return contracts.Errorf(contracts.ErrUnauthorized, "invalid or unknown pairing code")
}

// RedeemPairing exchanges a pairing code for an owner client credential,
// exactly once.
func (s *Store) RedeemPairing(ctx context.Context, ex contracts.PairingExchange) (contracts.ClientCredential, error) {
	if ex.PairingID == "" || !strings.HasPrefix(ex.Code, transport.PrefixPairingCode) {
		transport.SecretMatches(dummyHash, ex.Code)
		return contracts.ClientCredential{}, errBadCode()
	}
	var cred contracts.ClientCredential
	var userErr error
	err := sqlitex.Tx(ctx, s.db, func(tx *sql.Tx) error {
		var hash, reqName, reqKind, exp string
		var redeemed, invalid sql.NullString
		var failed int
		err := tx.QueryRowContext(ctx, `SELECT code_hash, client_name, kind, expires_at, redeemed_at, failed_attempts, invalidated_at FROM pairings WHERE pairing_id=?`, ex.PairingID).
			Scan(&hash, &reqName, &reqKind, &exp, &redeemed, &failed, &invalid)
		if errors.Is(err, sql.ErrNoRows) {
			transport.SecretMatches(dummyHash, ex.Code)
			userErr = errBadCode()
			return nil
		}
		if err != nil {
			return err
		}
		if !transport.SecretMatches(hash, ex.Code) {
			userErr = errBadCode()
			if redeemed.Valid || invalid.Valid {
				return nil
			}
			failed++
			if failed >= MaxFailedAttempts {
				_, err = tx.ExecContext(ctx, `UPDATE pairings SET failed_attempts=?, invalidated_at=? WHERE pairing_id=?`, failed, sqlitex.FormatTime(s.clock.Now()), ex.PairingID)
			} else {
				_, err = tx.ExecContext(ctx, `UPDATE pairings SET failed_attempts=? WHERE pairing_id=?`, failed, ex.PairingID)
			}
			return err
		}
		if redeemed.Valid {
			userErr = contracts.Errorf(contracts.ErrConflict, "pairing code already used; create a new pairing")
			return nil
		}
		if invalid.Valid {
			userErr = contracts.Errorf(contracts.ErrExpired, "pairing invalidated after repeated failures; create a new pairing")
			return nil
		}
		now := s.clock.Now()
		if expAt, _ := sqlitex.ParseTime(exp); !now.Before(expAt) {
			userErr = contracts.Errorf(contracts.ErrExpired, "pairing code expired; create a new pairing")
			return nil
		}
		kind := reqKind
		if kind == "" {
			kind = ex.Kind
		}
		if !ClientKinds[kind] {
			userErr = contracts.Errorf(contracts.ErrInvalid, "client kind must be cli, android, or tui")
			return nil
		}
		name := cleanName(ex.ClientName, 128)
		if name == "" {
			name = reqName
		}
		if name == "" {
			name = kind
		}
		clientID := contracts.NewID(contracts.PrefixClient)
		res, err := tx.ExecContext(ctx, `UPDATE pairings SET redeemed_at=?, client_id=? WHERE pairing_id=? AND redeemed_at IS NULL`, sqlitex.FormatTime(now), clientID, ex.PairingID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			userErr = contracts.Errorf(contracts.ErrConflict, "pairing code already used")
			return nil
		}
		tok, err := s.createClientTx(ctx, tx, clientID, name, kind, ex.PairingID)
		if err != nil {
			return err
		}
		cred = contracts.ClientCredential{ClientID: clientID, Token: tok}
		return nil
	})
	if err != nil {
		return contracts.ClientCredential{}, err
	}
	if userErr != nil {
		return contracts.ClientCredential{}, userErr
	}
	return cred, nil
}

func (s *Store) createClientTx(ctx context.Context, tx *sql.Tx, clientID, name, kind, pairingID string) (string, error) {
	if _, err := tx.ExecContext(ctx, `INSERT INTO clients(client_id, name, kind, created_at, pairing_id) VALUES(?,?,?,?,?)`,
		clientID, name, kind, sqlitex.FormatTime(s.clock.Now()), pairingID); err != nil {
		return "", err
	}
	return s.auth.IssueTx(ctx, tx, contracts.PrincipalOwner, clientID, name)
}

// BootstrapOwnerClient creates an owner client and credential without a
// pairing. It is for local hub initialisation only (e.g. `pyre-hub init`
// writing the first owner credential file); it is not exposed over HTTP.
func (s *Store) BootstrapOwnerClient(ctx context.Context, name, kind string) (contracts.ClientCredential, error) {
	if !ClientKinds[kind] {
		return contracts.ClientCredential{}, contracts.Errorf(contracts.ErrInvalid, "client kind must be cli, android, or tui")
	}
	name = cleanName(name, 128)
	if name == "" {
		name = kind
	}
	id := contracts.NewID(contracts.PrefixClient)
	var tok string
	err := sqlitex.Tx(ctx, s.db, func(tx *sql.Tx) error {
		var err error
		tok, err = s.createClientTx(ctx, tx, id, name, kind, "")
		return err
	})
	if err != nil {
		return contracts.ClientCredential{}, err
	}
	return contracts.ClientCredential{ClientID: id, Token: tok}, nil
}

// ---- clients ----

func scanClientRow(row interface{ Scan(...any) error }) (contracts.ClientInfo, error) {
	var c contracts.ClientInfo
	var created string
	var revoked sql.NullString
	if err := row.Scan(&c.ClientID, &c.Name, &c.Kind, &created, &revoked); err != nil {
		return c, err
	}
	c.CreatedAt, _ = sqlitex.ParseTime(created)
	c.Revoked = revoked.Valid
	return c, nil
}

func (s *Store) scanClient(ctx context.Context, row interface{ Scan(...any) error }) (contracts.ClientInfo, error) {
	c, err := scanClientRow(row)
	if err != nil {
		return c, err
	}
	lu, err := s.auth.LastUsed(ctx, contracts.PrincipalOwner, c.ClientID)
	if err != nil {
		return c, err
	}
	c.LastSeenAt = lu
	return c, nil
}

// GetClient returns one client.
func (s *Store) GetClient(ctx context.Context, clientID string) (contracts.ClientInfo, error) {
	c, err := s.scanClient(ctx, s.db.QueryRowContext(ctx, `SELECT client_id, name, kind, created_at, revoked_at FROM clients WHERE client_id=?`, clientID))
	if errors.Is(err, sql.ErrNoRows) {
		return c, contracts.Errorf(contracts.ErrNotFound, "client not found").WithRef(contracts.KindClient, clientID)
	}
	return c, err
}

// ListClients pages clients in creation order.
func (s *Store) ListClients(ctx context.Context, req contracts.PageRequest) (contracts.Page[contracts.ClientInfo], error) {
	limit := clampLimit(req.Limit)
	page := contracts.Page[contracts.ClientInfo]{Items: []contracts.ClientInfo{}, SnapshotAt: s.clock.Now()}
	rs, err := s.db.QueryContext(ctx, `SELECT client_id, name, kind, created_at, revoked_at FROM clients WHERE client_id > ? ORDER BY client_id LIMIT ?`, req.Cursor, limit+1)
	if err != nil {
		return page, err
	}
	var rows []contracts.ClientInfo
	for rs.Next() {
		var c contracts.ClientInfo
		var created string
		var revoked sql.NullString
		if err := rs.Scan(&c.ClientID, &c.Name, &c.Kind, &created, &revoked); err != nil {
			rs.Close()
			return page, err
		}
		c.CreatedAt, _ = sqlitex.ParseTime(created)
		c.Revoked = revoked.Valid
		rows = append(rows, c)
	}
	rs.Close()
	if err := rs.Err(); err != nil {
		return page, err
	}
	if len(rows) > limit {
		rows = rows[:limit]
		page.NextCursor = rows[limit-1].ClientID
	}
	for i := range rows {
		lu, err := s.auth.LastUsed(ctx, contracts.PrincipalOwner, rows[i].ClientID)
		if err != nil {
			return page, err
		}
		rows[i].LastSeenAt = lu
	}
	page.Items = rows
	return page, nil
}

// RevokeClientTx marks a client revoked and revokes its credentials inside
// tx (LastSeenAt is not filled). The caller calls auth.NotifyRevoked after
// commit.
func (s *Store) RevokeClientTx(ctx context.Context, tx *sql.Tx, clientID string) (contracts.ClientInfo, error) {
	res, err := tx.ExecContext(ctx, `UPDATE clients SET revoked_at=COALESCE(revoked_at, ?) WHERE client_id=?`, sqlitex.FormatTime(s.clock.Now()), clientID)
	if err != nil {
		return contracts.ClientInfo{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return contracts.ClientInfo{}, contracts.Errorf(contracts.ErrNotFound, "client not found").WithRef(contracts.KindClient, clientID)
	}
	if _, err := s.auth.RevokePrincipalTx(ctx, tx, contracts.PrincipalOwner, clientID); err != nil {
		return contracts.ClientInfo{}, err
	}
	return scanClientRow(tx.QueryRowContext(ctx, `SELECT client_id, name, kind, created_at, revoked_at FROM clients WHERE client_id=?`, clientID))
}

func clampLimit(n int) int {
	switch {
	case n <= 0:
		return 50
	case n > 200:
		return 200
	}
	return n
}
