package daemon

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/mock"
)

func TestRunModelAndEffort(t *testing.T) {
	rec := &launchRecorder{RuntimeAdapter: mock.NewAdapter(), last: map[string]contracts.ResolvedLaunch{}}
	n := startNode(t, t.TempDir(), Config{Adapters: []contracts.RuntimeAdapter{rec}})
	cwd := t.TempDir()

	// Nothing chosen: no model, no effort, nothing recorded (the harness
	// default, as before).
	r := n.start(startCmd("ses_m1", "run_m1", contracts.HarnessMock, cwd))
	if l := rec.get("run_m1"); l.Model != "" || l.Effort != "" || r.AdapterData[adapterDataModel] != "" || r.AdapterData[adapterDataEffort] != "" {
		t.Fatalf("default: launch %q/%q data %v", l.Model, l.Effort, r.AdapterData)
	}

	// Chosen: recorded on the run, handed to the adapter, never as args.
	sc := startCmd("ses_m2", "run_m2", contracts.HarnessMock, cwd)
	sc.Args = []string{"--pyre-model=synthetic-model-1", "--pyre-effort", "high", "--other"}
	r = n.start(sc)
	l := rec.get("run_m2")
	if l.Model != "synthetic-model-1" || l.Effort != "high" || !slices.Equal(l.Args, []string{"--other"}) {
		t.Fatalf("chosen: model %q effort %q args %v", l.Model, l.Effort, l.Args)
	}
	if r.AdapterData[adapterDataModel] != "synthetic-model-1" || r.AdapterData[adapterDataEffort] != "high" {
		t.Fatalf("recorded %v", r.AdapterData)
	}

	// Refused: flag-shaped or shell-shaped values.
	for _, args := range [][]string{{"--pyre-model=--dangerous"}, {"--pyre-effort=HIGH;rm"}, {"--pyre-model"}} {
		bad := startCmd("ses_m3", "run_m3", contracts.HarnessMock, cwd)
		bad.Args = args
		if res := n.do(command(contracts.CmdStartRun, bad)); res.Error == nil {
			t.Fatalf("%v accepted", args)
		}
	}
}

func TestRunModelResumeCarriesOver(t *testing.T) {
	prev := &contracts.Run{Harness: contracts.HarnessAgy, AdapterData: map[string]string{adapterDataModel: "m-prev", adapterDataEffort: "low"}}
	if m, e := runModel(contracts.HarnessAgy, "", "", prev); m != "m-prev" || e != "low" {
		t.Fatalf("resume kept %q %q", m, e)
	}
	if m, e := runModel(contracts.HarnessAgy, "m-new", "", prev); m != "m-new" || e != "low" {
		t.Fatalf("override %q %q", m, e)
	}
	if m, e := runModel(contracts.HarnessClaude, "", "", prev); m != "" || e != "" {
		t.Fatalf("other harness inherited %q %q", m, e)
	}
	if m, e := runModel(contracts.HarnessShell, "x", "high", nil); m != "" || e != "" {
		t.Fatalf("shell %q %q", m, e)
	}
}

// absentAdapter is a harness that is not installed on the node.
type absentAdapter struct{ contracts.RuntimeAdapter }

func (absentAdapter) Harness() contracts.Harness { return contracts.HarnessAgy }
func (absentAdapter) Name() string               { return "agy-tmux" }
func (absentAdapter) Probe(context.Context) (contracts.ProbeResult, error) {
	return contracts.ProbeResult{Harness: contracts.HarnessAgy, Adapter: "agy-tmux", Absent: true, Reason: "agy not installed"}, nil
}

// presentAdapter reports model choices.
type presentAdapter struct{ contracts.RuntimeAdapter }

func (p presentAdapter) Probe(ctx context.Context) (contracts.ProbeResult, error) {
	pr, err := p.RuntimeAdapter.Probe(ctx)
	pr.Models, pr.Efforts = []contracts.ModelChoice{{ID: "synthetic-m", Label: "Synthetic M"}}, []string{"low", "high"}
	return pr, err
}

// An effort the harness does not offer on the node is refused (Claude
// would ignore it while the run recorded it).
func TestRunEffortCheckedAgainstTheHarness(t *testing.T) {
	n := startNode(t, t.TempDir(), Config{Adapters: []contracts.RuntimeAdapter{presentAdapter{mock.NewAdapter()}}})
	sc := startCmd("ses_e1", "run_e1", contracts.HarnessMock, t.TempDir())
	sc.Args = []string{"--pyre-effort=ultra"}
	if res := n.do(command(contracts.CmdStartRun, sc)); res.Error == nil || !strings.Contains(res.Error.Message, "not offered") {
		t.Fatalf("ultra: %+v", res.Error)
	}
	sc = startCmd("ses_e2", "run_e2", contracts.HarnessMock, t.TempDir())
	sc.Args = []string{"--pyre-effort=high"}
	if r := n.start(sc); r.AdapterData[adapterDataEffort] != "high" {
		t.Fatalf("high: %v", r.AdapterData)
	}
}

func TestHarnessInfoSkipsAbsentAndCarriesModels(t *testing.T) {
	n := startNode(t, t.TempDir(), Config{Adapters: []contracts.RuntimeAdapter{presentAdapter{mock.NewAdapter()}, absentAdapter{mock.NewAdapter()}}})
	his := n.d.HarnessInfo(context.Background())
	if len(his) != 1 || his[0].Harness != contracts.HarnessMock || len(his[0].Models) != 1 || his[0].Models[0].ID != "synthetic-m" || len(his[0].Efforts) != 2 {
		t.Fatalf("harness info %+v", his)
	}
	// A launch on the absent harness says why.
	res := n.do(command(contracts.CmdStartRun, startCmd("ses_ab", "run_ab", contracts.HarnessAgy, t.TempDir())))
	if res.Error == nil || !strings.Contains(res.Error.Message, "not installed") {
		t.Fatalf("absent launch: %+v", res.Error)
	}
}
