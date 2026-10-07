package updates

import (
	"context"
	"sync"

	"github.com/jhh3/pyre/internal/contracts"
)

// NodeUpdater performs update actions on a node (the hub implements it with
// nodes.action update.stage / update.activate).
type NodeUpdater interface {
	Stage(ctx context.Context, nodeID string, p contracts.UpdateParams) (contracts.ToolVersion, error)
	Activate(ctx context.Context, nodeID string, p contracts.UpdateParams) (contracts.ToolVersion, error)
}

// RolloutPlan is a canary-then-fleet rollout.
type RolloutPlan struct {
	Canary      string                 `json:"canary"`
	Nodes       []string               `json:"nodes"` // remaining nodes (canary excluded)
	Params      contracts.UpdateParams `json:"params"`
	Concurrency int                    `json:"concurrency"` // default 1
	// ContinueOnError keeps scheduling other nodes after one fails. The
	// default stops scheduling new nodes (in-flight ones finish).
	ContinueOnError bool `json:"continue_on_error"`
}

// NodeOutcome is one node's result.
type NodeOutcome struct {
	NodeID  string                 `json:"node_id"`
	Staged  *contracts.ToolVersion `json:"staged,omitempty"`
	Active  *contracts.ToolVersion `json:"active,omitempty"`
	Error   *contracts.Error       `json:"error,omitempty"`
	Skipped bool                   `json:"skipped,omitempty"`
}

// RolloutReport summarizes a rollout.
type RolloutReport struct {
	Canary  NodeOutcome   `json:"canary"`
	Nodes   []NodeOutcome `json:"nodes"`
	Stopped bool          `json:"stopped"`
	Reason  string        `json:"reason,omitempty"`
}

func runNode(ctx context.Context, u NodeUpdater, node string, p contracts.UpdateParams) NodeOutcome {
	o := NodeOutcome{NodeID: node}
	tv, err := u.Stage(ctx, node, p)
	if err != nil {
		o.Error = contracts.AsError(err)
		return o
	}
	o.Staged = &tv
	ap := p
	ap.Version = tv.Version
	ap.Source = ""
	at, err := u.Activate(ctx, node, ap)
	if err != nil {
		o.Error = contracts.AsError(err)
		return o
	}
	o.Active = &at
	return o
}

// Rollout stages and activates on the canary first; only a healthy canary
// lets the remaining nodes proceed, with bounded concurrency. A node failure
// never touches other nodes' active versions or running sessions.
func Rollout(ctx context.Context, u NodeUpdater, plan RolloutPlan) RolloutReport {
	rep := RolloutReport{Canary: runNode(ctx, u, plan.Canary, plan.Params)}
	rep.Nodes = make([]NodeOutcome, len(plan.Nodes))
	for i, n := range plan.Nodes {
		rep.Nodes[i] = NodeOutcome{NodeID: n, Skipped: true}
	}
	if rep.Canary.Error != nil {
		rep.Stopped, rep.Reason = true, "canary "+plan.Canary+" failed: "+rep.Canary.Error.Message
		return rep
	}
	conc := plan.Concurrency
	if conc <= 0 {
		conc = 1
	}
	var mu sync.Mutex
	stop := false
	sem := make(chan struct{}, conc)
	var wg sync.WaitGroup
	for i, n := range plan.Nodes {
		sem <- struct{}{} // wait for a slot, then decide with the latest outcome
		mu.Lock()
		halted := stop
		mu.Unlock()
		if halted || ctx.Err() != nil {
			<-sem
			break
		}
		wg.Add(1)
		go func(i int, n string) {
			defer wg.Done()
			defer func() { <-sem }()
			o := runNode(ctx, u, n, plan.Params)
			mu.Lock()
			rep.Nodes[i] = o
			if o.Error != nil && !plan.ContinueOnError && !stop {
				stop = true
				rep.Stopped, rep.Reason = true, "node "+n+" failed: "+o.Error.Message
			}
			mu.Unlock()
		}(i, n)
	}
	wg.Wait()
	return rep
}
