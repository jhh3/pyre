package delivery

import (
	"context"
	"strings"
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/daemon/daemontest"
)

// Decision I16: a doorbell inbox read (pull evidence, no attempt) for a
// queued message mints an attempt, records accepted exactly once, and the
// message is never pasted afterwards — also across an engine restart.
func TestPullEvidenceMintsAttemptAcceptedOnceNeverPasted(t *testing.T) {
	e := newEnv(t)
	caps := append(append([]string(nil), e.ad.Caps...), contracts.CapInputMonitor)
	e.launch("ses_a", "run_1", caps)
	e.runs.set(func() { e.runs.attached["run_1"] = true })
	e.put("msg_1", "ses_a", nil)
	e.engine.ProcessSession(context.Background(), "ses_a", true)
	m := e.state("msg_1")
	if m.State != contracts.DeliveryQueued || !strings.Contains(m.Reason, "attached") || !strings.Contains(m.Reason, "input.monitor") {
		t.Fatalf("attached with doorbell: %+v", m)
	}
	accepted := 0
	e.engine.cfg.OnAccepted = func(string, string) { accepted++ }

	read := contracts.DeliveryReceipt{MessageID: "msg_1", SessionID: "ses_a", RunID: "run_1", State: contracts.DeliveryAccepted,
		Evidence: contracts.EvidenceClaudeInboxRead, EvidenceRef: "evt_1", Detail: "doorbell: read with the pyre inbox tool"}
	e.engine.ApplyEvidence(context.Background(), []contracts.DeliveryReceipt{read})
	m = e.state("msg_1")
	if m.State != contracts.DeliveryAccepted || m.BoundRunID != "run_1" || !m.Attempted || m.ConsumedAt == nil {
		t.Fatalf("after inbox read: %+v", m)
	}
	atts, err := e.inbox.Attempts(context.Background(), "msg_1")
	if err != nil || len(atts) != 1 || atts[0].State != contracts.DeliveryAccepted || atts[0].Evidence != contracts.EvidenceClaudeInboxRead || atts[0].RunID != "run_1" {
		t.Fatalf("attempts = %+v %v", atts, err)
	}
	rs := e.rep.Receipts("msg_1")
	var acc []contracts.DeliveryReceipt
	for _, r := range rs {
		if r.State == contracts.DeliveryAccepted {
			acc = append(acc, r)
		}
	}
	if len(acc) != 1 || acc[0].AttemptID != atts[0].AttemptID || acc[0].Evidence != contracts.EvidenceClaudeInboxRead || !acc[0].Consumed || acc[0].RunID != "run_1" {
		t.Fatalf("accepted receipts = %+v", acc)
	}
	if accepted != 1 {
		t.Fatalf("OnAccepted calls = %d", accepted)
	}

	// Detach: nothing is pasted.
	e.runs.set(func() { e.runs.attached["run_1"] = false })
	e.engine.ProcessSession(context.Background(), "ses_a", true)
	if e.ad.Hits() != 0 {
		t.Fatalf("message pasted after inbox read: hits=%d", e.ad.Hits())
	}

	// pyred restart: the adapter's observation cursor restarts, so the same
	// evidence arrives again (plus the agent's later re-read).
	e.engine = e.newEngine()
	if err := e.engine.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	again := read
	again.EvidenceRef = "evt_2"
	e.engine.ApplyEvidence(context.Background(), []contracts.DeliveryReceipt{read, again})
	e.engine.ProcessSession(context.Background(), "ses_a", true)
	if n := len(e.rep.Receipts("msg_1")); n != len(rs) {
		t.Fatalf("re-applied evidence produced %d new receipts", n-len(rs))
	}
	if atts, _ := e.inbox.Attempts(context.Background(), "msg_1"); len(atts) != 1 {
		t.Fatalf("attempts after restart = %d", len(atts))
	}
	if e.ad.Hits() != 0 {
		t.Fatalf("hits after restart = %d", e.ad.Hits())
	}
}

func TestPullEvidenceGuards(t *testing.T) {
	e := newEnv(t)
	e.launch("ses_a", "run_1", nil)
	e.launch("ses_b", "run_b", nil)
	e.runs.set(func() { e.runs.attached["run_1"] = true })
	e.put("msg_bound", "ses_a", func(d *contracts.DeliverCommand) { d.BoundRunID = "run_old" })
	e.put("msg_other", "ses_a", nil)
	e.put("msg_info", "ses_a", func(d *contracts.DeliverCommand) { d.Message.Mode = contracts.ModeInfo })
	e.engine.ProcessSession(context.Background(), "ses_a", true) // all stay queued (attached)
	ev := func(id, session, run string, kind contracts.EvidenceCategory) contracts.DeliveryReceipt {
		return contracts.DeliveryReceipt{MessageID: id, SessionID: session, RunID: run, State: contracts.DeliveryAccepted, Evidence: kind}
	}
	e.engine.ApplyEvidence(context.Background(), []contracts.DeliveryReceipt{
		ev("msg_bound", "ses_a", "run_1", contracts.EvidenceClaudeInboxRead),       // bound to another run
		ev("msg_other", "ses_b", "run_b", contracts.EvidenceClaudeInboxRead),       // another session's run
		ev("msg_other", "ses_a", "", contracts.EvidenceClaudeInboxRead),            // no run
		ev("msg_other", "ses_a", "run_1", contracts.EvidenceClaudeMonitorNotified), // informational
		ev("msg_info", "ses_a", "run_1", contracts.EvidenceClaudeInboxRead),        // info message
	})
	for id, want := range map[string]contracts.RuntimeDeliveryState{"msg_bound": contracts.DeliveryQueued, "msg_other": contracts.DeliveryQueued, "msg_info": contracts.DeliveryQueued} {
		if m := e.state(id); m.State != want {
			t.Errorf("%s: state %s, want %s (%s)", id, m.State, want, m.Reason)
		}
	}
	if atts, _ := e.inbox.Attempts(context.Background(), "msg_other"); len(atts) != 0 {
		t.Fatalf("attempts minted by guarded evidence: %+v", atts)
	}
	// A rejected paste (known not submitted) later read by the agent: facts
	// beat the earlier outcome.
	e.runs.set(func() { e.runs.attached["run_1"] = false })
	e.ad.DeliverFunc = func(p *daemontest.Proc, in contracts.DeliveryInput) (contracts.DeliveryResult, error, bool) {
		return contracts.DeliveryResult{State: contracts.DeliveryRejected, Evidence: contracts.EvidenceNone, Detail: "not submitted"}, nil, true
	}
	e.put("msg_rej", "ses_a", nil)
	e.engine.ProcessSession(context.Background(), "ses_a", true)
	if m := e.state("msg_rej"); m.State != contracts.DeliveryRejected {
		t.Fatalf("setup: %+v", m)
	}
	e.engine.ApplyEvidence(context.Background(), []contracts.DeliveryReceipt{ev("msg_rej", "ses_a", "run_1", contracts.EvidenceClaudeInboxRead)})
	if m := e.state("msg_rej"); m.State != contracts.DeliveryAccepted || m.BoundRunID != "run_1" {
		t.Fatalf("rejected then read: %+v", m)
	}
	if atts, _ := e.inbox.Attempts(context.Background(), "msg_rej"); len(atts) != 2 {
		t.Fatalf("attempts = %d (want the rejected paste plus the minted read)", len(atts))
	}
}

// A message whose paste attempt is in flight (attempting) resolves through
// the existing attempt; no second attempt is minted.
func TestPullEvidenceDuringAttemptUsesThatAttempt(t *testing.T) {
	e := newEnv(t)
	e.launch("ses_a", "run_1", nil)
	e.ad.DeliverFunc = func(p *daemontest.Proc, in contracts.DeliveryInput) (contracts.DeliveryResult, error, bool) {
		return contracts.DeliveryResult{State: contracts.DeliveryIndeterminate, Evidence: contracts.EvidenceNone, Detail: "no evidence yet"}, nil, true
	}
	e.put("msg_1", "ses_a", nil)
	e.engine.ProcessSession(context.Background(), "ses_a", false)
	e.engine.ApplyEvidence(context.Background(), []contracts.DeliveryReceipt{{MessageID: "msg_1", RunID: "run_1", State: contracts.DeliveryAccepted, Evidence: contracts.EvidenceClaudeInboxRead}})
	if m := e.state("msg_1"); m.State != contracts.DeliveryAccepted {
		t.Fatalf("state = %+v", m)
	}
	if atts, _ := e.inbox.Attempts(context.Background(), "msg_1"); len(atts) != 1 || atts[0].State != contracts.DeliveryAccepted {
		t.Fatalf("attempts = %+v", atts)
	}
}

func TestInformationalEvidenceNeverAccepts(t *testing.T) {
	e := newEnv(t)
	e.launch("ses_a", "run_1", nil)
	e.ad.DeliverFunc = func(p *daemontest.Proc, in contracts.DeliveryInput) (contracts.DeliveryResult, error, bool) {
		return contracts.DeliveryResult{State: contracts.DeliveryAccepted, Evidence: contracts.EvidenceClaudeMonitorNotified}, nil, true
	}
	e.put("msg_1", "ses_a", nil)
	e.engine.ProcessSession(context.Background(), "ses_a", false)
	if m := e.state("msg_1"); m.State != contracts.DeliveryIndeterminate {
		t.Fatalf("monitor notification counted as acceptance: %+v", m)
	}
}

func TestGateNoteOnlyWithInputMonitor(t *testing.T) {
	e := newEnv(t)
	e.launch("ses_a", "run_1", nil)
	e.runs.set(func() { e.runs.attached["run_1"] = true })
	e.put("msg_1", "ses_a", nil)
	e.engine.ProcessSession(context.Background(), "ses_a", true)
	if m := e.state("msg_1"); strings.Contains(m.Reason, "input.monitor") {
		t.Fatalf("doorbell note without the capability: %q", m.Reason)
	}
}
