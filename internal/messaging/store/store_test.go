package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/events"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type env struct {
	t     *testing.T
	path  string
	db    *sql.DB
	log   *events.Log
	s     *Store
	clock *clock
}

var (
	sesA  = contracts.Principal{Kind: contracts.PrincipalSession, ID: "ses_synth-a"}
	sesB  = contracts.Principal{Kind: contracts.PrincipalSession, ID: "ses_synth-b"}
	owner = contracts.Principal{Kind: contracts.PrincipalOwner, ID: "cli_synth-laptop", Name: "owner (synthetic)"}
)

func newEnv(t *testing.T, lim Limits) *env {
	t.Helper()
	e := &env{t: t, path: filepath.Join(t.TempDir(), "hub.db"), clock: &clock{t: time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)}}
	e.open(lim)
	t.Cleanup(func() { e.db.Close() })
	return e
}

func (e *env) open(lim Limits) {
	e.t.Helper()
	db, err := sqlitex.Open(e.path)
	if err != nil {
		e.t.Fatal(err)
	}
	ctx := context.Background()
	log, err := events.Open(ctx, db, events.Options{Clock: e.clock, PollInterval: 20 * time.Millisecond})
	if err != nil {
		e.t.Fatal(err)
	}
	s, err := Open(ctx, Config{DB: db, Clock: e.clock, Events: log, Limits: lim, PollInterval: 20 * time.Millisecond})
	if err != nil {
		e.t.Fatal(err)
	}
	e.db, e.log, e.s = db, log, s
}

func (e *env) reopen() {
	e.t.Helper()
	e.db.Close()
	e.open(Limits{})
}

func as(p contracts.Principal) context.Context {
	return contracts.WithPrincipal(context.Background(), p)
}

func (e *env) send(p contracts.Principal, op, id string, to contracts.Address, text string) contracts.MessageView {
	e.t.Helper()
	v, err := e.s.SendMessage(as(p), contracts.SendMessageRequest{Mutation: contracts.Mutation{OperationID: op}, MessageID: id, Recipient: to, Body: contracts.MessageBody{Text: text}})
	if err != nil {
		e.t.Fatalf("send %s: %v", op, err)
	}
	return v
}

func (e *env) receipt(node string, seq uint64, msg, run string, st contracts.RuntimeDeliveryState, ev contracts.EvidenceCategory) (ReceiptResult, error) {
	var res ReceiptResult
	err := sqlitex.Tx(context.Background(), e.db, func(tx *sql.Tx) error {
		var err error
		res, err = e.s.ApplyReceiptTx(context.Background(), tx, node, contracts.DeliveryReceipt{
			MessageID: msg, AttemptID: "dlv_t", RunID: run, State: st, Evidence: ev, OriginSeq: seq,
		})
		return err
	})
	return res, err
}

func (e *env) get(id string) contracts.MessageView {
	e.t.Helper()
	v, err := e.s.GetMessage(as(owner), contracts.GetRequest{ID: id})
	if err != nil {
		e.t.Fatal(err)
	}
	return v
}

var toB = contracts.Address{Kind: contracts.AddrSession, ID: "ses_synth-b"}
var toA = contracts.Address{Kind: contracts.AddrSession, ID: "ses_synth-a"}

func TestDuplicateOperationFixture(t *testing.T) {
	e := newEnv(t, Limits{})
	req := contracts.SendMessageRequest{Mutation: contracts.Mutation{OperationID: "op_synth-dup-1"}, MessageID: "msg_synth-dup", Recipient: toB, Mode: contracts.ModeQueue, Body: contracts.MessageBody{Text: "Synthetic: run lint."}}
	first, err := e.s.SendMessage(as(sesA), req)
	if err != nil {
		t.Fatal(err)
	}
	if first.Status.Storage != contracts.StorageStored || first.Status.Runtime != contracts.DeliveryQueued || first.Status.Ack != contracts.AckNone {
		t.Fatalf("status %+v", first.Status)
	}
	if first.Envelope.Sender != sesA {
		t.Fatalf("sender %+v", first.Envelope.Sender)
	}
	// Retry identical.
	again, err := e.s.SendMessage(as(sesA), req)
	if err != nil || again.Envelope.MessageID != first.Envelope.MessageID || again.Cursor != first.Cursor {
		t.Fatalf("replay: %v %+v", err, again)
	}
	// Same op, different body.
	bad := req
	bad.Body.Text = "Synthetic: run lint AND deploy."
	if _, err := e.s.SendMessage(as(sesA), bad); !contracts.IsCode(err, contracts.ErrConflict) {
		t.Fatalf("want conflict, got %v", err)
	}
	// New op, same message id, same content -> existing message.
	req2 := req
	req2.OperationID = "op_synth-dup-2"
	dup, err := e.s.SendMessage(as(sesA), req2)
	if err != nil || dup.Envelope.OperationID != "op_synth-dup-1" || dup.Cursor != first.Cursor {
		t.Fatalf("dedup: %v %+v", err, dup.Envelope)
	}
	// New op, same message id, different content -> conflict.
	req3 := req
	req3.OperationID = "op_synth-dup-3"
	req3.Body.Text = "other"
	if _, err := e.s.SendMessage(as(sesA), req3); !contracts.IsCode(err, contracts.ErrConflict) {
		t.Fatalf("want message conflict, got %v", err)
	}
	// A different sender reusing the message id is a conflict, not a spoof.
	req4 := req
	req4.OperationID = "op_synth-dup-4"
	if _, err := e.s.SendMessage(as(sesB), req4); !contracts.IsCode(err, contracts.ErrConflict) {
		t.Fatalf("want conflict for other sender, got %v", err)
	}
	var n int
	e.db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&n)
	if n != 1 {
		t.Fatalf("stored %d messages", n)
	}
	var evs int
	e.db.QueryRow(`SELECT COUNT(*) FROM events WHERE kind='message.stored'`).Scan(&evs)
	if evs != 1 {
		t.Fatalf("stored events %d", evs)
	}
}

func TestSenderFromPrincipalAndAuth(t *testing.T) {
	e := newEnv(t, Limits{})
	if _, err := e.s.SendMessage(context.Background(), contracts.SendMessageRequest{Mutation: contracts.Mutation{OperationID: "op_1"}, Recipient: toB, Body: contracts.MessageBody{Text: "x"}}); !contracts.IsCode(err, contracts.ErrUnauthorized) {
		t.Fatalf("want unauthorized, got %v", err)
	}
	node := contracts.Principal{Kind: contracts.PrincipalNode, ID: "node_synth-mac", OnBehalfOf: &sesA}
	v := e.send(node, "op_2", "", toB, "proxied")
	if v.Envelope.Sender.Kind != contracts.PrincipalNode || v.Envelope.Sender.OnBehalfOf == nil || v.Envelope.Sender.OnBehalfOf.ID != sesA.ID {
		t.Fatalf("delegated sender not recorded: %+v", v.Envelope.Sender)
	}
	// Outbox of A includes the delegated message.
	page, err := e.s.ListMessages(as(sesA), contracts.ListMessagesRequest{Box: "outbox"})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("outbox: %v %d", err, len(page.Items))
	}
}

func TestOwnerAndInfoNotApplicable(t *testing.T) {
	e := newEnv(t, Limits{})
	v := e.send(sesA, "op_1", "", contracts.Address{Kind: contracts.AddrOwner}, "hello human")
	if v.Status.Runtime != contracts.DeliveryNotApplicable || v.Envelope.Recipient.ID != "owner" {
		t.Fatalf("owner: %+v", v)
	}
	iv, err := e.s.SendMessage(as(sesA), contracts.SendMessageRequest{Mutation: contracts.Mutation{OperationID: "op_2"}, Recipient: toB, Mode: contracts.ModeInfo, Body: contracts.MessageBody{Text: "fyi"}})
	if err != nil || iv.Status.Runtime != contracts.DeliveryNotApplicable {
		t.Fatalf("info: %v %+v", err, iv.Status)
	}
	page, err := e.s.ListMessages(as(owner), contracts.ListMessagesRequest{})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("owner inbox: %v %d", err, len(page.Items))
	}
}

func TestValidation(t *testing.T) {
	e := newEnv(t, Limits{MaxBodyBytes: 10})
	cases := []contracts.SendMessageRequest{
		{Mutation: contracts.Mutation{OperationID: "op_1"}, Recipient: contracts.Address{Kind: "bogus", ID: "x"}, Body: contracts.MessageBody{Text: "x"}},
		{Mutation: contracts.Mutation{OperationID: "op_2"}, Recipient: contracts.Address{Kind: contracts.AddrSession, ID: "nope"}, Body: contracts.MessageBody{Text: "x"}},
		{Mutation: contracts.Mutation{OperationID: "op_3"}, Recipient: toB},
		{Mutation: contracts.Mutation{OperationID: "op_4"}, Recipient: toB, Mode: "shout", Body: contracts.MessageBody{Text: "x"}},
		{Recipient: toB, Body: contracts.MessageBody{Text: "x"}},
		{Mutation: contracts.Mutation{OperationID: "op_5"}, MessageID: "bad id", Recipient: toB, Body: contracts.MessageBody{Text: "x"}},
	}
	for i, c := range cases {
		if _, err := e.s.SendMessage(as(sesA), c); !contracts.IsCode(err, contracts.ErrInvalid) {
			t.Errorf("case %d: want invalid, got %v", i, err)
		}
	}
	_, err := e.s.SendMessage(as(sesA), contracts.SendMessageRequest{Mutation: contracts.Mutation{OperationID: "op_6"}, Recipient: toB, Body: contracts.MessageBody{Text: "this is too long"}})
	if !contracts.IsCode(err, contracts.ErrPayloadTooLarge) {
		t.Fatalf("want payload_too_large, got %v", err)
	}
}

func TestCapacityBackpressureNotStored(t *testing.T) {
	e := newEnv(t, Limits{MaxQueuedPerRecipient: 2})
	m1 := e.send(sesA, "op_1", "", toB, "one")
	e.send(sesA, "op_2", "", toB, "two")
	req := contracts.SendMessageRequest{Mutation: contracts.Mutation{OperationID: "op_3"}, Recipient: toB, Body: contracts.MessageBody{Text: "three"}}
	_, err := e.s.SendMessage(as(sesA), req)
	if !contracts.IsCode(err, contracts.ErrCapacityExhausted) || !contracts.AsError(err).Retryable {
		t.Fatalf("want retryable capacity_exhausted, got %v", err)
	}
	// Other recipients are unaffected.
	e.send(sesB, "op_4", "", toA, "to a")
	// Free capacity: the first message is accepted.
	if _, err := e.receipt("node_x", 1, m1.Envelope.MessageID, "run_b1", contracts.DeliveryAccepted, contracts.EvidenceHookReceipt); err != nil {
		t.Fatal(err)
	}
	// Retrying the same operation now succeeds (the failure was not stored).
	v, err := e.s.SendMessage(as(sesA), req)
	if err != nil || v.Status.Runtime != contracts.DeliveryQueued {
		t.Fatalf("retry: %v", err)
	}
}

func TestReceiptTransitionsAndBinding(t *testing.T) {
	e := newEnv(t, Limits{})
	m := e.send(sesA, "op_1", "msg_synth-1", toB, "run tests").Envelope.MessageID

	if _, err := e.receipt("node_l", 10, m, "run_b1", contracts.DeliveryAttempting, contracts.EvidenceNone); err != nil {
		t.Fatal(err)
	}
	v := e.get(m)
	if v.Status.Runtime != contracts.DeliveryAttempting || v.Status.BoundRunID != "run_b1" {
		t.Fatalf("attempting: %+v", v.Status)
	}
	// A different run can never take the message.
	if _, err := e.receipt("node_l", 11, m, "run_b2", contracts.DeliveryAccepted, contracts.EvidenceHookReceipt); !contracts.IsCode(err, contracts.ErrConflict) {
		t.Fatalf("rebind: want conflict, got %v", err)
	}
	// Crash window: indeterminate.
	if _, err := e.receipt("node_l", 12, m, "run_b1", contracts.DeliveryIndeterminate, contracts.EvidenceNone); err != nil {
		t.Fatal(err)
	}
	// A weak "rejected" (no evidence) does not resolve indeterminate.
	if _, err := e.receipt("node_l", 13, m, "run_b1", contracts.DeliveryRejected, contracts.EvidenceNone); err != nil {
		t.Fatal(err)
	}
	if got := e.get(m).Status.Runtime; got != contracts.DeliveryIndeterminate {
		t.Fatalf("weak reject resolved indeterminate: %s", got)
	}
	// Terminal writes never prove acceptance.
	if _, err := e.receipt("node_l", 14, m, "run_b1", contracts.DeliveryAccepted, contracts.EvidenceTerminalWrite); err != nil {
		t.Fatal(err)
	}
	if got := e.get(m).Status.Runtime; got != contracts.DeliveryIndeterminate {
		t.Fatalf("terminal write accepted: %s", got)
	}
	// Later evidence resolves it.
	if _, err := e.receipt("node_l", 15, m, "run_b1", contracts.DeliveryAccepted, contracts.EvidenceTranscript); err != nil {
		t.Fatal(err)
	}
	if got := e.get(m).Status.Runtime; got != contracts.DeliveryAccepted {
		t.Fatalf("evidence: %s", got)
	}
	// Accepted is final; an out-of-order older receipt cannot move it.
	if _, err := e.receipt("node_l", 9, m, "run_b1", contracts.DeliveryAttempting, contracts.EvidenceNone); err != nil {
		t.Fatal(err)
	}
	if _, err := e.receipt("node_l", 16, m, "run_b1", contracts.DeliveryRejected, contracts.EvidenceProtocolAck); err != nil {
		t.Fatal(err)
	}
	v = e.get(m)
	if v.Status.Runtime != contracts.DeliveryAccepted || v.Status.BoundRunID != "run_b1" {
		t.Fatalf("final: %+v", v.Status)
	}
	// Duplicate report is a no-op.
	res, err := e.receipt("node_l", 15, m, "run_b1", contracts.DeliveryAccepted, contracts.EvidenceTranscript)
	if err != nil || !res.Duplicate {
		t.Fatalf("dup: %v %+v", err, res)
	}
	if n := len(e.get(m).Status.Receipts); n != 7 {
		t.Fatalf("receipts recorded: %d", n)
	}
	// A node may not report for another node.
	err = sqlitex.Tx(context.Background(), e.db, func(tx *sql.Tx) error {
		_, err := e.s.ApplyReceiptTx(context.Background(), tx, "node_l", contracts.DeliveryReceipt{MessageID: m, NodeID: "node_other", RunID: "run_b1", State: contracts.DeliveryAccepted, OriginSeq: 99})
		return err
	})
	if !contracts.IsCode(err, contracts.ErrForbidden) {
		t.Fatalf("want forbidden, got %v", err)
	}
}

func TestTransitionTable(t *testing.T) {
	type tc struct {
		cur  contracts.RuntimeDeliveryState
		st   contracts.RuntimeDeliveryState
		ev   contracts.EvidenceCategory
		want contracts.RuntimeDeliveryState
	}
	Q, N, T, A, R, I, X, C := contracts.DeliveryQueued, contracts.DeliveryNodeReceived, contracts.DeliveryAttempting, contracts.DeliveryAccepted, contracts.DeliveryRejected, contracts.DeliveryIndeterminate, contracts.DeliveryExpired, contracts.DeliveryCancelled
	hook, none := contracts.EvidenceHookReceipt, contracts.EvidenceNone
	cases := []tc{
		{Q, N, none, N}, {Q, T, none, T}, {Q, A, hook, A}, {Q, A, none, I}, {Q, R, none, R}, {Q, I, none, I},
		{N, T, none, T}, {N, Q, none, N}, {T, Q, none, N}, {T, A, hook, A}, {T, R, none, R},
		{A, R, hook, A}, {A, I, none, A}, {A, T, none, A}, {A, Q, none, A},
		{I, R, none, I}, {I, R, hook, R}, {I, A, hook, A}, {I, T, none, I}, {I, Q, none, I}, {I, A, contracts.EvidenceTerminalWrite, I},
		{R, T, none, T}, {R, A, hook, A},
		{X, A, hook, A}, {X, T, none, T}, {X, R, hook, X}, {C, I, none, I}, {C, Q, none, C},
		{Q, X, none, X}, {N, C, none, C}, {T, X, none, T},
		{contracts.DeliveryNotApplicable, A, hook, contracts.DeliveryNotApplicable},
		// v1.6 doorbell kinds: an inbox-tool read is acceptance; the
		// monitor notification never is.
		{N, A, contracts.EvidenceClaudeInboxRead, A}, {Q, A, contracts.EvidenceClaudeInboxRead, A}, {R, A, contracts.EvidenceClaudeInboxRead, A},
		{N, A, contracts.EvidenceClaudeMonitorNotified, I}, {I, A, contracts.EvidenceClaudeMonitorNotified, I}, {I, R, contracts.EvidenceClaudeMonitorNotified, I},
	}
	for _, c := range cases {
		got, _, _ := Transition(c.cur, contracts.DeliveryReceipt{State: c.st, Evidence: c.ev})
		if got != c.want {
			t.Errorf("%s + %s(%s) = %s, want %s", c.cur, c.st, c.ev, got, c.want)
		}
	}
}

func TestReplyAckAndThreads(t *testing.T) {
	e := newEnv(t, Limits{})
	q, err := e.s.SendMessage(as(sesA), contracts.SendMessageRequest{Mutation: contracts.Mutation{OperationID: "op_1"}, Recipient: toB, ThreadID: "thr_synth-status", Body: contracts.MessageBody{Text: "status?"}})
	if err != nil {
		t.Fatal(err)
	}
	// A third party "replying" does not mark it replied.
	sesC := contracts.Principal{Kind: contracts.PrincipalSession, ID: "ses_synth-c"}
	if _, err := e.s.SendMessage(as(sesC), contracts.SendMessageRequest{Mutation: contracts.Mutation{OperationID: "op_2"}, Recipient: toA, InReplyTo: q.Envelope.MessageID, Body: contracts.MessageBody{Text: "not me"}}); err != nil {
		t.Fatal(err)
	}
	if e.get(q.Envelope.MessageID).Status.Ack != contracts.AckNone {
		t.Fatal("third party marked replied")
	}
	r, err := e.s.SendMessage(as(sesB), contracts.SendMessageRequest{Mutation: contracts.Mutation{OperationID: "op_3"}, Recipient: toA, InReplyTo: q.Envelope.MessageID, Body: contracts.MessageBody{Text: "tests pass"}})
	if err != nil {
		t.Fatal(err)
	}
	if r.Envelope.ThreadID != "thr_synth-status" {
		t.Fatalf("thread not inherited: %q", r.Envelope.ThreadID)
	}
	qv := e.get(q.Envelope.MessageID)
	if qv.Status.Ack != contracts.AckReplied || qv.Status.AckRef != r.Envelope.MessageID {
		t.Fatalf("reply link: %+v", qv.Status)
	}
	// Ack after reply keeps replied.
	v, err := e.s.AckMessage(as(sesB), contracts.AckMessageRequest{Mutation: contracts.Mutation{OperationID: "op_4"}, MessageID: q.Envelope.MessageID})
	if err != nil || v.Status.Ack != contracts.AckReplied {
		t.Fatalf("ack after reply: %v %+v", err, v.Status)
	}
	// Only recipient/owner can ack.
	if _, err := e.s.AckMessage(as(sesC), contracts.AckMessageRequest{Mutation: contracts.Mutation{OperationID: "op_5"}, MessageID: r.Envelope.MessageID}); !contracts.IsCode(err, contracts.ErrForbidden) {
		t.Fatalf("want forbidden, got %v", err)
	}
	v, err = e.s.AckMessage(as(sesA), contracts.AckMessageRequest{Mutation: contracts.Mutation{OperationID: "op_6"}, MessageID: r.Envelope.MessageID})
	if err != nil || v.Status.Ack != contracts.AckAcknowledged || v.Status.AckRef != "op_6" {
		t.Fatalf("ack: %v %+v", err, v.Status)
	}
	th, err := e.s.ListMessages(as(owner), contracts.ListMessagesRequest{ThreadID: "thr_synth-status"})
	if err != nil || len(th.Items) != 3 {
		t.Fatalf("thread: %v %d", err, len(th.Items))
	}
	un, err := e.s.ListMessages(as(sesA), contracts.ListMessagesRequest{Unacked: true})
	if err != nil || len(un.Items) != 1 {
		t.Fatalf("unacked inbox of A: %v %d", err, len(un.Items))
	}
}

func TestReissueRules(t *testing.T) {
	e := newEnv(t, Limits{})
	m := e.send(sesA, "op_1", "", toB, "delete the temp branch").Envelope.MessageID
	re := func(p contracts.Principal, op string, risk bool) (contracts.MessageView, error) {
		return e.s.ReissueMessage(as(p), contracts.ReissueMessageRequest{Mutation: contracts.Mutation{OperationID: op}, MessageID: m, AcceptDuplicateRisk: risk})
	}
	if _, err := re(sesA, "op_r1", true); !contracts.IsCode(err, contracts.ErrConflict) {
		t.Fatalf("queued original: want conflict, got %v", err)
	}
	if _, err := e.receipt("node_l", 1, m, "run_b1", contracts.DeliveryIndeterminate, contracts.EvidenceNone); err != nil {
		t.Fatal(err)
	}
	if _, err := re(sesA, "op_r2", false); !contracts.IsCode(err, contracts.ErrConflict) {
		t.Fatalf("indeterminate without risk: want conflict, got %v", err)
	}
	if _, err := re(sesB, "op_r3", true); !contracts.IsCode(err, contracts.ErrForbidden) {
		t.Fatalf("non-sender: want forbidden, got %v", err)
	}
	nv, err := re(sesA, "op_r4", true)
	if err != nil {
		t.Fatal(err)
	}
	if nv.Envelope.MessageID == m || nv.Envelope.Reissues != m || nv.Status.Runtime != contracts.DeliveryQueued || nv.Status.BoundRunID != "" {
		t.Fatalf("reissue: %+v", nv)
	}
	// The original keeps its binding and state.
	ov := e.get(m)
	if ov.Status.Runtime != contracts.DeliveryIndeterminate || ov.Status.BoundRunID != "run_b1" {
		t.Fatalf("original changed: %+v", ov.Status)
	}
	// Replay returns the same new message.
	nv2, err := re(sesA, "op_r4", true)
	if err != nil || nv2.Envelope.MessageID != nv.Envelope.MessageID {
		t.Fatalf("replay: %v", err)
	}
	var risk string
	e.db.QueryRow(`SELECT risk_accepted_by FROM messages WHERE message_id=?`, nv.Envelope.MessageID).Scan(&risk)
	if risk == "" {
		t.Fatal("risk acceptance not recorded")
	}
	// Accepted originals are never reissued.
	e.receipt("node_l", 2, nv.Envelope.MessageID, "run_b1", contracts.DeliveryAccepted, contracts.EvidenceHookReceipt)
	if _, err := e.s.ReissueMessage(as(sesA), contracts.ReissueMessageRequest{Mutation: contracts.Mutation{OperationID: "op_r5"}, MessageID: nv.Envelope.MessageID, AcceptDuplicateRisk: true}); !contracts.IsCode(err, contracts.ErrConflict) {
		t.Fatalf("reissue accepted: want conflict, got %v", err)
	}
}

func TestResolveAndCancel(t *testing.T) {
	e := newEnv(t, Limits{})
	m := e.send(sesA, "op_1", "", toB, "x").Envelope.MessageID
	if _, err := e.s.ResolveDelivery(as(owner), contracts.ResolveDeliveryRequest{Mutation: contracts.Mutation{OperationID: "op_2"}, MessageID: m, Resolution: contracts.DeliveryAccepted}); !contracts.IsCode(err, contracts.ErrConflict) {
		t.Fatalf("resolve queued: want conflict, got %v", err)
	}
	e.receipt("node_l", 1, m, "run_b1", contracts.DeliveryIndeterminate, contracts.EvidenceNone)
	if _, err := e.s.CancelMessage(as(sesA), contracts.CancelMessageRequest{Mutation: contracts.Mutation{OperationID: "op_3"}, MessageID: m}); !contracts.IsCode(err, contracts.ErrConflict) {
		t.Fatalf("cancel attempted: want conflict, got %v", err)
	}
	if _, err := e.s.ResolveDelivery(as(owner), contracts.ResolveDeliveryRequest{Mutation: contracts.Mutation{OperationID: "op_4"}, MessageID: m, Resolution: "queued"}); !contracts.IsCode(err, contracts.ErrInvalid) {
		t.Fatalf("bad resolution: %v", err)
	}
	v, err := e.s.ResolveDelivery(as(owner), contracts.ResolveDeliveryRequest{Mutation: contracts.Mutation{OperationID: "op_5"}, MessageID: m, Resolution: contracts.DeliveryRejected, Note: "checked transcript: not present"})
	if err != nil || v.Status.Runtime != contracts.DeliveryRejected || v.Status.BoundRunID != "run_b1" {
		t.Fatalf("resolve: %v %+v", err, v.Status)
	}
	// Cancel before any attempt.
	m2 := e.send(sesA, "op_6", "", toB, "y").Envelope.MessageID
	withdrawn := 0
	e.s.cfg.OnWithdrawn = func(context.Context, *sql.Tx, contracts.MessageView) error { withdrawn++; return nil }
	if _, err := e.s.CancelMessage(as(sesB), contracts.CancelMessageRequest{Mutation: contracts.Mutation{OperationID: "op_7"}, MessageID: m2}); !contracts.IsCode(err, contracts.ErrForbidden) {
		t.Fatalf("cancel by other: %v", err)
	}
	cv, err := e.s.CancelMessage(as(sesA), contracts.CancelMessageRequest{Mutation: contracts.Mutation{OperationID: "op_8"}, MessageID: m2})
	if err != nil || cv.Status.Runtime != contracts.DeliveryCancelled || withdrawn != 1 {
		t.Fatalf("cancel: %v %+v withdrawn=%d", err, cv.Status, withdrawn)
	}
	// An attempt that raced the cancel is still recorded truthfully.
	e.receipt("node_l", 2, m2, "run_b1", contracts.DeliveryAccepted, contracts.EvidenceHookReceipt)
	if got := e.get(m2).Status.Runtime; got != contracts.DeliveryAccepted {
		t.Fatalf("evidence after cancel: %s", got)
	}
}

func TestExpiryIsExplicit(t *testing.T) {
	e := newEnv(t, Limits{})
	m, err := e.s.SendMessage(as(sesA), contracts.SendMessageRequest{Mutation: contracts.Mutation{OperationID: "op_1"}, Recipient: toB, TTLSeconds: 60, Body: contracts.MessageBody{Text: "soon stale"}})
	if err != nil {
		t.Fatal(err)
	}
	keep := e.send(sesA, "op_2", "", toB, "long lived").Envelope.MessageID
	bound := e.send(sesA, "op_3", "", toB, "bound").Envelope.MessageID
	e.receipt("node_l", 1, bound, "run_b1", contracts.DeliveryIndeterminate, contracts.EvidenceNone)
	e.clock.Add(2 * time.Minute)
	n, err := e.s.ExpireDue(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("expire: %v n=%d", err, n)
	}
	v := e.get(m.Envelope.MessageID)
	if v.Status.Runtime != contracts.DeliveryExpired || v.Status.RuntimeReason == "" {
		t.Fatalf("expired: %+v", v.Status)
	}
	if e.get(keep).Status.Runtime != contracts.DeliveryQueued || e.get(bound).Status.Runtime != contracts.DeliveryIndeterminate {
		t.Fatal("unexpected expiry")
	}
	e.clock.Add(8 * 24 * time.Hour)
	if n, _ := e.s.ExpireDue(context.Background()); n != 1 {
		t.Fatalf("default ttl expiry n=%d", n)
	}
	if e.get(bound).Status.Runtime != contracts.DeliveryIndeterminate {
		t.Fatal("bound message expired")
	}
}

func TestLongPollAndAfterCursor(t *testing.T) {
	e := newEnv(t, Limits{})
	first := e.send(sesB, "op_1", "", toA, "one")
	done := make(chan contracts.Page[contracts.MessageView], 1)
	go func() {
		p, err := e.s.ListMessages(as(sesA), contracts.ListMessagesRequest{AfterCursor: first.Cursor, WaitSeconds: 5})
		if err != nil {
			t.Error(err)
		}
		done <- p
	}()
	time.Sleep(50 * time.Millisecond)
	second := e.send(sesB, "op_2", "", toA, "two")
	select {
	case p := <-done:
		if len(p.Items) != 1 || p.Items[0].Envelope.MessageID != second.Envelope.MessageID || p.Items[0].Cursor != first.Cursor+1 {
			t.Fatalf("long poll got %+v", p.Items)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("long poll did not wake")
	}
	// Timeout returns empty.
	start := time.Now()
	p, err := e.s.ListMessages(as(sesA), contracts.ListMessagesRequest{AfterCursor: second.Cursor, WaitSeconds: 1})
	if err != nil || len(p.Items) != 0 || time.Since(start) < 900*time.Millisecond {
		t.Fatalf("timeout poll: %v %d %v", err, len(p.Items), time.Since(start))
	}
	if _, err := e.s.ListMessages(as(sesA), contracts.ListMessagesRequest{Box: "outbox", AfterCursor: 1}); !contracts.IsCode(err, contracts.ErrInvalid) {
		t.Fatalf("after_cursor on outbox: %v", err)
	}
}

func TestPagination(t *testing.T) {
	e := newEnv(t, Limits{})
	for i := 0; i < 7; i++ {
		e.send(sesB, "op_"+string(rune('a'+i)), "", toA, "m")
	}
	var got []int64
	cursor := ""
	for {
		p, err := e.s.ListMessages(as(sesA), contracts.ListMessagesRequest{PageRequest: contracts.PageRequest{Cursor: cursor, Limit: 3}})
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range p.Items {
			got = append(got, it.Cursor)
		}
		if p.NextCursor == "" {
			break
		}
		cursor = p.NextCursor
	}
	if len(got) != 7 || got[0] != 1 || got[6] != 7 {
		t.Fatalf("pages: %v", got)
	}
}

func TestCrashRecoveryReopen(t *testing.T) {
	e := newEnv(t, Limits{})
	m := e.send(sesA, "op_1", "msg_synth-crash", toB, "x").Envelope.MessageID
	e.receipt("node_l", 5, m, "run_b1", contracts.DeliveryAttempting, contracts.EvidenceNone)
	e.reopen()
	// Same operation after restart replays; no duplicate.
	v := e.send(sesA, "op_1", "msg_synth-crash", toB, "x")
	if v.Envelope.MessageID != m {
		t.Fatal("replay after restart created a new message")
	}
	// Re-sent report after restart is deduplicated.
	res, err := e.receipt("node_l", 5, m, "run_b1", contracts.DeliveryAttempting, contracts.EvidenceNone)
	if err != nil || !res.Duplicate {
		t.Fatalf("dup after restart: %v %+v", err, res)
	}
	e.receipt("node_l", 6, m, "run_b1", contracts.DeliveryIndeterminate, contracts.EvidenceNone)
	e.reopen()
	got := e.get(m)
	if got.Status.Runtime != contracts.DeliveryIndeterminate || got.Status.BoundRunID != "run_b1" {
		t.Fatalf("after reopen: %+v", got.Status)
	}
	// Next inbox cursor continues after restart.
	n := e.send(sesA, "op_2", "", toB, "y")
	if n.Cursor != 2 {
		t.Fatalf("cursor after restart %d", n.Cursor)
	}
}

func TestOnStoredHookSeesQueuedAndCanReject(t *testing.T) {
	e := newEnv(t, Limits{})
	e.s.cfg.OnStored = func(ctx context.Context, tx *sql.Tx, v contracts.MessageView) error {
		if v.Envelope.Mode == contracts.ModeSteer {
			_, err := e.s.SetRuntimeTx(ctx, tx, v.Envelope.MessageID, contracts.DeliveryRejected, "unsupported: adapter does not advertise input.steer")
			return err
		}
		return nil
	}
	e.s.cfg.ValidateRecipient = func(ctx context.Context, tx *sql.Tx, a contracts.Address) error {
		if a.ID == "ses_synth-missing" {
			return contracts.Errorf(contracts.ErrNotFound, "no such session")
		}
		return nil
	}
	v, err := e.s.SendMessage(as(sesA), contracts.SendMessageRequest{Mutation: contracts.Mutation{OperationID: "op_1"}, Recipient: toB, Mode: contracts.ModeSteer, Body: contracts.MessageBody{Text: "steer now"}})
	if err != nil || v.Status.Runtime != contracts.DeliveryRejected || v.Status.Storage != contracts.StorageStored {
		t.Fatalf("steer: %v %+v", err, v.Status)
	}
	if _, err := e.s.SendMessage(as(sesA), contracts.SendMessageRequest{Mutation: contracts.Mutation{OperationID: "op_2"}, Recipient: contracts.Address{Kind: contracts.AddrSession, ID: "ses_synth-missing"}, Body: contracts.MessageBody{Text: "x"}}); !contracts.IsCode(err, contracts.ErrNotFound) {
		t.Fatalf("missing recipient: %v", err)
	}
	// Stored not-found error replays the same error.
	if _, err := e.s.SendMessage(as(sesA), contracts.SendMessageRequest{Mutation: contracts.Mutation{OperationID: "op_2"}, Recipient: contracts.Address{Kind: contracts.AddrSession, ID: "ses_synth-missing"}, Body: contracts.MessageBody{Text: "x"}}); !contracts.IsCode(err, contracts.ErrNotFound) {
		t.Fatalf("replayed error: %v", err)
	}
	var n int
	e.db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&n)
	if n != 1 {
		t.Fatalf("messages %d", n)
	}
}

// Decision I16: a doorbell read arrives as one accepted receipt carrying
// the node-minted attempt (older nodes: empty attempt id). It binds the
// run, sets consumed_at, and later reads change nothing.
func TestInboxReadReceiptAcceptsOnceAndConsumes(t *testing.T) {
	e := newEnv(t, Limits{})
	for i, attempt := range []string{"dlv_minted", ""} {
		m := e.send(sesA, fmt.Sprintf("op_ir%d", i), fmt.Sprintf("msg_synth-ir%d", i), toB, "doorbell").Envelope.MessageID
		apply := func(seq uint64, ev contracts.EvidenceCategory) ReceiptResult {
			t.Helper()
			var res ReceiptResult
			err := sqlitex.Tx(context.Background(), e.db, func(tx *sql.Tx) error {
				var err error
				res, err = e.s.ApplyReceiptTx(context.Background(), tx, "node_l", contracts.DeliveryReceipt{
					MessageID: m, AttemptID: attempt, RunID: "run_b1", State: contracts.DeliveryAccepted, Evidence: ev, OriginSeq: seq, Consumed: true,
				})
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			return res
		}
		base := uint64(100 * (i + 1))
		if r := apply(base, contracts.EvidenceClaudeMonitorNotified); r.View.Status.Runtime == contracts.DeliveryAccepted {
			t.Fatalf("notification accepted: %+v", r.View.Status)
		}
		r := apply(base+1, contracts.EvidenceClaudeInboxRead)
		st := r.View.Status
		if st.Runtime != contracts.DeliveryAccepted || st.BoundRunID != "run_b1" || st.ConsumedAt == nil || !r.Changed {
			t.Fatalf("attempt %q: inbox read: %+v", attempt, st)
		}
		if r2 := apply(base+2, contracts.EvidenceClaudeInboxRead); r2.Changed || r2.View.Status.Runtime != contracts.DeliveryAccepted {
			t.Fatalf("second read changed state: %+v", r2.View.Status)
		}
	}
}

// Receipt times are recorded on the hub's clock (decision I17: the hub
// never trusts node clocks for its own timeline); the node's observation
// time is kept in the receipt detail.
func TestReceiptTimesUseHubClock(t *testing.T) {
	e := newEnv(t, Limits{})
	m := e.send(sesA, "op_skew", "msg_synth-skew", toB, "skewed node").Envelope.MessageID
	hubNow := e.clock.Now()
	nodeAt := hubNow.Add(10 * time.Minute) // node clock 10 min fast
	var res ReceiptResult
	err := sqlitex.Tx(context.Background(), e.db, func(tx *sql.Tx) error {
		var err error
		res, err = e.s.ApplyReceiptTx(context.Background(), tx, "node_l", contracts.DeliveryReceipt{
			MessageID: m, AttemptID: "dlv_skew", RunID: "run_b1", State: contracts.DeliveryAccepted,
			Evidence: contracts.EvidenceClaudeInboxRead, Detail: "doorbell read", ObservedAt: nodeAt, OriginSeq: 7, Consumed: true,
		})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	st := e.get(m).Status
	if st.ConsumedAt == nil || !st.ConsumedAt.Equal(hubNow) {
		t.Fatalf("consumed_at %v, want hub time %v (node said %v)", st.ConsumedAt, hubNow, nodeAt)
	}
	if len(st.Receipts) == 0 {
		t.Fatalf("no receipts: %+v", st)
	}
	rc := st.Receipts[len(st.Receipts)-1]
	if !rc.ObservedAt.Equal(hubNow) {
		t.Fatalf("receipt observed_at %v, want hub time %v", rc.ObservedAt, hubNow)
	}
	if want := "node_observed_at=" + nodeAt.UTC().Format(time.RFC3339Nano); !strings.Contains(rc.Detail, want) || !strings.HasPrefix(rc.Detail, "doorbell read") {
		t.Fatalf("receipt detail %q should keep the node time (%s)", rc.Detail, want)
	}
	if strings.Contains(st.RuntimeReason, "node_observed_at") || strings.Contains(res.View.Status.RuntimeReason, "node_observed_at") {
		t.Fatalf("runtime reason carries the annotation: %q", st.RuntimeReason)
	}
}

func (e *env) receiptA(seq uint64, msg, attempt, run string, st contracts.RuntimeDeliveryState, ev contracts.EvidenceCategory) error {
	return sqlitex.Tx(context.Background(), e.db, func(tx *sql.Tx) error {
		_, err := e.s.ApplyReceiptTx(context.Background(), tx, "node_l", contracts.DeliveryReceipt{
			MessageID: msg, AttemptID: attempt, RunID: run, State: st, Evidence: ev, OriginSeq: seq,
		})
		return err
	})
}

// A20 soak: an attempt the runtime declined without consuming anything
// (the node's deferred attempt reports "queued" with the attempt's ID)
// releases the hub's binding, so the message can reach a replacement run.
// Before the fix the binding stayed and the message was stuck forever.
func TestDeferredAttemptReleasesHubBinding(t *testing.T) {
	e := newEnv(t, Limits{})
	m := e.send(sesA, "op_d1", "msg_synth-d1", toB, "busy then replaced").Envelope.MessageID
	if err := e.receiptA(1, m, "dlv_a1", "run_old", contracts.DeliveryAttempting, contracts.EvidenceNone); err != nil {
		t.Fatal(err)
	}
	if err := e.receiptA(2, m, "dlv_a1", "run_old", contracts.DeliveryQueued, contracts.EvidenceNone); err != nil {
		t.Fatal(err)
	}
	v := e.get(m)
	if v.Status.BoundRunID != "" || v.Status.Runtime != contracts.DeliveryNodeReceived {
		t.Fatalf("after deferred attempt: %+v", v.Status)
	}
	// The replacement run takes it.
	if err := e.receiptA(3, m, "dlv_b1", "run_new", contracts.DeliveryAttempting, contracts.EvidenceNone); err != nil {
		t.Fatal(err)
	}
	if err := e.receiptA(4, m, "dlv_b1", "run_new", contracts.DeliveryAccepted, contracts.EvidenceHookReceipt); err != nil {
		t.Fatal(err)
	}
	v = e.get(m)
	if v.Status.Runtime != contracts.DeliveryAccepted || v.Status.BoundRunID != "run_new" {
		t.Fatalf("replacement run: %+v", v.Status)
	}
}

// A queued receipt never releases a binding made by another attempt whose
// outcome is not known to be deferred, nor one without an attempt ID.
func TestQueuedReceiptKeepsBindingOfUnresolvedAttempt(t *testing.T) {
	e := newEnv(t, Limits{})
	m := e.send(sesA, "op_d2", "msg_synth-d2", toB, "two attempts").Envelope.MessageID
	if err := e.receiptA(1, m, "dlv_x1", "run_old", contracts.DeliveryAttempting, contracts.EvidenceNone); err != nil {
		t.Fatal(err)
	}
	// A queued reason without an attempt (the engine's deferMsg) keeps it.
	if err := e.receiptA(2, m, "", "run_old", contracts.DeliveryQueued, contracts.EvidenceNone); err != nil {
		t.Fatal(err)
	}
	if b := e.get(m).Status.BoundRunID; b != "run_old" {
		t.Fatalf("binding released by a queued reason without attempt: %q", b)
	}
	// A second attempt deferred while the first is unresolved keeps it too.
	if err := e.receiptA(3, m, "dlv_x2", "run_old", contracts.DeliveryAttempting, contracts.EvidenceNone); err != nil {
		t.Fatal(err)
	}
	if err := e.receiptA(4, m, "dlv_x2", "run_old", contracts.DeliveryQueued, contracts.EvidenceNone); err != nil {
		t.Fatal(err)
	}
	if b := e.get(m).Status.BoundRunID; b != "run_old" {
		t.Fatalf("binding released although attempt dlv_x1 is unresolved: %q", b)
	}
}

// A20 soak: the receipt dedup lookup must use the partial index
// message_receipts_origin, not scan every receipt.
func TestReceiptDedupUsesIndex(t *testing.T) {
	e := newEnv(t, Limits{})
	var id, parent, unused int
	var detail string
	if err := e.db.QueryRow("EXPLAIN QUERY PLAN "+receiptDedupQuery, "node_l", 7).Scan(&id, &parent, &unused, &detail); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(detail, "message_receipts_origin") {
		t.Fatalf("receipt dedup plan %q does not use message_receipts_origin", detail)
	}
	// Dedup still works.
	m := e.send(sesA, "op_dd", "msg_synth-dd", toB, "dedup").Envelope.MessageID
	if err := e.receiptA(42, m, "dlv_dd", "run_dd", contracts.DeliveryAttempting, contracts.EvidenceNone); err != nil {
		t.Fatal(err)
	}
	res, err := e.receipt("node_l", 42, m, "run_dd", contracts.DeliveryAttempting, contracts.EvidenceNone)
	if err != nil || !res.Duplicate {
		t.Fatalf("duplicate not detected: %v %+v", err, res)
	}
}

// Owner mail that asks nothing of the owner is stored acknowledged (an
// update); a question, a requested ack and a reply to him stay unacked.
func TestOwnerUpdatesStoredAcknowledged(t *testing.T) {
	e := newEnv(t, Limits{})
	toOwner := contracts.Address{Kind: contracts.AddrOwner}
	send := func(op, text string, mut func(*contracts.SendMessageRequest)) contracts.MessageView {
		req := contracts.SendMessageRequest{Mutation: contracts.Mutation{OperationID: op}, Recipient: toOwner, Body: contracts.MessageBody{Text: text}}
		if mut != nil {
			mut(&req)
		}
		v, err := e.s.SendMessage(as(sesA), req)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	upd := send("op_u", "Merged #12 and deployed. Nothing urgent.", nil)
	if upd.Status.Ack != contracts.AckAcknowledged || upd.Status.AckRef != contracts.AckRefUpdate {
		t.Fatalf("update: %+v", upd.Status)
	}
	q := send("op_q", "Ready. Should I merge it?", nil)
	ra := send("op_ra", "Prod is down.", func(r *contracts.SendMessageRequest) { r.RequestAck = true })
	for _, v := range []contracts.MessageView{q, ra} {
		if v.Status.Ack != contracts.AckNone {
			t.Fatalf("%q: %+v", v.Envelope.Body.Text, v.Status)
		}
	}
	mine, err := e.s.SendMessage(as(owner), contracts.SendMessageRequest{Mutation: contracts.Mutation{OperationID: "op_o"}, Recipient: contracts.Address{Kind: contracts.AddrSession, ID: sesA.ID}, Body: contracts.MessageBody{Text: "status of the export fix"}})
	if err != nil {
		t.Fatal(err)
	}
	reply := send("op_r", "On it; PR by tonight.", func(r *contracts.SendMessageRequest) { r.InReplyTo = mine.Envelope.MessageID })
	if reply.Status.Ack != contracts.AckNone {
		t.Fatalf("reply to the owner: %+v", reply.Status)
	}
	// An answer to the owner without reply_to (he wrote to the session since
	// its last message to him) is for him; the next report is an update.
	if _, err := e.s.SendMessage(as(owner), contracts.SendMessageRequest{Mutation: contracts.Mutation{OperationID: "op_o2"}, Recipient: contracts.Address{Kind: contracts.AddrSession, ID: sesA.ID}, Body: contracts.MessageBody{Text: "did the deploy finish"}}); err != nil {
		t.Fatal(err)
	}
	if v := send("op_a", "Deploy finished, all green.", nil); v.Status.Ack != contracts.AckNone {
		t.Fatalf("answer without reply_to: %+v", v.Status)
	}
	if v := send("op_a2", "Cleaned up the worktree too.", nil); v.Status.AckRef != contracts.AckRefUpdate {
		t.Fatalf("report after the answer: %+v", v.Status)
	}
	page, err := e.s.ListMessages(as(owner), contracts.ListMessagesRequest{Unacked: true})
	if err != nil || len(page.Items) != 4 {
		t.Fatalf("unacked owner inbox: %v %d", err, len(page.Items))
	}
	// The whole mailbox still has the updates.
	page, _ = e.s.ListMessages(as(owner), contracts.ListMessagesRequest{})
	if len(page.Items) != 6 {
		t.Fatalf("owner inbox: %d", len(page.Items))
	}
}

func TestAckOwnerMail(t *testing.T) {
	e := newEnv(t, Limits{})
	toOwner := contracts.Address{Kind: contracts.AddrOwner}
	ask := contracts.NewID(contracts.PrefixAsk)
	send := func(p contracts.Principal, op, thread, text string) contracts.MessageView {
		v, err := e.s.SendMessage(as(p), contracts.SendMessageRequest{Mutation: contracts.Mutation{OperationID: op}, Recipient: toOwner, ThreadID: thread, RequestAck: true, Body: contracts.MessageBody{Text: text}})
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	carrier := send(sesA, "op_1", ask, "[pyre ask] merge?")
	follow := send(sesA, "op_2", ask, "also: CI is green")
	other := send(sesB, "op_3", "", "b's question")
	plain := send(sesA, "op_4", "", "a's question")
	if _, err := e.s.AckOwnerMail(context.Background(), OwnerMail{}, "x"); err == nil {
		t.Fatal("empty selection accepted")
	}
	n, err := e.s.AckOwnerMail(context.Background(), OwnerMail{ThreadID: ask}, contracts.AckRefAskClosed)
	if err != nil || n != 2 {
		t.Fatalf("thread ack: %d %v", n, err)
	}
	e.clock.Add(time.Minute)
	late := send(sesA, "op_5", "", "a's later question")
	n, err = e.s.AckOwnerMail(context.Background(), OwnerMail{FromSession: sesA.ID, SkipAsks: true, Through: e.clock.Now().Add(-time.Second)}, contracts.AckRefEngaged)
	if err != nil || n != 1 {
		t.Fatalf("session ack: %d %v", n, err)
	}
	want := map[string]contracts.AckState{carrier.Envelope.MessageID: contracts.AckAcknowledged, follow.Envelope.MessageID: contracts.AckAcknowledged,
		other.Envelope.MessageID: contracts.AckNone, plain.Envelope.MessageID: contracts.AckAcknowledged, late.Envelope.MessageID: contracts.AckNone}
	for id, st := range want {
		v, err := e.s.GetMessage(as(owner), contracts.GetRequest{ID: id})
		if err != nil || v.Status.Ack != st {
			t.Fatalf("%s: %v %+v want %s", id, err, v.Status, st)
		}
	}
	evs, err := e.log.ListEvents(context.Background(), contracts.ListEventsRequest{Kinds: []contracts.EventKind{contracts.EvMessageAck}})
	if err != nil || len(evs.Events) != 3 {
		t.Fatalf("ack events: %v %d", err, len(evs.Events))
	}
}
