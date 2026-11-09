package cli

import (
	"context"
	"strings"
	"testing"

	c "github.com/jhh3/pyre/internal/contracts"
)

// steerAPI records the send request and answers like the hub's steer rule
// (internal/hub/routing.go routeToRunTx): steer needs a live run with
// input.steer on a connected node.
type steerAPI struct {
	c.API
	sess     c.SessionView
	sent     []c.SendMessageRequest
	reissued []c.ReissueMessageRequest
}

func (f *steerAPI) GetSession(context.Context, c.GetRequest) (c.SessionView, error) {
	return f.sess, nil
}

func (f *steerAPI) SendMessage(_ context.Context, req c.SendMessageRequest) (c.MessageView, error) {
	f.sent = append(f.sent, req)
	st := c.DeliveryStatus{Storage: c.StorageStored, Runtime: c.DeliveryQueued, RuntimeReason: "queued for delivery to run run_synth-1 on node node_synth"}
	r := f.sess.Run
	if req.Mode == c.ModeSteer {
		switch {
		case r == nil || r.State.Terminal():
			st.Runtime, st.RuntimeReason = c.DeliveryRejected, "rejected: recipient has no live run to steer"
		case runDisconnected(r):
			st.Runtime, st.RuntimeReason = c.DeliveryRejected, "rejected: recipient node is disconnected; steering is immediate-only"
		case !c.HasCap(r.Capabilities, c.CapInputSteer):
			st.Runtime, st.RuntimeReason = c.DeliveryRejected, "unsupported: adapter does not advertise input.steer"
		}
	}
	return c.MessageView{Envelope: c.MessageEnvelope{MessageID: "msg_synth-steer", Recipient: req.Recipient, Mode: req.Mode, Body: req.Body}, Status: st}, nil
}

func TestSendSteerToClaudeGoesQueued(t *testing.T) {
	claudeCaps := []string{c.CapInputQueue, c.CapInputReconcile, c.CapInputPaste, c.CapInputMonitor}
	codexCaps := []string{c.CapInputQueue, c.CapInputSteer, c.CapInputReconcile, c.CapInputAppServer}
	agyCaps := []string{c.CapInputQueue, c.CapInputReconcile, c.CapInputPaste}
	run := func(h c.Harness, state c.RunState, caps []string) *c.Run {
		return &c.Run{RunID: "run_synth-1", NodeID: "node_synth", Harness: h, State: state, Capabilities: caps}
	}
	disc := run(c.HarnessClaude, c.RunUnknown, claudeCaps)
	disc.StateReason, disc.Freshness.Reason = "node disconnected; last observed running", "node disconnected"
	discCodex := run(c.HarnessCodex, c.RunUnknown, codexCaps)
	discCodex.StateReason, discCodex.Freshness.Reason = "node disconnected; last observed running", "node disconnected"

	for _, tc := range []struct {
		name     string
		run      *c.Run
		wantMode c.MessageMode
		wantRT   c.RuntimeDeliveryState
		wantNote string // in delivery_status.summary
		reason   string // in the hub's runtime reason
	}{
		{"claude running", run(c.HarnessClaude, c.RunRunning, claudeCaps), c.ModeQueue, c.DeliveryQueued,
			"sent as a queued message, not a steer: this Claude session does not take steer; it sees queued mail mid-turn", "queued for delivery"},
		{"claude idle", run(c.HarnessClaude, c.RunWaitingInput, claudeCaps), c.ModeQueue, c.DeliveryQueued,
			"this Claude session does not take steer; it sees queued mail mid-turn", "queued for delivery"},
		{"agy no doorbell", run(c.HarnessAgy, c.RunRunning, agyCaps), c.ModeQueue, c.DeliveryQueued,
			"this agy session does not take steer; it reads queued mail when it can take input", "queued for delivery"},
		{"codex", run(c.HarnessCodex, c.RunRunning, codexCaps), c.ModeSteer, c.DeliveryQueued, "", "queued for delivery"},
		{"no run", nil, c.ModeQueue, c.DeliveryQueued, "the recipient has no live run; it gets this when its session is resumed", "queued"},
		{"exited", run(c.HarnessClaude, c.RunExited, claudeCaps), c.ModeQueue, c.DeliveryQueued, "the recipient has no live run; it gets this when its session is resumed", "queued"},
		{"codex exited", run(c.HarnessCodex, c.RunExited, codexCaps), c.ModeQueue, c.DeliveryQueued, "the recipient has no live run; it gets this when its session is resumed", "queued"},
		{"claude disconnected", disc, c.ModeSteer, c.DeliveryRejected, "", "steering is immediate-only"},
		{"codex disconnected", discCodex, c.ModeSteer, c.DeliveryRejected, "", "steering is immediate-only"},
		{"no queue either", run(c.HarnessShell, c.RunRunning, []string{c.CapInputPaste}), c.ModeSteer, c.DeliveryRejected, "", "does not advertise input.steer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := &steerAPI{sess: c.SessionView{Session: c.Session{SessionID: "ses_synth-peer"}, Run: tc.run}}
			o := &Ops{API: api}
			mv, err := o.Send(context.Background(), SendParams{To: "ses_synth-peer", Text: "urgent: rebase on main", Steer: true})
			if err != nil {
				t.Fatal(err)
			}
			if len(api.sent) != 1 || api.sent[0].Mode != tc.wantMode || mv.Envelope.Mode != tc.wantMode {
				t.Fatalf("sent %+v, envelope mode %s; want mode %s", api.sent, mv.Envelope.Mode, tc.wantMode)
			}
			if mv.Status.Runtime != tc.wantRT || !strings.Contains(mv.Status.RuntimeReason, tc.reason) {
				t.Fatalf("status %+v; want %s with %q", mv.Status, tc.wantRT, tc.reason)
			}
			if mv.Delivery == nil {
				t.Fatal("no delivery_status")
			}
			sum := mv.Delivery.Summary
			if tc.wantNote == "" {
				if strings.Contains(sum, "not a steer") {
					t.Fatalf("summary %q claims a downgrade", sum)
				}
				return
			}
			if !strings.HasPrefix(sum, "sent as a queued message, not a steer: ") || !strings.Contains(sum, tc.wantNote) {
				t.Fatalf("summary %q; want the note %q", sum, tc.wantNote)
			}
			if note := sendNote(mv); !strings.Contains(note, tc.wantNote) {
				t.Fatalf("send_message note %q lacks %q", note, tc.wantNote)
			}
		})
	}
}

// A plain message is never touched: no recipient lookup, mode queue.
func TestSendQueueSkipsSteerLookup(t *testing.T) {
	api := &steerAPI{sess: c.SessionView{Session: c.Session{SessionID: "ses_synth-peer"}}}
	o := &Ops{API: api}
	mv, err := o.Send(context.Background(), SendParams{To: "ses_synth-peer", Text: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if api.sent[0].Mode != c.ModeQueue || strings.Contains(mv.Delivery.Summary, "not a steer") {
		t.Fatalf("plain send: %+v %+v", api.sent[0], mv.Delivery)
	}
}

func (f *steerAPI) GetMessage(context.Context, c.GetRequest) (c.MessageView, error) {
	return c.MessageView{Envelope: c.MessageEnvelope{MessageID: "msg_synth-orig", Mode: c.ModeSteer,
		Recipient: c.Address{Kind: c.AddrSession, ID: "ses_synth-peer"}}}, nil
}

func (f *steerAPI) ReissueMessage(ctx context.Context, req c.ReissueMessageRequest) (c.MessageView, error) {
	f.reissued = append(f.reissued, req)
	mode := req.Mode
	if mode == "" {
		mode = c.ModeSteer // the original's
	}
	return f.SendMessage(ctx, c.SendMessageRequest{Recipient: c.Address{Kind: c.AddrSession, ID: "ses_synth-peer"}, Mode: mode})
}

// A steer rejected while the recipient had no live run, reissued after a
// Claude session resumed, goes as queued mail with send's note; to Codex
// it stays a steer, and with no live run it goes as queued mail held for
// the next run.
func TestReissueSteerToClaudeGoesQueued(t *testing.T) {
	claudeCaps := []string{c.CapInputQueue, c.CapInputReconcile, c.CapInputPaste, c.CapInputMonitor}
	codexCaps := []string{c.CapInputQueue, c.CapInputSteer, c.CapInputReconcile, c.CapInputAppServer}
	for _, tc := range []struct {
		name     string
		run      *c.Run
		override c.MessageMode
		wantRT   c.RuntimeDeliveryState
		wantNote string
	}{
		{"claude resumed", &c.Run{RunID: "run_synth-2", Harness: c.HarnessClaude, State: c.RunWaitingInput, Capabilities: claudeCaps},
			c.ModeQueue, c.DeliveryQueued, "sent as a queued message, not a steer: this Claude session does not take steer; it sees queued mail mid-turn"},
		{"codex", &c.Run{RunID: "run_synth-2", Harness: c.HarnessCodex, State: c.RunRunning, Capabilities: codexCaps}, "", c.DeliveryQueued, ""},
		{"no run", nil, c.ModeQueue, c.DeliveryQueued, "sent as a queued message, not a steer: the recipient has no live run"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := &steerAPI{sess: c.SessionView{Session: c.Session{SessionID: "ses_synth-peer"}, Run: tc.run}}
			o := &Ops{API: api}
			mv, err := o.Reissue(context.Background(), ReissueParams{MessageID: "msg_synth-orig", AcceptDuplicateRisk: true})
			if err != nil {
				t.Fatal(err)
			}
			if len(api.reissued) != 1 || api.reissued[0].Mode != tc.override {
				t.Fatalf("reissued %+v; want mode override %q", api.reissued, tc.override)
			}
			if mv.Status.Runtime != tc.wantRT || mv.Delivery == nil {
				t.Fatalf("status %+v delivery %+v; want %s", mv.Status, mv.Delivery, tc.wantRT)
			}
			sum := mv.Delivery.Summary
			if tc.wantNote == "" {
				if strings.Contains(sum, "not a steer") {
					t.Fatalf("summary %q claims a downgrade", sum)
				}
				return
			}
			if !strings.HasPrefix(sum, tc.wantNote) || !strings.Contains(sendNote(mv), tc.wantNote) {
				t.Fatalf("summary %q; want the note %q", sum, tc.wantNote)
			}
		})
	}
}

// A steer to a session that archived itself (its run ended) goes as queued
// mail the hub holds for its next run, and the sender is told it arrives
// only after an unarchive and resume (iss_…).
func TestSendSteerToArchivedSessionGoesQueued(t *testing.T) {
	codexCaps := []string{c.CapInputQueue, c.CapInputSteer, c.CapInputReconcile, c.CapInputAppServer}
	for _, sess := range []c.Session{
		{SessionID: "ses_synth-peer", Archived: true},
		{SessionID: "ses_synth-peer", Tags: []string{c.TagArchiveWhenDone}},
	} {
		api := &steerAPI{sess: c.SessionView{Session: sess,
			Run: &c.Run{RunID: "run_synth-1", Harness: c.HarnessCodex, State: c.RunExited, StateReason: "stopped by pyre", Capabilities: codexCaps}}}
		o := &Ops{API: api}
		mv, err := o.Send(context.Background(), SendParams{To: "ses_synth-peer", Text: "final SHA pushed; recheck", Steer: true})
		if err != nil {
			t.Fatal(err)
		}
		if api.sent[0].Mode != c.ModeQueue || mv.Status.Runtime != c.DeliveryQueued {
			t.Fatalf("sent %+v status %+v; want queued mail", api.sent[0], mv.Status)
		}
		want := "the recipient has no live run and is archived; it gets this only if it is unarchived and resumed"
		if !strings.Contains(mv.Delivery.Summary, want) || !strings.Contains(sendNote(mv), want) {
			t.Fatalf("summary %q; want %q", mv.Delivery.Summary, want)
		}
	}
}
