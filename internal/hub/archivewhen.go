package hub

import (
	"context"
	"database/sql"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// Archive when done (W-archive2).
//
// A session can archive itself (or a session it started) while it runs:
// UpdateSessionRequest{Archived: true, WhenDone: true} on a live session
// records the reserved tag contracts.TagArchiveWhenDone instead of
// archiving. This worker then waits for the current turn to end (the run
// waits for input: Claude's Stop hook, Codex's turn completion), stops the
// run through the ordinary stop command (Claude types /exit, Codex ends
// the thread, a shell exits) and archives the session once the run has
// ended. A shell has no turns: it is stopped after a short grace so the
// command that asked can return. Archived=false cancels the pending
// archive. Every step is audited.

const (
	// archiveWhenDonePoll bounds how long a pending archive waits for a
	// wake-up (the pass is one small query).
	archiveWhenDonePoll = 3 * time.Second
	// archiveStopRetry re-sends the stop when the run is still waiting
	// for input this long after the last one (a lost or expired command).
	archiveStopRetry = 2 * time.Minute
	// archiveShellGrace lets the shell command that scheduled its own
	// archive print its answer before the shell is stopped.
	archiveShellGrace = 2 * time.Second
)

// archiveWhenState is the worker's in-memory bookkeeping. Losing it (a hub
// restart) only re-sends a stop or shortens the shell grace.
type archiveWhenState struct {
	mu        sync.Mutex
	stops     map[string]time.Time // run ID → when its stop was enqueued
	scheduled map[string]time.Time // session ID → when it was scheduled (this process)
	kick      atomic.Bool
	wake      chan struct{}
}

func hasArchiveWhenDone(s contracts.Session) bool {
	return slices.Contains(s.Tags, contracts.TagArchiveWhenDone)
}

func withoutTag(tags []string, tag string) []string {
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		if t != tag {
			out = append(out, t)
		}
	}
	return out
}

// runNoHandle: the run's launch was interrupted before pyred recorded a
// handle; nothing can be stopped (W-resilience).
func runNoHandle(r contracts.Run) bool {
	return r.State == contracts.RunUnknown && r.AdapterData["pyre.no_handle"] == "true"
}

// runLive reports that the session's current run may still be executing.
func (h *Hub) runLive(ctx context.Context, sessionID string) bool {
	s, err := h.getSession(ctx, h.db, sessionID)
	if err != nil || s.CurrentRunID == "" {
		return false
	}
	r, err := h.getRun(ctx, h.db, s.CurrentRunID)
	if err != nil {
		return false
	}
	return !r.State.Terminal() && !runNoHandle(r)
}

// noteArchiveScheduled remembers when a session was scheduled and wakes
// the worker (an idle child is stopped at once).
func (h *Hub) noteArchiveScheduled(sessionID string) {
	h.awd.mu.Lock()
	h.awd.scheduled[sessionID] = h.now()
	h.awd.mu.Unlock()
	h.awd.kick.Store(true)
	h.kickArchiveWhenDone()
}

// noteRunTransition wakes the worker after a commit that ended a turn or
// a run (called inside the run-state transaction; cheap).
func (h *Hub) noteRunTransition(prev contracts.RunState, r contracts.Run) {
	if prev != r.State && (r.State == contracts.RunWaitingInput || r.State.Terminal()) {
		h.awd.kick.Store(true)
	}
}

func (h *Hub) kickArchiveWhenDone() {
	if h.awd.kick.Swap(false) {
		select {
		case h.awd.wake <- struct{}{}:
		default:
		}
	}
}

func (h *Hub) archiveWhenDoneLoop() {
	defer h.wg.Done()
	t := time.NewTicker(archiveWhenDonePoll)
	defer t.Stop()
	for {
		select {
		case <-h.stop:
			return
		case <-h.awd.wake:
		case <-t.C:
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		_ = h.ProcessArchiveWhenDone(ctx)
		cancel()
	}
}

// ProcessArchiveWhenDone advances every pending archive: a session whose
// run has ended is archived; one whose turn has ended is stopped.
func (h *Hub) ProcessArchiveWhenDone(ctx context.Context) error {
	rows, err := h.db.QueryContext(ctx, `SELECT session_id FROM sessions WHERE archived=0 AND tags LIKE ?`, "%"+contracts.TagArchiveWhenDone+"%")
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	h.awd.mu.Lock()
	for id, at := range h.awd.stops {
		if h.now().Sub(at) > time.Hour {
			delete(h.awd.stops, id)
		}
	}
	h.awd.mu.Unlock()
	var first error
	for _, id := range ids {
		if err := h.advanceArchive(ctx, id); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (h *Hub) advanceArchive(ctx context.Context, sessionID string) error {
	s, err := h.getSession(ctx, h.db, sessionID)
	if err != nil || s.Archived || !hasArchiveWhenDone(s) {
		return nil
	}
	if s.CurrentRunID == "" {
		return h.finishArchive(ctx, sessionID, "it has no run")
	}
	r, err := h.getRun(ctx, h.db, s.CurrentRunID)
	if err != nil {
		return err
	}
	switch {
	case r.State.Terminal():
		return h.finishArchive(ctx, sessionID, "its run "+string(r.State))
	case runNoHandle(r):
		return h.finishArchive(ctx, sessionID, "its run has no handle Pyre could stop")
	case !contracts.HasCap(r.Capabilities, contracts.CapStop):
		return h.finishArchive(ctx, sessionID, "its run cannot be stopped by Pyre; archived while it may still run")
	}
	if !h.turnEnded(s, r) {
		return nil
	}
	h.awd.mu.Lock()
	last, sent := h.awd.stops[r.RunID]
	h.awd.mu.Unlock()
	if sent && h.now().Sub(last) < archiveStopRetry {
		return nil
	}
	reason := "archive when done: the turn ended"
	if s.Harness == contracts.HarnessShell {
		reason = "archive when done"
	}
	err = h.tx(ctx, func(tx *sql.Tx) error {
		sys := contracts.Principal{Kind: contracts.PrincipalSystem, ID: "hub"}
		if _, err := h.enqueueTx(ctx, tx, r.NodeID, contracts.CmdStop, sys, contracts.RunCommand{SessionID: r.SessionID, RunID: r.RunID, Reason: reason},
			h.opt.StopTTL, contracts.ObjectRef{Kind: contracts.KindRun, ID: r.RunID}); err != nil {
			return err
		}
		h.auditTx(ctx, tx, auditRec{Action: contracts.AuditSessionStopped, Actor: pyreActor, NodeID: r.NodeID, SessionID: r.SessionID, RunID: r.RunID, Reason: reason})
		return nil
	})
	if err == nil {
		h.awd.mu.Lock()
		h.awd.stops[r.RunID] = h.now()
		h.awd.mu.Unlock()
	}
	return err
}

// turnEnded: the run waits for input (the turn that asked has ended), or
// a shell (which has no turns) is past its grace.
func (h *Hub) turnEnded(s contracts.Session, r contracts.Run) bool {
	if r.State == contracts.RunWaitingInput {
		return true
	}
	if s.Harness != contracts.HarnessShell || r.State == contracts.RunStarting {
		return false
	}
	h.awd.mu.Lock()
	at, ok := h.awd.scheduled[s.SessionID]
	h.awd.mu.Unlock()
	return !ok || h.now().Sub(at) >= archiveShellGrace
}

// finishArchive archives a session whose pending archive can complete and
// revokes its agent tokens.
func (h *Hub) finishArchive(ctx context.Context, sessionID, why string) error {
	done := false
	err := h.tx(ctx, func(tx *sql.Tx) error {
		s, err := h.getSession(ctx, tx, sessionID)
		if err != nil || s.Archived || !hasArchiveWhenDone(s) {
			return err
		}
		node := ""
		if r, err := h.getRun(ctx, tx, s.CurrentRunID); err == nil {
			node = r.NodeID
		}
		s.Archived = true
		s.Tags = withoutTag(s.Tags, contracts.TagArchiveWhenDone)
		if err := h.updateSessionTx(ctx, tx, s); err != nil {
			return err
		}
		if err := h.emitSession(ctx, tx, contracts.EvSessionUpdated, s, "hub", 0); err != nil {
			return err
		}
		h.auditTx(ctx, tx, auditRec{Action: contracts.AuditSessionArchived, Actor: pyreActor, NodeID: node, SessionID: sessionID,
			Reason: "archived when done: " + why})
		done = true
		return nil
	})
	if err != nil || !done {
		return err
	}
	h.awd.mu.Lock()
	delete(h.awd.scheduled, sessionID)
	h.awd.mu.Unlock()
	if h.cfg.Tokens != nil {
		return h.cfg.Tokens.RevokeSessionTokens(ctx, sessionID)
	}
	return nil
}
