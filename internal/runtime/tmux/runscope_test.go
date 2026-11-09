package tmux

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/supervisor"
)

// useRunScope makes every pane of this test process get sc (or no scope).
func useRunScope(t *testing.T, sc *supervisor.RunScope) {
	t.Helper()
	old := decideRunScope
	decideRunScope = func(context.Context, supervisor.RunScopeLimits) supervisor.RunScopeDecision {
		if sc == nil {
			return supervisor.RunScopeDecision{Reason: "test: off"}
		}
		return supervisor.RunScopeDecision{Use: true, Scope: *sc}
	}
	SetRunScopeLimits(&supervisor.RunScopeLimits{MemoryMax: "65%"})
	t.Cleanup(func() { decideRunScope = old; SetRunScopeLimits(nil) })
}

func testScope() supervisor.RunScope {
	return supervisor.RunScope{Busctl: "/usr/bin/busctl", Slice: "pyre-test.slice", MemoryMax: 64 << 20, SwapZero: true, OOMPolicy: true}
}

// A scoped pane always goes through the launcher, and its launch file
// carries the scope next to the environment and argv.
func TestScopedPaneLaunchFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no tmux")
	}
	s := &Server{Bin: "tmux", Socket: filepath.Join(t.TempDir(), "s.sock"), ExecHelper: "/opt/pyre/pyred"}
	sc := testScope()
	argv, file, err := s.paneCommandScoped(SessionSpec{Name: "run_synth-1", Argv: []string{"claude", "--x"}}, &sc)
	if err != nil {
		t.Fatal(err)
	}
	if file == "" || strings.Join(argv, " ") != "/opt/pyre/pyred "+ExecEnvArg+" "+file+" --" {
		t.Fatalf("argv %q file %q", argv, file)
	}
	lf, err := readLaunchFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if lf.Scope == nil || *lf.Scope != sc || lf.ScopeName != "run_synth-1" || !strings.HasPrefix(lf.ScopeUnit, "pyre-run-run_synth-1-") ||
		strings.Join(lf.Argv, " ") != "claude --x" {
		t.Fatalf("launch file %+v scope %+v", lf, lf.Scope)
	}
	// Without a scope a plain short command stays inline.
	if argv, file, _ := s.paneCommandScoped(SessionSpec{Name: "p", Argv: []string{"sh", "-c", "true"}}, nil); file != "" || argv[0] != "sh" {
		t.Fatalf("unscoped %q %q", argv, file)
	}
	// A forged unit name is refused.
	bad := filepath.Join(filepath.Dir(s.Socket), "pyre-env-bad")
	if err := os.WriteFile(bad, []byte("Aclaude\x00G../etc.scope\tx\x00Kbusctl=/b\x00Kslice=s\x00"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readLaunchFile(bad); err == nil {
		t.Fatal("forged unit accepted")
	}
}

func TestJoinRunScope(t *testing.T) {
	oldRun, oldCg, oldTimeout := runBusctl, selfCgroup, joinRunScopeTimeout
	t.Cleanup(func() { runBusctl, selfCgroup, joinRunScopeTimeout = oldRun, oldCg, oldTimeout })
	joinRunScopeTimeout = 300 * time.Millisecond
	cg := "/user.slice/user-1000.slice/user@1000.service/pyre.slice/pyre-runs.slice/tmux-spawn-x.scope"
	selfCgroup = func() string { return cg }
	var calls [][]string
	moveOn := func(oomOK bool) func(context.Context, []string) error {
		return func(_ context.Context, argv []string) error {
			calls = append(calls, argv)
			joined := strings.Join(argv, " ")
			if strings.Contains(joined, "OOMPolicy") && !oomOK {
				return errors.New("exit status 1: Call failed: Cannot set property OOMPolicy, or unknown property.")
			}
			cg = "/user.slice/user-1000.slice/user@1000.service/pyre.slice/pyre-test.slice/" + argv[10]
			return nil
		}
	}
	sc := testScope()

	runBusctl = moveOn(true)
	note, err := joinRunScope("pyre-run-run_a-1.scope", "Pyre run run_a", sc)
	if err != nil || len(calls) != 1 || !strings.Contains(note, "scope pyre-run-run_a-1.scope MemoryMax=64M MemorySwapMax=0 OOMPolicy=continue") {
		t.Fatalf("join: %q %v calls %v", note, err, calls)
	}
	pid := os.Getpid()
	if a := strings.Join(calls[0], " "); !strings.Contains(a, "PIDs au 1 "+strconv.Itoa(pid)) || !strings.Contains(a, "Slice s pyre-test.slice") {
		t.Fatalf("args %s", a)
	}

	// A user manager without OOMPolicy= on scopes: retried without it.
	calls = nil
	runBusctl = moveOn(false)
	note, err = joinRunScope("pyre-run-run_b-1.scope", "Pyre run run_b", sc)
	if err != nil || len(calls) != 2 || !strings.Contains(note, "without OOMPolicy=continue") || strings.Contains(strings.Join(calls[1], " "), "OOMPolicy") {
		t.Fatalf("retry: %q %v calls %v", note, err, calls)
	}

	// No user bus: the error goes back (the launcher runs unscoped).
	runBusctl = func(context.Context, []string) error {
		return errors.New("exit status 1: Failed to connect to bus: No such file or directory")
	}
	if _, err := joinRunScope("pyre-run-run_c-1.scope", "d", sc); err == nil || !strings.Contains(err.Error(), "Failed to connect to bus") {
		t.Fatalf("no bus: %v", err)
	}

	// Started but never moved: bounded wait, then an error.
	runBusctl = func(context.Context, []string) error { return nil }
	start := time.Now()
	if _, err := joinRunScope("pyre-run-run_d-1.scope", "d", sc); err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("no move: %v after %s", err, time.Since(start))
	}
}

// Without a user bus the pane still runs its program, unscoped, and the
// launcher logs why.
func TestScopedPaneRunsWithoutUserBus(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("per-run scopes are Linux-only")
	}
	s := testServer(t)
	sc := testScope()
	sc.Busctl = filepath.Join(t.TempDir(), "no-such-busctl")
	useRunScope(t, &sc)
	out := filepath.Join(t.TempDir(), "ran")
	ctx := context.Background()
	if err := s.NewSession(ctx, SessionSpec{Name: "run_synth-nobus", Env: map[string]string{"PYRE_SYNTH": "x"},
		Argv: []string{"/bin/sh", "-c", "echo \"$PYRE_SYNTH\" > " + out + "; sleep 30"}}); err != nil {
		t.Fatal(err)
	}
	if !waitFor(t, 10*time.Second, func() bool { b, _ := os.ReadFile(out); return strings.TrimSpace(string(b)) == "x" }) {
		screen, _ := s.CapturePane(ctx, "run_synth-nobus", CaptureOptions{Join: true})
		t.Fatalf("the program did not run without a bus; pane: %s", screen)
	}
	log, _ := os.ReadFile(filepath.Join(filepath.Dir(s.Socket), RunScopeLog))
	if !strings.Contains(string(log), "run_synth-nobus unscoped: ") {
		t.Fatalf("scope log %q", log)
	}
}
