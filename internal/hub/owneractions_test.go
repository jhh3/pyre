package hub

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// ---- owner approvals for owner-only operations ----

// approvalAsk returns the ask an approval_requested error names.
func (x *harness) approvalAsk(err error) contracts.Ask {
	x.t.Helper()
	wantCode(x.t, err, contracts.ErrApprovalRequested)
	e := contracts.AsError(err)
	id, _ := e.Details["ask_id"].(string)
	if id == "" || e.Ref == nil || e.Ref.ID != id {
		x.t.Fatalf("approval error without ask: %+v", e)
	}
	a, gerr := x.h.GetAsk(as(owner), contracts.GetRequest{ID: id})
	if gerr != nil {
		x.t.Fatal(gerr)
	}
	return a
}

func (x *harness) answerAsk(op, askID, option string) contracts.AnswerAsksResponse {
	x.t.Helper()
	res, err := x.h.AnswerAsks(as(owner), contracts.AnswerAsksRequest{Mutation: contracts.Mutation{OperationID: op},
		Answers: []contracts.AnswerAskItem{{AskID: askID, OptionID: option}}})
	if err != nil || len(res.Results) != 1 || res.Results[0].Error != nil {
		x.t.Fatalf("answer %s: %+v %v", askID, res, err)
	}
	return res
}

func (x *harness) ownerActionState(askID string) (state, result string) {
	x.t.Helper()
	if err := x.db.QueryRow(`SELECT state, result FROM owner_actions WHERE ask_id=?`, askID).Scan(&state, &result); err != nil {
		x.t.Fatal(err)
	}
	return state, result
}

func (x *harness) endRun(sessionID string) {
	x.t.Helper()
	r := *x.session(sessionID).Run
	r.State, r.StateReason = contracts.RunExited, "synthetic exit"
	x.report(nodeMac, contracts.ReportRun, r)
}

// A session archiving a session it did not start files an approval
// instead of being refused; repeats (even from a child) merge; the owner's
// approval archives it as the owner, audited, and the requesters hear the
// outcome. Nothing happens before the approval.
func TestOwnerApprovalArchive(t *testing.T) {
	f := newChildFixture(t, nil)
	x := f.x
	pid, cid := f.parent.Session.SessionID, f.child.Session.SessionID
	other := x.start(owner, "op_synth-oa-other", nodeMac)
	oid := other.Session.SessionID
	x.endRun(oid)
	yes := true
	archive := func(p contracts.Principal, op string) error {
		_, err := x.h.UpdateSession(as(p), contracts.UpdateSessionRequest{Mutation: contracts.Mutation{OperationID: op}, SessionID: oid, Archived: &yes})
		return err
	}

	err := archive(sesPrincipal(pid), "op_synth-oa1")
	a := x.approvalAsk(err)
	if !strings.Contains(err.Error(), "needs the owner's approval: filed as "+a.AskID) || contracts.AsError(err).Details["status"] != "filed" {
		t.Fatalf("approval text: %v", err)
	}
	if x.session(oid).Session.Archived {
		t.Fatal("archived before approval")
	}
	if a.State != contracts.AskOpen || a.SessionID != pid || a.DefaultOption != "" || len(a.Options) != 2 || a.Options[0].ID != "approve" || a.Options[1].ID != "deny" ||
		!strings.Contains(a.Title, "Archive session") || !strings.Contains(a.Title, oid) || !strings.Contains(a.Context, pid) || a.DeadlineAt == nil ||
		a.DeadlineAt.Sub(x.clock.Now()) < 6*24*time.Hour {
		t.Fatalf("approval ask: %+v", a)
	}
	toOwner := x.envelopes(`recipient_kind='owner'`)
	if len(toOwner) != 1 || toOwner[0].MessageID != a.MessageID || toOwner[0].Sender.ID != ownerActionSender.ID {
		t.Fatalf("owner message: %+v", toOwner)
	}

	// A repeat from the child (which may not ask the owner itself) merges.
	err = archive(sesPrincipal(cid), "op_synth-oa2")
	if m := x.approvalAsk(err); m.AskID != a.AskID || m.Count != 2 || contracts.AsError(err).Details["status"] != "merged" {
		t.Fatalf("merge: %+v %v", m, err)
	}
	if n := len(x.envelopes(`recipient_kind='owner'`)); n != 1 || x.count("asks") != 1 {
		t.Fatalf("merge mailed the owner again: %d messages, %d asks", n, x.count("asks"))
	}

	// Approve: archived as the owner, audited, both requesters told.
	res := x.answerAsk("op_synth-oa-ans", a.AskID, "approve")
	if !x.session(oid).Session.Archived {
		t.Fatal("approved archive did not happen")
	}
	if st, result := x.ownerActionState(a.AskID); st != "done" || !strings.Contains(result, "is archived") {
		t.Fatalf("owner action: %s %q", st, result)
	}
	audit, err := x.h.ListAudit(as(owner), contracts.ListAuditRequest{SessionID: oid})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range audit.Items {
		if e.Action == contracts.AuditSessionArchived && e.Actor.Kind == "owner" && e.Actor.ID == owner.ID && strings.Contains(e.Reason, "approved request from") && strings.Contains(e.Reason, pid) {
			found = true
		}
	}
	if !found {
		t.Fatalf("no owner audit entry with the approval reason: %+v", audit.Items)
	}
	toParent := x.envelopes(`recipient_kind='session' AND recipient_id=? AND envelope LIKE '%pyre approval%'`, pid)
	if len(toParent) != 1 || toParent[0].InReplyTo != a.MessageID || !strings.Contains(toParent[0].Body.Text, "Done: session") ||
		res.Results[0].Ask.DecisionMessageID != toParent[0].MessageID {
		t.Fatalf("outcome to requester: %+v (decision %q)", toParent, res.Results[0].Ask.DecisionMessageID)
	}
	if n := len(x.envelopes(`recipient_kind='session' AND recipient_id=? AND envelope LIKE '%pyre approval%'`, cid)); n != 1 {
		t.Fatalf("merged requester told %d times", n)
	}
	if n := len(x.envelopes(`recipient_kind='session' AND recipient_id=? AND envelope LIKE '%pyre ask decided%'`, pid)); n != 0 {
		t.Fatalf("generic decision message sent too: %d", n)
	}
	// The maintenance repair finds nothing owed and does not run it again.
	x.clock.Add(2 * askRepairGrace)
	if err := x.h.resolveAsks(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(x.envelopes(`envelope LIKE '%pyre approval%'`)); n != 2 {
		t.Fatalf("outcome resent: %d", n)
	}
	// Archiving an archived session needs no approval.
	if err := archive(sesPrincipal(pid), "op_synth-oa3"); err != nil {
		t.Fatalf("archiving an archived session: %v", err)
	}
	// The owner's own archive is unaffected.
	other2 := x.start(owner, "op_synth-oa-other2", nodeMac)
	x.endRun(other2.Session.SessionID)
	if _, err := x.h.UpdateSession(as(owner), contracts.UpdateSessionRequest{Mutation: contracts.Mutation{OperationID: "op_synth-oa4"}, SessionID: other2.Session.SessionID, Archived: &yes}); err != nil {
		t.Fatal(err)
	}
}

// Deny: nothing changes, the requester hears it, and the same request is
// not filed again right away. A self-archive of an owner-started session
// takes the same route; approved while live, it archives when done.
func TestOwnerApprovalDenyAndSelfArchive(t *testing.T) {
	f := newChildFixture(t, nil)
	x := f.x
	pid := f.parent.Session.SessionID
	other := x.start(owner, "op_synth-od-other", nodeMac)
	oid := other.Session.SessionID
	yes := true
	upd := func(op, sid string) error {
		_, err := x.h.UpdateSession(as(sesPrincipal(pid)), contracts.UpdateSessionRequest{Mutation: contracts.Mutation{OperationID: op}, SessionID: sid, Archived: &yes, WhenDone: true})
		return err
	}
	a := x.approvalAsk(upd("op_synth-od1", oid))
	x.answerAsk("op_synth-od-ans", a.AskID, "deny")
	if x.session(oid).Session.Archived || len(x.session(oid).Session.Tags) != len(other.Session.Tags) {
		t.Fatal("denied archive took effect")
	}
	if st, _ := x.ownerActionState(a.AskID); st != "denied" {
		t.Fatalf("state %s", st)
	}
	dec := x.envelopes(`recipient_kind='session' AND recipient_id=? AND in_reply_to=?`, pid, a.MessageID)
	if len(dec) != 1 || !strings.Contains(dec[0].Body.Text, "owner chose deny") {
		t.Fatalf("deny message: %+v", dec)
	}
	err := upd("op_synth-od2", oid)
	wantCode(t, err, contracts.ErrForbidden)
	if !strings.Contains(err.Error(), "declined") {
		t.Fatalf("repeat after deny: %v", err)
	}
	x.clock.Add(ownerActionDeniedHold + time.Minute)
	if b := x.approvalAsk(upd("op_synth-od3", oid)); b.AskID == a.AskID {
		t.Fatal("refiled request reused the denied ask")
	}

	// The owner-started parent archiving itself (its run is live).
	s := x.approvalAsk(upd("op_synth-od4", pid))
	if !strings.Contains(s.Context, "archive itself") {
		t.Fatalf("self-archive context: %s", s.Context)
	}
	x.answerAsk("op_synth-od-ans2", s.AskID, "approve")
	v := x.session(pid).Session
	if v.Archived || !hasArchiveWhenDone(v) {
		t.Fatalf("approved self-archive of a live session: archived=%v tags=%v", v.Archived, v.Tags)
	}
	if out := x.envelopes(`recipient_id=? AND envelope LIKE '%archives when its current turn ends%'`, pid); len(out) != 1 {
		t.Fatalf("outcome: %+v", out)
	}
}

// fakeFleet records owner operations and replays them by operation ID like
// the topology store (an enrollment replay rotates the secret).
type fakeFleet struct {
	contracts.FleetAPI
	mu      sync.Mutex
	nodes   map[string]contracts.Node
	tickets map[string]contracts.EnrollmentTicket // by operation ID
	revokes []contracts.Principal
	reasons []string
	n       int
}

func (f *fakeFleet) GetNode(ctx context.Context, req contracts.GetRequest) (contracts.Node, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, ok := f.nodes[req.ID]
	if !ok {
		return n, contracts.Errorf(contracts.ErrNotFound, "node not found")
	}
	return n, nil
}

func (f *fakeFleet) RevokeNode(ctx context.Context, req contracts.RevokeRequest) (contracts.Node, error) {
	p, _ := contracts.PrincipalFrom(ctx)
	if p.Kind != contracts.PrincipalOwner {
		return contracts.Node{}, contracts.Errorf(contracts.ErrForbidden, "owner only")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	n := f.nodes[req.ID]
	n.Connection.State = contracts.ConnRevoked
	f.nodes[req.ID] = n
	f.revokes = append(f.revokes, p)
	f.reasons = append(f.reasons, ownerActionReason(ctx))
	return n, nil
}

func (f *fakeFleet) CreateEnrollment(ctx context.Context, req contracts.EnrollmentRequest) (contracts.EnrollmentTicket, error) {
	p, _ := contracts.PrincipalFrom(ctx)
	if p.Kind != contracts.PrincipalOwner {
		return contracts.EnrollmentTicket{}, contracts.Errorf(contracts.ErrForbidden, "owner only")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	t, ok := f.tickets[req.OperationID]
	if !ok {
		t = contracts.EnrollmentTicket{NodeID: "node_synth-enrolled", ExpiresAt: time.Date(2026, 10, 4, 13, 0, 0, 0, time.UTC), HubURL: "https://hub.synth.invalid"}
	}
	t.Secret = fmt.Sprintf("pye_synthetic-secret-%d", f.n) // rotated on replay
	f.tickets[req.OperationID] = t
	return t, nil
}

// Revoking a node and creating an enrollment ticket take the same route;
// the enrollment secret reaches the requester only by asking again.
func TestOwnerApprovalRevokeAndEnroll(t *testing.T) {
	x := newHarness(t)
	fleet := &fakeFleet{nodes: map[string]contracts.Node{"node_synth-old": {NodeID: "node_synth-old", Name: "synth-old"}, nodeMac: {NodeID: nodeMac, Name: "synth-mac"}}, tickets: map[string]contracts.EnrollmentTicket{}}
	x.fleet = fleet
	x.restart()
	x.connect(nodeMac, 0)
	sid := x.start(owner, "op_synth-or-ses", nodeMac).Session.SessionID
	sp := as(sesPrincipal(sid))

	_, err := x.h.RevokeNode(sp, contracts.RevokeRequest{Mutation: contracts.Mutation{OperationID: "op_synth-or1"}, ID: "node_synth-old", Reason: "synthetic VM was deleted"})
	a := x.approvalAsk(err)
	if !strings.Contains(a.Title, "Revoke machine synth-old") || !strings.Contains(a.Context, "synthetic VM was deleted") || len(fleet.revokes) != 0 {
		t.Fatalf("revoke ask: %+v (revokes %d)", a, len(fleet.revokes))
	}
	_, err = x.h.RevokeNode(sp, contracts.RevokeRequest{Mutation: contracts.Mutation{OperationID: "op_synth-or2"}, ID: "node_synth-old"})
	if m := x.approvalAsk(err); m.AskID != a.AskID {
		t.Fatalf("revoke repeat not merged: %s", m.AskID)
	}
	if _, err := x.h.RevokeNode(sp, contracts.RevokeRequest{Mutation: contracts.Mutation{OperationID: "op_synth-or3"}, ID: "node_synth-missing"}); !contracts.IsCode(err, contracts.ErrNotFound) {
		t.Fatalf("unknown node: %v", err)
	}
	x.answerAsk("op_synth-or-ans", a.AskID, "approve")
	if len(fleet.revokes) != 1 || fleet.revokes[0].ID != owner.ID || !strings.Contains(fleet.reasons[0], "approved request from") {
		t.Fatalf("revoke ran as %+v reason %v", fleet.revokes, fleet.reasons)
	}
	if out := x.envelopes(`recipient_id=? AND envelope LIKE '%machine synth-old (node_synth-old) is revoked%'`, sid); len(out) != 1 {
		t.Fatalf("revoke outcome: %+v", out)
	}

	// Enrollment.
	req := contracts.EnrollmentRequest{Mutation: contracts.Mutation{OperationID: "op_synth-oe1"}, Name: "synth-new", SSHAlias: "synth-new-ssh"}
	_, err = x.h.CreateEnrollment(sp, req)
	e := x.approvalAsk(err)
	if !strings.Contains(e.Title, `enrollment ticket for "synth-new"`) || fleet.n != 0 {
		t.Fatalf("enroll ask: %+v", e)
	}
	x.answerAsk("op_synth-oe-ans", e.AskID, "approve")
	if fleet.n != 1 {
		t.Fatalf("enrollment ran %d times", fleet.n)
	}
	out := x.envelopes(`recipient_id=? AND envelope LIKE '%enrollment ticket for%'`, sid)
	if len(out) != 1 || strings.Contains(out[0].Body.Text, "pye_synthetic-secret") || !strings.Contains(out[0].Body.Text, "node_synth-enrolled") {
		t.Fatalf("enroll outcome (secret must not be in it): %+v", out)
	}
	req.OperationID = "op_synth-oe2"
	tk, err := x.h.CreateEnrollment(sp, req)
	if err != nil || tk.NodeID != "node_synth-enrolled" || tk.Secret != "pye_synthetic-secret-2" {
		t.Fatalf("approved ticket for the requester: %+v %v", tk, err)
	}
	// Another session gets no secret; its request is filed instead.
	other := x.start(owner, "op_synth-oe-other", nodeMac).Session.SessionID
	req.OperationID = "op_synth-oe3"
	if _, err := x.h.CreateEnrollment(as(sesPrincipal(other)), req); !contracts.IsCode(err, contracts.ErrApprovalRequested) {
		t.Fatalf("other session: %v", err)
	}
	page, err := x.h.ListAudit(as(owner), contracts.ListAuditRequest{})
	if err != nil {
		t.Fatal(err)
	}
	audited := false
	for _, en := range page.Items {
		if en.Action == contracts.AuditNodeTicketCreated && en.Actor.Kind == "owner" && strings.Contains(en.Reason, "approved request from") {
			audited = true
		}
	}
	if !audited {
		t.Fatalf("enrollment not audited: %+v", page.Items)
	}
	// The owner calls the fleet directly.
	if _, err := x.h.CreateEnrollment(as(owner), contracts.EnrollmentRequest{Mutation: contracts.Mutation{OperationID: "op_synth-oe4"}, Name: "synth-owner"}); err != nil {
		t.Fatal(err)
	}
}
