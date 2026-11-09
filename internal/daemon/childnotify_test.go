package daemon

import (
	"context"
	"strings"
	"testing"

	"github.com/jhh3/pyre/internal/agentguide"
	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/daemon/daemontest"
)

// W-childnotify: a session started by another session learns who started it
// ($PYRE_PARENT_SESSION_ID) and whether its results are relayed
// ($PYRE_REPORT_TO_PARENT=1); owner-started sessions get neither.
func TestRunEnvNamesTheParentSession(t *testing.T) {
	d := &Daemon{cfg: Config{StateDir: t.TempDir(), PyreBinDir: "/opt/synth/bin"}}
	child := contracts.StartRunCommand{RunID: "run_synth-child", Session: contracts.Session{SessionID: "ses_synth-child",
		Lineage: contracts.Lineage{ParentSessionID: "ses_synth-parent"}, Tags: []string{contracts.TagReportParent}}}
	env := d.runEnv(nil, child, t.TempDir())
	if env["PYRE_PARENT_SESSION_ID"] != "ses_synth-parent" || env["PYRE_REPORT_TO_PARENT"] != "1" {
		t.Fatalf("child env: parent=%q report=%q", env["PYRE_PARENT_SESSION_ID"], env["PYRE_REPORT_TO_PARENT"])
	}
	child.Session.Tags = []string{contracts.TagReportNone}
	if env := d.runEnv(nil, child, t.TempDir()); env["PYRE_PARENT_SESSION_ID"] != "ses_synth-parent" || env["PYRE_REPORT_TO_PARENT"] != "" {
		t.Fatalf("opted-out child env: %q", env["PYRE_REPORT_TO_PARENT"])
	}
	owner := contracts.StartRunCommand{RunID: "run_synth-owner", Session: contracts.Session{SessionID: "ses_synth-owner"}}
	if env := d.runEnv(nil, owner, t.TempDir()); env["PYRE_PARENT_SESSION_ID"] != "" || env["PYRE_REPORT_TO_PARENT"] != "" {
		t.Fatalf("owner-started env names a parent: %v", env)
	}
}

// A delegated lead (its parent is outside its project; the hub sets
// StartRunCommand.ProjectLead) is told it leads: $PYRE_PROJECT_LEAD for the
// page reminders and a line in its guide. A continuation the node starts
// on its own (account switch, crash resume) keeps it.
func TestDelegatedLeadEnvAndGuide(t *testing.T) {
	w := daemontest.NewWorld()
	fa := daemontest.New(w, contracts.HarnessClaude)
	n := startNode(t, t.TempDir(), Config{Adapters: []contracts.RuntimeAdapter{fa}, Accounts: &fakeBinder{},
		AgentGuide: func(contracts.Harness, contracts.Session) string { return "# synthetic guide\n" }})
	cwd := t.TempDir()
	project := "prj_synthticket"
	sc := startCmd("ses_synthlead", "run_l1", contracts.HarnessClaude, cwd)
	sc.Session.ProjectID = &project
	sc.Session.Lineage.ParentSessionID = "ses_synthcoordinator"
	sc.ProjectLead = true
	n.start(sc)
	p1, _ := w.Proc("run_l1")
	if p1.Launch.Env[agentguide.EnvProjectLead] != "1" || !strings.Contains(p1.Launch.AgentGuide, "you lead it") {
		t.Fatalf("delegated lead: env %q guide %q", p1.Launch.Env[agentguide.EnvProjectLead], p1.Launch.AgentGuide)
	}
	n.do(command(contracts.CmdStop, contracts.RunCommand{SessionID: "ses_synthlead", RunID: "run_l1"}))
	newRun, _, err := n.d.StartContinuation(context.Background(), ContinuationRequest{SessionID: "ses_synthlead", PreviousRunID: "run_l1",
		AccountID: "acct_1", Kind: contracts.ContinuationNativeResume, Prompt: "continue"})
	if err != nil {
		t.Fatal(err)
	}
	n.d.WaitLaunches()
	if p2, _ := w.Proc(newRun); p2.Launch.Env[agentguide.EnvProjectLead] != "1" {
		t.Fatalf("continuation lost the lead flag: %v", p2.Launch.Env)
	}

	// A child in its parent's project is not a lead.
	child := startCmd("ses_synthchild", "run_c1", contracts.HarnessClaude, cwd)
	child.Session.ProjectID = &project
	child.Session.Lineage.ParentSessionID = "ses_synthlead"
	n.start(child)
	if p, _ := w.Proc("run_c1"); p.Launch.Env[agentguide.EnvProjectLead] != "" || strings.Contains(p.Launch.AgentGuide, "you lead it") {
		t.Fatalf("child: env %q guide %q", p.Launch.Env[agentguide.EnvProjectLead], p.Launch.AgentGuide)
	}
}
