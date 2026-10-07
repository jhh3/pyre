package mock

import (
	"context"
	"sort"
	"strconv"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
)

func parseInt64(s string) (int64, error) { return strconv.ParseInt(s, 10, 64) }
func formatInt64(n int64) string         { return strconv.FormatInt(n, 10) }

func principalName(p c.Principal) string {
	if p.Name != "" {
		return p.Name
	}
	return string(p.Kind) + ":" + p.ID
}

// activeLease returns the run's active lease at now, if any.
func (b *Backend) activeLease(r *c.Run) *c.InputLease {
	if r.Lease == nil {
		return nil
	}
	l, ok := b.leases[r.Lease.LeaseID]
	if !ok || !l.Active(b.now()) {
		return nil
	}
	return l
}

func (b *Backend) setRunLease(r *c.Run, l *c.InputLease) {
	if l == nil {
		r.Lease = nil
		return
	}
	lc := *l
	r.Lease = &lc
}

// ---- leases ----

func (b *Backend) AcquireLease(ctx context.Context, req c.LeaseRequest) (c.InputLease, error) {
	return mutate(b, ctx, c.OpAcquireLease, req.Mutation, req, func(p c.Principal) (c.InputLease, error) {
		if err := requireNotNode(p); err != nil {
			return c.InputLease{}, err
		}
		r, ok := b.runs[req.RunID]
		if !ok {
			return c.InputLease{}, notFound(c.KindRun, req.RunID)
		}
		if r.State.Terminal() {
			return c.InputLease{}, c.Errorf(c.ErrConflict, "run %s has %s", r.RunID, r.State).WithRef(c.KindRun, r.RunID)
		}
		ttl := time.Duration(req.TTLSeconds) * time.Second
		if ttl <= 0 {
			ttl = 120 * time.Second
		}
		if ttl > time.Hour {
			ttl = time.Hour
		}
		now := b.now()
		cur := b.activeLease(r)
		var sess *string
		if s := b.sessions[r.SessionID]; s != nil {
			sess = s.ProjectID
		}
		if cur != nil && samePrincipal(cur.Holder, p) {
			cur.RenewedAt = now
			cur.ExpiresAt = now.Add(ttl)
			b.setRunLease(r, cur)
			b.emit(c.EvLeaseChanged, ref(c.KindLease, cur.LeaseID), r.SessionID, sess, cur)
			b.ownerTypedLocked(p, r.SessionID)
			return *cur, nil
		}
		nl := &c.InputLease{LeaseID: c.NewID(c.PrefixLease), RunID: r.RunID, Holder: p, AcquiredAt: now, RenewedAt: now, ExpiresAt: now.Add(ttl)}
		if cur != nil {
			if !req.Takeover {
				return c.InputLease{}, c.Errorf(c.ErrConflict, "run input is leased by %s until %s", principalName(cur.Holder), cur.ExpiresAt.UTC().Format("15:04:05Z")).
					WithRef(c.KindLease, cur.LeaseID).WithDetail("holder", cur.Holder).WithDetail("expires_at", cur.ExpiresAt)
			}
			cur.ReleasedAt = &now
			nl.TakeoverOf = cur.LeaseID
			b.emit(c.EvLeaseChanged, ref(c.KindLease, cur.LeaseID), r.SessionID, sess, cur)
		}
		b.leases[nl.LeaseID] = nl
		b.setRunLease(r, nl)
		b.emit(c.EvLeaseChanged, ref(c.KindLease, nl.LeaseID), r.SessionID, sess, nl)
		b.ownerTypedLocked(p, r.SessionID)
		return *nl, nil
	})
}

func (b *Backend) ReleaseLease(ctx context.Context, req c.LeaseReleaseRequest) (c.InputLease, error) {
	return mutate(b, ctx, c.OpReleaseLease, req.Mutation, req, func(p c.Principal) (c.InputLease, error) {
		l, ok := b.leases[req.LeaseID]
		if !ok {
			return c.InputLease{}, notFound(c.KindLease, req.LeaseID)
		}
		if !samePrincipal(l.Holder, p) && p.Kind != c.PrincipalOwner {
			return c.InputLease{}, c.Errorf(c.ErrForbidden, "only the holder or the owner may release a lease")
		}
		if l.ReleasedAt == nil {
			now := b.now()
			l.ReleasedAt = &now
			if r := b.runs[l.RunID]; r != nil {
				if r.Lease != nil && r.Lease.LeaseID == l.LeaseID {
					b.setRunLease(r, l)
				}
				var pid *string
				if s := b.sessions[r.SessionID]; s != nil {
					pid = s.ProjectID
				}
				b.emit(c.EvLeaseChanged, ref(c.KindLease, l.LeaseID), r.SessionID, pid, l)
			}
		}
		return *l, nil
	})
}

// ---- approvals ----

func (b *Backend) ListApprovals(ctx context.Context, req c.ListApprovalsRequest) (c.Page[c.Approval], error) {
	return query(b, ctx, func(p c.Principal) (c.Page[c.Approval], error) {
		b.expireApprovalsLocked()
		states := req.States
		if len(states) == 0 {
			states = []c.ApprovalState{c.ApprovalPending}
		}
		var out []c.Approval
		for _, a := range b.approvals {
			if req.SessionID != "" && a.SessionID != req.SessionID {
				continue
			}
			ok := false
			for _, st := range states {
				if a.State == st {
					ok = true
				}
			}
			if ok {
				out = append(out, *a)
			}
		}
		sort.Slice(out, func(i, j int) bool {
			if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
				return out[i].CreatedAt.After(out[j].CreatedAt)
			}
			return out[i].ApprovalID < out[j].ApprovalID
		})
		return paginate(out, req.PageRequest, b.now())
	})
}

func (b *Backend) GetApproval(ctx context.Context, req c.GetRequest) (c.Approval, error) {
	return query(b, ctx, func(p c.Principal) (c.Approval, error) {
		b.expireApprovalsLocked()
		a, ok := b.approvals[req.ID]
		if !ok {
			return c.Approval{}, notFound(c.KindApproval, req.ID)
		}
		return *a, nil
	})
}

func (b *Backend) AnswerApproval(ctx context.Context, req c.AnswerApprovalRequest) (c.Approval, error) {
	return mutate(b, ctx, c.OpAnswerApproval, req.Mutation, req, func(p c.Principal) (c.Approval, error) {
		if err := requireOwner(p); err != nil {
			return c.Approval{}, err
		}
		b.expireApprovalsLocked()
		a, ok := b.approvals[req.ApprovalID]
		if !ok {
			return c.Approval{}, notFound(c.KindApproval, req.ApprovalID)
		}
		switch a.State {
		case c.ApprovalWithdrawn:
			return c.Approval{}, c.Errorf(c.ErrExpired, "approval withdrawn: its run was replaced or moved on").WithRef(c.KindApproval, a.ApprovalID)
		case c.ApprovalExpired:
			return c.Approval{}, c.Errorf(c.ErrExpired, "approval expired").WithRef(c.KindApproval, a.ApprovalID)
		case c.ApprovalResolved:
			desc := "already resolved"
			if a.Resolution != nil {
				desc += " (" + a.Resolution.OptionID + ", " + a.Resolution.Source + ")"
			}
			return c.Approval{}, c.Errorf(c.ErrConflict, "approval %s", desc).WithRef(c.KindApproval, a.ApprovalID).WithDetail("resolution", a.Resolution)
		}
		if req.RunID != a.RunID {
			return c.Approval{}, c.Errorf(c.ErrConflict, "approval belongs to run %s, not %s", a.RunID, req.RunID).WithRef(c.KindApproval, a.ApprovalID)
		}
		if req.OptionID == "" && req.Text == "" {
			return c.Approval{}, c.Errorf(c.ErrInvalid, "option_id or text is required")
		}
		if req.OptionID != "" {
			found := false
			for _, o := range a.Options {
				if o.ID == req.OptionID {
					found = true
				}
			}
			if !found {
				return c.Approval{}, c.Errorf(c.ErrInvalid, "unknown option %q", req.OptionID)
			}
		}
		if req.Text != "" && !a.AllowText {
			return c.Approval{}, c.Errorf(c.ErrInvalid, "this request does not accept free text")
		}
		r := b.runs[a.RunID]
		if r != nil {
			if n := b.nodes[r.NodeID]; n == nil || n.Connection.State != c.ConnConnected {
				return c.Approval{}, c.Errorf(c.ErrUnavailable, "node for run %s is not connected; answer not sent", a.RunID).WithRef(c.KindNode, r.NodeID)
			}
		}
		now := b.now()
		a.State = c.ApprovalResolved
		a.Resolution = &c.ApprovalResolution{OptionID: req.OptionID, Text: req.Text, Actor: p, ResolvedAt: now, Source: "pyre", Effect: "submitted"}
		b.afterApprovalResolvedLocked(a)
		return *a, nil
	})
}

func (b *Backend) afterApprovalResolvedLocked(a *c.Approval) {
	r := b.runs[a.RunID]
	var pid *string
	if s := b.sessions[a.SessionID]; s != nil {
		pid = s.ProjectID
	}
	if r != nil {
		r.PendingApprovalIDs = removeString(r.PendingApprovalIDs, a.ApprovalID)
		if len(r.PendingApprovalIDs) == 0 && r.State == c.RunWaitingApproval {
			b.busy[r.RunID] = b.opts.BusyTicks
			b.setRunStateLocked(r, c.RunRunning, "approval answered (mock)", "hub")
		}
	}
	b.emit(c.EvApprovalResolved, ref(c.KindApproval, a.ApprovalID), a.SessionID, pid, a)
	b.withdrawAttentionLocked("approval:"+a.ApprovalID, c.AttnHandled)
}

func (b *Backend) expireApprovalsLocked() {
	now := b.now()
	for _, a := range b.approvals {
		if a.State == c.ApprovalPending && a.ExpiresAt != nil && now.After(*a.ExpiresAt) {
			a.State = c.ApprovalExpired
			if r := b.runs[a.RunID]; r != nil {
				r.PendingApprovalIDs = removeString(r.PendingApprovalIDs, a.ApprovalID)
			}
			b.emit(c.EvApprovalWithdrawn, ref(c.KindApproval, a.ApprovalID), a.SessionID, nil, a)
			b.withdrawAttentionLocked("approval:"+a.ApprovalID, c.AttnWithdrawn)
		}
	}
}

func (b *Backend) withdrawRunApprovalsLocked(r *c.Run, reason string) {
	for _, a := range b.approvals {
		if a.RunID == r.RunID && a.State == c.ApprovalPending {
			a.State = c.ApprovalWithdrawn
			b.emit(c.EvApprovalWithdrawn, ref(c.KindApproval, a.ApprovalID), a.SessionID, nil, a)
			b.withdrawAttentionLocked("approval:"+a.ApprovalID, c.AttnWithdrawn)
		}
	}
	r.PendingApprovalIDs = nil
}

func removeString(xs []string, x string) []string {
	out := xs[:0:0]
	for _, v := range xs {
		if v != x {
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ownerTypedLocked: the owner taking a run's input lease is the owner
// engaging with that session, which answers its messages and own flag.
func (b *Backend) ownerTypedLocked(p c.Principal, sessionID string) {
	if p.Kind == c.PrincipalOwner {
		b.engageLocked(sessionID, "You engaged with the session.", c.AttnMessage, c.AttnFlag)
	}
}
