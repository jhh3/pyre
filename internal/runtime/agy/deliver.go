package agy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/messaging/header"
	"github.com/jhh3/pyre/internal/osutil"
	"github.com/jhh3/pyre/internal/runtime/tmux"
)

// Journal states written to deliveries.jsonl.
const (
	jAttempting    = "attempting"    // fsynced before any byte reaches the TUI
	jSubmitted     = "submitted"     // Enter pressed
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
	Evidence  string    `json:"evidence,omitempty"`
	Ref       string    `json:"ref,omitempty"`
	Detail    string    `json:"detail,omitempty"`
	Tail      string    `json:"tail,omitempty"`     // end of the pasted text (recognises a scrolled draft)
	NoEnter   bool      `json:"no_enter,omitempty"` // indeterminate before Enter was pressed
}

func (a *Adapter) journal(h contracts.RunHandle, r journalRec) error {
	r.At = a.o.Now().UTC()
	r.RunID = h.RunID
	return appendJSONL(filepath.Join(h.StateDir, fileDeliveries), r)
}

// lastAttempts maps message id -> its latest journal record (the attempt
// id and tail carry over within one attempt).
func lastAttempts(stateDir string) map[string]journalRec {
	m := map[string]journalRec{}
	_, _ = readJSONLFrom(filepath.Join(stateDir, fileDeliveries), 0, func(line []byte, _ int64) {
		var r journalRec
		if json.Unmarshal(line, &r) != nil {
			return
		}
		if prev, ok := m[r.MessageID]; ok && r.State != jAttempting && (r.AttemptID == "" || r.AttemptID == prev.AttemptID) {
			if r.AttemptID == "" {
				r.AttemptID = prev.AttemptID
			}
			if r.Tail == "" {
				r.Tail = prev.Tail
			}
		}
		m[r.MessageID] = r
	})
	return m
}

func lockFile(stateDir string) (func(), error) {
	f, err := os.OpenFile(filepath.Join(stateDir, "deliver.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := osutil.Lock(f); err != nil {
		f.Close()
		return nil, err
	}
	return func() { _ = osutil.Unlock(f); f.Close() }, nil
}

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:4])
}

var safeIDRe = regexp.MustCompile(`^[A-Za-z0-9_]{1,128}$`)

func (a *Adapter) evidenceFor(h contracts.RunHandle, msgID string) (evidence, bool) {
	e := a.entry(h)
	e.mu.Lock()
	defer e.mu.Unlock()
	a.refresh(h, e)
	ev, ok := e.rs.accepted[msgID]
	return ev, ok
}

func accepted(ev evidence, detail string) contracts.DeliveryResult {
	return contracts.DeliveryResult{State: contracts.DeliveryAccepted, Evidence: contracts.EvidenceAgyUserInput, EvidenceRef: ev.Ref, Detail: detail}
}

func queued(reason string) contracts.DeliveryResult {
	return contracts.DeliveryResult{State: contracts.DeliveryQueued, Evidence: contracts.EvidenceNone, Detail: reason}
}

func rejected(reason string) contracts.DeliveryResult {
	return contracts.DeliveryResult{State: contracts.DeliveryRejected, Evidence: contracts.EvidenceNone, Detail: reason}
}

func indeterminate(reason string) contracts.DeliveryResult {
	return contracts.DeliveryResult{State: contracts.DeliveryIndeterminate, Evidence: contracts.EvidenceNone, Detail: reason}
}

// renderInput makes sure the text starts with the message marker.
func renderInput(in contracts.DeliveryInput) string {
	if header.HasMarker(in.Text, in.MessageID) {
		return in.Text
	}
	return contracts.MessageMarker(in.MessageID) + " " + in.Text
}

// pasteSafe drops control characters other than newline and tab from
// input before it is pasted: an ESC sequence in a message body (e.g. the
// bracketed-paste end "ESC[201~") would end the paste early and turn the
// rest into keystrokes, Enter included.
func pasteSafe(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return -1
		}
		return r
	}, s)
}

// draftTail is the normalised end of a pasted text.
func draftTail(text string) string {
	t := normalize(text)
	if r := []rune(t); len(r) > 40 {
		return string(r[len(r)-40:])
	}
	return t
}

// draftAction is what a human does about a draft Pyre may have left.
func draftAction(msgID string) string {
	return "Needs a human: attach to the session and check agy's input box. If it holds this message unsent, press Enter to send it " +
		"(acceptance then follows from the transcript) or clear it and run `pyre resolve-delivery " + msgID +
		" rejected --note \"draft cleared\"`. Do not resend it."
}

// Deliver pastes marked input into agy's idle prompt, or queues.
//
// Safe means: pane alive, no writable human client (input lease), the
// prompt idle and empty with no dialog. A busy run queues: Pyre retries when
// the turn ends (agy's own input queue is not used, so nothing is typed
// while a turn runs). An "attempting" record is fsynced before the paste;
// the marker (or the text's end) must be visible in the box before Enter;
// the result is accepted only when the marker appears in a USER_INPUT step
// of the transcript.
func (a *Adapter) Deliver(ctx context.Context, h contracts.RunHandle, in contracts.DeliveryInput) (contracts.DeliveryResult, error) {
	if !safeIDRe.MatchString(in.MessageID) {
		return contracts.DeliveryResult{}, contracts.Errorf(contracts.ErrInvalid, "invalid message id %q", in.MessageID)
	}
	switch in.Mode {
	case contracts.ModeInfo:
		return contracts.DeliveryResult{}, contracts.Errorf(contracts.ErrInvalid, "info messages are never injected into a runtime")
	case contracts.ModeSteer:
		return contracts.DeliveryResult{}, contracts.Errorf(contracts.ErrUnsupported, "agy runs have no mid-turn steering in Pyre; send as queued mail")
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
	if _, refusal, err := a.gate(ctx, h); err != nil {
		return contracts.DeliveryResult{}, err
	} else if refusal != nil {
		return *refusal, nil
	}
	text := pasteSafe(renderInput(in))
	if err := a.journal(h, journalRec{MessageID: in.MessageID, AttemptID: in.AttemptID, State: jAttempting, Tail: draftTail(text)}); err != nil {
		return contracts.DeliveryResult{}, contracts.Errorf(contracts.ErrUnavailable, "journal: %v", err)
	}
	if err := a.srv.Paste(ctx, h.TmuxTarget, "pyre-"+shortHash(in.MessageID), []byte(text)); err != nil {
		_ = a.journal(h, journalRec{MessageID: in.MessageID, AttemptID: in.AttemptID, State: jIndeterminate, Detail: "paste error: " + err.Error(), NoEnter: true})
		return indeterminate("paste-buffer failed after journaling: " + err.Error()), nil
	}
	var si screenInfo
	seen := false
	for i := 0; i < 30 && !seen; i++ {
		time.Sleep(100 * time.Millisecond)
		if si, err = a.screen(ctx, h.TmuxTarget); err == nil {
			seen = si.showsText(in.MessageID, text)
		}
	}
	if !seen {
		detail := "pasted text not recognised in agy's input box; Enter not pressed"
		if err == nil && si.InputEmpty() {
			_ = a.journal(h, journalRec{MessageID: in.MessageID, AttemptID: in.AttemptID, State: jRejected, Detail: detail})
			return rejected(detail + " (not submitted; safe to retry)"), nil
		}
		if err == nil && si.Dialog != "" {
			detail += " (dialog on screen: " + si.Dialog + ")"
		}
		detail += ". " + draftAction(in.MessageID)
		_ = a.journal(h, journalRec{MessageID: in.MessageID, AttemptID: in.AttemptID, State: jIndeterminate, Detail: detail, NoEnter: true})
		return indeterminate(detail), nil
	}
	if cs, cerr := a.srv.ListClients(ctx, h.TmuxTarget); cerr != nil || tmux.WritableClients(cs) > 0 {
		detail := "a human attached to the terminal while Pyre's paste was in the input box; Enter not pressed. " + draftAction(in.MessageID)
		_ = a.journal(h, journalRec{MessageID: in.MessageID, AttemptID: in.AttemptID, State: jIndeterminate, Detail: detail, NoEnter: true})
		return indeterminate(detail), nil
	}
	if err := a.srv.SendKeys(ctx, h.TmuxTarget, tmux.KeyEnter); err != nil {
		_ = a.journal(h, journalRec{MessageID: in.MessageID, AttemptID: in.AttemptID, State: jIndeterminate, Detail: "Enter failed: " + err.Error()})
		return indeterminate("send Enter failed: " + err.Error()), nil
	}
	_ = a.journal(h, journalRec{MessageID: in.MessageID, AttemptID: in.AttemptID, State: jSubmitted})
	return a.awaitEvidence(ctx, h, in)
}

func (a *Adapter) awaitEvidence(ctx context.Context, h contracts.RunHandle, in contracts.DeliveryInput) (contracts.DeliveryResult, error) {
	end := time.Now().Add(a.o.DeliverTimeout)
	cut := false
	for {
		if ev, ok := a.evidenceFor(h, in.MessageID); ok {
			_ = a.journal(h, journalRec{MessageID: in.MessageID, AttemptID: in.AttemptID, State: jAccepted, Evidence: string(contracts.EvidenceAgyUserInput), Ref: ev.Ref})
			return accepted(ev, ""), nil
		}
		if time.Now().After(end) {
			break
		}
		if ctx.Err() != nil && !cut {
			cut = true
			if soon := time.Now().Add(3 * time.Second); soon.Before(end) {
				end = soon
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	_ = a.journal(h, journalRec{MessageID: in.MessageID, AttemptID: in.AttemptID, State: jIndeterminate, Detail: "no evidence within timeout"})
	return indeterminate(fmt.Sprintf("submitted but no acceptance evidence within %s; reconcile later (never auto-replayed)", a.o.DeliverTimeout)), nil
}

// gate checks whether input can be pasted now (refusal nil) or why not.
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
		r := rejected("agy has exited")
		return screenInfo{}, &r, nil
	}
	cs, err := a.srv.ListClients(ctx, h.TmuxTarget)
	if err != nil {
		return screenInfo{}, nil, contracts.Errorf(contracts.ErrUnavailable, "tmux list-clients: %v", err)
	}
	if n := tmux.WritableClients(cs); n > 0 {
		r := queued(fmt.Sprintf("human attached to the terminal (%d client(s)); input lease", n))
		return screenInfo{}, &r, nil
	}
	st, reason, sip := a.current(ctx, h)
	if sip == nil {
		return screenInfo{}, nil, contracts.Errorf(contracts.ErrUnavailable, "capture-pane failed")
	}
	si := *sip
	switch {
	case si.Dialog != "":
		r := queued("dialog on screen: " + si.Dialog)
		return si, &r, nil
	case st != contracts.RunWaitingInput:
		r := queued(fmt.Sprintf("runtime not ready for input: %s (%s)", st, reason))
		return si, &r, nil
	case !si.HasInputBox:
		r := queued("input box not visible")
		return si, &r, nil
	case !si.InputEmpty():
		why := "input box has an unsent draft (not Pyre's); not pasting over it"
		if id := a.leftoverDraft(h, si); id != "" {
			why = "input box holds Pyre's unsent draft of " + id + " (its delivery is indeterminate); not pasting over it. " + draftAction(id)
		}
		r := queued(why)
		return si, &r, nil
	}
	return si, nil, nil
}

// InputReady implements delivery.InputReadiness: the paste gate without
// pasting (false: keep the mail queued without claiming an attempt).
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

// leftoverDraft names the message whose unsent paste the box shows (the
// latest indeterminate or rejected attempt without evidence).
func (a *Adapter) leftoverDraft(h contracts.RunHandle, si screenInfo) string {
	best := journalRec{}
	for id, r := range lastAttempts(h.StateDir) {
		if r.State != jIndeterminate && r.State != jRejected {
			continue
		}
		if !si.showsMarker(id) {
			continue
		}
		if best.MessageID == "" || r.At.After(best.At) {
			best = r
		}
	}
	if best.MessageID == "" {
		return ""
	}
	if _, ok := a.evidenceFor(h, best.MessageID); ok {
		return ""
	}
	return best.MessageID
}

// Reconcile looks for evidence of an earlier attempt. Evidence: accepted.
// The draft still in the box: never submitted, cleared, rejected. Enter
// never pressed and the box empty: rejected. Otherwise indeterminate.
// Never replays.
func (a *Adapter) Reconcile(ctx context.Context, h contracts.RunHandle, messageID string) (contracts.DeliveryResult, error) {
	if !safeIDRe.MatchString(messageID) {
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
	last, attempted := lastAttempts(h.StateDir)[messageID]
	if ev, ok := a.evidenceFor(h, messageID); ok {
		if attempted && last.State != jAccepted {
			_ = a.journal(h, journalRec{MessageID: messageID, AttemptID: last.AttemptID, State: jAccepted, Evidence: string(contracts.EvidenceAgyUserInput), Ref: ev.Ref, Detail: "reconciled"})
		}
		return accepted(ev, "reconciled from evidence"), nil
	}
	if !attempted {
		return rejected("no attempt recorded for this run (never pasted); safe to deliver"), nil
	}
	switch last.State {
	case jRejected, jCleared:
		return rejected("previous attempt known not submitted: " + last.Detail), nil
	}
	si, err := a.screen(ctx, h.TmuxTarget)
	if err != nil {
		return indeterminate("screen unavailable: " + err.Error()), nil
	}
	// Only a draft that shows this message's own marker is cleared: a tail
	// match alone could be another message with the same ending (a
	// reissue), and clearing it would wrongly prove this one unsent.
	if si.showsMarker(messageID) {
		if cs, cerr := a.srv.ListClients(ctx, h.TmuxTarget); cerr != nil || tmux.WritableClients(cs) > 0 {
			return indeterminate("unsent draft of this message is in the input box but a human is attached; not clearing. " + draftAction(messageID)), nil
		}
		if a.clearInput(ctx, h.TmuxTarget) {
			_ = a.journal(h, journalRec{MessageID: messageID, AttemptID: last.AttemptID, State: jCleared, Detail: "unsent draft cleared by reconcile"})
			return rejected("draft of this message was in the input box (never submitted); cleared; safe to retry"), nil
		}
		return indeterminate("draft of this message visible but could not be cleared. " + draftAction(messageID)), nil
	}
	if last.State == jIndeterminate && last.NoEnter && si.InputEmpty() {
		if cs, cerr := a.srv.ListClients(ctx, h.TmuxTarget); cerr == nil && tmux.WritableClients(cs) == 0 {
			_ = a.journal(h, journalRec{MessageID: messageID, AttemptID: last.AttemptID, State: jRejected, Detail: "Enter was never pressed and the input box is empty: not submitted"})
			return rejected("Enter was never pressed and the input box is empty: not submitted; safe to retry"), nil
		}
	}
	return indeterminate(fmt.Sprintf("attempt %s (%s) has no evidence and no visible draft; may have been submitted — resolve explicitly", last.AttemptID, last.State)), nil
}

// clearInput empties the input box (Ctrl-U clears a line; Backspace joins
// lines of a multi-line draft).
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
