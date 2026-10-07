package usage

import (
	"fmt"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// OnUse names why a working login's usage cannot be read at rest. Such a
// login is available: the selector checks it out and verifies it before
// the run is admitted (accounts: classRefresh / classVerify), so the
// display counts it as ready, never as unknown.
type OnUse string

const (
	// RefreshOnUse: the access token expired at rest; the harness refreshes
	// it in the checked-out profile.
	RefreshOnUse OnUse = "refreshes on use"
	// CheckOnUse: usage is readable only once checked out (Codex needs an
	// app-server on the profile).
	CheckOnUse OnUse = "usage visible once checked out"
)

// OnUseOf reports whether obs is unknown only because usage cannot be read
// at rest ("" otherwise).
func OnUseOf(obs *contracts.AccountObservation) OnUse {
	if obs == nil || obs.Eligibility != contracts.EligibleUnknown {
		return ""
	}
	d := strings.ToLower(obs.Detail)
	switch {
	case strings.Contains(d, "access token expired"):
		return RefreshOnUse
	case strings.Contains(d, "not checked out"):
		return CheckOnUse
	}
	return ""
}

// LastKnown is what last-known windows say about an account at now.
type LastKnown struct {
	// Limited: a plan-limit window is at 100% and its reset is still ahead
	// (or unknown). Until is that reset (nil when unknown).
	Limited bool
	Until   *time.Time
	// Near: a plan-limit window is at or above NearPercent.
	Near bool
	// Room ranks the account among other unverified candidates.
	Room Headroom
}

// Judge reads last-known windows at now. A window whose reset already
// passed counts as empty (it has reset since). The extra-usage budget is
// not a limit. This one function decides both what the owner sees and
// whether the selector may check an unverified account out.
func Judge(ws []contracts.UsageWindow, now time.Time) LastKnown {
	cur := make([]contracts.UsageWindow, 0, len(ws))
	for _, w := range ws {
		if w.ResetAt != nil && !w.ResetAt.After(now) {
			w.UsedPercent = 0
		}
		cur = append(cur, w)
	}
	var lk LastKnown
	for _, w := range cur {
		if !IsLimit(w.Name) {
			continue
		}
		if w.UsedPercent >= 100 {
			lk.Limited = true
			if w.ResetAt != nil && (lk.Until == nil || w.ResetAt.After(*lk.Until)) {
				t := *w.ResetAt
				lk.Until = &t
			}
		}
		if w.UsedPercent >= NearPercent {
			lk.Near = true
		}
	}
	lk.Room = HeadroomOf(cur)
	return lk
}

// assessOnUse is the state of a login available on use, from its
// last-known windows.
func assessOnUse(on OnUse, obs *contracts.AccountObservation, now time.Time) Assessment {
	lk := Judge(obs.Windows, now)
	a := Assessment{OnUse: on, Why: string(on), LastKnown: len(obs.Windows) > 0}
	switch {
	case lk.Limited:
		a.State, a.Why, a.Until = AtLimit, "limit reached (last known)", lk.Until
	case lk.Near:
		a.State = NearLimit
	default:
		a.State = Healthy
	}
	return a
}

// Label is the state in plain words: "healthy", "ready · refreshes on use",
// "near limit · refreshes on use", "at limit". Every on-use login reads
// "refreshes on use": its usage is current once it is checked out.
func (a Assessment) Label() string {
	switch {
	case a.OnUse == OwnUse && a.State == Healthy:
		return "ready · own login"
	case a.OnUse == OwnUse && a.State == NearLimit:
		return "near limit · own login"
	case a.OnUse == ProbeOnUse && a.State == Healthy:
		return "ready · " + string(ProbeOnUse)
	case a.OnUse == ProbeOnUse && a.State == NearLimit:
		return "near limit · " + string(ProbeOnUse)
	case a.OnUse != "" && a.State == Healthy:
		return "ready · refreshes on use"
	case a.OnUse != "" && a.State == NearLimit:
		return "near limit · refreshes on use"
	}
	return a.State.String()
}

// Available reports whether new work can start on this login now (fresh
// headroom, or unverified with headroom in its last-known windows).
func (a Assessment) Available() bool { return a.State.Ready() }

// Summary is the pool in words: "3 ready · 5 near or at limit · 1 needs
// re-auth". Ready counts available accounts with room; near-limit
// accounts are available too (see Tight).
func (p Pool) Summary() string {
	parts := []string{fmt.Sprintf("%d ready", p.Ready)}
	switch {
	case p.Near > 0 && p.AtLimit > 0:
		parts = append(parts, fmt.Sprintf("%d near or at limit", p.Near+p.AtLimit))
	case p.Near > 0:
		parts = append(parts, fmt.Sprintf("%d near limit", p.Near))
	case p.AtLimit > 0:
		parts = append(parts, fmt.Sprintf("%d at limit", p.AtLimit))
	}
	if p.Reauth > 0 {
		parts = append(parts, fmt.Sprintf("%d needs re-auth", p.Reauth))
	}
	if p.Unknown > 0 {
		parts = append(parts, fmt.Sprintf("%d unknown", p.Unknown))
	}
	return strings.Join(parts, " · ")
}

// Brief is the header form: "3 ready · 5 near limit" (ready and limit
// counts only).
func (p Pool) Brief() string {
	s := p.Summary()
	if p.Reauth > 0 || p.Unknown > 0 {
		parts := strings.Split(s, " · ")
		n := 1
		if p.Near+p.AtLimit > 0 {
			n = 2
		}
		s = strings.Join(parts[:n], " · ")
	}
	return s
}

// Available is the number of accounts new work can start on.
func (p Pool) Available() int { return p.Ready + p.Near }

// Tight: no account is available (shown red).
func (p Pool) Tight() bool { return p.Total > 0 && p.Available() == 0 }

// Warn: accounts are available but every one is near its limit (amber).
func (p Pool) Warn() bool { return p.Ready == 0 && p.Near > 0 }
