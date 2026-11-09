package accounts

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
)

func (e *testEnv) setCwd(sessionID, cwd string) {
	e.ctrl.mu.Lock()
	defer e.ctrl.mu.Unlock()
	r := e.ctrl.runs[sessionID]
	r.Cwd = cwd
	e.ctrl.runs[sessionID] = r
}

// A session that removed its launch worktree (after merging) and works
// elsewhere is not stopped for a proactive move: the relaunch would fail
// on the missing directory and the healthy session would stay down
// (iss_…). Once its directory exists the move
// goes ahead.
func TestRebalanceSkipsRunWhoseCwdIsGone(t *testing.T) {
	e := rebalanceEnv(t)
	ctx := context.Background()
	e.startRun("ses_1", "run_1", contracts.HarnessClaude, softPin, claudeCaps)
	e.setState("ses_1", contracts.RunWaitingInput)
	e.setCwd("ses_1", filepath.Join(t.TempDir(), "removed-worktree"))

	if plans := e.sweep(); len(plans) != 0 || len(e.ctrl.order) != 0 {
		t.Fatalf("stopped a run whose working directory is gone: %+v %v", plans, e.ctrl.order)
	}
	if p, _ := getFallback(ctx, e.svc.db, "run_1"); p != nil {
		t.Fatalf("claim left behind: %+v", p)
	}
	if e.svc.moveFailedRecently(ctx, "ses_1") {
		t.Fatal("a skipped move counts as a failed one")
	}

	e.setCwd("ses_1", t.TempDir())
	e.clock.Advance(rebalanceEvery)
	plans := e.svc.Rebalance(ctx)
	if len(plans) != 1 || plans[0].Outcome != contracts.FallbackNativeResume {
		t.Fatalf("plans once the directory exists: %+v", plans)
	}
}

func TestRelaunchBlocker(t *testing.T) {
	dir := t.TempDir()
	gone := filepath.Join(dir, "gone")
	for _, c := range []struct {
		cwd, profile string
		blocked      bool
	}{
		{dir, "", false},
		{"", "", false},
		{dir, dir, false},
		{gone, "", true},
		{dir, gone, true},
	} {
		if got := relaunchBlocker(c.cwd, c.profile) != ""; got != c.blocked {
			t.Errorf("relaunchBlocker(%q, %q) blocked=%v, want %v", c.cwd, c.profile, got, c.blocked)
		}
	}
}
