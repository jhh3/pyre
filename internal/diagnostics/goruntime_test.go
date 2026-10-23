package diagnostics

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestSampleRuntimeSeesGoroutinesAndFDs(t *testing.T) {
	base := SampleRuntime()
	if base.Goroutines < 1 || base.HeapAllocMB <= 0 {
		t.Fatalf("implausible runtime stats: %+v", base)
	}
	if base.OpenFDs < 0 {
		t.Skip("no fd listing on this platform")
	}
	stop := make(chan struct{})
	started := make(chan struct{})
	for i := 0; i < 20; i++ {
		go func() { started <- struct{}{}; <-stop }()
		<-started
	}
	var files []*os.File
	for i := 0; i < 5; i++ {
		f, err := os.Open(os.Args[0])
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	got := SampleRuntime()
	close(stop)
	for _, f := range files {
		f.Close()
	}
	// At least the 20 parked goroutines plus this one. Not base+20: a
	// goroutine counted in base (the test runner's own) may have exited
	// since (seen in CI: 22, want >= 23).
	if got.Goroutines < 21 {
		t.Errorf("goroutines %d, want >= 21 (20 parked + the test's)", got.Goroutines)
	}
	if got.OpenFDs < base.OpenFDs+5 {
		t.Errorf("open fds %d, want >= %d", got.OpenFDs, base.OpenFDs+5)
	}
}

func TestWriteRuntimeStatsReplacesFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "rt.json")
	for i := 0; i < 2; i++ {
		if err := WriteRuntimeStats(p); err != nil {
			t.Fatal(err)
		}
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var v RuntimeStatsFile
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	if v.PID != os.Getpid() || v.Goroutines < 1 || v.At.IsZero() {
		t.Fatalf("bad snapshot %+v", v)
	}
	ents, _ := os.ReadDir(filepath.Dir(p))
	if len(ents) != 1 {
		t.Fatalf("temp file left behind: %v", ents)
	}
}
