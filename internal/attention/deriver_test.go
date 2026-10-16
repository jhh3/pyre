package attention

import (
	"context"
	"strings"
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
)

func TestSequenceCollapseDuplicatesAndWithdrawal(t *testing.T) {
	e := newEnv(t)
	req := e.approval("apr_synth-1", contracts.EvApprovalRequested, nil)
	// The same approval reported twice (e.g. two observers) and the same
	// event re-delivered with a new cursor.
	e.approval("apr_synth-1", contracts.EvApprovalRequested, nil)
	e.log.Resend(req)
	for i := 0; i < 3; i++ {
		e.ownerMessage(contracts.NewID(contracts.PrefixMessage), "thr_synth-1", "ses_synth-a")
	}
	for i := 0; i < 10; i++ {
		e.peerMessage(contracts.NewID(contracts.PrefixMessage))
	}
	e.drain()

	items := e.list()
	if len(items) != 2 {
		t.Fatalf("want approval + message items, got %d: %+v", len(items), items)
	}
	apr := e.byGroup("approval:apr_synth-1")
	if len(apr) != 1 || apr[0].Count != 1 || apr[0].State != contracts.AttnOpen || apr[0].Severity != "urgent" {
		t.Fatalf("approval: %+v", apr)
	}
	if apr[0].DeepLink != "pyre://approval/apr_synth-1" {
		t.Fatalf("deep link = %q", apr[0].DeepLink)
	}
	msg := e.byGroup("message:thr_synth-1")
	if len(msg) != 1 || msg[0].Count != 3 || msg[0].DeepLink != "pyre://thread/thr_synth-1" {
		t.Fatalf("message collapse: %+v", msg)
	}
	// One notify job per item transition, not per event.
	if n := e.jobs(); n != 2 {
		t.Fatalf("notify jobs = %d, want 2", n)
	}

	// Resolved natively in the terminal: withdrawn everywhere.
	e.approval("apr_synth-1", contracts.EvApprovalResolved, func(a *contracts.Approval) {
		a.State = contracts.ApprovalResolved
		a.Resolution = &contracts.ApprovalResolution{Source: "native"}
	})
	e.drain()
	apr = e.byGroup("approval:apr_synth-1")
	if len(apr) != 1 || apr[0].State != contracts.AttnWithdrawn {
		t.Fatalf("approval not withdrawn: %+v", apr)
	}
	if len(e.list()) != 1 {
		t.Fatalf("withdrawn item still in default list")
	}
	// A late duplicate request cannot resurrect it.
	e.approval("apr_synth-1", contracts.EvApprovalRequested, nil)
	e.drain()
	if apr = e.byGroup("approval:apr_synth-1"); len(apr) != 1 || apr[0].State != contracts.AttnWithdrawn {
		t.Fatalf("late duplicate resurrected approval: %+v", apr)
	}
}

func TestResolutionBeforeRequestLeavesTombstone(t *testing.T) {
	e := newEnv(t)
	e.approval("apr_synth-1", contracts.EvApprovalWithdrawn, func(a *contracts.Approval) { a.State = contracts.ApprovalWithdrawn })
	e.approval("apr_synth-1", contracts.EvApprovalRequested, nil)
	e.drain()
	if len(e.list()) != 0 {
		t.Fatalf("withdrawn-first approval became open")
	}
	got := e.byGroup("approval:apr_synth-1")
	if len(got) != 1 || got[0].State != contracts.AttnWithdrawn {
		t.Fatalf("tombstone: %+v", got)
	}
	if e.jobs() != 0 {
		t.Fatalf("tombstone queued a notification")
	}
}

func TestNoLoopsFromAttentionEvents(t *testing.T) {
	e := newEnv(t)
	e.log.IgnoreKindFilter = true // a hub that ignores the kind filter
	e.approval("apr_synth-1", contracts.EvApprovalRequested, nil)
	e.ownerMessage("msg_synth-1", "thr_synth-1", "ses_synth-a")
	e.drain()
	head := e.log.Head()
	// Further steps see only our own attention.changed output and must
	// neither create items nor emit more events.
	e.drain()
	e.drain()
	if e.log.Head() != head {
		t.Fatalf("attention events produced more events: %d -> %d", head, e.log.Head())
	}
	if n := len(e.list()); n != 2 {
		t.Fatalf("items = %d", n)
	}
	// The deriver asks for derived kinds only, which exclude attention.changed.
	for _, k := range e.log.Requests[0].Kinds {
		if k == contracts.EvAttentionChanged {
			t.Fatal("deriver subscribes to attention.changed")
		}
	}
}

func TestRestartResumesFromPersistedCursor(t *testing.T) {
	e := newEnv(t)
	first := e.ownerMessage("msg_synth-1", "thr_synth-1", "ses_synth-a")
	e.ownerMessage("msg_synth-2", "thr_synth-1", "ses_synth-a")
	e.drain()
	cur, _ := e.store.Cursor(context.Background(), DefaultConsumer)
	if cur == 0 {
		t.Fatal("cursor not persisted")
	}
	// "Restart": a new Store and Deriver on the same database file.
	e.db.Close()
	e.open()
	if c2, _ := e.store.Cursor(context.Background(), DefaultConsumer); c2 != cur {
		t.Fatalf("cursor after restart = %d, want %d", c2, cur)
	}
	e.log.Resend(first) // duplicate delivery after restart
	e.ownerMessage("msg_synth-3", "thr_synth-1", "ses_synth-a")
	e.drain()
	msg := e.byGroup("message:thr_synth-1")
	if len(msg) != 1 || msg[0].Count != 3 {
		t.Fatalf("after restart: %+v", msg)
	}
	// Re-applying an already-applied event directly is a no-op.
	changed, err := e.store.applyEvent(context.Background(), DefaultConsumer, first, derive(context.Background(), first, nil))
	if err != nil || changed {
		t.Fatalf("re-apply: changed=%v err=%v", changed, err)
	}
}

type fakeSnapshot struct {
	approvals []contracts.Approval
	messages  []contracts.MessageView
}

func (f fakeSnapshot) PendingApprovals(context.Context) ([]contracts.Approval, error) {
	return f.approvals, nil
}
func (f fakeSnapshot) IndeterminateMessages(context.Context) ([]contracts.MessageView, error) {
	return f.messages, nil
}

func TestResetRequiredResnapshotsAndRecordsGap(t *testing.T) {
	e := newEnv(t)
	e.approval("apr_synth-old", contracts.EvApprovalRequested, nil)
	e.log.AppendPayload(contracts.EvMessageDelivery, contracts.ObjectRef{Kind: contracts.KindMessage, ID: "msg_synth-i1"}, "", nil,
		contracts.MessageView{Envelope: contracts.MessageEnvelope{MessageID: "msg_synth-i1"}, Status: contracts.DeliveryStatus{Runtime: contracts.DeliveryIndeterminate}})
	e.drain()
	if len(e.list()) != 2 {
		t.Fatalf("setup items = %d", len(e.list()))
	}
	from, _ := e.store.Cursor(context.Background(), DefaultConsumer)

	// While the deriver was away: the old approval was answered, a new one
	// is pending, the delivery was resolved, and history expired.
	for i := 0; i < 5; i++ {
		e.peerMessage(contracts.NewID(contracts.PrefixMessage))
	}
	e.log.Oldest = e.log.Head() - 1
	e.snap = fakeSnapshot{approvals: []contracts.Approval{{ApprovalID: "apr_synth-new", SessionID: "ses_synth-b", Kind: contracts.ApprovalQuestion, Title: "Pick one", State: contracts.ApprovalPending}}}
	e.der = NewDeriver(e.store, e.log, DeriverOptions{Snapshot: e.snap})
	snapHead := e.log.Head()
	if _, err := e.der.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if old := e.byGroup("approval:apr_synth-old"); len(old) != 1 || old[0].State != contracts.AttnWithdrawn {
		t.Fatalf("stale approval not withdrawn: %+v", old)
	}
	if nw := e.byGroup("approval:apr_synth-new"); len(nw) != 1 || nw[0].State != contracts.AttnOpen || nw[0].Kind != contracts.AttnQuestion {
		t.Fatalf("pending approval not re-derived: %+v", nw)
	}
	if ind := e.byGroup("indeterminate:msg_synth-i1"); len(ind) != 1 || ind[0].State != contracts.AttnWithdrawn {
		t.Fatalf("resolved delivery not withdrawn: %+v", ind)
	}
	gaps, _ := e.store.Gaps(context.Background())
	if len(gaps) != 1 || gaps[0].FromCursor != from || gaps[0].ToCursor != snapHead || gaps[0].Detail == "" {
		t.Fatalf("gap: %+v", gaps)
	}
	if cur, _ := e.store.Cursor(context.Background(), DefaultConsumer); cur != snapHead {
		t.Fatalf("cursor after reset = %d, want %d", cur, snapHead)
	}
	// Resumes normally afterwards.
	e.approval("apr_synth-new", contracts.EvApprovalResolved, func(a *contracts.Approval) { a.State = contracts.ApprovalResolved })
	e.drain()
	if nw := e.byGroup("approval:apr_synth-new"); nw[0].State != contracts.AttnWithdrawn {
		t.Fatalf("post-reset resolution: %+v", nw)
	}
}

func TestResetWithoutSnapshotRecordsGapOnly(t *testing.T) {
	e := newEnv(t)
	e.peerMessage("msg_synth-1")
	e.peerMessage("msg_synth-2")
	e.log.Oldest = 2
	if _, err := e.der.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	gaps, _ := e.store.Gaps(context.Background())
	if len(gaps) != 1 || gaps[0].FromCursor != 0 {
		t.Fatalf("gap: %+v", gaps)
	}
}

func TestSeverityEscalationRequeues(t *testing.T) {
	e := newEnv(t)
	e.log.AppendPayload(contracts.EvAccountExhausted, contracts.ObjectRef{Kind: contracts.KindAccount, ID: "acct_synth-1"}, "", nil,
		contracts.AccountObservation{AccountID: "acct_synth-1", Provider: contracts.ProviderAnthropic})
	e.drain()
	pool := e.byGroup("pool:anthropic")
	if len(pool) != 1 || pool[0].Severity != "normal" {
		t.Fatalf("pool: %+v", pool)
	}
	e.log.AppendPayload(contracts.EvFallbackPlanned, contracts.ObjectRef{Kind: contracts.KindSession, ID: "ses_synth-a"}, "ses_synth-a", nil,
		map[string]any{"session_id": "ses_synth-a", "outcome": "waiting", "provider": "anthropic", "uncertain_work": true})
	e.drain()
	pool = e.byGroup("pool:anthropic")
	if len(pool) != 1 || pool[0].Severity != "urgent" || pool[0].Count != 2 || pool[0].Title != "No eligible Claude account" {
		t.Fatalf("escalation: %+v", pool)
	}
	_, seq, _ := e.store.ItemForJob(context.Background(), pool[0].AttentionID)
	if seq != 2 || e.jobs() != 2 {
		t.Fatalf("escalation should be a new transition: seq=%d jobs=%d", seq, e.jobs())
	}
}

// An expired remote approval leaves the run waiting on its native prompt.
// The inbox keeps it actionable: the answer item is withdrawn (it can no
// longer be answered through Pyre) and an urgent item for the session says
// to answer in the terminal. The terminal resolving it withdraws that too.
func TestExpiredApprovalBecomesTerminalPromptItem(t *testing.T) {
	e := newEnv(t)
	e.approval("apr_synth-exp", contracts.EvApprovalRequested, nil)
	e.approval("apr_synth-exp", contracts.EvApprovalWithdrawn, func(a *contracts.Approval) { a.State = contracts.ApprovalExpired })
	e.drain()
	if apr := e.byGroup("approval:apr_synth-exp"); len(apr) != 1 || apr[0].State != contracts.AttnWithdrawn {
		t.Fatalf("answer item after expiry: %+v", apr)
	}
	term := e.byGroup("terminal:apr_synth-exp")
	if len(term) != 1 || term[0].State != contracts.AttnOpen || term[0].Severity != "urgent" || term[0].Object.Kind != contracts.KindSession {
		t.Fatalf("terminal item: %+v", term)
	}
	if !strings.Contains(term[0].Title, "terminal") || !strings.Contains(strings.ToLower(term[0].Summary), "attach") || !strings.Contains(term[0].Summary, "interrupt") {
		t.Fatalf("terminal item does not say how to resolve it: %q / %q", term[0].Title, term[0].Summary)
	}
	e.approval("apr_synth-exp", contracts.EvApprovalResolved, func(a *contracts.Approval) {
		a.State = contracts.ApprovalResolved
		a.Resolution = &contracts.ApprovalResolution{Source: "native"}
	})
	e.drain()
	if term := e.byGroup("terminal:apr_synth-exp"); len(term) != 1 || term[0].State != contracts.AttnWithdrawn {
		t.Fatalf("terminal item after native resolution: %+v", term)
	}
}

// One injected limit produces three events: the node's account.exhausted
// (accounts service), and the hub's account.fallback plus the
// account.exhausted it derives from that plan. They describe one incident:
// one item with count 1 (seen live: count x3). A later limit on another
// run is a new occurrence.
func TestOneLimitIsOneAttentionOccurrence(t *testing.T) {
	e := newEnv(t)
	sess := contracts.ObjectRef{Kind: contracts.KindSession, ID: "ses_synth-a"}
	limit := func(run string) {
		e.log.AppendPayload(contracts.EvAccountExhausted, sess, "ses_synth-a", nil, map[string]any{
			"provider": "openai", "session_id": "ses_synth-a", "from_run_id": run, "from_account_id": "acct_synth-2", "outcome": "waiting"})
		plan := contracts.FallbackPlan{SessionID: "ses_synth-a", FromRunID: run, FromAccountID: "acct_synth-2", Outcome: contracts.FallbackWaiting,
			Provider: contracts.ProviderOpenAI, Reason: "no eligible openai subscription"}
		e.log.AppendPayload(contracts.EvFallbackPlanned, sess, "ses_synth-a", nil, plan)
		e.log.AppendPayload(contracts.EvAccountExhausted, sess, "ses_synth-a", nil, plan)
		e.drain()
	}
	limit("run_synth-1")
	pool := e.byGroup("pool:openai")
	if len(pool) != 1 || pool[0].Count != 1 || pool[0].Severity != "urgent" {
		t.Fatalf("one limit: %+v", pool)
	}
	limit("run_synth-2")
	if pool = e.byGroup("pool:openai"); len(pool) != 1 || pool[0].Count != 2 {
		t.Fatalf("second limit: %+v", pool)
	}
}

// An exhaustion item a session raised ("No eligible Claude account") is
// withdrawn when a later run of that session goes live (not while it is
// only starting); another session's item stays.
func TestRunStartedWithdrawsStaleExhaustion(t *testing.T) {
	e := newEnv(t)
	for _, c := range []struct{ ses, prov string }{{"ses_synth-a", "anthropic"}, {"ses_synth-b", "openai"}} {
		plan := contracts.FallbackPlan{SessionID: c.ses, FromRunID: "run_" + c.ses, FromAccountID: "acct_synth-2", Outcome: contracts.FallbackWaiting,
			Provider: contracts.Provider(c.prov), Reason: "no eligible subscription"}
		e.log.AppendPayload(contracts.EvFallbackPlanned, contracts.ObjectRef{Kind: contracts.KindSession, ID: c.ses}, c.ses, nil, plan)
	}
	e.drain()
	if a, b := e.byGroup("pool:anthropic"), e.byGroup("pool:openai"); len(a) != 1 || a[0].State != contracts.AttnOpen || len(b) != 1 {
		t.Fatalf("raised: %+v %+v", a, b)
	}
	run := contracts.Run{RunID: "run_synth-next", SessionID: "ses_synth-a", State: contracts.RunStarting}
	e.log.AppendPayload(contracts.EvRunStarted, contracts.ObjectRef{Kind: contracts.KindRun, ID: run.RunID}, "ses_synth-a", nil, run)
	e.drain()
	if a := e.byGroup("pool:anthropic"); len(a) != 1 || a[0].State != contracts.AttnOpen {
		t.Fatalf("withdrawn by a run that is only starting: %+v", a)
	}
	run.State = contracts.RunRunning
	e.log.AppendPayload(contracts.EvRunObserved, contracts.ObjectRef{Kind: contracts.KindRun, ID: run.RunID}, "ses_synth-a", nil, run)
	e.drain()
	if a := e.byGroup("pool:anthropic"); len(a) != 1 || a[0].State != contracts.AttnWithdrawn {
		t.Fatalf("stale exhaustion kept: %+v", a)
	}
	if b := e.byGroup("pool:openai"); len(b) != 1 || b[0].State != contracts.AttnOpen {
		t.Fatalf("other session's item touched: %+v", b)
	}
}
