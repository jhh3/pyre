package hub

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/persistence"
)

// Session lifecycle cleanup (W-lifecycle; owner: "i see idle sessions -
// make sure they're getting closed and worktrees cleaned up when we're
// finished", Oct 8; ~80 such requests in two weeks).
//
//   - A child session (started by another session) whose run ended is
//     archived once its final report to the parent is settled (sent,
//     skipped as redundant, or folded into the parent's next report), at
//     least childArchiveGrace after a clean exit or childEndedArchiveGrace
//     after any other end (a non-zero or missing exit code, a failure), and
//     only while nobody touched it since: no new run, and the owner has not
//     messaged, typed into or attached to it (a child the owner engages with
//     is his to archive). Sessions the owner started are never archived
//     here. Before Oct 2026 only clean exits qualified and a folded report
//     (pending up to childReportHold) blocked the archive, so children that
//     exited with 143 or were stopped by their parent stayed in the
//     owner's sidebar for days (docs/operations.md, Automatic cleanup).
//   - Every automatic archive (that one, archive-when-done, the nightly
//     sweep) asks the session's node to clean up after it (session.cleanup):
//     stop processes the session left running and remove the worktrees it
//     created that are clean and whose branch is merged or pushed.
//   - Once a night (nightlySweepHour, owner's zone) the hub archives
//     sessions that failed to launch or ended over a day ago, asks every
//     connected node to sweep (node.sweep: their cleanup, orphan worktrees,
//     stale harness temp folders) and sends the owner one message saying
//     what was archived, removed and kept.
//
// Archived sessions stay resumable; nothing with uncommitted or unpushed
// work is removed.

const (
	// lifecyclePoll is the child auto-archive cadence.
	lifecyclePoll = 30 * time.Second
	// childArchiveGrace lets the parent react to the ended report before the
	// child is archived, and outlasts an account move between turns (stop,
	// then native resume on another account; 3-minute deadline).
	childArchiveGrace = 5 * time.Minute
	// childEndedArchiveGrace is the grace for a child whose run did not exit
	// cleanly: the parent (or the owner) has an hour to look at or resume it.
	childEndedArchiveGrace = time.Hour
	// staleAfter: an ended session is archived by the nightly sweep after
	// this; a failed launch after launchFailedAfter.
	staleAfter        = 24 * time.Hour
	launchFailedAfter = time.Hour
	// The sweep runs inside this local window (owner's zone), never later
	// in the day: a hub that was down at night skips that night.
	sweepWindowStart = 3*time.Hour + 30*time.Minute
	sweepWindowEnd   = 5 * time.Hour
	// sweepNodeWait bounds how long the sweep waits for each node's answer.
	sweepNodeWait = 10 * time.Minute
	// cleanupWait bounds how long a session cleanup is followed for its audit.
	cleanupWait = 5 * time.Minute
	// sweepMetaKey records the local date of the last nightly sweep;
	// dryRunMetaKey that the first sweep's dry-run report reached the owner
	// (until then every sweep is a dry run).
	sweepMetaKey  = "lifecycle.last_sweep"
	dryRunMetaKey = "lifecycle.dry_run_reported"
)

// cleanupSender is the system principal of the owner's cleanup notice.
var cleanupSender = contracts.Principal{Kind: contracts.PrincipalSystem, ID: "cleanup"}

type lifecycleState struct {
	sweeping atomic.Bool
	mu       sync.Mutex
}

func (h *Hub) lifecycleLoop() {
	defer h.wg.Done()
	t := time.NewTicker(lifecyclePoll)
	defer t.Stop()
	for {
		select {
		case <-h.stop:
			return
		case <-t.C:
		}
		ctx, cancel := context.WithTimeout(context.Background(), lifecyclePoll)
		_, _ = h.EndMissingUnknownRuns(ctx) // unknownruns.go
		_, _ = h.ArchiveFinishedChildren(ctx)
		cancel()
		if due, dry := h.sweepDue(context.Background()); due {
			h.wg.Add(1)
			go func() {
				defer h.wg.Done()
				ctx, cancel := context.WithTimeout(context.Background(), sweepNodeWait+5*time.Minute)
				defer cancel()
				_, _ = h.RunNightlySweep(ctx, dry) // dry runs until one report reached the owner
			}()
		}
	}
}

// ArchiveFinishedChildren archives child sessions whose run ended, whose
// final report has been settled and that nobody touched since (see the
// rules above), then asks their nodes to clean up. It returns the archived
// session IDs. Archiving sends the parent nothing: the ended report is
// already settled, and a folded one still rides along unchanged.
func (h *Hub) ArchiveFinishedChildren(ctx context.Context) ([]string, error) {
	rows, err := h.db.QueryContext(ctx, `SELECT s.session_id FROM sessions s JOIN runs r ON r.run_id=s.current_run_id
		WHERE s.archived=0 AND r.state IN (?,?) AND s.created_by LIKE '%"kind":"session"%' ORDER BY s.session_id LIMIT 500`,
		string(contracts.RunExited), string(contracts.RunFailed))
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	byNode := map[string][]string{}
	var done []string
	for _, id := range ids {
		s, err := h.getSession(ctx, h.db, id)
		if err != nil || s.Archived || hasArchiveWhenDone(s) || s.Parent() == "" || s.CreatedBy.Kind != contracts.PrincipalSession {
			continue
		}
		r, err := h.getRun(ctx, h.db, s.CurrentRunID)
		if err != nil || !r.State.Terminal() {
			continue
		}
		grace, why := childArchiveGrace, "it exited cleanly and its parent has its final report"
		if !cleanExit(r) {
			grace, why = childEndedArchiveGrace, "exited child ("+endedHow(r)+"), report delivered, untouched for an hour"
		}
		if r.EndedAt == nil || h.now().Sub(*r.EndedAt) < grace {
			continue // no recorded end, or too recent
		}
		if h.ownerTouchedSince(ctx, s, r.RunID, *r.EndedAt) {
			continue // the owner engaged with it after it ended: his to archive
		}
		var pending, planned int
		// A folded report (a clean exit the parent already has) waits up to
		// childReportHold to ride along with the parent's next report; it is
		// settled as far as the parent is concerned.
		_ = h.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM child_reports WHERE child_session_id=? AND status=? AND hold_state!=?`, id, reportQueued, holdFolded).Scan(&pending)
		if pending > 0 {
			continue // the parent has not been told yet (held while it is not running)
		}
		_ = h.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM fallback_plans WHERE session_id=? AND from_run_id=?`, id, r.RunID).Scan(&planned)
		if planned > 0 {
			continue // a successor run was planned (account switch or move): not finished
		}
		ok, err := h.archiveEnded(ctx, s, why, func(cur contracts.Session) bool {
			return cur.CurrentRunID == r.RunID
		})
		if err != nil || !ok {
			continue
		}
		done = append(done, id)
		byNode[r.NodeID] = append(byNode[r.NodeID], id)
	}
	for node, ids := range byNode {
		h.requestCleanup(node, ids)
	}
	return done, nil
}

// endedHow describes a run end that was not clean, for the audit reason.
func endedHow(r contracts.Run) string {
	switch {
	case r.State == contracts.RunFailed:
		return "failed"
	case r.ExitCode != nil:
		return fmt.Sprintf("exit %d", *r.ExitCode)
	}
	return "exit not observed"
}

// ownerTouchedSince reports whether the owner messaged, typed into
// (Session.OwnerActiveAt) or attached to the session's run after since.
func (h *Hub) ownerTouchedSince(ctx context.Context, s contracts.Session, runID string, since time.Time) bool {
	if s.OwnerActiveAt != nil && s.OwnerActiveAt.After(since) {
		return true
	}
	var n int
	_ = h.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM commands WHERE ref_kind=? AND ref_id=? AND kind IN (?,?) AND issued_at>? AND actor LIKE '%"kind":"owner"%'`,
		string(contracts.KindRun), runID, string(contracts.CmdAttach), string(contracts.CmdTerminalOpen), persistence.Time(since)).Scan(&n)
	return n > 0
}

// cleanExit: the run exited (not failed) with a recorded exit code of 0. A
// missing code is what a reboot, an OOM kill or a pyred restart leaves, so
// it never counts as clean.
func cleanExit(r contracts.Run) bool {
	return r.State == contracts.RunExited && r.ExitCode != nil && *r.ExitCode == 0
}

// archiveEnded archives s as Pyre (audited with why) if still(current)
// holds inside the transaction, and revokes its agent tokens.
func (h *Hub) archiveEnded(ctx context.Context, s contracts.Session, why string, still func(contracts.Session) bool) (bool, error) {
	return h.archiveAsPyre(ctx, s, "archived automatically: "+why, still)
}

// archiveAsPyre is archiveEnded with the whole audit reason.
func (h *Hub) archiveAsPyre(ctx context.Context, s contracts.Session, reason string, still func(contracts.Session) bool) (bool, error) {
	done := false
	err := h.tx(ctx, func(tx *sql.Tx) error {
		cur, err := h.getSession(ctx, tx, s.SessionID)
		if err != nil || cur.Archived || (still != nil && !still(cur)) {
			return err
		}
		node := ""
		if r, err := h.getRun(ctx, tx, cur.CurrentRunID); err == nil {
			node = r.NodeID
		}
		cur.Archived = true
		cur.Tags = withoutTag(cur.Tags, contracts.TagArchiveWhenDone)
		if err := h.updateSessionTx(ctx, tx, cur); err != nil {
			return err
		}
		if err := h.emitSession(ctx, tx, contracts.EvSessionUpdated, cur, "hub", 0); err != nil {
			return err
		}
		h.auditTx(ctx, tx, auditRec{Action: contracts.AuditSessionArchived, Automatic: true, Actor: pyreActor, NodeID: node, SessionID: cur.SessionID,
			Reason: reason})
		done = true
		return nil
	})
	if err != nil || !done {
		return false, err
	}
	if h.cfg.Tokens != nil {
		_ = h.cfg.Tokens.RevokeSessionTokens(ctx, s.SessionID)
	}
	return true, nil
}

// requestCleanup asks nodeID to clean up after sessions (session.cleanup)
// and audits what it did once it answers. Best effort, in the background.
func (h *Hub) requestCleanup(nodeID string, sessionIDs []string) {
	if nodeID == "" || len(sessionIDs) == 0 {
		return
	}
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), cleanupWait+time.Minute)
		defer cancel()
		go func() {
			select {
			case <-h.stop:
				cancel()
			case <-ctx.Done():
			}
		}()
		var res contracts.SessionCleanupResult
		if err := h.nodeActionAsSystem(ctx, nodeID, contracts.ActionSessionCleanup, contracts.SessionCleanupParams{SessionIDs: sessionIDs}, cleanupWait, &res); err != nil {
			return
		}
		h.auditCleanups(ctx, nodeID, res.Sessions)
	}()
}

func (h *Hub) auditCleanups(ctx context.Context, nodeID string, cs []contracts.SessionCleanup) {
	_ = h.tx(ctx, func(tx *sql.Tx) error {
		for _, c := range cs {
			if c.Error == "" && c.ProcessesStopped == 0 && len(c.Worktrees) == 0 {
				continue
			}
			h.auditTx(ctx, tx, auditRec{Action: contracts.AuditSessionCleanedUp, Automatic: true, Actor: pyreActor, NodeID: nodeID, SessionID: c.SessionID,
				Reason: cleanupLine(c)})
		}
		return nil
	})
}

// cleanupLine summarises one session's cleanup.
func cleanupLine(c contracts.SessionCleanup) string {
	if c.Error != "" {
		return "not cleaned up: " + c.Error
	}
	var parts []string
	if c.ProcessesStopped > 0 {
		parts = append(parts, plural(c.ProcessesStopped, "leftover process", "leftover processes")+" stopped")
	}
	removed, kept := 0, []string{}
	for _, w := range c.Worktrees {
		if w.Outcome == "removed" {
			removed++
		} else {
			kept = append(kept, w.Path+" ("+keptWhy(w)+")")
		}
	}
	if removed > 0 {
		parts = append(parts, plural(removed, "worktree", "worktrees")+" removed")
	}
	if len(kept) > 0 {
		parts = append(parts, "kept "+strings.Join(kept, ", "))
	}
	return strings.Join(parts, "; ")
}

// nodeActionAsSystem queues a node action as the hub itself and waits up to
// wait for its final result, decoded into out.
func (h *Hub) nodeActionAsSystem(ctx context.Context, nodeID, action string, params any, wait time.Duration, out any) error {
	sys := contracts.Principal{Kind: contracts.PrincipalSystem, ID: "hub"}
	b, _ := json.Marshal(params)
	req := contracts.NodeActionRequest{NodeID: nodeID, Action: action, Params: contracts.RawJSON(b)}
	var cmdID string
	err := h.tx(ctx, func(tx *sql.Tx) error {
		cmd, err := h.enqueueTx(ctx, tx, nodeID, contracts.CmdAction, sys, req, h.opt.ActionTTL, contracts.ObjectRef{Kind: contracts.KindNode, ID: nodeID})
		cmdID = cmd.CommandID
		return err
	})
	if err != nil {
		return err
	}
	h.flushNode(ctx, nodeID)
	rec, err := h.waitCommand(ctx, cmdID, wait)
	if err != nil {
		return err
	}
	if !rec.finished() {
		return contracts.Errorf(contracts.ErrUnavailable, "node did not finish %s within %s", action, wait)
	}
	if rec.Error != nil {
		return contracts.Errorf(rec.Error.Code, "%s", rec.Error.Message)
	}
	if rec.State == cmdExpired || rec.State == cmdCancelled {
		return commandError(rec)
	}
	if out != nil && len(rec.Result) > 0 {
		return json.Unmarshal(rec.Result, out)
	}
	return nil
}

// ---- nightly sweep ----

// sweepDue reports (and records) that tonight's sweep should start: it is
// inside the sweep window in the owner's zone and no sweep ran today.
// dry: the dry-run report has not reached the owner yet (this sweep is
// one).
func (h *Hub) sweepDue(ctx context.Context) (due, dry bool) {
	if h.life.sweeping.Load() {
		return false, false
	}
	loc := pacific
	if p := h.askQuietPrefs(ctx); p.TZ != "" {
		if l, err := time.LoadLocation(p.TZ); err == nil {
			loc = l
		}
	}
	lt := h.now().In(loc)
	if tod := time.Duration(lt.Hour())*time.Hour + time.Duration(lt.Minute())*time.Minute; tod < sweepWindowStart || tod >= sweepWindowEnd {
		return false, false
	}
	day := lt.Format("2006-01-02")
	h.life.mu.Lock()
	defer h.life.mu.Unlock()
	var last string
	_ = h.db.QueryRowContext(ctx, `SELECT value FROM hub_meta WHERE key=?`, sweepMetaKey).Scan(&last)
	if last == day {
		return false, false
	}
	_, err := h.db.ExecContext(ctx, `INSERT INTO hub_meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, sweepMetaKey, day)
	var reported string
	_ = h.db.QueryRowContext(ctx, `SELECT value FROM hub_meta WHERE key=?`, dryRunMetaKey).Scan(&reported)
	return err == nil, reported == ""
}

// SweepReport is what one nightly sweep did (or, DryRun, would do).
type SweepReport struct {
	DryRun   bool
	Archived []SweptSession
	Nodes    []NodeSweep
	Text     string // the owner notice ("" when nothing happened)
}

// SweptSession is a session the sweep archived.
type SweptSession struct {
	SessionID, Title, NodeID string
	LaunchFailed             bool
}

// NodeSweep is one node's part.
type NodeSweep struct {
	NodeID, Name string
	Result       *contracts.NodeSweepResult
	Error        string
}

// RunNightlySweep archives sessions that failed to launch (over
// launchFailedAfter ago) or ended over staleAfter ago, has every connected
// node sweep, and sends the owner one notice. A dry run changes nothing
// and reports what would happen. Safe to call any time (one sweep at a
// time).
func (h *Hub) RunNightlySweep(ctx context.Context, dryRun bool) (SweepReport, error) {
	rep := SweepReport{DryRun: dryRun}
	if !h.life.sweeping.CompareAndSwap(false, true) {
		return rep, contracts.Errorf(contracts.ErrConflict, "a sweep is already running")
	}
	defer h.life.sweeping.Store(false)
	cands, err := h.staleSessions(ctx)
	if err != nil {
		return rep, err
	}
	byNode := map[string][]string{}
	for _, c := range cands {
		why := "it ended over a day ago (nightly cleanup)"
		if c.LaunchFailed {
			why = "it failed to launch (nightly cleanup)"
		}
		if dryRun {
			rep.Archived = append(rep.Archived, c)
			continue
		}
		s, err := h.getSession(ctx, h.db, c.SessionID)
		if err != nil {
			continue
		}
		run := s.CurrentRunID
		ok, err := h.archiveEnded(ctx, s, why, func(cur contracts.Session) bool { return cur.CurrentRunID == run && !h.sessionLive(ctx, cur) })
		if err != nil || !ok {
			continue
		}
		rep.Archived = append(rep.Archived, c)
		if c.NodeID != "" {
			byNode[c.NodeID] = append(byNode[c.NodeID], c.SessionID)
		}
	}
	nodes, err := h.sweepNodes(ctx)
	if err != nil {
		return rep, err
	}
	keep, err := h.unarchivedSessions(ctx)
	if err != nil {
		return rep, err
	}
	for id := range byNode {
		if _, ok := nodes[id]; !ok {
			nodes[id] = false
		}
	}
	ids := make([]string, 0, len(nodes))
	for id := range nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	rep.Nodes = make([]NodeSweep, len(ids))
	var wg sync.WaitGroup
	for i, id := range ids {
		rep.Nodes[i] = NodeSweep{NodeID: id, Name: h.nodeName(ctx, h.db, id, true)}
		if !nodes[id] {
			rep.Nodes[i].Error = "offline"
			continue
		}
		wg.Add(1)
		go func(ns *NodeSweep) {
			defer wg.Done()
			var res contracts.NodeSweepResult
			if err := h.nodeActionAsSystem(ctx, ns.NodeID, contracts.ActionNodeSweep, contracts.NodeSweepParams{SessionIDs: byNode[ns.NodeID], KeepSessions: keep, DryRun: dryRun}, sweepNodeWait, &res); err != nil {
				ns.Error = err.Error()
				return
			}
			ns.Result = &res
			h.auditCleanups(ctx, ns.NodeID, res.Sessions)
		}(&rep.Nodes[i])
	}
	wg.Wait()
	rep.Text = renderSweep(rep)
	if rep.Text == "" && dryRun {
		rep.Text = "[pyre cleanup] Nightly cleanup (dry run): nothing to archive or remove tonight. From the next night Pyre cleans up for real. No reply needed."
	}
	if rep.Text != "" {
		// A report of what was tidied is an update (stored, not in the
		// inbox); one where part of the cleanup failed needs the owner, and
		// so does the dry run: it is his preview before cleanup runs for real.
		mode := contracts.ModeInfo
		if dryRun || sweepNeedsOwner(rep) {
			mode = contracts.ModeQueue
		}
		_, err = h.msgs.SendMessage(contracts.WithPrincipal(ctx, cleanupSender), contracts.SendMessageRequest{
			Mutation:  contracts.Mutation{OperationID: "op_cleanup-" + contracts.NewID("sweep")},
			Recipient: contracts.Address{Kind: contracts.AddrOwner, ID: "owner"},
			Mode:      mode,
			Body:      contracts.MessageBody{Text: rep.Text},
		})
		if err == nil && dryRun {
			_, err = h.db.ExecContext(ctx, `INSERT INTO hub_meta(key,value) VALUES(?,?) ON CONFLICT(key) DO NOTHING`, dryRunMetaKey, persistence.Time(h.now()))
		}
	}
	return rep, err
}

// staleSessions lists unarchived sessions the sweep archives: no live run,
// and the last run failed to launch over launchFailedAfter ago or ended
// over staleAfter ago (a session that never got a run: created that long
// ago). Sessions scheduled to archive when done, sessions with a live
// child (a monitor does not count) and children whose report still waits
// for the parent are left alone.
func (h *Hub) staleSessions(ctx context.Context) ([]SweptSession, error) {
	rows, err := h.db.QueryContext(ctx, `SELECT session_id FROM sessions WHERE archived=0`)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	now := h.now()
	liveParents := map[string]bool{}
	var out []SweptSession
	var maybe []contracts.Session
	for _, id := range ids {
		s, err := h.getSession(ctx, h.db, id)
		if err != nil || hasArchiveWhenDone(s) {
			continue
		}
		if h.sessionLive(ctx, s) {
			// A live monitor does not keep its ended parent: the parent's
			// archive ends the monitor (contracts/monitors.go).
			if p := s.Parent(); p != "" && !s.IsMonitor() {
				liveParents[p] = true
			}
			continue
		}
		maybe = append(maybe, s)
	}
	for _, s := range maybe {
		if liveParents[s.SessionID] {
			continue
		}
		var pending int
		_ = h.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM child_reports WHERE child_session_id=? AND status=?`, s.SessionID, reportQueued).Scan(&pending)
		if pending > 0 {
			continue // its report still waits for the parent; archived once it is settled
		}
		c := SweptSession{SessionID: s.SessionID, Title: s.Title}
		if s.CurrentRunID == "" {
			if now.Sub(s.CreatedAt) < staleAfter {
				continue
			}
			out = append(out, c)
			continue
		}
		r, err := h.getRun(ctx, h.db, s.CurrentRunID)
		if err != nil {
			continue
		}
		c.NodeID = r.NodeID
		ended := r.StartedAt
		if r.EndedAt != nil {
			ended = *r.EndedAt
		}
		c.LaunchFailed = r.State == contracts.RunFailed && isLaunchFailure(r.StateReason)
		switch {
		case c.LaunchFailed && now.Sub(ended) >= launchFailedAfter:
		case (r.State.Terminal() || runNoHandle(r)) && now.Sub(ended) >= staleAfter:
		default:
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

// unarchivedSessions lists every session not archived (after this sweep's
// archives): their worktrees are never orphans.
func (h *Hub) unarchivedSessions(ctx context.Context) ([]string, error) {
	rows, err := h.db.QueryContext(ctx, `SELECT session_id FROM sessions WHERE archived=0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			out = append(out, id)
		}
	}
	return out, rows.Err()
}

// sweepNodes maps every enrolled node to whether it is connected.
func (h *Hub) sweepNodes(ctx context.Context) (map[string]bool, error) {
	rows, err := h.db.QueryContext(ctx, `SELECT node_id, connected FROM node_state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		var c int
		if rows.Scan(&id, &c) == nil {
			out[id] = c == 1
		}
	}
	return out, rows.Err()
}

// renderSweep writes the owner's notice: per machine what was archived,
// stopped, removed and kept (with why), and how to undo an archive.
func renderSweep(rep SweepReport) string {
	archivedOn := map[string][]SweptSession{}
	for _, a := range rep.Archived {
		archivedOn[a.NodeID] = append(archivedOn[a.NodeID], a)
	}
	var b strings.Builder
	total := 0
	would := func(done, planned string) string {
		if rep.DryRun {
			return planned
		}
		return done
	}
	writeNode := func(name string, arch []SweptSession, ns *NodeSweep) {
		var lines []string
		if len(arch) > 0 {
			failed := 0
			for _, a := range arch {
				if a.LaunchFailed {
					failed++
				}
			}
			l := would("archived ", "would archive ") + plural(len(arch), "session", "sessions")
			switch {
			case failed == len(arch):
				l += " that failed to launch"
			case failed > 0:
				l += fmt.Sprintf(" (%d ended over a day ago, %d failed to launch)", len(arch)-failed, failed)
			default:
				l += " that ended over a day ago"
			}
			lines = append(lines, l+": "+sessionList(arch, 6))
		}
		if ns != nil && ns.Result != nil {
			procs := 0
			removed := 0
			var kept []string
			for _, s := range ns.Result.Sessions {
				procs += s.ProcessesStopped
				for _, w := range s.Worktrees {
					if w.Outcome == "removed" || w.Outcome == "would_remove" {
						removed++
					} else if w.Outcome != "missing" {
						kept = append(kept, w.Path+" ("+keptWhy(w)+")")
					}
				}
			}
			for _, w := range ns.Result.Worktrees {
				if w.Outcome == "removed" || w.Outcome == "would_remove" {
					removed++
				} else {
					kept = append(kept, w.Path+" ("+keptWhy(w)+")")
				}
			}
			if procs > 0 {
				lines = append(lines, "stopped "+plural(procs, "leftover process", "leftover processes"))
			}
			if removed > 0 {
				lines = append(lines, would("removed ", "would remove ")+plural(removed, "worktree", "worktrees")+" Pyre created for finished sessions (clean, branch merged or pushed)")
			}
			if ns.Result.TmpDirsRemoved > 0 {
				lines = append(lines, fmt.Sprintf(would("deleted", "would delete")+" %s of old temp files (%s)", humanBytes(ns.Result.TmpBytesFreed), plural(ns.Result.TmpDirsRemoved, "session folder", "session folders")))
			}
			if len(kept) > 0 {
				const maxKept = 8
				l := "kept " + plural(len(kept), "worktree", "worktrees") + ": "
				if len(kept) > maxKept {
					l += strings.Join(kept[:maxKept], "; ") + fmt.Sprintf("; and %d more", len(kept)-maxKept)
				} else {
					l += strings.Join(kept, "; ")
				}
				lines = append(lines, l)
			}
		}
		if ns != nil && ns.Error != "" && (len(lines) > 0 || ns.Error != "offline") {
			if ns.Error == "offline" {
				lines = append(lines, "machine offline: its worktrees and processes were not checked")
			} else {
				lines = append(lines, "machine cleanup failed: "+oneLineText(ns.Error, 200))
			}
		}
		if len(lines) == 0 {
			return
		}
		total++
		fmt.Fprintf(&b, "\n%s:\n", name)
		for _, l := range lines {
			b.WriteString("- " + l + "\n")
		}
	}
	for i := range rep.Nodes {
		ns := &rep.Nodes[i]
		name := ns.Name
		if name == "" {
			name = ns.NodeID
		}
		writeNode(name, archivedOn[ns.NodeID], ns)
	}
	if none := archivedOn[""]; len(none) > 0 {
		writeNode("Sessions that never ran", none, nil)
	}
	if total == 0 {
		return ""
	}
	if rep.DryRun {
		return "[pyre cleanup] Nightly cleanup (dry run: nothing was changed; from the next night Pyre does this for real)\n" + strings.TrimRight(b.String(), "\n") +
			"\nNo reply needed."
	}
	return "[pyre cleanup] Nightly cleanup\n" + strings.TrimRight(b.String(), "\n") +
		"\nArchived sessions keep their history and can be resumed or unarchived (pyre sessions unarchive <session>). No reply needed."
}

// sweepNeedsOwner reports whether part of the sweep failed (a machine's
// cleanup, a session's, or a worktree removal); a machine that was offline
// is not a failure.
func sweepNeedsOwner(rep SweepReport) bool {
	failed := func(ws []contracts.WorktreeRemoval) bool {
		for _, w := range ws {
			if w.Outcome == "failed" {
				return true
			}
		}
		return false
	}
	for _, ns := range rep.Nodes {
		if ns.Error != "" && ns.Error != "offline" {
			return true
		}
		if ns.Result == nil {
			continue
		}
		if len(ns.Result.Errors) > 0 || failed(ns.Result.Worktrees) {
			return true
		}
		for _, sc := range ns.Result.Sessions {
			if sc.Error != "" || failed(sc.Worktrees) {
				return true
			}
		}
	}
	return false
}

func sessionList(ss []SweptSession, max int) string {
	parts := make([]string, 0, max+1)
	for i, s := range ss {
		if i == max {
			parts = append(parts, fmt.Sprintf("and %d more", len(ss)-max))
			break
		}
		t := oneLineText(s.Title, 50)
		if t == "" {
			t = s.SessionID
		}
		parts = append(parts, fmt.Sprintf("%q", t))
	}
	return strings.Join(parts, ", ")
}

func keptWhy(w contracts.WorktreeRemoval) string {
	switch w.Outcome {
	case "kept_dirty":
		return "uncommitted changes"
	case "kept_unmerged":
		if w.Branch != "" {
			return "branch " + w.Branch + " not merged or pushed"
		}
		return "commit not on any remote branch"
	case "kept_in_use":
		return "in use: " + w.Detail
	case "failed":
		return "removal failed: " + oneLineText(w.Detail, 120)
	}
	if w.Detail != "" {
		return w.Detail
	}
	return w.Outcome
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.0f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d bytes", n)
}
