package enrollment

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

// PendingTicket is an enrollment ticket that is still redeemable: not
// redeemed, not invalidated, not expired. The fleet lists it as a node in
// state "never" so the owner sees nodes they enrolled but have not set up.
type PendingTicket struct {
	NodeID    string
	Name      string
	SSHAlias  string
	CreatedAt time.Time
	ExpiresAt time.Time
}

const pendingWhere = `redeemed_at IS NULL AND invalidated_at IS NULL AND expires_at > ?`

// RowQuerier is *sql.DB or *sql.Tx.
type RowQuerier interface {
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
}

func scanPending(sc interface{ Scan(...any) error }) (PendingTicket, error) {
	var p PendingTicket
	var created, exp string
	if err := sc.Scan(&p.NodeID, &p.Name, &p.SSHAlias, &created, &exp); err != nil {
		return p, err
	}
	p.CreatedAt, _ = sqlitex.ParseTime(created)
	p.ExpiresAt, _ = sqlitex.ParseTime(exp)
	return p, nil
}

// PendingTickets returns up to limit redeemable tickets with node_id >
// after, in node_id order.
func (s *Store) PendingTickets(ctx context.Context, after string, limit int) ([]PendingTicket, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT node_id, name, ssh_alias, created_at, expires_at FROM enroll_tickets WHERE `+pendingWhere+` AND node_id > ? ORDER BY node_id LIMIT ?`,
		sqlitex.FormatTime(s.clock.Now()), after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingTicket
	for rows.Next() {
		p, err := scanPending(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// PendingTicket returns the redeemable ticket for nodeID, if any, read
// through q (pass the caller's transaction when inside one).
func (s *Store) PendingTicket(ctx context.Context, q RowQuerier, nodeID string) (PendingTicket, bool, error) {
	p, err := scanPending(q.QueryRowContext(ctx, `SELECT node_id, name, ssh_alias, created_at, expires_at FROM enroll_tickets WHERE `+pendingWhere+` AND node_id = ?`,
		sqlitex.FormatTime(s.clock.Now()), nodeID))
	if errors.Is(err, sql.ErrNoRows) {
		return p, false, nil
	}
	return p, err == nil, err
}

// InvalidateTicketTx invalidates a redeemable ticket (the owner revoked a
// node before it enrolled). It reports whether a ticket was invalidated.
func (s *Store) InvalidateTicketTx(ctx context.Context, tx *sql.Tx, nodeID string) (bool, error) {
	now := sqlitex.FormatTime(s.clock.Now())
	res, err := tx.ExecContext(ctx, `UPDATE enroll_tickets SET invalidated_at=? WHERE node_id=? AND `+pendingWhere, now, nodeID, now)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}
