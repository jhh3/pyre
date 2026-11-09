package daemon

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/resumenote"
	"github.com/jhh3/pyre/internal/runtime/mock"
)

// shellMock is the mock runtime presented as the shell harness (no native
// resume), recording each launch's arguments.
type shellMock struct {
	*mock.Adapter
	mu   sync.Mutex
	args map[string][]string // run -> launch args
}

func (s *shellMock) Name() string               { return "shell-mock" }
func (s *shellMock) Harness() contracts.Harness { return contracts.HarnessShell }
func (s *shellMock) Probe(ctx context.Context) (contracts.ProbeResult, error) {
	return contracts.ProbeResult{Harness: contracts.HarnessShell, Adapter: "shell-mock", Version: "test", Executable: "/bin/sh", Available: true,
		Capabilities: []string{contracts.CapStop}}, nil
}
func (s *shellMock) ResumeSpec(context.Context, contracts.Run, bool) (contracts.ContinuationKind, *contracts.NativeRef, error) {
	return contracts.ContinuationNone, nil, contracts.Errorf(contracts.ErrUnsupported, "shell sessions do not resume")
}
func (s *shellMock) Launch(ctx context.Context, l contracts.ResolvedLaunch) (contracts.RunHandle, error) {
	s.mu.Lock()
	s.args[l.RunID] = slices.Clone(l.Args)
	s.mu.Unlock()
	return s.Adapter.Launch(ctx, l)
}
func (s *shellMock) launchArgs(run string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.args[run]
}

// A monitor a reboot killed is relaunched by pyred at start-up with its
// recorded command line and no note (iss_…); a
// plain shell is not, and a monitor that predates the recorded command is
// left ended with the reason.
func TestCrashResumeRelaunchesMonitors(t *testing.T) {
	shortCrashResume(t)
	sh := &shellMock{Adapter: mock.NewAdapter(), args: map[string][]string{}}
	n := startNode(t, t.TempDir(), Config{Adapters: []contracts.RuntimeAdapter{sh}})
	cwd := t.TempDir()
	monArgs := []string{"-lc", `exec "${PYRE_CLI:-pyre}" monitor run --to ses_target --every 15m0s --cmd-b64 ZGF0ZQ --label-b64 ZGF0ZQ --for 40h0m0s --until-change`}
	start := func(session, run string, tags []string, args []string) {
		sc := startCmd(session, run, contracts.HarnessShell, cwd)
		sc.Session.Tags, sc.Args = tags, args
		sc.SessionToken = "tok-synthetic-" + session
		n.start(sc)
	}
	monTags := []string{contracts.TagMonitor, contracts.TagMonitorToPrefix + "ses_target", contracts.TagReportNone}
	start("ses_mon", "run_m1", monTags, monArgs)
	start("ses_shell", "run_sh1", nil, []string{"-lc", "sleep 1000"})
	if got := contracts.MonitorRelaunchArgs(n.run("run_m1").Run); !slices.Equal(got, monArgs) {
		t.Fatalf("recorded monitor args = %q", got)
	}
	if _, ok := n.run("run_sh1").Run.AdapterData[contracts.AdapterDataMonitorArgs]; ok {
		t.Fatal("a plain shell recorded monitor args")
	}
	for _, r := range []string{"run_m1", "run_sh1"} {
		sh.SetState(r, contracts.RunRunning)
		n.d.PollNow(context.Background(), r)
	}

	n.stop()
	sh.Vanish("run_m1")
	sh.Vanish("run_sh1")
	n = n.restart()

	var nr runRecord
	waitFor(t, "monitor relaunched", func() bool {
		nr = n.latestRun("ses_mon")
		return nr.Run.PreviousRunID == "run_m1" && slices.Contains(crashResumeLive, nr.Run.State)
	})
	if nr.Run.ContinuationKind != contracts.ContinuationFresh || !slices.Equal(sh.launchArgs(nr.Run.RunID), monArgs) ||
		!slices.Equal(contracts.MonitorRelaunchArgs(nr.Run), monArgs) {
		t.Fatalf("relaunch = %s, args %q", nr.Run.ContinuationKind, sh.launchArgs(nr.Run.RunID))
	}
	if old := n.run("run_m1"); !strings.Contains(old.Run.StateReason, "exit not observed") {
		t.Fatalf("old monitor run: %s", old.Run.StateReason)
	}
	time.Sleep(100 * time.Millisecond)
	n.d.WaitLaunches()
	if s := n.latestRun("ses_shell"); s.Run.RunID != "run_sh1" {
		t.Fatalf("plain shell resumed as %s", s.Run.RunID)
	}
	if _, ok := n.outboxBody(resumenote.MessageID("ses_mon", nr.Run.RunID)); ok {
		t.Fatal("a note was sent to the monitor's shell")
	}

	// A monitor started before args were recorded is not a candidate.
	legacy := n.run("run_m1")
	delete(legacy.Run.AdapterData, contracts.AdapterDataMonitorArgs)
	legacy.Run.State = contracts.RunRunning
	if ok, why := n.d.crashResumeCandidate(legacy); ok || !strings.Contains(why, "pyre monitor add") {
		t.Fatalf("legacy monitor candidate: %v %s", ok, why)
	}
}
