package codex

import (
	"context"
	"errors"
	"fmt"
	"runtime"
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
// The same holds for a yielded command that exits while its own turn still
// runs: the model learns the exit only by polling the process again
// (write_stdin), and Codex emits nothing for a poll that finds it exited
// (iss_…: the exit came 26 s before the turn
// ended, the model never polled it and ended the turn on `pyre waiting-on`).
// Such a completion is held on the thread and judged once its turn ended:
// the rollout tells whether the model got the exit (exitsSeen); one it did
// not get is reported like a completion after the turn.
//
// Pyre provides the wake. A command that finishes after its own turn ended
// (or inside it, unseen) is queued on the thread; once the thread is idle
// (no active turn, no pending approval, no usage limit, no capacity
// continue due) and no further completion arrived for wakeSettle, one
// turn/start reports every queued completion. A completion during another
// turn waits for that turn to end (never a steer). The turn is a fresh
// notice, not a replay: a turn/start Codex refused is retried
// (wakeAttempts), one whose outcome is unknown is dropped. Not woken:
// stopped runs; the commands running when Pyre interrupts the thread (an
// interrupt, also of an idle thread, cleans its background terminals), and
// the in-turn completions of an interrupted turn; sessions the daemon says
// are archived or being archived (Options.WakeAllowed). The queue lives in
// memory; a recovered run backfills what it missed from the rollout
// (bgrecover.go). A dead app-server is reported the same way (serverlost.go).

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
	// wakeQueueMax the completions queued, and the in-turn exits held, for
	// one thread (capWakes).
	wakeSeenMax  = 512
	wakeQueueMax = 32
	// wakeUnknownMinRun: an in-turn completion the rollout says nothing
	// about wakes only when it ran at least this long (exec_command's
	// default yield), i.e. when it most likely went to the background.
	wakeUnknownMinRun = 10 * time.Second
	// execStartup is the commandExecution source of a command the model
	// started with exec_command (the only kind that yields to the model).
	execStartup = "unifiedExecStartup"
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

// bgDone is one background command that finished after its turn ended, or
// inside it (InTurn: reported only when the rollout shows the model did not
// get the exit).
type bgDone struct {
	ItemID, TurnID string
	ProcessID      string // exec_command's process (the model's session_id); else ""
	Command, Cwd   string
	Status         string
	ExitCode       *int
	DurationMs     *int64
	Output         string
	At             time.Time
	InTurn         bool
	attempts       int
}

// commandText is the command the model ran, without the `zsh -lc '…'`
// wrapper when Codex parsed it.
func commandText(it item) string {
	if len(it.CommandActions) == 1 && strings.TrimSpace(it.CommandActions[0].Command) != "" {
		return it.CommandActions[0].Command
	}
	return it.Command
}

func newBgDone(it item, turnID string, at time.Time) bgDone {
	w := bgDone{ItemID: it.ID, TurnID: turnID, Command: commandText(it), Cwd: it.Cwd, Status: it.Status,
		ExitCode: it.ExitCode, DurationMs: it.DurationMs, Output: lastBytes(it.AggregatedOutput, 4*wakeTailBytes), At: at}
	if it.Source == execStartup {
		w.ProcessID = it.ProcessID
	}
	return w
}

// noteCommandStart tracks the commands running on the thread (so an
// interrupt can mute exactly those). Called with ts.mu held.
func (ts *threadState) noteCommandStart(it item) {
	if it.Type != "commandExecution" || it.ID == "" {
		return
	}
	if ts.runningCmds == nil || len(ts.runningCmds) >= wakeSeenMax {
		ts.runningCmds = map[string]string{}
	}
	ts.runningCmds[it.ID] = commandText(it)
}

// muteRunning drops the queue and ignores the exits of the commands
// running now (the owner interrupted the work). Called with ts.mu held.
func (ts *threadState) muteRunning() {
	ts.wakes, ts.inTurn, ts.lost = nil, nil, nil
	ts.agentWakes, ts.agentHeld = nil, nil
	if ts.wakeIgnore == nil || len(ts.wakeIgnore)+len(ts.runningCmds) > wakeSeenMax {
		ts.wakeIgnore = map[string]bool{}
	}
	for id := range ts.runningCmds {
		ts.wakeIgnore[id] = true
	}
}

// noteCommandDone queues a finished commandExecution when the turn that
// started it is no longer running, and holds an exec_command one that
// finished inside its own turn for settleTurnWakes. Called with ts.mu held.
func (ts *threadState) noteCommandDone(it item, turnID string, at time.Time) bool {
	if it.Type != "commandExecution" || it.ID == "" {
		return false
	}
	delete(ts.runningCmds, it.ID)
	ts.noteLiveDone(it.ID)
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
	if ts.wakeSeen[it.ID] {
		return false
	}
	if ts.status.Type == "active" && (turnID == "" || turnID == ts.activeTurn) {
		// The turn that started it is still running. It sees the exit when
		// exec_command returned it or when the model polls the process
		// again; whether it did is judged once the turn has ended.
		if w := newBgDone(it, turnID, at); w.ProcessID != "" && turnID != "" {
			w.InTurn = true
			ts.inTurn = capWakes(append(ts.inTurn, w))
		}
		return false
	}
	ts.markSeen(it.ID)
	ts.wakes = append(ts.wakes, newBgDone(it, turnID, at))
	ts.trimWakes()
	return true
}

func (ts *threadState) markSeen(id string) {
	if ts.wakeSeen == nil || len(ts.wakeSeen) >= wakeSeenMax {
		ts.wakeSeen = map[string]bool{}
	}
	ts.wakeSeen[id] = true
}

func (ts *threadState) trimWakes() {
	ts.wakes = capWakes(ts.wakes)
}

// capWakes bounds ws to wakeQueueMax. It drops the InTurn candidate that
// ran shortest first (one exec_command most likely returned the exit of:
// a yielded command ran at least its yield), then the oldest. A long turn
// runs dozens of commands that returned their exit before a yielded one
// exits; dropping the oldest lost that one (iss_…).
func capWakes(ws []bgDone) []bgDone {
	for len(ws) > wakeQueueMax {
		drop := 0
		for i, w := range ws {
			if w.InTurn && (!ws[drop].InTurn || ranMs(w) < ranMs(ws[drop])) {
				drop = i
			}
		}
		ws = append(ws[:drop:drop], ws[drop+1:]...)
	}
	return ws
}

// ranMs is how long the command ran (-1: unknown).
func ranMs(w bgDone) int64 {
	if w.DurationMs == nil {
		return -1
	}
	return *w.DurationMs
}

// settleTurnWakes runs when a turn ends. The exec_command completions held
// while it ran, and those queued while its id was unknown (after a pyred
// restart), become InTurn candidates, reported unless the rollout shows the
// model got the exit (Adapter.unseenExits). The turn's other completions are
// dropped (it saw them), as are all of an interrupted turn's (the owner
// stopped the work). Called with ts.mu held.
func (ts *threadState) settleTurnWakes(turnID, status string) {
	held := ts.inTurn
	ts.inTurn = nil
	if turnID == "" {
		return
	}
	keep := status != "interrupted"
	kept := ts.wakes[:0]
	for _, w := range ts.wakes {
		if w.TurnID != turnID {
			kept = append(kept, w)
		} else if keep && w.ProcessID != "" {
			w.InTurn = true
			kept = append(kept, w)
		}
	}
	ts.wakes = kept
	for _, w := range held {
		if keep && w.TurnID == turnID && !ts.wakeSeen[w.ItemID] {
			ts.markSeen(w.ItemID)
			w.InTurn = true
			ts.wakes = append(ts.wakes, w)
		}
	}
	ts.trimWakes()
}

// unseenExits drops the batch's InTurn completions whose exit the model got
// (rollout evidence, exitsSeen); one the rollout says nothing about is kept
// only when it ran at least wakeUnknownMinRun.
func (a *Adapter) unseenExits(rr *runRef, batch []bgDone) []bgDone {
	var cands []execRef
	for _, w := range batch {
		if w.InTurn {
			cands = append(cands, execRef{ItemID: w.ItemID, ProcessID: w.ProcessID})
		}
	}
	if len(cands) == 0 {
		return batch
	}
	var seen map[string]bool
	if path := rolloutPath(rr.stateDir, rr.meta); path != "" {
		seen = exitsSeen(path, cands)
	}
	kept := batch[:0]
	for _, w := range batch {
		if w.InTurn {
			if s, ok := seen[w.ItemID]; ok && s {
				continue
			} else if !ok && (w.DurationMs == nil || time.Duration(*w.DurationMs)*time.Millisecond < wakeUnknownMinRun) {
				continue
			}
		}
		kept = append(kept, w)
	}
	return kept
}

// clearWakes drops the queue; with mute, completions are ignored until
// wakeMute (Pyre is killing the thread's background terminals).
func (ts *threadState) clearWakes(mute time.Time) {
	ts.wakes, ts.inTurn, ts.lost = nil, nil, nil
	ts.agentWakes, ts.agentHeld = nil, nil
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

// wakeDue: something is queued for a wake turn. Called with ts.mu held.
func (ts *threadState) wakeDue() bool {
	return len(ts.wakes) > 0 || len(ts.agentWakes) > 0 || ts.lost != nil
}

// lastQueued is when the newest queued completion arrived (zero: none).
// Called with ts.mu held.
func (ts *threadState) lastQueued() time.Time {
	var t time.Time
	if n := len(ts.wakes); n > 0 {
		t = ts.wakes[n-1].At
	}
	if n := len(ts.agentWakes); n > 0 && ts.agentWakes[n-1].At.After(t) {
		t = ts.agentWakes[n-1].At
	}
	return t
}

// armWake schedules a wake attempt after wakeSettle (once; a later
// completion inside the window joins the same notice). Called with ts.mu
// held; turn completion and Observe re-arm a queue that had to wait.
func (a *Adapter) armWake(ts *threadState) {
	if ts.wakeArmed || !ts.wakeDue() || !ts.wakeable() {
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
	due := ts.wakeDue()
	last := ts.lastQueued()
	quiet := due && (last.IsZero() || time.Since(last) >= a.wakeDelay())
	if !quiet && due {
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
	if !ts.wakeable() || !ts.wakeDue() {
		ts.mu.Unlock()
		return // a turn started meanwhile; its completion re-arms
	}
	batch, agents, lost := ts.wakes, ts.agentWakes, ts.lost
	ts.wakes, ts.agentWakes, ts.lost = nil, nil, nil
	ts.mu.Unlock()
	batch = a.unseenExits(rr, batch)
	if agents = a.unseenAgents(rr, agents); len(batch) == 0 && len(agents) == 0 && lost == nil {
		return // the model got every in-turn exit and report
	}
	id := fmt.Sprintf("%s%d", wakeIDPrefix, time.Now().UnixNano())
	var tr turnStartResponse
	err = c.call(ctx, "turn/start", turnStartParams{ThreadID: rr.meta.ThreadID, Input: []userInput{{Type: "text", Text: wakeText(lost, batch, agents)}}, ClientUserMessageID: id}, &tr)
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
		var retryAgents []agentDone
		for _, d := range agents {
			if d.attempts++; d.attempts < wakeAttempts {
				d.InTurn = false // judged once; still queued in Codex
				retryAgents = append(retryAgents, d)
			}
		}
		ts.agentWakes = append(retryAgents, ts.agentWakes...)
		if lost != nil && ts.lost == nil {
			if lost.attempts++; lost.attempts < wakeAttempts {
				ts.lost = lost
			}
		}
		a.armWake(ts)
		ts.mu.Unlock()
		return
	}
	// Hold deliverMu until the turn is visible (its userMessage item comes
	// after turn/started), so a delivery right after sees the thread busy.
	a.awaitTurnVisible(rr, id)
	if a.o.OnAutomation != nil {
		go a.o.OnAutomation(contracts.NodeAutomation{RunID: rr.meta.RunID, SessionID: rr.meta.SessionID, Harness: contracts.HarnessCodex,
			Action: contracts.AutomationBackgroundWake, Detail: wakeDetail(lost, batch, agents)})
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
	b.WriteString("\nPyre started this turn because no turn of yours saw the command finish. Act on the result if it matters to your task; otherwise reply in one line and end the turn.")
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
	if w.ExitCode != nil && *w.ExitCode == exitSIGTERM {
		b.WriteString(sigtermNote())
	}
	out := strings.TrimRight(w.Output, " \t\r\n")
	if out == "" {
		b.WriteString("No output.")
		return
	}
	b.WriteString("Last output:\n")
	b.WriteString(tailBytes(out, tail))
}

// exitSIGTERM is a shell's exit status for a command ended by SIGTERM.
const exitSIGTERM = 128 + 15

// parentDeathSIGTERM: Codex gives pipe-mode exec children
// PR_SET_PDEATHSIG(SIGTERM) (codex-rs utils/pty spawn_helper), and Linux
// sends it when the app-server *thread* that spawned the command exits.
// Tokio's runtime threads come and go (a worker that ran block_in_place,
// as Codex's image caches do, idles out 10 s later), so a long command's
// top process can get SIGTERM at any time while its children, which do not
// inherit the setting, keep running (decision I61).
var parentDeathSIGTERM = runtime.GOOS == "linux"

// sigtermNote explains an exit 143 in a wake. Pyre never sends a background
// command SIGTERM: the commands its interrupts and cleans end never wake the
// agent, and Codex's own clean and prune use SIGKILL.
func sigtermNote() string {
	if parentDeathSIGTERM {
		return "Exit 143 is SIGTERM, not sent by Pyre: Codex sends it to a command's top process when the app-server thread that started it exits, and the command's children may still be running. Check them and the job's outputs before treating it as failed. "
	}
	return "Exit 143 is SIGTERM, not sent by Pyre; the command's children may still be running. "
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
func wakeDetail(lost *serverLoss, batch []bgDone, agents []agentDone) string {
	cmds := make([]string, 0, len(batch)+len(agents)+1)
	if lost != nil {
		cmds = append(cmds, "app-server lost")
	}
	for _, w := range batch {
		cmds = append(cmds, oneLine(w.Command, 60))
	}
	for _, d := range agents {
		cmds = append(cmds, "agent "+oneLine(agentName(d), 60))
	}
	return redact(oneLine(strings.Join(cmds, "; "), 160))
}
