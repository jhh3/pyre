package mock

import (
	"context"
	"sort"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
)

// Account refresh-family leases (contract v1.3), same rules as the hub:
// node principals; one holder at a time unless the account is
// PerMachineIndependent; same node+holder returns the lease extended.

func leaseTTL(sec int) time.Duration {
	if sec <= 0 {
		return 30 * time.Minute
	}
	return min(max(time.Duration(sec)*time.Second, time.Minute), time.Hour)
}

func requireNodePrincipal(p c.Principal) error {
	if p.Kind != c.PrincipalNode {
		return c.Errorf(c.ErrForbidden, "account leases are taken by nodes (principal %s)", p.Kind)
	}
	return nil
}

func (b *Backend) activeAcctLeasesLocked(accountID string) []*c.AccountLease {
	now := b.now()
	var out []*c.AccountLease
	for _, l := range b.acctLeases {
		if l.AccountID == accountID && l.ReleasedAt == nil && l.ExpiresAt.After(now) {
			out = append(out, l)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AcquiredAt.Before(out[j].AcquiredAt) })
	return out
}

func acctLeaseConflict(l *c.AccountLease) error {
	e := c.Errorf(c.ErrConflict, "account %s is leased by node %s until %s", l.AccountID, l.NodeID, l.ExpiresAt.UTC().Format(time.RFC3339)).
		WithRef(c.KindNode, l.NodeID).WithDetail("holder_node", l.NodeID).WithDetail("holder", l.Holder)
	e.Retryable = true
	return e
}

func (b *Backend) AcquireAccountLease(ctx context.Context, req c.AcquireAccountLeaseRequest) (c.AccountLease, error) {
	return mutate(b, ctx, c.OpAcquireAccountLease, req.Mutation, req, func(p c.Principal) (c.AccountLease, error) {
		if err := requireNodePrincipal(p); err != nil {
			return c.AccountLease{}, err
		}
		if req.AccountID == "" || req.Holder == "" {
			return c.AccountLease{}, c.Errorf(c.ErrInvalid, "account_id and holder are required")
		}
		a, ok := b.accounts[req.AccountID]
		if !ok {
			return c.AccountLease{}, notFound(c.KindAccount, req.AccountID)
		}
		now, ttl := b.now(), leaseTTL(req.TTLSeconds)
		for _, l := range b.activeAcctLeasesLocked(req.AccountID) {
			if l.NodeID == p.ID && l.Holder == req.Holder {
				if exp := now.Add(ttl); exp.After(l.ExpiresAt) {
					l.ExpiresAt = exp
				}
				return *l, nil
			}
			if !a.Account.PerMachineIndependent || l.NodeID == p.ID {
				return c.AccountLease{}, acctLeaseConflict(l)
			}
		}
		l := &c.AccountLease{LeaseID: c.NewID(c.PrefixLease), AccountID: req.AccountID, NodeID: p.ID, Holder: req.Holder, AcquiredAt: now, ExpiresAt: now.Add(ttl)}
		b.acctLeases[l.LeaseID] = l
		b.emit(c.EvAccountLease, ref(c.KindAccount, l.AccountID), "", nil, *l)
		return *l, nil
	})
}

func (b *Backend) ownAcctLeaseLocked(p c.Principal, id string) (*c.AccountLease, error) {
	l, ok := b.acctLeases[id]
	if !ok {
		return nil, notFound(c.KindLease, id)
	}
	if l.NodeID != p.ID {
		return nil, c.Errorf(c.ErrForbidden, "lease %s belongs to another node", id).WithRef(c.KindLease, id)
	}
	return l, nil
}

func (b *Backend) RenewAccountLease(ctx context.Context, req c.RenewAccountLeaseRequest) (c.AccountLease, error) {
	return mutate(b, ctx, c.OpRenewAccountLease, req.Mutation, req, func(p c.Principal) (c.AccountLease, error) {
		if err := requireNodePrincipal(p); err != nil {
			return c.AccountLease{}, err
		}
		l, err := b.ownAcctLeaseLocked(p, req.LeaseID)
		if err != nil {
			return c.AccountLease{}, err
		}
		if l.ReleasedAt != nil {
			return c.AccountLease{}, c.Errorf(c.ErrConflict, "lease %s was released; acquire a new one", l.LeaseID).WithRef(c.KindLease, l.LeaseID)
		}
		now := b.now()
		if !l.ExpiresAt.After(now) {
			indep := false
			if a, ok := b.accounts[l.AccountID]; ok {
				indep = a.Account.PerMachineIndependent
			}
			for _, o := range b.activeAcctLeasesLocked(l.AccountID) {
				if o.LeaseID != l.LeaseID && (!indep || o.NodeID == p.ID) {
					return c.AccountLease{}, acctLeaseConflict(o)
				}
			}
		}
		l.ExpiresAt = now.Add(leaseTTL(req.TTLSeconds))
		return *l, nil
	})
}

func (b *Backend) ReleaseAccountLease(ctx context.Context, req c.ReleaseAccountLeaseRequest) (c.AccountLease, error) {
	return mutate(b, ctx, c.OpReleaseAccountLease, req.Mutation, req, func(p c.Principal) (c.AccountLease, error) {
		if err := requireNodePrincipal(p); err != nil {
			return c.AccountLease{}, err
		}
		l, err := b.ownAcctLeaseLocked(p, req.LeaseID)
		if err != nil {
			return c.AccountLease{}, err
		}
		if l.ReleasedAt == nil {
			now := b.now()
			l.ReleasedAt = &now
			b.emit(c.EvAccountLease, ref(c.KindAccount, l.AccountID), "", nil, *l)
		}
		return *l, nil
	})
}
