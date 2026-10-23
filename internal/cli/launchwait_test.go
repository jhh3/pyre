package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
)

// launchCwd is the directory of the runs these tests start.
const launchCwd = "/home/synth/launchwait"

// observeLaunch plays the node: the first run in launchCwd seen starting
// goes to state with reason (the mock's fixture runs are left alone). It
// stops at test end.
func observeLaunch(t *testing.T, h *hub, state c.RunState, reason string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	t.Cleanup(func() { cancel(); <-done })
	go func() {
		defer close(done)
		for ctx.Err() == nil {
			page, err := h.b.ListSessions(ctx, c.ListSessionsRequest{States: []c.RunState{c.RunStarting}})
			if err == nil {
				for _, v := range page.Items {
					if v.Run != nil && v.Run.State == c.RunStarting && v.Run.Cwd == launchCwd {
						_ = h.b.SetRunState(v.Run.RunID, state, reason)
						return
					}
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
}

// pyre start waits (bounded) for the launch outcome: a launch that failed
// (issue iss_01M5KW3QAPJSDZZ4TAMKETXBF7: tmux refused the command line and
// start had said "started") exits 1 with the reason; a launch still
// starting at the deadline, or one that succeeded, exits 0.
func TestStartWaitsForLaunchOutcome(t *testing.T) {
	const reason = "launch failed: start claude in tmux: tmux new-session: command too long: exit status 1"
	args := []string{"start", "--node", "synth-mac", "--harness", "claude", "--cwd", launchCwd, "--prompt", "synthetic brief"}

	t.Run("failed", func(t *testing.T) {
		h := newHub(t)
		observeLaunch(t, h, c.RunFailed, reason)
		r := run(t, newEnv(t, h), &App{LaunchWait: 10 * time.Second}, args...)
		expectCode(t, r, ExitError)
		if !strings.Contains(r.stderr, "did not start") || !strings.Contains(r.stderr, "command too long") {
			t.Fatalf("the failure is not shown:\n%s", r)
		}
		if !strings.Contains(r.stdout, "started session ") || !strings.Contains(r.stdout, ": failed") || strings.Contains(r.stdout, "queued") {
			t.Fatalf("stdout:\n%s", r)
		}
		// --json: the session view with the failed run, and the same exit.
		observeLaunch(t, h, c.RunFailed, reason)
		r = run(t, newEnv(t, h), &App{LaunchWait: 10 * time.Second}, append([]string{"--json"}, args...)...)
		expectCode(t, r, ExitError)
		if res := mustJSON[c.StartSessionResponse](t, r); res.Session.Run == nil || res.Session.Run.State != c.RunFailed || res.Session.Run.StateReason != reason {
			t.Fatalf("json run: %+v", res.Session.Run)
		}
	})

	t.Run("succeeded", func(t *testing.T) {
		h := newHub(t)
		observeLaunch(t, h, c.RunRunning, "launched")
		start := time.Now()
		r := run(t, newEnv(t, h), &App{LaunchWait: 10 * time.Second}, args...)
		expectCode(t, r, 0)
		if !strings.Contains(r.stdout, ": running") || time.Since(start) > 5*time.Second {
			t.Fatalf("after %s:\n%s", time.Since(start), r)
		}
	})

	t.Run("still starting", func(t *testing.T) {
		h := newHub(t)
		start := time.Now()
		r := run(t, newEnv(t, h), &App{LaunchWait: 10 * time.Second}, append(args, "--wait", "200ms")...)
		expectCode(t, r, 0)
		if !strings.Contains(r.stdout, ": starting") || !strings.Contains(r.stdout, "queued") {
			t.Fatalf("stdout:\n%s", r)
		}
		if d := time.Since(start); d < 200*time.Millisecond || d > 5*time.Second {
			t.Fatalf("waited %s, want about 200ms", d)
		}
		// --wait 0 returns at once.
		start = time.Now()
		expectCode(t, run(t, newEnv(t, h), &App{LaunchWait: 10 * time.Second}, append(args, "--wait", "0")...), 0)
		if d := time.Since(start); d > 2*time.Second {
			t.Fatalf("--wait 0 waited %s", d)
		}
	})
}
