package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/claudeplugin"
	"github.com/jhh3/pyre/internal/runtime/tmux"
)

// Journal states written to deliveries.jsonl.
const (
	jAttempting    = "attempting"    // written (fsync) before any byte reaches the TUI
	jSubmitted     = "submitted"     // Enter pressed (or injection queued)
	jAccepted      = "accepted"      // evidence found
	jRejected      = "rejected"      // known not submitted; safe to retry
	jIndeterminate = "indeterminate" // may have been submitted
	jCleared       = "cleared"       // reconcile removed an unsent draft
)

type journalRec struct {
	At        time.Time `json:"at"`
	MessageID string    `json:"message_id"`
	AttemptID string    `json:"attempt_id,omitempty"`
	RunID     string    `json:"run_id"`
	State     string    `json:"state"`
	Mode      string    `json:"mode,omitempty"` // paste | stop_hook
	Evidence  string    `json:"evidence,omitempty"`
	Ref       string    `json:"ref,omitempty"`
	Detail    string    `json:"detail,omitempty"`
	// Tail is the normalised end of the pasted text (draftTail), kept on
	// the attempting record so a leftover draft whose marker scrolled out
	// of view can still be recognised (Reconcile, Observe).
	Tail string `json:"tail,omitempty"`
}

func (a *Adapter) journal(h contracts.RunHandle, r journalRec) error {
	r.At = a.o.Now().UTC()
	r.RunID = h.RunID
	return appendJSONL(filepath.Join(h.StateDir, fileDeliveries), r)
}

func readJournal(stateDir string) []journalRec {
	var out []journalRec
	_, _ = readJSONLFrom(filepath.Join(stateDir, fileDeliveries), 0, 0, func(line []byte, _ int64) {
		var r journalRec
		if json.Unmarshal(line, &r) == nil {
			out = append(out, r)
		}
	})
	return out
}

// lastAttempts maps message id -> its most recent journal record. The
// attempt ID, mode and draft tail of the attempt carry over to its later
// records.
func lastAttempts(stateDir string) map[string]journalRec {
	m := map[string]journalRec{}
	for _, r := range readJournal(stateDir) {
		if prev, ok := m[r.MessageID]; ok && (r.AttemptID == "" || r.AttemptID == prev.AttemptID) && r.State != jAttempting {
			if r.AttemptID == "" {
				r.AttemptID = prev.AttemptID
			}
			if r.Mode == "" {
				r.Mode = prev.Mode
			}
			if r.Tail == "" {
				r.Tail = prev.Tail
			}
		}
		m[r.MessageID] = r
	}
	return m
}

// lockFile takes an exclusive cross-process lock on <state>/deliver.lock.
func lockFile(stateDir string) (func(), error) {
	f, err := os.OpenFile(filepath.Join(stateDir, "deliver.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}

func (a *Adapter) evidenceFor(h contracts.RunHandle, msgID string) (evidence, bool) {
	e := a.entry(h)
	e.mu.Lock()
	defer e.mu.Unlock()
	a.refresh(h, e)
	return e.rs.bestEvidence(msgID)
}

func accepted(ev evidence, detail string) contracts.DeliveryResult {
	return contracts.DeliveryResult{State: contracts.DeliveryAccepted, Evidence: ev.Kind, EvidenceRef: ev.Ref, Detail: detail}
}

func queued(reason string) contracts.DeliveryResult {
	return contracts.DeliveryResult{State: contracts.DeliveryQueued, Evidence: contracts.EvidenceNone, Detail: reason}
}

// queuedDoorbell is queued plus, with the doorbell on, a note that the
// plugin monitor tells the agent and acceptance arrives as a receipt.
func (a *Adapter) queuedDoorbell(h contracts.RunHandle, reason string) contracts.DeliveryResult {
	if len(a.o.DoorbellCommand) > 0 {
		if DoorbellAlive(h.StateDir) {
			reason += "; doorbell (input.monitor) notifies the agent, acceptance follows as a claude.tool.inbox_read receipt"
		} else {
			reason += "; doorbell monitor is not running (Claude does not restart plugin monitors), so mail waits for the paste path"
		}
	}
	return queued(reason)
}

// doorbellHold keeps queued mail that the doorbell already announced for
// the agent's own inbox read (DoorbellPasteHold), so a paste cannot race
// the queued notification. A dead watcher, steering, or an expired hold
// falls through to the paste path.
func (a *Adapter) doorbellHold(h contracts.RunHandle, in contracts.DeliveryInput) (contracts.DeliveryResult, bool) {
	if len(a.o.DoorbellCommand) == 0 || in.Mode == contracts.ModeSteer {
		return contracts.DeliveryResult{}, false
	}
	at, ok := claudeplugin.NotifiedAt(h.StateDir, in.MessageID)
	if !ok || !DoorbellAlive(h.StateDir) {
		return contracts.DeliveryResult{}, false
	}
	left := a.o.DoorbellPasteHold - a.o.Now().Sub(at)
	if left <= 0 {
		return contracts.DeliveryResult{}, false
	}
	// Stable text: the engine reports a queued reason only when it changes.
	return queued(fmt.Sprintf("doorbell notified the agent at %s; waiting for its inbox read (paste fallback after %s)",
		at.UTC().Format(time.RFC3339), a.o.DoorbellPasteHold)), true
}

func rejected(reason string) contracts.DeliveryResult {
	return contracts.DeliveryResult{State: contracts.DeliveryRejected, Evidence: contracts.EvidenceNone, Detail: reason}
}

func indeterminate(reason string) contracts.DeliveryResult {
	return contracts.DeliveryResult{State: contracts.DeliveryIndeterminate, Evidence: contracts.EvidenceNone, Detail: reason}
}

func (a *Adapter) crash(point string) error {
	if a.o.testHook != nil {
		return a.o.testHook(point)
	}
	return nil
}

// renderInput makes sure the text starts with contracts.MessageMarker.
func renderInput(in contracts.DeliveryInput) string {
	marker := contracts.MessageMarker(in.MessageID)
	if strings.Contains(in.Text, marker) {
		return in.Text
	}
	return marker + " " + in.Text
}

// Deliver pastes marked input into the TUI when it is safe, or queues.
//
// Safe means: pane alive, no writable human client attached (input lease),
// hook state idle/running/waiting_on_background, no pending approval or
// dialog, and an empty input box. An "attempting" record is fsynced before
// the paste; the marker must be visible on the input line before Enter; the
// result is accepted only with evidence (UserPromptSubmit hook containing the
// marker, a transcript queue enqueue, or a transcript user record).
func (a *Adapter) Deliver(ctx context.Context, h contracts.RunHandle, in contracts.DeliveryInput) (contracts.DeliveryResult, error) {
	if !safeID(in.MessageID) {
		return contracts.DeliveryResult{}, contracts.Errorf(contracts.ErrInvalid, "invalid message id %q", in.MessageID)
	}
	switch in.Mode {
	case contracts.ModeInfo:
		return contracts.DeliveryResult{}, contracts.Errorf(contracts.ErrInvalid, "info messages are never injected into a runtime")
	case contracts.ModeSteer:
		if !a.o.StopHookSteer {
			return contracts.DeliveryResult{}, contracts.Errorf(contracts.ErrUnsupported, "Claude TUI has no mid-turn steer; enable Stop-hook steering or send as queued mail")
		}
	}
	e := a.entry(h)
	e.deliverMu.Lock()
	defer e.deliverMu.Unlock()
	unlock, err := lockFile(h.StateDir)
	if err != nil {
		return contracts.DeliveryResult{}, contracts.Errorf(contracts.ErrUnavailable, "state dir: %v", err)
	}
	defer unlock()

	if ev, ok := a.evidenceFor(h, in.MessageID); ok {
		return accepted(ev, "already accepted (evidence present; not re-sent)"), nil
	}
	if r, held := a.doorbellHold(h, in); held {
		return r, nil
	}
	if in.Mode == contracts.ModeSteer {
		state, _, _ := a.derived(h)
		if state == contracts.RunRunning || state == contracts.RunWaitingApproval {
			return a.deliverStopHook(ctx, h, in)
		}
	}
	return a.deliverPaste(ctx, h, in)
}

// InputReady implements delivery.InputReadiness: the paste gate without
// pasting. A run that would refuse with "queued" (not started, busy, a
// dialog or a human on the terminal, an approval pending) is not ready, so
// the engine keeps the mail queued without claiming an attempt. Steering,
// held doorbell mail, errors and refusals that reject are left to Deliver.
func (a *Adapter) InputReady(ctx context.Context, h contracts.RunHandle, mode contracts.MessageMode) (bool, string) {
	if mode == contracts.ModeSteer {
		return true, ""
	}
	_, refusal, err := a.gate(ctx, h)
	if err != nil || refusal == nil || refusal.State != contracts.DeliveryQueued {
		return true, ""
	}
	return false, refusal.Detail
}

func (a *Adapter) gate(ctx context.Context, h contracts.RunHandle) (screenInfo, *contracts.DeliveryResult, error) {
	p, err := a.srv.Pane(ctx, h.TmuxTarget)
	if err != nil {
		if errors.Is(err, tmux.ErrNoSession) || errors.Is(err, tmux.ErrNoServer) {
			r := rejected("run is gone (tmux session missing)")
			return screenInfo{}, &r, nil
		}
		return screenInfo{}, nil, contracts.Errorf(contracts.ErrUnavailable, "tmux: %v", err)
	}
	if p.Dead {
		r := rejected("claude has exited")
		return screenInfo{}, &r, nil
	}
	cs, err := a.srv.ListClients(ctx, h.TmuxTarget)
	if err != nil {
		return screenInfo{}, nil, contracts.Errorf(contracts.ErrUnavailable, "tmux list-clients: %v", err)
	}
	if n := tmux.WritableClients(cs); n > 0 {
		r := a.queuedDoorbell(h, fmt.Sprintf("human attached to the terminal (%d client(s)); input lease", n))
		return screenInfo{}, &r, nil
	}
	e := a.entry(h)
	e.mu.Lock()
	a.refresh(h, e)
	state, reason := e.rs.derive()
	pending := false
	for _, ap := range a.evalApprovals(h, e.rs, state) {
		if ap.State == contracts.ApprovalPending || ap.State == contracts.ApprovalExpired {
			pending = true
		}
	}
	e.mu.Unlock()
	switch state {
	case contracts.RunWaitingInput, contracts.RunRunning, contracts.RunWaitingBackground:
	default:
		r := a.queuedDoorbell(h, fmt.Sprintf("runtime not ready for input: %s (%s)", state, reason))
		return screenInfo{}, &r, nil
	}
	if pending {
		r := a.queuedDoorbell(h, "approval pending")
		return screenInfo{}, &r, nil
	}
	si, err := a.screen(ctx, h.TmuxTarget)
	// The TUI may still be rendering right after a hook fired: give the
	// input box a moment to appear (but never wait out a dialog).
	for i := 0; i < 20 && err == nil && !si.HasInputBox && si.Dialog == ""; i++ {
		time.Sleep(100 * time.Millisecond)
		si, err = a.screen(ctx, h.TmuxTarget)
	}
	if err != nil {
		return screenInfo{}, nil, contracts.Errorf(contracts.ErrUnavailable, "capture-pane: %v", err)
	}
	if !si.HasInputBox {
		why := "input box not visible"
		if si.Dialog != "" {
			why = "dialog on screen: " + si.Dialog
		}
		r := queued(why)
		return si, &r, nil
	}
	if !si.InputEmpty() {
		why := "input line has an unsent draft (not Pyre's); not pasting over it"
		if id, _ := a.leftoverDraft(h, si); id != "" {
			why = "input line holds Pyre's unsent draft of " + id + " (its delivery is indeterminate); not pasting over it. " + draftAction(id)
		}
		r := queued(why)
		return si, &r, nil
	}
	return si, nil, nil
}

// Claude's launch size. A pane takes the size of the last attached client
// and keeps it after the client leaves (fitPane restores it).
const (
	launchWidth  = 200
	launchHeight = 50
)

// fitPane restores the launch size of a run window that an attached client
// shrank (window-size latest keeps the client's size after it detaches), so
// a paste has the full width again. It acts only when no client at all is
// attached; an attached client keeps its size. It waits briefly for Claude
// to redraw at the new width and reports whether it resized.
func (a *Adapter) fitPane(ctx context.Context, h contracts.RunHandle) bool {
	p, err := a.srv.Pane(ctx, h.TmuxTarget)
	if err != nil || p.Width == 0 || (p.Width >= launchWidth && p.Height >= launchHeight) {
		return false
	}
	cs, err := a.srv.ListClients(ctx, h.TmuxTarget)
	if err != nil || len(cs) > 0 {
		return false
	}
	if a.srv.ResizeWindow(ctx, h.TmuxTarget, max(p.Width, launchWidth), max(p.Height, launchHeight)) != nil {
		return false
	}
	for i := 0; i < 20; i++ {
		time.Sleep(100 * time.Millisecond)
		if si, err := a.screen(ctx, h.TmuxTarget); err == nil && si.HasInputBox && si.RuleWidth >= launchWidth-2 {
			break
		}
	}
	return true
}

// draftAction is what a human does about a draft Pyre may have left in
// Claude's input box. Pyre never presses Enter on text it cannot recognise
// as its own and never replays an attempt whose outcome is unknown.
func draftAction(msgID string) string {
	return "Needs a human: attach to the session and check Claude's input box. If it holds this message unsent, press Enter to send it " +
		"(acceptance then follows from Claude's prompt hook) or Ctrl-C to clear it and run `pyre resolve-delivery " + msgID +
		" rejected --note \"draft cleared\"`. Do not resend it."
}

// leftoverDraft returns the message whose unsent paste the input box shows:
// the latest finished paste attempt (indeterminate, or rejected because the
// paste rendered late) that has no acceptance evidence and whose marker or
// journaled tail the box shows. An attempt still in progress is not a
// leftover.
func (a *Adapter) leftoverDraft(h contracts.RunHandle, si screenInfo) (string, journalRec) {
	if !si.HasInputBox || si.InputEmpty() {
		return "", journalRec{}
	}
	var best journalRec
	for id, r := range lastAttempts(h.StateDir) {
		if r.Mode != "paste" || (r.State != jIndeterminate && r.State != jRejected) {
			continue
		}
		if !si.showsDraft(contracts.MessageMarker(id), r.Tail) {
			continue
		}
		if best.MessageID == "" || r.At.After(best.At) {
			best = r
		}
	}
	if best.MessageID == "" {
		return "", journalRec{}
	}
	if _, ok := a.evidenceFor(h, best.MessageID); ok {
		return "", journalRec{}
	}
	return best.MessageID, best
}

func (a *Adapter) deliverPaste(ctx context.Context, h contracts.RunHandle, in contracts.DeliveryInput) (contracts.DeliveryResult, error) {
	_, refusal, err := a.gate(ctx, h)
	if err != nil {
		return contracts.DeliveryResult{}, err
	}
	if refusal != nil {
		return *refusal, nil
	}
	if a.fitPane(ctx, h) {
		// Claude redrew at the restored size: check the box again.
		if _, refusal, err = a.gate(ctx, h); err != nil {
			return contracts.DeliveryResult{}, err
		}
		if refusal != nil {
			return *refusal, nil
		}
	}
	text := renderInput(in)
	marker := contracts.MessageMarker(in.MessageID)
	if err := a.journal(h, journalRec{MessageID: in.MessageID, AttemptID: in.AttemptID, State: jAttempting, Mode: "paste", Tail: draftTail(text)}); err != nil {
		return contracts.DeliveryResult{}, contracts.Errorf(contracts.ErrUnavailable, "journal: %v", err)
	}
	if err := a.crash("after_journal"); err != nil {
		return contracts.DeliveryResult{}, err
	}
	if err := a.srv.Paste(ctx, h.TmuxTarget, "pyre-"+shortHash(in.MessageID), []byte(text)); err != nil {
		_ = a.journal(h, journalRec{MessageID: in.MessageID, AttemptID: in.AttemptID, State: jIndeterminate, Detail: "paste error: " + err.Error()})
		return indeterminate("paste-buffer failed after journaling: " + err.Error()), nil
	}
	if err := a.crash("after_paste"); err != nil {
		return contracts.DeliveryResult{}, err
	}
	// The paste must be recognisable in the input box before Enter: a paste
	// into a dialog is ignored, and a bare Enter there could approve it.
	// Recognised: the marker, the collapsed "[Pasted text #N]" placeholder,
	// or (when Claude scrolled the marker out of view) the box showing
	// exactly the end of the text.
	how := ""
	var si screenInfo
	for i := 0; i < 30 && how == ""; i++ {
		time.Sleep(100 * time.Millisecond)
		si, err = a.screen(ctx, h.TmuxTarget)
		if err == nil {
			how = si.pasteVisible(marker, text)
		}
	}
	if how == "" {
		detail := "pasted text not recognised in the input box; Enter not pressed"
		if err == nil && si.HasInputBox && si.InputEmpty() {
			_ = a.journal(h, journalRec{MessageID: in.MessageID, AttemptID: in.AttemptID, State: jRejected, Detail: detail})
			return rejected(detail + " (not submitted; safe to retry)"), nil
		}
		switch {
		case err != nil:
			detail += " (screen unavailable: " + err.Error() + ")"
		case si.Dialog != "":
			detail += " (dialog on screen: " + si.Dialog + ")"
		case !si.HasInputBox:
			detail += " (input box not visible)"
		default:
			detail += " (the box shows text that is neither the marker nor the end of this message)"
		}
		detail += ". " + draftAction(in.MessageID)
		_ = a.journal(h, journalRec{MessageID: in.MessageID, AttemptID: in.AttemptID, State: jIndeterminate, Detail: detail})
		return indeterminate(detail), nil
	}
	// A human who attached after the gate may be typing into the box: never
	// press Enter under them. The draft stays for them to send or clear.
	if cs, cerr := a.srv.ListClients(ctx, h.TmuxTarget); cerr != nil || tmux.WritableClients(cs) > 0 {
		why := "a human attached to the terminal while Pyre's paste was in the input box"
		if cerr != nil {
			why = "tmux list-clients failed after the paste (" + cerr.Error() + ")"
		}
		detail := why + "; Enter not pressed. " + draftAction(in.MessageID)
		_ = a.journal(h, journalRec{MessageID: in.MessageID, AttemptID: in.AttemptID, State: jIndeterminate, Detail: detail})
		return indeterminate(detail), nil
	}
	if err := a.srv.SendKeys(ctx, h.TmuxTarget, tmux.KeyEnter); err != nil {
		_ = a.journal(h, journalRec{MessageID: in.MessageID, AttemptID: in.AttemptID, State: jIndeterminate, Detail: "Enter failed: " + err.Error()})
		return indeterminate("send Enter failed: " + err.Error()), nil
	}
	if err := a.crash("after_enter"); err != nil {
		return contracts.DeliveryResult{}, err
	}
	_ = a.journal(h, journalRec{MessageID: in.MessageID, AttemptID: in.AttemptID, State: jSubmitted, Mode: "paste", Detail: "paste recognised by " + how})
	return a.awaitEvidence(ctx, h, in)
}

func (a *Adapter) awaitEvidence(ctx context.Context, h contracts.RunHandle, in contracts.DeliveryInput) (contracts.DeliveryResult, error) {
	end := time.Now().Add(a.o.DeliverTimeout)
	for {
		if ev, ok := a.evidenceFor(h, in.MessageID); ok {
			_ = a.journal(h, journalRec{MessageID: in.MessageID, AttemptID: in.AttemptID, State: jAccepted, Evidence: string(ev.Kind), Ref: ev.Ref})
			return accepted(ev, ""), nil
		}
		if time.Now().After(end) || ctx.Err() != nil {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	_ = a.journal(h, journalRec{MessageID: in.MessageID, AttemptID: in.AttemptID, State: jIndeterminate, Detail: "no evidence within timeout"})
	return indeterminate(fmt.Sprintf("submitted but no acceptance evidence within %s; reconcile later (never auto-replayed)", a.o.DeliverTimeout)), nil
}

// deliverStopHook queues a Stop-hook injection (turn-end steer). If the turn
// does not end within DeliverTimeout the injection is withdrawn atomically
// and the result is rejected (safe to retry).
func (a *Adapter) deliverStopHook(ctx context.Context, h contracts.RunHandle, in contracts.DeliveryInput) (contracts.DeliveryResult, error) {
	text := renderInput(in)
	if err := a.journal(h, journalRec{MessageID: in.MessageID, AttemptID: in.AttemptID, State: jAttempting, Mode: "stop_hook"}); err != nil {
		return contracts.DeliveryResult{}, contracts.Errorf(contracts.ErrUnavailable, "journal: %v", err)
	}
	inj := injection{MessageID: in.MessageID, AttemptID: in.AttemptID, Text: text, At: a.o.Now().UTC()}
	b, _ := json.Marshal(inj)
	name := injectionName(a.o.Now().UnixNano(), in.MessageID)
	path := filepath.Join(h.StateDir, dirInject, name+".msg")
	if err := writeFileAtomic(path, b); err != nil {
		return contracts.DeliveryResult{}, contracts.Errorf(contracts.ErrUnavailable, "write injection: %v", err)
	}
	_ = a.journal(h, journalRec{MessageID: in.MessageID, AttemptID: in.AttemptID, State: jSubmitted, Mode: "stop_hook", Ref: name})
	end := time.Now().Add(a.o.DeliverTimeout)
	for time.Now().Before(end) && ctx.Err() == nil {
		if ev, ok := a.evidenceFor(h, in.MessageID); ok {
			_ = a.journal(h, journalRec{MessageID: in.MessageID, AttemptID: in.AttemptID, State: jAccepted, Evidence: string(ev.Kind), Ref: ev.Ref})
			return accepted(ev, "delivered at turn end via Stop hook"), nil
		}
		time.Sleep(150 * time.Millisecond)
	}
	if os.Rename(path, strings.TrimSuffix(path, ".msg")+".withdrawn") == nil {
		_ = a.journal(h, journalRec{MessageID: in.MessageID, AttemptID: in.AttemptID, State: jRejected, Detail: "turn did not end in time; injection withdrawn"})
		return rejected("turn did not end within timeout; Stop-hook injection withdrawn (not delivered; safe to retry)"), nil
	}
	// Claimed concurrently: evidence must follow.
	return a.awaitEvidence(ctx, h, in)
}

// Reconcile looks for evidence of an earlier attempt. Evidence → accepted.
// A draft with the marker still on the input line → it was never submitted:
// clear it and return rejected (safe to retry). Otherwise indeterminate.
// Never replays.
func (a *Adapter) Reconcile(ctx context.Context, h contracts.RunHandle, messageID string) (contracts.DeliveryResult, error) {
	if !safeID(messageID) {
		return contracts.DeliveryResult{}, contracts.Errorf(contracts.ErrInvalid, "invalid message id %q", messageID)
	}
	e := a.entry(h)
	e.deliverMu.Lock()
	defer e.deliverMu.Unlock()
	unlock, err := lockFile(h.StateDir)
	if err != nil {
		return contracts.DeliveryResult{}, contracts.Errorf(contracts.ErrUnavailable, "state dir: %v", err)
	}
	defer unlock()
	if ev, ok := a.evidenceFor(h, messageID); ok {
		last := lastAttempts(h.StateDir)[messageID]
		if last.State != jAccepted {
			_ = a.journal(h, journalRec{MessageID: messageID, AttemptID: last.AttemptID, State: jAccepted, Evidence: string(ev.Kind), Ref: ev.Ref, Detail: "reconciled"})
		}
		return accepted(ev, "reconciled from evidence"), nil
	}
	last, ok := lastAttempts(h.StateDir)[messageID]
	if !ok {
		return rejected("no attempt recorded for this run (never pasted); safe to deliver"), nil
	}
	switch last.State {
	case jRejected, jCleared:
		return rejected("previous attempt known not submitted: " + last.Detail), nil
	}
	// Stop-hook injection still queued → withdraw it.
	if r, done := a.reconcileInjection(h, messageID, last); done {
		return r, nil
	}
	marker := contracts.MessageMarker(messageID)
	si, err := a.screen(ctx, h.TmuxTarget)
	// A paste written just before a crash may still be rendering. The
	// marker may be out of view in a long draft: the journaled tail then
	// identifies it.
	for i := 0; i < 12 && err == nil && !si.showsDraft(marker, last.Tail); i++ {
		if ev, ok := a.evidenceFor(h, messageID); ok {
			return accepted(ev, "reconciled from evidence"), nil
		}
		time.Sleep(150 * time.Millisecond)
		si, err = a.screen(ctx, h.TmuxTarget)
	}
	if err == nil && si.showsDraft(marker, last.Tail) {
		cs, cerr := a.srv.ListClients(ctx, h.TmuxTarget)
		if cerr != nil || tmux.WritableClients(cs) > 0 {
			return indeterminate("unsent draft of this message is on the input line but a human is attached; not clearing. " + draftAction(messageID)), nil
		}
		if a.clearInput(ctx, h.TmuxTarget) {
			_ = a.journal(h, journalRec{MessageID: messageID, AttemptID: last.AttemptID, State: jCleared, Detail: "unsent draft cleared by reconcile"})
			return rejected("draft of this message was on the input line (never submitted); cleared; safe to retry"), nil
		}
		return indeterminate("draft of this message visible but could not be cleared. " + draftAction(messageID)), nil
	}
	if err != nil {
		return indeterminate("screen unavailable: " + err.Error()), nil
	}
	return indeterminate(fmt.Sprintf("attempt %s (%s) has no evidence and no visible draft; may have been submitted — resolve explicitly", last.AttemptID, last.State)), nil
}

func (a *Adapter) reconcileInjection(h contracts.RunHandle, messageID string, last journalRec) (contracts.DeliveryResult, bool) {
	dir := filepath.Join(h.StateDir, dirInject)
	ents, _ := os.ReadDir(dir)
	for _, en := range ents {
		n := en.Name()
		if !strings.HasSuffix(n, "-"+messageID+".msg") {
			continue
		}
		p := filepath.Join(dir, n)
		if os.Rename(p, strings.TrimSuffix(p, ".msg")+".withdrawn") == nil {
			_ = a.journal(h, journalRec{MessageID: messageID, AttemptID: last.AttemptID, State: jRejected, Detail: "queued injection withdrawn by reconcile"})
			return rejected("Stop-hook injection was still queued; withdrawn; safe to retry"), true
		}
	}
	return contracts.DeliveryResult{}, false
}

// clearInput empties the input box (C-u clears one line of a multi-line
// draft, so BSpace joins lines until the box is empty). Returns success.
func (a *Adapter) clearInput(ctx context.Context, name string) bool {
	for i := 0; i < 80; i++ {
		si, err := a.screen(ctx, name)
		if err != nil || !si.HasInputBox {
			return false
		}
		if si.InputEmpty() {
			return true
		}
		key := tmux.KeyCtrlU
		if i%2 == 1 {
			key = tmux.KeyBackspace
		}
		if err := a.srv.SendKeys(ctx, name, key); err != nil {
			return false
		}
		time.Sleep(80 * time.Millisecond)
	}
	return false
}
