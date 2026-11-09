package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/usage"
)

// Placement is where a launch without --node went and why (printed on
// stderr by the CLI, returned beside the session by MCP start_session).
type Placement struct {
	NodeID   string `json:"node_id"`
	NodeName string `json:"node_name"`
	Reason   string `json:"reason"`
	// Warning notes a start on a node with an avoid flag (a named node, or
	// the only one that fits) or an overloaded one.
	Warning string `json:"warning,omitempty"`
	// overloaded: the pick is past an overload threshold (every eligible
	// node was).
	overloaded bool
}

// placeCand is one eligible node with its load score (higher is busier) and
// why it is overloaded ("" when healthy).
type placeCand struct {
	node  c.Node
	score float64
	why   string
	over  string
}

// Overload thresholds: automatic placement skips a node past any of them
// while another eligible node is healthy, like a soft avoid flag
// (iss_…: new sessions kept landing on a 16-core
// machine at load 38 with 5 GB free while a 32-core one sat at load 2).
const (
	overLoadPerCore = 1.5  // 1- or 5-minute load average (the higher) per core
	overMemFree     = 0.10 // available memory below this share of the total
	overSwapUsed    = 0.5  // swap at least this full, and
	overSwapMinMB   = 4096 // at least this much (macOS keeps small swap files busy)
)

// overloaded says why a node is too busy to take new work, from its last
// heartbeat ("" when it is healthy or reports no machine stats).
func overloaded(r c.NodeResources) string {
	var why []string
	if r.CPUCount > 0 {
		if l := max(r.Load1, r.Load5) / float64(r.CPUCount); l > overLoadPerCore {
			why = append(why, fmt.Sprintf("load %.1f per core", l))
		}
	}
	if r.MemTotalMB > 0 && float64(r.MemAvailMB) < overMemFree*float64(r.MemTotalMB) {
		why = append(why, fmt.Sprintf("%.0f%% memory free (%.1f GB)", 100*float64(r.MemAvailMB)/float64(r.MemTotalMB), float64(r.MemAvailMB)/1024))
	}
	if r.SwapTotalMB > 0 && r.SwapUsedMB >= overSwapMinMB && float64(r.SwapUsedMB) >= overSwapUsed*float64(r.SwapTotalMB) {
		why = append(why, fmt.Sprintf("swap %.1f of %.0f GB used", float64(r.SwapUsedMB)/1024, float64(r.SwapTotalMB)/1024))
	}
	if r.MemPressure == c.MemPressureCritical {
		why = append(why, "memory pressure critical")
	}
	return strings.Join(why, ", ")
}

// loadScore rates how busy a node is from its last heartbeat: machine CPU
// (busy time or load per core, the higher), memory in use, swap (weighed by
// memory in use: swapped-out pages on a machine with plenty free cost
// nothing), memory pressure, live runs per core (plus pending: runs still
// starting and launches this process just placed there, which the heartbeat
// has not counted yet), and how little absolute headroom is left (under 16
// GB free or 8 idle cores), so a small idle machine does not beat a large
// one with far more room. Higher is busier. A node that reports no machine
// stats (an older pyred) scores as moderately busy so it never wins only by
// saying nothing.
func loadScore(n c.Node, pending int) (float64, string) {
	r := n.Resources
	runs := r.ActiveRuns + pending
	runsNote := fmt.Sprintf("%d runs", runs)
	if pending > 0 {
		runsNote = fmt.Sprintf("%d runs (%d starting)", runs, pending)
	}
	if r.MemTotalMB <= 0 {
		return 1.5 + 0.05*float64(runs), "no machine stats, " + runsNote
	}
	cpu := r.CPUPct / 100
	mem := 1 - float64(r.MemAvailMB)/float64(r.MemTotalMB)
	perRun := 0.05
	s := mem
	if r.CPUCount > 0 {
		cores := float64(r.CPUCount)
		cpu = max(cpu, r.Load1/cores)
		perRun = 0.5 / cores
		if idle := cores * (1 - min(cpu, 1)); idle < 8 {
			s += 0.5 * (1 - idle/8)
		}
	}
	// Each pending launch will take about a gigabyte and some CPU soon.
	s += cpu + perRun*float64(runs) + 0.1*float64(pending)
	if free := float64(r.MemAvailMB-int64(pending)*1024) / 1024; free < 16 {
		s += 1 - max(free, 0)/16
	}
	parts := []string{fmt.Sprintf("cpu %.0f%%", cpu*100), fmt.Sprintf("mem %.0f%% (%.1f GB free)", mem*100, float64(r.MemAvailMB)/1024)}
	if r.SwapTotalMB > 0 && r.SwapUsedMB > 0 {
		swap := float64(r.SwapUsedMB) / float64(r.SwapTotalMB)
		s += swap * mem
		parts = append(parts, fmt.Sprintf("swap %.0f%%", swap*100))
	}
	if r.MemAvailMB-int64(pending)*1024 < 2048 {
		s += 1 // a new agent needs about a gigabyte
	}
	switch r.MemPressure {
	case c.MemPressureWarn:
		s += 0.5
		parts = append(parts, "memory pressure warn")
	case c.MemPressureCritical:
		s += 2
		parts = append(parts, "memory pressure critical")
	}
	parts = append(parts, runsNote)
	return s, strings.Join(parts, ", ")
}

// placeEligible reports whether a node can take a harness launch now:
// connected, harness available, under its advertised run capacity.
func placeEligible(n c.Node, harness string, pending int) bool {
	if n.Connection.State != c.ConnConnected {
		return false
	}
	if n.Resources.MaxRuns > 0 && n.Resources.ActiveRuns+pending >= n.Resources.MaxRuns {
		return false
	}
	_, ok := nodeHarness(n, harness)
	return ok
}

func nodeHarness(n c.Node, harness string) (c.HarnessInfo, bool) {
	for _, h := range n.Harnesses {
		if string(h.Harness) == harness {
			return h, h.Available
		}
	}
	return c.HarnessInfo{}, false
}

// selfBonus keeps a launch on the caller's machine when it is about as
// idle as the best one (no reason to move for noise in the samples).
const selfBonus = 0.1

// placeReq is what pickPlacement chooses by.
type placeReq struct {
	Harness string
	Self    string   // the caller's node ("" for a human at a terminal)
	Among   []string // nil: any node; else only these (order breaks ties)
	// Pending counts runs per node the heartbeat has not reported yet.
	Pending map[string]int
	// Reject names nodes whose accounts cannot run the launch, with why
	// (accountFilter; nil = not checked).
	Reject map[string]string
	// Avoid names nodes with a soft avoid flag (or an overridden hard
	// one) for the launch, with a description. They are skipped unless
	// UseAvoided, and then chosen only when no other node is eligible.
	Avoid      map[string]string
	UseAvoided bool
}

// pickPlacement chooses the least-loaded eligible node. Windows nodes are
// left out unless the caller runs on Windows or nothing else is eligible:
// agents' prompts and shell habits assume a unix shell.
func pickPlacement(nodes []c.Node, r placeReq) (Placement, error) {
	allowed := map[string]int{}
	for i, id := range r.Among {
		if _, ok := allowed[id]; !ok {
			allowed[id] = i
		}
	}
	selfWindows := false
	for _, n := range nodes {
		selfWindows = selfWindows || n.NodeID == r.Self && n.Platform == "windows"
	}
	var cands, windows, avoided []placeCand
	var rejected, skipped []string
	for _, n := range nodes {
		if r.Among != nil {
			if _, ok := allowed[n.NodeID]; !ok {
				continue
			}
		}
		if !placeEligible(n, r.Harness, r.Pending[n.NodeID]) {
			continue
		}
		if why, bad := r.Reject[n.NodeID]; bad {
			rejected = append(rejected, placeName(n)+": "+why)
			continue
		}
		s, why := loadScore(n, r.Pending[n.NodeID])
		if n.NodeID == r.Self {
			s -= selfBonus
		}
		cand := placeCand{n, s, why, overloaded(n.Resources)}
		if desc, ok := r.Avoid[n.NodeID]; ok {
			if r.UseAvoided {
				avoided = append(avoided, cand)
			} else {
				skipped = append(skipped, placeName(n)+" ("+desc+")")
			}
			continue
		}
		if n.Platform == "windows" && !selfWindows {
			windows = append(windows, cand)
			continue
		}
		cands = append(cands, cand)
	}
	onlyWindows, onlyAvoided := false, false
	if len(cands) == 0 && len(windows) > 0 {
		cands, onlyWindows = windows, true
	}
	if len(cands) == 0 && len(avoided) > 0 {
		cands, onlyAvoided, onlyWindows = avoided, true, false
	}
	if len(cands) == 0 {
		where := "connected node"
		if r.Among != nil {
			where = "connected node holding the checkout"
		}
		msg := fmt.Sprintf("no %s can run %s now", where, r.Harness)
		if len(rejected) > 0 {
			msg += " (" + strings.Join(rejected, "; ") + ")"
		}
		if len(skipped) > 0 {
			msg += "; avoided: " + strings.Join(skipped, "; ")
		}
		return Placement{}, c.Errorf(c.ErrUnavailable, "%s; pass --node (see `pyre nodes`)", msg)
	}
	// Healthy nodes first; an overloaded one only when every candidate is.
	sort.SliceStable(cands, func(i, j int) bool {
		if oi, oj := cands[i].over != "", cands[j].over != ""; oi != oj {
			return oj
		}
		if cands[i].score != cands[j].score {
			return cands[i].score < cands[j].score
		}
		return allowed[cands[i].node.NodeID] < allowed[cands[j].node.NodeID]
	})
	best := cands[0]
	var over []string // overloaded candidates passed over
	if best.over == "" {
		healthy := cands[:0:0]
		for _, o := range cands {
			if o.over == "" {
				healthy = append(healthy, o)
			} else {
				over = append(over, placeName(o.node)+" ("+o.over+")")
			}
		}
		cands = healthy
	}
	var b strings.Builder
	switch {
	case len(cands) == 1 && r.Among != nil:
		b.WriteString("the only connected node with the checkout and " + r.Harness)
	case len(cands) == 1:
		b.WriteString("the only connected node with " + r.Harness)
	default:
		b.WriteString("least loaded of " + fmt.Sprint(len(cands)) + " eligible nodes")
		if r.Among != nil {
			b.WriteString(" holding the checkout")
		}
	}
	if r.Reject != nil {
		b.WriteString(" and a usable account")
	}
	if onlyWindows {
		b.WriteString(", Windows (no other node is eligible)")
	}
	warning := ""
	if onlyAvoided {
		desc := r.Avoid[best.node.NodeID]
		b.WriteString(", although it is avoided (" + desc + "): no other node is eligible")
		warning = placeName(best.node) + " is avoided (" + desc + "); no other node could take this launch"
	}
	if best.over != "" {
		every := "every eligible node is overloaded"
		if !onlyWindows && slices.ContainsFunc(windows, func(w placeCand) bool { return w.over == "" }) {
			every = "every eligible non-Windows node is overloaded (a Windows one is not: name it with --node)"
		}
		b.WriteString(", although it is overloaded (" + best.over + "): " + every)
		warning = joinWarn(warning, placeName(best.node)+" is overloaded ("+best.over+"); "+every+", so it starts there anyway")
	}
	b.WriteString(" (" + best.why + ")")
	if len(cands) > 1 {
		var others []string
		for _, o := range cands[1:min(len(cands), 4)] {
			others = append(others, fmt.Sprintf("%s: %s", placeName(o.node), o.why))
		}
		b.WriteString("; others: " + strings.Join(others, "; "))
	}
	if len(windows) > 0 && !onlyWindows {
		b.WriteString(fmt.Sprintf("; %d Windows node(s) skipped (name one with --node)", len(windows)))
	}
	if len(rejected) > 0 {
		b.WriteString("; no usable account on " + strings.Join(rejected, "; "))
	}
	if len(over) > 0 {
		b.WriteString("; skipped overloaded " + strings.Join(over, "; "))
	}
	if len(skipped) > 0 {
		b.WriteString("; skipped avoided " + strings.Join(skipped, "; "))
	}
	return Placement{NodeID: best.node.NodeID, NodeName: placeName(best.node), Reason: b.String(), Warning: warning, overloaded: best.over != ""}, nil
}

func placeName(n c.Node) string {
	if n.Name != "" {
		return n.Name
	}
	return n.NodeID
}

// placementLog counts launches one Ops (a CLI process, an MCP server)
// placed per node in the last two minutes: a burst of starts from one
// coordinator would otherwise all see the same heartbeat and land on one
// node.
type placementLog struct {
	mu sync.Mutex
	at map[string][]time.Time
}

const recentWindow = 2 * time.Minute

var placementLogs sync.Map // *Ops -> *placementLog

func (o *Ops) placements() *placementLog {
	l, _ := placementLogs.LoadOrStore(o, &placementLog{at: map[string][]time.Time{}})
	return l.(*placementLog)
}

func (l *placementLog) record(nodeID string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.at[nodeID] = append(l.at[nodeID], now)
}

func (l *placementLog) recent(now time.Time) map[string]int {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := map[string]int{}
	for id, ts := range l.at {
		keep := ts[:0]
		for _, t := range ts {
			if now.Sub(t) < recentWindow {
				keep = append(keep, t)
			}
		}
		l.at[id] = keep
		out[id] = len(keep)
	}
	return out
}

// pendingRuns adds runs still starting (the hub knows them before the next
// heartbeat does) to this process's recent placements.
func (o *Ops) pendingRuns(ctx context.Context) map[string]int {
	pending := o.placements().recent(time.Now())
	page, err := o.ListSessions(ctx, SessionFilter{States: []string{string(c.RunStarting)}})
	if err != nil {
		return pending
	}
	starting := map[string]int{}
	for _, s := range page.Items {
		if s.Run != nil {
			starting[s.Run.NodeID]++
		}
	}
	for id, n := range starting {
		pending[id] = max(pending[id], n) // a run this process placed is also starting
	}
	return pending
}

// accountFilter names the candidate nodes whose accounts cannot run the
// launch, asking each node's account selector for a dry run (it knows
// device grants, token accounts and limits). hard: no eligible account now
// and no own login (used when the node has no Pyre account for the
// provider), or --ambient without an own login. soft: an explicit account
// is not the one selected there although another is eligible (a non-strict
// pin falls back to it; a strict pin makes this hard), or the node would
// run it on a reserve account (Account.Tier) only. room: the nodes whose
// dry run found an eligible account (placeAmong trusts it over the usage
// cards). A node whose dry run cannot be asked (an older pyred, a timeout)
// is not rejected. nil maps for harnesses without accounts.
func (o *Ops) accountFilter(ctx context.Context, nodes []c.Node, harness, account string, ambient, strict bool) (hard, soft map[string]string, room map[string]bool) {
	prov := c.ProviderFor(c.Harness(harness))
	if prov == "" {
		return nil, nil, nil
	}
	if !c.ManagedLogins(c.Harness(harness)) {
		// agy: no accounts to weigh, every run is the node's own login (the
		// probe already requires a sign-in). An explicit account can only
		// run where it is that login.
		if account == "" {
			return nil, nil, nil
		}
		acct, err := o.resolveAccount(ctx, account, prov)
		if err != nil {
			return nil, nil, nil // cannot tell: the node decides
		}
		reject := map[string]string{}
		for _, n := range nodes {
			h, _ := nodeHarness(n, harness)
			if !usage.MatchesLogin(acct, h.Login) {
				reject[n.NodeID] = "its own " + harness + " login is not account " + account
			}
		}
		return reject, map[string]string{}, nil
	}
	reject, softRej, room := map[string]string{}, map[string]string{}, map[string]bool{}
	login := func(n c.Node) bool {
		h, _ := nodeHarness(n, harness)
		return h.Login != nil
	}
	if ambient {
		for _, n := range nodes {
			if !login(n) {
				reject[n.NodeID] = "no own " + harness + " login (--ambient)"
			}
		}
		return reject, softRej, nil
	}
	pol := c.AccountPolicy{Mode: c.AccountEligible}
	if account != "" {
		pol = c.AccountPolicy{Mode: c.AccountExplicit, AccountID: account, Strict: strict}
	}
	params, _ := json.Marshal(c.AccountSelectParams{Provider: prov, Policy: pol})
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, n := range nodes {
		wg.Add(1)
		go func(n c.Node) {
			defer wg.Done()
			res, err := o.API.InvokeNodeAction(ctx, c.NodeActionRequest{Mutation: mut(""), NodeID: n.NodeID,
				Action: c.ActionAccountSelect, Params: params, TimeoutSeconds: 5})
			var sel c.AccountSelection
			if err != nil || res.Error != nil || res.Outcome != c.OutcomeSucceeded || json.Unmarshal(res.Result, &sel) != nil {
				return // cannot tell: not a reason to skip the node
			}
			eligible := sel.AccountID != "" && sel.Outcome == ""
			if eligible {
				mu.Lock()
				room[n.NodeID] = true
				mu.Unlock()
			}
			why, into := "", reject
			switch {
			case account != "" && sel.AccountID != account && eligible && !strict:
				why, into = "account "+account+" is not usable there", softRej
			case account != "" && sel.AccountID != account:
				why = "account " + account + " is not usable there"
			case account == "" && !eligible && !login(n):
				why = "no eligible " + string(prov) + " account now and no own login"
			case account == "" && eligible && sel.Reserve:
				// Usable, but only on a reserve account: a node where a
				// normal account has room goes first (soft, like a pin
				// that would fall back).
				why, into = "only a reserve "+string(prov)+" account has room there", softRej
			}
			if why == "" {
				return
			}
			if sel.Reason != "" {
				why += " (" + oneLineClip(sel.Reason, 80) + ")"
			}
			mu.Lock()
			into[n.NodeID] = why
			mu.Unlock()
		}(n)
	}
	wg.Wait()
	return reject, softRej, room
}

// place picks the node for a launch that named none (see pickPlacement).
func (o *Ops) place(ctx context.Context, p *StartParams, among []string) (Placement, error) {
	page, err := listAll(ctx, func(ctx context.Context, cur string) (c.Page[c.Node], error) {
		return o.API.ListNodes(ctx, c.ListNodesRequest{PageRequest: c.PageRequest{Cursor: cur}})
	})
	if err != nil {
		return Placement{}, err
	}
	nodes, err := o.placeNeeds(ctx, page.Items, p.Needs) // needs.go
	if err != nil {
		return Placement{}, err
	}
	pl, err := o.placeAmong(ctx, nodes, p.Harness, p.Account, p.Ambient, p.StrictAccount, among, p.Model != "" || p.Effort != "",
		o.avoidProjectFor(ctx, page.Items, p.Project), p.IgnoreAvoid, p.Force, p.FallbackHarness != "")
	if len(p.Needs) > 0 {
		ns, _ := parseNeeds(p.Needs)
		if err == nil {
			pl.Reason += "; only nodes with " + ns.String() + " (" + strings.Join(nodeNames(nodes), ", ") + ")"
		} else {
			err = c.Errorf(c.CodeOf(err), "%s; considered only nodes with %s: %s", c.AsError(err).Message, ns, strings.Join(nodeNames(nodes), ", "))
		}
	}
	return pl, err
}

// noRoomHint ends a refusal to start a harness where no login has room
// (decisions I72, I74).
const noRoomHint = "try another --harness, or wait for a reset (--force starts it anyway)"

// resetWhen words a reset in the owner's zone (askZone, Pacific): "Nov 1
// 10:49 PM PDT (in 6d 7h)".
func resetWhen(t time.Time, now time.Time) string {
	r := t.In(askZone)
	return usage.Clock(now.In(askZone), r) + " " + r.Format("MST") + " (" + usage.In(now, r) + ")"
}

// roomFilter is which logins count for a launch: only the node's own login
// (--ambient), only one account (a strict --account: its ID), else all.
type roomFilter struct {
	ambient bool
	account string
}

// usageCards are the account usage cards (`pyre accounts`) of harness's
// provider on nodes; nil for a harness without accounts. A hub that cannot
// list accounts leaves only the nodes' own logins (unobserved: room).
func (o *Ops) usageCards(ctx context.Context, nodes []c.Node, harness string) []usage.Card {
	prov := c.ProviderFor(c.Harness(harness))
	if prov == "" {
		return nil
	}
	page, _ := listAll(ctx, func(ctx context.Context, cur string) (c.Page[c.AccountView], error) {
		return o.API.ListAccounts(ctx, c.ListAccountsRequest{PageRequest: c.PageRequest{Cursor: cur}, Provider: prov})
	})
	return usage.Cards(page.Items, nodes, o.now())
}

// noRoomOn reports that node n has no login for harness with room, from
// its usage cards (the node's own login counts too): every login there that
// f lets run it is at its 5-hour or weekly limit until a reset ahead, or
// cannot run at all (disabled, needs re-auth), and at least one is at a
// limit. A login whose usage is unknown or not observed may have room, and
// a limit without a reset does not count (nothing says when it ends). why
// names each login at its limit with its reset, earliest first.
func noRoomOn(cards []usage.Card, n c.Node, harness string, f roomFilter, now time.Time) (why string, reset time.Time, out bool) {
	prov := c.ProviderFor(c.Harness(harness))
	type limit struct {
		label string
		until time.Time
	}
	var at []limit
	for _, cd := range cards {
		if cd.Provider != prov || cd.NodeID != n.NodeID && !slices.Contains(cd.TokenNodes, n.NodeID) {
			continue
		}
		own := cd.Account == nil || cd.Own
		if f.ambient && !own || f.account != "" && (cd.Account == nil || cd.Account.AccountID != f.account) {
			continue
		}
		switch {
		case cd.State == usage.AtLimit && cd.Until != nil && cd.Until.After(now):
			label := cd.Label()
			if cd.Account == nil {
				label = "the own " + harness + " login"
			} else if own {
				label += " (own login)"
			}
			at = append(at, limit{label, *cd.Until})
		case cd.State == usage.Disabled || cd.State == usage.NeedsReauth:
		default:
			return "", time.Time{}, false // room, or it may have room
		}
	}
	if len(at) == 0 {
		return "", time.Time{}, false
	}
	sort.SliceStable(at, func(i, j int) bool { return at[i].until.Before(at[j].until) })
	parts := make([]string, len(at))
	for i, l := range at {
		parts[i] = l.label + " until " + resetWhen(l.until, now)
	}
	return "no " + string(prov) + " account with room for " + harness + " (at its limit: " + strings.Join(parts, ", ") + ")", at[0].until, true
}

// checkRoom refuses a launch on a named node (or this one, for --cwd)
// where no login for the harness has room (noRoomOn), unless p.Force; with
// Force it starts with a warning. The refusal names the logins at their
// limits with their resets and the nodes where a login has room. A node
// that cannot be read is not refused (the run's own failure says why). A
// Codex launch with --fallback-harness is left to the harness preflight,
// which starts it on Claude.
func (o *Ops) checkRoom(ctx context.Context, nodeID string, p *StartParams) error {
	prov := c.ProviderFor(c.Harness(p.Harness))
	if prov == "" {
		return nil
	}
	page, err := listAll(ctx, func(ctx context.Context, cur string) (c.Page[c.Node], error) {
		return o.API.ListNodes(ctx, c.ListNodesRequest{PageRequest: c.PageRequest{Cursor: cur}})
	})
	if err != nil {
		return nil
	}
	i := slices.IndexFunc(page.Items, func(n c.Node) bool { return n.NodeID == nodeID })
	if i < 0 {
		return nil
	}
	n := page.Items[i]
	f := roomFilter{ambient: p.Ambient}
	if p.Account != "" && p.StrictAccount {
		if id, err := o.accountRef(ctx, p.Account, prov); err == nil {
			f.account = id
		}
	}
	now := o.now()
	cards := o.usageCards(ctx, page.Items, p.Harness)
	why, reset, out := noRoomOn(cards, n, p.Harness, f, now)
	if !out {
		return nil
	}
	if !p.Force && p.FallbackHarness != "" && c.Harness(p.Harness) == c.HarnessCodex {
		return nil // the harness preflight starts it on Claude instead
	}
	text := placeName(n) + " has " + why
	if !p.Force {
		var room []string
		for _, m := range page.Items {
			if m.NodeID == nodeID || m.Connection.State != c.ConnConnected || !hasHarness(m, c.Harness(p.Harness)) {
				continue
			}
			if _, _, out := noRoomOn(cards, m, p.Harness, f, now); !out {
				room = append(room, placeName(m))
			}
		}
		sort.Strings(room)
		where := "no other machine has one either"
		if len(room) > 0 {
			where = "one has room on " + strings.Join(room, ", ") + " (name it with --node, or leave out --node)"
		}
		return c.Errorf(c.ErrCapacityExhausted, "%s, so no session was started; %s; %s", text, where, noRoomHint).
			WithDetail("node_id", nodeID).WithDetail("reset_at", reset.UTC().Format(time.RFC3339))
	}
	if sink, ok := ctx.Value(placementKey{}).(*Placement); ok && sink != nil {
		if sink.NodeID != nodeID {
			*sink = Placement{NodeID: nodeID, NodeName: placeName(n)}
		}
		sink.Warning = joinWarn(sink.Warning, text+"; starting there anyway (--force)")
	}
	return nil
}

func joinWarn(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}

// placeAmong checks accounts on the nodes that could take the launch and
// picks the least loaded; a successful pick counts as pending there. Nodes
// holding a non-strict pinned account beat nodes that would fall back to
// another, unless every such node is overloaded (pickPlacement): a healthy
// node on a fallback or reserve account beats an overloaded one. When accounts rule out every Codex node, the least loaded one is
// still chosen: the harness preflight (ops_harnessfallback.go) then fails
// with the reason or switches to --fallback-harness, without searching the
// nodes again (StartParams.placed).
//
// Avoid flags for project (node_avoid.go): a node with a hard flag is left
// out (unless ignoreAvoid and the caller may override it, when it counts as
// soft); soft-avoided nodes are tried only when every pass without them
// finds nothing, so an avoided node never beats a node that would need a
// fallback account.
//
// A node with no login for the harness that has room (noRoomOn: every
// account there, its own login included, at its 5-hour or weekly limit,
// and its dry run found none either) is out until a reset; with force it
// is tried last, with a warning. When every candidate is out for that
// reason the error names each node's logins and resets (decisions I72,
// I74). fallback (--fallback-harness) lets the Codex pass below pick such
// a node anyway: the preflight then switches the launch to Claude.
func (o *Ops) placeAmong(ctx context.Context, nodes []c.Node, harness, account string, ambient, strict bool, among []string, wantModel bool, project string, ignoreAvoid, force, fallback bool) (Placement, error) {
	if account != "" {
		id, err := o.accountRef(ctx, account, c.ProviderFor(c.Harness(harness)))
		if err != nil {
			return Placement{}, err
		}
		account = id
	}
	avoid, hardAvoid := o.avoidedNodes(nodes, project, ignoreAvoid)
	if len(hardAvoid) > 0 {
		var keep []c.Node
		for _, n := range nodes {
			if _, out := hardAvoid[n.NodeID]; !out {
				keep = append(keep, n)
			}
		}
		nodes = keep
	}
	pending := o.pendingRuns(ctx)
	var possible []c.Node
	for _, n := range nodes {
		if placeEligible(n, harness, pending[n.NodeID]) && (among == nil || slices.Contains(among, n.NodeID)) {
			possible = append(possible, n)
		}
	}
	hard, soft, room := o.accountFilter(ctx, possible, harness, account, ambient, strict)
	if wantModel {
		// --model/--effort: only machines whose pyred applies them (an older
		// one passes the args to the harness, which exits).
		for _, n := range possible {
			if _, ok := modelSupport(n, harness); !ok {
				if hard == nil {
					hard = map[string]string{}
				}
				hard[n.NodeID] = "its pyred cannot apply --model/--effort for " + harness + " (an older version)"
			}
		}
	}
	now := o.now()
	cards := o.usageCards(ctx, possible, harness)
	f := roomFilter{ambient: ambient}
	if strict {
		f.account = account
	}
	limited := map[string]string{} // node -> "laptop has no openai account with room for codex (…)"
	for _, n := range possible {
		if room[n.NodeID] {
			continue // its dry run found an account with room
		}
		why, _, out := noRoomOn(cards, n, harness, f, now)
		if !out {
			continue
		}
		limited[n.NodeID] = placeName(n) + " has " + why
		into := &hard
		if force {
			into = &soft // tried only when no other node fits
		}
		if *into == nil {
			*into = map[string]string{}
		}
		if _, gone := hard[n.NodeID]; !gone {
			(*into)[n.NodeID] = why
		}
	}
	passes := func(useAvoided bool) (Placement, error) {
		req := placeReq{Harness: harness, Self: o.Self.NodeID, Among: among, Pending: pending, Reject: hard, Avoid: avoid, UseAvoided: useAvoided}
		if len(soft) > 0 {
			req.Reject = map[string]string{}
			for id, why := range hard {
				req.Reject[id] = why
			}
			for id, why := range soft {
				req.Reject[id] = why
			}
		}
		pl, err := pickPlacement(nodes, req)
		if (err != nil || pl.overloaded) && len(soft) > 0 {
			// The pin falls back to another account there (or a reserve one
			// runs it): still better than an overloaded machine.
			req.Reject = hard
			if pl2, err2 := pickPlacement(nodes, req); err2 == nil && (err != nil || !pl2.overloaded) {
				pl, err = pl2, err2
			}
		}
		if err != nil && c.Harness(harness) == c.HarnessCodex && len(hard) > 0 {
			req.Reject = nil
			if !fallback && !force {
				// A node with every account at its limit stays out: the
				// refusal below names the resets.
				for id := range limited {
					if req.Reject == nil {
						req.Reject = map[string]string{}
					}
					req.Reject[id] = hard[id]
				}
			}
			pl, err = pickPlacement(nodes, req)
			if err == nil {
				pl.Reason += "; no node has a free Codex account (the preflight decides)"
			}
		}
		return pl, err
	}
	pl, err := passes(false)
	if err != nil && len(avoid) > 0 {
		pl, err = passes(true) // nothing else fits: an avoided node, with a warning
	}
	if err != nil && len(hardAvoid) > 0 {
		var names []string
		for _, why := range hardAvoid {
			names = append(names, why)
		}
		sort.Strings(names)
		err = c.Errorf(c.CodeOf(err), "%s; hard-avoided: %s (pyre node avoid; --ignore-avoid overrides)", c.AsError(err).Message, strings.Join(names, "; "))
	}
	if err != nil && !force && len(limited) > 0 {
		if len(limited) == len(possible) {
			texts := make([]string, 0, len(limited))
			for _, t := range limited {
				texts = append(texts, t)
			}
			sort.Strings(texts)
			err = c.Errorf(c.ErrCapacityExhausted, "%s, so no session was started; %s", strings.Join(texts, "; "), noRoomHint)
		} else {
			err = c.Errorf(c.CodeOf(err), "%s; --force starts it on a machine at its limit anyway", c.AsError(err).Message)
		}
	}
	if err == nil {
		if t, ok := limited[pl.NodeID]; ok {
			pl.Warning = joinWarn(pl.Warning, t+"; starting there anyway (--force): no other node could take this launch")
		}
		o.placements().record(pl.NodeID, time.Now())
	}
	return pl, err
}

// avoidedNodes splits the nodes with an avoid flag in force for a launch in
// project: soft (or a hard flag the caller overrides with ignoreAvoid) and
// hard, each with a description. The caller is the owner unless it runs on
// a session credential.
func (o *Ops) avoidedNodes(nodes []c.Node, project string, ignoreAvoid bool) (soft, hard map[string]string) {
	now := o.now()
	byOwner := o.ClientCredential || o.Self.SessionID == ""
	for _, n := range nodes {
		a, ok := c.EffectiveAvoid(n.Avoid, project, now)
		if !ok {
			continue
		}
		desc := avoidBrief(a, now)
		if a.Hard && !(ignoreAvoid && a.Overridable(byOwner)) {
			if hard == nil {
				hard = map[string]string{}
			}
			hard[n.NodeID] = placeName(n) + " (" + desc + ")"
			continue
		}
		if soft == nil {
			soft = map[string]string{}
		}
		soft[n.NodeID] = desc
	}
	return soft, hard
}

// avoidProjectFor is the project a launch is placed for, when some node has
// a project-scoped avoid flag (else "", and no lookups): the named project,
// else the calling session's own (a child joins its parent's project).
func (o *Ops) avoidProjectFor(ctx context.Context, nodes []c.Node, project string) string {
	scoped := false
	for _, n := range nodes {
		for _, a := range n.Avoid {
			scoped = scoped || a.ProjectID != nil && *a.ProjectID != ""
		}
	}
	if !scoped {
		return ""
	}
	if project != "" {
		if pid, err := o.projectPtr(ctx, project); err == nil && pid != nil {
			return *pid
		}
		return ""
	}
	if !o.ClientCredential && o.Self.SessionID != "" {
		if sv, err := o.API.GetSession(ctx, c.GetRequest{ID: o.Self.SessionID}); err == nil && sv.Session.ProjectID != nil {
			return *sv.Session.ProjectID
		}
	}
	return ""
}

type placementKey struct{}

// withPlacement asks launches under ctx to record where they placed a
// session that named no node (the CLI prints it, MCP returns it).
func withPlacement(ctx context.Context, sink *Placement) context.Context {
	return context.WithValue(ctx, placementKey{}, sink)
}

func notePlacement(ctx context.Context, pl Placement) {
	if sink, ok := ctx.Value(placementKey{}).(*Placement); ok && sink != nil {
		*sink = pl
	}
}

// warnNamed notes a start on a named node (or this one, for --cwd) that has
// an avoid flag in force for the launch, or is overloaded: a soft flag or an
// overload starts anyway with a warning, a hard flag is the hub's to refuse
// (sessions.start) unless overridden, which is noted too. A read failure
// warns nothing.
func (o *Ops) warnNamed(ctx context.Context, nodeID string, p *StartParams) {
	sink, ok := ctx.Value(placementKey{}).(*Placement)
	if !ok || sink == nil {
		return
	}
	n, err := o.API.GetNode(ctx, c.GetRequest{ID: nodeID})
	if err != nil {
		return
	}
	var warn []string
	if a, ok := c.EffectiveAvoid(n.Avoid, o.avoidProjectFor(ctx, []c.Node{n}, p.Project), o.now()); ok && (!a.Hard || p.IgnoreAvoid && a.Overridable(o.ClientCredential || o.Self.SessionID == "")) {
		what := "starting there anyway"
		if a.Hard {
			what = "starting there anyway (--ignore-avoid)"
		}
		warn = append(warn, fmt.Sprintf("%s is avoided (%s); %s", placeName(n), avoidBrief(a, o.now()), what))
	}
	if why := overloaded(n.Resources); why != "" {
		how := "it was named with --node"
		if p.Node == "" {
			how = "a start with --cwd stays on the caller's machine; leave out --cwd (or start with --repo) to let Pyre place it on a healthy one"
		}
		warn = append(warn, fmt.Sprintf("%s is overloaded (%s); %s", placeName(n), why, how))
	}
	if len(warn) == 0 {
		return
	}
	if sink.NodeID != nodeID {
		*sink = Placement{NodeID: nodeID, NodeName: placeName(n)}
	}
	sink.Warning = strings.Join(warn, "; ")
}

// launchNode resolves the machine for StartSession. A named node wins. A
// --cwd is a path on the caller's machine, so it stays there. With
// --project and --repo the candidates are the nodes holding a recorded
// checkout of the repo. Otherwise every connected node with the harness is
// a candidate, and the session starts in that node's home folder. Among
// candidates the least loaded wins (pickPlacement).
func (o *Ops) launchNode(ctx context.Context, p *StartParams) (string, error) {
	if p.Node != "" || p.Cwd != "" {
		id, err := o.NodeID(ctx, p.Node)
		if err == nil {
			o.warnNamed(ctx, id, p)
		}
		return id, err
	}
	if p.Repo == "" && p.Worktree == "" {
		p.Cwd = "~"
	}
	var among []string
	if p.Repo != "" && p.Project != "" {
		pid, err := o.projectPtr(ctx, p.Project)
		if err != nil || pid == nil {
			return o.NodeID(ctx, "")
		}
		proj, err := o.API.GetProject(ctx, c.GetRequest{ID: *pid})
		if err != nil {
			return o.NodeID(ctx, "")
		}
		among = []string{}
		for _, b := range proj.Checkouts {
			if b.RepoID == p.Repo {
				among = append(among, b.NodeID)
			}
		}
		if len(among) == 0 {
			return o.NodeID(ctx, "") // launchCheckout explains the missing checkout
		}
	}
	pl, err := o.place(ctx, p, among)
	if err != nil {
		return "", err
	}
	p.placed = true // accounts were checked on every candidate
	notePlacement(ctx, pl)
	return pl.NodeID, nil
}

// StartResult is MCP start_session's answer: the launch, plus where it
// was placed when no node was named.
type StartResult struct {
	c.StartSessionResponse
	Placement *Placement `json:"placement,omitempty"`
}

// notePlaced tells the launcher (stderr, never in --json output) which
// machine a launch without --node went to and why.
func (a *App) notePlaced(pl *Placement) {
	if pl == nil || pl.NodeID == "" {
		return
	}
	if pl.Reason != "" {
		a.warnf("placed on %s: %s (--node picks a machine)", pl.NodeName, pl.Reason)
	}
	if pl.Warning != "" {
		a.warnf("warning: %s", pl.Warning)
	}
}
