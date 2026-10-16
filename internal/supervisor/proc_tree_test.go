package supervisor

import (
	"testing"
	"time"
)

func TestTreeUsageSkipsReusedParentPID(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	table := map[int]Proc{
		10: {PID: 10, PPID: 1, RSSKB: 100, CPUPct: 1, Start: start},
		11: {PID: 11, PPID: 10, RSSKB: 50, CPUPct: 2, Start: start.Add(time.Second)},
		12: {PID: 12, PPID: 10, RSSKB: 1000, CPUPct: 50, Start: start.Add(-time.Hour)}, // stale PPID
	}
	rss, cpu, n := TreeUsage(table, []int{10})
	if rss != 150 || cpu != 3 || n != 2 {
		t.Fatalf("rss=%d cpu=%v n=%d", rss, cpu, n)
	}
	// ps rows carry no start time: every link counts, as before.
	for k, p := range table {
		p.Start = time.Time{}
		table[k] = p
	}
	if _, _, n := TreeUsage(table, []int{10}); n != 3 {
		t.Fatalf("n=%d", n)
	}
}
