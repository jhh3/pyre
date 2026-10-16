package cli

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
)

// stuckAPI serves one message and its recipient session (an older hub:
// no delivery_status), so the client derives the status itself.
type stuckAPI struct {
	c.API
	mu   sync.Mutex
	msg  c.MessageView
	sess c.SessionView
	gets int
}

func (f *stuckAPI) GetMessage(context.Context, c.GetRequest) (c.MessageView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets++
	return f.msg, nil
}

func (f *stuckAPI) GetSession(context.Context, c.GetRequest) (c.SessionView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sess, nil
}

func stuckFixture(state c.RunState, reason string) *stuckAPI {
	stored := time.Now().Add(-40 * time.Minute)
	return &stuckAPI{
		msg: c.MessageView{
			Envelope: c.MessageEnvelope{MessageID: "msg_synth-stuck", Recipient: c.Address{Kind: c.AddrSession, ID: "ses_synth-lane"}, Mode: c.ModeQueue},
			Status:   c.DeliveryStatus{Storage: c.StorageStored, StoredAt: &stored, Runtime: c.DeliveryQueued, Ack: c.AckNone, UpdatedAt: stored},
		},
		sess: c.SessionView{Session: c.Session{SessionID: "ses_synth-lane", CurrentRunID: "run_synth-1"}, NodeName: "synth-server",
			Run: &c.Run{RunID: "run_synth-1", State: state, StateReason: reason}},
	}
}

func TestWaitReplyReturnsEarlyWhenRecipientEnded(t *testing.T) {
	api := stuckFixture(c.RunWaitingInput, "thread idle")
	o := &Ops{API: api, PollInterval: 10 * time.Millisecond}
	go func() {
		time.Sleep(50 * time.Millisecond)
		api.mu.Lock()
		api.sess.Run.State = c.RunExited
		api.mu.Unlock()
	}()
	start := time.Now()
	res, err := o.WaitReply(context.Background(), WaitReplyParams{MessageID: "msg_synth-stuck", TimeoutSeconds: 30})
	if !c.IsCode(err, c.ErrConflict) || time.Since(start) > 10*time.Second {
		t.Fatalf("want an early conflict, got %v after %s", err, time.Since(start))
	}
	if !strings.Contains(err.Error(), "held: recipient session has ended") || !strings.HasPrefix(res.Delivery, "held:") ||
		res.Message.Delivery == nil || !res.Message.Delivery.Final {
		t.Fatalf("result: %v %+v", err, res)
	}
}

func TestWaitReplyTimeoutSaysWhyQueued(t *testing.T) {
	api := stuckFixture(c.RunWaitingInput, "last turn failed: Selected model is at capacity · retry 1/4 at 19:52")
	o := &Ops{API: api, PollInterval: 200 * time.Millisecond}
	res, err := o.WaitReply(context.Background(), WaitReplyParams{MessageID: "msg_synth-stuck", TimeoutSeconds: 1})
	if !c.IsCode(err, c.ErrUnavailable) {
		t.Fatalf("want a timeout, got %v", err)
	}
	want := "your message is still queued: recipient's last turn failed (model at capacity); Pyre retries at 19:52"
	if !strings.Contains(err.Error(), want) || res.Delivery == "" {
		t.Fatalf("timeout text %q lacks %q (%+v)", err, want, res)
	}
}

func TestSendNoteFlagsUndeliverable(t *testing.T) {
	api := stuckFixture(c.RunExited, "")
	api.sess.Session.Archived = true
	o := &Ops{API: api}
	m := api.msg
	o.withDelivery(context.Background(), &m, nil)
	n := sendNote(m)
	if !strings.Contains(n, "delivery: undeliverable: recipient archived") || !strings.Contains(n, "will not get through") {
		t.Fatalf("note: %q", n)
	}
	if l := factLines(m); !strings.HasPrefix(l[0], "status:        undeliverable") {
		t.Fatalf("facts: %v", l)
	}
}
