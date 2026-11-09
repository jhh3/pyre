package daemon

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/daemon/daemontest"
)

// guardAdapter reports synthetic background tasks (contracts.BackgroundGuard,
// decision I67) and refuses a guarded stop while it has any.
type guardAdapter struct {
	contracts.RuntimeAdapter
	mu      sync.Mutex
	tasks   []string
	guarded int
	plain   int
}

func (g *guardAdapter) BackgroundWork(ctx context.Context, h contracts.RunHandle) []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.tasks)
}

func (g *guardAdapter) StopUnlessBackground(ctx context.Context, h contracts.RunHandle) (contracts.CommandReceipt, error) {
	g.mu.Lock()
	g.guarded++
	tasks := slices.Clone(g.tasks)
	g.mu.Unlock()
	if len(tasks) > 0 {
		return contracts.CommandReceipt{Outcome: contracts.OutcomeFailed, Detail: "not stopped: background tasks"},
			contracts.Errorf(contracts.ErrConflict, "not stopped").WithDetail(contracts.DetailBackgroundWork, tasks)
	}
	return g.RuntimeAdapter.Stop(ctx, h)
}

func (g *guardAdapter) Stop(ctx context.Context, h contracts.RunHandle) (contracts.CommandReceipt, error) {
	g.mu.Lock()
	g.plain++
	g.mu.Unlock()
	return g.RuntimeAdapter.Stop(ctx, h)
}

func (g *guardAdapter) set(tasks ...string) {
	g.mu.Lock()
	g.tasks = tasks
	g.mu.Unlock()
}

func (n *node) adapterData(runID string) map[string]string {
	n.t.Helper()
	rec, ok, err := n.d.st.run(context.Background(), runID)
	if err != nil || !ok {
		n.t.Fatalf("run %s: %v %v", runID, ok, err)
	}
	return rec.Run.AdapterData
}

// A proactive move's stop (guarded) leaves a run with live background tasks
// running and unmarked; a limit switch's stop ends them and says which; a
// guarded stop of a run without any proceeds. The run carries the switch
// marker the hub words its report from.
func TestQuiesceRunBackgroundGuard(t *testing.T) {
	w := daemontest.NewWorld()
	ga := &guardAdapter{RuntimeAdapter: daemontest.New(w, contracts.HarnessClaude)}
	n := startNode(t, t.TempDir(), Config{Adapters: []contracts.RuntimeAdapter{ga}, Accounts: &fakeBinder{}})
	cwd := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	n.start(startCmd("ses_qb1", "run_qb1", contracts.HarnessClaude, cwd))
	ga.set("shell: until grep -q synced /tmp/synthetic.log")
	if got := n.d.RunBackgroundWork(ctx, "run_qb1"); !slices.Equal(got, []string{"shell: until grep -q synced /tmp/synthetic.log"}) {
		t.Fatalf("background work %q", got)
	}
	if got := n.d.RunBackgroundWork(ctx, "run_nonesuch"); got != nil {
		t.Fatalf("unknown run %q", got)
	}
	q, err := n.d.QuiesceRun(ctx, "ses_qb1", "run_qb1", contracts.AccountSwitchProactive, true)
	if err != nil || q.Exited || len(q.Refused) != 1 || ga.guarded != 1 || ga.plain != 0 {
		t.Fatalf("guarded stop with a live task: %+v %v (guarded %d plain %d)", q, err, ga.guarded, ga.plain)
	}
	if _, live := n.d.getLive("run_qb1"); !live {
		t.Fatal("the run was stopped")
	}
	if ad := n.adapterData("run_qb1"); ad[contracts.AdapterDataAccountSwitch] != "" || ad[adapterDataStopRequested] != "" {
		t.Fatalf("markers left on a run that was not stopped: %v", ad)
	}

	// A limit leaves no choice: the stop proceeds and names what it ended.
	q, err = n.d.QuiesceRun(ctx, "ses_qb1", "run_qb1", contracts.AccountSwitchLimit, false)
	if err != nil || !q.Exited || !slices.Equal(q.Stopped, []string{"shell: until grep -q synced /tmp/synthetic.log"}) || ga.plain != 1 {
		t.Fatalf("limit stop: %+v %v", q, err)
	}
	if ad := n.adapterData("run_qb1"); ad[contracts.AdapterDataAccountSwitch] != contracts.AccountSwitchLimit {
		t.Fatalf("switch marker: %v", ad)
	}

	// No background tasks: the guarded stop goes ahead.
	ga.set()
	n.start(startCmd("ses_qb2", "run_qb2", contracts.HarnessClaude, cwd))
	q, err = n.d.QuiesceRun(ctx, "ses_qb2", "run_qb2", contracts.AccountSwitchProactive, true)
	if err != nil || !q.Exited || len(q.Refused) != 0 || len(q.Stopped) != 0 {
		t.Fatalf("guarded stop without tasks: %+v %v", q, err)
	}
	if ad := n.adapterData("run_qb2"); ad[contracts.AdapterDataAccountSwitch] != contracts.AccountSwitchProactive {
		t.Fatalf("switch marker: %v", ad)
	}
}
