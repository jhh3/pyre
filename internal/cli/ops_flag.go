package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	c "github.com/jhh3/pyre/internal/contracts"
)

func init() { mcpToolProviders = append(mcpToolProviders, flagMCPTools) }

// FlagParams raises or clears a session's own "needs you" flag (v1.3).
type FlagParams struct {
	Session     string `json:"session,omitempty"` // default: the calling session
	Text        string `json:"text,omitempty"`
	Clear       bool   `json:"clear,omitempty"`
	OperationID string `json:"operation_id,omitempty"`
}

// FlagAttention flags (or clears) a session. An agent session flags
// itself; the owner names the session.
func (o *Ops) FlagAttention(ctx context.Context, p FlagParams) (c.AttentionItem, error) {
	sid := ""
	ref := strings.TrimSpace(p.Session)
	switch {
	case ref == "" || ref == "self":
		sid = o.Self.SessionID // empty: the hub uses the calling session
	default:
		sv, err := o.ResolveSession(ctx, ref)
		if err != nil {
			return c.AttentionItem{}, err
		}
		sid = sv.Session.SessionID
	}
	if !p.Clear && strings.TrimSpace(p.Text) == "" {
		return c.AttentionItem{}, usagef("say why you need the owner (text), e.g. \"pick a schema: A or B\"")
	}
	return o.API.FlagAttention(ctx, c.FlagAttentionRequest{Mutation: mut(p.OperationID), SessionID: sid, Text: p.Text, Clear: p.Clear})
}

func flagMCPTools() []MCPTool {
	return []MCPTool{
		tool("flag_attention", "Flag for the owner",
			"Mark this session \"needs you\" with a one-line reason, for what only the owner can decide or provide (not progress). Clears when the owner engages, you call clear_attention, or the run ends.",
			obj(props{"text": str("one-line reason the owner sees, e.g. \"pick a schema: A or B\""), "operation_id": opIDProp}, "text"),
			hints("Flag for the owner", idempotent, true),
			func(ctx context.Context, o *Ops, p FlagParams) (c.AttentionItem, error) {
				p.Session, p.Clear = "", false
				return o.FlagAttention(ctx, p)
			}),
		tool("clear_attention", "Clear flag",
			"Clear this session's own \"needs you\" flag (for example, you resolved it yourself).",
			obj(props{"operation_id": opIDProp}),
			hints("Clear flag", idempotent, true),
			func(ctx context.Context, o *Ops, p FlagParams) (c.AttentionItem, error) {
				p.Session, p.Text, p.Clear = "", "", true
				return o.FlagAttention(ctx, p)
			}),
	}
}

// flagCommand is `pyre attention flag [<text>...] [--clear] [--session s]`.
func (a *App) flagCommand() *cobra.Command {
	var clear bool
	cmd := &cobra.Command{
		Use:   "flag [<text>...]",
		Short: "Flag a session \"needs you\" with a reason (an agent flags itself), or --clear it",
		Long: `Flag a session as needing the owner. Run inside an agent session it flags that
session; the owner names one with --session. The flag clears when the owner
engages with the session (types into it, messages it), with --clear, or when
the run ends.`,
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			session, _ := cmd.Flags().GetString("session") // inherited from attention
			it, err := ops.FlagAttention(cmd.Context(), FlagParams{Session: session, Text: strings.Join(args, " "), Clear: clear, OperationID: a.opID})
			if err != nil {
				return err
			}
			return a.emit(it, func(w io.Writer) {
				switch {
				case it.AttentionID == "":
					fmt.Fprintln(w, "nothing to clear")
				case clear:
					fmt.Fprintf(w, "cleared %s\n", it.AttentionID)
				default:
					fmt.Fprintf(w, "flagged %s: %s\n", it.SessionID, it.Title)
				}
			})
		}),
	}
	cmd.Flags().BoolVar(&clear, "clear", false, "clear the flag instead")
	return cmd
}
