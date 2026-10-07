package transport

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

// AuthMigrations are the "auth" namespace migrations. NewAuthStore applies
// them; they are exported so a composing binary can migrate up front.
var AuthMigrations = []sqlitex.Migration{{
	Namespace: "auth", Version: 1, Name: "tokens_and_tickets", SQL: `
CREATE TABLE auth_tokens (
	token_hash   TEXT PRIMARY KEY,
	kind         TEXT NOT NULL,
	principal_id TEXT NOT NULL,
	name         TEXT NOT NULL DEFAULT '',
	created_at   TEXT NOT NULL,
	last_used_at TEXT,
	revoked_at   TEXT
);
CREATE INDEX auth_tokens_principal ON auth_tokens(kind, principal_id);
CREATE TABLE auth_tickets (
	ticket_hash TEXT PRIMARY KEY,
	purpose     TEXT NOT NULL,
	ref_kind    TEXT NOT NULL,
	ref_id      TEXT NOT NULL,
	created_at  TEXT NOT NULL,
	expires_at  TEXT NOT NULL,
	redeemed_at TEXT
);
CREATE INDEX auth_tickets_expiry ON auth_tickets(expires_at);
`}}

// Ticket TTL bounds.
const (
	DefaultTicketTTL = 60 * time.Second
	MaxTicketTTL     = 24 * time.Hour
)

// AuthStore persists credentials (owner, node, and session tokens) and
// one-time tickets as SHA-256 hashes. It implements contracts.Authenticator
// and contracts.TokenIssuer.
type AuthStore struct {
	db    *sql.DB
	clock contracts.Clock
	// touchEvery throttles last_used_at writes from Authenticate.
	touchEvery time.Duration

	mu        sync.Mutex
	listeners []func(contracts.Principal)
}

var (
	_ contracts.Authenticator = (*AuthStore)(nil)
	_ contracts.TokenIssuer   = (*AuthStore)(nil)
)

// NewAuthStore migrates the "auth" namespace in db and returns the store.
// A nil clock uses contracts.SystemClock.
func NewAuthStore(db *sql.DB, clock contracts.Clock) (*AuthStore, error) {
	if clock == nil {
		clock = contracts.SystemClock{}
	}
	if err := sqlitex.Migrate(context.Background(), db, AuthMigrations...); err != nil {
		return nil, err
	}
	return &AuthStore{db: db, clock: clock, touchEvery: time.Minute}, nil
}

func errUnauthorized(msg string) error { return contracts.Errorf(contracts.ErrUnauthorized, "%s", msg) }

// Authenticate resolves a bearer token to its Principal. Unknown, malformed,
// and revoked tokens yield ErrUnauthorized. The token value never appears in
// the returned error.
func (a *AuthStore) Authenticate(ctx context.Context, bearer string) (contracts.Principal, error) {
	kind, ok := tokenKind(bearer)
	if !ok {
		return contracts.Principal{}, errUnauthorized("invalid credential")
	}
	h := HashSecret(bearer)
	var storedHash, storedKind, id, name string
	var lastUsed, revoked sql.NullString
	err := a.db.QueryRowContext(ctx, `SELECT token_hash, kind, principal_id, name, last_used_at, revoked_at FROM auth_tokens WHERE token_hash=?`, h).
		Scan(&storedHash, &storedKind, &id, &name, &lastUsed, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return contracts.Principal{}, errUnauthorized("invalid credential")
	}
	if err != nil {
		return contracts.Principal{}, contracts.Errorf(contracts.ErrUnavailable, "credential store unavailable")
	}
	if !SecretMatches(storedHash, bearer) || storedKind != string(kind) {
		return contracts.Principal{}, errUnauthorized("invalid credential")
	}
	if revoked.Valid {
		return contracts.Principal{}, errUnauthorized("credential revoked")
	}
	now := a.clock.Now()
	if t, _ := sqlitex.ParseTime(lastUsed.String); !lastUsed.Valid || now.Sub(t) >= a.touchEvery {
		// Best effort; failure to record last use never fails authentication.
		_, _ = a.db.ExecContext(ctx, `UPDATE auth_tokens SET last_used_at=? WHERE token_hash=?`, sqlitex.FormatTime(now), h)
	}
	return contracts.Principal{Kind: kind, ID: id, Name: name}, nil
}

// IssueTx mints a credential for (kind, id) inside tx and returns the
// plaintext once. Only the hash is stored.
func (a *AuthStore) IssueTx(ctx context.Context, tx *sql.Tx, kind contracts.PrincipalKind, id, name string) (string, error) {
	prefix, ok := prefixFor(kind)
	if !ok {
		return "", contracts.Errorf(contracts.ErrInvalid, "cannot issue a credential for principal kind %q", kind)
	}
	if strings.TrimSpace(id) == "" {
		return "", contracts.Errorf(contracts.ErrInvalid, "principal id is required")
	}
	tok, err := NewSecret(prefix)
	if err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO auth_tokens(token_hash, kind, principal_id, name, created_at) VALUES(?,?,?,?,?)`,
		HashSecret(tok), string(kind), id, name, sqlitex.FormatTime(a.clock.Now())); err != nil {
		return "", err
	}
	return tok, nil
}

// Issue mints a credential for (kind, id) in its own transaction.
func (a *AuthStore) Issue(ctx context.Context, kind contracts.PrincipalKind, id, name string) (string, error) {
	var tok string
	err := sqlitex.Tx(ctx, a.db, func(tx *sql.Tx) error {
		var err error
		tok, err = a.IssueTx(ctx, tx, kind, id, name)
		return err
	})
	return tok, err
}

// IssueSessionToken mints a credential identifying an agent session.
func (a *AuthStore) IssueSessionToken(ctx context.Context, sessionID string) (string, error) {
	return a.Issue(ctx, contracts.PrincipalSession, sessionID, "")
}

// RevokeSessionTokens revokes every credential of a session.
func (a *AuthStore) RevokeSessionTokens(ctx context.Context, sessionID string) error {
	_, err := a.RevokePrincipal(ctx, contracts.PrincipalSession, sessionID)
	return err
}

// RevokePrincipalTx revokes every live credential of (kind, id) inside tx and
// returns how many were revoked. The caller must call NotifyRevoked after the
// transaction commits so live connections are closed.
func (a *AuthStore) RevokePrincipalTx(ctx context.Context, tx *sql.Tx, kind contracts.PrincipalKind, id string) (int64, error) {
	res, err := tx.ExecContext(ctx, `UPDATE auth_tokens SET revoked_at=? WHERE kind=? AND principal_id=? AND revoked_at IS NULL`,
		sqlitex.FormatTime(a.clock.Now()), string(kind), id)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// RevokePrincipal revokes every credential of (kind, id) and notifies
// revocation listeners after commit.
func (a *AuthStore) RevokePrincipal(ctx context.Context, kind contracts.PrincipalKind, id string) (int64, error) {
	var n int64
	err := sqlitex.Tx(ctx, a.db, func(tx *sql.Tx) error {
		var err error
		n, err = a.RevokePrincipalTx(ctx, tx, kind, id)
		return err
	})
	if err != nil {
		return 0, err
	}
	a.NotifyRevoked(contracts.Principal{Kind: kind, ID: id})
	return n, nil
}

// OnRevoke registers fn to be called (synchronously, after commit) whenever a
// principal's credentials are revoked. The hub wires NodeServer.Disconnect
// here so revocation closes live node connections immediately.
func (a *AuthStore) OnRevoke(fn func(contracts.Principal)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.listeners = append(a.listeners, fn)
}

// NotifyRevoked invokes revocation listeners for p.
func (a *AuthStore) NotifyRevoked(p contracts.Principal) {
	a.mu.Lock()
	ls := make([]func(contracts.Principal), len(a.listeners))
	copy(ls, a.listeners)
	a.mu.Unlock()
	for _, fn := range ls {
		fn(p)
	}
}

// LastUsed returns the most recent use of any credential of (kind, id).
func (a *AuthStore) LastUsed(ctx context.Context, kind contracts.PrincipalKind, id string) (*time.Time, error) {
	var s sql.NullString
	if err := a.db.QueryRowContext(ctx, `SELECT MAX(last_used_at) FROM auth_tokens WHERE kind=? AND principal_id=?`, string(kind), id).Scan(&s); err != nil {
		return nil, err
	}
	if !s.Valid || s.String == "" {
		return nil, nil
	}
	t, err := sqlitex.ParseTime(s.String)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// IssueTicket mints a one-time ticket bound to purpose and ref. ttl <= 0
// uses DefaultTicketTTL; ttl is capped at MaxTicketTTL.
func (a *AuthStore) IssueTicket(ctx context.Context, purpose string, ref contracts.ObjectRef, ttl time.Duration) (string, time.Time, error) {
	if purpose == "" {
		return "", time.Time{}, contracts.Errorf(contracts.ErrInvalid, "ticket purpose is required")
	}
	if ttl <= 0 {
		ttl = DefaultTicketTTL
	}
	if ttl > MaxTicketTTL {
		ttl = MaxTicketTTL
	}
	tok, err := NewSecret(PrefixTicket)
	if err != nil {
		return "", time.Time{}, err
	}
	now := a.clock.Now()
	exp := now.Add(ttl)
	err = sqlitex.Tx(ctx, a.db, func(tx *sql.Tx) error {
		// Opportunistic cleanup keeps the table bounded.
		if _, err := tx.ExecContext(ctx, `DELETE FROM auth_tickets WHERE expires_at < ?`, sqlitex.FormatTime(now.Add(-time.Hour))); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO auth_tickets(ticket_hash, purpose, ref_kind, ref_id, created_at, expires_at) VALUES(?,?,?,?,?,?)`,
			HashSecret(tok), purpose, string(ref.Kind), ref.ID, sqlitex.FormatTime(now), sqlitex.FormatTime(exp))
		return err
	})
	if err != nil {
		return "", time.Time{}, err
	}
	return tok, exp, nil
}

// RedeemTicket consumes a ticket exactly once. Unknown tickets and tickets
// presented for a different purpose yield ErrUnauthorized (and are not
// consumed); expired tickets ErrExpired; already-redeemed tickets
// ErrConflict.
func (a *AuthStore) RedeemTicket(ctx context.Context, ticket, purpose string) (contracts.ObjectRef, error) {
	if !strings.HasPrefix(ticket, PrefixTicket) {
		return contracts.ObjectRef{}, errUnauthorized("invalid ticket")
	}
	h := HashSecret(ticket)
	var ref contracts.ObjectRef
	err := sqlitex.Tx(ctx, a.db, func(tx *sql.Tx) error {
		var storedHash, gotPurpose, kind, id, exp string
		var redeemed sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT ticket_hash, purpose, ref_kind, ref_id, expires_at, redeemed_at FROM auth_tickets WHERE ticket_hash=?`, h).
			Scan(&storedHash, &gotPurpose, &kind, &id, &exp, &redeemed)
		if errors.Is(err, sql.ErrNoRows) {
			return errUnauthorized("invalid ticket")
		}
		if err != nil {
			return err
		}
		if !SecretMatches(storedHash, ticket) || gotPurpose != purpose {
			return errUnauthorized("invalid ticket")
		}
		if redeemed.Valid {
			return contracts.Errorf(contracts.ErrConflict, "ticket already redeemed")
		}
		expAt, err := sqlitex.ParseTime(exp)
		if err != nil {
			return err
		}
		now := a.clock.Now()
		if !now.Before(expAt) {
			return contracts.Errorf(contracts.ErrExpired, "ticket expired")
		}
		res, err := tx.ExecContext(ctx, `UPDATE auth_tickets SET redeemed_at=? WHERE ticket_hash=? AND redeemed_at IS NULL`, sqlitex.FormatTime(now), h)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return contracts.Errorf(contracts.ErrConflict, "ticket already redeemed")
		}
		ref = contracts.ObjectRef{Kind: contracts.ObjectKind(kind), ID: id}
		return nil
	})
	return ref, err
}
