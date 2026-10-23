package codex

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// Background-command wake (contracts.CapWakeBackground).
//
// Codex's unified exec (on by default since 0.16x) yields a long command
// back to the model after ~10 s and keeps it running; the model may end its
// turn while it runs. Codex reports the exit (item/completed for the
// commandExecution item, carrying the turn that started it) but never starts
// a turn for it, so the agent never learns the result. Verified live with
// codex 0.163.0 (docs/evidence/runtime/codex-bg-wake-2026-10-19.md):
// turn/completed at 7.3 s, the item/completed with exitCode and
// aggregatedOutput at 44.9 s, the thread idle throughout.
//
// Pyre provides the wake. A command that finishes after its own turn ended
// is queued on the thread; once the thread is idle (no active turn, no
// pending approval, no usage limit, no capacity continue due) and no
// further completion arrived for wakeSettle, one turn/start reports every
// queued completion. A completion during another turn waits for that turn
// to end (never a steer). The turn is a fresh notice, not a replay: a
// turn/start Codex refused is retried (wakeAttempts), one whose outcome is
// unknown is dropped. Not woken: stopped runs; the commands running when
// Pyre interrupts the thread (an interrupt, also of an idle thread, cleans
// its background terminals); sessions the daemon says are archived or
// being archived (Options.WakeAllowed). The queue lives in memory:
// completions while pyred is down are missed.

const (
	// wakeIDPrefix starts the clientUserMessageId of a wake turn (also how
	// the transcript tells it from typed input).
	wakeIDPrefix = "pyre-bgwake-"
	// wakeTailBytes is the output tail one completion reports; several
	// share wakeTotalBytes (at least wakeMinTailBytes each).
	wakeTailBytes    = 2048
	wakeTotalBytes   = 6144
	wakeMinTailBytes = 400
	// wakeMaxListed completions are described in one notice; the rest are
	// counted.
	wakeMaxListed = 8
	// wakeAttempts bounds turn/starts Codex refused for one completion.
	wakeAttempts = 3
	// wakeMuteAfterClean: completions this soon after Pyre cleaned the
	// thread's background terminals (interrupt) are the processes it killed.
	wakeMuteAfterClean = 5 * time.Second
	// wakeSeenMax bounds the item ids remembered against duplicates;
	// wakeQueueMax the completions held for a thread that stays busy
	// (oldest dropped).
	wakeSeenMax  = 512
	wakeQueueMax = 32
)

// wakeSettle is how long the queue must be quiet before the wake turn
// starts, so completions moments apart share one notice.
const wakeSettle = 1500 * time.Millisecond

func (a *Adapter) wakeDelay() time.Duration {
	if a.wakeSettle > 0 {
		return a.wakeSettle
	}
	return wakeSettle
}

// bgDone is one background command that finished after its turn ended.
type bgDone struct {
	ItemID, TurnID string
	Command, Cwd   string
	Status         string
	ExitCode       *int
	DurationMs     *int64
	Output         string
	At             time.Time
	attempts       int
}

// noteCommandStart tracks the commands running on the thread (so an
// interrupt can mute exactly those). Called with ts.mu held.
func (ts *threadState) noteCommandStart(it item) {
	if it.Type != "commandExecution" || it.ID == "" {
		return
	}
	if ts.runningCmds == nil || len(ts.runningCmds) >= wakeSeenMax {
		ts.runningCmds = map[string]bool{}
	}
	ts.runningCmds[it.ID] = true
}

// muteRunning drops the queue and ignores the exits of the commands
// running now (the owner interrupted the work). Called with ts.mu held.
func (ts *threadState) muteRunning() {
	ts.wakes = nil
	if ts.wakeIgnore == nil || len(ts.wakeIgnore)+len(ts.runningCmds) > wakeSeenMax {
		ts.wakeIgnore = map[string]bool{}
	}
	for id := range ts.runningCmds {
		ts.wakeIgnore[id] = true
	}
}

// noteCommandDone queues a finished commandExecution when the turn that
// started it is no longer running. Called with ts.mu held.
func (ts *threadState) noteCommandDone(it item, turnID string, at time.Time) bool {
	if it.Type != "commandExecution" || it.ID == "" {
		return false
	}
	delete(ts.runningCmds, it.ID)
	if ts.wakeIgnore[it.ID] {
		delete(ts.wakeIgnore, it.ID)
		return false
	}
	if ts.closing || at.Before(ts.wakeMute) {
		return false
	}
	if it.Status == "declined" || it.Status == "inProgress" {
		return false
	}
	if ts.status.Type == "active" && (turnID == "" || turnID == ts.activeTurn) {
		return false // the turn that started it is still running: it sees the exit itself
	}
	if ts.wakeSeen[it.ID] {
		return false
	}
	if ts.wakeSeen == nil || len(ts.wakeSeen) >= wakeSeenMax {
		ts.wakeSeen = map[string]bool{}
	}
	ts.wakeSeen[it.ID] = true
	cmd := it.Command
	if len(it.CommandActions) == 1 && strings.TrimSpace(it.CommandActions[0].Command) != "" {
		cmd = it.CommandActions[0].Command // without the `zsh -lc '…'` wrapper
	}
	ts.wakes = append(ts.wakes, bgDone{ItemID: it.ID, TurnID: turnID, Command: cmd, Cwd: it.Cwd, Status: it.Status,
		ExitCode: it.ExitCode, DurationMs: it.DurationMs, Output: lastBytes(it.AggregatedOutput, 4*wakeTailBytes), At: at})
	if n := len(ts.wakes); n > wakeQueueMax {
		ts.wakes = append([]bgDone(nil), ts.wakes[n-wakeQueueMax:]...)
	}
	return true
}

// dropTurnWakes forgets completions of a turn that has just ended: they
// were queued while that turn's id was unknown (after a pyred restart) and
// belonged to it after all. Called with ts.mu held.
func (ts *threadState) dropTurnWakes(turnID string) {
	if turnID == "" || len(ts.wakes) == 0 {
		return
	}
	kept := ts.wakes[:0]
	for _, w := range ts.wakes {
		if w.TurnID != turnID {
			kept = append(kept, w)
		}
	}
	ts.wakes = kept
}

// clearWakes drops the queue; with mute, completions are ignored until
// wakeMute (Pyre is killing the thread's background terminals).
func (ts *threadState) clearWakes(mute time.Time) {
	ts.wakes = nil
	if mute.After(ts.wakeMute) {
		ts.wakeMute = mute
	}
}

// wakeable: the thread would accept a new turn now, and no capacity
// continue is due (a wake turn's completion would cancel it; the queue
// waits for the continue instead). Called with ts.mu held.
func (ts *threadState) wakeable() bool {
	if ts.closing || ts.status.Type == "active" || ts.limit != nil || !ts.retryAt.IsZero() {
		return false
	}
	for _, pa := range ts.approvals {
		if pa.approval.State == contracts.ApprovalPending {
			return false
		}
	}
	return true
}

// armWake schedules a wake attempt after wakeSettle (once; a later
// completion inside the window joins the same notice). Called with ts.mu
// held; turn completion and Observe re-arm a queue that had to wait.
func (a *Adapter) armWake(ts *threadState) {
	if ts.wakeArmed || len(ts.wakes) == 0 || !ts.wakeable() {
		return
	}
	ts.wakeArmed = true
	time.AfterFunc(a.wakeDelay(), func() { a.wake(ts) })
}

// wake starts one turn reporting the queued completions, if the thread is
// still idle and the run may be woken.
func (a *Adapter) wake(ts *threadState) {
	ts.mu.Lock()
	ts.wakeArmed = false
	quiet := len(ts.wakes) > 0 && time.Since(ts.wakes[len(ts.wakes)-1].At) >= a.wakeDelay()
	if !quiet && len(ts.wakes) > 0 {
		a.armWake(ts) // a completion arrived inside the window: wait for quiet
	}
	ts.mu.Unlock()
	if !quiet {
		return
	}
	a.mu.Lock()
	rr := a.runs[ts.runID]
	a.mu.Unlock()
	if rr == nil || rr.ts != ts {
		return
	}
	if stopped(rr.stateDir) {
		ts.mu.Lock()
		ts.clearWakes(time.Time{})
		ts.mu.Unlock()
		return
	}
	if f := a.o.WakeAllowed; f != nil {
		cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		ok := f(cctx, rr.meta.SessionID)
		cancel()
		if !ok {
			ts.mu.Lock()
			ts.clearWakes(time.Time{})
			ts.mu.Unlock()
			return
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	rr.deliverMu.Lock()
	defer rr.deliverMu.Unlock()
	// Never start an app-server for a wake: its background terminals died
	// with the one that is gone. Observe re-arms when it is reachable.
	c, err := a.client(ctx, rr, false)
	if err != nil {
		return
	}
	ts.mu.Lock()
	if rr.retrying.Load() {
		a.armWake(ts) // a capacity continue is starting: it goes first
		ts.mu.Unlock()
		return
	}
	if !ts.wakeable() || len(ts.wakes) == 0 {
		ts.mu.Unlock()
		return // a turn started meanwhile; its completion re-arms
	}
	batch := ts.wakes
	ts.wakes = nil
	ts.mu.Unlock()
	id := fmt.Sprintf("%s%d", wakeIDPrefix, time.Now().UnixNano())
	var tr turnStartResponse
	err = c.call(ctx, "turn/start", turnStartParams{ThreadID: rr.meta.ThreadID, Input: []userInput{{Type: "text", Text: wakeNotice(batch)}}, ClientUserMessageID: id}, &tr)
	if err != nil {
		var re *rpcError
		if !errors.As(err, &re) {
			return // outcome unknown: the notice may have landed; never resend it
		}
		ts.mu.Lock()
		var retry []bgDone
		for _, w := range batch {
			if w.attempts++; w.attempts < wakeAttempts {
				retry = append(retry, w)
			}
		}
		ts.wakes = append(retry, ts.wakes...)
		a.armWake(ts)
		ts.mu.Unlock()
		return
	}
	// Hold deliverMu until the turn is visible (its userMessage item comes
	// after turn/started), so a delivery right after sees the thread busy.
	a.awaitTurnVisible(rr, id)
	if a.o.OnAutomation != nil {
		go a.o.OnAutomation(contracts.NodeAutomation{RunID: rr.meta.RunID, SessionID: rr.meta.SessionID, Harness: contracts.HarnessCodex,
			Action: contracts.AutomationBackgroundWake, Detail: wakeDetail(batch)})
	}
}

// awaitTurnVisible waits (at most DeliverTimeout) until the userMessage
// with clientId id was seen, i.e. the turn it started is known as active.
// Callers hold rr.deliverMu.
func (a *Adapter) awaitTurnVisible(rr *runRef, id string) {
	end := time.Now().Add(a.o.DeliverTimeout)
	for time.Now().Before(end) {
		if _, ok := a.evidenceOf(rr, id); ok {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// syncDeliver waits until no delivery, wake or capacity continue is
// sending on the run (each holds deliverMu until its turn is visible),
// bounded by ctx and DeliverTimeout plus a second. It reports whether it
// got through.
func (a *Adapter) syncDeliver(ctx context.Context, rr *runRef) bool {
	end := time.Now().Add(a.o.DeliverTimeout + time.Second)
	if dl, ok := ctx.Deadline(); ok && dl.Add(-2*time.Second).Before(end) {
		end = dl.Add(-2 * time.Second) // leave the caller time to act
	}
	for {
		if rr.deliverMu.TryLock() {
			rr.deliverMu.Unlock()
			return true
		}
		if time.Now().After(end) || ctx.Err() != nil {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// wakeNotice is the wake turn's input.
func wakeNotice(batch []bgDone) string {
	var b strings.Builder
	if len(batch) == 1 {
		b.WriteString("[pyre] background command finished: ")
		describeDone(&b, batch[0], wakeTailBytes)
	} else {
		fmt.Fprintf(&b, "[pyre] %d background commands finished:", len(batch))
		per := max(wakeTotalBytes/min(len(batch), wakeMaxListed), wakeMinTailBytes)
		for i, w := range batch {
			if i == wakeMaxListed {
				fmt.Fprintf(&b, "\n(and %d more)", len(batch)-wakeMaxListed)
				break
			}
			fmt.Fprintf(&b, "\n%d. ", i+1)
			describeDone(&b, w, per)
		}
	}
	b.WriteString("\nPyre started this turn because the command finished after your turn ended. Act on the result if it matters to your task; otherwise reply in one line and end the turn.")
	return b.String()
}

func describeDone(b *strings.Builder, w bgDone, tail int) {
	fmt.Fprintf(b, "`%s` (", oneLine(w.Command, 300))
	switch {
	case w.ExitCode != nil:
		fmt.Fprintf(b, "exit %d", *w.ExitCode)
	case w.Status != "":
		b.WriteString(w.Status + ", exit code unknown")
	default:
		b.WriteString("exit code unknown")
	}
	if w.Cwd != "" {
		b.WriteString(", cwd " + w.Cwd)
	}
	if w.DurationMs != nil {
		b.WriteString(", ran " + (time.Duration(*w.DurationMs) * time.Millisecond).Round(time.Second).String())
	}
	b.WriteString("). ")
	out := strings.TrimRight(w.Output, " \t\r\n")
	if out == "" {
		b.WriteString("No output.")
		return
	}
	b.WriteString("Last output:\n")
	b.WriteString(tailBytes(out, tail))
}

// tailBytes keeps the last n bytes of s (whole runes, from a line start
// when one is near), marking a cut.
func tailBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	t := lastBytes(s, n)
	if i := strings.IndexByte(t, '\n'); i >= 0 && i < n/4 {
		t = t[i+1:]
	}
	return "…\n" + t
}

// lastBytes is at most the last n bytes of s, starting on a rune.
func lastBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := len(s) - n
	for cut < len(s) && (s[cut]&0xC0) == 0x80 {
		cut++
	}
	return s[cut:]
}

// wakeDetail is the audit line: commands only (redacted), never output.
func wakeDetail(batch []bgDone) string {
	cmds := make([]string, 0, len(batch))
	for _, w := range batch {
		cmds = append(cmds, oneLine(w.Command, 60))
	}
	return redact(oneLine(strings.Join(cmds, "; "), 160))
}
