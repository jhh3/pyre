package daemon

import (
	"context"
	"os/exec"
	"sync"
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/supervisor"
	"github.com/jhh3/pyre/internal/terminal"
)

type fakeOpener struct {
	mu    sync.Mutex
	calls []contracts.TerminalOpenCommand
	argv  [][]string
	err   error
}

func (f *fakeOpener) Open(ctx context.Context, tc contracts.TerminalOpenCommand, argv []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, tc)
	f.argv = append(f.argv, argv)
	return f.err
}

// TestTerminalOpenUsesRunAttachArgv: terminal_open hands the run's own
// isolated tmux attach argv (with -r when read-only) to the bridge.
func TestTerminalOpenUsesRunAttachArgv(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	dir := shortDir(t)
	op := &fakeOpener{}
	n := startNode(t, dir, Config{Adapters: []contracts.RuntimeAdapter{&tmuxShell{}}, Terminal: op})
	n.start(startCmd("ses_wt", "run_wt", contracts.HarnessShell, t.TempDir()))
	rec := n.run("run_wt")
	tm := supervisor.Tmux{Socket: rec.Handle.TmuxSocket, OwnedRoot: dir}
	t.Cleanup(func() { tm.Kill(context.Background()) })

	res := n.do(command(terminal.CmdTerminalOpen, contracts.TerminalOpenCommand{StreamID: "tstr_synth1", RunID: "run_wt", Cols: 90, Rows: 20}))
	if res.Outcome != contracts.OutcomeSucceeded || res.Error != nil {
		t.Fatalf("terminal_open = %+v", res)
	}
	res = n.do(command(terminal.CmdTerminalOpen, contracts.TerminalOpenCommand{StreamID: "tstr_synth2", RunID: "run_wt", ReadOnly: true}))
	if res.Outcome != contracts.OutcomeSucceeded {
		t.Fatalf("read-only terminal_open = %+v", res)
	}
	want := tm.AttachArgv("pyre", false)
	if len(op.argv) != 2 || len(op.argv[0]) != len(want) || op.argv[0][2] != rec.Handle.TmuxSocket || op.argv[0][len(want)-1] != "pyre" {
		t.Fatalf("interactive argv = %q, want %q", op.argv, want)
	}
	if ro := op.argv[1]; ro[len(ro)-1] != "-r" || !op.calls[1].ReadOnly {
		t.Fatalf("read-only argv = %q", ro)
	}
	// Unknown runs and nodes without a bridge are refused.
	if res := n.do(command(terminal.CmdTerminalOpen, contracts.TerminalOpenCommand{StreamID: "tstr_synth3", RunID: "run_missing"})); res.Error == nil || res.Error.Code != contracts.ErrNotFound {
		t.Fatalf("unknown run = %+v", res)
	}
	op.mu.Lock()
	op.err = contracts.Errorf(contracts.ErrUnavailable, "synthetic dial failure")
	op.mu.Unlock()
	if res := n.do(command(terminal.CmdTerminalOpen, contracts.TerminalOpenCommand{StreamID: "tstr_synth4", RunID: "run_wt"})); res.Error == nil || res.Error.Code != contracts.ErrUnavailable {
		t.Fatalf("bridge failure = %+v", res)
	}
}

func TestTerminalOpenWithoutBridgeIsUnsupported(t *testing.T) {
	n := startNode(t, shortDir(t), Config{})
	res := n.do(command(terminal.CmdTerminalOpen, contracts.TerminalOpenCommand{StreamID: "tstr_synth", RunID: "run_x"}))
	if res.Outcome != contracts.OutcomeUnsupported {
		t.Fatalf("terminal_open without bridge = %+v", res)
	}
}
