package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
)

func synthStatsNode() c.Node {
	return c.Node{NodeID: "node_synth", Name: "synth-linux", Platform: "linux", Arch: "amd64",
		Connection: c.NodeConnection{State: c.ConnConnected},
		Resources: c.NodeResources{CPUCount: 16, CPUPct: 35.2, Load1: 4.5, Load5: 3, Load15: 2, MemTotalMB: 32768, MemAvailMB: 8192,
			SwapTotalMB: 8192, SwapUsedMB: 6144, MemPressure: c.MemPressureWarn, MemPressureSome10: 12.5,
			Disks: []c.DiskUsage{{Path: "/home/synth", Roles: []string{"home", "data"}, TotalMB: 1000 << 10, FreeMB: 50 << 10}},
			Runs:  []c.RunUsage{{RunID: "run_synth", Procs: 12, RSSMB: 1229, CPUPct: 35, Top: &c.ProcUsage{PID: 7, Name: "claude", RSSMB: 900}}}}}
}

func TestPrintNodesMachineStats(t *testing.T) {
	var b bytes.Buffer
	printNodes(&b, []c.Node{synthStatsNode(), {NodeID: "node_old", Name: "old-pyred"}}, time.Now())
	out := b.String()
	for _, want := range []string{"CPU", "DISK FREE", "35% 4.5/16", "24G/32G warn!", "6.0G/8.0G!", "50G 5%!",
		"! synth-linux: disk home+data 50G free (5%); memory pressure warn; swap 6.0G of 8.0G used"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "! old-pyred") {
		t.Fatalf("a node without stats got a warning:\n%s", out)
	}
	b.Reset()
	printNode(&b, synthStatsNode())
	for _, want := range []string{"cpu:         35.2% busy, load 4.50 3.00 2.00", "pressure:    warn (psi some avg10 12.5%)",
		"disk:        /home/synth (home+data) 50G free of 1000G", "run usage:   run_synth 1.2G rss · 35% cpu · 12 procs · top claude 900M", "WARNING:     swap"} {
		if !strings.Contains(b.String(), want) {
			t.Fatalf("missing %q in:\n%s", want, b.String())
		}
	}
}

func TestPrintSessionUsage(t *testing.T) {
	var b bytes.Buffer
	u := synthStatsNode().Resources.Runs[0]
	u.ObservedAt = time.Now()
	printSession(&b, c.SessionView{Session: c.Session{SessionID: "ses_synth", Harness: c.HarnessClaude},
		Run: &c.Run{RunID: "run_synth", NodeID: "node_synth", State: c.RunRunning, Usage: &u}})
	if !strings.Contains(b.String(), "usage:     1.2G rss · 35% cpu · 12 procs · top claude 900M (sampled") {
		t.Fatalf("session show:\n%s", b.String())
	}
}
