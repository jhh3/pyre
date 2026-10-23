package daemon

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/jhh3/pyre/internal/client"
	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/resumenote"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

// Crash resume (Oct 20 2026: server's systemd user manager was SIGKILLed; tmux
// and every agent died with it, and only the Codex runs came back because
// codex Recover restarts its own app-server).
//
// A graceful pyred restart or upgrade leaves runs alive, so a run whose
// process is gone when pyred starts died underneath pyred. recoverRuns
// still ends such a run ("exit not observed"); when it was a live session
// (running or waiting) of a harness that resumes natively and Codex does
// not already restart, pyred then starts a native-resume continuation
// itself, exactly as an account fallback does (StartContinuation: a
// node-local CmdResume, so the hub sees an ordinary resume through the run
// reports), and tells the resumed session what happened with a durable
// message (resumenote, stable ID per session and new run; held in the node
// outbox while the hub is unreachable).
//
// Native resume never replays the interrupted instruction, and there is
// no handoff fallback (NativeOnly). Each run is resumed at most once: the
// run pyred starts carries adapterDataCrashResumed, and a run that dies
// again within crashResumeLoop of being crash-resumed is left ended (a
// crash loop is not resumed forever). A run whose stop was requested
// (adapterDataStopRequested) is never resumed. Sessions without a parent
// are resumed first, then their children, one launch at a time.

const (
	crashResumeActor         = "crash-resume"
	adapterDataCrashResumed  = "pyre.crash_resumed_at"
	adapterDataStopRequested = "pyre.stop_requested"
	crashResumeLoop          = 10 * time.Minute
	// metaAliveAt is when this pyred last stamped itself alive (best
	// estimate of when processes that died with it died).
	metaAliveAt = "alive_at"
)

// Tunables (tests shorten them).
var (
	crashResumeGap      = 1500 * time.Millisecond // between launches
	crashResumeLiveWait = 3 * time.Minute         // for a resumed run to come up before the note
	aliveStampEvery     = 30 * time.Second
	crashHubWait        = 10 * time.Minute // for the hub to answer before a session is left ended
	crashHubRetry       = 5 * time.Second
	crashHubTimeout     = 10 * time.Second // one hub check
	crashStopWait       = 30 * time.Minute // for a resumed run to come up so a stop that arrived for it applies
)

// crashResumeLive are the states of a live session: what a run must have
// been in to be resumed, and what the resumed run must reach to be noted.
var crashResumeLive = []contracts.RunState{contracts.RunRunning, contracts.RunWaitingInput, contracts.RunWaitingBackground, contracts.RunWaitingApproval}

// crashResumeCandidate reports whether a run recovery could not find
// should be resumed, or why not.
func (d *Daemon) crashResumeCandidate(rec runRecord) (bool, string) {
	r := rec.Run
	if !slices.Contains(crashResumeLive, r.State) {
		return false, "last state " + string(r.State)
	}
	switch r.Harness {
	case contracts.HarnessShell:
		return false, "shell sessions do not resume"
	case contracts.HarnessCodex:
		return false, "codex recovery restarts its own runs; not found means stopped or thread gone"
	}
	if !contracts.HasCap(r.Capabilities, contracts.CapResumeNative) {
		return false, "harness does not resume natively"
	}
	if r.AdapterData[adapterDataMode] == modeHeadless {
		return false, "headless run"
	}
	if r.AdapterData[adapterDataStopRequested] != "" {
		return false, "stop was requested"
	}
	if rec.Session.Archived {
		return false, "session archived"
	}
	if at, err := sqlitex.ParseTime(r.AdapterData[adapterDataCrashResumed]); err == nil && !at.IsZero() && d.now().Sub(at) < crashResumeLoop {
		return false, "already crash-resumed at " + at.Format(time.RFC3339) + " (crash loop)"
	}
	return true, ""
}

// markStopRequested records durably that a stop was asked for, so a crash
// before the exit is observed does not resume the run. It reports whether
// it set the marker (false: already set).
func (d *Daemon) markStopRequested(ctx context.Context, lr *liveRun, by string) bool {
	return d.setStopRequested(ctx, lr, ftime(d.now())+" "+by)
}

// clearStopRequested undoes markStopRequested when the stop failed or was
// refused: the run keeps going, and a later crash may resume it.
func (d *Daemon) clearStopRequested(ctx context.Context, lr *liveRun) {
	d.setStopRequested(ctx, lr, "")
}

func (d *Daemon) setStopRequested(ctx context.Context, lr *liveRun, v string) bool {
	lr.mu.Lock()
	if (lr.rec.Run.AdapterData[adapterDataStopRequested] != "") == (v != "") {
		lr.mu.Unlock()
		return false
	}
	ad := make(map[string]string, len(lr.rec.Run.AdapterData)+1)
	for k, val := range lr.rec.Run.AdapterData {
		ad[k] = val
	}
	if v == "" {
		delete(ad, adapterDataStopRequested)
	} else {
		ad[adapterDataStopRequested] = v
	}
	lr.rec.Run.AdapterData = ad
	rec := lr.rec
	lr.mu.Unlock()
	if err := sqlitex.Tx(ctx, d.db, func(tx *sql.Tx) error { return putRunTx(ctx, tx, rec, d.now()) }); err != nil {
		d.noteErr("record stop request for run %s: %v", rec.Run.RunID, err)
	}
	return true
}

// aliveLoop stamps metaAliveAt so the next start knows roughly when this
// pyred (and whatever died with it) was last alive.
func (d *Daemon) aliveLoop(ctx context.Context) {
	t := time.NewTicker(aliveStampEvery)
	defer t.Stop()
	for {
		if err := d.st.setMeta(ctx, metaAliveAt, ftime(d.now())); err != nil && ctx.Err() == nil {
			d.noteErr("alive stamp: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// diedAt estimates when a run found gone after a restart died: the later
// of its last recorded observation and pyred's last alive stamp.
func diedAt(rec runRecord, aliveAt time.Time) time.Time {
	t := rec.Run.Freshness.ObservedAt
	if aliveAt.After(t) {
		t = aliveAt
	}
	return t
}

// crashResumeOrder sorts candidates parents first: by depth within the
// candidate set (a session whose parent is not being resumed is a root),
// then by session ID for a stable order.
func crashResumeOrder(recs []runRecord) {
	parent := map[string]string{}
	for _, r := range recs {
		parent[r.Session.SessionID] = r.Session.Parent()
	}
	depth := func(sid string) int {
		n := 0
		for p := parent[sid]; p != "" && n < len(recs); p = parent[p] {
			if _, ok := parent[p]; !ok {
				break
			}
			n++
		}
		return n
	}
	sort.SliceStable(recs, func(i, j int) bool {
		di, dj := depth(recs[i].Session.SessionID), depth(recs[j].Session.SessionID)
		if di != dj {
			return di < dj
		}
		return recs[i].Session.SessionID < recs[j].Session.SessionID
	})
}

// crashResumePolicy keeps the previous run's account (falling back to the
// best eligible one when it is unusable), or its ambient login.
func crashResumePolicy(r contracts.Run) contracts.AccountPolicy {
	switch {
	case contracts.ProviderFor(r.Harness) == "":
		return contracts.AccountPolicy{Mode: contracts.AccountEligible}
	case r.AccountID != "":
		return contracts.AccountPolicy{Mode: contracts.AccountExplicit, AccountID: r.AccountID}
	case r.AdapterData["pyre.account"] == string(contracts.AccountAmbient):
		return contracts.AccountPolicy{Mode: contracts.AccountAmbient}
	}
	return contracts.AccountPolicy{Mode: contracts.AccountEligible}
}

// crashSet tracks this start's crash-resume candidates by dead run ID. A
// stop the hub queued for a dead run while the node was down (it arrives
// once the link is up, aimed at the run the hub still thought current)
// cancels that run's resume, or stops the run that replaced it.
type crashSet struct {
	mu   sync.Mutex
	runs map[string]*crashEntry
	kick chan struct{} // the hub link came up: retry hub checks now
}

type crashEntry struct {
	launching bool   // StartContinuation is under way
	newRun    string // the continuation, once launched
	stopped   bool   // a stop arrived for the dead run
}

func (c *crashSet) add(runID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.runs == nil {
		c.runs = map[string]*crashEntry{}
	}
	c.runs[runID] = &crashEntry{}
}

func (c *crashSet) stopped(runID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.runs[runID]
	return e != nil && e.stopped
}

// launching records that the continuation of runID is being started.
func (c *crashSet) launching(runID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e := c.runs[runID]; e != nil {
		e.launching = true
	}
}

// launched records the continuation of runID and reports whether a stop
// for runID arrived meanwhile.
func (c *crashSet) launched(runID, newRun string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.runs[runID]
	if e == nil {
		return false
	}
	e.newRun = newRun
	return e.stopped
}

// stop marks runID stopped; ok=false when runID is not a crash-resume
// candidate of this start. resuming: a continuation is launched or being
// started (crashResumeFollow stops it once it is up).
func (c *crashSet) stop(runID string) (newRun string, resuming, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.runs[runID]
	if e == nil {
		return "", false, false
	}
	e.stopped = true
	return e.newRun, e.launching || e.newRun != "", true
}

func (c *crashSet) wake() {
	c.mu.Lock()
	k := c.kick
	c.mu.Unlock()
	if k != nil {
		select {
		case k <- struct{}{}:
		default:
		}
	}
}

func (c *crashSet) kicks() chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.kick == nil {
		c.kick = make(chan struct{}, 1)
	}
	return c.kick
}

// crashStop handles a stop for a run in the crash-resume set: before the
// resume it cancels it; after, it stops the run that replaced it (now, when
// that run is under control, else once it is up: crashResumeFollow).
// handled=false: not a crash-resume run, stop as usual.
func (d *Daemon) crashStop(ctx context.Context, cmd contracts.NodeCommand, rc contracts.RunCommand) (contracts.CommandResult, bool, error) {
	newRun, resuming, ok := d.crash.stop(rc.RunID)
	if !ok {
		return contracts.CommandResult{}, false, nil
	}
	d.cfg.Logger.Info("crash resume: stop received for a dead run", "run", rc.RunID, "session", rc.SessionID, "resumed_as", newRun)
	if newRun != "" {
		if lr, live := d.getLive(newRun); live && lr.recovered {
			rc.RunID = newRun
			res, err := d.runControl(ctx, cmd, rc)
			return res, true, err
		}
	}
	detail := "run had already ended (its process died before pyred restarted); its crash resume is cancelled"
	if resuming {
		detail = "run had already ended; the run that resumes it " + newRun + " is stopped as soon as it is up"
	}
	res, err := okResult(contracts.OutcomeSucceeded, contracts.CommandReceipt{CommandID: cmd.CommandID, Accepted: true,
		Outcome: contracts.OutcomeSucceeded, Detail: detail, ObservedAt: d.now()})
	return res, true, err
}

// crashResumeAll resumes the candidates recoverRuns found, in order, and
// follows each new run (note, or a stop that arrived meanwhile). With a
// hub configured, every session is checked with the hub first; while the
// hub cannot be reached its sessions wait (retried every crashHubRetry and
// when the link comes up) for up to crashHubWait, then are left ended:
// never resumed blind, since an archive, a stop or a resume elsewhere may
// have happened meanwhile. A child waits while its parent does.
func (d *Daemon) crashResumeAll(ctx context.Context, recs []runRecord, aliveAt time.Time) {
	crashResumeOrder(recs) // registered in d.crash by recoverRuns
	kick := d.crash.kicks()
	deadline := time.Now().Add(crashHubWait)
	launched := 0
	pending := recs
	for len(pending) > 0 {
		var retry []runRecord
		waiting := map[string]bool{}
		for _, rec := range pending {
			if ctx.Err() != nil || d.isDraining() {
				return
			}
			log := d.cfg.Logger.With("session", rec.Run.SessionID, "run", rec.Run.RunID)
			if d.crash.stopped(rec.Run.RunID) {
				log.Info("crash resume: skipped", "reason", "a stop for the run arrived after the restart")
				continue
			}
			if p := rec.Session.Parent(); p != "" && waiting[p] {
				retry, waiting[rec.Session.SessionID] = append(retry, rec), true
				continue
			}
			hv, reachable := d.crashResumeHubView(ctx, rec)
			if !reachable {
				retry, waiting[rec.Session.SessionID] = append(retry, rec), true
				continue
			}
			if hv.skip != "" {
				log.Info("crash resume: skipped", "reason", hv.skip)
				continue
			}
			if launched > 0 {
				select {
				case <-ctx.Done():
					return
				case <-time.After(crashResumeGap):
				}
			}
			launched++
			d.crash.launching(rec.Run.RunID)
			policy := crashResumePolicy(rec.Run)
			newRun, _, err := d.StartContinuation(ctx, ContinuationRequest{SessionID: rec.Run.SessionID, PreviousRunID: rec.Run.RunID,
				Kind: contracts.ContinuationNativeResume, Reason: "pyred restarted after the run's process died", NativeOnly: true,
				Policy: &policy, Actor: crashResumeActor})
			if err != nil {
				// Left ended, as before crash resume existed; pyre node resume or
				// pyre resume can still bring it back.
				d.noteErr("crash resume of session %s (run %s) failed: %v", rec.Run.SessionID, rec.Run.RunID, err)
				continue
			}
			d.crash.launched(rec.Run.RunID, newRun)
			log.Info("crash resume: resumed natively", "new_run", newRun)
			rec, died, drained := rec, diedAt(rec, aliveAt), hv.drained
			d.asyncWG.Add(1)
			go func() {
				defer d.asyncWG.Done()
				d.crashResumeFollow(ctx, rec.Run.SessionID, rec.Run.RunID, newRun, died, drained)
			}()
		}
		if len(retry) == 0 {
			return
		}
		if time.Now().After(deadline) {
			for _, rec := range retry {
				d.noteErr("crash resume of session %s (run %s): hub unreachable for %s; left ended, not resumed without the hub's view", rec.Run.SessionID, rec.Run.RunID, crashHubWait)
			}
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-kick:
		case <-time.After(crashHubRetry):
		}
		pending = retry
	}
}

// crashHubView is what the hub says about a session pyred is about to
// crash-resume.
type crashHubView struct {
	skip    string // why not to resume ("" = resume)
	drained bool   // tagged drained:<this node> (a planned reboot)
}

// crashResumeHubView asks the hub, with the node's own credential (a
// revoked session token says nothing), whether the session moved on while
// this node was down: archived, or resumed elsewhere; and whether it was
// drained for a planned reboot. It fails closed: any answer but the
// session (not found, refused) skips the resume. reachable=false when the
// hub could not be asked (link down, unavailable, timeout): the caller
// retries. A node without a hub (standalone) has nothing to ask.
func (d *Daemon) crashResumeHubView(ctx context.Context, rec runRecord) (v crashHubView, reachable bool) {
	if d.cfg.Upstream == nil {
		return crashHubView{}, true
	}
	if d.cfg.NodeToken == "" {
		return crashHubView{skip: "no node credential to check the session with the hub"}, true
	}
	if d.hubLinkDown.Load() {
		return crashHubView{}, false
	}
	up, err := d.upstream(d.cfg.NodeToken)
	if err != nil {
		return crashHubView{}, false
	}
	gctx, cancel := context.WithTimeout(client.WithMaxAttempts(ctx, 1), crashHubTimeout)
	defer cancel()
	sv, err := up.GetSession(gctx, contracts.GetRequest{ID: rec.Run.SessionID})
	switch {
	case err == nil:
	case gctx.Err() != nil || crashHubRetryable(err):
		return crashHubView{}, false
	case contracts.IsCode(err, contracts.ErrUnauthorized) || contracts.IsCode(err, contracts.ErrForbidden):
		// The node's own credential refused: every crash resume on this node
		// is blocked until it is fixed, so say so loudly.
		d.noteErr("crash resume: the hub refused this node's credential checking session %s: %v; not resumed", rec.Run.SessionID, err)
		return crashHubView{skip: "hub refused the node credential"}, true
	default:
		return crashHubView{skip: "hub: " + redact(err.Error())}, true
	}
	// The tag pyre node drain sets (internal/cli/cmd_drain.go drainTag).
	v = crashHubView{drained: slices.Contains(sv.Session.Tags, "drained:"+d.nodeID)}
	switch {
	case sv.Session.Archived:
		v.skip = "session archived"
	case sv.Session.CurrentRunID != "" && sv.Session.CurrentRunID != rec.Run.RunID:
		v.skip = "session's current run is now " + sv.Session.CurrentRunID
	}
	return v, true
}

// crashHubRetryable: the hub could not answer now (as opposed to answering
// no). An error without a Pyre code is a transport failure.
func crashHubRetryable(err error) bool {
	var ce *contracts.Error
	if !errors.As(err, &ce) {
		return true
	}
	switch ce.Code {
	case contracts.ErrUnavailable, contracts.ErrCapacityExhausted, contracts.ErrInternal:
		return true
	}
	return false
}

// crashResumeFollow waits for the resumed run to come up. If a stop for
// the dead run arrived meanwhile it stops the new run; otherwise (unless
// the session was drained: pyre node resume sends the owner's note) it
// sends the session its note through the local API (hub, or the outbox
// while the hub is unreachable), as the session itself. The note is given
// up after crashResumeLiveWait; a stop (crashStop promised it) is kept
// until the run is up or ends, bounded by crashStopWait.
func (d *Daemon) crashResumeFollow(ctx context.Context, sessionID, oldRun, runID string, died time.Time, drained bool) {
	start := time.Now()
	noNote := false
	for {
		rec, ok, err := d.st.run(ctx, runID)
		if lr, live := d.getLive(runID); live {
			rec, _ = lr.snapshot()
			ok, err = true, nil
			if lr.recovered && slices.Contains(crashResumeLive, rec.Run.State) {
				break
			}
		}
		if err == nil && ok && rec.Run.State.Terminal() {
			if !d.crash.stopped(oldRun) && !noNote {
				d.noteErr("crash resume of session %s: run %s %s before it came up (%s); no note sent", sessionID, runID, rec.Run.State, rec.Run.StateReason)
			}
			return
		}
		waited := time.Since(start)
		if !noNote && waited > crashResumeLiveWait {
			noNote = true
			d.noteErr("crash resume of session %s: run %s not up after %s; no note sent", sessionID, runID, crashResumeLiveWait)
		}
		if waited > crashStopWait {
			if d.crash.stopped(oldRun) {
				d.noteErr("crash resume of session %s: a stop arrived for its dead run, but the resumed run %s did not come up within %s; stop it by hand", sessionID, runID, crashStopWait)
			}
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(250 * time.Millisecond):
		}
	}
	if d.crash.stopped(oldRun) {
		cmd := contracts.NodeCommand{CommandID: contracts.NewID(contracts.PrefixCommand), Kind: contracts.CmdStop,
			Actor: contracts.Principal{Kind: contracts.PrincipalSystem, ID: crashResumeActor}, IssuedAt: d.now()}
		if _, err := d.runControl(ctx, cmd, contracts.RunCommand{SessionID: sessionID, RunID: runID, Reason: "a stop for the run it resumed arrived"}); err != nil {
			d.noteErr("crash resume of session %s: stop of run %s (a stop for %s arrived): %v", sessionID, runID, oldRun, err)
			return
		}
		d.cfg.Logger.Info("crash resume: stopped the resumed run (a stop for the dead run arrived)", "session", sessionID, "run", runID)
		return
	}
	if noNote {
		return // reported above; a late note would contradict the error
	}
	if drained {
		d.cfg.Logger.Info("crash resume: session was drained for a reboot; the note is left to pyre node resume", "session", sessionID, "run", runID)
		return
	}
	tok, ok, err := d.st.tokenForSession(ctx, sessionID)
	if err != nil || !ok {
		d.cfg.Logger.Warn("crash resume: no session credential on this node; resumed without a note", "session", sessionID, "run", runID)
		return
	}
	self := contracts.Principal{Kind: contracts.PrincipalSession, ID: sessionID}
	sctx := WithBearer(contracts.WithPrincipal(ctx, self), tok)
	req := contracts.SendMessageRequest{Mutation: contracts.Mutation{OperationID: resumenote.OperationID(sessionID, runID)},
		MessageID: resumenote.MessageID(sessionID, runID), Recipient: contracts.Address{Kind: contracts.AddrSession, ID: sessionID},
		Mode: contracts.ModeQueue, Body: contracts.MessageBody{Text: resumenote.AfterCrash(died, d.now())}}
	if _, err := d.LocalAPI().SendMessage(sctx, req); err != nil {
		d.noteErr("crash resume note to session %s: %v", sessionID, err)
		return
	}
	d.cfg.Logger.Info("crash resume: note sent", "session", sessionID, "run", runID, "message", req.MessageID)
}
