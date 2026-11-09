package daemon

import (
	"context"
	"testing"
	"time"
)

// The pacer holds a pass to its byte rate and waits while the machine is
// busy.
func TestSearchPacerRateAndBusy(t *testing.T) {
	var slept []time.Duration
	busy := 2
	p := &searchPacer{start: time.Now(), rate: 8 << 20, busy: func() bool { busy--; return busy >= 0 },
		sleep: func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }}
	if err := p.Wait(context.Background(), 16<<20); err != nil {
		t.Fatal(err)
	}
	if len(slept) != 3 || slept[0] != searchBusyRecheck || slept[1] != searchBusyRecheck {
		t.Fatalf("slept %v, want two busy rechecks then the rate wait", slept)
	}
	if d := slept[2]; d < 1900*time.Millisecond || d > 2*time.Second {
		t.Fatalf("rate wait %v, want ~2s for 16 MiB at 8 MiB/s", d)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p = &searchPacer{start: time.Now(), rate: 1, busy: func() bool { return false }, sleep: sleepCtx}
	if err := p.Wait(ctx, 1<<20); err == nil {
		t.Fatal("wait with an ended context returned nil")
	}
}

func TestParsePressureSome(t *testing.T) {
	s := "some avg10=12.50 avg60=0.18 avg300=0.25 total=70768586244\nfull avg10=99.00 avg60=0.09 avg300=0.17 total=1\n"
	if got := parsePressureSome(s); got != 12.5 {
		t.Fatalf("got %v", got)
	}
	if got := parsePressureSome(""); got != 0 {
		t.Fatalf("empty: %v", got)
	}
}
