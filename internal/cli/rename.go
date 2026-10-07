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
	Session     string `json:"session,omitempty"`
	Title       string `json:"title"`
	OperationID string `json:"operation_id,omitempty"`
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
	return o.API.UpdateSession(ctx, c.UpdateSessionRequest{Mutation: mut(p.OperationID), SessionID: sv.Session.SessionID, Title: &title})
}

func renameMCPTools() []MCPTool {
	return []MCPTool{
		tool("rename_session", "Rename session",
			"Rename any session (default your own) so the owner can tell the work apart, e.g. your own or sessions you started (\"Fix stripe webhook retries\"). Short, specific, at most ~6 words. A title the owner chose is kept.",
			obj(props{"title": str("the new title"), "session": str("session ID or exact title; \"self\" (default) is your own session"), "operation_id": opIDProp}, "title"),
			hints("Rename session", idempotent, true),
			func(ctx context.Context, o *Ops, p RenameParams) (c.SessionView, error) { return o.Rename(ctx, p) }),
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
