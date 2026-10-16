package codex

import (
	"context"
	"fmt"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// maybeRetry continues a thread whose last turn failed at model capacity
// once its backoff has elapsed, and reads the last turn once to learn why a
// thread is in systemError (after a pyred restart the failure notification
// was never seen). Called from Observe with rr.ts.mu held.
func (a *Adapter) maybeRetry(h contracts.RunHandle, rr *runRef, now time.Time) {
	ts := rr.ts
	if ts.status.Type == "systemError" && ts.lastErr == nil && !ts.errProbed {
		ts.errProbed = true
		go a.probeLastTurn(rr)
	}
	if ts.retryAt.IsZero() || now.Before(ts.retryAt) || ts.status.Type == "active" || rr.retrying.Load() {
		return
	}
	for _, pa := range ts.approvals {
		if pa.approval.State == contracts.ApprovalPending {
			return
		}
	}
	ts.capRetries++
	n := ts.capRetries
	ts.retryAt = time.Time{}
	rr.retrying.Store(true)
	go func() {
		defer rr.retrying.Store(false)
		a.continueAfterCapacity(h, rr, n)
	}()
}

// probeLastTurn learns the newest turn's error when the thread reports
// systemError without one recorded.
func (a *Adapter) probeLastTurn(rr *runRef) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := a.client(ctx, rr, false)
	if err != nil {
		return
	}
	var resp turnsListResponse
	if err := c.call(ctx, "thread/turns/list", turnsListParams{ThreadID: rr.meta.ThreadID, Limit: 1, ItemsView: "full", SortDirection: "desc"}, &resp); err != nil || len(resp.Data) == 0 {
		return
	}
	t := resp.Data[0]
	if t.Status != "failed" || t.Error == nil {
		return
	}
	rr.ts.mu.Lock()
	defer rr.ts.mu.Unlock()
	if rr.ts.lastErr == nil && rr.ts.status.Type == "systemError" {
		rr.ts.noteFailure(*t.Error, a.o.Now())
	}
}

// continueAfterCapacity starts a new turn asking the agent to continue. It
// is a new instruction, not a replay: the failed turn ended with a definite
// failure. Skipped when someone else already started a turn.
func (a *Adapter) continueAfterCapacity(h contracts.RunHandle, rr *runRef, n int) {
	if stopped(h.StateDir) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	rr.deliverMu.Lock()
	defer rr.deliverMu.Unlock()
	c, err := a.client(ctx, rr, true)
	if err != nil {
		return
	}
	rr.ts.mu.Lock()
	busy := rr.ts.status.Type == "active"
	rr.ts.mu.Unlock()
	if busy {
		return
	}
	text := fmt.Sprintf("[pyre] Your previous turn failed because the model was at capacity. This is automatic retry %d of %d; continue where you left off.", n, len(capBackoff))
	id := fmt.Sprintf("pyre-capretry-%d-%d", n, a.o.Now().Unix())
	var tr turnStartResponse
	if err := c.call(ctx, "turn/start", turnStartParams{ThreadID: rr.meta.ThreadID, Input: []userInput{{Type: "text", Text: text}}, ClientUserMessageID: id}, &tr); err != nil {
		return
	}
	if a.o.OnAutomation != nil {
		go a.o.OnAutomation(contracts.NodeAutomation{RunID: rr.meta.RunID, SessionID: rr.meta.SessionID, Harness: contracts.HarnessCodex,
			Action: contracts.AutomationCapacityRetry, Detail: fmt.Sprintf("retry %d of %d", n, len(capBackoff))})
	}
}
