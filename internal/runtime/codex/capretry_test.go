package codex

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

func TestCapacityFailureSchedulesBoundedRetries(t *testing.T) {
	ts := newThreadState("run_synth", "ses_synth", "thr_synth")
	at := time.Date(2026, 10, 8, 18, 0, 0, 0, time.UTC)
	capErr := &turnError{Message: "Selected model is at capacity. Please try a different model."}
	for i := range capBackoff {
		ts.onTurnCompleted(turn{ID: "t", Status: "failed", Error: capErr}, at)
		if want := at.Add(capBackoff[i]); !ts.retryAt.Equal(want) {
			t.Fatalf("retry %d at %v, want %v", i, ts.retryAt, want)
		}
		if ts.limit != nil {
			t.Fatal("capacity must not feed account fallback")
		}
		ts.capRetries++ // what maybeRetry does when it fires
	}
	ts.onTurnCompleted(turn{ID: "t", Status: "failed", Error: capErr}, at)
	if !ts.retryAt.IsZero() {
		t.Fatal("no retry after the backoff schedule is exhausted")
	}
	ts.onTurnCompleted(turn{ID: "t", Status: "completed"}, at)
	if ts.capRetries != 0 || ts.lastErr != nil {
		t.Fatal("a completed turn resets the retry budget and error")
	}
	ts.onTurnCompleted(turn{ID: "t", Status: "failed", Error: &turnError{Message: "You've hit your usage limit"}}, at)
	if ts.limit == nil || !ts.retryAt.IsZero() {
		t.Fatal("usage limits go to account fallback, not capacity retry")
	}
}

// A turn that fails at model capacity mid-work (a commentary message, then
// serverOverloaded; server, Nov 2026, iss_…) is not
// idle while Pyre's automatic continue is pending: idle sent the parent a
// "finished its turn" report with the commentary as the result. The run is
// idle only once the continued turn really completes.
func TestCodexCapacityFailureIsNotIdleWhileRetryPending(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	h := e.launch(e.a, "run_capacity", e.home, nil, nil)
	e.wait(e.a, h, 5*time.Second, idle)
	e.a.Observe(context.Background(), h, "")
	dlv(t, e.a, h, "msg_cap", "review FAKE_CAPACITY", contracts.ModeQueue)
	rr, _ := e.a.run(h)
	waitFor := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			rr.ts.mu.Lock()
			ok := cond()
			rr.ts.mu.Unlock()
			if ok {
				return
			}
			if time.Now().After(deadline) {
				t.Fatal(what)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	waitFor("the turn did not fail", func() bool { return rr.ts.lastErr != nil && !rr.ts.retryAt.IsZero() })
	for i := 0; i < 3; i++ {
		o, _ := e.a.Observe(context.Background(), h, "")
		if o.State == contracts.RunWaitingInput {
			t.Fatalf("a capacity failure with a retry pending was reported idle: %+v", o)
		}
		if o.State == contracts.RunWaitingBackground && !strings.Contains(o.StateReason, "Pyre continues it at") {
			t.Fatalf("reason does not say Pyre continues it: %q", o.StateReason)
		}
	}
	// The backoff elapses: Pyre continues the thread, which then completes.
	rr.ts.mu.Lock()
	rr.ts.retryAt = time.Now().Add(-time.Second)
	rr.ts.mu.Unlock()
	if o, _ := e.a.Observe(context.Background(), h, ""); o.State == contracts.RunWaitingInput {
		t.Fatalf("idle while the continue is being sent: %+v", o)
	}
	waitFor("the continued turn did not complete", func() bool {
		return rr.ts.lastErr == nil && rr.ts.status.Type == "idle" && !rr.retrying.Load()
	})
	var o contracts.Observation
	for i := 0; i < 3; i++ { // at most one "ended since the last observation"
		if o, _ = e.a.Observe(context.Background(), h, ""); o.State == contracts.RunWaitingInput {
			break
		}
	}
	if o.State != contracts.RunWaitingInput {
		t.Fatalf("idle after the continued turn completed: %+v", o)
	}
}
