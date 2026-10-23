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
}

// placeCand is one eligible node with its load score (higher is busier).
type placeCand struct {
	node  c.Node
	score float64
	why   string
}

// loadScore rates how busy a node is from its last heartbeat: machine CPU
// (or load per core), memory in use, swap in use, memory pressure, and
// live runs (plus pending: runs still starting and launches this process
// just placed there, which the heartbeat has not counted yet). Comparable
// across machines of different sizes; higher is busier. A node that reports
// no machine stats (an older pyred) scores as moderately busy so it never
// wins only by saying nothing.
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
	if cpu == 0 && r.CPUCount > 0 {
		cpu = r.Load1 / float64(r.CPUCount)
	}
	mem := 1 - float64(r.MemAvailMB)/float64(r.MemTotalMB)
	// Each pending launch will take about a gigabyte and some CPU soon.
	s := cpu + mem + 0.05*float64(runs) + 0.1*float64(pending)
	parts := []string{fmt.Sprintf("cpu %.0f%%", cpu*100), fmt.Sprintf("mem %.0f%% (%.1f GB free)", mem*100, float64(r.MemAvailMB)/1024)}
	if r.SwapTotalMB > 0 && r.SwapUsedMB > 0 {
		swap := float64(r.SwapUsedMB) / float64(r.SwapTotalMB)
		s += swap
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
	var cands, windows []placeCand
	var rejected []string
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
		if n.Platform == "windows" && !selfWindows {
			windows = append(windows, placeCand{n, s, why})
			continue
		}
		cands = append(cands, placeCand{n, s, why})
	}
	onlyWindows := false
	if len(cands) == 0 && len(windows) > 0 {
		cands, onlyWindows = windows, true
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
		return Placement{}, c.Errorf(c.ErrUnavailable, "%s; pass --node (see `pyre nodes`)", msg)
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].score != cands[j].score {
			return cands[i].score < cands[j].score
		}
		return allowed[cands[i].node.NodeID] < allowed[cands[j].node.NodeID]
	})
	best := cands[0]
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
	return Placement{NodeID: best.node.NodeID, NodeName: placeName(best.node), Reason: b.String()}, nil
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
// run it on a reserve account (Account.Tier) only. A node whose dry run
// cannot be asked (an older pyred, a timeout) is not rejected. nil maps for
// harnesses without accounts.
func (o *Ops) accountFilter(ctx context.Context, nodes []c.Node, harness, account string, ambient, strict bool) (hard, soft map[string]string) {
	prov := c.ProviderFor(c.Harness(harness))
	if prov == "" {
		return nil, nil
	}
	if !c.ManagedLogins(c.Harness(harness)) {
		// agy: no accounts to weigh, every run is the node's own login (the
		// probe already requires a sign-in). An explicit account can only
		// run where it is that login.
		if account == "" {
			return nil, nil
		}
		acct, err := o.resolveAccount(ctx, account, prov)
		if err != nil {
			return nil, nil // cannot tell: the node decides
		}
		reject := map[string]string{}
		for _, n := range nodes {
			h, _ := nodeHarness(n, harness)
			if !usage.MatchesLogin(acct, h.Login) {
				reject[n.NodeID] = "its own " + harness + " login is not account " + account
			}
		}
		return reject, map[string]string{}
	}
	reject, softRej := map[string]string{}, map[string]string{}
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
		return reject, softRej
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
	return reject, softRej
}

// place picks the node for a launch that named none (see pickPlacement).
func (o *Ops) place(ctx context.Context, p *StartParams, among []string) (Placement, error) {
	page, err := listAll(ctx, func(ctx context.Context, cur string) (c.Page[c.Node], error) {
		return o.API.ListNodes(ctx, c.ListNodesRequest{PageRequest: c.PageRequest{Cursor: cur}})
	})
	if err != nil {
		return Placement{}, err
	}
	return o.placeAmong(ctx, page.Items, p.Harness, p.Account, p.Ambient, p.StrictAccount, among, p.Model != "" || p.Effort != "")
}

// placeAmong checks accounts on the nodes that could take the launch and
// picks the least loaded; a successful pick counts as pending there. Nodes
// holding a non-strict pinned account beat nodes that would fall back to
// another. When accounts rule out every Codex node, the least loaded one is
// still chosen: the harness preflight (ops_harnessfallback.go) then fails
// with the reason or switches to --fallback-harness, without searching the
// nodes again (StartParams.placed).
func (o *Ops) placeAmong(ctx context.Context, nodes []c.Node, harness, account string, ambient, strict bool, among []string, wantModel bool) (Placement, error) {
	if account != "" {
		id, err := o.accountRef(ctx, account, c.ProviderFor(c.Harness(harness)))
		if err != nil {
			return Placement{}, err
		}
		account = id
	}
	pending := o.pendingRuns(ctx)
	var possible []c.Node
	for _, n := range nodes {
		if placeEligible(n, harness, pending[n.NodeID]) && (among == nil || slices.Contains(among, n.NodeID)) {
			possible = append(possible, n)
		}
	}
	hard, soft := o.accountFilter(ctx, possible, harness, account, ambient, strict)
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
	req := placeReq{Harness: harness, Self: o.Self.NodeID, Among: among, Pending: pending, Reject: hard}
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
	if err != nil && len(soft) > 0 {
		req.Reject = hard // the pin falls back to another account there
		pl, err = pickPlacement(nodes, req)
	}
	if err != nil && c.Harness(harness) == c.HarnessCodex && len(hard) > 0 {
		req.Reject = nil
		pl, err = pickPlacement(nodes, req)
		if err == nil {
			pl.Reason += "; no node has a free Codex account (the preflight decides)"
		}
	}
	if err == nil {
		o.placements().record(pl.NodeID, time.Now())
	}
	return pl, err
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

// launchNode resolves the machine for StartSession. A named node wins. A
// --cwd is a path on the caller's machine, so it stays there. With
// --project and --repo the candidates are the nodes holding a recorded
// checkout of the repo. Otherwise every connected node with the harness is
// a candidate, and the session starts in that node's home folder. Among
// candidates the least loaded wins (pickPlacement).
func (o *Ops) launchNode(ctx context.Context, p *StartParams) (string, error) {
	if p.Node != "" || p.Cwd != "" {
		return o.NodeID(ctx, p.Node)
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
	a.warnf("placed on %s: %s (--node picks a machine)", pl.NodeName, pl.Reason)
}
