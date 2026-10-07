package tui

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/jhh3/pyre/internal/contracts"
)

// Input lease and starting-run handling for the embedded terminal.
//
//   - A lease held by another of the owner's own clients (an owner principal
//     on another machine) is offered for takeover: the pane says where you are
//     typing, and enter (or a click) takes it over through the contract's
//     takeover path. The other client sees a newer lease and drops to
//     watching. A lease held by anyone else is explained inline.
//   - A focused pane gives its lease up after LeaseIdle without keys and takes
//     it back on the next key, so an idle pane never blocks another client.
//   - A run that is still starting is retried (0.5s doubling to 2s) and on
//     its state change, with a calm "starting…" instead of an error.

const askTakeoverFor = 30 * time.Second // how long a takeover offer stands

type termRetryMsg struct {
	sid string
	tok int
}

// leaseEvery is the renew/idle-check interval for a held lease.
func (m *Model) leaseEvery() time.Duration {
	return max(10*time.Millisecond, min(leaseRenew, m.opts.LeaseIdle/2))
}

func (m *Model) leaseTick(sid, leaseID string) tea.Cmd {
	return tea.Tick(m.leaseEvery(), func(time.Time) tea.Msg { return leaseTickMsg{sid: sid, leaseID: leaseID} })
}

// leaseConflict reports whether err refuses a lease (rather than, say, a
// run that is not live), with the lease named in it when there is one.
func leaseConflict(err error) (contracts.InputLease, bool) {
	var ce *contracts.Error
	if !errors.As(err, &ce) || ce.Code != contracts.ErrConflict {
		return contracts.InputLease{}, false
	}
	var l contracts.InputLease
	if v, ok := ce.Details["lease"]; ok {
		if b, err := json.Marshal(v); err == nil {
			_ = json.Unmarshal(b, &l)
		}
	}
	if l.LeaseID != "" || (ce.Ref != nil && ce.Ref.Kind == contracts.KindLease) || strings.Contains(ce.Message, "leased by") {
		return l, true
	}
	return l, false
}

func holderName(p contracts.Principal) string {
	if p.Name != "" {
		return p.Name
	}
	if p.ID != "" {
		return p.ID
	}
	return "another client"
}

// startingConflict reports whether err is an attach refused because the
// run has not finished starting.
func (m *Model) startingConflict(sid string, err error) bool {
	if !contracts.IsCode(err, contracts.ErrConflict) {
		return false
	}
	if _, lease := leaseConflict(err); lease {
		return false
	}
	if strings.Contains(err.Error(), string(contracts.RunStarting)) {
		return true
	}
	s, ok := m.session(sid)
	return ok && s.Run != nil && s.Run.State == contracts.RunStarting
}

// focusRefused explains a lease refusal inline (and in a toast). The
// owner's own other client is offered for takeover: the next enter or click
// takes it over.
func (m *Model) focusRefused(tv *termView, err error) tea.Cmd {
	l, _ := leaseConflict(err)
	if l.LeaseID == "" {
		if s, ok := m.session(tv.sid); ok && s.Run != nil && s.Run.Lease != nil {
			l = *s.Run.Lease
		}
	}
	name := holderName(l.Holder)
	if l.Holder.Kind == contracts.PrincipalOwner {
		tv.askTakeover, tv.askUntil, tv.focusNote = name, time.Now().Add(askTakeoverFor), ""
		return m.setToast("You're typing in this session on "+name+" — take over here? (enter)", toneWarn)
	}
	until := ""
	if !l.ExpiresAt.IsZero() {
		until = " until " + l.ExpiresAt.Local().Format("15:04")
	}
	tv.askTakeover = ""
	tv.focusNote = "typing: " + name + until
	return m.setToast("cannot type here: input leased by "+name+until+"; messages queue (pyre attach --takeover takes it over)", toneWarn)
}

// takeoverOffered reports whether a takeover offer for tv still stands.
func (tv *termView) takeoverOffered() bool {
	return tv.askTakeover != "" && time.Now().Before(tv.askUntil)
}

// leaseLost drops tv to watching after another client took its lease.
func (m *Model) leaseLost(tv *termView, by string) tea.Cmd {
	tv.leaseID, tv.leaseIdle, tv.retaking, tv.pending = "", false, false, nil
	tv.holdUntil = time.Time{}
	tv.focusNote, tv.askTakeover = by+" took over · enter to take back", ""
	if m.wantsInteractive(tv.sid) {
		m.focus = 0 // reconcileTerms returns the pane to watching
	}
	return m.setToast(by+" took over typing in this session; watching here", toneWarn)
}

// leaseWatch runs after every update: a newer lease by someone else means
// ours was taken over; a run that stopped starting attaches at once.
func (m *Model) leaseWatch() tea.Cmd {
	var cmds []tea.Cmd
	now := m.now()
	for _, tv := range m.term.views {
		s, ok := m.session(tv.sid)
		if !ok || s.Run == nil || s.Run.RunID != tv.runID {
			continue
		}
		if l := s.Run.Lease; l != nil && l.Active(now) && l.LeaseID != tv.leaseID && !tv.leaseAt.IsZero() && l.AcquiredAt.After(tv.leaseAt) &&
			(tv.leaseID != "" || (tv.leaseIdle && m.keepsInteractive(tv))) {
			cmds = append(cmds, m.leaseLost(tv, holderName(l.Holder)))
		}
		if st, _, _ := tv.status(); st == termStarting && string(s.Run.State) != tv.startState && m.view == viewSessions && m.sessList.sel == tv.sid {
			tv.startState = string(s.Run.State) // once per state change
			tv.unavailableUntil = time.Time{}
			cmds = append(cmds, m.retryTerm(s, tv))
		}
	}
	return tea.Batch(cmds...)
}

// termStarting shows "starting…" and retries the attach with backoff.
func (m *Model) termStarting(tv *termView, interactive bool) tea.Cmd {
	wait := tv.startWait * 2
	if wait <= 0 {
		wait = m.opts.StartingRetry
	}
	wait = min(wait, 4*m.opts.StartingRetry)
	tv.startWait = wait
	tv.unavailableUntil = time.Now().Add(wait)
	if !interactive {
		tv.reqMode = termReadOnly
	}
	if live, _ := tv.live(); !live {
		tv.setState(termStarting, "")
	}
	if s, ok := m.session(tv.sid); ok && s.Run != nil {
		tv.startState = string(s.Run.State)
	}
	sid, tok := tv.sid, tv.reqTok
	return tea.Tick(wait, func(time.Time) tea.Msg { return termRetryMsg{sid: sid, tok: tok} })
}

func (m *Model) onTermRetry(msg termRetryMsg) tea.Cmd {
	tv := m.terms().views[msg.sid]
	if tv == nil || tv.reqTok != msg.tok {
		return nil
	}
	s, ok := m.session(msg.sid)
	if !ok || m.view != viewSessions || m.sessList.sel != msg.sid {
		if st, _, _ := tv.status(); st == termStarting {
			tv.setState(termIdle, "")
		}
		tv.unavailableUntil = time.Time{}
		return nil
	}
	tv.unavailableUntil = time.Time{}
	return m.retryTerm(s, tv)
}

// retryTerm re-requests tv's attach in the mode the pane wants now.
func (m *Model) retryTerm(s contracts.SessionView, tv *termView) tea.Cmd {
	if ok, _ := m.embeddable(s); !ok {
		return nil
	}
	if m.keepsInteractive(tv) {
		return tea.Batch(m.requestTerm(s, tv, termInteractive), m.termWait())
	}
	tv.reqMode = termReadOnly
	return m.openTermView()
}

// leaseTickDue renews a held lease, or gives it up after LeaseIdle without
// keys (the pane stays focused; the next key takes it back).
func (m *Model) onLeaseTick(msg leaseTickMsg) tea.Cmd {
	tv := m.terms().views[msg.sid]
	if tv == nil || tv.leaseID != msg.leaseID {
		return nil
	}
	if tv.holdUntil.IsZero() && time.Since(tv.lastKey) >= m.opts.LeaseIdle {
		m.releaseLease(tv.leaseID)
		tv.leaseID, tv.leaseIdle = "", true
		return nil
	}
	runID, api, leaseID := tv.runID, m.api, tv.leaseID
	return tea.Batch(m.leaseTick(msg.sid, msg.leaseID), m.do(func(ctx context.Context) (any, error) {
		return api.AcquireLease(ctx, contracts.LeaseRequest{Mutation: contracts.Mutation{OperationID: contracts.NewID(contracts.PrefixOperation)}, RunID: runID, TTLSeconds: leaseTTL})
	}, func(m *Model, v any, err error) tea.Cmd {
		if err == nil {
			return nil
		}
		if l, ok := leaseConflict(err); ok {
			if cur := m.terms().views[msg.sid]; cur == tv && tv.leaseID == leaseID {
				return m.leaseLost(tv, holderName(l.Holder))
			}
			return nil
		}
		return m.errToast("could not renew the input lease", err)
	}))
}

// retakeLease takes the lease back for a focused pane that gave it up while
// idle; keys typed meanwhile wait in tv.pending and follow on success.
func (m *Model) retakeLease(tv *termView) tea.Cmd {
	if tv.retaking {
		return nil
	}
	tv.retaking = true
	runID, api := tv.runID, m.api
	return m.do(func(ctx context.Context) (any, error) {
		return api.AcquireLease(ctx, contracts.LeaseRequest{Mutation: contracts.Mutation{OperationID: contracts.NewID(contracts.PrefixOperation)}, RunID: runID, TTLSeconds: leaseTTL})
	}, func(m *Model, v any, err error) tea.Cmd {
		if m.terms().views[tv.sid] != tv || !tv.retaking {
			if l, ok := v.(contracts.InputLease); ok && err == nil {
				m.releaseLease(l.LeaseID)
			}
			return nil
		}
		tv.retaking = false
		if err != nil {
			tv.pending = nil
			if _, ok := leaseConflict(err); ok {
				if m.wantsInteractive(tv.sid) {
					m.focus = 0
				}
				return m.focusRefused(tv, err)
			}
			return m.errToast("could not take the input lease back", err)
		}
		l, _ := v.(contracts.InputLease)
		if !m.keepsInteractive(tv) {
			m.releaseLease(l.LeaseID)
			return nil
		}
		tv.leaseID, tv.leaseAt, tv.leaseIdle = l.LeaseID, l.AcquiredAt, false
		if len(tv.pending) > 0 {
			m.terms().lastKey.Store(time.Now().UnixNano()) // their echo is drawn at once
		}
		for _, b := range tv.pending {
			tv.send(b)
		}
		tv.pending = nil
		return m.leaseTick(tv.sid, tv.leaseID)
	})
}
