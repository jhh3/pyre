package cli

import (
	"context"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	"github.com/spf13/cobra"

	c "github.com/jhh3/pyre/internal/contracts"
)

// nodeAutoStatus is one node's automatic updater state (`pyre update auto`).
type nodeAutoStatus struct {
	NodeID string               `json:"node_id"`
	Name   string               `json:"name,omitempty"`
	Tools  []c.AutoUpdateStatus `json:"tools"`
	Error  string               `json:"error,omitempty"`
}

// autoPollEvery and autoWaitMax bound how `pyre update auto --now` waits
// for the nodes' checks (a stage downloads ~200 MB and health-checks it).
var (
	autoPollEvery = 5 * time.Second
	autoWaitMax   = 30 * time.Minute
)

func (a *App) updateAutoCommand(node, tool *string) *cobra.Command {
	var now bool
	cmd := &cobra.Command{
		Use:   "auto [--tool codex] [--node N] [--now]",
		Short: "Automatic updates (codex): show each node's latest-release check, or run one now",
		Long: `Every node checks for the newest stable Codex release soon after pyred starts
and daily, and stages, health-checks and activates it for new runs (running
sessions keep their version). It never downgrades; a version that fails is
left inactive and reported once as a machine issue.

Without --node this covers every node. --now runs the check immediately and
waits for it to finish.`,
		Args: cobra.NoArgs,
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			var targets []nodeAutoStatus
			if *node != "" {
				targets = []nodeAutoStatus{{NodeID: *node}}
			} else {
				np, err := ops.ListNodes(ctx, false)
				if err != nil {
					return err
				}
				for _, n := range np.Items {
					targets = append(targets, nodeAutoStatus{NodeID: n.NodeID, Name: n.Name})
				}
			}
			params := c.AutoUpdateParams{Tool: *tool}
			action := c.ActionUpdateAutoStatus
			if now {
				action = c.ActionUpdateAuto
			}
			ops.eachAuto(ctx, targets, action, params, a.opID)
			if now {
				deadline := time.Now().Add(autoWaitMax)
				for anyAutoRunning(targets) && time.Now().Before(deadline) {
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-time.After(autoPollEvery):
					}
					var running []nodeAutoStatus
					for _, t := range targets {
						if autoRunning(t) {
							running = append(running, t)
						}
					}
					ops.eachAuto(ctx, running, c.ActionUpdateAutoStatus, params, "")
					for _, r := range running {
						for i := range targets {
							if targets[i].NodeID == r.NodeID {
								targets[i] = r
							}
						}
					}
				}
			}
			return a.emit(targets, func(w io.Writer) { printAutoStatus(w, targets) })
		}),
	}
	cmd.Flags().BoolVar(&now, "now", false, "check (and update) now and wait for the result")
	return cmd
}

// eachAuto runs an auto-update action on every target in parallel, filling
// Tools or Error in place (the slice's entries are replaced).
func (o *Ops) eachAuto(ctx context.Context, targets []nodeAutoStatus, action string, p c.AutoUpdateParams, opID string) {
	var wg sync.WaitGroup
	for i := range targets {
		wg.Add(1)
		go func(t *nodeAutoStatus) {
			defer wg.Done()
			var out []c.AutoUpdateStatus
			op := opID
			if op != "" {
				op += "-" + t.NodeID
			}
			if _, err := o.NodeAction(ctx, t.NodeID, action, p, &out, op); err != nil {
				t.Tools, t.Error = nil, err.Error()
				return
			}
			t.Tools, t.Error = out, ""
		}(&targets[i])
	}
	wg.Wait()
}

func autoRunning(t nodeAutoStatus) bool {
	for _, s := range t.Tools {
		if s.Running {
			return true
		}
	}
	return false
}

func anyAutoRunning(ts []nodeAutoStatus) bool {
	for _, t := range ts {
		if autoRunning(t) {
			return true
		}
	}
	return false
}

func printAutoStatus(w io.Writer, ts []nodeAutoStatus) {
	sort.SliceStable(ts, func(i, j int) bool { return nodeLabel(ts[i]) < nodeLabel(ts[j]) })
	var rows [][]string
	for _, t := range ts {
		if t.Error != "" {
			rows = append(rows, []string{nodeLabel(t), "-", "-", "-", "-", "error", t.Error})
			continue
		}
		for _, s := range t.Tools {
			outcome := orDash(s.Outcome)
			if s.Running {
				outcome = "checking"
			}
			rows = append(rows, []string{nodeLabel(t), s.Tool, orDash(s.Current), orDash(s.Latest), agoPtr(s.CheckedAt), outcome, orDash(s.Detail)})
		}
	}
	table(w, "NODE\tTOOL\tCURRENT\tLATEST\tCHECKED\tOUTCOME\tDETAIL", rows)
}

func nodeLabel(t nodeAutoStatus) string {
	if t.Name != "" {
		return t.Name
	}
	return t.NodeID
}

// printAutoLine is the automatic updater summary under `pyre update ls`.
func printAutoLine(w io.Writer, ss []c.AutoUpdateStatus) {
	for _, s := range ss {
		state := orDash(s.Outcome)
		if s.Running {
			state = "checking now"
		}
		next := ""
		if s.NextCheckAt != nil {
			next = ", next check " + ago(*s.NextCheckAt)
		}
		fmt.Fprintf(w, "auto-update %s: new runs use %s; latest release %s (checked %s%s): %s\n",
			s.Tool, orDash(s.Current), orDash(s.Latest), agoPtr(s.CheckedAt), next, state)
	}
}
