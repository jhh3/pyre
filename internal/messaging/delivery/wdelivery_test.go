package delivery

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// claudeLike wraps the fake adapter like the Claude TUI adapter: a doorbell
// that records when it told the agent about a message, and an optional
// paste (Deliver) / Reconcile override.
type claudeLike struct {
	contracts.RuntimeAdapter
	mu        sync.Mutex
	notified  map[string]time.Time
	deliver   func(ctx context.Context, h contracts.RunHandle, in contracts.DeliveryInput) (contracts.DeliveryResult, error)
	reconcile func(messageID string) contracts.DeliveryResult
	started   chan string
}

func (c *claudeLike) DoorbellNotifiedAt(h contracts.RunHandle, id string) (time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.notified[id]
	return t, ok
}

func (c *claudeLike) Deliver(ctx context.Context, h contracts.RunHandle, in contracts.DeliveryInput) (contracts.DeliveryResult, error) {
	if c.started != nil {
		c.started <- in.MessageID
	}
	if c.deliver != nil {
		return c.deliver(ctx, h, in)
	}
	return c.RuntimeAdapter.Deliver(ctx, h, in)
}

func (c *claudeLike) Reconcile(ctx context.Context, h contracts.RunHandle, id string) (contracts.DeliveryResult, error) {
	if c.reconcile != nil {
		return c.reconcile(id), nil
	}
	return c.RuntimeAdapter.Reconcile(ctx, h, id)
}

func newClaudeEnv(t *testing.T) (*env, *claudeLike) {
	e := newEnv(t)
	c := &claudeLike{notified: map[string]time.Time{}}
	e.runs.wrap = func(a contracts.RuntimeAdapter) contracts.RuntimeAdapter { c.RuntimeAdapter = a; return c }
	e.launch("ses_a", "run_1", nil)
	return e, c
}

// The recipient read the message through the Pyre MCP (the hub's read
// withdrawal): it is accepted with pyre.read and never pasted afterwards.
func TestReadViaMCPCancelsPaste(t *testing.T) {
	e, _ := newClaudeEnv(t)
	e.put("msg_1", "ses_a", nil)
	m, err := e.engine.ApplyRead(context.Background(), "msg_1", "run_1", contracts.EvidencePyreRead, "read by the recipient session (mcp get_message); not pasted")
	if err != nil || m.State != contracts.DeliveryAccepted || m.BoundRunID != "run_1" || m.ConsumedAt == nil {
		t.Fatalf("after read: %+v %v", m, err)
	}
	for i := 0; i < 3; i++ {
		e.engine.ProcessSession(context.Background(), "ses_a", true)
	}
	if e.ad.Hits() != 0 {
		t.Fatalf("read message was pasted (%d Deliver calls)", e.ad.Hits())
	}
	rs := e.rep.Receipts("msg_1")
	if len(rs) != 1 || rs[0].State != contracts.DeliveryAccepted || rs[0].Evidence != contracts.EvidencePyreRead || !rs[0].Consumed || rs[0].RunID != "run_1" {
		t.Fatalf("receipts = %+v", rs)
	}
	// A repeated withdrawal (hub resend) is a no-op.
	if m, _ := e.engine.ApplyRead(context.Background(), "msg_1", "run_1", contracts.EvidencePyreRead, ""); m.State != contracts.DeliveryAccepted || len(e.rep.Receipts("msg_1")) != 1 {
		t.Fatalf("repeat read: %+v receipts=%d", m, len(e.rep.Receipts("msg_1")))
	}
}

// After a pyred restart, mail that was already waiting and that the doorbell
// announced long ago is not pasted (no backlog flood) and does not block
// newer mail; recently announced backlog and new mail are delivered.
func TestBackgroundBacklogNotFlushedOnRestart(t *testing.T) {
	e, c := newClaudeEnv(t)
	put := func(id string, at time.Time) {
		d := contracts.DeliverCommand{SessionID: "ses_a", Message: contracts.MessageEnvelope{SchemaVersion: 1, MessageID: id, OperationID: "op_" + id,
			Sender: contracts.Principal{Kind: contracts.PrincipalSession, ID: "ses_sender"}, Recipient: contracts.Address{Kind: contracts.AddrSession, ID: "ses_a"},
			Mode: contracts.ModeQueue, Body: contracts.MessageBody{Text: "body of " + id}}}
		if _, _, err := e.inbox.Put(context.Background(), d, at); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	// Queued while the run sat in waiting_on_background behind a human
	// attach; the doorbell told the agent 25 minutes ago.
	put("msg_stale1", now.Add(-25*time.Minute))
	put("msg_stale2", now.Add(-20*time.Minute))
	put("msg_recent", now.Add(-2*time.Minute))
	c.notified["msg_stale1"] = now.Add(-25 * time.Minute)
	c.notified["msg_stale2"] = now.Add(-20 * time.Minute)
	c.notified["msg_recent"] = now.Add(-2 * time.Minute)
	// pyred restarts: a new engine.
	e.engine = e.newEngine()
	put("msg_new", time.Now().Add(time.Second))
	e.engine.ProcessSession(context.Background(), "ses_a", true)
	for _, id := range []string{"msg_stale1", "msg_stale2"} {
		m := e.state(id)
		if m.State != contracts.DeliveryQueued || m.Attempted || !strings.Contains(m.Reason, "stale backlog") {
			t.Fatalf("%s = %+v", id, m)
		}
	}
	if m := e.state("msg_recent"); m.State != contracts.DeliveryAccepted {
		t.Fatalf("recent backlog = %+v", m)
	}
	if m := e.state("msg_new"); m.State != contracts.DeliveryAccepted {
		t.Fatalf("new mail blocked behind stale backlog: %+v", m)
	}
	if e.ad.Hits() != 2 {
		t.Fatalf("Deliver calls = %d, want 2", e.ad.Hits())
	}
	// Stays parked on later passes (one queued receipt, no attempt), and the
	// agent's later inbox read settles it.
	for i := 0; i < 3; i++ {
		e.engine.ProcessSession(context.Background(), "ses_a", true)
	}
	if got := strings.Join(states(e.rep.Receipts("msg_stale1")), ","); got != "queued" || e.ad.Hits() != 2 {
		t.Fatalf("stale receipts = %s hits=%d", got, e.ad.Hits())
	}
	if m, _ := e.engine.ApplyRead(context.Background(), "msg_stale1", "", contracts.EvidencePyreRead, ""); m.State != contracts.DeliveryAccepted {
		t.Fatalf("read of parked mail = %+v", m)
	}
}

// Shutdown with a paste in flight: the drain lets it finish and starts no
// new attempt.
func TestShutdownDrainLetsInflightPasteFinish(t *testing.T) {
	e, c := newClaudeEnv(t)
	c.started = make(chan string, 4)
	release := make(chan struct{})
	c.deliver = func(ctx context.Context, h contracts.RunHandle, in contracts.DeliveryInput) (contracts.DeliveryResult, error) {
		<-release // the paste is still being verified
		return c.RuntimeAdapter.Deliver(ctx, h, in)
	}
	e.put("msg_1", "ses_a", nil)
	done := make(chan struct{})
	go func() { e.engine.ProcessSession(context.Background(), "ses_a", true); close(done) }()
	<-c.started
	drained := make(chan int)
	go func() { drained <- e.engine.Drain(5 * time.Second) }()
	time.Sleep(50 * time.Millisecond)
	select {
	case n := <-drained:
		t.Fatalf("drain returned with a paste in flight (%d)", n)
	default:
	}
	close(release)
	if n := <-drained; n != 0 {
		t.Fatalf("drain left %d in flight", n)
	}
	<-done
	if m := e.state("msg_1"); m.State != contracts.DeliveryAccepted {
		t.Fatalf("in-flight paste = %+v", m)
	}
	// Draining: no new attempt is claimed.
	e.put("msg_2", "ses_a", nil)
	e.engine.ProcessSession(context.Background(), "ses_a", true)
	if m := e.state("msg_2"); m.State != contracts.DeliveryQueued || m.Attempted || e.ad.Hits() != 1 {
		t.Fatalf("attempt during drain: %+v hits=%d", m, e.ad.Hits())
	}
}

// A paste the shutdown cuts anyway: when the screen proves it was not
// submitted it stays queued (no human needed); only an unknown outcome is
// indeterminate, with a "pyred shutdown" reason.
func TestShutdownCutPasteResolvedWithoutHuman(t *testing.T) {
	cut := func(t *testing.T, rec contracts.DeliveryResult) (*env, inboxState) {
		e, c := newClaudeEnv(t)
		c.started = make(chan string, 1)
		c.deliver = func(ctx context.Context, h contracts.RunHandle, in contracts.DeliveryInput) (contracts.DeliveryResult, error) {
			<-ctx.Done()
			return contracts.DeliveryResult{State: contracts.DeliveryIndeterminate, Evidence: contracts.EvidenceNone,
				Detail: "pasted text not recognised in the input box; Enter not pressed (screen unavailable: tmux capture-pane: context canceled)"}, nil
		}
		c.reconcile = func(string) contracts.DeliveryResult { return rec }
		e.put("msg_1", "ses_a", nil)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { e.engine.ProcessSession(ctx, "ses_a", true); close(done) }()
		<-c.started
		if n := e.engine.Drain(30 * time.Millisecond); n != 1 {
			t.Fatalf("drain = %d, want the stuck paste", n)
		}
		cancel() // the daemon gives up on it
		<-done
		m := e.state("msg_1")
		return e, inboxState{m.State, m.Reason, m.Attempted, m.BoundRunID}
	}
	t.Run("not submitted stays queued", func(t *testing.T) {
		e, s := cut(t, contracts.DeliveryResult{State: contracts.DeliveryRejected, Evidence: contracts.EvidenceNone, Detail: "Enter was never pressed and the input box is empty: not submitted"})
		if s.state != contracts.DeliveryQueued || s.attempted || s.bound != "" || !strings.Contains(s.reason, "pyred shutdown") {
			t.Fatalf("cut paste = %+v", s)
		}
		if got := strings.Join(states(e.rep.Receipts("msg_1")), ","); got != "attempting,queued" {
			t.Fatalf("receipts = %s", got)
		}
		// The next pyred delivers it normally.
		e.engine = e.newEngine()
		e.runs.wrap = nil
		e.engine.ProcessSession(context.Background(), "ses_a", true)
		if m := e.state("msg_1"); m.State != contracts.DeliveryAccepted {
			t.Fatalf("after restart = %+v", m)
		}
	})
	t.Run("unknown outcome is indeterminate with the reason", func(t *testing.T) {
		_, s := cut(t, contracts.DeliveryResult{State: contracts.DeliveryIndeterminate, Evidence: contracts.EvidenceNone, Detail: "draft visible but a human is attached"})
		if s.state != contracts.DeliveryIndeterminate || !strings.Contains(s.reason, "pyred shutdown cut the delivery") || !strings.Contains(s.reason, "human is attached") {
			t.Fatalf("cut paste = %+v", s)
		}
	})
}

type inboxState struct {
	state     contracts.RuntimeDeliveryState
	reason    string
	attempted bool
	bound     string
}
