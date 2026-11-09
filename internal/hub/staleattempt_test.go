package hub

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/messaging/store"
)

// Incident 2026-10-23 (iss_…): a message cycled
// attempting -> withdrawn unsent for 15 minutes and `pyre reissue
// --accept-duplicate-risk` was refused as "still attempting". These pin the
// reissue rules and the stale-attempt safety net.

func (x *harness) sendTo(sid, op string) string {
	x.t.Helper()
	m, err := x.h.SendMessage(as(owner), contracts.SendMessageRequest{Mutation: contracts.Mutation{OperationID: op},
		Recipient: contracts.Address{Kind: contracts.AddrSession, ID: sid}, Body: contracts.MessageBody{Text: "synthetic " + op}})
	if err != nil {
		x.t.Fatal(err)
	}
	return m.Envelope.MessageID
}

func (x *harness) reissue(op, id string, risk bool) (contracts.MessageView, error) {
	return x.h.ReissueMessage(as(owner), contracts.ReissueMessageRequest{Mutation: contracts.Mutation{OperationID: op}, MessageID: id, AcceptDuplicateRisk: risk})
}

func (x *harness) cancelsFor(nodeID, id string) int {
	n := 0
	for _, c := range x.ch.sentKinds(nodeID, contracts.CmdCancelDelivery) {
		var p map[string]string
		_ = json.Unmarshal(c.Payload, &p)
		if p["message_id"] == id {
			n++
		}
	}
	return n
}

func TestReissueAfterWithdrawnUnsentSupersedesOriginal(t *testing.T) {
	x := newHarness(t)
	x.connect(nodeMac, 0)
	v := x.start(owner, "op_synth-start", nodeMac)
	sid, run := v.Session.SessionID, v.Run.RunID

	// Never attempted: cancel first (unchanged contract).
	fresh := x.sendTo(sid, "op_synth-fresh")
	if _, err := x.reissue("op_synth-re0", fresh, true); !contracts.IsCode(err, contracts.ErrConflict) {
		t.Fatalf("reissue of a never-attempted message: %v", err)
	}

	id := x.sendTo(sid, "op_synth-msg")
	attempt := func(n string) {
		x.report(nodeMac, contracts.ReportReceipt, contracts.DeliveryReceipt{MessageID: id, AttemptID: n, RunID: run, State: contracts.DeliveryAttempting, Evidence: contracts.EvidenceNone})
	}
	withdrawn := func(n string) {
		x.report(nodeMac, contracts.ReportReceipt, contracts.DeliveryReceipt{MessageID: id, AttemptID: n, RunID: run, State: contracts.DeliveryQueued, Evidence: contracts.EvidenceNone,
			Detail: "the session's plugin did not take the message; withdrawn unsent"})
	}
	attempt("dlv_synth1")
	// Genuinely in flight and recent: refused, even accepting the risk.
	if _, err := x.reissue("op_synth-re1", id, true); !contracts.IsCode(err, contracts.ErrConflict) || !strings.Contains(err.Error(), "in flight") {
		t.Fatalf("reissue while attempting: %v", err)
	}
	withdrawn("dlv_synth1")
	attempt("dlv_synth2")
	withdrawn("dlv_synth2")
	m := x.msg(id)
	if m.Status.Runtime != contracts.DeliveryNodeReceived || m.Status.BoundRunID != "" {
		t.Fatalf("after withdrawals: %+v", m.Status)
	}
	// Withdrawn unsent is a known outcome (not delivered): no risk flag needed.
	before := x.cancelsFor(nodeMac, id)
	re, err := x.reissue("op_synth-re2", id, false)
	if err != nil {
		t.Fatal(err)
	}
	if re.Envelope.Reissues != id || re.Envelope.MessageID == id || re.Status.Runtime == contracts.DeliveryCancelled {
		t.Fatalf("reissue: %+v", re)
	}
	orig := x.msg(id)
	if orig.Status.Runtime != contracts.DeliveryCancelled || !strings.Contains(orig.Status.RuntimeReason, "superseded by reissue "+re.Envelope.MessageID) {
		t.Fatalf("original must be superseded: %+v", orig.Status)
	}
	if x.cancelsFor(nodeMac, id) <= before {
		t.Fatal("the node was not told to drop the superseded original")
	}
}

func TestStaleAttemptRequeuedWhileRecipientIdle(t *testing.T) {
	x := newHarness(t)
	x.connect(nodeMac, 0)
	v := x.start(owner, "op_synth-start", nodeMac)
	sid, run := v.Session.SessionID, v.Run.RunID
	x.observeState(sid, contracts.RunWaitingInput)

	id := x.sendTo(sid, "op_synth-stale")
	x.report(nodeMac, contracts.ReportReceipt, contracts.DeliveryReceipt{MessageID: id, AttemptID: "dlv_synth-stale", RunID: run, State: contracts.DeliveryAttempting, Evidence: contracts.EvidenceNone})

	// Recent: left alone.
	x.clock.Add(time.Minute)
	x.maintain()
	if got := x.msg(id).Status.Runtime; got != contracts.DeliveryAttempting {
		t.Fatalf("recent attempt changed: %s", got)
	}
	// Stale but the recipient is busy (the attempt may have started its turn).
	x.observeState(sid, contracts.RunRunning)
	x.clock.Add(2 * time.Minute)
	x.maintain()
	if got := x.msg(id).Status.Runtime; got != contracts.DeliveryAttempting {
		t.Fatalf("stale attempt on a busy run changed: %s", got)
	}
	// Disconnected node: its outcome may still come.
	x.observeState(sid, contracts.RunWaitingInput)
	live := *x.session(sid).Run
	x.disconnect(nodeMac)
	x.clock.Add(store.StaleAttemptAfter)
	x.maintain()
	if got := x.msg(id).Status.Runtime; got != contracts.DeliveryAttempting {
		t.Fatalf("stale attempt on a disconnected node changed: %s", got)
	}
	x.connect(nodeMac, 0, live)

	// Idle recipient, no outcome for StaleAttemptAfter: back in the queue,
	// still bound (the outcome was never reported).
	x.maintain()
	m := x.msg(id)
	if m.Status.Runtime != contracts.DeliveryNodeReceived || m.Status.BoundRunID != run || !strings.Contains(m.Status.RuntimeReason, "dlv_synth-stale") ||
		!strings.Contains(m.Status.RuntimeReason, "observed idle") {
		t.Fatalf("stale attempt: %+v", m.Status)
	}
	last := m.Status.Receipts[len(m.Status.Receipts)-1]
	if last.State != contracts.DeliveryQueued || last.AttemptID != "" {
		t.Fatalf("hub receipt: %+v", last)
	}
	// Its outcome is still unknown: reissue needs the risk flag.
	if _, err := x.reissue("op_synth-re1", id, false); !contracts.IsCode(err, contracts.ErrConflict) || !strings.Contains(err.Error(), "accept_duplicate_risk") {
		t.Fatalf("reissue without risk: %v", err)
	}
	re, err := x.reissue("op_synth-re2", id, true)
	if err != nil {
		t.Fatal(err)
	}
	if orig := x.msg(id); orig.Status.Runtime != contracts.DeliveryCancelled || !strings.Contains(orig.Status.RuntimeReason, re.Envelope.MessageID) {
		t.Fatalf("original after reissue: %+v", orig.Status)
	}
}

// Reissue applies the stale rule itself instead of waiting for the next
// maintenance pass.
func TestReissueAppliesStaleAttemptRule(t *testing.T) {
	x := newHarness(t)
	x.connect(nodeMac, 0)
	v := x.start(owner, "op_synth-start", nodeMac)
	sid, run := v.Session.SessionID, v.Run.RunID
	x.observeState(sid, contracts.RunWaitingInput)
	id := x.sendTo(sid, "op_synth-msg")
	x.report(nodeMac, contracts.ReportReceipt, contracts.DeliveryReceipt{MessageID: id, AttemptID: "dlv_synth", RunID: run, State: contracts.DeliveryAttempting, Evidence: contracts.EvidenceNone})
	x.clock.Add(store.StaleAttemptAfter + time.Second)
	if _, err := x.reissue("op_synth-re", id, true); err != nil {
		t.Fatalf("reissue of a stale attempt with accepted risk: %v", err)
	}
	if got := x.msg(id).Status.Runtime; got != contracts.DeliveryCancelled {
		t.Fatalf("original: %s", got)
	}
}
