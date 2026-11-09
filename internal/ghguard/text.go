package ghguard

import (
	"fmt"
	"sort"
	"strings"

	"github.com/jhh3/pyre/internal/contracts"
)

// FleetLines summarizes the nodes' GitHub budgets for `pyre nodes` and
// get_context: the freshest rate-limit sample (one login per machine, the
// same account everywhere) and the heaviest gh callers across nodes in
// the last hour. Empty when no node reported either.
func FleetLines(ns []contracts.Node, top int) []string {
	var lines []string
	var fresh *contracts.GitHubBudget
	freshNode := ""
	type caller struct {
		contracts.GitHubCaller
		node string
	}
	var callers []caller
	for _, n := range ns {
		b := n.Resources.GitHub
		if b == nil {
			continue
		}
		if !b.ObservedAt.IsZero() && (fresh == nil || b.ObservedAt.After(fresh.ObservedAt)) {
			fresh, freshNode = b, n.Name
		}
		for _, c := range b.TopSessions {
			callers = append(callers, caller{c, n.Name})
		}
	}
	if fresh != nil {
		l := fmt.Sprintf("GitHub API budget (shared by every agent): %s; sampled on %s at %s", BudgetText(*fresh), freshNode, Clock(fresh.ObservedAt))
		if fresh.Low() {
			l += ". LOW: poll GitHub at most once a minute and skip calls you can avoid"
		}
		lines = append(lines, l)
	}
	sort.SliceStable(callers, func(i, j int) bool { return callers[i].Calls > callers[j].Calls })
	if len(callers) > top {
		callers = callers[:top]
	}
	var parts []string
	for _, c := range callers {
		p := fmt.Sprintf("%s on %s %d", c.SessionID, c.node, c.Calls)
		var x []string
		if c.Throttled > 0 {
			x = append(x, fmt.Sprintf("%d delayed", c.Throttled))
		}
		if c.Refused > 0 {
			x = append(x, fmt.Sprintf("%d refused", c.Refused))
		}
		if len(x) > 0 {
			p += " (" + strings.Join(x, ", ") + ")"
		}
		parts = append(parts, p)
	}
	if len(parts) > 0 {
		lines = append(lines, "Top gh callers, last hour: "+strings.Join(parts, "; "))
	}
	return lines
}
