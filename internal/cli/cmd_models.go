package cli

import (
	"context"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	c "github.com/jhh3/pyre/internal/contracts"
)

// modelDesc is a run's chosen model and effort for session details.
func modelDesc(model, effort string) string {
	switch {
	case model == "":
		return "default model, effort " + effort
	case effort == "":
		return model
	}
	return model + ", effort " + effort
}

// NodeModels is one harness's model choices on one node (pyre models).
type NodeModels struct {
	NodeID   string          `json:"node_id"`
	NodeName string          `json:"node_name"`
	Harness  c.Harness       `json:"harness"`
	Models   []c.ModelChoice `json:"models"`
	Efforts  []string        `json:"efforts"`
}

// ModelsResult is the list_models MCP result.
type ModelsResult struct {
	Harnesses []NodeModels `json:"harnesses"`
}

// Models lists the model choices the nodes advertise per harness.
func (o *Ops) Models(ctx context.Context, node, harness string) ([]NodeModels, error) {
	var nodes []c.Node
	if node != "" {
		n, err := o.ResolveNode(ctx, node)
		if err != nil {
			return nil, err
		}
		nodes = []c.Node{n}
	} else {
		page, err := listAll(ctx, func(ctx context.Context, cur string) (c.Page[c.Node], error) {
			return o.API.ListNodes(ctx, c.ListNodesRequest{PageRequest: c.PageRequest{Cursor: cur}})
		})
		if err != nil {
			return nil, err
		}
		nodes = page.Items
	}
	out := []NodeModels{}
	for _, n := range nodes {
		for _, h := range n.Harnesses {
			if !h.Available || (harness != "" && string(h.Harness) != harness) || h.Harness == c.HarnessShell || h.Harness == c.HarnessMock {
				continue
			}
			out = append(out, NodeModels{NodeID: n.NodeID, NodeName: n.Name, Harness: h.Harness, Models: orEmpty(h.Models), Efforts: orEmptyS(h.Efforts)})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].NodeName != out[j].NodeName {
			return out[i].NodeName < out[j].NodeName
		}
		return out[i].Harness < out[j].Harness
	})
	return out, nil
}

func orEmpty(m []c.ModelChoice) []c.ModelChoice {
	if m == nil {
		return []c.ModelChoice{}
	}
	return m
}

func orEmptyS(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func (a *App) addModelCommands(root *cobra.Command) {
	var node, harness string
	cmd := &cobra.Command{
		Use:   "models [--node N] [--harness claude|codex|agy]",
		Short: "List the models and efforts each machine's harnesses offer (for pyre start --model/--effort)",
		Args:  cobra.NoArgs,
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			list, err := ops.Models(cmd.Context(), node, harness)
			if err != nil {
				return err
			}
			return a.emit(list, func(w io.Writer) {
				if len(list) == 0 {
					fmt.Fprintln(w, "no agent harness reports models (an older pyred lists none; --model still takes any name)")
					return
				}
				for _, nm := range list {
					fmt.Fprintf(w, "%s · %s   efforts: %s\n", orDash(nm.NodeName), nm.Harness, orDash(strings.Join(nm.Efforts, ", ")))
					if len(nm.Models) == 0 {
						fmt.Fprintln(w, "  (no list; --model takes any name the harness accepts)")
					}
					for _, m := range nm.Models {
						line := "  " + m.ID
						if m.Label != "" && m.Label != m.ID {
							line += "  " + m.Label
						}
						if m.Default {
							line += "  (default)"
						}
						if len(m.Efforts) > 0 {
							line += "  [" + strings.Join(m.Efforts, " ") + "]"
						}
						fmt.Fprintln(w, line)
					}
				}
			})
		}),
	}
	cmd.Flags().StringVar(&node, "node", "", "only this node")
	cmd.Flags().StringVar(&harness, "harness", "", "only this harness")
	root.AddCommand(cmd)
}

// modelSupport is what node n offers for harness h: ok when its pyred
// lists model or effort choices there, i.e. it understands --pyre-model /
// --pyre-effort (an older pyred passes them to the harness, which exits on
// the unknown option).
func modelSupport(n c.Node, harness string) (c.HarnessInfo, bool) {
	h, _ := nodeHarness(n, harness)
	return h, len(h.Models) > 0 || len(h.Efforts) > 0
}

// checkNodeModel refuses a model or effort node n cannot apply for harness:
// an older pyred, or an effort the harness does not offer there.
func checkNodeModel(n c.Node, harness, model, effort string) error {
	if model == "" && effort == "" {
		return nil
	}
	h, ok := modelSupport(n, harness)
	name := n.Name
	if name == "" {
		name = n.NodeID
	}
	if !ok {
		return usagef("%s's pyred lists no model choices for %s (an older version): it cannot apply --model/--effort; upgrade it, pick another machine, or start without them", name, harness)
	}
	if effort = strings.ToLower(strings.TrimSpace(effort)); effort != "" && len(h.Efforts) > 0 && !slices.Contains(h.Efforts, effort) {
		return usagef("--effort %s is not offered by %s on %s (choices: %s)", effort, harness, name, strings.Join(h.Efforts, ", "))
	}
	return nil
}

// nodeModelArgs checks the launch's model and effort against the node and
// renders them as start args. When the harness preflight swapped the
// harness (Codex unavailable, Claude review) the model is dropped and the
// effort kept only when the new harness offers it; the prompt says so.
func (o *Ops) nodeModelArgs(ctx context.Context, nodeID, requested string, p *StartParams) ([]string, error) {
	if strings.TrimSpace(p.Model) == "" && strings.TrimSpace(p.Effort) == "" {
		return nil, nil
	}
	n, err := o.ResolveNode(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	if p.Harness != requested {
		var dropped []string
		if p.Model != "" {
			dropped = append(dropped, "model "+p.Model)
			p.Model = ""
		}
		if p.Effort != "" && checkNodeModel(n, p.Harness, "", p.Effort) != nil {
			dropped = append(dropped, "effort "+p.Effort)
			p.Effort = ""
		}
		if len(dropped) > 0 {
			p.Prompt = fmt.Sprintf("[pyre] The %s requested for %s does not apply to %s, so this session uses %s's default.\n\n%s",
				strings.Join(dropped, " and "), requested, p.Harness, p.Harness, p.Prompt)
		}
		if p.Model == "" && p.Effort == "" {
			return nil, nil
		}
	}
	if err := checkNodeModel(n, p.Harness, p.Model, p.Effort); err != nil {
		return nil, err
	}
	return modelArgs(p.Harness, p.Model, p.Effort)
}
