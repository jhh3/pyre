package delivery

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/daemon/daemontest"
	"github.com/jhh3/pyre/internal/messaging/inbox"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

type fakeRuns struct {
	mu       sync.Mutex
	adapter  *daemontest.Adapter
	wrap     func(contracts.RuntimeAdapter) contracts.RuntimeAdapter
	runs     map[string]*contracts.Run // run ID -> run
	current  map[string]string         // session -> run ID
	attached map[string]bool
}

func (f *fakeRuns) target(r *contracts.Run) Target {
	h := contracts.RunHandle{RunID: r.RunID, SessionID: r.SessionID, Adapter: f.adapter.Name()}
	if p, ok := f.adapter.World.Proc(r.RunID); ok {
		h = p.Handle
	}
	var ad contracts.RuntimeAdapter = f.adapter
	if f.wrap != nil {
		ad = f.wrap(ad)
	}
	return Target{Run: *r, Handle: h, Adapter: ad, Attached: f.attached[r.RunID]}
}

func (f *fakeRuns) Current(ctx context.Context, sessionID string) (Target, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.current[sessionID]
	if !ok {
		return Target{}, false
	}
	return f.target(f.runs[id]), true
}

func (f *fakeRuns) Get(ctx context.Context, runID string) (Target, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.runs[runID]
	if !ok {
		return Target{}, false
	}
	return f.target(r), true
}

func (f *fakeRuns) set(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn()
}

type env struct {
	t      *testing.T
	db     *sql.DB
	path   string
	inbox  *inbox.Store
	rep    *daemontest.Reporter
	world  *daemontest.World
	ad     *daemontest.Adapter
	runs   *fakeRuns
	engine *Engine
}

func newEnv(t *testing.T) *env {
	t.Helper()
	path := filepath.Join(t.TempDir(), "node.db")
	e := &env{t: t, path: path, world: daemontest.NewWorld()}
	e.ad = daemontest.New(e.world, contracts.HarnessClaude)
	e.open()
	e.runs = &fakeRuns{adapter: e.ad, runs: map[string]*contracts.Run{}, current: map[string]string{}, attached: map[string]bool{}}
	e.engine = e.newEngine()
	return e
}

func (e *env) open() {
	db, err := sqlitex.Open(e.path)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { db.Close() })
	e.db = db
	e.inbox, err = inbox.Open(context.Background(), db, inbox.Limits{})
	if err != nil {
		e.t.Fatal(err)
	}
	e.rep, err = daemontest.NewReporter(db)
	if err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) newEngine() *Engine {
	return New(Config{NodeID: "node_test", Inbox: e.inbox, Runs: e.runs, Reporter: e.rep, MinBackoff: 10 * time.Millisecond, MaxBackoff: 50 * time.Millisecond})
}

// launch creates a live fake run for a session.
func (e *env) launch(session, runID string, caps []string) {
	e.t.Helper()
	if _, err := e.ad.Launch(context.Background(), contracts.ResolvedLaunch{RunID: runID, SessionID: session, StateDir: "/x/" + runID}); err != nil {
		e.t.Fatal(err)
	}
	if caps == nil {
		caps = e.ad.Caps
	}
	e.runs.set(func() {
		e.runs.runs[runID] = &contracts.Run{RunID: runID, SessionID: session, Adapter: e.ad.Name(), State: contracts.RunWaitingInput, Capabilities: caps}
		e.runs.current[session] = runID
	})
}

func (e *env) put(id, session string, mut func(*contracts.DeliverCommand)) {
	e.t.Helper()
	d := contracts.DeliverCommand{SessionID: session, Message: contracts.MessageEnvelope{
		SchemaVersion: 1, MessageID: id, OperationID: "op_" + id,
		Sender:    contracts.Principal{Kind: contracts.PrincipalSession, ID: "ses_sender", Name: "planner"},
		Recipient: contracts.Address{Kind: contracts.AddrSession, ID: session},
		Mode:      contracts.ModeQueue, Body: contracts.MessageBody{Text: "body of " + id},
	}}
	if mut != nil {
		mut(&d)
	}
	if _, _, err := e.inbox.Put(context.Background(), d, time.Now()); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) state(id string) inbox.Message {
	e.t.Helper()
	m, err := e.inbox.Get(context.Background(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	return m
}

func states(rs []contracts.DeliveryReceipt) []string {
	var s []string
	for _, r := range rs {
		s = append(s, string(r.State))
	}
	return s
}

func TestAcceptedDeliveryReportsAttemptThenReceipt(t *testing.T) {
	e := newEnv(t)
	e.launch("ses_a", "run_1", nil)
	e.put("msg_1", "ses_a", func(d *contracts.DeliverCommand) {
		d.Message.ThreadID = "thr_1"
		d.Message.Attachments = []contracts.Attachment{{ArtifactVersionID: "ver_9"}}
	})
	e.engine.ProcessSession(context.Background(), "ses_a", false)
	m := e.state("msg_1")
	if m.State != contracts.DeliveryAccepted || m.BoundRunID != "run_1" {
		t.Fatalf("message = %+v", m)
	}
	p, _ := e.world.Proc("run_1")
	if len(p.Delivered) != 1 {
		t.Fatalf("delivered = %d", len(p.Delivered))
	}
	text := p.Delivered[0].Text
	if !strings.HasPrefix(text, "[pyre-msg:msg_1] from session:planner (thread thr_1):\nbody of msg_1") ||
		!strings.Contains(text, "pyre artifact get ver_9") {
		t.Fatalf("rendered = %q", text)
	}
	rs := e.rep.Receipts("msg_1")
	if got := strings.Join(states(rs), ","); got != "attempting,accepted" {
		t.Fatalf("receipts = %s", got)
	}
	if rs[0].AttemptID == "" || rs[0].AttemptID != rs[1].AttemptID || rs[1].Evidence != contracts.EvidenceMock || rs[1].RunID != "run_1" || rs[1].NodeID != "node_test" {
		t.Fatalf("receipt fields = %+v", rs)
	}
}

func TestHumanLeaseBlocksPasteAndStructuredPathContinues(t *testing.T) {
	e := newEnv(t)
	e.launch("ses_a", "run_1", nil)
	now := time.Now()
	e.runs.set(func() {
		e.runs.runs["run_1"].Lease = &contracts.InputLease{LeaseID: "lease_1", RunID: "run_1",
			Holder: contracts.Principal{Kind: contracts.PrincipalOwner, ID: "cli_me"}, ExpiresAt: now.Add(time.Hour)}
	})
	e.put("msg_1", "ses_a", nil)
	for i := 0; i < 3; i++ {
		e.engine.ProcessSession(context.Background(), "ses_a", true)
	}
	if e.ad.Hits() != 0 {
		t.Fatalf("adapter called %d times under a human lease", e.ad.Hits())
	}
	m := e.state("msg_1")
	if m.State != contracts.DeliveryQueued || !strings.Contains(m.Reason, "lease") || m.BoundRunID != "" {
		t.Fatalf("message under lease = %+v", m)
	}
	if rs := e.rep.Receipts("msg_1"); len(rs) != 1 || rs[0].State != contracts.DeliveryQueued {
		t.Fatalf("queued reason should be reported once: %+v", rs)
	}
	// Attached terminal also blocks raw paste.
	e.runs.set(func() { e.runs.runs["run_1"].Lease = nil; e.runs.attached["run_1"] = true })
	e.engine.ProcessSession(context.Background(), "ses_a", true)
	if e.ad.Hits() != 0 || !strings.Contains(e.state("msg_1").Reason, "attached") {
		t.Fatalf("attached client did not block: %+v", e.state("msg_1"))
	}
	// A structured path the adapter declares safe continues alongside a human.
	e.runs.set(func() {
		e.runs.runs["run_1"].Capabilities = append(e.runs.runs["run_1"].Capabilities, contracts.CapInputAppServer)
	})
	e.engine.ProcessSession(context.Background(), "ses_a", true)
	if got := e.state("msg_1").State; got != contracts.DeliveryAccepted {
		t.Fatalf("structured delivery while attached = %s", got)
	}
}

func TestLeaseReleaseThenDeliver(t *testing.T) {
	e := newEnv(t)
	e.launch("ses_a", "run_1", nil)
	e.runs.set(func() {
		e.runs.runs["run_1"].Lease = &contracts.InputLease{RunID: "run_1", Holder: contracts.Principal{Kind: contracts.PrincipalOwner, ID: "x"}, ExpiresAt: time.Now().Add(time.Hour)}
	})
	e.put("msg_1", "ses_a", nil)
	e.engine.ProcessSession(context.Background(), "ses_a", false)
	e.runs.set(func() { e.runs.runs["run_1"].Lease = nil })
	e.engine.ProcessSession(context.Background(), "ses_a", true)
	if e.state("msg_1").State != contracts.DeliveryAccepted {
		t.Fatal("not delivered after lease release")
	}
}

func TestBusyKeepsOrderAndBacksOff(t *testing.T) {
	e := newEnv(t)
	e.engine = New(Config{NodeID: "node_test", Inbox: e.inbox, Runs: e.runs, Reporter: e.rep, MinBackoff: time.Minute, MaxBackoff: time.Hour})
	e.launch("ses_a", "run_1", nil)
	e.world.Mutate("run_1", func(p *daemontest.Proc) { p.State = contracts.RunRunning })
	e.runs.set(func() { e.runs.runs["run_1"].State = contracts.RunRunning })
	e.put("msg_1", "ses_a", nil)
	e.put("msg_2", "ses_a", nil)
	e.engine.ProcessSession(context.Background(), "ses_a", false)
	m1, m2 := e.state("msg_1"), e.state("msg_2")
	if m1.State != contracts.DeliveryQueued || m1.Backoff <= 0 || m1.NextAt.IsZero() || m1.BoundRunID != "" || m1.Attempted {
		t.Fatalf("busy msg_1 = %+v", m1)
	}
	if e.ad.Hits() != 1 || m2.Reason != "" {
		t.Fatalf("msg_2 must not be attempted behind a busy head: hits=%d m2=%+v", e.ad.Hits(), m2)
	}
	// Backoff respected without force.
	e.engine.ProcessSession(context.Background(), "ses_a", false)
	if e.ad.Hits() != 1 {
		t.Fatalf("backoff not respected: hits=%d", e.ad.Hits())
	}
	// Runtime becomes idle -> kick -> both delivered in order.
	e.world.Mutate("run_1", func(p *daemontest.Proc) { p.State = contracts.RunWaitingInput })
	e.runs.set(func() { e.runs.runs["run_1"].State = contracts.RunWaitingInput })
	e.engine.ProcessSession(context.Background(), "ses_a", true)
	p, _ := e.world.Proc("run_1")
	if len(p.Delivered) != 2 || p.Delivered[0].MessageID != "msg_1" || p.Delivered[1].MessageID != "msg_2" {
		t.Fatalf("delivery order = %+v", p.Delivered)
	}
}

func TestShellNoQueueCapAndSteerUnsupported(t *testing.T) {
	e := newEnv(t)
	e.launch("ses_sh", "run_sh", []string{contracts.CapAttachNative, contracts.CapStop})
	e.put("msg_q", "ses_sh", nil)
	e.put("msg_s", "ses_sh", func(d *contracts.DeliverCommand) { d.Message.Mode = contracts.ModeSteer })
	e.engine.ProcessSession(context.Background(), "ses_sh", true)
	if e.ad.Hits() != 0 {
		t.Fatal("shell adapter must never receive typed input")
	}
	q, s := e.state("msg_q"), e.state("msg_s")
	if q.State != contracts.DeliveryQueued || !strings.Contains(q.Reason, "input.queue") {
		t.Fatalf("queue msg = %+v", q)
	}
	if s.State != contracts.DeliveryRejected || !strings.Contains(s.Reason, "unsupported") {
		t.Fatalf("steer msg = %+v", s)
	}
}

func TestCrashBetweenClaimAndReceiptBecomesIndeterminateNotReplayed(t *testing.T) {
	e := newEnv(t)
	e.launch("ses_a", "run_1", nil)
	e.put("msg_1", "ses_a", nil)
	// Crash injection: the runtime accepts the input, then the node dies
	// before the receipt commits (the finishing transaction fails).
	e.rep.FailOn = func(kind contracts.ReportKind, payload any) error {
		if r, ok := payload.(contracts.DeliveryReceipt); ok && r.State == contracts.DeliveryAccepted {
			return errors.New("simulated crash before receipt commit")
		}
		return nil
	}
	e.engine.ProcessSession(context.Background(), "ses_a", false)
	if m := e.state("msg_1"); m.State != contracts.DeliveryAttempting || m.BoundRunID != "run_1" {
		t.Fatalf("after crash = %+v", m)
	}
	if e.ad.Hits() != 1 {
		t.Fatalf("hits = %d", e.ad.Hits())
	}
	// Restart: new engine, reconcile finds no evidence.
	e.rep.FailOn = nil
	e.ad.ReconcileUnknown = true
	e.engine = e.newEngine()
	if err := e.engine.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	m := e.state("msg_1")
	if m.State != contracts.DeliveryIndeterminate || !strings.Contains(m.Reason, "no reconcilable evidence") {
		t.Fatalf("after recover = %+v", m)
	}
	// Never replayed automatically.
	for i := 0; i < 3; i++ {
		e.engine.ProcessSession(context.Background(), "ses_a", true)
	}
	e.engine.Recover(context.Background())
	if e.ad.Hits() != 1 {
		t.Fatalf("indeterminate message was replayed: hits=%d", e.ad.Hits())
	}
	rs := e.rep.Receipts("msg_1")
	if got := strings.Join(states(rs), ","); got != "attempting,indeterminate" {
		t.Fatalf("receipts = %s", got)
	}
	// Later evidence (e.g. a hook receipt) resolves it.
	e.engine.ApplyEvidence(context.Background(), []contracts.DeliveryReceipt{{MessageID: "msg_1", RunID: "run_1", State: contracts.DeliveryAccepted, Evidence: contracts.EvidenceClaudePromptHook, EvidenceRef: "hook:1"}})
	if m := e.state("msg_1"); m.State != contracts.DeliveryAccepted {
		t.Fatalf("evidence did not resolve: %+v", m)
	}
	// Evidence from another run never rebinds.
	e.engine.ApplyEvidence(context.Background(), []contracts.DeliveryReceipt{{MessageID: "msg_1", RunID: "run_other", State: contracts.DeliveryRejected}})
	if m := e.state("msg_1"); m.State != contracts.DeliveryAccepted {
		t.Fatalf("foreign evidence changed state: %+v", m)
	}
	// Consumption evidence is recorded once.
	e.engine.ApplyEvidence(context.Background(), []contracts.DeliveryReceipt{{MessageID: "msg_1", RunID: "run_1", State: contracts.DeliveryAccepted, Evidence: contracts.EvidenceClaudeUserRecord}})
	if m := e.state("msg_1"); m.ConsumedAt == nil {
		t.Fatalf("consumed not recorded: %+v", m)
	}
	rs = e.rep.Receipts("msg_1")
	if last := rs[len(rs)-1]; !last.Consumed || last.State != contracts.DeliveryAccepted {
		t.Fatalf("consumption receipt = %+v", last)
	}
}

func TestCrashWithReconcilableEvidenceIsAccepted(t *testing.T) {
	e := newEnv(t)
	e.launch("ses_a", "run_1", nil)
	e.put("msg_1", "ses_a", nil)
	e.rep.FailOn = func(kind contracts.ReportKind, payload any) error {
		if r, ok := payload.(contracts.DeliveryReceipt); ok && r.State == contracts.DeliveryAccepted {
			return errors.New("crash")
		}
		return nil
	}
	e.engine.ProcessSession(context.Background(), "ses_a", false)
	e.rep.FailOn = nil
	e.engine = e.newEngine()
	e.engine.Recover(context.Background())
	if m := e.state("msg_1"); m.State != contracts.DeliveryAccepted {
		t.Fatalf("reconciled = %+v", m)
	}
}

func TestBoundMessageNeverMovesToReplacementRun(t *testing.T) {
	e := newEnv(t)
	e.launch("ses_a", "run_old", nil)
	e.put("msg_bound", "ses_a", func(d *contracts.DeliverCommand) { d.BoundRunID = "run_old" })
	// Old run exits and is replaced.
	e.runs.set(func() { e.runs.runs["run_old"].State = contracts.RunExited })
	e.launch("ses_a", "run_new", nil)
	e.put("msg_fresh", "ses_a", nil)
	e.engine.ProcessSession(context.Background(), "ses_a", true)
	b, f := e.state("msg_bound"), e.state("msg_fresh")
	if b.State != contracts.DeliveryQueued || b.BoundRunID != "run_old" || !strings.Contains(b.Reason, "bound to run run_old") {
		t.Fatalf("bound msg = %+v", b)
	}
	if f.State != contracts.DeliveryAccepted || f.BoundRunID != "run_new" {
		t.Fatalf("fresh msg = %+v", f)
	}
	p, _ := e.world.Proc("run_new")
	for _, d := range p.Delivered {
		if d.MessageID == "msg_bound" {
			t.Fatal("bound message delivered to replacement run")
		}
	}
}

func TestExpiryAndCancel(t *testing.T) {
	e := newEnv(t)
	past := time.Now().Add(-time.Minute)
	e.put("msg_old", "ses_a", func(d *contracts.DeliverCommand) { d.Message.ExpiresAt = &past })
	e.put("msg_c", "ses_a", nil)
	e.engine.ProcessSession(context.Background(), "ses_a", true)
	if m := e.state("msg_old"); m.State != contracts.DeliveryExpired {
		t.Fatalf("expired = %+v", m)
	}
	if rs := e.rep.Receipts("msg_old"); len(rs) != 1 || rs[0].State != contracts.DeliveryExpired {
		t.Fatalf("expired receipt = %+v", rs)
	}
	m, err := e.engine.Cancel(context.Background(), "msg_c")
	if err != nil || m.State != contracts.DeliveryCancelled {
		t.Fatalf("cancel = %+v %v", m, err)
	}
	// Cancel after an attempt is refused.
	e.launch("ses_b", "run_b", nil)
	e.put("msg_d", "ses_b", nil)
	e.engine.ProcessSession(context.Background(), "ses_b", true)
	if _, err := e.engine.Cancel(context.Background(), "msg_d"); !contracts.IsCode(err, contracts.ErrConflict) {
		t.Fatalf("cancel after attempt = %v", err)
	}
}

func TestDeliverErrorWithoutEvidenceIsIndeterminate(t *testing.T) {
	e := newEnv(t)
	e.launch("ses_a", "run_1", nil)
	e.ad.ReconcileUnknown = true
	e.ad.DeliverFunc = func(p *daemontest.Proc, in contracts.DeliveryInput) (contracts.DeliveryResult, error, bool) {
		p.Delivered = append(p.Delivered, in) // runtime took it, then the call failed
		return contracts.DeliveryResult{}, contracts.Errorf(contracts.ErrUnavailable, "pane vanished mid-paste"), true
	}
	e.put("msg_1", "ses_a", nil)
	e.engine.ProcessSession(context.Background(), "ses_a", false)
	if m := e.state("msg_1"); m.State != contracts.DeliveryIndeterminate {
		t.Fatalf("state = %+v", m)
	}
	// Terminal-write evidence never counts as accepted.
	e.ad.DeliverFunc = func(p *daemontest.Proc, in contracts.DeliveryInput) (contracts.DeliveryResult, error, bool) {
		return contracts.DeliveryResult{State: contracts.DeliveryAccepted, Evidence: contracts.EvidenceTerminalWrite}, nil, true
	}
	e.put("msg_2", "ses_a", nil)
	e.engine.ProcessSession(context.Background(), "ses_a", true)
	if m := e.state("msg_2"); m.State != contracts.DeliveryIndeterminate {
		t.Fatalf("terminal-write state = %+v", m)
	}
}

func TestNoLiveRunKeepsQueuedAndRunLoop(t *testing.T) {
	e := newEnv(t)
	e.put("msg_1", "ses_a", nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.engine.Run(ctx); close(done) }()
	time.Sleep(50 * time.Millisecond)
	if m := e.state("msg_1"); m.State != contracts.DeliveryQueued || !strings.Contains(m.Reason, "no live run") {
		t.Fatalf("no-run = %+v", m)
	}
	e.launch("ses_a", "run_1", nil)
	e.engine.Kick("ses_a")
	deadline := time.Now().Add(3 * time.Second)
	for e.state("msg_1").State != contracts.DeliveryAccepted {
		if time.Now().After(deadline) {
			t.Fatal("run loop did not deliver after kick")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
}

// A20 soak: a message whose only attempt on a run was deferred (runtime
// busy, nothing submitted) and whose binding the hub then echoed back in a
// re-sent deliver command must still reach the session's replacement run
// once the old run ends; it was provably never submitted. (Before the fix
// it stayed queued forever: "bound to run … which is not live".)
func TestDeferredAttemptEchoedByHubMovesToReplacementRun(t *testing.T) {
	e := newEnv(t)
	e.launch("ses_a", "run_old", nil)
	e.world.Mutate("run_old", func(p *daemontest.Proc) { p.State = contracts.RunRunning })
	e.runs.set(func() { e.runs.runs["run_old"].State = contracts.RunRunning })
	e.put("msg_1", "ses_a", nil)
	e.engine.ProcessSession(context.Background(), "ses_a", true)
	if m := e.state("msg_1"); m.State != contracts.DeliveryQueued || m.Attempted || m.BoundRunID != "" {
		t.Fatalf("after busy deferral: %+v", m)
	}
	// The hub saw the "attempting" receipt and re-sends the deliver
	// command (e.g. after a reconnect) with that run as the binding.
	e.put("msg_1", "ses_a", func(d *contracts.DeliverCommand) { d.BoundRunID = "run_old" })
	// pyred restarts: the old run is gone; the session is resumed.
	e.runs.set(func() { e.runs.runs["run_old"].State = contracts.RunExited })
	e.launch("ses_a", "run_new", nil)
	e.engine.ProcessSession(context.Background(), "ses_a", true)
	m := e.state("msg_1")
	if m.State != contracts.DeliveryAccepted || m.BoundRunID != "run_new" {
		t.Fatalf("deferred-only message did not reach the replacement run: %+v", m)
	}
	p, _ := e.world.Proc("run_new")
	if len(p.Delivered) != 1 || p.Delivered[0].MessageID != "msg_1" {
		t.Fatalf("run_new deliveries = %+v", p.Delivered)
	}
}

// A binding the hub sent with the first copy of the message (an earlier
// attempt this node may no longer know about, e.g. after a state restore)
// is kept even if the only local attempt was deferred: the safe default
// stays "never moves to another run".
func TestHubBoundFromStartStaysBoundAfterDeferral(t *testing.T) {
	e := newEnv(t)
	e.launch("ses_a", "run_old", nil)
	e.put("msg_1", "ses_a", func(d *contracts.DeliverCommand) { d.BoundRunID = "run_old" })
	e.world.Mutate("run_old", func(p *daemontest.Proc) { p.State = contracts.RunRunning })
	e.runs.set(func() { e.runs.runs["run_old"].State = contracts.RunRunning })
	e.engine.ProcessSession(context.Background(), "ses_a", true) // deferred, stays hub-bound
	e.runs.set(func() { e.runs.runs["run_old"].State = contracts.RunExited })
	e.launch("ses_a", "run_new", nil)
	e.engine.ProcessSession(context.Background(), "ses_a", true)
	if m := e.state("msg_1"); m.State != contracts.DeliveryQueued || m.BoundRunID != "run_old" {
		t.Fatalf("hub-bound message moved: %+v", m)
	}
}

// readyAdapter adds InputReadiness to the fake adapter.
type readyAdapter struct {
	contracts.RuntimeAdapter
	mu     sync.Mutex
	ready  bool
	reason string
	checks int
}

func (r *readyAdapter) InputReady(ctx context.Context, h contracts.RunHandle, mode contracts.MessageMode) (bool, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.checks++
	return r.ready, r.reason
}

func (r *readyAdapter) set(ready bool, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ready, r.reason = ready, reason
}

// TestNotReadyRunCoalescesReceipts: a run that looks idle but cannot take
// input (live: a startup modal behind waiting_input) is retried on every
// kick. No attempt is claimed while the adapter says not ready, the backoff
// still grows, and the hub gets one queued receipt per reason change (live
// before the fix: 162 attempting/queued receipts in 3.3 minutes).
func TestNotReadyRunCoalescesReceipts(t *testing.T) {
	e := newEnv(t)
	ra := &readyAdapter{reason: "runtime not ready for input: starting (launching; no SessionStart hook yet)"}
	e.runs.wrap = func(a contracts.RuntimeAdapter) contracts.RuntimeAdapter { ra.RuntimeAdapter = a; return ra }
	e.launch("ses_a", "run_1", nil)
	e.put("msg_1", "ses_a", nil)
	for i := 0; i < 80; i++ { // one forced retry per observation for minutes
		e.engine.ProcessSession(context.Background(), "ses_a", true)
	}
	if e.ad.Hits() != 0 {
		t.Fatalf("adapter Deliver called %d times while not ready", e.ad.Hits())
	}
	m := e.state("msg_1")
	if m.State != contracts.DeliveryQueued || m.Attempted || m.BoundRunID != "" || m.Backoff != 50*time.Millisecond {
		t.Fatalf("not-ready message = %+v", m)
	}
	if got := strings.Join(states(e.rep.Receipts("msg_1")), ","); got != "queued" {
		t.Fatalf("receipts while not ready = %s", got)
	}
	// Without force the backoff is respected (no readiness check either).
	n := ra.checks
	e.engine.ProcessSession(context.Background(), "ses_a", false)
	if ra.checks != n {
		t.Fatal("backoff not respected")
	}
	// A new reason is one new receipt.
	ra.set(false, "dialog on screen: Claude in Chrome")
	for i := 0; i < 5; i++ {
		e.engine.ProcessSession(context.Background(), "ses_a", true)
	}
	if got := strings.Join(states(e.rep.Receipts("msg_1")), ","); got != "queued,queued" {
		t.Fatalf("receipts after a reason change = %s", got)
	}
	// Ready: one attempt, accepted.
	ra.set(true, "")
	e.engine.ProcessSession(context.Background(), "ses_a", true)
	if got := strings.Join(states(e.rep.Receipts("msg_1")), ","); got != "queued,queued,attempting,accepted" || e.ad.Hits() != 1 {
		t.Fatalf("receipts after ready = %s hits=%d", got, e.ad.Hits())
	}
}
