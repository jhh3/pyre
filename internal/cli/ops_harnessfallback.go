package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
)

// codexUnavailableTitle prefixes the title of a review that runs on Claude
// because no Codex account was free, so the starter sees the swap.
const codexUnavailableTitle = "Codex unavailable, Claude review"

// harnessPreflight runs before a Codex session is created. When the node has
// no free OpenAI account (the account.select dry run reports waiting or
// intervention) the launch fails at once with the reason and the fix,
// instead of creating a session whose run fails (104 such launches, most of
// them review "second opinions" that silently never happened). Without
// --node and --cwd the other connected Codex nodes are tried first and the
// launch moves to the first with a free account (*nodeID changes); with a
// --cwd (a path on the placed node) the error names them instead. With
// FallbackHarness "claude" the session then starts on Claude, labelled in
// its title and first prompt. A node that cannot answer the dry run is not
// blocked: the launch proceeds as before.
func (o *Ops) harnessPreflight(ctx context.Context, nodeID *string, p *StartParams) error {
	fb := strings.ToLower(strings.TrimSpace(p.FallbackHarness))
	if fb != "" && fb != string(c.HarnessClaude) {
		return usagef("--fallback-harness must be claude (got %q)", p.FallbackHarness)
	}
	if c.Harness(p.Harness) != c.HarnessCodex || p.Ambient {
		return nil
	}
	policy := c.AccountPolicy{Mode: c.AccountEligible}
	if p.Account != "" {
		ref, err := o.accountRef(ctx, p.Account, c.ProviderOpenAI)
		if err != nil {
			return err
		}
		policy = accountPolicy(ref, false, p.StrictAccount)
	}
	sel, ok := o.selectOn(ctx, *nodeID, policy)
	if !ok || sel.Outcome == "" {
		return nil
	}
	why := sel.Reason
	if why == "" {
		why = string(sel.Outcome)
	}
	var free []c.Node
	if p.Node == "" && !p.placed { // placement already asked every candidate
		free = o.codexNodesWithRoom(ctx, *nodeID, policy)
	}
	if len(free) > 0 && p.Cwd == "" {
		*nodeID = free[0].NodeID
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
			WithDetail("outcome", string(sel.Outcome))
	}
	p.Harness, p.Account, p.StrictAccount = string(c.HarnessClaude), "", false
	title := p.Title
	if title == "" {
		title = "Codex review"
	}
	p.Title = codexUnavailableTitle + ": " + title
	p.Prompt = fmt.Sprintf("[pyre] %s: no Codex (OpenAI) account was free (%s), so this session runs on Claude instead of Codex. "+
		"You stand in for the requested Codex review: work independently and say in your report that it is a Claude review.\n\n%s",
		codexUnavailableTitle, truncateNote(why, 300), p.Prompt)
	p.Tags = append(append([]string{}, p.Tags...), "codex-unavailable")
	return nil
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
// whose dry run finds a free account.
func (o *Ops) codexNodesWithRoom(ctx context.Context, except string, policy c.AccountPolicy) []c.Node {
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
			free[i] = ok && sel.Outcome == ""
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
