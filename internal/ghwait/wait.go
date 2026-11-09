package ghwait

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"
)

// ErrorGrace is how long a transient error (network, rate limit) may last
// before the wait gives up with an error message.
const ErrorGrace = 10 * time.Minute

// SettleDelay is how much later a second poll must see the same complete
// state before checks-done or checks-fail is reported.
const SettleDelay = 20 * time.Second

// Outcome is how a wait ended.
type Outcome struct {
	Kind string `json:"kind"` // met, timeout, error
	Text string `json:"text"` // the message sent
}

// WaitState is a wait's progress on disk, so a relaunch after a restart
// or reboot keeps its start (the timeout and "since" for reviews and
// comments), the head it started on, and ends at once if it already ended.
type WaitState struct {
	Start    time.Time `json:"start"`
	After    time.Time `json:"after,omitempty"` // --after`
	BaseHead string    `json:"base_head,omitempty"`
	ErrSince time.Time `json:"err_since,omitempty"`
	Done     string    `json:"done,omitempty"`
	Outcome  *Outcome  `json:"outcome,omitempty"`
}

// Waiter runs one wait to its end.
type Waiter struct {
	Target  Target
	Cond    Cond
	Timeout time.Duration
	Poller  *Poller
	Now     func() time.Time
	Sleep   func(context.Context, time.Duration) error
	// Deliver sends the one message; an error is retried next poll.
	Deliver func(ctx context.Context, text string) error
	Log     io.Writer
	// Load/Save keep WaitState across a relaunch (optional).
	Load func() (WaitState, bool)
	Save func(WaitState)
	// After (--after): failures that finished before it count as being
	// rerun (checks-done, checks-fail).
	After time.Time
	// ErrorGrace and Settle override the package defaults (tests).
	ErrorGrace, Settle time.Duration
	// MinSleep and MaxSleep bound the time between local reads of the
	// shared snapshot (default 5 s and 60 s).
	MinSleep, MaxSleep time.Duration
}

// Label is "gh-pr owner/repo#N --until cond".
func (w *Waiter) Label() string {
	return fmt.Sprintf("%s %s --until %s", w.Target.Kind, w.Target, w.Cond)
}

func (w *Waiter) logf(format string, a ...any) {
	if w.Log != nil {
		fmt.Fprintf(w.Log, w.Now().Format("15:04:05")+" "+format+"\n", a...)
	}
}

// Run polls until the condition is met, the timeout passes or an error
// persists, and delivers one message. It returns the outcome.
func (w *Waiter) Run(ctx context.Context) (Outcome, error) {
	grace := w.ErrorGrace
	if grace <= 0 {
		grace = ErrorGrace
	}
	minS, maxS := w.MinSleep, w.MaxSleep
	if minS <= 0 {
		minS = 5 * time.Second
	}
	if maxS <= 0 {
		maxS = time.Minute
	}
	settle := w.Settle
	if settle <= 0 {
		settle = SettleDelay
	}
	st := WaitState{Start: w.Now(), After: w.After}
	if w.Load != nil {
		if old, ok := w.Load(); ok && !old.Start.IsZero() {
			st = old
			if st.Done != "" {
				w.logf("done: this wait already ended (%s); not restarted", st.Done)
				if st.Outcome != nil {
					return *st.Outcome, nil
				}
				return Outcome{Kind: "done", Text: st.Done}, nil
			}
			w.logf("relaunched: waiting since %s", st.Start.Local().Format("Jan 2 15:04"))
		}
	}
	save := func() {
		if w.Save != nil {
			w.Save(st)
		}
	}
	save()
	head := "[pyre wait " + w.Label() + "] "
	var pending *Outcome // decided but not yet delivered
	lastState := "no data yet"
	// A met result waiting for its settle confirmation: its state and the
	// fetch time of the snapshot it was seen on.
	var candKey string
	var candAt time.Time
	for {
		if pending == nil {
			var notBefore time.Time
			if candKey != "" {
				notBefore = candAt.Add(settle)
			}
			snap, info, err := w.Poller.PollFresh(ctx, w.Target, notBefore)
			now := w.Now()
			switch {
			case ctx.Err() != nil:
				return Outcome{}, ctx.Err()
			case err != nil:
				var pe *PollError
				perm := errors.As(err, &pe) && pe.Permanent
				if st.ErrSince.IsZero() {
					st.ErrSince = now
				}
				w.logf("error (%s): %v", map[bool]string{true: "needs a person", false: "retrying"}[perm], err)
				if perm || now.Sub(st.ErrSince) >= grace {
					why := "GitHub answered"
					if !perm {
						why = fmt.Sprintf("GitHub kept failing for %s", now.Sub(st.ErrSince).Round(time.Second))
					}
					pending = &Outcome{Kind: "error", Text: fmt.Sprintf("%sstopped: %s: %v\n%s\nLast state: %s", head, why, err, w.Target.URL(), lastState)}
				}
			default:
				st.ErrSince = time.Time{}
				if snap.PR != nil && st.BaseHead == "" {
					st.BaseHead = snap.PR.Pull.Head.SHA
				}
				r := Eval(w.Cond, snap, WaitCtx{Start: st.Start, Now: now, BaseHead: st.BaseHead, After: st.After})
				src := "shared snapshot"
				if info.Fetched {
					src = "fetched"
				}
				if r.Met && r.Settle != "" {
					at := snap.Fetched
					if at.IsZero() {
						at = now
					}
					switch {
					case r.Settle != candKey:
						// The window starts when this waiter saw it, not when
						// the (maybe shared, older) snapshot was fetched: the
						// confirmation must be a fetch made settle after now.
						candKey, candAt, r.Met = r.Settle, now, false
						w.logf("looks met (%s): %s; confirming in %s", src, r.State, settle)
						lastState = r.State
					case at.Before(candAt.Add(settle)):
						r.Met = false
					}
				} else {
					candKey = ""
				}
				w.logf("%s (%s): %s", map[bool]string{true: "met", false: "waiting"}[r.Met], src, r.State)
				lastState = r.State
				if r.Met {
					pending = &Outcome{Kind: "met", Text: head + "condition met:\n" + r.Text}
				}
			}
			if pending == nil && w.Timeout > 0 && now.Sub(st.Start) >= w.Timeout {
				pending = &Outcome{Kind: "timeout", Text: fmt.Sprintf("%stimed out after %s. Last state: %s\n%s", head, w.Timeout, lastState, w.Target.URL())}
			}
			save()
			if pending == nil {
				d := info.Next.Sub(now)
				d = min(max(d, minS), maxS)
				if candKey != "" && err == nil {
					d = max(candAt.Add(settle).Sub(now), time.Second)
				}
				if err := w.Sleep(ctx, d); err != nil {
					return Outcome{}, err
				}
				continue
			}
		}
		if err := w.Deliver(ctx, pending.Text); err != nil {
			w.logf("%s; delivery failed (retrying in %s): %v", pending.Kind, minS, err)
			if err := w.Sleep(ctx, max(minS, 30*time.Second)); err != nil {
				return Outcome{}, err
			}
			continue
		}
		w.logf("done: %s, message delivered", pending.Kind)
		st.Done, st.Outcome = pending.Kind, pending
		save()
		return *pending, nil
	}
}
