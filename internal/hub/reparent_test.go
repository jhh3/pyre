package hub

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
)

// ---- reparenting (W-reparent) ----

// msgsBetween lists message texts from sender to recipient session, oldest first.
func (x *harness) msgsBetween(sender, recipient string) []string {
	x.t.Helper()
	rows, err := x.db.Query(`SELECT envelope FROM messages WHERE recipient_kind='session' AND recipient_id=? AND sender_id=? ORDER BY inbox_cursor`, recipient, sender)
	if err != nil {
		x.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var body string
		rows.Scan(&body)
		var e contracts.MessageEnvelope
		json.Unmarshal([]byte(body), &e)
		out = append(out, e.Body.Text)
	}
	return out
}

func (x *harness) reparent(p contracts.Principal, op, sid, parent string) (contracts.SessionView, error) {
	return x.h.UpdateSession(as(p), contracts.UpdateSessionRequest{Mutation: contracts.Mutation{OperationID: op}, SessionID: sid, Parent: &parent})
}

func countWith(texts []string, sub string) int {
	n := 0
	for _, t := range texts {
		if strings.Contains(t, sub) {
			n++
		}
	}
	return n
}

func TestReparentRetargetsReportsAndNotifies(t *testing.T) {
	f := newChildFixture(t, nil)
	x := f.x
	coord, child := f.parent.Session.SessionID, f.child.Session.SessionID
	lead := x.start(owner, "op_synth-lead", nodeMac).Session.SessionID
	child2 := f.startChild("op_synth-child2", nil).Session.SessionID

	// A turn ends before the reparent: its pending report retargets.
	f.observe(contracts.RunRunning, "working")
	f.observe(contracts.RunWaitingInput, "idle")
	v, err := x.reparent(sesPrincipal(coord), "op_synth-rp1", child, lead)
	if err != nil {
		t.Fatal(err)
	}
	s := v.Session
	if s.Lineage.ParentSessionID != lead || s.Lineage.ReparentedFrom != coord || s.Lineage.ReparentedAt == nil || s.Parent() != lead {
		t.Fatalf("lineage after reparent: %+v", s.Lineage)
	}
	if s.CreatedBy.ID != coord {
		t.Fatalf("created_by must stay the historical creator: %+v", s.CreatedBy)
	}
	f.process()
	if n := len(x.msgsBetween(child, coord)); n != 0 {
		t.Fatalf("coordinator still got %d messages from the child: %v", n, f.statuses())
	}
	toLead := x.msgsBetween(child, lead)
	if countWith(toLead, "[pyre auto-report]") != 1 || countWith(toLead, "which reports to you") != 1 {
		t.Fatalf("pending report not retargeted to the lead: %q %v", toLead, f.statuses())
	}
	if countWith(toLead, "now reports to you") != 1 || countWith(toLead, coord) == 0 {
		t.Fatalf("lead notice: %q", toLead)
	}
	toChild := x.msgsBetween(lead, child)
	if len(toChild) != 1 || !strings.Contains(toChild[0], "You now report to") || !strings.Contains(toChild[0], lead) {
		t.Fatalf("child notice: %q", toChild)
	}
	// The next turn reports to the lead too; a replay sends nothing new.
	f.observe(contracts.RunRunning, "working")
	f.observe(contracts.RunWaitingInput, "idle")
	f.process()
	if _, err := x.reparent(sesPrincipal(coord), "op_synth-rp1", child, lead); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if got := x.msgsBetween(child, lead); countWith(got, "[pyre auto-report]") != 2 || countWith(got, "now reports to you") != 1 {
		t.Fatalf("after the next turn / replay: %q", got)
	}
	// Audit: who, from, to.
	var found bool
	for _, e := range x.audit(contracts.ListAuditRequest{SessionID: child}) {
		if e.Action == contracts.AuditSessionReparented {
			found = true
			if e.From != coord || e.To != lead || e.Actor.Kind != "session" || e.Actor.ID != coord {
				t.Fatalf("audit entry: %+v", e)
			}
		}
	}
	if !found {
		t.Fatal("no session.reparented audit entry")
	}

	// Validation: no cycles, not itself, the parent exists and is live.
	wantCode(t, errOf(x.reparent(owner, "op_synth-rp2", lead, child)), contracts.ErrInvalid) // lead under its own child
	wantCode(t, errOf(x.reparent(owner, "op_synth-rp3", child, child)), contracts.ErrInvalid)
	wantCode(t, errOf(x.reparent(owner, "op_synth-rp4", child, "ses_synthmissing")), contracts.ErrInvalid)
	arch := true
	if _, err := x.h.UpdateSession(as(owner), contracts.UpdateSessionRequest{Mutation: contracts.Mutation{OperationID: "op_synth-arch"}, SessionID: child2, Archived: &arch}); err != nil {
		t.Fatal(err)
	}
	wantCode(t, errOf(x.reparent(owner, "op_synth-rp5", child, child2)), contracts.ErrInvalid)

	// Detach to the top level: pending reports are dropped, the child is told.
	f.observe(contracts.RunRunning, "working")
	f.observe(contracts.RunWaitingInput, "idle")
	v, err = x.reparent(owner, "op_synth-rp6", child, "none")
	if err != nil {
		t.Fatal(err)
	}
	if v.Session.Parent() != "" || v.Session.Lineage.ReparentedFrom != lead {
		t.Fatalf("detached lineage: %+v", v.Session.Lineage)
	}
	f.process()
	if got := x.msgsBetween(child, lead); countWith(got, "[pyre auto-report]") != 2 {
		t.Fatalf("report after detach: %q", got)
	}
	if st := strings.Join(f.statuses(), " "); !strings.Contains(st, "moved to the top level") {
		t.Fatalf("pending report not dropped: %s", st)
	}
	var fromOwner int
	x.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE recipient_id=? AND sender_kind='owner'`, child).Scan(&fromOwner)
	if fromOwner != 1 {
		t.Fatalf("detach notice to the child: %d", fromOwner)
	}
}

func errOf(_ contracts.SessionView, err error) error { return err }

func TestReparentPermissions(t *testing.T) {
	f := newChildFixture(t, nil)
	x := f.x
	coord, child := f.parent.Session.SessionID, f.child.Session.SessionID
	lead := x.start(owner, "op_synth-lead", nodeMac).Session.SessionID
	other := x.start(owner, "op_synth-other", nodeMac).Session.SessionID

	// A session that neither is, nor parents, nor shares a project with it: refused.
	err := errOf(x.reparent(sesPrincipal(lead), "op_synth-p1", child, lead))
	wantCode(t, err, contracts.ErrForbidden)
	if !strings.Contains(err.Error(), "ask the owner") {
		t.Fatalf("refusal should explain: %v", err)
	}
	wantCode(t, errOf(x.reparent(sesPrincipal(child), "op_synth-p2", coord, child)), contracts.ErrForbidden) // child moving its parent
	// Itself: allowed (and the actor is not notified of its own move).
	if _, err := x.reparent(sesPrincipal(child), "op_synth-p3", child, lead); err != nil {
		t.Fatalf("self reparent: %v", err)
	}
	if n := len(x.msgsBetween(lead, child)); n != 0 {
		t.Fatalf("the child moved itself; it needs no notice (%d)", n)
	}
	// The former parent no longer parents it.
	wantCode(t, errOf(x.reparent(sesPrincipal(coord), "op_synth-p4", child, coord)), contracts.ErrForbidden)
	// The current parent may hand it on.
	if _, err := x.reparent(sesPrincipal(lead), "op_synth-p5", child, other); err != nil {
		t.Fatalf("current parent: %v", err)
	}
	// Same project: a lead may adopt agents in its project.
	if _, err := x.db.Exec(`UPDATE sessions SET project_id='prj_synth-shared' WHERE session_id IN (?,?)`, lead, child); err != nil {
		t.Fatal(err)
	}
	if _, err := x.reparent(sesPrincipal(lead), "op_synth-p6", child, lead); err != nil {
		t.Fatalf("same project: %v", err)
	}
	// The owner may reparent anything.
	if _, err := x.reparent(owner, "op_synth-p7", coord, other); err != nil {
		t.Fatalf("owner: %v", err)
	}
	if got := x.session(child).Session.Parent(); got != lead {
		t.Fatalf("final parent %s", got)
	}
}
