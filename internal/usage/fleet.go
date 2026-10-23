package usage

import (
	"sort"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// Fleet utilization (the TUI header): per provider, the mean used percent
// of the 5-hour and the all-models weekly window over the provider's
// enabled subscription accounts that have a current reading.
//
// Each account weighs the same. Pyre has no capacity per account (Plan is
// a free-text label, and the probes report percentages only), so an equal
// weight is the only defensible total; for accounts on one plan it is
// exactly the share of the pooled cap that is used.
//
// The 5-hour mean uses current readings only: a ready or at-limit login
// whose windows are not last known (Assessment.LastKnown, older than
// FreshFor). The weekly mean also takes a good reading up to WeekFreshFor
// old (an account idle in Pyre is re-read only when chosen, and its week
// moves slowly), never a failed probe's carried-over windows (Stale).
// Unknown, re-auth and not-yet-observed accounts are left out and counted
// in Total, so Current < Total says the numbers cover only some accounts.
// A window whose reset already passed counts as empty (as Judge reads it).

// WeekFreshFor is how old a good reading may be for the weekly mean.
const WeekFreshFor = 3 * time.Hour

// WindowUse is one window's fleet mean.
type WindowUse struct {
	// Has: some enabled account of the provider reports the window (a
	// current or a last-known reading). A provider without a 5-hour window
	// (Codex plans that report only the week) has Has false.
	Has bool
	// N is the number of readings averaged; Percent is valid when N > 0.
	N       int
	Percent float64
	// Of is the number of accounts with a reading counted (Current) that
	// report this window: N < Of says this window's mean covers fewer of
	// them (a 5-hour window read over FreshFor ago).
	Of int
}

// Partial reports whether some counted accounts are left out of this
// window's mean.
func (w WindowUse) Partial() bool { return w.N < w.Of }

// Utilization is one provider's fleet utilization.
type Utilization struct {
	Provider         contracts.Provider
	FiveHour, Weekly WindowUse
	// Current is the accounts with a reading counted (in either mean);
	// Total the enabled
	// managed accounts, plus each machine's own agy login (agy reports its
	// own usage; a Claude or Codex own login outside Pyre is not observed
	// and not counted).
	Current, Total int
}

// Partial reports whether some accounts are left out of the mean.
func (u Utilization) Partial() bool { return u.Current < u.Total }

// utilOrder is the header order: Claude, Codex, agy.
var utilOrder = []contracts.Provider{contracts.ProviderAnthropic, contracts.ProviderOpenAI, contracts.ProviderGoogle}

// current reports whether c is a current reading with a 5-hour or weekly
// window.
func current(c Card) bool {
	switch c.State {
	case Healthy, NearLimit, AtLimit, Ambient: // Ambient: a harness-reported reading (agy)
	default:
		return false
	}
	if c.LastKnown || c.Obs == nil {
		return false
	}
	sp := Split(c.Obs.Windows)
	return sp.FiveHour != nil || sp.Weekly != nil
}

// weekOK reports whether c's reading may count for the weekly mean: a good
// probe (not a failed one's carried-over windows) up to WeekFreshFor old,
// on a login that is not disabled or in need of re-auth.
func weekOK(c Card, now time.Time) bool {
	if c.Obs == nil || c.Obs.Stale || now.Sub(c.Obs.ObservedAt) > WeekFreshFor || Split(c.Obs.Windows).Weekly == nil {
		return false
	}
	switch c.State {
	case NeedsReauth, Disabled:
		return false
	}
	switch c.Obs.Eligibility {
	case contracts.EligibleYes, contracts.EligibleExhausted, contracts.EligibleCooldown:
		return true
	}
	return false
}

// Utilizations summarizes cards per provider, in the order Claude, Codex,
// agy (then any other provider). A provider appears when it has an
// enabled account or an ambient login; one with only disabled accounts
// does not.
func Utilizations(cards []Card, now time.Time) []Utilization {
	type acct struct {
		five, week       *contracts.AccountObservation // newest reading counted per window
		hasFive, hasWeek bool
	}
	per := map[contracts.Provider]map[string]*acct{}
	seen := map[contracts.Provider]bool{}
	var order []contracts.Provider
	note := func(p contracts.Provider) {
		if !seen[p] {
			seen[p] = true
			order = append(order, p)
		}
	}
	for _, c := range cards {
		var key string
		switch {
		case c.Account == nil && c.State == Ambient:
			note(c.Provider)
			if contracts.ManagedLogins(harnessFor(c.Provider)) {
				continue // claude/codex own logins: not observed, not counted
			}
			// agy: each machine's own login is the account (one per
			// identity when the login is known).
			key = "own:" + c.NodeID
			if c.Login != nil && c.Login.Email != "" {
				key = "own:" + strings.ToLower(c.Login.Email)
			}
		case c.Account == nil || c.Account.Disabled || c.State == Disabled:
			continue
		default:
			note(c.Provider)
			key = c.Account.AccountID
		}
		if per[c.Provider] == nil {
			per[c.Provider] = map[string]*acct{}
		}
		a := per[c.Provider][key]
		if a == nil {
			a = &acct{}
			per[c.Provider][key] = a
		}
		if c.Obs == nil {
			continue
		}
		sp := Split(c.Obs.Windows)
		a.hasFive = a.hasFive || sp.FiveHour != nil
		a.hasWeek = a.hasWeek || sp.Weekly != nil
		// Usage is per account: the newest counted reading of any of its
		// logins, per window.
		if current(c) && sp.FiveHour != nil && (a.five == nil || c.Obs.ObservedAt.After(a.five.ObservedAt)) {
			a.five = c.Obs
		}
		if (current(c) && sp.Weekly != nil || weekOK(c, now)) && (a.week == nil || c.Obs.ObservedAt.After(a.week.ObservedAt)) {
			a.week = c.Obs
		}
	}
	rank := func(p contracts.Provider) int {
		for i, q := range utilOrder {
			if p == q {
				return i
			}
		}
		return len(utilOrder)
	}
	sort.Slice(order, func(i, j int) bool {
		if ri, rj := rank(order[i]), rank(order[j]); ri != rj {
			return ri < rj
		}
		return order[i] < order[j]
	})
	used := func(w *contracts.UsageWindow) float64 {
		if w.ResetAt != nil && !w.ResetAt.After(now) {
			return 0 // reset since the reading
		}
		return w.UsedPercent
	}
	var out []Utilization
	for _, p := range order {
		u := Utilization{Provider: p, Total: len(per[p])}
		var five, week float64
		for _, a := range per[p] {
			u.FiveHour.Has = u.FiveHour.Has || a.hasFive
			u.Weekly.Has = u.Weekly.Has || a.hasWeek
			if a.five == nil && a.week == nil {
				continue
			}
			u.Current++
			if a.hasFive {
				u.FiveHour.Of++
			}
			if a.hasWeek {
				u.Weekly.Of++
			}
			if a.five != nil {
				five += used(Split(a.five.Windows).FiveHour)
				u.FiveHour.N++
			}
			if a.week != nil {
				week += used(Split(a.week.Windows).Weekly)
				u.Weekly.N++
			}
		}
		if u.FiveHour.N > 0 {
			u.FiveHour.Percent = five / float64(u.FiveHour.N)
		}
		if u.Weekly.N > 0 {
			u.Weekly.Percent = week / float64(u.Weekly.N)
		}
		out = append(out, u)
	}
	return out
}
