package conformance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"testing"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
)

type suite struct {
	t   *testing.T
	tg  *Target
	ctx context.Context
	api c.API
}

var (
	ownerA = c.Principal{Kind: c.PrincipalOwner, ID: "cli_conformance-a", Name: "conformance owner A"}
	ownerB = c.Principal{Kind: c.PrincipalOwner, ID: "cli_conformance-b", Name: "conformance owner B"}
)

func (s *suite) owner() context.Context  { return s.tg.As(s.ctx, ownerA) }
func (s *suite) ownerB() context.Context { return s.tg.As(s.ctx, ownerB) }
func (s *suite) session(id string) context.Context {
	return s.tg.As(s.ctx, c.Principal{Kind: c.PrincipalSession, ID: id})
}

func newOp() c.Mutation { return c.Mutation{OperationID: c.NewID(c.PrefixOperation)} }

func (s *suite) must(err error, what string) {
	s.t.Helper()
	if err != nil {
		s.t.Fatalf("%s: %v", what, err)
	}
}

func (s *suite) wantCode(err error, code c.ErrorCode, what string) *c.Error {
	s.t.Helper()
	if !c.IsCode(err, code) {
		s.t.Fatalf("%s: want %s, got %v", what, code, err)
	}
	return c.AsError(err)
}

// start launches a standalone session and, when the target can simulate
// node observations, marks its run idle so deliveries can proceed.
func (s *suite) start(title string) c.SessionView {
	s.t.Helper()
	resp, err := s.api.StartSession(s.owner(), c.StartSessionRequest{Mutation: newOp(), NodeID: s.tg.NodeID, Harness: s.tg.Harness, Title: title, Cwd: s.tg.Cwd})
	s.must(err, "start session")
	if resp.Session.Run == nil {
		s.t.Fatal("started session has no run")
	}
	if sim, ok := s.tg.Sim.(RunStateSimulator); ok {
		s.must(sim.SetRunState(s.ctx, resp.Session.Run.RunID, c.RunWaitingInput), "simulate idle run")
	}
	return resp.Session
}

func (s *suite) send(from, to string, text string) c.MessageView {
	s.t.Helper()
	m, err := s.api.SendMessage(s.session(from), c.SendMessageRequest{Mutation: newOp(), Recipient: c.Address{Kind: c.AddrSession, ID: to}, Body: c.MessageBody{Text: text}})
	s.must(err, "send")
	return m
}

func (s *suite) msg(id string) c.MessageView {
	s.t.Helper()
	m, err := s.api.GetMessage(s.owner(), c.GetRequest{ID: id})
	s.must(err, "get message")
	return m
}

func digest(b []byte) c.Digest {
	h := sha256.Sum256(b)
	return c.Digest("sha256:" + hex.EncodeToString(h[:]))
}

// publish uploads data in one chunk and finalizes.
func (s *suite) publish(ctx context.Context, artifactID string, data []byte, declared c.Digest) (c.PublishReceipt, error) {
	if declared == "" {
		declared = digest(data)
	}
	req := c.BeginUploadRequest{Mutation: newOp(), ArtifactID: artifactID, Filename: "report.txt", MediaType: "text/plain", Size: int64(len(data)), Digest: declared}
	if artifactID == "" {
		req.Title = "conformance report"
	}
	up, err := s.api.BeginUpload(ctx, req)
	if err != nil {
		return c.PublishReceipt{}, err
	}
	if _, err := s.api.UploadChunk(ctx, up.UploadID, 0, bytes.NewReader(data)); err != nil {
		return c.PublishReceipt{}, err
	}
	return s.api.FinalizeUpload(ctx, c.FinalizeUploadRequest{Mutation: newOp(), UploadID: up.UploadID})
}

// ---- cases ----

func testUnauthenticated(t *testing.T, s *suite) {
	if _, err := s.api.WhoAmI(context.Background()); !c.IsCode(err, c.ErrUnauthorized) {
		t.Fatalf("call without principal: want unauthorized, got %v", err)
	}
	p, err := s.api.WhoAmI(s.owner())
	s.must(err, "whoami")
	if p.Kind != c.PrincipalOwner || p.ID != ownerA.ID {
		t.Fatalf("whoami %+v", p)
	}
}

func testStandalone(t *testing.T, s *suite) {
	sv := s.start("standalone")
	if sv.Session.ProjectID != nil {
		t.Fatal("standalone session must have null project_id")
	}
	empty := ""
	page, err := s.api.ListSessions(s.owner(), c.ListSessionsRequest{ProjectID: &empty, PageRequest: c.PageRequest{Limit: 500}})
	s.must(err, "list standalone")
	found := false
	for _, x := range page.Items {
		if x.Session.ProjectID != nil {
			t.Fatal("standalone filter returned a project session")
		}
		found = found || x.Session.SessionID == sv.Session.SessionID
	}
	if !found {
		t.Fatal("standalone session missing from standalone listing")
	}
	// Messaging, notes and artifacts work without a project.
	other := s.start("peer")
	m := s.send(other.Session.SessionID, sv.Session.SessionID, "hello standalone")
	if m.Envelope.ProjectID != nil || m.Status.Storage != c.StorageStored {
		t.Fatalf("message %+v", m)
	}
	n, err := s.api.CreateNote(s.session(sv.Session.SessionID), c.CreateNoteRequest{Mutation: newOp(), Kind: c.NoteHandoff, Title: "h", Body: "b"})
	s.must(err, "note without project")
	if n.ProjectID != nil {
		t.Fatal("note gained a project")
	}
	rec, err := s.publish(s.session(sv.Session.SessionID), "", []byte("standalone artifact"), "")
	s.must(err, "publish without project")
	if rec.Artifact.ProjectID != nil {
		t.Fatal("artifact gained a project")
	}
}

func testIdempotency(t *testing.T, s *suite) {
	req := c.CreateProjectRequest{Mutation: newOp(), Name: "idempotent"}
	p1, err := s.api.CreateProject(s.owner(), req)
	s.must(err, "create")
	p2, err := s.api.CreateProject(s.owner(), req)
	s.must(err, "replay")
	if p1.ProjectID != p2.ProjectID {
		t.Fatalf("replay created a second project: %s vs %s", p1.ProjectID, p2.ProjectID)
	}
	all, _ := s.api.ListProjects(s.owner(), c.ListProjectsRequest{Query: "idempotent", PageRequest: c.PageRequest{Limit: 500}})
	count := 0
	for _, p := range all.Items {
		if p.Name == "idempotent" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("%d projects after replay", count)
	}
	diff := req
	diff.Name = "different"
	e := s.wantCode(func() error { _, err := s.api.CreateProject(s.owner(), diff); return err }(), c.ErrConflict, "same op, different body")
	if e.Ref == nil || e.Ref.Kind != c.KindOperation {
		t.Fatalf("conflict should reference the operation: %+v", e)
	}
	// Same operation id on a different op is also a conflict.
	_, err = s.api.CreateNote(s.owner(), c.CreateNoteRequest{Mutation: req.Mutation, Kind: c.NoteNote, Title: "x", Body: "y"})
	s.wantCode(err, c.ErrConflict, "same op id on another operation")
	// Missing operation id is rejected by servers (HTTP clients may instead
	// generate one before sending, which is also conformant).
	if _, err = s.api.CreateProject(s.owner(), c.CreateProjectRequest{Name: "no-op"}); err != nil {
		s.wantCode(err, c.ErrInvalid, "missing operation_id")
	}
	// Start session replay returns the same session.
	sreq := c.StartSessionRequest{Mutation: newOp(), NodeID: s.tg.NodeID, Harness: s.tg.Harness, Cwd: s.tg.Cwd}
	r1, err := s.api.StartSession(s.owner(), sreq)
	s.must(err, "start")
	r2, err := s.api.StartSession(s.owner(), sreq)
	s.must(err, "start replay")
	if r1.Session.Session.SessionID != r2.Session.Session.SessionID {
		t.Fatal("start replay launched a second session")
	}
}

func testMessageDedup(t *testing.T, s *suite) {
	a, b := s.start("a"), s.start("b")
	msgID := c.NewID(c.PrefixMessage)
	req := c.SendMessageRequest{Mutation: newOp(), MessageID: msgID, Recipient: c.Address{Kind: c.AddrSession, ID: b.Session.SessionID}, Body: c.MessageBody{Text: "run lint"}}
	m1, err := s.api.SendMessage(s.session(a.Session.SessionID), req)
	s.must(err, "send")
	req2 := req
	req2.Mutation = newOp() // outbox retry with a new operation id
	m2, err := s.api.SendMessage(s.session(a.Session.SessionID), req2)
	s.must(err, "resend same message_id")
	if m2.Envelope.MessageID != msgID || m2.Cursor != m1.Cursor || m2.Envelope.OperationID != m1.Envelope.OperationID {
		t.Fatalf("dedup returned a different message: %+v", m2.Envelope)
	}
	req3 := req2
	req3.Mutation = newOp()
	req3.Body.Text = "run lint AND deploy"
	_, err = s.api.SendMessage(s.session(a.Session.SessionID), req3)
	e := s.wantCode(err, c.ErrConflict, "same message_id, different content")
	if e.Ref == nil || e.Ref.ID != msgID {
		t.Fatalf("conflict ref %+v", e.Ref)
	}
	inbox, err := s.api.ListMessages(s.session(b.Session.SessionID), c.ListMessagesRequest{Box: "inbox", Address: &c.Address{Kind: c.AddrSession, ID: b.Session.SessionID}})
	s.must(err, "inbox")
	n := 0
	for _, m := range inbox.Items {
		if m.Envelope.MessageID == msgID {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("message stored %d times", n)
	}
	// A session cannot claim another sender.
	if m1.Envelope.Sender.Kind != c.PrincipalSession || m1.Envelope.Sender.ID != a.Session.SessionID {
		t.Fatalf("sender %+v", m1.Envelope.Sender)
	}
}

func testDeliveryFacts(t *testing.T, s *suite) {
	a, b := s.start("a"), s.start("b")
	m := s.send(a.Session.SessionID, b.Session.SessionID, "please review")
	st := m.Status
	if st.Storage != c.StorageStored || st.StoredAt == nil {
		t.Fatalf("storage %+v", st)
	}
	if st.Ack != c.AckNone || st.OutcomeRef != nil {
		t.Fatalf("ack/outcome must start empty: %+v", st)
	}
	switch st.Runtime {
	case c.DeliveryQueued, c.DeliveryNodeReceived, c.DeliveryAttempting, c.DeliveryAccepted:
	default:
		t.Fatalf("unexpected initial runtime state %s", st.Runtime)
	}
	if st.Runtime == c.DeliveryQueued && st.BoundRunID != "" {
		t.Fatal("queued message must not be bound to a run")
	}
	info, err := s.api.SendMessage(s.session(a.Session.SessionID), c.SendMessageRequest{Mutation: newOp(), Recipient: c.Address{Kind: c.AddrSession, ID: b.Session.SessionID}, Mode: c.ModeInfo, Body: c.MessageBody{Text: "fyi"}})
	s.must(err, "info")
	if info.Status.Runtime != c.DeliveryNotApplicable {
		t.Fatalf("info message runtime %s", info.Status.Runtime)
	}
	toOwner, err := s.api.SendMessage(s.session(a.Session.SessionID), c.SendMessageRequest{Mutation: newOp(), Recipient: c.Address{Kind: c.AddrOwner, ID: "owner"}, Body: c.MessageBody{Text: "need input"}})
	s.must(err, "to owner")
	if toOwner.Status.Runtime != c.DeliveryNotApplicable {
		t.Fatalf("owner message runtime %s", toOwner.Status.Runtime)
	}
	_, err = s.api.SendMessage(s.session(a.Session.SessionID), c.SendMessageRequest{Mutation: newOp(), Recipient: c.Address{Kind: c.AddrSession, ID: "ses_does-not-exist"}, Body: c.MessageBody{Text: "x"}})
	s.wantCode(err, c.ErrNotFound, "unknown recipient")

	sim := need[DeliverySimulator](t, s.tg, "DeliverySimulator")
	s.must(sim.SettleDelivery(s.ctx), "settle")
	got := s.msg(m.Envelope.MessageID).Status
	if got.Runtime != c.DeliveryAccepted {
		t.Fatalf("after settle runtime %s (%s)", got.Runtime, got.RuntimeReason)
	}
	if got.BoundRunID != b.Run.RunID || len(got.Receipts) == 0 {
		t.Fatalf("accepted message must be bound with receipts: %+v", got)
	}
	last := got.Receipts[len(got.Receipts)-1]
	if !c.AcceptanceEvidence(last.Evidence) {
		t.Fatalf("accepted without acceptance evidence: %s", last.Evidence)
	}
	if got.Ack != c.AckNone {
		t.Fatal("runtime acceptance must not imply acknowledgement")
	}
}

func testAckReply(t *testing.T, s *suite) {
	a, b := s.start("a"), s.start("b")
	m := s.send(a.Session.SessionID, b.Session.SessionID, "status?")
	_, err := s.api.AckMessage(s.session(a.Session.SessionID), c.AckMessageRequest{Mutation: newOp(), MessageID: m.Envelope.MessageID})
	s.wantCode(err, c.ErrForbidden, "sender acking its own outbound message")
	acked, err := s.api.AckMessage(s.session(b.Session.SessionID), c.AckMessageRequest{Mutation: newOp(), MessageID: m.Envelope.MessageID})
	s.must(err, "ack")
	if acked.Status.Ack != c.AckAcknowledged {
		t.Fatalf("ack state %s", acked.Status.Ack)
	}
	m2 := s.send(a.Session.SessionID, b.Session.SessionID, "and now?")
	reply, err := s.api.SendMessage(s.session(b.Session.SessionID), c.SendMessageRequest{Mutation: newOp(), Recipient: c.Address{Kind: c.AddrSession, ID: a.Session.SessionID}, InReplyTo: m2.Envelope.MessageID, Body: c.MessageBody{Text: "done"}})
	s.must(err, "reply")
	if reply.Envelope.InReplyTo != m2.Envelope.MessageID {
		t.Fatal("reply lost in_reply_to")
	}
	orig := s.msg(m2.Envelope.MessageID)
	if orig.Status.Ack != c.AckReplied || orig.Status.AckRef != reply.Envelope.MessageID {
		t.Fatalf("original after reply: %+v", orig.Status)
	}
	unacked, err := s.api.ListMessages(s.session(b.Session.SessionID), c.ListMessagesRequest{Address: &c.Address{Kind: c.AddrSession, ID: b.Session.SessionID}, Unacked: true})
	s.must(err, "unacked")
	for _, x := range unacked.Items {
		if x.Envelope.MessageID == m.Envelope.MessageID || x.Envelope.MessageID == m2.Envelope.MessageID {
			t.Fatal("acked/replied message listed as unacked")
		}
	}
}

func testCancelReissue(t *testing.T, s *suite) {
	a, b := s.start("a"), s.start("b")
	if sim, ok := s.tg.Sim.(RunStateSimulator); ok {
		s.must(sim.SetRunState(s.ctx, b.Run.RunID, c.RunRunning), "busy recipient")
	}
	m := s.send(a.Session.SessionID, b.Session.SessionID, "work item")
	if st := s.msg(m.Envelope.MessageID).Status.Runtime; st == c.DeliveryQueued || st == c.DeliveryNodeReceived {
		_, err := s.api.ReissueMessage(s.session(a.Session.SessionID), c.ReissueMessageRequest{Mutation: newOp(), MessageID: m.Envelope.MessageID, AcceptDuplicateRisk: true})
		s.wantCode(err, c.ErrConflict, "reissue of in-flight message")
		_, err = s.api.CancelMessage(s.session(b.Session.SessionID), c.CancelMessageRequest{Mutation: newOp(), MessageID: m.Envelope.MessageID})
		s.wantCode(err, c.ErrForbidden, "recipient cancelling sender's message")
		cm, err := s.api.CancelMessage(s.session(a.Session.SessionID), c.CancelMessageRequest{Mutation: newOp(), MessageID: m.Envelope.MessageID})
		s.must(err, "cancel")
		if cm.Status.Runtime != c.DeliveryCancelled || cm.Status.RuntimeReason == "" {
			t.Fatalf("cancel status %+v", cm.Status)
		}
		re, err := s.api.ReissueMessage(s.session(a.Session.SessionID), c.ReissueMessageRequest{Mutation: newOp(), MessageID: m.Envelope.MessageID})
		s.must(err, "reissue cancelled")
		if re.Envelope.MessageID == m.Envelope.MessageID || re.Envelope.Reissues != m.Envelope.MessageID || re.Status.BoundRunID != "" {
			t.Fatalf("reissue must be a new unbound message referencing the original: %+v", re)
		}
		if s.msg(m.Envelope.MessageID).Status.Runtime != c.DeliveryCancelled {
			t.Fatal("original changed by reissue")
		}
	} else {
		t.Skip("target delivered immediately; cannot observe an in-flight message")
	}
}

func testIndeterminate(t *testing.T, s *suite) {
	rep := need[DeliveryReporter](t, s.tg, "DeliveryReporter")
	a, b := s.start("a"), s.start("b")
	m := s.send(a.Session.SessionID, b.Session.SessionID, "delete the temp branch")
	s.must(rep.ReportDelivery(s.ctx, m.Envelope.MessageID, c.DeliveryIndeterminate), "report indeterminate")
	cur := s.msg(m.Envelope.MessageID)
	if cur.Status.Runtime != c.DeliveryIndeterminate || cur.Status.BoundRunID != b.Run.RunID {
		t.Fatalf("indeterminate status %+v", cur.Status)
	}
	att, err := s.api.ListAttention(s.owner(), c.ListAttentionRequest{Kinds: []c.AttentionKind{c.AttnIndeterminate}})
	s.must(err, "attention")
	if !hasObject(att.Items, m.Envelope.MessageID) {
		t.Fatal("indeterminate delivery raised no attention item")
	}
	_, err = s.api.ReissueMessage(s.session(a.Session.SessionID), c.ReissueMessageRequest{Mutation: newOp(), MessageID: m.Envelope.MessageID})
	s.wantCode(err, c.ErrConflict, "reissue indeterminate without accepting duplicate risk")
	re, err := s.api.ReissueMessage(s.session(a.Session.SessionID), c.ReissueMessageRequest{Mutation: newOp(), MessageID: m.Envelope.MessageID, AcceptDuplicateRisk: true})
	s.must(err, "reissue with risk")
	if re.Envelope.Reissues != m.Envelope.MessageID || re.Envelope.MessageID == m.Envelope.MessageID {
		t.Fatalf("reissue %+v", re.Envelope)
	}
	orig := s.msg(m.Envelope.MessageID)
	if orig.Status.Runtime != c.DeliveryIndeterminate || orig.Status.BoundRunID != b.Run.RunID {
		t.Fatalf("original must keep its state and binding: %+v", orig.Status)
	}
	_, err = s.api.ResolveDelivery(s.owner(), c.ResolveDeliveryRequest{Mutation: newOp(), MessageID: m.Envelope.MessageID, Resolution: c.DeliveryQueued})
	s.wantCode(err, c.ErrInvalid, "resolve to a non-terminal state")
	res, err := s.api.ResolveDelivery(s.owner(), c.ResolveDeliveryRequest{Mutation: newOp(), MessageID: m.Envelope.MessageID, Resolution: c.DeliveryAccepted, Note: "found in transcript"})
	s.must(err, "resolve")
	if res.Status.Runtime != c.DeliveryAccepted || res.Status.BoundRunID != b.Run.RunID {
		t.Fatalf("resolved %+v", res.Status)
	}
	_, err = s.api.ResolveDelivery(s.owner(), c.ResolveDeliveryRequest{Mutation: newOp(), MessageID: m.Envelope.MessageID, Resolution: c.DeliveryRejected})
	s.wantCode(err, c.ErrConflict, "resolve twice")
	_, err = s.api.ReissueMessage(s.owner(), c.ReissueMessageRequest{Mutation: newOp(), MessageID: m.Envelope.MessageID, AcceptDuplicateRisk: true})
	s.wantCode(err, c.ErrConflict, "reissue of accepted message")

	// Never rebind: a bound message keeps its run after a replacement run.
	rs := need[RunStateSimulator](t, s.tg, "RunStateSimulator")
	s.must(rs.SetRunState(s.ctx, b.Run.RunID, c.RunExited), "exit run")
	resumed, err := s.api.ResumeSession(s.owner(), c.ResumeRequest{Mutation: newOp(), SessionID: b.Session.SessionID, AllowHandoff: true})
	if c.IsCode(err, c.ErrUnsupported) {
		t.Skip("resume unsupported by target")
	}
	s.must(err, "resume")
	if resumed.Session.Run == nil || resumed.Session.Run.RunID == b.Run.RunID {
		t.Fatal("resume must create a new run")
	}
	if got := s.msg(m.Envelope.MessageID).Status.BoundRunID; got != b.Run.RunID {
		t.Fatalf("message rebound from %s to %s", b.Run.RunID, got)
	}
}

func hasObject(items []c.AttentionItem, id string) bool {
	for _, a := range items {
		if a.Object.ID == id {
			return true
		}
	}
	return false
}

func testLease(t *testing.T, s *suite) {
	sv := s.start("leased")
	l1, err := s.api.AcquireLease(s.owner(), c.LeaseRequest{Mutation: newOp(), RunID: sv.Run.RunID, TTLSeconds: 120})
	s.must(err, "acquire")
	if l1.Holder.ID != ownerA.ID || !l1.ExpiresAt.After(l1.AcquiredAt) {
		t.Fatalf("lease %+v", l1)
	}
	_, err = s.api.AcquireLease(s.ownerB(), c.LeaseRequest{Mutation: newOp(), RunID: sv.Run.RunID})
	e := s.wantCode(err, c.ErrConflict, "competing acquire")
	if e.Ref == nil || e.Ref.ID != l1.LeaseID {
		t.Fatalf("conflict should reference the held lease: %+v", e.Ref)
	}
	renew, err := s.api.AcquireLease(s.owner(), c.LeaseRequest{Mutation: newOp(), RunID: sv.Run.RunID})
	s.must(err, "holder renew")
	if renew.LeaseID != l1.LeaseID {
		t.Fatal("renewal changed the lease id")
	}
	l2, err := s.api.AcquireLease(s.ownerB(), c.LeaseRequest{Mutation: newOp(), RunID: sv.Run.RunID, Takeover: true})
	s.must(err, "takeover")
	if l2.TakeoverOf != l1.LeaseID || l2.Holder.ID != ownerB.ID {
		t.Fatalf("takeover not recorded: %+v", l2)
	}
	_, err = s.api.AcquireLease(s.owner(), c.LeaseRequest{Mutation: newOp(), RunID: sv.Run.RunID})
	s.wantCode(err, c.ErrConflict, "former holder after takeover")
	rel, err := s.api.ReleaseLease(s.ownerB(), c.LeaseReleaseRequest{Mutation: newOp(), LeaseID: l2.LeaseID})
	s.must(err, "release")
	if rel.ReleasedAt == nil {
		t.Fatal("release not recorded")
	}
	got, err := s.api.GetSession(s.owner(), c.GetRequest{ID: sv.Session.SessionID})
	s.must(err, "get session")
	if got.Run.Lease != nil && got.Run.Lease.Active(time.Now()) && got.Run.Lease.ReleasedAt == nil {
		t.Fatal("released lease still shown active")
	}
	// Structured mail is not blocked by a lease.
	_, err = s.api.AcquireLease(s.owner(), c.LeaseRequest{Mutation: newOp(), RunID: sv.Run.RunID})
	s.must(err, "re-acquire")
	peer := s.start("peer")
	m := s.send(peer.Session.SessionID, sv.Session.SessionID, "fyi build green")
	if m.Status.Storage != c.StorageStored {
		t.Fatal("message refused during lease")
	}
}

func testApprovals(t *testing.T, s *suite) {
	sim := need[ApprovalSimulator](t, s.tg, "ApprovalSimulator")
	sv := s.start("approvals")
	ap, err := sim.RequestApproval(s.ctx, sv.Run.RunID, 0)
	s.must(err, "request approval")
	if ap.State != c.ApprovalPending || len(ap.Options) == 0 {
		t.Fatalf("approval %+v", ap)
	}
	att, _ := s.api.ListAttention(s.owner(), c.ListAttentionRequest{})
	if !hasObject(att.Items, ap.ApprovalID) {
		t.Fatal("pending approval has no attention item")
	}
	_, err = s.api.AnswerApproval(s.session(sv.Session.SessionID), c.AnswerApprovalRequest{Mutation: newOp(), ApprovalID: ap.ApprovalID, RunID: sv.Run.RunID, OptionID: ap.Options[0].ID})
	s.wantCode(err, c.ErrForbidden, "agent session answering its own approval")
	_, err = s.api.AnswerApproval(s.owner(), c.AnswerApprovalRequest{Mutation: newOp(), ApprovalID: ap.ApprovalID, RunID: "run_wrong", OptionID: ap.Options[0].ID})
	s.wantCode(err, c.ErrConflict, "answer for a different run")
	req := c.AnswerApprovalRequest{Mutation: newOp(), ApprovalID: ap.ApprovalID, RunID: sv.Run.RunID, OptionID: ap.Options[0].ID}
	r1, err := s.api.AnswerApproval(s.owner(), req)
	s.must(err, "answer")
	if r1.State != c.ApprovalResolved || r1.Resolution == nil || r1.Resolution.Source != "pyre" || r1.Resolution.Actor.ID != ownerA.ID {
		t.Fatalf("resolution %+v", r1.Resolution)
	}
	r2, err := s.api.AnswerApproval(s.owner(), req)
	s.must(err, "double tap (same operation)")
	if r2.Resolution == nil || r2.Resolution.ResolvedAt != r1.Resolution.ResolvedAt {
		t.Fatal("double tap must return the stored result")
	}
	_, err = s.api.AnswerApproval(s.ownerB(), c.AnswerApprovalRequest{Mutation: newOp(), ApprovalID: ap.ApprovalID, RunID: sv.Run.RunID, OptionID: ap.Options[len(ap.Options)-1].ID})
	s.wantCode(err, c.ErrConflict, "second answer")
	att, _ = s.api.ListAttention(s.owner(), c.ListAttentionRequest{})
	if hasObject(att.Items, ap.ApprovalID) {
		t.Fatal("resolved approval still pending in attention")
	}
	// Natively resolved approval: stale link sees the existing resolution.
	ap2, err := sim.RequestApproval(s.ctx, sv.Run.RunID, 0)
	s.must(err, "second approval")
	s.must(sim.ResolveApprovalNatively(s.ctx, ap2.ApprovalID, ap2.Options[0].ID), "native resolve")
	_, err = s.api.AnswerApproval(s.owner(), c.AnswerApprovalRequest{Mutation: newOp(), ApprovalID: ap2.ApprovalID, RunID: sv.Run.RunID, OptionID: ap2.Options[0].ID})
	s.wantCode(err, c.ErrConflict, "stale link after native resolution")
	got, err := s.api.GetApproval(s.owner(), c.GetRequest{ID: ap2.ApprovalID})
	s.must(err, "get")
	if got.Resolution == nil || got.Resolution.Source != "native" {
		t.Fatalf("native resolution lost: %+v", got.Resolution)
	}
}

func testApprovalExpiry(t *testing.T, s *suite) {
	sim := need[ApprovalSimulator](t, s.tg, "ApprovalSimulator")
	sv := s.start("expiry")
	if clk, ok := s.tg.Sim.(ClockAdvancer); ok {
		ap, err := sim.RequestApproval(s.ctx, sv.Run.RunID, time.Minute)
		s.must(err, "request")
		clk.AdvanceClock(2 * time.Minute)
		_, err = s.api.AnswerApproval(s.owner(), c.AnswerApprovalRequest{Mutation: newOp(), ApprovalID: ap.ApprovalID, RunID: sv.Run.RunID, OptionID: ap.Options[0].ID})
		s.wantCode(err, c.ErrExpired, "answer after expiry")
	}
	rs := need[RunStateSimulator](t, s.tg, "RunStateSimulator")
	ap, err := sim.RequestApproval(s.ctx, sv.Run.RunID, 0)
	s.must(err, "request")
	s.must(rs.SetRunState(s.ctx, sv.Run.RunID, c.RunExited), "run ends")
	_, err = s.api.AnswerApproval(s.owner(), c.AnswerApprovalRequest{Mutation: newOp(), ApprovalID: ap.ApprovalID, RunID: sv.Run.RunID, OptionID: ap.Options[0].ID})
	s.wantCode(err, c.ErrExpired, "answer after the run ended")
	got, _ := s.api.GetApproval(s.owner(), c.GetRequest{ID: ap.ApprovalID})
	if got.State != c.ApprovalWithdrawn && got.State != c.ApprovalExpired {
		t.Fatalf("approval of ended run is %s", got.State)
	}
}

func testDocumentCAS(t *testing.T, s *suite) {
	d, err := s.api.CreateDocument(s.owner(), c.CreateDocumentRequest{Mutation: newOp(), Title: "Brief", Content: "# Brief\nGoal: X.\n"})
	s.must(err, "create doc")
	rev1 := d.Revision.RevisionID
	if d.Document.ProjectID != nil || d.Revision.Digest != digest([]byte("# Brief\nGoal: X.\n")) {
		t.Fatalf("doc %+v", d)
	}
	alice, bob := s.start("alice"), s.start("bob")
	d2, err := s.api.ProposeRevision(s.session(alice.Session.SessionID), c.ProposeRevisionRequest{Mutation: newOp(), DocumentID: d.Document.DocumentID, BaseRevisionID: rev1, Content: "# Brief\nGoal: X.\nConstraint: stable API.\n"})
	s.must(err, "first propose")
	if d2.Revision.ParentRevisionID != rev1 || d2.Document.HeadRevisionID != d2.Revision.RevisionID {
		t.Fatalf("rev2 %+v", d2.Revision)
	}
	bobContent := "# Brief\nGoal: X and Y.\n"
	_, err = s.api.ProposeRevision(s.session(bob.Session.SessionID), c.ProposeRevisionRequest{Mutation: newOp(), DocumentID: d.Document.DocumentID, BaseRevisionID: rev1, Content: bobContent})
	e := s.wantCode(err, c.ErrConflict, "stale propose")
	head, _ := e.Details["current_head"].(string)
	propID, _ := e.Details["proposal_id"].(string)
	if head != d2.Revision.RevisionID || propID == "" {
		t.Fatalf("conflict details %+v", e.Details)
	}
	cur, err := s.api.GetDocument(s.owner(), c.GetDocumentRequest{DocumentID: d.Document.DocumentID})
	s.must(err, "get head")
	if cur.Document.HeadRevisionID != d2.Revision.RevisionID || cur.Revision.Content == bobContent {
		t.Fatal("conflicting proposal changed the head")
	}
	props, err := s.api.ListProposals(s.owner(), c.ListProposalsRequest{DocumentID: d.Document.DocumentID})
	s.must(err, "proposals")
	found := false
	for _, p := range props.Items {
		if p.ProposalID == propID {
			found = true
			if p.Content != bobContent || p.State != c.ProposalConflicted || p.BaseRevisionID != rev1 {
				t.Fatalf("proposal not preserved: %+v", p)
			}
		}
	}
	if !found {
		t.Fatal("conflicting proposal not preserved")
	}
	old, err := s.api.GetDocument(s.owner(), c.GetDocumentRequest{DocumentID: d.Document.DocumentID, RevisionID: rev1})
	s.must(err, "get old revision")
	if old.Revision.Content != "# Brief\nGoal: X.\n" {
		t.Fatal("old revision changed")
	}
	disc, err := s.api.DiscardProposal(s.owner(), c.DiscardProposalRequest{Mutation: newOp(), ProposalID: propID})
	s.must(err, "discard")
	if disc.State != c.ProposalDiscarded || disc.Content != bobContent {
		t.Fatalf("discard must retain content: %+v", disc)
	}
}

func testArtifacts(t *testing.T, s *suite) {
	ctx := s.owner()
	data := []byte("version one bytes\n")
	_, err := s.publish(ctx, "", data, digest([]byte("something else")))
	s.wantCode(err, c.ErrConflict, "finalize with mismatched digest")
	list, err := s.api.ListArtifacts(ctx, c.ListArtifactsRequest{PageRequest: c.PageRequest{Limit: 500}})
	s.must(err, "list")
	for _, a := range list.Items {
		if a.Title == "conformance report" {
			t.Fatal("digest mismatch published an artifact")
		}
	}
	_, err = s.api.BeginUpload(ctx, c.BeginUploadRequest{Mutation: newOp(), Title: "t", Filename: "f", Size: 3, Digest: "md5:abc"})
	s.wantCode(err, c.ErrInvalid, "malformed digest")

	v1, err := s.publish(ctx, "", data, "")
	s.must(err, "publish v1")
	if v1.Version.Seq != 1 || v1.Version.Digest != digest(data) || v1.Version.Size != int64(len(data)) {
		t.Fatalf("v1 %+v", v1.Version)
	}
	data2 := []byte("version two bytes, longer\n")
	v2, err := s.publish(ctx, v1.Artifact.ArtifactID, data2, "")
	s.must(err, "publish v2")
	if v2.Version.Seq != 2 || v2.Version.PreviousVersionID != v1.Version.VersionID || v2.Artifact.LatestVersionID != v2.Version.VersionID {
		t.Fatalf("v2 %+v", v2.Version)
	}
	rc, ver, err := s.api.OpenContent(ctx, v1.Version.VersionID)
	s.must(err, "open v1")
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, data) || ver.Digest != v1.Version.Digest {
		t.Fatal("v1 bytes changed after v2 was published")
	}
	vs, err := s.api.ListVersions(ctx, c.ListVersionsRequest{ArtifactID: v1.Artifact.ArtifactID})
	s.must(err, "versions")
	if len(vs.Items) != 2 {
		t.Fatalf("versions %d", len(vs.Items))
	}
	// Upload beyond declared size is refused.
	up, err := s.api.BeginUpload(ctx, c.BeginUploadRequest{Mutation: newOp(), Title: "small", Filename: "s.txt", Size: 4, Digest: digest([]byte("abcd"))})
	s.must(err, "begin small")
	if _, err := s.api.UploadChunk(ctx, up.UploadID, 0, bytes.NewReader([]byte("abcdefgh"))); err == nil {
		t.Fatal("chunk beyond declared size accepted")
	}
	st, err := s.api.GetUpload(ctx, c.GetRequest{ID: up.UploadID})
	s.must(err, "get upload")
	if st.Received != 0 {
		t.Fatalf("oversized chunk partially stored: received=%d", st.Received)
	}
}

func testCommentVersion(t *testing.T, s *suite) {
	ctx := s.owner()
	v1, err := s.publish(ctx, "", []byte("v1"), "")
	s.must(err, "v1")
	cm, err := s.api.CreateComment(ctx, c.CreateCommentRequest{Mutation: newOp(), Target: c.ObjectRef{Kind: c.KindArtifactVersion, ID: v1.Version.VersionID}, Anchor: &c.Anchor{Kind: "line", Value: "1"}, Body: "include the failure count"})
	s.must(err, "comment")
	v2, err := s.publish(ctx, v1.Artifact.ArtifactID, []byte("v2"), "")
	s.must(err, "v2")
	on1, err := s.api.ListComments(ctx, c.ListCommentsRequest{Target: &c.ObjectRef{Kind: c.KindArtifactVersion, ID: v1.Version.VersionID}})
	s.must(err, "list v1")
	on2, err := s.api.ListComments(ctx, c.ListCommentsRequest{Target: &c.ObjectRef{Kind: c.KindArtifactVersion, ID: v2.Version.VersionID}})
	s.must(err, "list v2")
	if len(on1.Items) != 1 || on1.Items[0].CommentID != cm.CommentID || len(on2.Items) != 0 {
		t.Fatalf("comment moved: v1=%d v2=%d", len(on1.Items), len(on2.Items))
	}
	byArt, err := s.api.ListComments(ctx, c.ListCommentsRequest{ArtifactID: v1.Artifact.ArtifactID})
	s.must(err, "list by artifact")
	if len(byArt.Items) != 1 || byArt.Items[0].Target.ID != v1.Version.VersionID {
		t.Fatal("artifact-wide listing lost the comment's version")
	}
	got, err := s.api.GetComment(ctx, c.GetRequest{ID: cm.CommentID})
	s.must(err, "get comment")
	if got.Target.ID != v1.Version.VersionID {
		t.Fatal("comment target changed")
	}
	_, err = s.api.CreateComment(ctx, c.CreateCommentRequest{Mutation: newOp(), Target: c.ObjectRef{Kind: c.KindArtifactVersion, ID: v2.Version.VersionID}, InReplyTo: cm.CommentID, Body: "reply on wrong version"})
	s.wantCode(err, c.ErrInvalid, "reply targeting a different version")
}

func testEvents(t *testing.T, s *suite) {
	ctx := s.owner()
	first, err := s.api.ListEvents(ctx, c.ListEventsRequest{AfterCursor: 0, Limit: 1000})
	if c.IsCode(err, c.ErrResetRequired) {
		first = c.ListEventsResponse{}
		e := c.AsError(err)
		if snap, ok := toInt64(e.Details["snapshot_cursor"]); ok {
			first.NextCursor = snap
		}
	} else {
		s.must(err, "list events")
	}
	prev := int64(0)
	for _, e := range first.Events {
		if e.Cursor <= prev {
			t.Fatalf("cursors not strictly increasing: %d after %d", e.Cursor, prev)
		}
		prev = e.Cursor
	}
	cursor := first.NextCursor
	quiet, err := s.api.ListEvents(ctx, c.ListEventsRequest{AfterCursor: cursor})
	s.must(err, "list at head")
	cursor = quiet.NextCursor
	p, err := s.api.CreateProject(ctx, c.CreateProjectRequest{Mutation: newOp(), Name: "evented"})
	s.must(err, "mutation")
	after, err := s.api.ListEvents(ctx, c.ListEventsRequest{AfterCursor: cursor, WaitSeconds: 2})
	s.must(err, "events after mutation")
	found := false
	for _, e := range after.Events {
		if e.Cursor <= cursor {
			t.Fatalf("event %d not after cursor %d", e.Cursor, cursor)
		}
		if e.Kind == c.EvProjectChanged && e.Object.ID == p.ProjectID {
			found = true
		}
	}
	if !found {
		t.Fatal("project.changed event not observed after cursor")
	}
	again, err := s.api.ListEvents(ctx, c.ListEventsRequest{AfterCursor: cursor})
	s.must(err, "repeat read")
	if len(again.Events) != len(after.Events) {
		t.Fatal("re-reading from the same cursor is not repeatable")
	}
	tr := need[EventTrimmer](t, s.tg, "EventTrimmer")
	s.must(tr.TrimEvents(s.ctx, 1), "trim")
	_, err = s.api.ListEvents(ctx, c.ListEventsRequest{AfterCursor: 0})
	e := s.wantCode(err, c.ErrResetRequired, "cursor older than retention")
	snap, ok := toInt64(e.Details["snapshot_cursor"])
	if !ok {
		t.Fatalf("reset_required lacks snapshot_cursor: %+v", e.Details)
	}
	if _, err := s.api.ListEvents(ctx, c.ListEventsRequest{AfterCursor: snap}); err != nil {
		t.Fatalf("resume from snapshot_cursor: %v", err)
	}
}

func toInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case int:
		return int64(n), true
	case float64:
		return int64(n), true
	}
	return 0, false
}

func testAttention(t *testing.T, s *suite) {
	sv := s.start("asker")
	m, err := s.api.SendMessage(s.session(sv.Session.SessionID), c.SendMessageRequest{Mutation: newOp(), Recipient: c.Address{Kind: c.AddrOwner, ID: "owner"}, Body: c.MessageBody{Text: "which branch should I use?"}, RequestAck: true})
	s.must(err, "message to owner")
	att, err := s.api.ListAttention(s.owner(), c.ListAttentionRequest{})
	s.must(err, "attention")
	var item *c.AttentionItem
	for i := range att.Items {
		if att.Items[i].Object.ID == m.Envelope.MessageID {
			item = &att.Items[i]
		}
	}
	if item == nil {
		t.Fatal("owner message raised no attention item")
	}
	if item.DeepLink != c.DeepLink(item.Object) || item.State != c.AttnOpen {
		t.Fatalf("item %+v", item)
	}
	_, err = s.api.AckMessage(s.owner(), c.AckMessageRequest{Mutation: newOp(), MessageID: m.Envelope.MessageID})
	s.must(err, "owner ack")
	att, _ = s.api.ListAttention(s.owner(), c.ListAttentionRequest{})
	if hasObject(att.Items, m.Envelope.MessageID) {
		t.Fatal("acknowledged message still pending in attention")
	}
	all, _ := s.api.ListAttention(s.owner(), c.ListAttentionRequest{States: []c.AttentionState{c.AttnHandled, c.AttnWithdrawn}})
	if !hasObject(all.Items, m.Envelope.MessageID) {
		t.Fatal("handled item should remain as a durable record")
	}
}

func testPagination(t *testing.T, s *suite) {
	ctx := s.owner()
	for i := 0; i < 5; i++ {
		_, err := s.api.CreateNote(ctx, c.CreateNoteRequest{Mutation: newOp(), Kind: c.NoteNote, Title: fmt.Sprintf("paged-%d", i), Body: "paging"})
		s.must(err, "note")
	}
	seen := map[string]bool{}
	cursor := ""
	for pages := 0; pages < 100; pages++ {
		p, err := s.api.ListNotes(ctx, c.ListNotesRequest{Query: "paging", PageRequest: c.PageRequest{Cursor: cursor, Limit: 2}})
		s.must(err, "page")
		if len(p.Items) > 2 {
			t.Fatalf("limit ignored: %d items", len(p.Items))
		}
		for _, n := range p.Items {
			if seen[n.NoteID] {
				t.Fatalf("note %s repeated across pages", n.NoteID)
			}
			seen[n.NoteID] = true
		}
		if p.NextCursor == "" {
			break
		}
		cursor = p.NextCursor
	}
	if len(seen) != 5 {
		t.Fatalf("paged %d of 5 notes", len(seen))
	}
}
