package daemon

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/daemon/daemontest"
)

// sparingAdapter counts StopSparing calls (decision I62).
type sparingAdapter struct {
	contracts.RuntimeAdapter
	spared atomic.Int32
}

func (s *sparingAdapter) StopSparing(ctx context.Context, h contracts.RunHandle) (contracts.CommandReceipt, error) {
	s.spared.Add(1)
	return s.RuntimeAdapter.Stop(ctx, h)
}

// Only Pyre's own stop (automatic, from a system principal) is a sparing
// stop; a stop the owner or an agent asked for kills as before, even if the
// command claims to be automatic.
func TestAutomaticStopSpares(t *testing.T) {
	w := daemontest.NewWorld()
	fa := &sparingAdapter{RuntimeAdapter: daemontest.New(w, contracts.HarnessCodex)}
	n := startNode(t, t.TempDir(), Config{Adapters: []contracts.RuntimeAdapter{fa}, Accounts: &fakeBinder{}})
	cwd := t.TempDir()

	n.start(startCmd("ses_sp1", "run_sp1", contracts.HarnessCodex, cwd))
	owner := command(contracts.CmdStop, contracts.RunCommand{SessionID: "ses_sp1", RunID: "run_sp1", Automatic: true})
	if res := n.do(owner); res.Error != nil {
		t.Fatalf("stop: %+v", res.Error)
	}
	if fa.spared.Load() != 0 {
		t.Fatal("an owner's stop spared live work")
	}

	n.start(startCmd("ses_sp2", "run_sp2", contracts.HarnessCodex, cwd))
	sys := command(contracts.CmdStop, contracts.RunCommand{SessionID: "ses_sp2", RunID: "run_sp2", Automatic: true, Reason: "archive when done: the turn ended"})
	sys.Actor = contracts.Principal{Kind: contracts.PrincipalSystem, ID: "hub"}
	if res := n.do(sys); res.Error != nil {
		t.Fatalf("stop: %+v", res.Error)
	}
	if fa.spared.Load() != 1 {
		t.Fatalf("archive-when-done stop not sparing: %d", fa.spared.Load())
	}

	n.start(startCmd("ses_sp3", "run_sp3", contracts.HarnessCodex, cwd))
	plain := command(contracts.CmdStop, contracts.RunCommand{SessionID: "ses_sp3", RunID: "run_sp3"})
	plain.Actor = contracts.Principal{Kind: contracts.PrincipalSystem, ID: "hub"}
	if res := n.do(plain); res.Error != nil {
		t.Fatalf("stop: %+v", res.Error)
	}
	if fa.spared.Load() != 1 {
		t.Fatal("a stop not marked automatic spared live work")
	}
}
