package codex

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/tmux"
)

// Codex's "Hooks need review" screen in a run's TUI (0.166,
// iss_…):
//
//	Hooks need review
//	1 hook is new or changed.
//	Hooks can run outside the sandbox after you trust them.
//	› 1. Review hooks
//	  2. Trust all and continue
//	  3. Continue without trusting (hooks won't run)
//	enter confirm · esc skip
//
// The TUI opens it whenever a hook is new or changed (a plugin update
// changes its hook's hash; Codex 0.166 also hashes the script a hook
// runs). The app-server runs the thread's turns either way (untrusted
// hooks just do not run), but the terminal the owner or a parent looks at
// is stuck on the screen, and a turn that ended underneath it was reported
// as a plain "finished". So while the screen is up the run reports
// waiting_approval ("needs human: Codex hook review") with a pending
// approval offering "Trust all and continue" / "Continue without
// trusting"; answering it moves the TUI's selection there and presses
// Enter. Pyre never answers it by itself: trusting a hook is the owner's
// call (Pyre only mirrors trust the owner already gave, hooktrust.go).
//
// Watching costs a pane capture: every hookReviewFast while the launch's
// hooks/list named untrusted hooks (for hookReviewHintWindow after a TUI
// start) or the screen is up, otherwise every hookReviewSlow, in the
// background.

const hookReviewRequestPrefix = "tui:hook-review:"

var (
	hookReviewFast       = 1500 * time.Millisecond
	hookReviewSlow       = 20 * time.Second
	hookReviewHintWindow = 3 * time.Minute
)

// hookReviewOptions are the screen's rows (1-based) Pyre offers.
var hookReviewOptions = []struct {
	id, label, kind, match string
}{
	{"trust_all", "Trust all and continue", "allow", "trust all"},
	{"continue", "Continue without trusting (hooks won't run)", "deny", "continue without trusting"},
}

// hookReviewScreen is a parsed "Hooks need review" screen.
type hookReviewScreen struct {
	Detail   string   // "1 hook is new or changed."
	Rows     []string // option rows in order, lower-cased, numbering dropped
	Selected int      // index into Rows of the highlighted row (-1 unknown)
}

// parseHookReviewScreen reads a captured TUI screen; ok is false when it is
// not the hooks review screen.
func parseHookReviewScreen(screen string) (hookReviewScreen, bool) {
	if !strings.Contains(screen, "Hooks need review") || !strings.Contains(strings.ToLower(screen), "continue without trusting") {
		return hookReviewScreen{}, false
	}
	s := hookReviewScreen{Selected: -1}
	for _, line := range strings.Split(screen, "\n") {
		l := strings.TrimSpace(line)
		if s.Detail == "" && strings.Contains(l, "new or changed") {
			s.Detail = l
			continue
		}
		sel := false
		for _, m := range []string{"›", ">", "❯"} {
			if rest, ok := strings.CutPrefix(l, m); ok {
				l, sel = strings.TrimSpace(rest), true
				break
			}
		}
		// "1. Review hooks"
		num, rest, ok := strings.Cut(l, ". ")
		if !ok || len(num) != 1 || num[0] < '1' || num[0] > '9' {
			continue
		}
		if sel {
			s.Selected = len(s.Rows)
		}
		s.Rows = append(s.Rows, strings.ToLower(strings.TrimSpace(rest)))
	}
	return s, len(s.Rows) > 0
}

// row is the index of the row starting with match (-1 none).
func (s hookReviewScreen) row(match string) int {
	for i, r := range s.Rows {
		if strings.HasPrefix(r, match) {
			return i
		}
	}
	return -1
}

// hookReview is a run's hook-review watch (runRef.hooks).
type hookReview struct {
	mu        sync.Mutex
	checking  bool
	lastCheck time.Time
	untrusted []string  // launch hooks/list: hooks still untrusted
	hintUntil time.Time // capture every hookReviewFast until then
	up        bool      // the screen is showing
	gen       int       // counts screens seen (request id)
	since     time.Time
	detail    string
	answered  string              // option Pyre sent for the current screen
	resolved  *contracts.Approval // a closed screen's approval, reported once
}

// noteUntrusted records the hooks a launch found untrusted.
func (hr *hookReview) noteUntrusted(hooks []string) {
	hr.mu.Lock()
	hr.untrusted = hooks
	hr.mu.Unlock()
}

// tuiStarted: a (re)started TUI shows the screen again while hooks are
// untrusted, so watch closely for a while.
func (hr *hookReview) tuiStarted(now time.Time) {
	hr.mu.Lock()
	if len(hr.untrusted) > 0 {
		hr.hintUntil = now.Add(hookReviewHintWindow)
	}
	hr.mu.Unlock()
}

// checkHookReview captures the run's TUI when due: synchronously while it
// is watched closely (so the state of the observation that follows is
// right), else in the background.
func (a *Adapter) checkHookReview(ctx context.Context, rr *runRef, now time.Time) {
	if a.o.DisableTUI {
		return
	}
	hr := &rr.hooks
	hr.mu.Lock()
	fast := hr.up || now.Before(hr.hintUntil)
	every := hookReviewSlow
	if fast {
		every = hookReviewFast
	}
	if hr.checking || (!hr.lastCheck.IsZero() && now.Sub(hr.lastCheck) < every) {
		hr.mu.Unlock()
		return
	}
	hr.checking, hr.lastCheck = true, now
	hr.mu.Unlock()
	capture := func(ctx context.Context) {
		cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		screen, err := a.srv.CapturePane(cctx, rr.meta.RunID, tmux.CaptureOptions{})
		gone := err != nil && (errors.Is(err, tmux.ErrNoSession) || errors.Is(err, tmux.ErrNoServer))
		hr.mu.Lock()
		defer hr.mu.Unlock()
		hr.checking = false
		if err != nil && !gone {
			return
		}
		s, ok := parseHookReviewScreen(screen)
		hr.observe(rr, s, ok && !gone, a.o.Now())
	}
	if fast {
		capture(ctx)
		return
	}
	go capture(context.Background())
}

// observe applies one capture. Caller holds hr.mu.
func (hr *hookReview) observe(rr *runRef, s hookReviewScreen, showing bool, now time.Time) {
	switch {
	case showing && !hr.up:
		hr.up, hr.gen, hr.since, hr.answered, hr.detail = true, hr.gen+1, now, "", s.Detail
	case showing:
		hr.detail = s.Detail
	case hr.up:
		ap := hr.approval(rr)
		ap.State = contracts.ApprovalResolved
		res := &contracts.ApprovalResolution{ResolvedAt: now, Source: "native", Effect: "effective", Reason: "answered_in_terminal"}
		if hr.answered != "" {
			res.OptionID, res.Source, res.Reason = hr.answered, "pyre", ""
		}
		ap.Resolution = res
		hr.up, hr.resolved, hr.hintUntil = false, &ap, time.Time{}
	}
}

// approval is the current screen's approval (pending). Caller holds hr.mu.
func (hr *hookReview) approval(rr *runRef) contracts.Approval {
	rid := fmt.Sprintf("%s%d", hookReviewRequestPrefix, hr.gen)
	body := "Codex's terminal is showing \"Hooks need review\""
	if hr.detail != "" {
		body += ": " + hr.detail
	}
	if len(hr.untrusted) > 0 {
		body += " Untrusted: " + strings.Join(hr.untrusted, "; ") + "."
	}
	body += " The run keeps working meanwhile, but these hooks do not run until trusted. Trusting runs them outside the sandbox from now on; trusting them in your own Codex (~/.codex) also lets Pyre trust them in its account profiles."
	ap := contracts.Approval{
		ApprovalID: approvalID(rr.meta.RunID, rid), SessionID: rr.meta.SessionID, RunID: rr.meta.RunID, RequestID: rid,
		Kind: contracts.ApprovalPermission, Title: "Codex hooks need review", Body: redact(body),
		CreatedAt: hr.since, State: contracts.ApprovalPending,
	}
	for _, o := range hookReviewOptions {
		ap.Options = append(ap.Options, contracts.ApprovalOption{ID: o.id, Label: o.label, Kind: o.kind})
	}
	return ap
}

// observeHookReview adds the screen to an observation: a pending approval
// and waiting_approval while it is up, its resolution once after.
func (rr *runRef) observeHookReview(obs *contracts.Observation) {
	hr := &rr.hooks
	hr.mu.Lock()
	defer hr.mu.Unlock()
	if hr.resolved != nil {
		obs.Approvals = append(obs.Approvals, *hr.resolved)
		hr.resolved = nil
	}
	if !hr.up {
		return
	}
	ap := hr.approval(rr)
	if hr.answered != "" {
		return // answered; the next capture closes it
	}
	obs.Approvals = append(obs.Approvals, ap)
	reason := "needs human: Codex hook review"
	if hr.detail != "" {
		reason += " (" + strings.TrimSuffix(hr.detail, ".") + ")"
	}
	obs.State, obs.StateReason = contracts.RunWaitingApproval, reason+"; answer the approval or attach to its terminal"
}

// answerHookReview answers the screen in the TUI: it moves the selection
// to the chosen row and presses Enter, then waits for the screen to close.
func (a *Adapter) answerHookReview(ctx context.Context, rr *runRef, ap contracts.Approval, optionID string) (contracts.CommandReceipt, error) {
	fail := func(kind contracts.ErrorCode, f string, args ...any) (contracts.CommandReceipt, error) {
		return a.receipt(contracts.OutcomeFailed, false, ""), contracts.Errorf(kind, f, args...)
	}
	match := ""
	for _, o := range hookReviewOptions {
		if o.id == optionID {
			match = o.match
		}
	}
	switch optionID {
	case "allow", "accept":
		match = hookReviewOptions[0].match
		optionID = hookReviewOptions[0].id
	case "deny", "decline":
		match = hookReviewOptions[1].match
		optionID = hookReviewOptions[1].id
	}
	if match == "" {
		return fail(contracts.ErrInvalid, "option %q not offered", optionID)
	}
	hr := &rr.hooks
	hr.mu.Lock()
	cur := fmt.Sprintf("%s%d", hookReviewRequestPrefix, hr.gen)
	if !hr.up || ap.RequestID != cur || hr.answered != "" {
		hr.mu.Unlock()
		return fail(contracts.ErrConflict, "the hooks review screen %s is no longer showing in run %s", ap.RequestID, rr.meta.RunID)
	}
	hr.mu.Unlock()
	screen, err := a.srv.CapturePane(ctx, rr.meta.RunID, tmux.CaptureOptions{})
	if err != nil {
		return fail(contracts.ErrUnavailable, "capture the TUI: %v", err)
	}
	s, ok := parseHookReviewScreen(screen)
	if !ok {
		return fail(contracts.ErrConflict, "the hooks review screen is no longer showing in run %s", rr.meta.RunID)
	}
	target := s.row(match)
	if target < 0 || s.Selected < 0 {
		return fail(contracts.ErrConflict, "the hooks review screen has no %q row Pyre can select", match)
	}
	var keys []tmux.Key
	for i := s.Selected; i < target; i++ {
		keys = append(keys, tmux.KeyDown)
	}
	for i := s.Selected; i > target; i-- {
		keys = append(keys, tmux.KeyUp)
	}
	if len(keys) > 0 {
		if err := a.srv.SendKeys(ctx, rr.meta.RunID, keys...); err != nil {
			return a.receipt(contracts.OutcomeFailed, false, "move selection: "+err.Error()), nil
		}
		time.Sleep(150 * time.Millisecond)
		// Enter only on the chosen row: a lost key must not confirm another.
		screen, err = a.srv.CapturePane(ctx, rr.meta.RunID, tmux.CaptureOptions{})
		if s, ok = parseHookReviewScreen(screen); err != nil || !ok || s.Selected != target {
			return a.receipt(contracts.OutcomeFailed, false, "the selection did not reach "+match), nil
		}
	}
	if err := a.srv.SendKeys(ctx, rr.meta.RunID, tmux.KeyEnter); err != nil {
		return a.receipt(contracts.OutcomeIndeterminate, true, "send Enter: "+err.Error()), nil
	}
	hr.mu.Lock()
	hr.answered = optionID
	hr.mu.Unlock()
	end := time.Now().Add(3 * time.Second)
	for time.Now().Before(end) {
		time.Sleep(200 * time.Millisecond)
		screen, err := a.srv.CapturePane(ctx, rr.meta.RunID, tmux.CaptureOptions{})
		if err != nil {
			continue
		}
		if _, ok := parseHookReviewScreen(screen); !ok {
			hr.mu.Lock()
			hr.observe(rr, hookReviewScreen{}, false, a.o.Now())
			hr.mu.Unlock()
			return a.receipt(contracts.OutcomeSucceeded, true, "effect=effective; hooks review screen closed"), nil
		}
	}
	return a.receipt(contracts.OutcomeDelivered, true, "effect=submitted; the hooks review screen has not closed yet"), nil
}
