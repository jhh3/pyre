package hub

import (
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
)

// A live run's process-tree usage from its node's latest status report is
// presented on the session's run; a disconnected node's usage is not.
func TestSessionViewCarriesRunUsage(t *testing.T) {
	x := newHarness(t)
	x.connect(nodeMac, 0)
	v := x.start(owner, "op_usage", nodeMac)
	if v.Run == nil || v.Run.Usage != nil {
		t.Fatalf("before any status: %+v", v.Run)
	}
	runID := v.Run.RunID
	x.report(nodeMac, contracts.ReportStatus, contracts.NodeStatusReport{Resources: contracts.NodeResources{CPUCount: 8, CPUPct: 41.5,
		SwapTotalMB: 8192, SwapUsedMB: 6000, MemPressure: contracts.MemPressureWarn,
		Disks: []contracts.DiskUsage{{Path: "/home/synth", Roles: []string{"home", "data"}, TotalMB: 100000, FreeMB: 5000}},
		Runs: []contracts.RunUsage{{RunID: "run_other", RSSMB: 1}, {RunID: runID, SessionID: v.Session.SessionID, Procs: 9, RSSMB: 1229.5, CPUPct: 35,
			Top: &contracts.ProcUsage{PID: 4242, Name: "claude", RSSMB: 900}}}}})
	got := x.session(v.Session.SessionID)
	if got.Run.Usage == nil || got.Run.Usage.RSSMB != 1229.5 || got.Run.Usage.Top.Name != "claude" || got.Run.Usage.Procs != 9 {
		t.Fatalf("usage = %+v", got.Run.Usage)
	}
	page, err := x.h.ListSessions(as(owner), contracts.ListSessionsRequest{})
	if err != nil || len(page.Items) != 1 || page.Items[0].Run.Usage == nil || page.Items[0].Run.Usage.CPUPct != 35 {
		t.Fatalf("list: %+v %v", page.Items, err)
	}
	x.disconnect(nodeMac)
	if got := x.session(v.Session.SessionID); got.Run.Usage != nil {
		t.Fatalf("stale node usage presented: %+v", got.Run.Usage)
	}
}
