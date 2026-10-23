package accounts

import (
	"context"
	"fmt"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/usage"
)

// Reserve tier (W-reserve). The owner marks some accounts reserve
// (`pyre accounts tier <account> reserve`, contracts.Account.Tier): "ideally
// not unless everything else is out or close to out". Automatic selection
// (new starts, placement's dry run, pyre run, limit fallback and the
// rebalancer, which all go through choose) therefore holds every reserve
// account back while a normal account of the provider has comfortable room:
//
//	comfortable  a normal candidate that can take the run now (fresh, or a
//	             checked-in login judged by its last-known windows) with a
//	             reported window and neither its 5-hour nor its weekly window
//	             at or above usage.NearPercent (80 %, what `pyre accounts`
//	             shows as "near limit").
//
// Only when no normal account is comfortable (all near or at a limit,
// cooling down, needing re-auth, busy, unknown or absent) do reserve
// accounts join the candidates, and then the usual ranking decides (a
// normal account near its limit is "hot" in spread.go and loses to a reserve
// account with room; among reserve accounts the order is unchanged).
//
// An explicit account and a policy Prefer list win over the tier: a named
// or preferred reserve account is never held back.
//
// A normal account whose availability is unknown (a stale reading) is not
// room, but ResolveFor and Select take their one observation pass before
// they give the run to a reserve account because of it.
//
// The tier comes from the hub's fleet view when it lists the account
// (seconds after a change), else from the node's copy, which the token sync
// refreshes from the hub (tokenmerge.go). No tier (an older hub) is normal.

// isReserve is the candidate's tier: the hub's when the fleet view lists
// the account, else the local copy's.
func (c *candidate) isReserve() bool {
	if c.fleetOK() {
		if t, ok := c.fleet.tiers[c.acct.AccountID]; ok {
			return t == contracts.TierReserve
		}
	}
	return c.acct.Reserve()
}

// comfortable: known room below the near-limit threshold on both windows.
func comfortable(h usage.Headroom) bool {
	if !h.FiveOK && !h.WeekOK {
		return false
	}
	return !(h.FiveOK && h.FiveHour >= usage.NearPercent) && !(h.WeekOK && h.Weekly >= usage.NearPercent)
}

// gated: a reserve candidate the tier applies to (not on the Prefer list,
// whose length is nPrefer).
func (c *candidate) gated(nPrefer int) bool { return c.reserve && c.pref >= nPrefer }

// holdReserve drops gated reserve candidates from the class lists (fresh,
// verify, refresh, unknown) when a normal candidate in the first three is
// comfortable; otherwise it returns the lists unchanged.
func holdReserve(nPrefer int, lists ...[]*candidate) [][]*candidate {
	room := false
	for _, l := range lists[:min(3, len(lists))] {
		for _, c := range l {
			room = room || !c.reserve && comfortable(c.room)
		}
	}
	if !room {
		return lists
	}
	out := make([][]*candidate, len(lists))
	for i, l := range lists {
		for _, c := range l {
			if !c.gated(nPrefer) {
				out[i] = append(out[i], c)
			}
		}
	}
	return out
}

// normalFirst stably moves gated reserve candidates behind the others
// (the unknown class under AllowUnknown: no known room to hold them back
// by, but a reserve account still goes last).
func normalFirst(nPrefer int, l []*candidate) []*candidate {
	out := make([]*candidate, 0, len(l))
	for _, c := range l {
		if !c.gated(nPrefer) {
			out = append(out, c)
		}
	}
	for _, c := range l {
		if c.gated(nPrefer) {
			out = append(out, c)
		}
	}
	return out
}

// reserveHomeNote says the machine's home account was held back as
// reserve (it is usable, but not in list).
func reserveHomeNote(c *candidate, list, all []*candidate) string {
	for _, h := range all {
		if !h.home || h == c || !h.reserve || !h.ver.OK || h.class != classFresh && h.class != classVerify && h.class != classRefresh {
			continue
		}
		for _, x := range list {
			if x == h {
				return ""
			}
		}
		return "home account " + acctName(h.acct) + " is a reserve account and a normal account has room"
	}
	return ""
}

// reserveOverUnknown: the choice would go to a reserve account (the first
// of fresh, verify, refresh is a gated one) while a normal account's
// availability is unknown. Callers observe once before accepting that.
func reserveOverUnknown(nPrefer int, unknown []*candidate, classes ...[]*candidate) bool {
	first := (*candidate)(nil)
	for _, l := range classes {
		if len(l) > 0 {
			first = l[0]
			break
		}
	}
	if first == nil || !first.gated(nPrefer) {
		return false
	}
	for _, c := range unknown {
		if !c.reserve {
			return true
		}
	}
	return false
}

// reserveNote starts the reason of an automatic choice of a reserve
// account ("" otherwise).
func reserveNote(c *candidate, provider contracts.Provider, nPrefer int, explicit bool) string {
	if explicit || !c.gated(nPrefer) {
		return ""
	}
	return fmt.Sprintf("reserve account: no normal %s account has room (each is near or at a limit, from %.0f%%, or unavailable); ", provider, usage.NearPercent)
}

// reserveAccounts maps the reserve accounts among ids to their provider
// (the hub's fleet view when it lists them, else the local copy). The
// rebalancer drains idle, unnamed runs off them while a normal account has
// comfortable room (rebalance.go moveIdle).
func (s *Service) reserveAccounts(ctx context.Context, ids []string) map[string]contracts.Provider {
	out := map[string]contracts.Provider{}
	seen := map[string]bool{}
	var f *fleetUse
	fetched := false
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		a, err := getAccount(ctx, s.db, id)
		if err != nil {
			continue
		}
		if !fetched {
			f, fetched = s.fleetView(ctx), true
		}
		if (&candidate{acct: a, fleet: f}).isReserve() {
			out[id] = a.Provider
		}
	}
	return out
}

// normalRoom: a normal account of provider could take a drained run now
// (what moveIdle requires of its target: fresh, not near a limit,
// comfortable). One dry selection per provider per sweep, so idle runs on
// reserve accounts cost nothing while every normal account is busy.
func (s *Service) normalRoom(ctx context.Context, p contracts.Provider) bool {
	h := harnessForProvider(p)
	if h == "" {
		return false
	}
	req := ResolveRequest{Harness: h, Policy: contracts.AccountPolicy{Mode: contracts.AccountEligible}, RunID: "rebalance-room"}
	cands, notes, err := s.gather(ctx, req)
	if err != nil {
		return false
	}
	normal := cands[:0:0]
	for _, c := range cands {
		if !c.reserve {
			normal = append(normal, c)
		}
	}
	c, err := s.choose(ctx, s.db, req, normal, notes, s.cfg.CodexLeaseHolder)
	return err == nil && c.class == classFresh && !nearLimit(c.room) && comfortable(c.room)
}
