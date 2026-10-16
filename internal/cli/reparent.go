package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	c "github.com/jhh3/pyre/internal/contracts"
)

// Reparenting (W-reparent): move sessions under another session (or to the
// top level). Their automatic reports go to the new parent from then on;
// the hub checks permissions and tells both sides with a Pyre message.

func init() { mcpToolProviders = append(mcpToolProviders, reparentMCPTools) }

// ReparentParams moves one or more sessions (MCP reparent_session input).
// Parent "" (or "none") moves them to the top level.
type ReparentParams struct {
	Session     string   `json:"session,omitempty"`
	Sessions    []string `json:"sessions,omitempty"`
	Parent      string   `json:"parent"`
	OperationID string   `json:"operation_id,omitempty"`
}

// ReparentItem is one session's outcome.
type ReparentItem struct {
	SessionID   string `json:"session_id"`
	Title       string `json:"title,omitempty"`
	ParentID    string `json:"parent_id,omitempty"` // "" = top level
	ParentTitle string `json:"parent_title,omitempty"`
	Error       string `json:"error,omitempty"`
}

// ReparentResult is reparent_session's answer.
type ReparentResult struct {
	Sessions []ReparentItem `json:"sessions"`
	Note     string         `json:"note"`
}

// topLevelParent reports whether ref names no parent.
func topLevelParent(ref string) bool {
	switch strings.ToLower(strings.TrimSpace(ref)) {
	case "", "none", "top", "-":
		return true
	}
	return false
}

// Reparent moves each session under p.Parent. Every session is attempted;
// the error reports the ones that failed.
func (o *Ops) Reparent(ctx context.Context, p ReparentParams) (ReparentResult, error) {
	refs := append([]string(nil), p.Sessions...)
	if p.Session != "" || len(refs) == 0 {
		refs = append([]string{p.Session}, refs...)
	}
	parentID, parentTitle := "", ""
	if !topLevelParent(p.Parent) {
		pv, err := o.ResolveSession(ctx, p.Parent)
		if err != nil {
			return ReparentResult{}, err
		}
		parentID, parentTitle = pv.Session.SessionID, pv.Session.Title
	}
	var res ReparentResult
	var failed []string
	seen := map[string]bool{}
	for i, ref := range refs {
		item := ReparentItem{SessionID: ref, ParentID: parentID, ParentTitle: parentTitle}
		sv, err := o.ResolveSession(ctx, ref)
		if err == nil && !seen[sv.Session.SessionID] {
			seen[sv.Session.SessionID] = true
			opID := p.OperationID
			if opID != "" && len(refs) > 1 {
				opID = fmt.Sprintf("%s.%d", opID, i)
			}
			to := parentID
			sv, err = o.API.UpdateSession(ctx, c.UpdateSessionRequest{Mutation: mut(opID), SessionID: sv.Session.SessionID, Parent: &to})
		} else if err == nil {
			continue
		}
		if err != nil {
			item.Error = err.Error()
			failed = append(failed, ref)
		} else {
			item.SessionID, item.Title = sv.Session.SessionID, sv.Session.Title
		}
		res.Sessions = append(res.Sessions, item)
	}
	moved := len(res.Sessions) - len(failed)
	target := "the top level"
	if parentID != "" {
		target = fmt.Sprintf("%q (%s)", parentTitle, parentID)
	}
	res.Note = fmt.Sprintf("%d session(s) now report to %s", moved, target)
	if parentID == "" {
		res.Note = fmt.Sprintf("%d session(s) moved to the top level", moved)
	}
	if moved > 0 {
		res.Note += "; their automatic reports go there from now on and each was told by a Pyre message"
	}
	if len(failed) > 0 {
		return res, fmt.Errorf("could not reparent %s: %s", strings.Join(failed, ", "), firstError(res.Sessions))
	}
	return res, nil
}

func firstError(items []ReparentItem) string {
	for _, it := range items {
		if it.Error != "" {
			return it.Error
		}
	}
	return ""
}

func reparentMCPTools() []MCPTool {
	return []MCPTool{
		tool("reparent_session", "Reparent session",
			"Move sessions under another parent: their end-of-turn reports go there from now on; both sides get a Pyre message. "+
				"A coordinator can hand its agents to leads; a lead can adopt agents in its project.",
			obj(props{"parent": str("new parent: session ID, exact title, or \"self\"; \"\" or \"none\" = top level"),
				"session":  str("session to move: ID, exact title, or \"self\" (default: your own session)"),
				"sessions": strs("several sessions to move to the same parent (IDs or exact titles)"), "operation_id": opIDProp}, "parent"),
			hints("Reparent session", idempotent, true),
			func(ctx context.Context, o *Ops, p ReparentParams) (ReparentResult, error) { return o.Reparent(ctx, p) }),
	}
}

// reparentCommand is `pyre sessions reparent <session...> --to <parent|none>`.
func (a *App) reparentCommand() *cobra.Command {
	var to string
	cmd := &cobra.Command{
		Use:   "reparent <session>... --to <parent|none>",
		Short: "Move sessions under another parent session (their reports go there), or to the top level",
		Long: `Move one or more sessions under another session. The new parent receives
their automatic end-of-turn reports from now on (pending ones too); both
sides are told with a Pyre message. --to none moves them to the top level.

  pyre sessions reparent ses_a ses_b --to ses_lead
  pyre sessions reparent self --to none

The owner may reparent any session; an agent only itself, the sessions it
currently parents, and sessions in its own project.`,
		Args: cobra.MinimumNArgs(1),
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			if !cmd.Flags().Changed("to") {
				return usagef("--to <parent|none> is required")
			}
			ops, err := a.connect()
			if err != nil {
				return err
			}
			res, err := ops.Reparent(cmd.Context(), ReparentParams{Sessions: args, Parent: to, OperationID: a.opID})
			if err != nil && len(res.Sessions) == 0 {
				return err
			}
			if eerr := a.emit(res, func(w io.Writer) {
				for _, it := range res.Sessions {
					switch {
					case it.Error != "":
						fmt.Fprintf(w, "%s: %s\n", it.SessionID, it.Error)
					case it.ParentID == "":
						fmt.Fprintf(w, "%s %q now at the top level\n", it.SessionID, it.Title)
					default:
						fmt.Fprintf(w, "%s %q now reports to %q (%s)\n", it.SessionID, it.Title, it.ParentTitle, it.ParentID)
					}
				}
			}); eerr != nil {
				return eerr
			}
			return err
		}),
	}
	cmd.Flags().StringVar(&to, "to", "", "new parent session (ID, exact title or self), or none for the top level")
	return cmd
}
