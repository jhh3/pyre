package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	c "github.com/jhh3/pyre/internal/contracts"
)

func init() { mcpToolProviders = append(mcpToolProviders, renameMCPTools) }

// RenameParams renames one session (MCP rename_session input). A person's
// rename always wins and stops automatic naming; an agent may rename any
// session, but not over a title the owner chose (the hub enforces it).
type RenameParams struct {
	Session     string  `json:"session,omitempty"`
	Title       string  `json:"title"`
	Icon        *string `json:"icon,omitempty"` // W-archive2: also set the emoji ("" clears)
	OperationID string  `json:"operation_id,omitempty"`
}

// Rename sets a session's title.
func (o *Ops) Rename(ctx context.Context, p RenameParams) (c.SessionView, error) {
	title := strings.Join(strings.Fields(p.Title), " ")
	if title == "" {
		return c.SessionView{}, usagef("a title is required")
	}
	if n := len([]rune(title)); n > 120 {
		return c.SessionView{}, usagef("title is %d characters; keep it under 120", n)
	}
	sv, err := o.ResolveSession(ctx, p.Session)
	if err != nil {
		return c.SessionView{}, err
	}
	return o.API.UpdateSession(ctx, c.UpdateSessionRequest{Mutation: mut(p.OperationID), SessionID: sv.Session.SessionID, Title: &title, Icon: p.Icon})
}

// SetIconParams sets one session's emoji (MCP set_session_icon input).
type SetIconParams struct {
	Session     string `json:"session,omitempty"`
	Icon        string `json:"icon"`
	OperationID string `json:"operation_id,omitempty"`
}

// SetIcon sets (or with "" clears) a session's emoji. Any session may set
// any session's icon, like a rename; automatic naming never changes it.
func (o *Ops) SetIcon(ctx context.Context, p SetIconParams) (c.SessionView, error) {
	icon := strings.TrimSpace(p.Icon)
	sv, err := o.ResolveSession(ctx, p.Session)
	if err != nil {
		return c.SessionView{}, err
	}
	return o.API.UpdateSession(ctx, c.UpdateSessionRequest{Mutation: mut(p.OperationID), SessionID: sv.Session.SessionID, Icon: &icon})
}

func renameMCPTools() []MCPTool {
	return []MCPTool{
		tool("rename_session", "Rename session",
			"Rename any session (default your own) so the owner can tell the work apart, e.g. your own or sessions you started (\"Fix stripe webhook retries\"). Short, specific, at most ~6 words. A title the owner chose is kept.",
			obj(props{"title": str("the new title"), "session": str("session ID or exact title; \"self\" (default) is your own session"),
				"icon": str("optional: also set the session's emoji (one emoji; \"\" clears)"), "operation_id": opIDProp}, "title"),
			hints("Rename session", idempotent, true),
			func(ctx context.Context, o *Ops, p RenameParams) (c.SessionView, error) { return o.Rename(ctx, p) }),
		tool("set_session_icon", "Set session icon",
			"Give a session (default your own) an emoji shown before its title that reflects its work, e.g. 🐛 for a bug fix, 🚀 for a release. One emoji; \"\" clears it.",
			obj(props{"icon": str("one emoji, or \"\" to clear"), "session": str("session ID or exact title; \"self\" (default) is your own session"), "operation_id": opIDProp}, "icon"),
			hints("Set session icon", idempotent, true),
			func(ctx context.Context, o *Ops, p SetIconParams) (c.SessionView, error) { return o.SetIcon(ctx, p) }),
	}
}

// renameCommand is `pyre sessions rename <session> <title…>`.
func (a *App) renameCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "rename <session> <title>...",
		Short: "Rename a session (ID, exact title, or self); stops automatic naming",
		Long: `Rename a session. The rest of the arguments form the title, so quoting is
optional: pyre sessions rename ses_01… fix stripe webhook retries

Sessions started without a title are named automatically (Claude's own
conversation title, Codex's thread name, else the first prompt). A rename
always wins and stops automatic naming for that session.`,
		Args: cobra.MinimumNArgs(2),
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			sv, err := ops.Rename(cmd.Context(), RenameParams{Session: args[0], Title: strings.Join(args[1:], " "), OperationID: a.opID})
			if err != nil {
				return err
			}
			return a.emit(sv, func(w io.Writer) { fmt.Fprintf(w, "renamed %s %q\n", sv.Session.SessionID, sv.Session.Title) })
		}),
	}
}

// iconCommand is `pyre sessions icon <session> [emoji]` (W-archive2).
func (a *App) iconCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "icon <session> [emoji]",
		Short: "Set a session's emoji, shown before its title (no emoji clears it)",
		Long: `Set a session's icon: one emoji shown before its title in the TUI and the
apps. pyre sessions icon self 🚀 sets your own; pyre sessions icon <s> clears
it. Automatic naming never changes the icon.`,
		Args: cobra.RangeArgs(1, 2),
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			icon := ""
			if len(args) == 2 {
				icon = args[1]
			}
			sv, err := ops.SetIcon(cmd.Context(), SetIconParams{Session: args[0], Icon: icon, OperationID: a.opID})
			if err != nil {
				return err
			}
			return a.emit(sv, func(w io.Writer) {
				if sv.Session.Icon == "" {
					fmt.Fprintf(w, "cleared the icon of %s %q\n", sv.Session.SessionID, sv.Session.Title)
					return
				}
				fmt.Fprintf(w, "%s %s %q\n", sv.Session.Icon, sv.Session.SessionID, sv.Session.Title)
			})
		}),
	}
}
