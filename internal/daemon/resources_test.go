package daemon

import (
	"context"
	"os/exec"
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/supervisor"
)

// The node resources sent to the hub (hello and status reports) carry the
// agent processes' usage, separate from pyred's own overhead, the same
// numbers `pyred status` shows.
func TestResourcesReportAgentUsage(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	dir := shortDir(t)
	n := startNode(t, dir, Config{Adapters: []contracts.RuntimeAdapter{&tmuxShell{}}})
	n.start(startCmd("ses_res", "run_res", contracts.HarnessShell, t.TempDir()))
	rec := n.run("run_res")
	if rec.Handle == nil {
		t.Fatalf("no handle: %+v", rec)
	}
	tm := supervisor.Tmux{Socket: rec.Handle.TmuxSocket, OwnedRoot: dir}
	t.Cleanup(func() { tm.Kill(context.Background()) })

	r := n.d.Resources(context.Background())
	if r.AgentRSSMB <= 0 {
		t.Fatalf("agent_rss_mb = %v with a live shell run; want the tmux server + shell RSS", r.AgentRSSMB)
	}
	// Machine stats and the run's own process tree (pane shell) ride along.
	if r.MemTotalMB <= 0 || len(r.Disks) == 0 || r.Disks[0].TotalMB <= 0 {
		t.Fatalf("machine stats missing: %+v", r)
	}
	if len(r.Runs) != 1 || r.Runs[0].RunID != "run_res" || r.Runs[0].SessionID != "ses_res" || r.Runs[0].RSSMB <= 0 || r.Runs[0].Procs < 1 || r.Runs[0].Top == nil {
		t.Fatalf("run usage = %+v", r.Runs)
	}
	diag := n.d.Diagnostics(context.Background())
	if diag.Agents.RSSMB <= 0 || diag.Resources.AgentRSSMB <= 0 {
		t.Fatalf("diagnostics agents=%+v resources=%+v", diag.Agents, diag.Resources)
	}
}
