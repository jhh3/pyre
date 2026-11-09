package daemon

import (
	"context"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Pacing the background transcript indexer (W-search). A backfill reads
// every transcript on the node (server: ~44 GB), so the background loop reads
// at most searchRateBytes a second, waits while the machine is busy, and
// runs on an OS thread at the lowest CPU and I/O priority
// (lowerThreadPriority). Query catch-ups are not paced: their budget is a
// fraction of a second.
var (
	searchRateBytes   float64 = 8 << 20 // per second
	searchBusyRecheck         = 5 * time.Second
	searchBusyDelay           = time.Minute // next pass when a pass starts busy
	// nodeBusy reports whether the machine is busy enough for background
	// indexing to wait. A variable for tests.
	nodeBusy = machineBusy
)

// searchPacer is one background pass's search.Pacer.
type searchPacer struct {
	start time.Time
	read  int64
	rate  float64
	busy  func() bool
	sleep func(context.Context, time.Duration) error
}

func newSearchPacer() *searchPacer {
	return &searchPacer{start: time.Now(), rate: searchRateBytes, busy: nodeBusy, sleep: sleepCtx}
}

// Wait blocks while the machine is busy, then until the pass's average
// read rate is back under the limit.
func (p *searchPacer) Wait(ctx context.Context, read int64) error {
	for p.busy() {
		if err := p.sleep(ctx, searchBusyRecheck); err != nil {
			return err
		}
	}
	p.read += read
	if p.rate <= 0 {
		return nil
	}
	due := time.Duration(float64(p.read) / p.rate * float64(time.Second))
	if ahead := due - time.Since(p.start); ahead > 0 {
		return p.sleep(ctx, ahead)
	}
	return nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Busy thresholds: one-minute load per CPU, and the share of the last 10s
// some task stalled on I/O or CPU (Linux PSI, /proc/pressure).
const (
	busyLoadPerCPU = 0.75
	busyIOSome     = 10.0 // percent
	busyCPUSome    = 25.0 // percent
)

// machineBusy reads Linux load and pressure; elsewhere (no /proc) the
// machine never counts as busy and the rate limit alone applies.
func machineBusy() bool {
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		if f := strings.Fields(string(b)); len(f) > 0 {
			if l, err := strconv.ParseFloat(f[0], 64); err == nil && l >= busyLoadPerCPU*float64(runtime.NumCPU()) {
				return true
			}
		}
	}
	return pressureSome("/proc/pressure/io") >= busyIOSome || pressureSome("/proc/pressure/cpu") >= busyCPUSome
}

// pressureSome returns a PSI file's "some avg10" (0 when unreadable).
func pressureSome(path string) float64 {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	return parsePressureSome(string(b))
}

func parsePressureSome(s string) float64 {
	for _, line := range strings.Split(s, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || f[0] != "some" {
			continue
		}
		if v, ok := strings.CutPrefix(f[1], "avg10="); ok {
			x, _ := strconv.ParseFloat(v, 64)
			return x
		}
	}
	return 0
}
