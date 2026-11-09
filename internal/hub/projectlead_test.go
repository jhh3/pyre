package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
)

// projectLeadOf (iss_…): the walk up a session's
// ancestors stops at the first session whose parent is outside the
// project, and is bounded and safe on a lineage loop.
func TestProjectLeadOfBoundedAndCycleSafe(t *testing.T) {
	x := newHarness(t)
	x.connect(nodeMac, 0)
	ctx := context.Background()
	const p, q = "prj_synthp", "prj_synthq"
	set := func(id, project, parent string) {
		t.Helper()
		lin := `{}`
		if parent != "" {
			lin = fmt.Sprintf(`{"parent_session_id":%q}`, parent)
		}
		if _, err := x.db.Exec(`UPDATE sessions SET project_id=?, lineage=? WHERE session_id=?`, project, lin, id); err != nil {
			t.Fatal(err)
		}
	}
	ids := make([]string, maxLeadDepth+3)
	tx, err := x.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := range ids {
		ids[i] = fmt.Sprintf("ses_synthlead%02d", i)
		if err := x.h.insertSessionTx(ctx, tx, contracts.Session{SessionID: ids[i], Harness: contracts.HarnessClaude, Title: ids[i],
			CreatedBy: owner, CreatedAt: x.clock.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	leadOf := func(id string) string {
		t.Helper()
		s, err := x.h.getSession(ctx, x.db, id)
		if err != nil {
			t.Fatal(err)
		}
		got, err := x.h.projectLeadOf(ctx, x.db, s, p)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	// coordinator (q) → lead (p) → child (p) → grandchild (p).
	coord, lead, child, grand := ids[0], ids[1], ids[2], ids[3]
	set(coord, q, "")
	set(lead, p, coord)
	set(child, p, lead)
	set(grand, p, child)
	for _, id := range []string{lead, child, grand} {
		if got := leadOf(id); got != lead {
			t.Errorf("%s: lead %q, want the delegated lead %s", id, got, lead)
		}
	}
	if got := leadOf(coord); got != "" {
		t.Errorf("a session outside the project leads nothing in it: %q", got)
	}
	if !x.h.leadsOwnProject(ctx, x.db, mustSession(t, x, lead)) || x.h.leadsOwnProject(ctx, x.db, mustSession(t, x, child)) {
		t.Error("leadsOwnProject: the delegated lead leads, its child does not")
	}
	// A missing parent counts as outside the project.
	set(lead, p, "ses_synthgone")
	if got := leadOf(child); got != lead {
		t.Errorf("missing parent: lead %q", got)
	}
	// A loop inside the project has no lead.
	set(lead, p, grand)
	if got := leadOf(child); got != "" {
		t.Errorf("loop: lead %q", got)
	}
	// A chain inside the project longer than maxLeadDepth has none either;
	// one within the bound does.
	for i := 1; i < len(ids); i++ {
		set(ids[i], p, ids[i-1])
	}
	set(ids[0], p, "")
	if got := leadOf(ids[maxLeadDepth]); got != ids[0] {
		t.Errorf("depth %d: lead %q", maxLeadDepth, got)
	}
	if got := leadOf(ids[len(ids)-1]); got != "" {
		t.Errorf("past the bound: lead %q", got)
	}
}

func mustSession(t *testing.T, x *harness, id string) contracts.Session {
	t.Helper()
	s, err := x.h.getSession(context.Background(), x.db, id)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// The start command tells the node whether the session leads its project,
// so the run's guide and page reminders treat a delegated lead as a lead.
func TestStartRunCommandProjectLead(t *testing.T) {
	x := newHarness(t)
	x.connect(nodeMac, 0)
	q, p := "prj_synthq", "prj_synthp"
	start := func(by contracts.Principal, op string, project *string) contracts.StartRunCommand {
		t.Helper()
		if _, err := x.h.StartSession(as(by), contracts.StartSessionRequest{Mutation: contracts.Mutation{OperationID: op}, NodeID: nodeMac,
			Harness: contracts.HarnessClaude, Cwd: "/home/synth/work", ProjectID: project}); err != nil {
			t.Fatalf("start %s: %v", op, err)
		}
		cmds := x.ch.sentKinds(nodeMac, contracts.CmdStartRun)
		var sc contracts.StartRunCommand
		if err := json.Unmarshal(cmds[len(cmds)-1].Payload, &sc); err != nil {
			t.Fatal(err)
		}
		return sc
	}
	coord := start(owner, "op_synth-coord", &q)
	lead := start(sesPrincipal(coord.Session.SessionID), "op_synth-lead", &p)
	child := start(sesPrincipal(lead.Session.SessionID), "op_synth-child", &p)
	free := start(owner, "op_synth-free", nil)
	if !coord.ProjectLead || !lead.ProjectLead || child.ProjectLead || free.ProjectLead {
		t.Fatalf("project_lead: coordinator %v, delegated lead %v, child %v, no project %v", coord.ProjectLead, lead.ProjectLead, child.ProjectLead, free.ProjectLead)
	}
}
