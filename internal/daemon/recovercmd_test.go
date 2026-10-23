package daemon

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/mock"
)

// A start_run that was executing when pyred restarted, before its
// launching record committed (office-box1, Oct 2026: the run sat
// "unknown" for 30 minutes), is reported failed: nothing exists for it.
func TestRecoverCommandStartRunNeverCreatedFails(t *testing.T) {
	old := runProcessFound
	runProcessFound = func(string) (bool, bool) { return false, true }
	t.Cleanup(func() { runProcessFound = old })
	ad := &countingAdapter{RuntimeAdapter: mock.NewAdapter()}
	n := startNode(t, t.TempDir(), Config{Adapters: []contracts.RuntimeAdapter{ad}})
	cmd := command(contracts.CmdStartRun, startCmd("ses_gone", "run_gone", contracts.HarnessMock, t.TempDir()))
	res, ok := n.d.RecoverCommand(context.Background(), cmd)
	if !ok || res.Outcome != contracts.OutcomeFailed || res.Error == nil || res.Error.Message != InterruptedLaunchReason {
		t.Fatalf("recovered = %v %+v", ok, res)
	}
	if ad.launches.Load() != 0 {
		t.Fatal("recovery launched the run")
	}
}

// One whose run exists (adopted/recorded by recoverRuns) is not failed:
// the result is the run as observed.
func TestRecoverCommandStartRunThatExistsIsNotFailed(t *testing.T) {
	ad := &countingAdapter{RuntimeAdapter: mock.NewAdapter()}
	n := startNode(t, t.TempDir(), Config{Adapters: []contracts.RuntimeAdapter{ad}})
	sc := startCmd("ses_here", "run_here", contracts.HarnessMock, t.TempDir())
	n.start(sc)
	n = n.restart()
	res, ok := n.d.RecoverCommand(context.Background(), command(contracts.CmdStartRun, sc))
	if !ok || res.Outcome != contracts.OutcomeSucceeded {
		t.Fatalf("recovered = %v %+v", ok, res)
	}
	var run contracts.Run
	if err := json.Unmarshal(res.Result, &run); err != nil || run.RunID != "run_here" || run.State.Terminal() {
		t.Fatalf("run = %+v (%v)", run, err)
	}
	if ad.launches.Load() != 1 {
		t.Fatalf("launches = %d", ad.launches.Load())
	}
	// Other kinds stay indeterminate.
	if _, ok := n.d.RecoverCommand(context.Background(), command(contracts.CmdDeliver, struct{}{})); ok {
		t.Fatal("deliver recovered")
	}
}
