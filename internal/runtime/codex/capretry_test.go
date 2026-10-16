package codex

import (
	"testing"
	"time"
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
