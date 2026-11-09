package accounts

// Machine affinity (W-affinity): automatic selection keeps the machines on
// separate accounts where it can, as a soft preference inside an
// availability class (fresh, verify, refresh, unknown). It never makes a
// run fail and never picks an at-limit or ineligible account while a usable
// one exists; explicit accounts and Prefer lists are unaffected.
//
// Order inside a class (rank):
//
//  1. Policy preference (Prefer list).
//  2. Affinity tier, only for accounts below the near-limit threshold
//     (usage.NearPercent, 80 %, on the 5-hour window; HomeWeeklyPercent,
//     70 %, on the weekly window):
//     tierHome   this machine's home account (the one it is using now, else
//     the one it used last), unless another machine was on it first;
//     tierFree   not in use by runs on any other machine;
//     tierShared in use elsewhere: fewest other machines first, then the home
//     account, then headroom;
//     tierRest   everything else (near the limit, unknown windows, or no
//     fleet view): most headroom, as before affinity.
//  3. Most headroom, fewest local runs, then a per-node hash (equal numbers
//     on two machines do not pick the same account), then the account id.
//
// "In use elsewhere" comes from the hub's active assignments
// (AccountView.Assignments; Config.Fleet), read before the selection
// transaction and cached briefly. The read runs in the background (bounded
// by fleetFetchTimeout): selection uses the last good view up to
// fleetStaleFor old while a newer one loads, and waits at most FleetTimeout
// only when it has none. Unreachable or slow hub: no spread, only local
// stickiness and headroom.
//
// Concurrency: the race is accepted. Two machines resolving at the same
// moment can both take the same free account (the hub learns of each
// assignment from the node's report). The next run rebalances: on a shared
// home account the machine whose active run started first keeps it, and the
// later machine moves to a free account if one exists. Running sessions are
// never moved.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"hash/fnv"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/usage"
)

// FleetDirectory reads the hub's account views (node credential); the hub
// client implements it. When it also implements ListNodes, node names are
// used in assignment reasons.
type FleetDirectory interface {
	ListAccounts(ctx context.Context, req contracts.ListAccountsRequest) (contracts.Page[contracts.AccountView], error)
}

type nodeLister interface {
	ListNodes(ctx context.Context, req contracts.ListNodesRequest) (contracts.Page[contracts.Node], error)
}

const (
	defaultFleetTimeout = 2 * time.Second
	defaultFleetCache   = 5 * time.Second
	fleetFailCache      = 30 * time.Second
	// fleetFetchTimeout bounds one background read: a slow hub link (a
	// relayed connection takes seconds for the account list) still
	// completes it for the next launch.
	fleetFetchTimeout = 20 * time.Second
	// fleetStaleFor is how long the last good view stands in while a
	// newer read is loading or failed (affinity is a soft preference).
	fleetStaleFor = 10 * time.Minute
)

// fleetUse is the hub's view of which other machines run on each account.
type fleetUse struct {
	ok  bool
	why string // when !ok
	// since: account -> other node -> its earliest active assignment.
	since map[string]map[string]time.Time
	names map[string]string // node id -> name
	// tiers: every listed account's tier (reserve.go); "" is normal.
	tiers map[string]contracts.AccountTier
}

type fleetCache struct {
	mu sync.Mutex
	at time.Time // when v was read (wall clock)
	v  *fleetUse // the latest read, failed or not
	// good is the latest successful read (at goodAt).
	good   *fleetUse
	goodAt time.Time
	// loading closes when the read in flight ends; nil when none is.
	loading chan struct{}
}

// fleetView returns the fleet view for a selection; nil when no directory
// is configured. It never waits on the hub for longer than FleetTimeout,
// and not at all while a good view up to fleetStaleFor old exists (the
// refresh then runs in the background).
func (s *Service) fleetView(ctx context.Context) *fleetUse {
	if s.cfg.Fleet == nil {
		return nil
	}
	now := time.Now() // wall clock: the cache must expire even under a fake Config.Now
	s.fleet.mu.Lock()
	ttl := s.cfg.FleetCache
	if v := s.fleet.v; v != nil && !v.ok {
		ttl = fleetFailCache
	}
	fresh := s.fleet.v != nil && now.Sub(s.fleet.at) < ttl
	var loading chan struct{}
	if !fresh {
		loading = s.loadFleetLocked()
	}
	// A negative FleetCache (always read) never serves an older view.
	if g := s.fleet.good; g != nil && s.cfg.FleetCache >= 0 && now.Sub(s.fleet.goodAt) < fleetStaleFor && (!fresh || !s.fleet.v.ok) {
		s.fleet.mu.Unlock()
		return g
	}
	if fresh {
		v := s.fleet.v
		s.fleet.mu.Unlock()
		return v
	}
	s.fleet.mu.Unlock()
	t := time.NewTimer(s.cfg.FleetTimeout)
	defer t.Stop()
	select {
	case <-loading:
		s.fleet.mu.Lock()
		defer s.fleet.mu.Unlock()
		return s.fleet.v
	case <-t.C:
		return &fleetUse{why: fmt.Sprintf("fleet view unavailable: the hub did not answer within %s", s.cfg.FleetTimeout)}
	case <-ctx.Done():
		return &fleetUse{why: "fleet view unavailable: " + ctx.Err().Error()}
	}
}

// loadFleetLocked starts a background read of the fleet view unless one is
// in flight and returns the channel that closes when it ends (caller holds
// s.fleet.mu).
func (s *Service) loadFleetLocked() chan struct{} {
	if s.fleet.loading != nil {
		return s.fleet.loading
	}
	done := make(chan struct{})
	s.fleet.loading = done
	timeout := max(s.cfg.FleetTimeout, fleetFetchTimeout)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		v := s.fetchFleet(ctx)
		cancel()
		if !v.ok {
			s.log.Info("accounts: fleet view unavailable; selection without machine spread", "why", v.why)
		}
		s.fleet.mu.Lock()
		at := time.Now()
		s.fleet.v, s.fleet.at, s.fleet.loading = v, at, nil
		if v.ok {
			s.fleet.good, s.fleet.goodAt = v, at
		}
		s.fleet.mu.Unlock()
		close(done)
	}()
	return done
}

func (s *Service) fetchFleet(ctx context.Context) *fleetUse {
	u := &fleetUse{since: map[string]map[string]time.Time{}, names: map[string]string{}, tiers: map[string]contracts.AccountTier{}}
	cur := ""
	for i := 0; i < 20; i++ {
		page, err := s.cfg.Fleet.ListAccounts(ctx, contracts.ListAccountsRequest{PageRequest: contracts.PageRequest{Cursor: cur, Limit: 200}})
		if err != nil {
			return &fleetUse{why: "fleet view unavailable: " + redact(contracts.AsError(err).Message)}
		}
		for _, v := range page.Items {
			u.tiers[v.Account.AccountID] = v.Account.Tier
			for _, a := range v.Assignments {
				if a.ReleasedAt != nil || a.AccountID == "" || a.NodeID == "" || a.NodeID == s.cfg.NodeID {
					continue
				}
				m := u.since[a.AccountID]
				if m == nil {
					m = map[string]time.Time{}
					u.since[a.AccountID] = m
				}
				if t, ok := m[a.NodeID]; !ok || a.AssignedAt.Before(t) {
					m[a.NodeID] = a.AssignedAt
				}
			}
		}
		if page.NextCursor == "" {
			break
		}
		cur = page.NextCursor
	}
	u.ok = true
	if nl, ok := s.cfg.Fleet.(nodeLister); ok && len(u.since) > 0 {
		if page, err := nl.ListNodes(ctx, contracts.ListNodesRequest{}); err == nil {
			for _, n := range page.Items {
				if n.Name != "" {
					u.names[n.NodeID] = n.Name
				}
			}
		}
	}
	return u
}

func (u *fleetUse) name(id string) string {
	if u != nil && u.names[id] != "" {
		return u.names[id]
	}
	return id
}

// affinity tiers (see the file comment).
const (
	tierHome = iota
	tierFree
	tierShared
	tierRest
)

func (c *candidate) fleetOK() bool { return c.fleet != nil && c.fleet.ok }

func (c *candidate) tier() int {
	switch {
	case c.near:
		return tierRest
	case c.home && (len(c.others) == 0 || c.senior):
		return tierHome
	case !c.fleetOK():
		return tierRest
	case len(c.others) == 0:
		return tierFree
	}
	return tierShared
}

// nearNote says no account is below the near-limit thresholds.
var nearNote = fmt.Sprintf("every eligible account is near a limit (5h from %.0f%%, weekly from %.0f%%)", usage.NearPercent, HomeWeeklyPercent)

// HomeWeeklyPercent is the weekly used-percent from which a machine stops
// preferring its home account: past it, new runs go to the account with the
// most headroom (a home at 97 % weekly next to accounts at 4-36 % starved
// them while its own week ran out and cut sessions off mid-step).
const HomeWeeklyPercent = 70.0

// nearLimit: a reported 5-hour window at or above usage.NearPercent, a
// weekly window at or above HomeWeeklyPercent, or no window reported at all.
func nearLimit(h usage.Headroom) bool {
	if !h.FiveOK && !h.WeekOK {
		return true
	}
	return h.FiveOK && h.FiveHour >= usage.NearPercent || h.WeekOK && h.Weekly >= HomeWeeklyPercent
}

// markAffinity sets home/others/senior/near/tie on candidates (inside the
// selection transaction; local data only, the fleet view was read before).
func (s *Service) markAffinity(ctx context.Context, q querier, provider contracts.Provider, cs []*candidate) error {
	if len(cs) == 0 {
		return nil
	}
	// Home: the account this machine runs on now (newest active run), else
	// the one it used last, among this provider's accounts.
	var home string
	err := q.QueryRowContext(ctx, `SELECT a.account_id FROM accounts_assignment a JOIN accounts_account c ON c.account_id=a.account_id
		WHERE c.provider=? AND a.account_id<>'' ORDER BY a.released_at='' DESC, a.assigned_at DESC, a.rowid DESC LIMIT 1`, string(provider)).Scan(&home)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	for _, c := range cs {
		id := c.acct.AccountID
		c.home = id == home
		c.near = nearLimit(c.room)
		c.others, c.senior, c.tie = nil, false, 0
		if !c.fleetOK() {
			continue
		}
		h := fnv.New64a()
		h.Write([]byte(s.cfg.NodeID + "\x00" + id))
		c.tie = h.Sum64()
		for n := range c.fleet.since[id] {
			c.others = append(c.others, n)
		}
		sort.Strings(c.others)
		if c.home && len(c.others) > 0 {
			var mine string
			if err := q.QueryRowContext(ctx, `SELECT COALESCE(MIN(assigned_at),'') FROM accounts_assignment WHERE account_id=? AND released_at=''`, id).Scan(&mine); err != nil {
				return err
			}
			c.senior = mine != "" && s.firstOn(pt(mine), c.fleet.since[id])
		}
	}
	return nil
}

// firstOn: this machine's run on the account started before every other
// machine's (ties: the lower node id keeps it).
func (s *Service) firstOn(mine time.Time, others map[string]time.Time) bool {
	for n, t := range others {
		if t.Before(mine) || t.Equal(mine) && n < s.cfg.NodeID {
			return false
		}
	}
	return true
}

func acctName(a contracts.Account) string {
	if a.Label != "" {
		return a.Label
	}
	return a.AccountID
}

// acctRef names an account unambiguously in agent-facing text:
// `account "opus"` (a bare label such as "opus" reads as a model name).
func acctRef(label string) string { return fmt.Sprintf("account %q", label) }

func (s *Service) nodeName() string {
	if s.cfg.NodeName != "" {
		return s.cfg.NodeName
	}
	return s.cfg.NodeID
}

func (c *candidate) otherNames() string {
	names := make([]string, 0, len(c.others))
	for _, n := range c.others {
		names = append(names, c.fleet.name(n))
	}
	return strings.Join(names, ", ")
}

// explainAuto is the assignment reason of an automatic choice c, the first
// of list (its class), with all candidates for the home-account note.
func (s *Service) explainAuto(c *candidate, list, all []*candidate) string {
	nums := c.room.Numbers()
	var parts []string
	switch c.tier() {
	case tierHome:
		p := "home account for " + s.nodeName()
		switch {
		case !c.fleetOK():
		case len(c.others) == 0:
			p += "; not used on other machines"
		default:
			p += "; also in use on " + c.otherNames() + " (this machine was on it first)"
		}
		return p + " (" + nums + ")"
	case tierFree:
		if n := s.homeNote(c, list, all); n != "" {
			parts = append(parts, n)
		}
		parts = append(parts, "not used on other machines", usage.Explain(c.room, len(list)))
	case tierShared:
		if n := s.homeNote(c, list, all); n != "" {
			parts = append(parts, n)
		}
		parts = append(parts, fmt.Sprintf("every eligible account with room is in use on another machine; shared with %s (fewest other machines, %s)",
			c.otherNames(), nums))
	default:
		if c.fleetOK() && c.near {
			parts = append(parts, nearNote)
		}
		parts = append(parts, usage.Explain(c.room, len(list)))
		if c.fleetOK() && len(c.others) > 0 {
			parts = append(parts, "also in use on "+c.otherNames())
		}
		if c.fleet != nil && !c.fleet.ok {
			parts = append(parts, c.fleet.why)
		}
	}
	return strings.Join(parts, "; ")
}

// homeNote says why the machine left its home account, if it had one.
func (s *Service) homeNote(c *candidate, list, all []*candidate) string {
	var h *candidate
	for _, x := range all {
		if x.home {
			h = x
		}
	}
	if h == nil || h == c {
		return ""
	}
	inList := false
	for _, x := range list {
		inList = inList || x == h
	}
	name := acctName(h.acct)
	switch {
	case !inList && reserveHomeNote(c, list, all) != "":
		return reserveHomeNote(c, list, all)
	case !inList:
		return "home account " + name + " is not available now"
	case h.near:
		return "home account " + name + " is near its limit (" + h.room.Numbers() + ")"
	case h.crowd > c.crowd:
		return "spreading runs: home account " + name + " has " + s.crowdNote(h)
	case len(h.others) > 0:
		return "home account " + name + " is also in use on " + h.otherNames() + " (there first)"
	}
	return ""
}
