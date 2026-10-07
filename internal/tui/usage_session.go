package tui

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/usage"
)

// Session account facts: the run's account with its live headroom, and
// the account switches Pyre made mid-session (handoff on a limit).

// accountCard is the card of the run's account on the run's machine.
func (m *Model) accountCard(s contracts.SessionView) (usage.Card, bool) {
	if s.Run == nil || s.Run.AccountID == "" {
		return usage.Card{}, false
	}
	var any *usage.Card
	for _, c := range m.usageCards() {
		if c.Account == nil || c.Account.AccountID != s.Run.AccountID {
			continue
		}
		if c.NodeID == s.Run.NodeID {
			return c, true
		}
		if any == nil {
			cc := c
			any = &cc
		}
	}
	if any != nil {
		return *any, true
	}
	return usage.Card{}, false
}

func (m *Model) accountLabel(id string) string {
	if a := m.account(id); a != nil && a.Account.Label != "" {
		return a.Account.Label
	}
	return id
}

// headroomText is "5h 12% · week 40%", toned like the bars.
func (m *Model) headroomText(c usage.Card) string {
	if c.Obs == nil {
		return ""
	}
	sp := usage.Split(c.Obs.Windows)
	var parts []string
	for _, x := range []struct {
		label string
		w     *contracts.UsageWindow
	}{{"5h", sp.FiveHour}, {"week", sp.Weekly}} {
		if x.w == nil {
			continue
		}
		s := fmt.Sprintf("%s %.0f%%", x.label, x.w.UsedPercent)
		switch {
		case c.LastKnown:
			s = m.th.dim.Render(s)
		case x.w.UsedPercent >= 100:
			s = m.th.tone(toneBad, s)
		case x.w.UsedPercent >= usage.NearPercent:
			s = m.th.tone(toneWarn, s)
		}
		parts = append(parts, s)
	}
	out := strings.Join(parts, m.th.dim.Render(" · "))
	if c.LastKnown && out != "" {
		out += m.th.dim.Render(" (last known)")
	}
	return out
}

// accountFact is the info overlay's Account value: "dev-server anthropic ·
// ● healthy · 5h 12% · week 40%".
func (m *Model) accountFact(s contracts.SessionView) string {
	if s.Run == nil || s.Run.AccountID == "" {
		return m.th.dim.Render("your own login (not managed by Pyre)")
	}
	c, ok := m.accountCard(s)
	if !ok {
		return s.Run.AccountID
	}
	out := c.Label() + m.th.dim.Render(" "+string(c.Provider)) + m.th.dim.Render(" · ") + m.th.status(usageStatus(c.Assessment))
	if c.State == usage.AtLimit && c.Until != nil {
		out += m.th.dim.Render(" until " + usage.Clock(m.now(), *c.Until))
	}
	if h := m.headroomText(c); h != "" {
		out += m.th.dim.Render(" · ") + h
	}
	return out
}

// accountHeadline is the account in the terminal header with its usage at
// a glance: "me 5h 12% wk 40%" (amber near the limit, red at it), or "at
// limit" / "re-auth" (usageBadge, sessions_modes.go). The sidenav rows have
// no room for it at 120 columns, so the header carries it.
func (m *Model) accountHeadline(s contracts.SessionView) string {
	c, ok := m.accountCard(s)
	if !ok {
		return s.Run.AccountID
	}
	if b := m.usageBadge(c); b != "" {
		return c.Label() + " " + b
	}
	return c.Label()
}

// ---- account switches ----

func switchesKey(sid string) string { return "fallbacks:" + sid }

// loadSwitches loads the session's fallback plans (account.fallback).
func (m *Model) loadSwitches(sid string) tea.Cmd {
	api := m.api
	return m.ensure(switchesKey(sid), 30*time.Second, func(ctx context.Context) (any, error) {
		return all(ctx, func(ctx context.Context, cur string) (contracts.Page[contracts.FallbackPlan], error) {
			return api.ListFallbacks(ctx, contracts.ListFallbacksRequest{PageRequest: contracts.PageRequest{Cursor: cur}, SessionID: sid})
		})
	})
}

// switchCause words a plan's reason: "limit reached" or "login unusable".
func switchCause(reason string) string {
	r := strings.ToLower(reason)
	if strings.Contains(r, "auth") || strings.Contains(r, "unusable") || strings.Contains(r, "login") && !strings.Contains(r, "limit") {
		return "login unusable"
	}
	return "limit reached"
}

// switchLine renders one plan: "switched from alice-server to dev-server at
// 3:12 PM (limit reached)".
func (m *Model) switchLine(p contracts.FallbackPlan) string {
	at := usage.Clock(m.now(), p.PlannedAt)
	from, cause := m.accountLabel(p.FromAccountID), switchCause(p.Reason)
	switch p.Outcome {
	case contracts.FallbackNativeResume:
		return fmt.Sprintf("switched from %s to %s at %s (%s)", from, m.accountLabel(p.ToAccountID), at, cause)
	case contracts.FallbackHandoff:
		return fmt.Sprintf("handed off from %s to %s at %s (%s; a new thread with a handoff note)", from, m.accountLabel(p.ToAccountID), at, cause)
	case contracts.FallbackWaiting:
		return fmt.Sprintf("%s on %s at %s; waiting for an eligible account", cause, from, at)
	}
	return fmt.Sprintf("%s on %s at %s; needs you", cause, from, at)
}

// accountSwitchLines are the session's account switches, oldest first,
// under the Account fact.
func (m *Model) accountSwitchLines(s contracts.SessionView, lw, w int) []string {
	plans, st := get[[]contracts.FallbackPlan](m, switchesKey(s.Session.SessionID))
	if !st.ok() || len(plans) == 0 {
		return nil
	}
	plans = append([]contracts.FallbackPlan(nil), plans...)
	sort.SliceStable(plans, func(i, j int) bool { return plans[i].PlannedAt.Before(plans[j].PlannedAt) })
	var out []string
	for _, p := range plans {
		tn := toneNone
		switch p.Outcome {
		case contracts.FallbackWaiting:
			tn = toneWarn
		case contracts.FallbackIntervention:
			tn = toneBad
		}
		for i, l := range wrap(m.switchLine(p), max(10, w-lw-2)) {
			lead := "↳ "
			if i > 0 {
				lead = "  "
			}
			out = append(out, strings.Repeat(" ", lw)+m.th.dim.Render(lead)+m.th.tone(tn, l))
		}
	}
	return out
}
