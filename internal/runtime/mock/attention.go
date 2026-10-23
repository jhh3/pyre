package mock

import (
	"context"
	"sort"
	"strings"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
)

// raiseAttentionLocked creates or bumps the item for groupKey.
func (b *Backend) raiseAttentionLocked(kind c.AttentionKind, severity, groupKey string, obj c.ObjectRef, sessionID string, projectID *string, title, summary string) *c.AttentionItem {
	now := b.now()
	for _, a := range b.attention {
		if a.GroupKey == groupKey && (a.State == c.AttnOpen || a.State == c.AttnRead || a.State == c.AttnSnoozed) {
			a.Count++
			a.UpdatedAt = now
			a.Object = obj
			a.Title = title
			a.Summary = summary
			a.State = c.AttnOpen
			b.emit(c.EvAttentionChanged, ref(c.KindAttention, a.AttentionID), a.SessionID, a.ProjectID, a)
			return a
		}
	}
	a := &c.AttentionItem{
		AttentionID: c.NewID(c.PrefixAttention), Kind: kind, Severity: severity, GroupKey: groupKey, Object: obj,
		SessionID: sessionID, ProjectID: cloneStrPtr(projectID), Title: title, Summary: summary, State: c.AttnOpen, Count: 1,
		CreatedAt: now, UpdatedAt: now, DeepLink: c.DeepLink(obj),
		Notification: &c.NotificationStatus{Provider: "none", State: "disabled", Detail: "mock backend sends no push"},
	}
	b.attention[a.AttentionID] = a
	b.emit(c.EvAttentionChanged, ref(c.KindAttention, a.AttentionID), sessionID, projectID, a)
	return a
}

// withdrawAttentionLocked closes pending items for groupKey.
func (b *Backend) withdrawAttentionLocked(groupKey string, state c.AttentionState) {
	now := b.now()
	for _, a := range b.attention {
		if a.GroupKey == groupKey && a.State != c.AttnWithdrawn && a.State != c.AttnHandled {
			a.State = state
			a.UpdatedAt = now
			b.emit(c.EvAttentionChanged, ref(c.KindAttention, a.AttentionID), a.SessionID, a.ProjectID, a)
		}
	}
}

// engageLocked marks the session's pending items of the given kinds handled:
// the owner engaged with the session (typed into it, messaged it, read its
// message). Approvals and questions are never passed here.
func (b *Backend) engageLocked(sessionID, summary string, kinds ...c.AttentionKind) {
	// Its chat-like mail to the owner so far is seen: unthreaded, not asking
	// to be acknowledged, not an ask's carrier (the hub's ownerEngaged).
	carriers := map[string]bool{}
	for _, a := range b.askMap() {
		carriers[a.MessageID] = true
	}
	b.ackOwnerMailLocked(c.AckRefEngaged, func(m *c.MessageView) bool {
		return m.Envelope.Sender.Kind == c.PrincipalSession && m.Envelope.Sender.ID == sessionID && !carriers[m.Envelope.MessageID] &&
			m.Envelope.ThreadID == "" && !m.Envelope.RequestAck
	})
	b.engageItemsLocked(sessionID, summary, kinds...)
}

// engageItemsLocked answers the session's items of kinds.
func (b *Backend) engageItemsLocked(sessionID, summary string, kinds ...c.AttentionKind) {
	now := b.now()
	for _, a := range b.attention {
		if a.SessionID != sessionID || !hasKind(kinds, a.Kind) {
			continue
		}
		if a.State == c.AttnOpen || a.State == c.AttnRead || a.State == c.AttnSnoozed {
			a.State, a.UpdatedAt, a.SnoozedUntil = c.AttnHandled, now, nil
			if summary != "" {
				a.Summary = summary
			}
			b.emit(c.EvAttentionChanged, ref(c.KindAttention, a.AttentionID), a.SessionID, a.ProjectID, a)
		}
	}
}

// waitingGroup is a session's "waiting on you" item group (the hub's
// attention.WaitingGroup).
func waitingGroup(sessionID string) string { return "waiting:" + sessionID }

// RaiseWaiting simulates the hub's turn-end detection: the session ended
// its turn with question for the owner in its chat (contracts.AttnWaiting).
// An active item is updated; otherwise a new one is created (a new
// question is a new conversation, as on the hub). The next turn starting,
// an ask, the run ending, archiving and the owner engaging resolve it, as
// on the hub.
func (b *Backend) RaiseWaiting(sessionID, question, summary string) (c.AttentionItem, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, ok := b.sessions[sessionID]
	if !ok {
		return c.AttentionItem{}, notFound(c.KindSession, sessionID)
	}
	q := truncate(firstLineOf(question), 100)
	if q == "" {
		q = "a question in its chat"
	}
	title, sum := "Waiting on you: "+q, truncate(strings.TrimSpace(summary), 300)
	return *b.raiseAttentionLocked(c.AttnWaiting, "normal", waitingGroup(sessionID), ref(c.KindSession, sessionID), sessionID, s.ProjectID, title, sum), nil
}

// FlagAttention raises or clears a session's own flag (v1.3).
func (b *Backend) FlagAttention(ctx context.Context, req c.FlagAttentionRequest) (c.AttentionItem, error) {
	return mutate(b, ctx, c.OpFlagAttention, req.Mutation, req, func(p c.Principal) (c.AttentionItem, error) {
		sid := req.SessionID
		switch p.Kind {
		case c.PrincipalSession:
			if sid == "" {
				sid = p.ID
			}
			if sid != p.ID {
				return c.AttentionItem{}, c.Errorf(c.ErrForbidden, "a session may only flag itself")
			}
		case c.PrincipalOwner, c.PrincipalSystem:
			if sid == "" {
				return c.AttentionItem{}, c.Errorf(c.ErrInvalid, "session_id is required")
			}
		default:
			return c.AttentionItem{}, c.Errorf(c.ErrForbidden, "%s principals may not flag sessions", p.Kind)
		}
		s, ok := b.sessions[sid]
		if !ok {
			return c.AttentionItem{}, notFound(c.KindSession, sid)
		}
		group := "flag:session:" + sid
		if req.Clear {
			var out c.AttentionItem
			for _, a := range b.attention {
				if a.GroupKey == group && (a.State == c.AttnOpen || a.State == c.AttnRead || a.State == c.AttnSnoozed) {
					a.State, a.UpdatedAt, a.Summary = c.AttnHandled, b.now(), "Cleared."
					b.emit(c.EvAttentionChanged, ref(c.KindAttention, a.AttentionID), a.SessionID, a.ProjectID, a)
					out = *a
				}
			}
			return out, nil
		}
		text := truncate(strings.TrimSpace(req.Text), 120)
		if text == "" {
			text = "Asks for you"
		}
		return *b.raiseAttentionLocked(c.AttnFlag, "urgent", group, ref(c.KindSession, sid), sid, s.ProjectID, text, ""), nil
	})
}

func (b *Backend) ListAttention(ctx context.Context, req c.ListAttentionRequest) (c.Page[c.AttentionItem], error) {
	return query(b, ctx, func(p c.Principal) (c.Page[c.AttentionItem], error) {
		b.expireApprovalsLocked()
		now := b.now()
		states := req.States
		if len(states) == 0 {
			states = []c.AttentionState{c.AttnOpen, c.AttnRead}
		}
		var out []c.AttentionItem
		for _, a := range b.attention {
			st := a.State
			if st == c.AttnSnoozed && a.SnoozedUntil != nil && !now.Before(*a.SnoozedUntil) {
				st = c.AttnOpen // snooze elapsed
			}
			if !hasState(states, st) {
				continue
			}
			if len(req.Kinds) > 0 && !hasKind(req.Kinds, a.Kind) {
				continue
			}
			if req.SessionID != "" && a.SessionID != req.SessionID {
				continue
			}
			if req.ProjectID != nil && !eqStrPtr(a.ProjectID, req.ProjectID) {
				continue
			}
			item := *a
			item.State = st
			out = append(out, item)
		}
		sev := map[string]int{"urgent": 0, "normal": 1, "info": 2}
		sort.Slice(out, func(i, j int) bool {
			if sev[out[i].Severity] != sev[out[j].Severity] {
				return sev[out[i].Severity] < sev[out[j].Severity]
			}
			if !out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
				return out[i].UpdatedAt.After(out[j].UpdatedAt)
			}
			return out[i].AttentionID < out[j].AttentionID
		})
		return paginate(out, req.PageRequest, now)
	})
}

func hasState(xs []c.AttentionState, s c.AttentionState) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func hasKind(xs []c.AttentionKind, k c.AttentionKind) bool {
	for _, x := range xs {
		if x == k {
			return true
		}
	}
	return false
}

func (b *Backend) UpdateAttention(ctx context.Context, req c.UpdateAttentionRequest) (c.AttentionItem, error) {
	return mutate(b, ctx, c.OpUpdateAttention, req.Mutation, req, func(p c.Principal) (c.AttentionItem, error) {
		if err := requireOwner(p); err != nil {
			return c.AttentionItem{}, err
		}
		a, ok := b.attention[req.AttentionID]
		if !ok {
			return c.AttentionItem{}, notFound(c.KindAttention, req.AttentionID)
		}
		switch req.State {
		case c.AttnOpen, c.AttnRead, c.AttnHandled:
			a.SnoozedUntil = nil
		case c.AttnSnoozed:
			if req.SnoozedUntil == nil || !req.SnoozedUntil.After(b.now()) {
				return c.AttentionItem{}, c.Errorf(c.ErrInvalid, "snoozed_until must be in the future")
			}
			t := req.SnoozedUntil.UTC()
			a.SnoozedUntil = &t
		default:
			return c.AttentionItem{}, c.Errorf(c.ErrInvalid, "state must be open, read, handled or snoozed")
		}
		if a.State == c.AttnWithdrawn {
			return c.AttentionItem{}, c.Errorf(c.ErrConflict, "item was withdrawn: the underlying request was resolved elsewhere").WithRef(c.KindAttention, a.AttentionID)
		}
		a.State = req.State
		a.UpdatedAt = b.now()
		b.emit(c.EvAttentionChanged, ref(c.KindAttention, a.AttentionID), a.SessionID, a.ProjectID, a)
		if a.Kind == c.AttnMessage && a.State == c.AttnHandled && a.Object.Kind == c.KindMessage {
			// Done with a message item: its message is acknowledged, as on the hub.
			b.ackOwnerMailLocked(c.AckRefEngaged, func(m *c.MessageView) bool { return m.Envelope.MessageID == a.Object.ID })
		}
		return *a, nil
	})
}

// ---- events ----

func (b *Backend) ListEvents(ctx context.Context, req c.ListEventsRequest) (c.ListEventsResponse, error) {
	if _, err := b.principal(ctx); err != nil {
		return c.ListEventsResponse{}, err
	}
	if req.AfterCursor < 0 {
		return c.ListEventsResponse{}, c.Errorf(c.ErrInvalid, "after_cursor must be >= 0")
	}
	limit := req.Limit
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	wait := time.Duration(min(max(req.WaitSeconds, 0), 60)) * time.Second
	deadline := time.Now().Add(wait)
	for {
		b.mu.Lock()
		if req.AfterCursor < b.eventFloor || req.AfterCursor > b.eventHead {
			e := c.Errorf(c.ErrResetRequired, "cursor %d is outside retained events (%d..%d); re-snapshot and resume from snapshot_cursor", req.AfterCursor, b.eventFloor, b.eventHead).
				WithDetail("oldest_cursor", b.eventFloor+1).WithDetail("snapshot_cursor", b.eventHead)
			b.mu.Unlock()
			return c.ListEventsResponse{}, e
		}
		resp := c.ListEventsResponse{Events: []c.Event{}, NextCursor: req.AfterCursor, HeadCursor: b.eventHead}
		for _, e := range b.events {
			if e.Cursor <= req.AfterCursor {
				continue
			}
			if len(resp.Events) >= limit {
				break
			}
			resp.NextCursor = e.Cursor // advance past filtered-out events too
			if len(req.Kinds) > 0 && !hasEventKind(req.Kinds, e.Kind) {
				continue
			}
			if req.SessionID != "" && e.SessionID != req.SessionID {
				continue
			}
			if req.ProjectID != nil && !eqStrPtr(e.ProjectID, req.ProjectID) {
				continue
			}
			resp.Events = append(resp.Events, e)
		}
		ch := b.changed
		b.mu.Unlock()
		if len(resp.Events) > 0 || wait <= 0 || !time.Now().Before(deadline) {
			return clone(resp), nil
		}
		if resp.NextCursor > req.AfterCursor {
			req.AfterCursor = resp.NextCursor // skip scanned non-matching events
		}
		b.waitChange(ctx, ch, time.Until(deadline))
		if ctx.Err() != nil {
			return c.ListEventsResponse{}, ctx.Err()
		}
	}
}

func hasEventKind(xs []c.EventKind, k c.EventKind) bool {
	for _, x := range xs {
		if x == k {
			return true
		}
	}
	return false
}

// ---- accounts ----

func (b *Backend) ListAccounts(ctx context.Context, req c.ListAccountsRequest) (c.Page[c.AccountView], error) {
	return query(b, ctx, func(p c.Principal) (c.Page[c.AccountView], error) {
		var out []c.AccountView
		for _, id := range sortedKeys(b.accounts) {
			a := b.accounts[id]
			if req.Provider != "" && a.Account.Provider != req.Provider {
				continue
			}
			v := *a
			if v.Grants == nil {
				v.Grants = []c.DeviceGrant{}
			}
			var active []c.AccountAssignment
			for _, as := range a.Assignments {
				if as.ReleasedAt == nil {
					active = append(active, as)
				}
			}
			if active == nil {
				active = []c.AccountAssignment{}
			}
			v.Assignments = active
			out = append(out, v)
		}
		return paginate(out, req.PageRequest, b.now())
	})
}

func (b *Backend) UpsertAccount(ctx context.Context, req c.UpsertAccountRequest) (c.Account, error) {
	return mutate(b, ctx, c.OpUpsertAccount, req.Mutation, req, func(p c.Principal) (c.Account, error) {
		if err := requireOwner(p); err != nil {
			return c.Account{}, err
		}
		if c.HarnessForProvider(req.Provider) == "" {
			return c.Account{}, c.Errorf(c.ErrInvalid, "provider must be anthropic or openai")
		}
		if req.Label == "" {
			return c.Account{}, c.Errorf(c.ErrInvalid, "label is required")
		}
		if req.AccountID == "" {
			a := &c.AccountView{Account: c.Account{AccountID: c.NewID(c.PrefixAccount), Provider: req.Provider, Label: req.Label, Kind: "subscription", Disabled: req.Disabled, CreatedAt: b.now(), RotaRef: req.RotaRef, PerMachineIndependent: req.PerMachineIndependent, ProviderRef: req.ProviderRef},
				Grants: []c.DeviceGrant{}, Assignments: []c.AccountAssignment{}}
			b.accounts[a.Account.AccountID] = a
			return a.Account, nil
		}
		a, ok := b.accounts[req.AccountID]
		if !ok {
			return c.Account{}, notFound(c.KindAccount, req.AccountID)
		}
		if a.Account.Provider != req.Provider {
			return c.Account{}, c.Errorf(c.ErrInvalid, "an account's provider cannot change")
		}
		a.Account.Label = req.Label
		a.Account.Disabled = req.Disabled
		a.Account.RotaRef = req.RotaRef
		a.Account.PerMachineIndependent = req.PerMachineIndependent
		if req.ProviderRef != "" {
			if a.Account.ProviderRef != "" && a.Account.ProviderRef != req.ProviderRef {
				return c.Account{}, c.Errorf(c.ErrConflict, "account %s is bound to another provider identity; it cannot be rebound", a.Account.AccountID)
			}
			a.Account.ProviderRef = req.ProviderRef
		}
		return a.Account, nil
	})
}

func (b *Backend) ListFallbacks(ctx context.Context, req c.ListFallbacksRequest) (c.Page[c.FallbackPlan], error) {
	return query(b, ctx, func(p c.Principal) (c.Page[c.FallbackPlan], error) {
		var out []c.FallbackPlan
		for i := len(b.fallbacks) - 1; i >= 0; i-- {
			f := b.fallbacks[i]
			if req.SessionID == "" || f.SessionID == req.SessionID {
				out = append(out, f)
			}
		}
		return paginate(out, req.PageRequest, b.now())
	})
}

func (b *Backend) assignAccountLocked(accountID string, r *c.Run, reason string) {
	a, ok := b.accounts[accountID]
	if !ok {
		return
	}
	fr := mockFreshness(b.now())
	if a.Latest != nil {
		fr = c.Freshness{ObservedAt: a.Latest.ObservedAt, Source: "mock:" + a.Latest.Source, Stale: a.Latest.Stale}
	}
	as := c.AccountAssignment{AssignmentID: c.NewID(c.PrefixAssignment), AccountID: accountID, RunID: r.RunID, SessionID: r.SessionID, NodeID: r.NodeID, Reason: reason, AssignedAt: b.now(), Freshness: fr}
	for _, g := range a.Grants {
		if g.NodeID == r.NodeID && g.Harness == r.Harness {
			as.GrantID, as.GrantGeneration = g.GrantID, g.Generation
		}
	}
	r.AssignmentID = as.AssignmentID
	a.Assignments = append(a.Assignments, as)
	b.emit(c.EvAccountAssigned, ref(c.KindAssignment, as.AssignmentID), r.SessionID, nil, as)
}

func (b *Backend) releaseAssignmentsLocked(runID string) {
	now := b.now()
	for _, a := range b.accounts {
		for i := range a.Assignments {
			if a.Assignments[i].RunID == runID && a.Assignments[i].ReleasedAt == nil {
				a.Assignments[i].ReleasedAt = &now
			}
		}
	}
}
