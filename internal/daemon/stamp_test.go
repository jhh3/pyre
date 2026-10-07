package daemon

import (
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/claude"
	"github.com/jhh3/pyre/internal/runtime/codex"
	"github.com/jhh3/pyre/internal/runtime/mock"
)

// Both native adapters wake a backed-off observer through their change
// stamp.
var (
	_ changeStamper = (*claude.Adapter)(nil)
	_ changeStamper = (*codex.Adapter)(nil)
)

// stampedMock is a mock adapter whose runs leave a cheap change stamp, the
// way the Claude adapter's hook-event and transcript files do.
type stampedMock struct {
	*mock.Adapter
	stamp atomic.Int64
	reads atomic.Int64
}

func (s *stampedMock) ChangeStamp(contracts.RunHandle) string {
	s.reads.Add(1)
	return strconv.FormatInt(s.stamp.Load(), 10)
}

// A run whose observation has backed off (nothing changed for a while) is
// observed again promptly when its change stamp moves, instead of after
// Poll.IdleMax. Seen live: after a native-terminal interrupt (which fires
// no hook) the Claude run stayed waiting_approval for ~30 s.
func TestChangeStampWakesBackedOffObservation(t *testing.T) {
	dir := shortDir(t)
	ma := &stampedMock{Adapter: mock.NewAdapter()}
	n := startNode(t, dir, Config{Adapters: []contracts.RuntimeAdapter{ma},
		Poll: PollConfig{Interval: 40 * time.Millisecond, IdleMax: 10 * time.Second}})
	n.start(startCmd("ses_st", "run_st", contracts.HarnessMock, t.TempDir()))
	ma.SetState("run_st", contracts.RunWaitingApproval)
	waitFor(t, "waiting_approval observed", func() bool { return n.run("run_st").Run.State == contracts.RunWaitingApproval })
	// Let the per-run interval back off well past a second.
	time.Sleep(3 * time.Second)
	lr, _ := n.d.getLive("run_st")
	lr.mu.Lock()
	backedOff := lr.interval
	lr.mu.Unlock()
	if backedOff < time.Second {
		t.Fatalf("interval did not back off: %s", backedOff)
	}
	ma.SetState("run_st", contracts.RunWaitingInput)
	ma.stamp.Add(1)
	start := time.Now()
	waitFor(t, "waiting_input observed", func() bool { return n.run("run_st").Run.State == contracts.RunWaitingInput })
	if d := time.Since(start); d > 800*time.Millisecond {
		t.Fatalf("state change seen after %s (backed-off interval %s); the change stamp should wake the observer", d, backedOff)
	}
	if ma.reads.Load() == 0 {
		t.Fatal("change stamp never read")
	}
}
