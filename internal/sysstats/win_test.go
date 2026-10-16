package sysstats

import (
	"context"
	"encoding/binary"
	"testing"
	"time"
)

func TestSampleWindows(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	raws := []WinRaw{
		{MemTotal: 16 << 30, MemAvail: 6 << 30, Idle: 1000, Kernel: 1500, User: 500, PageTotal: 4 << 30, PageInUse: 1 << 30, HavePagefile: true},
		// +1000 total ticks, of which +250 idle -> 75% busy
		{MemTotal: 16 << 30, MemAvail: 5 << 30, Idle: 1250, Kernel: 2100, User: 900, PageTotal: 4 << 30, PageInUse: 5 << 30, HavePagefile: true},
	}
	i := 0
	s := &Sampler{GOOS: "windows", MinInterval: 0, Now: func() time.Time { return now }, NumCPU: func() int { return 8 },
		Statfs: func(string) (FSInfo, error) { return FSInfo{}, nil },
		Win:    func() (WinRaw, error) { r := raws[i]; i++; return r, nil }}
	h := s.Sample(context.Background())
	if h.MemTotalMB != 16<<10 || h.MemAvailMB != 6<<10 || h.SwapTotalMB != 4<<10 || h.SwapUsedMB != 1<<10 {
		t.Fatalf("first sample: %+v", h)
	}
	if h.CPUPct != 0 || h.Load1 != 0 || h.Pressure != "" {
		t.Fatalf("first sample has no CPU delta, load or pressure: %+v", h)
	}
	now = now.Add(time.Second)
	h = s.Sample(context.Background())
	if h.CPUPct != 75 {
		t.Fatalf("cpu = %v, want 75", h.CPUPct)
	}
	if h.SwapUsedMB != h.SwapTotalMB {
		t.Fatalf("swap used is clamped to total: %+v", h)
	}
}

func TestParsePageFiles(t *testing.T) {
	entry := func(next, total, inUse uint32) []byte {
		b := make([]byte, 32)
		binary.LittleEndian.PutUint32(b[0:], next)
		binary.LittleEndian.PutUint32(b[4:], total)
		binary.LittleEndian.PutUint32(b[8:], inUse)
		return b
	}
	buf := append(entry(32, 1000, 100), entry(0, 500, 50)...)
	if tot, use := parsePageFiles(buf); tot != 1500 || use != 150 {
		t.Fatalf("got %d/%d", tot, use)
	}
	if tot, use := parsePageFiles(nil); tot != 0 || use != 0 {
		t.Fatalf("empty list: %d/%d", tot, use)
	}
	if tot, _ := parsePageFiles(append(entry(32, 7, 1), 0, 0)); tot != 7 {
		t.Fatalf("truncated trailing entry must be ignored: %d", tot)
	}
}

func TestLifetimeCPUAndParentLink(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	p := Proc{PID: 5, CPU: filetimeDuration(5_000_000), Start: start} // 0.5 s
	if got := LifetimeCPUPct(p, start.Add(2*time.Second)); got != 25 {
		t.Fatalf("lifetime cpu = %v", got)
	}
	if LifetimeCPUPct(Proc{CPU: time.Second}, start) != 0 {
		t.Fatal("no start time -> 0")
	}
	parent := Proc{PID: 1, Start: start}
	if !ParentLink(parent, Proc{PID: 2, PPID: 1, Start: start.Add(time.Second)}) {
		t.Fatal("younger child is linked")
	}
	if ParentLink(parent, Proc{PID: 2, PPID: 1, Start: start.Add(-time.Second)}) {
		t.Fatal("child older than its parent PID is a reused PID")
	}
	if !ParentLink(Proc{PID: 1}, Proc{PID: 2, PPID: 1}) {
		t.Fatal("unix rows (no start times) are always linked")
	}
}

func TestRunSamplerWindowsTreeSkipsReusedPID(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	table := map[int]Proc{
		10: {PID: 10, PPID: 1, RSSBytes: 100, Start: start},
		11: {PID: 11, PPID: 10, RSSBytes: 50, Start: start.Add(time.Second)},
		// 12's real parent exited; PID 10 was reused by the newer run root.
		12: {PID: 12, PPID: 10, RSSBytes: 1000, Start: start.Add(-time.Hour)},
	}
	s := &RunSampler{GOOS: "windows", MinInterval: time.Second, Now: func() time.Time { return start.Add(time.Minute) },
		NativeTable: func() (map[int]Proc, error) { return table, nil }}
	us := s.Sample(context.Background(), []RunRoot{{RunID: "r", PID: 10}})
	if len(us) != 1 || us[0].Procs != 2 || us[0].RSSBytes != 150 {
		t.Fatalf("usage = %+v", us)
	}
}
