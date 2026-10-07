package attention

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// Session flags (W-attn). Live case (2026-10-06): a Codex session messaged
// the owner, the owner opened the session and typed into it, and the
// session still said "! needs you" until the item was acked in the Inbox.

func sessionCtx(id string) context.Context {
	return contracts.WithPrincipal(context.Background(), contracts.Principal{Kind: contracts.PrincipalSession, ID: id})
}

// fl gives a request a fresh operation ID unless it carries one.
func fl(r contracts.FlagAttentionRequest) contracts.FlagAttentionRequest {
	if r.OperationID == "" {
		r.OperationID = contracts.NewID(contracts.PrefixOperation)
	}
	return r
}

func (e *env) flag(sid string) (int, *contracts.SessionFlag) {
	e.t.Helper()
	n, f, err := e.store.SessionAttention(context.Background(), sid)
	if err != nil {
		e.t.Fatal(err)
	}
	return n, f
}

func (e *env) ownerToSession(id, sid string, at time.Time) {
	v := contracts.MessageView{Envelope: contracts.MessageEnvelope{MessageID: id, Sender: contracts.Principal{Kind: contracts.PrincipalOwner, ID: "cli_synth-phone"},
		Recipient: contracts.Address{Kind: contracts.AddrSession, ID: sid}, Mode: contracts.ModeQueue, Body: contracts.MessageBody{Text: "synthetic reply"}}}
	b, _ := json.Marshal(v)
	e.log.Append(contracts.Event{Origin: "hub", Kind: contracts.EvMessageStored, Object: contracts.ObjectRef{Kind: contracts.KindMessage, ID: id},
		SessionID: sid, Payload: b, OccurredAt: at})
}

func (e *env) runEnded(sid string, st contracts.RunState, at time.Time) {
	r := contracts.Run{RunID: "run_synth-end", SessionID: sid, State: st}
	b, _ := json.Marshal(r)
	e.log.Append(contracts.Event{Origin: "hub", Kind: contracts.EvRunEnded, Object: contracts.ObjectRef{Kind: contracts.KindRun, ID: r.RunID},
		SessionID: sid, Payload: b, OccurredAt: at})
}

func TestMessageFromSessionIsInformationalNotNeedsYou(t *testing.T) {
	e := newEnv(t)
	e.ownerMessage("msg_synth-1", "", "ses_synth-a")
	e.drain()
	n, f := e.flag("ses_synth-a")
	if n != 1 || f == nil || f.Level != contracts.FlagMessage || f.Reason != "message" || f.Messages != 1 {
		t.Fatalf("flag = %d %+v", n, f)
	}
	// An approval outranks it: the session now needs you.
	e.approval("apr_synth-1", contracts.EvApprovalRequested, nil)
	e.drain()
	if _, f = e.flag("ses_synth-a"); f.Level != contracts.FlagNeedsYou || f.Reason != "approval" || f.Count != 2 || f.Messages != 1 {
		t.Fatalf("with approval: %+v", f)
	}
}

func TestOwnerMessagingSessionAnswersItsMessages(t *testing.T) {
	e := newEnv(t)
	e.ownerMessage("msg_synth-1", "thr_synth-x", "ses_synth-a") // threaded: group message:thr_synth-x
	e.drain()
	// A new (unthreaded) owner message to the session engages with it.
	e.ownerToSession("msg_synth-2", "ses_synth-a", time.Time{})
	e.drain()
	if _, f := e.flag("ses_synth-a"); f != nil {
		t.Fatalf("flag after owner messaged the session: %+v", f)
	}
	items := e.byGroup("message:thr_synth-x")
	if len(items) != 1 || items[0].State != contracts.AttnHandled || items[0].Summary != "You messaged the session." {
		t.Fatalf("item not handled (never deleted): %+v", items)
	}
}

func TestOwnerEngagedAnswersMessagesAndFlagNotApprovals(t *testing.T) {
	e := newEnv(t)
	e.ownerMessage("msg_synth-1", "", "ses_synth-a")
	e.approval("apr_synth-1", contracts.EvApprovalRequested, nil)
	e.drain()
	if _, err := e.store.FlagAttention(sessionCtx("ses_synth-a"), fl(contracts.FlagAttentionRequest{Text: "pick A or B"})); err != nil {
		t.Fatal(err)
	}
	n, err := e.store.OwnerEngaged(context.Background(), "ses_synth-a", "You typed into the session.", EngageKinds...)
	if err != nil || n != 2 {
		t.Fatalf("engaged = %d, %v", n, err)
	}
	_, f := e.flag("ses_synth-a")
	if f == nil || f.Reason != "approval" || f.Count != 1 {
		t.Fatalf("approval must stay until answered: %+v", f)
	}
	// Answering the approval resolves the rest.
	e.approval("apr_synth-1", contracts.EvApprovalResolved, func(a *contracts.Approval) { a.State = contracts.ApprovalResolved })
	e.drain()
	if _, f = e.flag("ses_synth-a"); f != nil {
		t.Fatalf("flag after answer: %+v", f)
	}
	// Nothing left: a second engagement is a cheap no-op.
	if n, err := e.store.OwnerEngaged(context.Background(), "ses_synth-a", "x", EngageKinds...); n != 0 || err != nil {
		t.Fatalf("idle engage = %d, %v", n, err)
	}
	// Other sessions are untouched.
	e.ownerMessage("msg_synth-b", "", "ses_synth-b")
	e.drain()
	if n, _ := e.store.OwnerEngaged(context.Background(), "ses_synth-a", "x", EngageKinds...); n != 0 {
		t.Fatal("engaging one session answered another")
	}
	if _, f = e.flag("ses_synth-b"); f == nil {
		t.Fatal("ses_synth-b lost its message")
	}
}

func TestReadKindsAnswerOnlyMessages(t *testing.T) {
	e := newEnv(t)
	e.ownerMessage("msg_synth-1", "", "ses_synth-a")
	e.drain()
	if _, err := e.store.FlagAttention(sessionCtx("ses_synth-a"), fl(contracts.FlagAttentionRequest{Text: "need a token"})); err != nil {
		t.Fatal(err)
	}
	if n, err := e.store.OwnerEngaged(context.Background(), "ses_synth-a", "Read.", ReadKinds...); n != 1 || err != nil {
		t.Fatalf("read = %d %v", n, err)
	}
	if _, f := e.flag("ses_synth-a"); f == nil || f.Reason != "asked" || f.Text != "need a token" {
		t.Fatalf("viewing a message must not clear the session's own ask: %+v", f)
	}
}

func TestFlagAttentionAuthorizationAndClear(t *testing.T) {
	e := newEnv(t)
	if _, err := e.store.FlagAttention(sessionCtx("ses_synth-b"), fl(contracts.FlagAttentionRequest{SessionID: "ses_synth-a", Text: "x"})); !contracts.IsCode(err, contracts.ErrForbidden) {
		t.Fatalf("another session flagged a: %v", err)
	}
	node := contracts.WithPrincipal(context.Background(), contracts.Principal{Kind: contracts.PrincipalNode, ID: "node_synth"})
	if _, err := e.store.FlagAttention(node, fl(contracts.FlagAttentionRequest{SessionID: "ses_synth-a", Text: "x"})); !contracts.IsCode(err, contracts.ErrForbidden) {
		t.Fatalf("node flagged: %v", err)
	}
	if _, err := e.store.FlagAttention(ownerCtx(), fl(contracts.FlagAttentionRequest{Text: "x"})); !contracts.IsCode(err, contracts.ErrInvalid) {
		t.Fatalf("owner without session: %v", err)
	}
	it, err := e.store.FlagAttention(sessionCtx("ses_synth-a"), fl(contracts.FlagAttentionRequest{Text: "choose\nschema   A or B"}))
	if err != nil || it.Kind != contracts.AttnFlag || it.SessionID != "ses_synth-a" || it.Title != "choose schema A or B" || it.State != contracts.AttnOpen {
		t.Fatalf("flag = %+v, %v", it, err)
	}
	// Re-flagging collapses into the same item.
	again, err := e.store.FlagAttention(sessionCtx("ses_synth-a"), fl(contracts.FlagAttentionRequest{Text: "still need A or B"}))
	if err != nil || again.AttentionID != it.AttentionID || again.Count != 2 {
		t.Fatalf("reflag = %+v, %v", again, err)
	}
	if _, f := e.flag("ses_synth-a"); f == nil || f.Level != contracts.FlagNeedsYou || f.Reason != "asked" {
		t.Fatalf("flag = %+v", f)
	}
	cl, err := e.store.FlagAttention(sessionCtx("ses_synth-a"), fl(contracts.FlagAttentionRequest{Clear: true}))
	if err != nil || cl.State != contracts.AttnHandled || cl.Summary != "Cleared by the session." {
		t.Fatalf("clear = %+v, %v", cl, err)
	}
	if _, f := e.flag("ses_synth-a"); f != nil {
		t.Fatalf("flag after clear: %+v", f)
	}
	// Clearing with nothing flagged is not an error.
	if cl, err := e.store.FlagAttention(sessionCtx("ses_synth-a"), fl(contracts.FlagAttentionRequest{Clear: true})); err != nil || cl.AttentionID != "" {
		t.Fatalf("idle clear = %+v, %v", cl, err)
	}
	// The owner may clear a session's flag.
	if _, err := e.store.FlagAttention(sessionCtx("ses_synth-a"), fl(contracts.FlagAttentionRequest{Text: "again"})); err != nil {
		t.Fatal(err)
	}
	if cl, err := e.store.FlagAttention(ownerCtx(), fl(contracts.FlagAttentionRequest{SessionID: "ses_synth-a", Clear: true})); err != nil || cl.Summary != "Cleared." {
		t.Fatalf("owner clear = %+v, %v", cl, err)
	}
	// Idempotent by operation ID.
	req := fl(contracts.FlagAttentionRequest{Mutation: contracts.Mutation{OperationID: "op_synth-flag"}, Text: "once"})
	a, _ := e.store.FlagAttention(sessionCtx("ses_synth-a"), req)
	b, _ := e.store.FlagAttention(sessionCtx("ses_synth-a"), req)
	if a.AttentionID == "" || a.AttentionID != b.AttentionID || b.Count != 1 {
		t.Fatalf("idempotent flag: %+v %+v", a, b)
	}
}

func TestRunEndWithdrawsOwnFlagButNotANewerOne(t *testing.T) {
	e := newEnv(t)
	if _, err := e.store.FlagAttention(sessionCtx("ses_synth-a"), fl(contracts.FlagAttentionRequest{Text: "need you"})); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(time.Minute)
	e.runEnded("ses_synth-a", contracts.RunExited, e.clock.Now())
	e.drain()
	if _, f := e.flag("ses_synth-a"); f != nil {
		t.Fatalf("flag survived its run: %+v", f)
	}
	items := e.byGroup("flag:session:ses_synth-a")
	if len(items) != 1 || items[0].State != contracts.AttnWithdrawn {
		t.Fatalf("items: %+v", items)
	}
	// A run end processed late (it happened before a newer flag) keeps it.
	ended := e.clock.Now()
	e.clock.Advance(time.Minute)
	if _, err := e.store.FlagAttention(sessionCtx("ses_synth-a"), fl(contracts.FlagAttentionRequest{Text: "new run needs you"})); err != nil {
		t.Fatal(err)
	}
	e.runEnded("ses_synth-a", contracts.RunExited, ended)
	e.drain()
	if _, f := e.flag("ses_synth-a"); f == nil || f.Text != "new run needs you" {
		t.Fatalf("newer flag cleared by an older run end: %+v", f)
	}
	// Same for a late owner message: it answers only what it saw.
	e.ownerToSession("msg_synth-old", "ses_synth-a", ended)
	e.drain()
	if _, f := e.flag("ses_synth-a"); f == nil {
		t.Fatal("newer flag cleared by an older owner message")
	}
}

func TestReadMessageStopsFlaggingReadApprovalDoesNot(t *testing.T) {
	e := newEnv(t)
	e.ownerMessage("msg_synth-1", "", "ses_synth-a")
	e.approval("apr_synth-1", contracts.EvApprovalRequested, nil)
	e.drain()
	for _, it := range e.list() {
		if _, err := e.store.UpdateAttention(ownerCtx(), contracts.UpdateAttentionRequest{Mutation: contracts.Mutation{OperationID: contracts.NewID(contracts.PrefixOperation)}, AttentionID: it.AttentionID, State: contracts.AttnRead}); err != nil {
			t.Fatal(err)
		}
	}
	n, f := e.flag("ses_synth-a")
	if n != 0 || f == nil || f.Reason != "approval" || f.Count != 1 || f.Messages != 0 {
		t.Fatalf("after read: %d %+v", n, f)
	}
}
