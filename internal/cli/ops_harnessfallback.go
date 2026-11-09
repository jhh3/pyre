package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/usage"
)

// harnessPreflight runs before a Codex session is created. When the node has
// no free OpenAI account (the account.select dry run reports waiting or
// intervention, or it picks the node's own login while that login is at its
// 5-hour or weekly limit per the usage cards: noRoomOn) the launch fails at
// once with the reason and the fix, instead of creating a session whose run
// fails (104 such launches, most of them review "second opinions" that
// silently never happened). Without --node and --cwd the other connected
// Codex nodes are tried first and the launch moves to the first with a free
// account (*nodeID changes); with a --cwd (a path on the placed node) the
// error names them instead. With FallbackHarness "claude" the session then
// starts on Claude, labelled in its title, first prompt and placement
// warning. A node that cannot answer the dry run is judged by its usage
// cards alone.
//
// A Codex launch that goes ahead with FallbackHarness set carries
// TagFallbackClaude, so a usage limit at runtime with no other account
// restarts it on Claude (the hub's limit resume, harness fallback mode).
func (o *Ops) harnessPreflight(ctx context.Context, nodeID *string, p *StartParams) error {
	fb := strings.ToLower(strings.TrimSpace(p.FallbackHarness))
	if fb != "" && fb != string(c.HarnessClaude) {
		return usagef("--fallback-harness must be claude (got %q)", p.FallbackHarness)
	}
	if c.Harness(p.Harness) != c.HarnessCodex {
		return nil
	}
	if p.Ambient && fb == "" {
		return nil // checkRoom already refused an own login at its limit
	}
	policy := c.AccountPolicy{Mode: c.AccountEligible}
	if p.Account != "" {
		ref, err := o.accountRef(ctx, p.Account, c.ProviderOpenAI)
		if err != nil {
			return err
		}
		policy = accountPolicy(ref, false, p.StrictAccount)
	}
	room := o.codexRoom(ctx, p)
	why, outcome := "", c.FallbackWaiting
	if p.Ambient {
		why = room.limited(*nodeID)
	} else if sel, ok := o.selectOn(ctx, *nodeID, policy); ok && sel.Outcome != "" {
		why, outcome = sel.Reason, sel.Outcome
		if why == "" {
			why = string(sel.Outcome)
		}
	} else if !ok || sel.GrantID == "" {
		// The node's own login (or no answer): the node does not judge its
		// usage, the cards do.
		why = room.limited(*nodeID)
	}
	if why == "" {
		o.tagFallback(p, fb)
		return nil
	}
	var free []c.Node
	if p.Node == "" && !p.placed && !p.Ambient { // placement already asked every candidate
		free = o.codexNodesWithRoom(ctx, *nodeID, policy, room)
	}
	if len(free) > 0 && p.Cwd == "" {
		*nodeID = free[0].NodeID
		o.tagFallback(p, fb)
		return nil
	}
	if fb == "" {
		hint := ""
		if len(free) > 0 {
			names := make([]string, 0, len(free))
			for _, n := range free {
				names = append(names, codexNodeName(n))
			}
			hint = " A free Codex account exists on " + strings.Join(names, ", ") + " (pass --node and a --cwd there)."
		}
		return c.Errorf(c.ErrCapacityExhausted, "no free Codex (OpenAI) account on this node, so no session was started: %s.%s "+
			"Pass --fallback-harness claude (MCP fallback_harness) to run a review on Claude instead, or start with --harness claude", why, hint).
			WithDetail("outcome", string(outcome))
	}
	p.Harness, p.Account, p.StrictAccount, p.Ambient = string(c.HarnessClaude), "", false, false
	p.Title = c.HarnessFallbackTitle(p.Title)
	p.Prompt = c.HarnessFallbackPrompt("no Codex (OpenAI) account was free ("+truncateNote(why, 300)+")", p.Prompt)
	p.Tags = append(append([]string{}, p.Tags...), c.TagCodexUnavailable)
	if sink, ok := ctx.Value(placementKey{}).(*Placement); ok && sink != nil {
		if sink.NodeID != *nodeID {
			*sink = Placement{NodeID: *nodeID, NodeName: room.name(*nodeID)}
		}
		sink.Warning = joinWarn(sink.Warning, "no Codex (OpenAI) account has room on "+room.name(*nodeID)+" ("+truncateNote(why, 200)+"), so this starts on Claude (--fallback-harness claude)")
	}
	return nil
}

// tagFallback marks a Codex launch that may fall back to Claude at runtime.
func (o *Ops) tagFallback(p *StartParams, fb string) {
	if fb != "" && !slices.Contains(p.Tags, c.TagFallbackClaude) {
		p.Tags = append(slices.Clone(p.Tags), c.TagFallbackClaude)
	}
}

// codexRoomView is the fleet's nodes and OpenAI usage cards, read once per
// preflight. With --force a login at its limit is tried anyway (force).
type codexRoomView struct {
	nodes []c.Node
	cards []usage.Card
	f     roomFilter
	now   time.Time
	force bool
}

func (o *Ops) codexRoom(ctx context.Context, p *StartParams) codexRoomView {
	v := codexRoomView{f: roomFilter{ambient: p.Ambient}, now: o.now(), force: p.Force}
	if p.Account != "" && p.StrictAccount {
		if id, err := o.accountRef(ctx, p.Account, c.ProviderOpenAI); err == nil {
			v.f.account = id
		}
	}
	page, err := listAll(ctx, func(ctx context.Context, cur string) (c.Page[c.Node], error) {
		return o.API.ListNodes(ctx, c.ListNodesRequest{PageRequest: c.PageRequest{Cursor: cur}})
	})
	if err != nil {
		return v
	}
	v.nodes = page.Items
	v.cards = o.usageCards(ctx, v.nodes, string(c.HarnessCodex))
	return v
}

// limited is why no OpenAI login on node id has room ("" when one may).
func (v codexRoomView) limited(id string) string {
	i := slices.IndexFunc(v.nodes, func(n c.Node) bool { return n.NodeID == id })
	if i < 0 || v.force {
		return ""
	}
	why, _, out := noRoomOn(v.cards, v.nodes[i], string(c.HarnessCodex), v.f, v.now)
	if !out {
		return ""
	}
	return why
}

func (v codexRoomView) name(id string) string {
	if i := slices.IndexFunc(v.nodes, func(n c.Node) bool { return n.NodeID == id }); i >= 0 {
		return placeName(v.nodes[i])
	}
	return id
}

// selectOn asks a node for its account.select dry run; ok is false when it
// cannot answer.
func (o *Ops) selectOn(ctx context.Context, nodeID string, policy c.AccountPolicy) (c.AccountSelection, bool) {
	raw, _ := json.Marshal(c.AccountSelectParams{Provider: c.ProviderOpenAI, Policy: policy})
	res, err := o.API.InvokeNodeAction(ctx, c.NodeActionRequest{Mutation: mut(""), NodeID: nodeID, Action: c.ActionAccountSelect, Params: raw, TimeoutSeconds: 15})
	var sel c.AccountSelection
	if err != nil || res.Error != nil || len(res.Result) == 0 || json.Unmarshal(res.Result, &sel) != nil {
		return sel, false
	}
	return sel, true
}

// codexNodesWithRoom lists the other connected nodes with Codex installed
// whose dry run finds a free account (their own login only while it is not
// at its limit).
func (o *Ops) codexNodesWithRoom(ctx context.Context, except string, policy c.AccountPolicy, room codexRoomView) []c.Node {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second) // one deadline for all of them
	defer cancel()
	page, err := listAll(ctx, func(ctx context.Context, cur string) (c.Page[c.Node], error) {
		return o.API.ListNodes(ctx, c.ListNodesRequest{PageRequest: c.PageRequest{Cursor: cur}})
	})
	if err != nil {
		return nil
	}
	var cands []c.Node
	for _, n := range page.Items {
		if n.NodeID != except && n.Connection.State == c.ConnConnected && hasHarness(n, c.HarnessCodex) {
			cands = append(cands, n)
		}
	}
	free := make([]bool, len(cands))
	var wg sync.WaitGroup
	for i, n := range cands {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sel, ok := o.selectOn(ctx, n.NodeID, policy)
			free[i] = ok && sel.Outcome == "" && (sel.GrantID != "" || room.limited(n.NodeID) == "")
		}()
	}
	wg.Wait()
	var out []c.Node
	for i, n := range cands {
		if free[i] {
			out = append(out, n)
		}
	}
	return out
}

func hasHarness(n c.Node, h c.Harness) bool {
	for _, hi := range n.Harnesses {
		if hi.Harness == h && hi.Available {
			return true
		}
	}
	return false
}

func codexNodeName(n c.Node) string {
	if n.Name != "" {
		return n.Name
	}
	return n.NodeID
}

func truncateNote(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// accountDisplay names an account for agent-facing output:
// `"opus" (acct_…)`, so a label never reads as a model name. Falls back to
// the ID when the hub cannot say.
func (o *Ops) accountDisplay(ctx context.Context, id string) string {
	if id == "" {
		return "-"
	}
	page, err := listAll(ctx, func(ctx context.Context, cur string) (c.Page[c.AccountView], error) {
		return o.API.ListAccounts(ctx, c.ListAccountsRequest{PageRequest: c.PageRequest{Cursor: cur}})
	})
	if err != nil {
		return id
	}
	for _, av := range page.Items {
		if av.Account.AccountID == id && av.Account.Label != "" {
			return fmt.Sprintf("%q (%s)", av.Account.Label, id)
		}
	}
	return id
}
