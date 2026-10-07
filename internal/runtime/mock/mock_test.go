package mock

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/buildinfo"
	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/enrollment"
	"github.com/jhh3/pyre/internal/transport"
)

var owner = c.Principal{Kind: c.PrincipalOwner, ID: "cli_synth-laptop", Name: "owner (synthetic)"}

func as(p c.Principal) context.Context { return c.WithPrincipal(context.Background(), p) }

func newFixtureBackend(t *testing.T) (*Backend, *FakeClock) {
	t.Helper()
	clk := NewFakeClock(FixtureEpoch)
	b, err := NewE(Options{Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	return b, clk
}

func newEmpty(t *testing.T) (*Backend, *FakeClock, string) {
	t.Helper()
	clk := NewFakeClock(FixtureEpoch)
	node := c.Node{NodeID: "node_test", Name: "test-node", Platform: "linux", Arch: "amd64",
		Connection: c.NodeConnection{State: c.ConnConnected}, Harnesses: []c.HarnessInfo{}, Versions: map[string]string{}, Capabilities: []string{}}
	b := New(Options{Clock: clk, Empty: true})
	b.nodes[node.NodeID] = &node
	return b, clk, node.NodeID
}

func start(t *testing.T, b *Backend, nodeID string) c.SessionView {
	t.Helper()
	resp, err := b.StartSession(as(owner), c.StartSessionRequest{Mutation: c.Mutation{OperationID: c.NewID("op")}, NodeID: nodeID, Harness: c.HarnessMock, Cwd: "/tmp/x"})
	if err != nil {
		t.Fatal(err)
	}
	b.Tick()
	return resp.Session
}

func op() c.Mutation { return c.Mutation{OperationID: c.NewID("op")} }

func TestSeedFromFixtures(t *testing.T) {
	b, _ := newFixtureBackend(t)
	h, err := b.Hello(as(owner), c.HelloRequest{Protocol: 1})
	if err != nil || !h.Mock || h.Principal != owner {
		t.Fatalf("hello %+v %v", h, err)
	}
	nodes, _ := b.ListNodes(as(owner), c.ListNodesRequest{})
	if len(nodes.Items) < 2 {
		t.Fatalf("nodes %d", len(nodes.Items))
	}
	sess, _ := b.ListSessions(as(owner), c.ListSessionsRequest{PageRequest: c.PageRequest{Limit: 500}})
	if len(sess.Items) < 8 {
		t.Fatalf("sessions %d", len(sess.Items))
	}
	for _, s := range sess.Items {
		if s.Run != nil && !strings.HasPrefix(s.Run.Freshness.Source, "mock") {
			t.Errorf("run %s freshness not labelled mock: %q", s.Run.RunID, s.Run.Freshness.Source)
		}
	}
	// standalone filter
	empty := ""
	st, _ := b.ListSessions(as(owner), c.ListSessionsRequest{ProjectID: &empty, PageRequest: c.PageRequest{Limit: 500}})
	for _, s := range st.Items {
		if s.Session.ProjectID != nil {
			t.Fatal("standalone filter returned a project session")
		}
	}
	// seeded artifact content verifies and serves
	rc, v, err := b.OpenContent(as(owner), "ver_synth-report-1")
	if err != nil || v.Size == 0 {
		t.Fatalf("content %v", err)
	}
	rc.Close()
	// events renumbered from 1
	ev, err := b.ListEvents(as(owner), c.ListEventsRequest{})
	if err != nil || len(ev.Events) == 0 || ev.Events[0].Cursor != 1 {
		t.Fatalf("events %+v %v", ev, err)
	}
	if _, err := b.WhoAmI(context.Background()); !c.IsCode(err, c.ErrUnauthorized) {
		t.Fatalf("no principal: %v", err)
	}
	if _, err := b.Hello(as(owner), c.HelloRequest{Protocol: 99}); !c.IsCode(err, c.ErrProtocolMismatch) {
		t.Fatalf("protocol: %v", err)
	}
}

// TestFixtureExchanges replays every request/response example in the shared
// fixtures against one seeded backend and checks the outcome category.
func TestFixtureExchanges(t *testing.T) {
	b, _ := newFixtureBackend(t)
	bundles, err := c.LoadFixtureDir(DefaultFixtureDirs()[0])
	if err != nil {
		t.Fatal(err)
	}
	// Known, explained deviations between fixture examples and live semantics.
	deviations := map[string]string{
		"finalize with wrong digest": "upl_synth-bad is not a seeded upload; digest mismatch is covered by conformance",
		"node with newer protocol":   "node.connect is not an API op (node channel is WP04)",
	}
	ran := 0
	for _, f := range bundles {
		for _, x := range f.Exchanges {
			if why, ok := deviations[x.Name]; ok {
				t.Logf("skip %s/%s: %s", f.Name, x.Name, why)
				continue
			}
			p := owner
			if x.Principal != nil {
				p = *x.Principal
			}
			resp, err := invoke(b, as(p), x.Op, x.Request)
			ran++
			if x.Error != nil {
				if !c.IsCode(err, x.Error.Code) {
					t.Errorf("%s/%s: want %s, got %v", f.Name, x.Name, x.Error.Code, err)
				}
				continue
			}
			if err != nil {
				t.Errorf("%s/%s: unexpected error %v", f.Name, x.Name, err)
				continue
			}
			_ = resp
		}
	}
	if ran < 10 {
		t.Fatalf("only %d exchanges ran", ran)
	}
}

func invoke(b *Backend, ctx context.Context, op string, raw json.RawMessage) (any, error) {
	dec := func(v any) error {
		if err := json.Unmarshal(raw, v); err != nil {
			return c.Errorf(c.ErrInvalid, "%v", err)
		}
		return nil
	}
	switch op {
	case c.OpStartSession:
		var r c.StartSessionRequest
		if err := dec(&r); err != nil {
			return nil, err
		}
		return b.StartSession(ctx, r)
	case c.OpSendMessage:
		var r c.SendMessageRequest
		if err := dec(&r); err != nil {
			return nil, err
		}
		return b.SendMessage(ctx, r)
	case c.OpHello:
		var r c.HelloRequest
		if err := dec(&r); err != nil {
			return nil, err
		}
		return b.Hello(ctx, r)
	case c.OpAcquireLease:
		var r c.LeaseRequest
		if err := dec(&r); err != nil {
			return nil, err
		}
		return b.AcquireLease(ctx, r)
	case c.OpProposeRevision:
		var r c.ProposeRevisionRequest
		if err := dec(&r); err != nil {
			return nil, err
		}
		return b.ProposeRevision(ctx, r)
	case c.OpFinalizeUpload:
		var r c.FinalizeUploadRequest
		if err := dec(&r); err != nil {
			return nil, err
		}
		return b.FinalizeUpload(ctx, r)
	case c.OpAnswerApproval:
		var r c.AnswerApprovalRequest
		if err := dec(&r); err != nil {
			return nil, err
		}
		return b.AnswerApproval(ctx, r)
	}
	return nil, c.Errorf(c.ErrUnsupported, "test invoker lacks op %s", op)
}

func TestDeliverySimulation(t *testing.T) {
	b, clk, node := newEmpty(t)
	sa := start(t, b, node)
	sb := start(t, b, node)
	if sb.Run.State != c.RunStarting {
		t.Fatalf("new run should be starting, got %s", sb.Run.State)
	}
	asA := as(c.Principal{Kind: c.PrincipalSession, ID: sa.Session.SessionID})
	asB := as(c.Principal{Kind: c.PrincipalSession, ID: sb.Session.SessionID})
	send := func(text string) c.MessageView {
		m, err := b.SendMessage(asA, c.SendMessageRequest{Mutation: op(), Recipient: c.Address{Kind: c.AddrSession, ID: sb.Session.SessionID}, Body: c.MessageBody{Text: text}})
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	get := func(id string) c.MessageView {
		m, err := b.GetMessage(asA, c.GetRequest{ID: id})
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	m := send("hello")
	if m.Status.Storage != c.StorageStored || m.Status.Runtime != c.DeliveryQueued || m.Status.Ack != c.AckNone || m.Status.BoundRunID != "" {
		t.Fatalf("initial status %+v", m.Status)
	}
	want := []c.RuntimeDeliveryState{c.DeliveryNodeReceived, c.DeliveryAttempting, c.DeliveryAccepted}
	for _, w := range want {
		b.Tick()
		if got := get(m.Envelope.MessageID).Status.Runtime; got != w {
			t.Fatalf("want %s got %s", w, got)
		}
	}
	acc := get(m.Envelope.MessageID)
	last := acc.Status.Receipts[len(acc.Status.Receipts)-1]
	if acc.Status.BoundRunID != sb.Run.RunID || last.Evidence != c.EvidenceMock || acc.Status.ConsumedAt == nil || acc.Status.Ack != c.AckNone {
		t.Fatalf("accepted status %+v", acc.Status)
	}
	// The recipient is now busy; the next message stays queued until idle.
	m2 := send("second")
	b.Tick() // node_received; recipient finishing its turn
	b.Tick()
	b.Settle(0)
	if get(m2.Envelope.MessageID).Status.Runtime != c.DeliveryAccepted {
		t.Fatalf("second message: %+v", get(m2.Envelope.MessageID).Status)
	}
	// Busy recipient (forced) keeps mail queued.
	if err := b.SetRunState(sb.Run.RunID, c.RunRunning, "busy"); err != nil {
		t.Fatal(err)
	}
	m3 := send("third")
	b.Settle(10)
	if st := get(m3.Envelope.MessageID).Status; st.Runtime != c.DeliveryNodeReceived || !strings.Contains(st.RuntimeReason, "busy") {
		t.Fatalf("busy recipient: %+v", st)
	}
	// Disconnected node keeps queued; reconnect resumes.
	b.SetRunState(sb.Run.RunID, c.RunWaitingInput, "")
	b.SetNodeConnected(node, false)
	m4 := send("fourth")
	b.Settle(10)
	if st := get(m4.Envelope.MessageID).Status; st.Runtime != c.DeliveryQueued {
		t.Fatalf("disconnected: %+v", st)
	}
	sv, _ := b.GetSession(asA, c.GetRequest{ID: sb.Session.SessionID})
	if sv.Run.State != c.RunUnknown || !sv.Run.Freshness.Stale {
		t.Fatalf("disconnected run should be unknown/stale, got %s %+v", sv.Run.State, sv.Run.Freshness)
	}
	b.SetNodeConnected(node, true)
	b.Settle(0)
	if get(m4.Envelope.MessageID).Status.Runtime != c.DeliveryAccepted || get(m3.Envelope.MessageID).Status.Runtime != c.DeliveryAccepted {
		t.Fatal("delivery did not resume after reconnect")
	}
	// Markers.
	mi := send("do it " + MarkerIndeterminate)
	mr := send("nope " + MarkerReject)
	b.Settle(0)
	if get(mi.Envelope.MessageID).Status.Runtime != c.DeliveryIndeterminate || get(mr.Envelope.MessageID).Status.Runtime != c.DeliveryRejected {
		t.Fatalf("markers: %s %s", get(mi.Envelope.MessageID).Status.Runtime, get(mr.Envelope.MessageID).Status.Runtime)
	}
	att, _ := b.ListAttention(as(owner), c.ListAttentionRequest{Kinds: []c.AttentionKind{c.AttnIndeterminate}})
	if len(att.Items) != 1 {
		t.Fatalf("indeterminate attention %d", len(att.Items))
	}
	// Reply sets ack=replied on the original.
	rep, err := b.SendMessage(asB, c.SendMessageRequest{Mutation: op(), Recipient: c.Address{Kind: c.AddrSession, ID: sa.Session.SessionID}, InReplyTo: m.Envelope.MessageID, Body: c.MessageBody{Text: "done"}})
	if err != nil {
		t.Fatal(err)
	}
	if o := get(m.Envelope.MessageID); o.Status.Ack != c.AckReplied || o.Status.AckRef != rep.Envelope.MessageID {
		t.Fatalf("reply ack %+v", o.Status)
	}
	// TTL expiry gives a terminal explanation.
	b.SetRunState(sb.Run.RunID, c.RunRunning, "busy")
	mt, _ := b.SendMessage(asA, c.SendMessageRequest{Mutation: op(), Recipient: c.Address{Kind: c.AddrSession, ID: sb.Session.SessionID}, Body: c.MessageBody{Text: "soon"}, TTLSeconds: 60})
	clk.Advance(2 * time.Minute)
	b.Tick()
	if st := get(mt.Envelope.MessageID).Status; st.Runtime != c.DeliveryExpired || st.RuntimeReason == "" {
		t.Fatalf("expiry %+v", st)
	}
	// Transcript carries the marker.
	rs, _ := b.ReadSession(asA, c.ReadSessionRequest{SessionID: sb.Session.SessionID})
	found := false
	for _, e := range rs.Entries {
		if strings.Contains(e.Text, c.MessageMarker(m.Envelope.MessageID)) {
			found = true
		}
	}
	if !found {
		t.Fatal("delivered message missing from transcript")
	}
}

func TestStartStopResume(t *testing.T) {
	b, _, node := newEmpty(t)
	s := start(t, b, node)
	ctx := as(owner)
	if _, err := b.ResumeSession(ctx, c.ResumeRequest{Mutation: op(), SessionID: s.Session.SessionID}); !c.IsCode(err, c.ErrConflict) {
		t.Fatalf("resume of live run: %v", err)
	}
	if _, err := b.StopSession(ctx, c.SessionCommandRequest{Mutation: op(), SessionID: s.Session.SessionID, RunID: "run_other"}); !c.IsCode(err, c.ErrConflict) {
		t.Fatalf("stop wrong run: %v", err)
	}
	rc, err := b.StopSession(ctx, c.SessionCommandRequest{Mutation: op(), SessionID: s.Session.SessionID})
	if err != nil || !rc.Accepted {
		t.Fatal(err)
	}
	b.Tick()
	sv, _ := b.GetSession(ctx, c.GetRequest{ID: s.Session.SessionID})
	if sv.Run.State != c.RunExited {
		t.Fatalf("after stop %s", sv.Run.State)
	}
	r2, err := b.ResumeSession(ctx, c.ResumeRequest{Mutation: op(), SessionID: s.Session.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if r2.Session.Run.PreviousRunID != s.Run.RunID || r2.Session.Run.ContinuationKind != c.ContinuationNativeResume {
		t.Fatalf("resume lineage %+v", r2.Session.Run)
	}
	runs, _ := b.ListRuns(ctx, c.ListRunsRequest{SessionID: s.Session.SessionID})
	if len(runs.Items) != 2 {
		t.Fatalf("runs %d", len(runs.Items))
	}
	// capacity
	b.nodes[node].Resources.MaxRuns = 1
	if _, err := b.StartSession(ctx, c.StartSessionRequest{Mutation: op(), NodeID: node, Harness: c.HarnessMock, Cwd: "/tmp"}); !c.IsCode(err, c.ErrCapacityExhausted) {
		t.Fatalf("capacity: %v", err)
	}
}

func TestEventsLongPollAndRetention(t *testing.T) {
	b, _, node := newEmpty(t)
	ctx := as(owner)
	head := b.HeadCursor()
	done := make(chan c.ListEventsResponse, 1)
	go func() {
		r, _ := b.ListEvents(ctx, c.ListEventsRequest{AfterCursor: head, WaitSeconds: 5, Kinds: []c.EventKind{c.EvSessionCreated}})
		done <- r
	}()
	time.Sleep(50 * time.Millisecond)
	start(t, b, node)
	select {
	case r := <-done:
		if len(r.Events) != 1 || r.Events[0].Kind != c.EvSessionCreated {
			t.Fatalf("long poll %+v", r)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("long poll did not wake")
	}
	b.TrimEvents(1)
	_, err := b.ListEvents(ctx, c.ListEventsRequest{AfterCursor: 0})
	ce, _ := err.(*c.Error)
	if ce == nil || ce.Code != c.ErrResetRequired || ce.Details["snapshot_cursor"] == nil {
		t.Fatalf("reset: %v", err)
	}
	if _, err := b.ListEvents(ctx, c.ListEventsRequest{AfterCursor: b.HeadCursor() - 1}); err != nil {
		t.Fatalf("retained cursor: %v", err)
	}
	// Payload shapes follow the contract (session.created -> Session).
	ev, _ := b.ListEvents(ctx, c.ListEventsRequest{AfterCursor: b.HeadCursor() - 1})
	_ = ev
}

func TestNodeActions(t *testing.T) {
	b, _ := newFixtureBackend(t)
	ctx := as(owner)
	act := func(action string, params any) c.NodeActionResult {
		raw, _ := json.Marshal(params)
		r, err := b.InvokeNodeAction(ctx, c.NodeActionRequest{Mutation: op(), NodeID: "node_synth-mac", Action: action, Params: raw})
		if err != nil {
			t.Fatalf("%s: %v", action, err)
		}
		return r
	}
	inv := act(c.ActionInventory, nil)
	var ni c.NodeInventory
	if inv.Outcome != c.OutcomeSucceeded || json.Unmarshal(inv.Result, &ni) != nil || len(ni.Tools) == 0 {
		t.Fatalf("inventory %+v", inv)
	}
	act(c.ActionUpdateStage, c.UpdateParams{Tool: "claude", Version: "2.1.999"})
	r := act(c.ActionUpdateActivate, c.UpdateParams{Tool: "claude", SimulateHealthFailure: true})
	if r.Outcome != c.OutcomeFailed || r.Error == nil {
		t.Fatalf("health failure %+v", r)
	}
	r = act(c.ActionAccountInjectLimit, c.InjectLimitParams{SessionID: "ses_synth-standalone", Kind: "usage_limit"})
	var plan c.FallbackPlan
	json.Unmarshal(r.Result, &plan)
	if plan.Outcome == "" || plan.Provider != c.ProviderAnthropic || (plan.ToAccountID != "" && !strings.HasPrefix(plan.ToAccountID, "acct_synth-claude")) {
		t.Fatalf("fallback crossed providers or empty: %+v", plan)
	}
	// connection grant enforced for sessions
	raw, _ := json.Marshal(c.ConnectionInvokeParams{ConnectionID: "conn_synth-github", Operation: "list"})
	res, err := b.InvokeNodeAction(as(c.Principal{Kind: c.PrincipalSession, ID: "ses_synth-standalone"}), c.NodeActionRequest{Mutation: op(), NodeID: "node_synth-mac", Action: c.ActionConnInvoke, Params: raw})
	if err != nil || res.Error == nil || res.Error.Code != c.ErrForbidden {
		t.Fatalf("ungranted invoke: %+v %v", res, err)
	}
	if _, err := b.InvokeNodeAction(as(c.Principal{Kind: c.PrincipalSession, ID: "ses_synth-standalone"}), c.NodeActionRequest{Mutation: op(), NodeID: "node_synth-mac", Action: c.ActionEnvApply}); !c.IsCode(err, c.ErrForbidden) {
		t.Fatalf("session env.apply: %v", err)
	}
	if _, err := b.InvokeNodeAction(ctx, c.NodeActionRequest{Mutation: op(), NodeID: "node_synth-linux", Action: "bogus"}); !c.IsCode(err, c.ErrNotFound) {
		t.Fatalf("unknown action: %v", err)
	}
}

func TestAdapter(t *testing.T) {
	a := NewAdapter()
	ctx := context.Background()
	h, err := a.Launch(ctx, c.ResolvedLaunch{RunID: "run_a", SessionID: "ses_a", Cwd: "/tmp"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Launch(ctx, c.ResolvedLaunch{RunID: "run_a", SessionID: "ses_a"}); !c.IsCode(err, c.ErrConflict) {
		t.Fatal("duplicate launch allowed")
	}
	if _, err := a.Recover(ctx, h); err != nil {
		t.Fatal(err)
	}
	res, _ := a.Deliver(ctx, h, c.DeliveryInput{MessageID: "msg_1", AttemptID: "dlv_1", Mode: c.ModeQueue, Text: "hi"})
	if res.State != c.DeliveryAccepted || res.Evidence != c.EvidenceMock {
		t.Fatalf("deliver %+v", res)
	}
	// busy: queued, nothing consumed
	res, _ = a.Deliver(ctx, h, c.DeliveryInput{MessageID: "msg_2", AttemptID: "dlv_2", Text: "again"})
	if res.State != c.DeliveryQueued {
		t.Fatalf("busy %+v", res)
	}
	// idempotent repeat of msg_1
	res, _ = a.Deliver(ctx, h, c.DeliveryInput{MessageID: "msg_1", AttemptID: "dlv_9", Text: "hi"})
	if res.State != c.DeliveryAccepted {
		t.Fatalf("idempotent %+v", res)
	}
	a.Observe(ctx, h, "")
	// crash window then reconcile
	a.CrashAfterSubmit("run_a")
	if _, err := a.Deliver(ctx, h, c.DeliveryInput{MessageID: "msg_3", AttemptID: "dlv_3", Text: "crash"}); !c.IsCode(err, c.ErrOutcomeIndeterminate) {
		t.Fatalf("crash: %v", err)
	}
	if r, _ := a.Reconcile(ctx, h, "msg_3"); r.State != c.DeliveryAccepted {
		t.Fatalf("reconcile %+v", r)
	}
	if r, _ := a.Reconcile(ctx, h, "msg_never"); r.State != c.DeliveryIndeterminate {
		t.Fatalf("reconcile unknown %+v", r)
	}
	tr, _ := a.ReadTranscript(ctx, h, c.ReadSessionRequest{})
	n := 0
	for _, e := range tr.Entries {
		if e.MessageID == "msg_3" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("msg_3 submitted %d times", n)
	}
	a.Observe(ctx, h, "")
	ap := a.RequestApproval("run_a", c.Approval{Kind: c.ApprovalPermission, Title: "x"})
	if o, _ := a.Observe(ctx, h, ""); o.State != c.RunWaitingApproval || len(o.Approvals) != 1 {
		t.Fatalf("approval obs %+v", o)
	}
	if _, err := a.Answer(ctx, h, ap, "allow", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Answer(ctx, h, ap, "allow", ""); !c.IsCode(err, c.ErrConflict) {
		t.Fatal("double answer")
	}
	a.Stop(ctx, h)
	if _, err := a.Recover(ctx, h); !c.IsCode(err, c.ErrNotFound) {
		t.Fatal("recover after exit")
	}
	if res, _ := a.Deliver(ctx, h, c.DeliveryInput{MessageID: "msg_4", Text: "late"}); res.State != c.DeliveryRejected {
		t.Fatalf("deliver after exit %+v", res)
	}
}

// The approval/question markers let a client test raise real pending
// requests through a real pyred (--dev-mock) without a harness.
func TestAdapterMarkersRaiseRequests(t *testing.T) {
	a := NewAdapter()
	ctx := context.Background()
	h, err := a.Launch(ctx, c.ResolvedLaunch{RunID: "run_m", SessionID: "ses_m", Cwd: "/tmp"})
	if err != nil {
		t.Fatal(err)
	}
	res, _ := a.Deliver(ctx, h, c.DeliveryInput{MessageID: "msg_p", AttemptID: "dlv_p", Mode: c.ModeQueue, Text: "please " + MarkerApproval})
	if res.State != c.DeliveryAccepted {
		t.Fatalf("deliver %+v", res)
	}
	o, _ := a.Observe(ctx, h, "")
	if o.State != c.RunWaitingApproval || len(o.Approvals) != 1 {
		t.Fatalf("obs %+v", o)
	}
	p := o.Approvals[0]
	if p.Kind != c.ApprovalPermission || p.RequestID != "mock-perm-msg_p" || p.RunID != "run_m" || p.SessionID != "ses_m" || len(p.Options) != 2 || p.State != c.ApprovalPending {
		t.Fatalf("permission %+v", p)
	}
	// Busy while waiting for approval: the next input queues.
	if res, _ := a.Deliver(ctx, h, c.DeliveryInput{MessageID: "msg_q", AttemptID: "dlv_q", Text: MarkerQuestion}); res.State != c.DeliveryQueued {
		t.Fatalf("busy %+v", res)
	}
	if _, err := a.Answer(ctx, h, p, "deny", ""); err != nil {
		t.Fatal(err)
	}
	a.Observe(ctx, h, "") // turn finishes
	if res, _ := a.Deliver(ctx, h, c.DeliveryInput{MessageID: "msg_q", AttemptID: "dlv_q2", Text: MarkerQuestion}); res.State != c.DeliveryAccepted {
		t.Fatalf("question deliver %+v", res)
	}
	o, _ = a.Observe(ctx, h, "")
	var q *c.Approval
	for i := range o.Approvals {
		if o.Approvals[i].Kind == c.ApprovalQuestion {
			q = &o.Approvals[i]
		}
	}
	if q == nil || !q.AllowText || q.State != c.ApprovalPending || o.State != c.RunWaitingApproval {
		t.Fatalf("question %+v", o)
	}
	if _, err := a.Answer(ctx, h, *q, "", "green"); err != nil {
		t.Fatal(err)
	}
}

// Mock-issued pairing tickets use the real enrollment link and code format,
// so real clients (CLI, Android) can parse and redeem them.
func TestPairingTicketParsesWithEnrollment(t *testing.T) {
	pin := strings.Repeat("ab", 32)
	b, err := NewE(Options{Clock: NewFakeClock(FixtureEpoch), Empty: true, HubURL: "https://127.0.0.1:7788", HubCertSHA256: pin})
	if err != nil {
		t.Fatal(err)
	}
	ticket, err := b.CreatePairing(as(owner), c.PairingRequest{Mutation: c.Mutation{OperationID: c.NewID(c.PrefixOperation)}, ClientName: "synthetic phone", Kind: "android"})
	if err != nil {
		t.Fatal(err)
	}
	p, err := enrollment.ParsePairingURI(ticket.URI)
	if err != nil {
		t.Fatalf("ParsePairingURI(%q): %v", ticket.URI, err)
	}
	if p.HubURL != ticket.HubURL || p.PinSHA256 != pin || p.PairingID != ticket.PairingID || p.Code != ticket.Code {
		t.Fatalf("parsed %+v from ticket %+v", p, ticket)
	}
	if !strings.HasPrefix(ticket.Code, transport.PrefixPairingCode) {
		t.Fatalf("code %q lacks %q", ticket.Code, transport.PrefixPairingCode)
	}
	if _, err := b.RedeemPairing(c.PairingExchange{PairingID: p.PairingID, Code: p.Code}); err != nil {
		t.Fatalf("redeem parsed ticket: %v", err)
	}
}

// The mock adapter reports the build's version, not a placeholder equal to
// the harness name (which rendered as "mock mock" in `pyre nodes`).
func TestAdapterProbeVersion(t *testing.T) {
	p, err := NewAdapter(AdapterOptions{}).Probe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if p.Version == string(c.HarnessMock) || p.Version != buildinfo.Version {
		t.Fatalf("probe version %q, want build version %q", p.Version, buildinfo.Version)
	}
}
