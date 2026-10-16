package hub

import (
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
)

// W-archive2: any session may set any session's icon; it persists, is
// audited, and automatic titling never removes it.
func TestSessionIcon(t *testing.T) {
	x := newHarness(t)
	x.connect(nodeMac, 0)
	v := x.start(owner, "op_synth-icon", nodeMac)
	sid, rid := v.Session.SessionID, v.Run.RunID
	other := x.start(owner, "op_synth-icon-peer", nodeMac)
	set := func(op, icon string) (contracts.SessionView, error) {
		return x.h.UpdateSession(as(sesPrincipal(other.Session.SessionID)), contracts.UpdateSessionRequest{Mutation: contracts.Mutation{OperationID: op}, SessionID: sid, Icon: &icon})
	}
	if _, err := set("op_synth-icon-bad", "ab"); !contracts.IsCode(err, contracts.ErrInvalid) {
		t.Fatalf("not an emoji: %v", err)
	}
	got, err := set("op_synth-icon-set", " 🚀 ")
	if err != nil || got.Session.Icon != "🚀" {
		t.Fatalf("set: %v %q", err, got.Session.Icon)
	}
	x.report(nodeMac, contracts.ReportEvent, titleEvent(sid, rid, "Synthetic release prep"))
	if s := x.session(sid).Session; s.Title != "Synthetic release prep" || s.Icon != "🚀" {
		t.Fatalf("auto-title kept the icon: %q %q", s.Title, s.Icon)
	}
	x.restart()
	if s := x.session(sid).Session; s.Icon != "🚀" {
		t.Fatalf("persisted: %q", s.Icon)
	}
	e := findAudit(t, x.audit(contracts.ListAuditRequest{}), contracts.AuditSessionIconSet)
	if e.To != "🚀" {
		t.Fatalf("audit: %+v", e)
	}
	if got, err := set("op_synth-icon-clear", ""); err != nil || got.Session.Icon != "" {
		t.Fatalf("clear: %v %q", err, got.Session.Icon)
	}
}
