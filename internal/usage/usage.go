// Package usage derives what the owner sees about subscription usage from
// account observations: the 5-hour and weekly windows, a plain state per
// account login (healthy, near limit, at limit, needs re-auth, unknown),
// the pool summary per provider, and the explanation of an automatic
// account choice. It is pure (contracts only) so the selector, the CLI,
// the TUI and the mock backend share one meaning.
package usage

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// Window names reported by the usage probes (accounts.ParseClaudeUsage,
// accounts.ParseCodexRateLimits).
const (
	FiveHour = "five_hour"
	Weekly   = "seven_day"
	// Extra is the paid extra-usage (overage) budget. It is not a plan
	// limit: at 100% the subscription windows still work, so it never
	// counts toward exhaustion, headroom or ranking.
	Extra = "extra_usage"
)

// NearPercent is the used-percent at which a window reads "near limit".
const NearPercent = 80.0

// FreshFor is how old an observation may be before its numbers are shown
// as last known rather than current (the selector's default fresh window).
const FreshFor = 15 * time.Minute

// IsLimit reports whether a window is a plan limit (every window except
// the extra-usage budget).
func IsLimit(name string) bool { return name != Extra }

// Windows splits an observation's windows by meaning.
type Windows struct {
	FiveHour *contracts.UsageWindow
	Weekly   *contracts.UsageWindow
	// Scoped are model-scoped weekly windows ("seven_day_fable").
	Scoped []contracts.UsageWindow
	Extra  *contracts.UsageWindow
	// Other are windows Pyre does not know (shown, counted as limits).
	Other []contracts.UsageWindow
}

// Split classifies ws.
func Split(ws []contracts.UsageWindow) Windows {
	var out Windows
	for i := range ws {
		w := ws[i]
		switch {
		case w.Name == FiveHour:
			out.FiveHour = &w
		case w.Name == Weekly:
			out.Weekly = &w
		case w.Name == Extra:
			out.Extra = &w
		case strings.HasPrefix(w.Name, Weekly+"_"):
			out.Scoped = append(out.Scoped, w)
		default:
			out.Other = append(out.Other, w)
		}
	}
	return out
}

// ScopedLabel names a model-scoped weekly window: "seven_day_fable" →
// "fable week".
func ScopedLabel(name string) string {
	rest := strings.TrimPrefix(name, Weekly+"_")
	return strings.ReplaceAll(rest, "_", " ") + " week"
}

// Used returns the used percent of the 5-hour window and of the weekly
// limit (the highest of the all-models and model-scoped weekly windows),
// with ok=false when the window is not reported.
func Used(ws []contracts.UsageWindow) (fiveHour float64, fiveOK bool, weekly float64, weekOK bool) {
	sp := Split(ws)
	if sp.FiveHour != nil {
		fiveHour, fiveOK = sp.FiveHour.UsedPercent, true
	}
	if sp.Weekly != nil {
		weekly, weekOK = sp.Weekly.UsedPercent, true
	}
	for _, w := range sp.Scoped {
		if !weekOK || w.UsedPercent > weekly {
			weekly, weekOK = w.UsedPercent, true
		}
	}
	return
}

// MaxUsed is the highest used percent across plan-limit windows (the
// extra-usage budget excluded) and that window's reset.
func MaxUsed(ws []contracts.UsageWindow) (float64, *time.Time) {
	maxUsed := 0.0
	var reset *time.Time
	for _, w := range ws {
		if IsLimit(w.Name) && w.UsedPercent >= maxUsed {
			maxUsed, reset = w.UsedPercent, w.ResetAt
		}
	}
	return maxUsed, reset
}

// Headroom is the ranking key of an eligible account: unreported windows
// rank as fully used (unknown is never capacity).
type Headroom struct {
	FiveHour, Weekly float64 // used percent
	FiveOK, WeekOK   bool
}

// HeadroomOf reads the ranking key from windows.
func HeadroomOf(ws []contracts.UsageWindow) Headroom {
	f, fo, w, wo := Used(ws)
	return Headroom{FiveHour: f, FiveOK: fo, Weekly: w, WeekOK: wo}
}

func (h Headroom) key() (int, int) {
	f, w := 100, 100
	if h.FiveOK {
		f = int(math.Round(h.FiveHour))
	}
	if h.WeekOK {
		w = int(math.Round(h.Weekly))
	}
	return f, w
}

// Compare orders by most headroom: the lower 5-hour use first, then the
// lower weekly use (whole percents, so a fraction does not override the
// weekly window). It returns <0 when h has more headroom than o.
func (h Headroom) Compare(o Headroom) int {
	hf, hw := h.key()
	of, ow := o.key()
	if hf != of {
		return hf - of
	}
	return hw - ow
}

// Numbers renders "5h 12% used, weekly 40%".
func (h Headroom) Numbers() string {
	var parts []string
	if h.FiveOK {
		parts = append(parts, fmt.Sprintf("5h %.0f%% used", h.FiveHour))
	}
	if h.WeekOK {
		w := fmt.Sprintf("weekly %.0f%%", h.Weekly)
		if !h.FiveOK {
			w = fmt.Sprintf("weekly %.0f%% used", h.Weekly)
		}
		parts = append(parts, w)
	}
	if len(parts) == 0 {
		return "no usage windows reported"
	}
	return strings.Join(parts, ", ")
}

// Explain says why the first of n eligible accounts was chosen:
// "most headroom (5h 12% used, weekly 40%)", or "only eligible account
// (…)" when there was no choice to make.
func Explain(h Headroom, n int) string {
	if n <= 1 {
		return "only eligible account (" + h.Numbers() + ")"
	}
	return "most headroom (" + h.Numbers() + ")"
}

// State is the plain-words health of one account login.
type State int

const (
	Healthy State = iota
	NearLimit
	AtLimit
	NeedsReauth
	Unknown
	Disabled
	// Ambient is the owner's own login on a node without Pyre accounts for
	// the provider: not managed, usage not observed.
	Ambient
)

func (s State) String() string {
	return [...]string{"healthy", "near limit", "at limit", "needs re-auth", "unknown", "disabled", "not managed"}[s]
}

// Ready reports whether new work can start on this login now.
func (s State) Ready() bool { return s == Healthy || s == NearLimit }

// Assessment is the state of one account login with its reason.
type Assessment struct {
	State State
	// Why is a short plain reason for unknown / needs re-auth / at limit.
	Why string
	// Until is when an at-limit login resets (when known).
	Until *time.Time
	// LastKnown marks windows from a stale or unknown observation: the
	// numbers are the last ones seen, not current capacity.
	LastKnown bool
	// OnUse is set when the login works but its usage is read only at use
	// (OnUseOf); the state then comes from the last-known windows (Judge).
	OnUse OnUse
}

// Assess derives the state of an account login from its newest
// observation on that node and the node's grant (nil when unknown).
func Assess(acct contracts.Account, obs *contracts.AccountObservation, grant *contracts.DeviceGrant, now time.Time) Assessment {
	if acct.Disabled {
		return Assessment{State: Disabled, Why: "disabled"}
	}
	if grant != nil {
		switch grant.State {
		case contracts.GrantExpired, contracts.GrantDead, contracts.GrantRevoked:
			why := "login " + string(grant.State)
			if grant.Detail != "" {
				why += ": " + grant.Detail
			}
			return Assessment{State: NeedsReauth, Why: why}
		}
	}
	if obs == nil {
		return Assessment{State: Unknown, Why: "no usage observation yet"}
	}
	if on := OnUseOf(obs); on != "" {
		return assessOnUse(on, obs, now)
	}
	if grant != nil && grant.Kind == contracts.CredOAuthToken {
		if a, ok := assessToken(obs, now); ok { // kind.go
			return a
		}
	}
	stale := obs.Stale || now.Sub(obs.ObservedAt) > FreshFor
	switch obs.Eligibility {
	case contracts.EligibleUnusable:
		return Assessment{State: NeedsReauth, Why: Humanize(obs.Detail, "login unusable")}
	case contracts.EligibleExhausted, contracts.EligibleCooldown:
		until := obs.ResetAt
		if until == nil {
			until = limitReset(obs.Windows)
		}
		if until != nil && !until.After(now) {
			return Assessment{State: Unknown, Why: "limit reset passed; waiting for a new observation", LastKnown: true}
		}
		return Assessment{State: AtLimit, Why: "limit reached", Until: until}
	case contracts.EligibleYes:
	default:
		return Assessment{State: Unknown, Why: Humanize(obs.Detail, "availability unknown"), LastKnown: len(obs.Windows) > 0}
	}
	if stale {
		return Assessment{State: Unknown, Why: "last observed " + agoWords(now, obs.ObservedAt), LastKnown: len(obs.Windows) > 0}
	}
	if m, reset := MaxUsed(obs.Windows); m >= 100 {
		return Assessment{State: AtLimit, Why: "limit reached", Until: reset}
	}
	f, fo, w, wo := Used(obs.Windows)
	if fo && f >= NearPercent || wo && w >= NearPercent {
		return Assessment{State: NearLimit}
	}
	return Assessment{State: Healthy}
}

// limitReset is the latest reset among exhausted plan-limit windows.
func limitReset(ws []contracts.UsageWindow) *time.Time {
	var r *time.Time
	for _, w := range ws {
		if IsLimit(w.Name) && w.UsedPercent >= 100 && w.ResetAt != nil && (r == nil || w.ResetAt.After(*r)) {
			t := *w.ResetAt
			r = &t
		}
	}
	return r
}

// Humanize shortens probe details into plain words.
func Humanize(detail, fallback string) string {
	d := strings.ToLower(detail)
	switch {
	case strings.Contains(d, "access token expired"):
		return "token expired; refreshes on next use"
	case strings.Contains(d, "not checked out"):
		return "usage visible once checked out"
	case strings.Contains(d, "rate-limited") || strings.Contains(d, "rate limited"):
		return "usage endpoint rate-limited"
	case detail == "":
		return fallback
	}
	if r := []rune(detail); len(r) > 60 {
		return string(r[:59]) + "…"
	}
	return detail
}

// Card is one account login (an account on one node), or the owner's own
// login on a node without Pyre accounts (Account nil, State Ambient).
type Card struct {
	Provider contracts.Provider
	NodeID   string
	Account  *contracts.Account
	Obs      *contracts.AccountObservation
	Grant    *contracts.DeviceGrant
	Assessment
	Assignments []contracts.AccountAssignment
	// TokenNodes is set on a token account's single card (kind.go): the
	// machines holding its token grant. NodeID is then empty and Machines
	// is the MACHINE cell ("all machines" or their names).
	TokenNodes []string
	Machines   string
	// Own marks the machines' own login matched to this account
	// (ownlogin.go): TokenNodes are those machines, no grant.
	Own bool
}

// Label is the account label, or "your own login (not managed)".
func (c Card) Label() string {
	if c.Account == nil {
		return "your own login (not managed)"
	}
	if c.Account.Label != "" {
		return c.Account.Label
	}
	return c.Account.AccountID
}

// harnessFor maps a provider to the harness whose login it is.
func harnessFor(p contracts.Provider) contracts.Harness {
	switch p {
	case contracts.ProviderAnthropic:
		return contracts.HarnessClaude
	case contracts.ProviderOpenAI:
		return contracts.HarnessCodex
	}
	return ""
}

// newestWindows is the windows of the account's newest observation that
// reports any (nil when none does).
func newestWindows(v *contracts.AccountView) []contracts.UsageWindow {
	var best *contracts.AccountObservation
	for i := range v.ByNode {
		o := &v.ByNode[i]
		if len(o.Windows) > 0 && (best == nil || o.ObservedAt.After(best.ObservedAt)) {
			best = o
		}
	}
	if best == nil && v.Latest != nil && len(v.Latest.Windows) > 0 {
		best = v.Latest
	}
	if best == nil {
		return nil
	}
	return best.Windows
}

// Cards builds one card per account per node that holds a login for it,
// plus one ambient card per provider for each node that has the harness
// but no Pyre account for that provider. A token account is one card for
// all its machines (kind.go). Order: provider, token cards, node name,
// account label. nodeName may be nil (node IDs are used).
func Cards(views []contracts.AccountView, nodes []contracts.Node, now time.Time) []Card {
	var out []Card
	hasGrant := map[contracts.Provider]map[string]bool{}
	providers := map[contracts.Provider]bool{}
	own, ownAt := ownLogins(views, nodes)
	for vi := range views {
		v := &views[vi]
		p := v.Account.Provider
		if ids := own[v.Account.AccountID]; len(ids) > 0 && len(v.Grants) == 0 {
			// Not a Pyre login anywhere, but the machines' own login: one
			// "own login" card instead of "not signed in" (ownlogin.go).
			providers[p] = true
			out = append(out, ownCard(v, ids, nodes, now))
			continue
		}
		providers[p] = true
		if hasGrant[p] == nil {
			hasGrant[p] = map[string]bool{}
		}
		byNode := map[string]*contracts.AccountObservation{}
		for i := range v.ByNode {
			byNode[v.ByNode[i].ReportingNode] = &v.ByNode[i]
		}
		var nodeIDs []string
		grants := map[string]*contracts.DeviceGrant{}
		for i := range v.Grants {
			g := &v.Grants[i]
			if _, dup := grants[g.NodeID]; !dup {
				nodeIDs = append(nodeIDs, g.NodeID)
			}
			grants[g.NodeID] = g
			hasGrant[p][g.NodeID] = true
		}
		if hasTokenGrant(v) {
			// One identity, one reading: a single card (kind.go).
			out = append(out, tokenCard(v, nodes, now))
			continue
		}
		if len(nodeIDs) == 0 {
			// No login anywhere yet: one card under the reporting node (or none).
			n := ""
			if v.Latest != nil {
				n = v.Latest.ReportingNode
			}
			nodeIDs = append(nodeIDs, n)
		}
		for _, nid := range nodeIDs {
			obs := byNode[nid]
			if obs == nil && v.Latest != nil && len(v.ByNode) == 0 && (v.Latest.ReportingNode == nid || len(nodeIDs) == 1) {
				obs = v.Latest
			}
			if obs != nil && len(obs.Windows) == 0 && obs.Eligibility == contracts.EligibleUnknown {
				// Usage is per account, not per machine: show the newest
				// windows another machine saw (last known).
				if ws := newestWindows(v); ws != nil {
					cp := *obs
					cp.Windows = ws
					obs = &cp
				}
			}
			c := Card{Provider: p, NodeID: nid, Account: &v.Account, Obs: obs, Grant: grants[nid]}
			c.Assessment = Assess(v.Account, obs, c.Grant, now)
			for _, a := range v.Assignments {
				if a.NodeID == nid || len(nodeIDs) == 1 && a.NodeID == "" {
					c.Assignments = append(c.Assignments, a)
				}
			}
			out = append(out, c)
		}
	}
	for _, n := range nodes {
		for _, p := range []contracts.Provider{contracts.ProviderAnthropic, contracts.ProviderOpenAI} {
			if hasGrant[p][n.NodeID] || ownAt[p][n.NodeID] {
				continue
			}
			avail := false
			for _, h := range n.Harnesses {
				if h.Harness == harnessFor(p) && h.Available {
					avail = true
				}
			}
			if !avail {
				continue
			}
			out = append(out, Card{Provider: p, NodeID: n.NodeID,
				Assessment: Assessment{State: Ambient, Why: "usage unknown; Pyre does not observe your own login"}})
		}
	}
	names := map[string]string{}
	for _, n := range nodes {
		names[n.NodeID] = strings.ToLower(n.Name)
	}
	nodeKey := func(id string) string {
		if s, ok := names[id]; ok && s != "" {
			return s
		}
		return id
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		if a.Collapsed() != b.Collapsed() {
			return a.Collapsed() // token accounts (every machine) first
		}
		if a.Collapsed() && a.Machines != b.Machines {
			return a.Machines < b.Machines
		}
		if ka, kb := nodeKey(a.NodeID), nodeKey(b.NodeID); ka != kb {
			return ka < kb
		}
		if (a.Account == nil) != (b.Account == nil) {
			return b.Account == nil // managed accounts before the ambient login
		}
		return strings.ToLower(a.Label()) < strings.ToLower(b.Label())
	})
	return out
}

// Pool summarizes one provider's enabled accounts, each counted once by
// its best login: Ready (available with room), Near (available, near a
// limit), AtLimit, Reauth (needs re-auth) and Unknown. Available and
// Tight (nothing available) are in onuse.go.
type Pool struct {
	Provider                                     contracts.Provider
	Ready, Near, AtLimit, Reauth, Unknown, Total int
}

// rankOf orders login states for the pool: the best login of an account
// decides how it counts.
func rankOf(s State) int {
	switch s {
	case Healthy:
		return 0
	case NearLimit:
		return 1
	case AtLimit:
		return 2
	case NeedsReauth:
		return 4
	}
	return 3
}

// Pools summarizes cards per provider (ambient logins are not counted).
func Pools(cards []Card) []Pool {
	per := map[contracts.Provider]map[string]int{}
	var order []contracts.Provider
	for _, c := range cards {
		if c.Account == nil || c.State == Disabled {
			continue
		}
		if per[c.Provider] == nil {
			per[c.Provider] = map[string]int{}
			order = append(order, c.Provider)
		}
		r := rankOf(c.State)
		if old, ok := per[c.Provider][c.Account.AccountID]; !ok || r < old {
			per[c.Provider][c.Account.AccountID] = r
		}
	}
	sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })
	var out []Pool
	for _, p := range order {
		pl := Pool{Provider: p, Total: len(per[p])}
		for _, r := range per[p] {
			switch r {
			case 0:
				pl.Ready++
			case 1:
				pl.Near++
			case 2:
				pl.AtLimit++
			case 3:
				pl.Unknown++
			default:
				pl.Reauth++
			}
		}
		out = append(out, pl)
	}
	return out
}

// Clock renders t as a wall-clock time in now's location: "5:00 PM"
// today, "Thu 5:00 PM" within a week, "Oct 12 5:00 PM" beyond.
func Clock(now, t time.Time) string {
	lt := t.In(now.Location())
	ln := now.In(now.Location())
	switch {
	case lt.Year() == ln.Year() && lt.YearDay() == ln.YearDay():
		return lt.Format("3:04 PM")
	case lt.Sub(ln) < 6*24*time.Hour && lt.Sub(ln) > -6*24*time.Hour:
		return lt.Format("Mon 3:04 PM")
	}
	return lt.Format("Jan 2 3:04 PM")
}

// In renders the time until t: "in 2h 14m", "in 3d 4h", "in 40s"; a past
// time reads "now".
func In(now, t time.Time) string {
	d := t.Sub(now)
	if d <= 0 {
		return "now"
	}
	return "in " + Span(d)
}

// Span renders a duration with two units at most: "2h 14m", "3d 4h", "9m".
func Span(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		h := int(d.Hours())
		m := int(d.Minutes()) - h*60
		if m == 0 {
			return fmt.Sprintf("%dh", h)
		}
		return fmt.Sprintf("%dh %dm", h, m)
	}
	days := int(d.Hours()) / 24
	h := int(d.Hours()) - days*24
	if h == 0 {
		return fmt.Sprintf("%dd", days)
	}
	return fmt.Sprintf("%dd %dh", days, h)
}

// Resets renders "resets 5:00 PM · in 2h 14m" ("reset passed" when t is
// in the past).
func Resets(now, t time.Time) string {
	if !t.After(now) {
		return "reset passed"
	}
	return "resets " + Clock(now, t) + " · " + In(now, t)
}

func agoWords(now, t time.Time) string {
	d := now.Sub(t)
	if d < time.Minute {
		return "just now"
	}
	return Span(d) + " ago"
}
