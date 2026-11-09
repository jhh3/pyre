package cli

import (
	"context"
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"sync"

	c "github.com/jhh3/pyre/internal/contracts"
)

// Launch requirements (pyre start --needs, MCP start_session needs): a
// command-line tool (doppler, gcloud, docker, ...) or a path (absolute, or
// ~/... under the machine's home) the task cannot run without. Placement
// keeps only nodes that have every one; a named node without one is
// refused, naming the nodes that qualify. A tool a node reports in its tool
// facts (NodeResources.Tools) is read from there; any other tool and every
// path is asked of the node (needs.check). A node that cannot answer (an
// older pyred) does not qualify, with a note saying so.

var needToolRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`)

// needSet is a launch's requirements, split by kind.
type needSet struct {
	tools, paths []string
}

func (n needSet) empty() bool { return len(n.tools)+len(n.paths) == 0 }

// parseNeeds validates --needs values (repeatable; commas also split).
func parseNeeds(raw []string) (needSet, error) {
	var ns needSet
	seen := map[string]bool{}
	for _, r := range raw {
		for _, v := range strings.Split(r, ",") {
			v = strings.TrimSpace(v)
			if v == "" || seen[v] {
				continue
			}
			seen[v] = true
			switch {
			case strings.HasPrefix(v, "/") || strings.HasPrefix(v, "~/"):
				ns.paths = append(ns.paths, v)
			case needToolRe.MatchString(v):
				ns.tools = append(ns.tools, v)
			default:
				return needSet{}, usagef("--needs %q is neither a tool name (e.g. doppler) nor an absolute path (/... or ~/...)", v)
			}
		}
	}
	if len(ns.tools)+len(ns.paths) > 32 {
		return needSet{}, usagef("--needs: at most 32 requirements")
	}
	return ns, nil
}

// needsMissing checks one node: "" when it has every need, else what it
// lacks ("no doppler, no /x") or why it cannot tell.
func (o *Ops) needsMissing(ctx context.Context, n c.Node, ns needSet) string {
	var ask c.NeedsCheckParams
	have := map[string]bool{}
	for _, t := range ns.tools {
		if v, ok := n.Resources.Tools[t]; ok {
			have[t] = v
		} else {
			ask.Tools = append(ask.Tools, t)
		}
	}
	ask.Paths = ns.paths
	if len(ask.Tools)+len(ask.Paths) > 0 {
		params, _ := json.Marshal(ask)
		res, err := o.API.InvokeNodeAction(ctx, c.NodeActionRequest{Mutation: mut(""), NodeID: n.NodeID,
			Action: c.ActionNeedsCheck, Params: params, TimeoutSeconds: 5})
		var out c.NeedsCheckResult
		if err != nil || res.Error != nil || res.Outcome != c.OutcomeSucceeded || json.Unmarshal(res.Result, &out) != nil {
			unknown := append(append([]string(nil), ask.Tools...), ask.Paths...)
			why := "cannot check " + strings.Join(unknown, ", ") + " there"
			switch {
			case err == nil && res.Outcome == c.OutcomeUnsupported:
				why += " (its pyred is older; unknown counts as missing)"
			case err != nil:
				why += " (" + oneLineClip(err.Error(), 100) + "; unknown counts as missing)"
			case res.Error != nil:
				why += " (" + oneLineClip(res.Error.Error(), 100) + "; unknown counts as missing)"
			default:
				why += " (no answer; unknown counts as missing)"
			}
			return why
		}
		for _, t := range ask.Tools {
			have[t] = out.Tools[t]
		}
		for _, p := range ask.Paths {
			have[p] = out.Paths[p]
		}
	}
	var miss []string
	for _, t := range ns.tools {
		if !have[t] {
			miss = append(miss, "no "+t)
		}
	}
	for _, p := range ns.paths {
		if !have[p] {
			miss = append(miss, "no "+p)
		}
	}
	return strings.Join(miss, ", ")
}

// needsFilter splits the connected nodes into those meeting every need and
// the rest (node ID -> "name: what it lacks"), checking nodes in parallel.
// Nodes that are not connected are left out of both.
func (o *Ops) needsFilter(ctx context.Context, nodes []c.Node, ns needSet) (ok []c.Node, lacking map[string]string) {
	lacking = map[string]string{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	good := map[string]bool{}
	for _, n := range nodes {
		if n.Connection.State != c.ConnConnected {
			continue
		}
		wg.Add(1)
		go func(n c.Node) {
			defer wg.Done()
			why := o.needsMissing(ctx, n, ns)
			mu.Lock()
			defer mu.Unlock()
			if why == "" {
				good[n.NodeID] = true
			} else {
				lacking[n.NodeID] = placeName(n) + ": " + why
			}
		}(n)
	}
	wg.Wait()
	for _, n := range nodes {
		if good[n.NodeID] {
			ok = append(ok, n)
		}
	}
	return ok, lacking
}

func (n needSet) String() string {
	return strings.Join(append(append([]string(nil), n.tools...), n.paths...), ", ")
}

// lackingText joins needsFilter's "name: why" entries, one per reason
// ("a, b: no doppler; c: no gcloud").
func lackingText(m map[string]string) string {
	byWhy := map[string][]string{}
	for _, v := range m {
		name, why, _ := strings.Cut(v, ": ")
		byWhy[why] = append(byWhy[why], name)
	}
	out := make([]string, 0, len(byWhy))
	for why, names := range byWhy {
		sort.Strings(names)
		out = append(out, strings.Join(names, ", ")+": "+why)
	}
	sort.Strings(out)
	return strings.Join(out, "; ")
}

func nodeNames(ns []c.Node) []string {
	out := make([]string, 0, len(ns))
	for _, n := range ns {
		out = append(out, placeName(n))
	}
	sort.Strings(out)
	return out
}

// placeNeeds narrows placement candidates to the nodes meeting p's needs;
// with none, the error names what each connected node lacks.
func (o *Ops) placeNeeds(ctx context.Context, nodes []c.Node, raw []string) ([]c.Node, error) {
	ns, err := parseNeeds(raw)
	if err != nil || ns.empty() {
		return nodes, err
	}
	ok, lacking := o.needsFilter(ctx, nodes, ns)
	if len(ok) == 0 {
		return nil, c.Errorf(c.ErrUnavailable, "no connected node has everything in --needs %s, so no session was started (%s)", ns, lackingText(lacking))
	}
	return ok, nil
}

// checkNeeds refuses a launch on a named node (or this one, for --cwd) that
// lacks one of p's needs, naming the connected nodes that qualify.
func (o *Ops) checkNeeds(ctx context.Context, nodeID string, raw []string) error {
	ns, err := parseNeeds(raw)
	if err != nil || ns.empty() {
		return err
	}
	page, err := listAll(ctx, func(ctx context.Context, cur string) (c.Page[c.Node], error) {
		return o.API.ListNodes(ctx, c.ListNodesRequest{PageRequest: c.PageRequest{Cursor: cur}})
	})
	if err != nil {
		return err
	}
	ok, lacking := o.needsFilter(ctx, page.Items, ns)
	why, bad := lacking[nodeID]
	if !bad {
		return nil // has them all, or offline (the launch itself says so)
	}
	where := "no connected node has them all"
	if len(ok) > 0 {
		where = "qualifying: " + strings.Join(nodeNames(ok), ", ") + " (name one with --node, or leave out --node)"
	}
	return c.Errorf(c.ErrUnavailable, "--needs %s: %s, so no session was started; %s", ns, why, where).WithDetail("node_id", nodeID)
}
