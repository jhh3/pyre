package assemble

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/client"
	"github.com/jhh3/pyre/internal/conformance"
	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/events"
	"github.com/jhh3/pyre/internal/httpapi"
	"github.com/jhh3/pyre/internal/hub"
	"github.com/jhh3/pyre/internal/transport"
)

// ---- deterministic clock ----

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (k *testClock) Now() time.Time          { k.mu.Lock(); defer k.mu.Unlock(); return k.t }
func (k *testClock) Advance(d time.Duration) { k.mu.Lock(); k.t = k.t.Add(d); k.mu.Unlock() }

// ---- simulated node daemon ----

var mockCaps = []string{
	c.CapAttachNative, c.CapAttachReadOnly, c.CapInputQueue, c.CapInputReconcile, c.CapApprovals, c.CapQuestions,
	c.CapInterrupt, c.CapStop, c.CapResumeNative, c.CapResumeHandoff, c.CapTranscript, c.CapScreen,
}

type pendingDelivery struct {
	msgID, sessionID, boundRun string
}

// simNode is a contracts.NodeChannel plus an asynchronous node daemon: it
// journals and acknowledges every command, launches runs, answers commands
// and, on SettleDelivery, reports runtime acceptance for delivered mail.
type simNode struct {
	t      *testing.T
	stack  *Stack
	clock  *testClock
	nodeID string

	mu        sync.Mutex
	cond      *sync.Cond
	connected bool
	queue     []c.NodeCommand
	busy      bool
	seq       uint64
	runs      map[string]c.Run // by run ID
	current   map[string]string
	pending   []pendingDelivery
	lastCmd   int64
	onAction  func(c.NodeActionRequest) (any, error) // optional node action handler
	failStart func(c.StartRunCommand) *c.Error       // optional: a non-nil error fails the launch
	lastText  func(sessionID string) string          // optional: the run's last assistant message (transcript reads)
	stop      bool
	done      chan struct{}
}

func newSimNode(t *testing.T, k *testClock) *simNode {
	n := &simNode{t: t, clock: k, runs: map[string]c.Run{}, current: map[string]string{}, done: make(chan struct{})}
	n.cond = sync.NewCond(&n.mu)
	go n.loop()
	return n
}

// shutdown drains and stops the daemon before the hub closes.
func (n *simNode) shutdown() {
	n.idle()
	n.mu.Lock()
	n.stop = true
	n.cond.Broadcast()
	n.mu.Unlock()
	<-n.done
}

func (n *simNode) Send(ctx context.Context, nodeID string, cmd c.NodeCommand) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if !n.connected || nodeID != n.nodeID {
		return c.Errorf(c.ErrUnavailable, "not connected")
	}
	n.queue = append(n.queue, cmd)
	n.cond.Broadcast()
	return nil
}

func (n *simNode) Connected(nodeID string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.connected && nodeID == n.nodeID
}

func (n *simNode) loop() {
	defer close(n.done)
	for {
		n.mu.Lock()
		for len(n.queue) == 0 && !n.stop {
			n.busy = false
			n.cond.Broadcast()
			n.cond.Wait()
		}
		if n.stop {
			n.mu.Unlock()
			return
		}
		cmd := n.queue[0]
		n.queue = n.queue[1:]
		n.busy = true
		n.mu.Unlock()
		n.handle(cmd)
	}
}

// idle waits until every queued command has been handled.
func (n *simNode) idle() {
	n.mu.Lock()
	defer n.mu.Unlock()
	for (len(n.queue) > 0 || n.busy) && !n.stop {
		n.cond.Wait()
	}
}

func (n *simNode) ctx() context.Context {
	return c.WithPrincipal(context.Background(), c.Principal{Kind: c.PrincipalNode, ID: n.nodeID})
}

func (n *simNode) report(kind c.ReportKind, payload any) error {
	n.mu.Lock()
	n.seq++
	seq := n.seq
	n.mu.Unlock()
	b, _ := json.Marshal(payload)
	_, err := n.stack.Hub.HandleReport(n.ctx(), n.nodeID, c.NodeReport{OriginSeq: seq, Kind: kind, At: n.clock.Now(), Payload: b})
	return err
}

func (n *simNode) result(cmd c.NodeCommand, outcome c.CommandOutcome, res any) {
	var raw c.RawJSON
	if res != nil {
		raw, _ = json.Marshal(res)
	}
	if err := n.report(c.ReportCommandResult, c.CommandResult{CommandID: cmd.CommandID, Kind: cmd.Kind, Outcome: outcome, Result: raw, At: n.clock.Now()}); err != nil {
		n.t.Errorf("sim result: %v", err)
	}
}

func (n *simNode) handle(cmd c.NodeCommand) {
	if err := n.stack.Hub.HandleCommandAck(n.ctx(), n.nodeID, c.CommandAck{CommandID: cmd.CommandID, Seq: cmd.Seq}); err != nil {
		n.t.Errorf("sim ack: %v", err)
	}
	n.mu.Lock()
	if cmd.Seq > n.lastCmd {
		n.lastCmd = cmd.Seq
	}
	n.mu.Unlock()
	switch cmd.Kind {
	case c.CmdStartRun, c.CmdResume:
		var sr c.StartRunCommand
		json.Unmarshal(cmd.Payload, &sr)
		n.mu.Lock()
		fail := n.failStart
		n.mu.Unlock()
		if fail != nil {
			if e := fail(sr); e != nil {
				if err := n.report(c.ReportCommandResult, c.CommandResult{CommandID: cmd.CommandID, Kind: cmd.Kind, Outcome: c.OutcomeFailed, Error: e, At: n.clock.Now()}); err != nil {
					n.t.Errorf("sim result: %v", err)
				}
				return
			}
		}
		run := c.Run{RunID: sr.RunID, SessionID: sr.Session.SessionID, NodeID: n.nodeID, Adapter: "mock", Harness: sr.Session.Harness, Cwd: sr.Cwd,
			State: c.RunWaitingInput, StartedAt: n.clock.Now(), Capabilities: mockCaps}
		if sr.PreviousRun != nil {
			run.PreviousRunID = sr.PreviousRun.RunID
			run.ContinuationKind = c.ContinuationNativeResume
		}
		n.mu.Lock()
		n.runs[run.RunID] = run
		n.current[run.SessionID] = run.RunID
		n.mu.Unlock()
		n.result(cmd, c.OutcomeSucceeded, run)
	case c.CmdDeliver:
		var dc c.DeliverCommand
		json.Unmarshal(cmd.Payload, &dc)
		n.mu.Lock()
		n.pending = append(n.pending, pendingDelivery{msgID: dc.Message.MessageID, sessionID: dc.SessionID, boundRun: dc.BoundRunID})
		n.mu.Unlock()
		n.result(cmd, c.OutcomeSucceeded, nil)
	case c.CmdCancelDelivery:
		var p struct {
			MessageID string `json:"message_id"`
		}
		json.Unmarshal(cmd.Payload, &p)
		n.mu.Lock()
		kept := n.pending[:0]
		for _, d := range n.pending {
			if d.msgID != p.MessageID {
				kept = append(kept, d)
			}
		}
		n.pending = kept
		n.mu.Unlock()
		n.result(cmd, c.OutcomeSucceeded, nil)
	case c.CmdReadTranscript:
		var rr c.ReadSessionRequest
		json.Unmarshal(cmd.Payload, &rr)
		n.mu.Lock()
		last := n.lastText
		n.mu.Unlock()
		entries := []c.TranscriptEntry{}
		if last != nil {
			if text := last(rr.SessionID); text != "" {
				entries = append(entries, c.TranscriptEntry{Seq: 1, RunID: rr.RunID, At: n.clock.Now(), Kind: "assistant", Text: text})
			}
		}
		n.result(cmd, c.OutcomeSucceeded, c.ReadSessionResponse{Entries: entries})
	case c.CmdAttach:
		n.result(cmd, c.OutcomeSucceeded, c.AttachDescriptor{Mechanism: c.AttachReadOnly, ReadOnly: true, ExpiresAt: n.clock.Now().Add(time.Minute)})
	case c.CmdAction:
		var ar c.NodeActionRequest
		json.Unmarshal(cmd.Payload, &ar)
		n.mu.Lock()
		fn := n.onAction
		n.mu.Unlock()
		if fn != nil {
			res, err := fn(ar)
			if err != nil {
				if rerr := n.report(c.ReportCommandResult, c.CommandResult{CommandID: cmd.CommandID, Kind: cmd.Kind, Outcome: c.OutcomeFailed, Error: c.AsError(err), At: n.clock.Now()}); rerr != nil {
					n.t.Errorf("sim result: %v", rerr)
				}
				return
			}
			n.result(cmd, c.OutcomeSucceeded, res)
			return
		}
		n.result(cmd, c.OutcomeSucceeded, c.CommandReceipt{CommandID: cmd.CommandID, Accepted: true, Outcome: c.OutcomeSucceeded, ObservedAt: n.clock.Now()})
	default:
		n.result(cmd, c.OutcomeSucceeded, c.CommandReceipt{CommandID: cmd.CommandID, Accepted: true, Outcome: c.OutcomeSucceeded, ObservedAt: n.clock.Now()})
	}
}

func (n *simNode) hello() error {
	n.mu.Lock()
	n.connected = true
	last := n.lastCmd
	var live []c.Run
	for _, r := range n.runs {
		if !r.State.Terminal() {
			live = append(live, r)
		}
	}
	n.mu.Unlock()
	h := c.NodeHello{NodeID: n.nodeID, Protocol: 1, MinProtocol: 1, PyredVersion: "0.1.0-synth", LastCommandSeq: last, Hostname: "synth-mac", Platform: "darwin", Arch: "arm64",
		Harnesses: []c.HarnessInfo{{Harness: c.HarnessMock, Version: "mock", Available: true, Capabilities: mockCaps, ProbedAt: n.clock.Now()},
			// claude: node-started headless runs (NodeStartHeadlessOnly) are claude or codex.
			{Harness: c.HarnessClaude, Version: "2.1.299-synth", Available: true, Capabilities: mockCaps, ProbedAt: n.clock.Now()}},
		Resources: c.NodeResources{CPUCount: 8, MaxRuns: 100}, LiveRuns: live}
	if _, err := n.stack.Fleet.ObserveHello(n.ctx(), n.nodeID, transport.ConnInfo{ConnID: c.NewID("conn"), Protocol: 1, ConnectedAt: n.clock.Now()}, h); err != nil {
		return err
	}
	if _, err := n.stack.Hub.HandleHello(n.ctx(), n.nodeID, h); err != nil {
		return err
	}
	cmds, err := n.stack.Hub.WelcomeCommands(n.ctx(), n.nodeID)
	if err != nil {
		return err
	}
	n.mu.Lock()
	n.queue = append(n.queue, cmds...)
	n.cond.Broadcast()
	n.mu.Unlock()
	return nil
}

// ---- conformance simulator interfaces ----

func (n *simNode) SettleDelivery(ctx context.Context) error {
	n.idle()
	n.mu.Lock()
	ds := n.pending
	n.pending = nil
	n.mu.Unlock()
	for _, d := range ds {
		run := d.boundRun
		if run == "" {
			n.mu.Lock()
			run = n.current[d.sessionID]
			n.mu.Unlock()
		}
		if err := n.report(c.ReportReceipt, c.DeliveryReceipt{MessageID: d.msgID, AttemptID: c.NewID(c.PrefixAttempt), SessionID: d.sessionID, RunID: run,
			State: c.DeliveryAccepted, Evidence: c.EvidenceMock, EvidenceRef: "mock:" + d.msgID, ObservedAt: n.clock.Now()}); err != nil {
			return err
		}
	}
	n.idle()
	return nil
}

func (n *simNode) ReportDelivery(ctx context.Context, messageID string, st c.RuntimeDeliveryState) error {
	n.idle()
	m, err := n.stack.Hub.GetMessage(n.ctx(), c.GetRequest{ID: messageID})
	if err != nil {
		return err
	}
	run := m.Status.BoundRunID
	if run == "" {
		n.mu.Lock()
		run = n.current[m.Envelope.Recipient.ID]
		n.mu.Unlock()
	}
	ev := c.EvidenceMock
	if st == c.DeliveryIndeterminate {
		ev = c.EvidenceNone
	}
	return n.report(c.ReportReceipt, c.DeliveryReceipt{MessageID: messageID, AttemptID: c.NewID(c.PrefixAttempt), SessionID: m.Envelope.Recipient.ID, RunID: run, State: st, Evidence: ev, ObservedAt: n.clock.Now()})
}

func (n *simNode) SetRunState(ctx context.Context, runID string, st c.RunState) error {
	n.idle()
	n.mu.Lock()
	r, ok := n.runs[runID]
	if ok {
		r.State = st
		n.runs[runID] = r
	}
	n.mu.Unlock()
	if !ok {
		return c.Errorf(c.ErrNotFound, "sim: unknown run %s", runID)
	}
	return n.report(c.ReportRun, r)
}

func (n *simNode) SetNodeConnected(ctx context.Context, nodeID string, up bool) error {
	if up {
		return n.hello()
	}
	n.idle()
	n.mu.Lock()
	n.connected = false
	n.mu.Unlock()
	return n.stack.Hub.HandleDisconnect(n.ctx(), n.nodeID)
}

func (n *simNode) RequestApproval(ctx context.Context, runID string, ttl time.Duration) (c.Approval, error) {
	n.idle()
	n.mu.Lock()
	r := n.runs[runID]
	n.mu.Unlock()
	a := c.Approval{ApprovalID: c.NewID(c.PrefixApproval), SessionID: r.SessionID, RunID: runID, RequestID: "toolu_" + c.NewID("r"), Kind: c.ApprovalPermission,
		Title: "Run synthetic command?", Options: []c.ApprovalOption{{ID: "allow", Label: "Allow once", Kind: "allow"}, {ID: "deny", Label: "Deny", Kind: "deny"}},
		CreatedAt: n.clock.Now(), State: c.ApprovalPending}
	if ttl > 0 {
		e := n.clock.Now().Add(ttl)
		a.ExpiresAt = &e
	}
	if err := n.report(c.ReportApproval, a); err != nil {
		return a, err
	}
	return n.stack.Hub.GetApproval(n.ctx(), c.GetRequest{ID: a.ApprovalID})
}

func (n *simNode) ResolveApprovalNatively(ctx context.Context, id, opt string) error {
	a, err := n.stack.Hub.GetApproval(n.ctx(), c.GetRequest{ID: id})
	if err != nil {
		return err
	}
	a.State = c.ApprovalResolved
	a.Resolution = &c.ApprovalResolution{OptionID: opt, Actor: c.Principal{Kind: c.PrincipalSystem, ID: "native-terminal"}, ResolvedAt: n.clock.Now(), Source: "native", Effect: "effective"}
	return n.report(c.ReportApproval, a)
}

func (n *simNode) ReportConnection(ctx context.Context, conn c.Connection) error {
	n.idle()
	return n.report(c.ReportConnections, []c.Connection{conn})
}

func (n *simNode) TrimEvents(ctx context.Context, keep int) error {
	l, err := events.Open(ctx, n.stack.DB, events.Options{Clock: n.clock, Retention: events.Retention{MaxEvents: keep}})
	if err != nil {
		return err
	}
	_, err = l.Prune(ctx)
	return err
}

func (n *simNode) AdvanceClock(d time.Duration) { n.clock.Advance(d) }

// ---- targets ----

var ownerP = c.Principal{Kind: c.PrincipalOwner, ID: "cli_conformance-setup"}

// knownExternal lists suite cases failing because of a defect in another
// work package's store, with the owner. They are skipped (visibly) unless
// PYRE_CONFORMANCE_STRICT=1, so this package's CI stays meaningful while
// the defects remain reported.
// (Empty: the WP11 oversize-chunk and WP17 message.ack defects were fixed
// in WP19.)
var knownExternal = map[string]string{}

func skipKnownExternal(t *testing.T) {
	if os.Getenv("PYRE_CONFORMANCE_STRICT") == "1" {
		return
	}
	for name, why := range knownExternal {
		if strings.HasSuffix(t.Name(), "/"+name) {
			t.Skipf("known external defect (set PYRE_CONFORMANCE_STRICT=1 to run): %s", why)
		}
	}
}

// newStack composes the full hub with a simulated, enrolled, connected node.
func newStack(t *testing.T) (*Stack, *simNode) {
	t.Helper()
	k := &testClock{t: time.Now().UTC().Truncate(time.Second)}
	n := newSimNode(t, k)
	st, err := Open(context.Background(), Config{DataDir: t.TempDir(), Clock: k, Nodes: n, HubURL: "https://hub.synth.invalid:7443",
		Hub: hub.Options{CommandWait: 2 * time.Second, ControlWait: 500 * time.Millisecond, PollInterval: 20 * time.Millisecond}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	t.Cleanup(n.shutdown) // runs first (LIFO)
	n.stack = st
	ctx := c.WithPrincipal(context.Background(), ownerP)
	tk, err := st.Fleet.CreateEnrollment(ctx, c.EnrollmentRequest{Mutation: c.Mutation{OperationID: c.NewID(c.PrefixOperation)}, Name: "synth-mac"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Enrollment.RedeemNode(context.Background(), c.NodeEnrollExchange{NodeID: tk.NodeID, Secret: tk.Secret, Hostname: "synth-mac", Platform: "darwin", Arch: "arm64"}); err != nil {
		t.Fatal(err)
	}
	n.nodeID = tk.NodeID
	if err := n.hello(); err != nil {
		t.Fatal(err)
	}
	return st, n
}

// TestConformanceHub runs the WP03 suite against *Hub in process, composed
// with the real WP04/WP11/WP17 domain stores.
func TestConformanceHub(t *testing.T) {
	conformance.Run(t, conformance.Harness{Name: "hub", New: func(t *testing.T) *conformance.Target {
		skipKnownExternal(t)
		st, n := newStack(t)
		return &conformance.Target{
			API:    st.Hub,
			As:     func(ctx context.Context, p c.Principal) context.Context { return c.WithPrincipal(ctx, p) },
			NodeID: n.nodeID,
			Sim:    n,
		}
	}})
}

// TestConformanceHubOverHTTP runs the suite through httpapi + client.
func TestConformanceHubOverHTTP(t *testing.T) {
	conformance.Run(t, conformance.Harness{Name: "hub-http", New: func(t *testing.T) *conformance.Target {
		skipKnownExternal(t)
		st, n := newStack(t)
		auth := httpapi.NewStaticTokens(nil)
		// Token "p:<kind>:<id>" authenticates as that principal (test only).
		auth.Fallback = func(tok string) (c.Principal, bool) {
			parts := strings.SplitN(tok, ":", 3)
			if len(parts) != 3 || parts[0] != "p" {
				return c.Principal{}, false
			}
			return c.Principal{Kind: c.PrincipalKind(parts[1]), ID: parts[2]}, true
		}
		srv := httptest.NewServer(httpapi.NewServer(st.Hub, auth, httpapi.Options{StreamWait: time.Second}))
		t.Cleanup(srv.Close)
		var mu sync.Mutex
		clients := map[string]*client.Client{}
		api := conformance.ByPrincipal(func(ctx context.Context) c.API {
			p, ok := c.PrincipalFrom(ctx)
			tok := "anonymous"
			if ok {
				tok = "p:" + string(p.Kind) + ":" + p.ID
			}
			mu.Lock()
			defer mu.Unlock()
			if cl, ok := clients[tok]; ok {
				return cl
			}
			cl := client.New(srv.URL, tok, client.Options{RetryBase: time.Millisecond, MaxAttempts: 2})
			clients[tok] = cl
			return cl
		})
		return &conformance.Target{
			API:    api,
			As:     func(ctx context.Context, p c.Principal) context.Context { return c.WithPrincipal(ctx, p) },
			NodeID: n.nodeID,
			Sim:    n,
		}
	}})
}
