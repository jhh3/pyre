package delivery

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/daemon/daemontest"
)

// noticeAdapter records the engine's in-turn doorbell reports.
type noticeAdapter struct {
	contracts.RuntimeAdapter
	mu    sync.Mutex
	calls [][]string
}

func (n *noticeAdapter) NotifyQueuedMail(ctx context.Context, h contracts.RunHandle, mail []contracts.QueuedMail) {
	n.mu.Lock()
	defer n.mu.Unlock()
	var ids []string
	for _, m := range mail {
		ids = append(ids, m.MessageID)
	}
	n.calls = append(n.calls, ids)
}

func (n *noticeAdapter) last() (int, []string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.calls) == 0 {
		return 0, nil
	}
	return len(n.calls), n.calls[len(n.calls)-1]
}

func (e *env) putAt(id string, at time.Time, mut func(*contracts.DeliverCommand)) {
	e.t.Helper()
	d := contracts.DeliverCommand{SessionID: "ses_a", Message: contracts.MessageEnvelope{
		SchemaVersion: 1, MessageID: id, OperationID: "op_" + id,
		Sender:    contracts.Principal{Kind: contracts.PrincipalSession, ID: "ses_peer", Name: "integrator"},
		Recipient: contracts.Address{Kind: contracts.AddrSession, ID: "ses_a"},
		Mode:      contracts.ModeQueue, Body: contracts.MessageBody{Text: "body of " + id},
	}}
	if mut != nil {
		mut(&d)
	}
	if _, _, err := e.inbox.Put(context.Background(), d, at); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) runState(runID string, s contracts.RunState) {
	e.world.Mutate(runID, func(p *daemontest.Proc) { p.State = s })
	e.runs.set(func() { e.runs.runs[runID].State = s })
}

// input.doorbell (iss_…): while the run is busy,
// the engine reports queued mail older than MailNoticeAge to the adapter:
// not young mail, not the session's own messages, not Pyre housekeeping
// mail, not info messages, never while the run waits on an approval. The
// mail stays queued; read through the inbox it is never pasted, the rest is
// delivered at idle as before.
func TestMailNoticeWhileBusy(t *testing.T) {
	e := newEnv(t)
	na := &noticeAdapter{}
	e.runs.wrap = func(a contracts.RuntimeAdapter) contracts.RuntimeAdapter { na.RuntimeAdapter = a; return na }
	e.launch("ses_a", "run_1", append(append([]string(nil), e.ad.Caps...), contracts.CapInputDoorbell))
	e.runState("run_1", contracts.RunRunning)
	old := time.Now().Add(-2 * time.Minute)
	ctx := context.Background()

	e.putAt("msg_young", time.Now(), nil)
	e.engine.ProcessSession(ctx, "ses_a", true)
	if n, _ := na.last(); n != 0 {
		t.Fatalf("young mail reported: %v", na.calls)
	}
	e.putAt("msg_self", old, func(d *contracts.DeliverCommand) {
		d.Message.Sender = contracts.Principal{Kind: contracts.PrincipalSession, ID: "ses_a"}
	})
	e.putAt("msg_sys", old, func(d *contracts.DeliverCommand) {
		d.Message.Sender = contracts.Principal{Kind: contracts.PrincipalSystem, ID: "stuckmail"}
	})
	e.putAt("msg_info", old, func(d *contracts.DeliverCommand) { d.Message.Mode = contracts.ModeInfo })
	e.engine.ProcessSession(ctx, "ses_a", true)
	if n, _ := na.last(); n != 0 {
		t.Fatalf("own, system or info mail reported: %v", na.calls)
	}
	e.putAt("msg_old", old, nil)
	e.putAt("msg_digest", old, func(d *contracts.DeliverCommand) {
		d.Message.Sender = contracts.Principal{Kind: contracts.PrincipalSystem, ID: "child-reports"}
	})
	e.engine.ProcessSession(ctx, "ses_a", true)
	if n, ids := na.last(); n != 1 || strings.Join(ids, ",") != "msg_old,msg_digest" {
		t.Fatalf("reported %v", na.calls)
	}
	for _, id := range []string{"msg_old", "msg_digest", "msg_young", "msg_self"} {
		if m := e.state(id); m.State != contracts.DeliveryQueued || m.Attempted {
			t.Fatalf("%s changed by the doorbell: %+v", id, m)
		}
	}

	// Waiting on an approval: no report.
	e.runState("run_1", contracts.RunWaitingApproval)
	e.engine.ProcessSession(ctx, "ses_a", true)
	if n, _ := na.last(); n != 1 {
		t.Fatalf("reported with an approval pending: %v", na.calls)
	}

	// The agent reads msg_old through the inbox: no longer reported.
	e.runState("run_1", contracts.RunRunning)
	if _, err := e.engine.ApplyRead(ctx, "msg_old", "", contracts.EvidencePyreRead, ""); err != nil {
		t.Fatal(err)
	}
	e.engine.ProcessSession(ctx, "ses_a", true)
	if _, ids := na.last(); strings.Join(ids, ",") != "msg_digest" {
		t.Fatalf("read mail still reported: %v", na.calls)
	}

	// Idle: everything else is pasted in order, the read mail never.
	e.runState("run_1", contracts.RunWaitingInput)
	before, _ := na.last()
	for range 5 {
		e.engine.ProcessSession(ctx, "ses_a", true)
	}
	p, _ := e.world.Proc("run_1")
	var got []string
	for _, d := range p.Delivered {
		got = append(got, d.MessageID)
	}
	if strings.Join(got, ",") != "msg_young,msg_self,msg_sys,msg_digest" {
		t.Fatalf("idle delivery = %v", got)
	}
	if n, _ := na.last(); n != before {
		t.Fatalf("reported while idle: %v", na.calls)
	}
}

// Without input.doorbell (or with MailNoticeAge negative) the engine never
// reports.
func TestMailNoticeNeedsCapability(t *testing.T) {
	for _, tc := range []struct {
		name string
		caps bool
		age  time.Duration
	}{{"no capability", false, 0}, {"disabled", true, -1}} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.engine = New(Config{NodeID: "node_test", Inbox: e.inbox, Runs: e.runs, Reporter: e.rep,
				MinBackoff: 10 * time.Millisecond, MaxBackoff: 50 * time.Millisecond, MailNoticeAge: tc.age})
			na := &noticeAdapter{}
			e.runs.wrap = func(a contracts.RuntimeAdapter) contracts.RuntimeAdapter { na.RuntimeAdapter = a; return na }
			var caps []string
			if tc.caps {
				caps = append(append([]string(nil), e.ad.Caps...), contracts.CapInputDoorbell)
			}
			e.launch("ses_a", "run_1", caps)
			e.runState("run_1", contracts.RunRunning)
			e.putAt("msg_1", time.Now().Add(-time.Hour), nil)
			e.engine.ProcessSession(context.Background(), "ses_a", true)
			if n, _ := na.last(); n != 0 {
				t.Fatalf("reported: %v", na.calls)
			}
		})
	}
}
