package hub

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/persistence"
)

// Account refresh-family leases (accounts.lease.*): at most one node holds
// an account's login at a time, so a rota snapshot copied between machines
// is never refreshed on two of them. Node principals only. Leases expire on
// their own; holders renew at half-life.

const (
	defaultAccountLeaseTTL = 30 * time.Minute
	minAccountLeaseTTL     = time.Minute
	maxAccountLeaseTTL     = time.Hour
)

func accountLeaseTTL(sec int) time.Duration {
	if sec <= 0 {
		return defaultAccountLeaseTTL
	}
	return min(max(time.Duration(sec)*time.Second, minAccountLeaseTTL), maxAccountLeaseTTL)
}

func requireNode(ctx context.Context) (contracts.Principal, error) {
	p, err := principal(ctx)
	if err != nil {
		return p, err
	}
	if p.Kind != contracts.PrincipalNode || p.OnBehalfOf != nil {
		return p, contracts.Errorf(contracts.ErrForbidden, "account leases are taken by nodes (principal %s)", p.Kind)
	}
	return p, nil
}

const acctLeaseCols = `lease_id, account_id, node_id, holder, acquired_at, expires_at, released_at`

func scanAccountLease(r interface{ Scan(...any) error }) (contracts.AccountLease, error) {
	var l contracts.AccountLease
	var acq, exp string
	var rel sql.NullString
	if err := r.Scan(&l.LeaseID, &l.AccountID, &l.NodeID, &l.Holder, &acq, &exp, &rel); err != nil {
		return l, err
	}
	l.AcquiredAt, l.ExpiresAt, l.ReleasedAt = persistence.ParseTime(acq), persistence.ParseTime(exp), persistence.ParseTimePtr(rel)
	return l, nil
}

func (h *Hub) getAccountLease(ctx context.Context, q queryer, id string) (contracts.AccountLease, error) {
	l, err := scanAccountLease(q.QueryRowContext(ctx, `SELECT `+acctLeaseCols+` FROM account_leases WHERE lease_id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return l, contracts.Errorf(contracts.ErrNotFound, "account lease not found").WithRef(contracts.KindLease, id)
	}
	return l, err
}

// activeAccountLeases returns unreleased, unexpired leases of an account.
func (h *Hub) activeAccountLeases(ctx context.Context, q queryer, accountID string) ([]contracts.AccountLease, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+acctLeaseCols+` FROM account_leases WHERE account_id=? AND released_at IS NULL AND expires_at > ? ORDER BY acquired_at`,
		accountID, persistence.Time(h.now()))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []contracts.AccountLease
	for rows.Next() {
		l, err := scanAccountLease(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// leaseConflict is retryable (not stored with the operation): the other
// lease may be released or expire.
func (h *Hub) leaseConflict(ctx context.Context, q queryer, l contracts.AccountLease) error {
	who := l.NodeID
	if name := h.nodeName(ctx, q, l.NodeID, false); name != "" {
		who = name + " (" + l.NodeID + ")"
	}
	e := contracts.Errorf(contracts.ErrConflict, "account %s is leased by node %s until %s", l.AccountID, who, l.ExpiresAt.UTC().Format(time.RFC3339)).
		WithRef(contracts.KindNode, l.NodeID).WithDetail("holder_node", l.NodeID).WithDetail("holder", l.Holder).WithDetail("expires_at", l.ExpiresAt.UTC().Format(time.RFC3339))
	e.Retryable = true
	return e
}

func (h *Hub) emitLease(ctx context.Context, tx *sql.Tx, l contracts.AccountLease) error {
	return h.emitPayload(ctx, tx, contracts.EvAccountLease, contracts.ObjectRef{Kind: contracts.KindAccount, ID: l.AccountID}, "", nil, "hub", 0, l)
}

// AcquireAccountLease grants the calling node an account's lease. Another
// holder's unexpired lease is a conflict naming that node, unless the
// account is PerMachineIndependent (then leases do not exclude each other).
// The same node and holder get their existing lease back, extended.
func (h *Hub) AcquireAccountLease(ctx context.Context, req contracts.AcquireAccountLeaseRequest) (contracts.AccountLease, error) {
	p, err := requireNode(ctx)
	if err != nil {
		return contracts.AccountLease{}, err
	}
	if err := requireOp(req.Mutation); err != nil {
		return contracts.AccountLease{}, err
	}
	if req.AccountID == "" || req.Holder == "" || len(req.Holder) > 256 {
		return contracts.AccountLease{}, contracts.Errorf(contracts.ErrInvalid, "account_id and a holder (<=256 bytes) are required")
	}
	ttl := accountLeaseTTL(req.TTLSeconds)
	var out contracts.AccountLease
	err = h.mutate(ctx, req.OperationID, contracts.OpAcquireAccountLease, p, req, &out, func(tx *sql.Tx) (any, error) {
		acct, err := h.getAccount(ctx, tx, req.AccountID)
		if err != nil {
			return nil, err
		}
		active, err := h.activeAccountLeases(ctx, tx, req.AccountID)
		if err != nil {
			return nil, err
		}
		now := h.now()
		for _, l := range active {
			if l.NodeID == p.ID && l.Holder == req.Holder {
				if exp := now.Add(ttl); exp.After(l.ExpiresAt) {
					l.ExpiresAt = exp
					if _, err := tx.ExecContext(ctx, `UPDATE account_leases SET expires_at=? WHERE lease_id=?`, persistence.Time(exp), l.LeaseID); err != nil {
						return nil, err
					}
				}
				return l, nil
			}
			if !acct.PerMachineIndependent || l.NodeID == p.ID {
				return nil, h.leaseConflict(ctx, tx, l)
			}
		}
		l := contracts.AccountLease{LeaseID: contracts.NewID(contracts.PrefixLease), AccountID: req.AccountID, NodeID: p.ID, Holder: req.Holder,
			AcquiredAt: now, ExpiresAt: now.Add(ttl)}
		if _, err := tx.ExecContext(ctx, `INSERT INTO account_leases(`+acctLeaseCols+`) VALUES(?,?,?,?,?,?,NULL)`,
			l.LeaseID, l.AccountID, l.NodeID, l.Holder, persistence.Time(l.AcquiredAt), persistence.Time(l.ExpiresAt)); err != nil {
			return nil, err
		}
		return l, h.emitLease(ctx, tx, l)
	})
	return out, err
}

// RenewAccountLease extends the calling node's lease. An expired lease is
// revived only while nobody else holds the account.
func (h *Hub) RenewAccountLease(ctx context.Context, req contracts.RenewAccountLeaseRequest) (contracts.AccountLease, error) {
	p, err := requireNode(ctx)
	if err != nil {
		return contracts.AccountLease{}, err
	}
	if err := requireOp(req.Mutation); err != nil {
		return contracts.AccountLease{}, err
	}
	ttl := accountLeaseTTL(req.TTLSeconds)
	var out contracts.AccountLease
	err = h.mutate(ctx, req.OperationID, contracts.OpRenewAccountLease, p, req, &out, func(tx *sql.Tx) (any, error) {
		l, err := h.getAccountLease(ctx, tx, req.LeaseID)
		if err != nil {
			return nil, err
		}
		if l.NodeID != p.ID {
			return nil, contracts.Errorf(contracts.ErrForbidden, "lease %s belongs to another node", l.LeaseID).WithRef(contracts.KindLease, l.LeaseID)
		}
		if l.ReleasedAt != nil {
			return nil, contracts.Errorf(contracts.ErrConflict, "lease %s was released; acquire a new one", l.LeaseID).WithRef(contracts.KindLease, l.LeaseID)
		}
		now := h.now()
		if !l.ExpiresAt.After(now) {
			acct, err := h.getAccount(ctx, tx, l.AccountID)
			if err != nil {
				return nil, err
			}
			active, err := h.activeAccountLeases(ctx, tx, l.AccountID)
			if err != nil {
				return nil, err
			}
			for _, o := range active {
				if o.LeaseID != l.LeaseID && (!acct.PerMachineIndependent || o.NodeID == p.ID) {
					return nil, h.leaseConflict(ctx, tx, o)
				}
			}
		}
		l.ExpiresAt = now.Add(ttl)
		if _, err := tx.ExecContext(ctx, `UPDATE account_leases SET expires_at=? WHERE lease_id=?`, persistence.Time(l.ExpiresAt), l.LeaseID); err != nil {
			return nil, err
		}
		return l, nil
	})
	return out, err
}

// ReleaseAccountLease ends the calling node's lease (idempotent).
func (h *Hub) ReleaseAccountLease(ctx context.Context, req contracts.ReleaseAccountLeaseRequest) (contracts.AccountLease, error) {
	p, err := requireNode(ctx)
	if err != nil {
		return contracts.AccountLease{}, err
	}
	if err := requireOp(req.Mutation); err != nil {
		return contracts.AccountLease{}, err
	}
	var out contracts.AccountLease
	err = h.mutate(ctx, req.OperationID, contracts.OpReleaseAccountLease, p, req, &out, func(tx *sql.Tx) (any, error) {
		l, err := h.getAccountLease(ctx, tx, req.LeaseID)
		if err != nil {
			return nil, err
		}
		if l.NodeID != p.ID {
			return nil, contracts.Errorf(contracts.ErrForbidden, "lease %s belongs to another node", l.LeaseID).WithRef(contracts.KindLease, l.LeaseID)
		}
		if l.ReleasedAt != nil {
			return l, nil
		}
		now := h.now()
		l.ReleasedAt = &now
		if _, err := tx.ExecContext(ctx, `UPDATE account_leases SET released_at=? WHERE lease_id=?`, persistence.Time(now), l.LeaseID); err != nil {
			return nil, err
		}
		return l, h.emitLease(ctx, tx, l)
	})
	return out, err
}
