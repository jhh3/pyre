package mock

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
)

// ---- clocks ----

// FakeClock is a manually advanced clock for deterministic tests.
type FakeClock struct {
	mu  sync.Mutex
	now time.Time
}

// NewFakeClock returns a clock fixed at t until advanced.
func NewFakeClock(t time.Time) *FakeClock { return &FakeClock{now: t.UTC()} }

func (f *FakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// Advance moves the clock forward.
func (f *FakeClock) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

// Set moves the clock to t.
func (f *FakeClock) Set(t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = t.UTC()
}

// OffsetClock runs in real time from a chosen epoch, so fixture ages
// ("last seen 7m ago") look right in UIs.
type OffsetClock struct {
	epoch time.Time
	start time.Time
}

// NewOffsetClock returns a clock reading epoch now and advancing in real time.
func NewOffsetClock(epoch time.Time) *OffsetClock {
	return &OffsetClock{epoch: epoch.UTC(), start: time.Now()}
}

func (o *OffsetClock) Now() time.Time { return o.epoch.Add(time.Since(o.start)).UTC() }

// ---- scheduler ----

// Tick advances the simulation one step: expirations, pending run commands,
// busy countdowns, and one delivery step per message (queued →
// node_received → attempting → accepted/rejected/indeterminate). It returns
// whether anything changed.
func (b *Backend) Tick() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.tickLocked()
}

// Settle ticks until nothing changes (at most maxTicks; <=0 means 100) and
// returns the number of ticks that changed state.
func (b *Backend) Settle(maxTicks int) int {
	if maxTicks <= 0 {
		maxTicks = 100
	}
	n := 0
	for i := 0; i < maxTicks; i++ {
		if !b.Tick() {
			break
		}
		n++
	}
	return n
}

// Run ticks every interval until ctx ends (for interactive mock hubs).
func (b *Backend) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			b.Tick()
		}
	}
}

func (b *Backend) tickLocked() bool {
	b.tickCount++
	head := b.eventHead
	changed := false
	now := b.now()

	b.expireApprovalsLocked()
	for _, id := range sortedKeys(b.messages) {
		m := b.messages[id]
		if m.Envelope.ExpiresAt != nil && now.After(*m.Envelope.ExpiresAt) &&
			(m.Status.Runtime == c.DeliveryQueued || m.Status.Runtime == c.DeliveryNodeReceived) && m.Status.Storage == c.StorageStored {
			b.setDeliveryLocked(m, c.DeliveryExpired, "retention ended before delivery; never attempted", nil)
		}
	}

	// Approval effects: a submitted answer becomes effective on the next tick.
	for _, id := range sortedKeys(b.approvals) {
		a := b.approvals[id]
		if a.State == c.ApprovalResolved && a.Resolution != nil && a.Resolution.Effect == "submitted" {
			a.Resolution.Effect = "effective"
			b.emitFrom("node:"+b.nodeOfRun(a.RunID), c.EvApprovalResolved, ref(c.KindApproval, a.ApprovalID), a.SessionID, nil, a)
		}
	}

	// Pending run commands and busy countdowns.
	for _, id := range sortedKeys(b.runs) {
		r := b.runs[id]
		if r.State.Terminal() || !b.nodeConnected(r.NodeID) {
			continue
		}
		origin := "node:" + r.NodeID
		switch b.pendingCmd[id] {
		case "start":
			delete(b.pendingCmd, id)
			if b.busy[id] > 0 {
				b.setRunStateLocked(r, c.RunRunning, "working on initial prompt (mock)", origin)
			} else {
				b.setRunStateLocked(r, c.RunWaitingInput, "idle at prompt (mock)", origin)
			}
			continue
		case "interrupt":
			delete(b.pendingCmd, id)
			delete(b.busy, id)
			b.setRunStateLocked(r, c.RunWaitingInput, "interrupted (mock)", origin)
			continue
		case "stop":
			delete(b.pendingCmd, id)
			b.setRunStateLocked(r, c.RunExited, "stopped (mock)", origin)
			continue
		}
		if r.State == c.RunRunning {
			if left, ok := b.busy[id]; ok {
				if left <= 1 {
					delete(b.busy, id)
					b.appendTranscript(id, "assistant", "Done (synthetic mock turn).", "")
					b.setRunStateLocked(r, c.RunWaitingInput, "turn finished (mock)", origin)
				} else {
					b.busy[id] = left - 1
				}
				changed = true
			}
		}
	}

	if b.deliveryStepLocked() {
		changed = true
	}
	return changed || b.eventHead != head
}

func (b *Backend) nodeOfRun(runID string) string {
	if r := b.runs[runID]; r != nil {
		return r.NodeID
	}
	return "unknown"
}

func (b *Backend) nodeConnected(nodeID string) bool {
	n := b.nodes[nodeID]
	return n != nil && n.Connection.State == c.ConnConnected
}

// deliveryStepLocked moves each in-flight message one step, processing later
// stages first so a message advances at most one stage per tick.
func (b *Backend) deliveryStepLocked() bool {
	var inflight []*c.MessageView
	for _, m := range b.messages {
		if m.Status.Storage != c.StorageStored || m.Envelope.Recipient.Kind != c.AddrSession {
			continue
		}
		switch m.Status.Runtime {
		case c.DeliveryQueued, c.DeliveryNodeReceived, c.DeliveryAttempting:
			inflight = append(inflight, m)
		}
	}
	sort.Slice(inflight, func(i, j int) bool { return inflight[i].Cursor < inflight[j].Cursor })
	changed := false
	now := b.now()

	// 1. attempting -> outcome
	for _, m := range inflight {
		if m.Status.Runtime != c.DeliveryAttempting {
			continue
		}
		changed = true
		r := b.runs[m.Status.BoundRunID]
		att := lastAttempt(m)
		rc := c.DeliveryReceipt{MessageID: m.Envelope.MessageID, AttemptID: att, SessionID: recipientSession(m), RunID: m.Status.BoundRunID, ObservedAt: now, OriginSeq: uint64(b.tickCount)}
		if r != nil {
			rc.NodeID = r.NodeID
		}
		text := m.Envelope.Body.Text
		switch {
		case r == nil || r.State.Terminal() || !b.nodeConnected(r.NodeID):
			rc.State, rc.Evidence, rc.Detail = c.DeliveryIndeterminate, c.EvidenceNone, "lost contact with the run during the attempt (mock)"
			b.setDeliveryLocked(m, c.DeliveryIndeterminate, rc.Detail, &rc)
			b.raiseIndeterminateLocked(m)
		case strings.Contains(text, MarkerIndeterminate):
			rc.State, rc.Evidence, rc.Detail = c.DeliveryIndeterminate, c.EvidenceNone, "simulated crash between submit and receipt (mock)"
			b.setDeliveryLocked(m, c.DeliveryIndeterminate, "node restarted after submitting input and before recording a receipt; no evidence found (mock)", &rc)
			b.raiseIndeterminateLocked(m)
		case strings.Contains(text, MarkerReject):
			rc.State, rc.Evidence, rc.Detail = c.DeliveryRejected, c.EvidenceNone, "simulated runtime refusal (mock)"
			b.setDeliveryLocked(m, c.DeliveryRejected, rc.Detail, &rc)
		default:
			rc.State, rc.Evidence, rc.EvidenceRef, rc.Detail = c.DeliveryAccepted, c.EvidenceMock, "mock:"+att, "mock acceptance; not runtime validation"
			t := now
			m.Status.ConsumedAt = &t
			b.appendTranscript(r.RunID, "pyre_message", c.MessageMarker(m.Envelope.MessageID)+" "+truncate(text, 4000), m.Envelope.MessageID)
			b.setDeliveryLocked(m, c.DeliveryAccepted, "accepted by mock runtime", &rc)
			b.busy[r.RunID] = b.opts.BusyTicks
			b.setRunStateLocked(r, c.RunRunning, "working on delivered message (mock)", "node:"+r.NodeID)
		}
	}

	// 2. node_received -> attempting (serialized per run, hub order)
	claimed := map[string]bool{}
	for _, m := range inflight {
		if m.Status.Runtime == c.DeliveryAttempting {
			claimed[m.Status.BoundRunID] = true
		}
	}
	for _, m := range inflight {
		if m.Status.Runtime != c.DeliveryNodeReceived {
			continue
		}
		sid := recipientSession(m)
		r := b.currentRun(sid)
		if m.Status.BoundRunID != "" && (r == nil || r.RunID != m.Status.BoundRunID) {
			continue // never rebind
		}
		if r == nil || claimed[r.RunID] || claimed["session:"+sid] {
			claimed["session:"+sid] = true
			continue
		}
		ready := r.State == c.RunWaitingInput || r.State == c.RunWaitingBackground ||
			(m.Envelope.Mode == c.ModeSteer && r.State == c.RunRunning)
		if !ready || !b.nodeConnected(r.NodeID) {
			b.updateWaitReason(m)
			claimed["session:"+sid] = true // preserve order behind this message
			continue
		}
		claimed[r.RunID] = true
		claimed["session:"+sid] = true
		changed = true
		m.Status.BoundRunID = r.RunID
		rc := c.DeliveryReceipt{MessageID: m.Envelope.MessageID, AttemptID: c.NewID(c.PrefixAttempt), SessionID: sid, RunID: r.RunID, NodeID: r.NodeID,
			State: c.DeliveryAttempting, Evidence: c.EvidenceNone, Detail: "attempt claimed (mock)", ObservedAt: now, OriginSeq: uint64(b.tickCount)}
		b.setDeliveryLocked(m, c.DeliveryAttempting, "attempt claimed and bound to "+r.RunID, &rc)
	}

	// 3. queued -> node_received
	for _, m := range inflight {
		if m.Status.Runtime != c.DeliveryQueued {
			continue
		}
		r := b.currentRun(recipientSession(m))
		if r == nil || r.State.Terminal() || !b.nodeConnected(r.NodeID) {
			b.updateWaitReason(m)
			continue
		}
		changed = true
		b.setDeliveryLocked(m, c.DeliveryNodeReceived, "recipient node committed it to its inbox (mock)", nil)
	}
	return changed
}

func (b *Backend) updateWaitReason(m *c.MessageView) {
	reason := b.waitReason(recipientSession(m), m.Envelope.Mode)
	if m.Status.Runtime == c.DeliveryNodeReceived {
		reason = "on recipient node; " + reason
	}
	if reason != m.Status.RuntimeReason {
		m.Status.RuntimeReason = reason
		m.Status.UpdatedAt = b.now()
	}
}

func (b *Backend) raiseIndeterminateLocked(m *c.MessageView) {
	b.raiseAttentionLocked(c.AttnIndeterminate, "urgent", "indeterminate:"+m.Envelope.MessageID, ref(c.KindMessage, m.Envelope.MessageID),
		recipientSession(m), m.Envelope.ProjectID, "Delivery outcome unknown", "Inspect evidence before reissuing.")
}

// ---- simulated node reports (test and demo controls) ----

// SetNodeHarnesses replaces what a node reports about its harnesses (test
// and demo control: e.g. a machine with agy and its model choices).
func (b *Backend) SetNodeHarnesses(nodeID string, hs []c.HarnessInfo) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	n, ok := b.nodes[nodeID]
	if !ok {
		return notFound(c.KindNode, nodeID)
	}
	n.Harnesses = append([]c.HarnessInfo(nil), hs...)
	return nil
}

// SetNodeConnected simulates a node's control connection dropping or
// returning. Disconnect marks its runs unknown/stale (never failed or idle)
// and turns in-flight attempts indeterminate on the next tick.
func (b *Backend) SetNodeConnected(nodeID string, connected bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	n, ok := b.nodes[nodeID]
	if !ok {
		return notFound(c.KindNode, nodeID)
	}
	if n.Connection.State == c.ConnRevoked {
		return c.Errorf(c.ErrForbidden, "node is revoked")
	}
	now := b.now()
	if connected {
		if n.Connection.State == c.ConnConnected {
			return nil
		}
		n.Connection = c.NodeConnection{State: c.ConnConnected, LastSeenAt: &now, ConnectedSince: &now, Protocol: c.ProtocolVersion}
		b.emitFrom("node:"+nodeID, c.EvNodeConnection, ref(c.KindNode, nodeID), "", nil, n.Connection)
		for _, r := range b.runs {
			if r.NodeID == nodeID && r.State == c.RunUnknown {
				st := b.lastState[r.RunID]
				if st == "" {
					st = c.RunWaitingInput
				}
				b.setRunStateLocked(r, st, "node reconnected; observation refreshed (mock)", "node:"+nodeID)
			}
		}
		return nil
	}
	if n.Connection.State != c.ConnConnected {
		return nil
	}
	n.Connection = c.NodeConnection{State: c.ConnDisconnected, LastSeenAt: &now, Protocol: n.Connection.Protocol}
	b.emit(c.EvNodeConnection, ref(c.KindNode, nodeID), "", nil, n.Connection)
	b.markNodeRunsStale(nodeID, "node disconnected")
	return nil
}

// SetRunState simulates a node observation of a run (e.g. busy, idle,
// exited). Terminal states end the run and withdraw its approvals.
func (b *Backend) SetRunState(runID string, st c.RunState, reason string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	r, ok := b.runs[runID]
	if !ok {
		return notFound(c.KindRun, runID)
	}
	if r.State.Terminal() {
		return c.Errorf(c.ErrConflict, "run already %s", r.State)
	}
	delete(b.busy, runID)
	delete(b.pendingCmd, runID)
	if reason == "" {
		reason = "simulated observation (mock)"
	}
	b.setRunStateLocked(r, st, reason, "node:"+r.NodeID)
	return nil
}

// ApprovalSpec describes a simulated runtime approval request.
type ApprovalSpec struct {
	Kind      c.ApprovalKind
	Title     string
	Body      string
	Options   []c.ApprovalOption
	AllowText bool
	TTL       time.Duration // 0 = no expiry
}

// RequestApproval simulates a runtime asking for approval on a run.
func (b *Backend) RequestApproval(runID string, spec ApprovalSpec) (c.Approval, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	r, ok := b.runs[runID]
	if !ok {
		return c.Approval{}, notFound(c.KindRun, runID)
	}
	if r.State.Terminal() {
		return c.Approval{}, c.Errorf(c.ErrConflict, "run already %s", r.State)
	}
	if spec.Kind == "" {
		spec.Kind = c.ApprovalPermission
	}
	if len(spec.Options) == 0 && !spec.AllowText {
		spec.Options = []c.ApprovalOption{{ID: "allow", Label: "Allow once", Kind: "allow"}, {ID: "deny", Label: "Deny", Kind: "deny"}}
	}
	if spec.Title == "" {
		spec.Title = "Synthetic permission request"
	}
	now := b.now()
	a := &c.Approval{ApprovalID: c.NewID(c.PrefixApproval), SessionID: r.SessionID, RunID: r.RunID, RequestID: "mock-req-" + c.NewID("r"), Kind: spec.Kind,
		Title: spec.Title, Body: spec.Body, Options: spec.Options, AllowText: spec.AllowText, CreatedAt: now, State: c.ApprovalPending}
	if spec.TTL > 0 {
		a.ExpiresAt = timePtr(now.Add(spec.TTL))
	}
	b.approvals[a.ApprovalID] = a
	r.PendingApprovalIDs = append(r.PendingApprovalIDs, a.ApprovalID)
	delete(b.busy, r.RunID)
	b.setRunStateLocked(r, c.RunWaitingApproval, "waiting for approval (mock)", "node:"+r.NodeID)
	var pid *string
	if s := b.sessions[r.SessionID]; s != nil {
		pid = s.ProjectID
	}
	b.emitFrom("node:"+r.NodeID, c.EvApprovalRequested, ref(c.KindApproval, a.ApprovalID), r.SessionID, pid, a)
	kind := c.AttnApproval
	if spec.Kind == c.ApprovalQuestion {
		kind = c.AttnQuestion
	}
	b.raiseAttentionLocked(kind, "urgent", "approval:"+a.ApprovalID, ref(c.KindApproval, a.ApprovalID), r.SessionID, pid, "Approve: "+a.Title, truncate(a.Body, 120))
	return clone(*a), nil
}

// ResolveApprovalNatively simulates the approval being answered directly in
// the terminal (so a stale phone link must see the existing resolution).
func (b *Backend) ResolveApprovalNatively(approvalID, optionID string) (c.Approval, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	a, ok := b.approvals[approvalID]
	if !ok {
		return c.Approval{}, notFound(c.KindApproval, approvalID)
	}
	if a.State != c.ApprovalPending {
		return c.Approval{}, c.Errorf(c.ErrConflict, "approval is %s", a.State)
	}
	a.State = c.ApprovalResolved
	a.Resolution = &c.ApprovalResolution{OptionID: optionID, Actor: c.Principal{Kind: c.PrincipalSystem, ID: "native-terminal"}, ResolvedAt: b.now(), Source: "native", Effect: "effective"}
	b.afterApprovalResolvedLocked(a)
	return clone(*a), nil
}

// TrimEvents drops all but the newest keep events, as retention would, so
// older cursors get reset_required.
func (b *Backend) TrimEvents(keep int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if keep < 0 {
		keep = 0
	}
	if over := len(b.events) - keep; over > 0 {
		b.eventFloor = b.events[over-1].Cursor
		b.events = append([]c.Event(nil), b.events[over:]...)
	}
}

// HeadCursor returns the newest event cursor.
func (b *Backend) HeadCursor() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.eventHead
}

// ReportDelivery simulates a node receipt for a message: it binds the
// message to the recipient's current run if it is not bound yet (never
// rebinding) and records the given outcome with mock evidence.
func (b *Backend) ReportDelivery(messageID string, st c.RuntimeDeliveryState, detail string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	m, ok := b.messages[messageID]
	if !ok {
		return notFound(c.KindMessage, messageID)
	}
	if m.Envelope.Recipient.Kind != c.AddrSession {
		return c.Errorf(c.ErrInvalid, "message has no runtime recipient")
	}
	switch st {
	case c.DeliveryNodeReceived, c.DeliveryAttempting, c.DeliveryAccepted, c.DeliveryRejected, c.DeliveryIndeterminate:
	default:
		return c.Errorf(c.ErrInvalid, "cannot report %s", st)
	}
	r := b.runs[m.Status.BoundRunID]
	if r == nil {
		r = b.currentRun(recipientSession(m))
	}
	if r == nil {
		return c.Errorf(c.ErrConflict, "recipient has no run")
	}
	if detail == "" {
		detail = "simulated node report (mock)"
	}
	if st != c.DeliveryNodeReceived {
		m.Status.BoundRunID = r.RunID
	}
	ev := c.EvidenceNone
	if st == c.DeliveryAccepted {
		ev = c.EvidenceMock
		now := b.now()
		m.Status.ConsumedAt = &now
	}
	rc := c.DeliveryReceipt{MessageID: messageID, AttemptID: c.NewID(c.PrefixAttempt), SessionID: r.SessionID, RunID: r.RunID, NodeID: r.NodeID,
		State: st, Evidence: ev, Detail: detail, ObservedAt: b.now()}
	b.setDeliveryLocked(m, st, detail, &rc)
	if st == c.DeliveryIndeterminate {
		b.raiseIndeterminateLocked(m)
	}
	return nil
}
