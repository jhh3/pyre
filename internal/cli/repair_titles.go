package cli

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/jhh3/pyre/internal/autotitle"
	c "github.com/jhh3/pyre/internal/contracts"
)

// One-off repair (2026-11-06): nodes before the envelope fix named
// automatically titled sessions after Claude Code's header for a
// plugin-submitted prompt ("The pyre-session plugin sent a message").
// `pyre sessions repair-titles` finds sessions still carrying title:auto
// whose title is such an envelope and renames each one from its first real
// prompt (read from its transcript and named with the fixed
// autotitle.FromPrompt), else back to the title the audit log says it had
// before. It keeps title:auto so Claude's own title can still improve the
// name. Only envelope titles are touched, so running it again changes
// nothing.

// RepairTitleChange is one session the repair renamed (or would rename).
type RepairTitleChange struct {
	SessionID string `json:"session_id"`
	From      string `json:"from"`
	To        string `json:"to"`
	How       string `json:"how"` // re-derived | restored
}

// RepairTitlesResult is `pyre sessions repair-titles --json`.
type RepairTitlesResult struct {
	Applied    bool                `json:"applied"`
	Changes    []RepairTitleChange `json:"changes"`
	Restored   int                 `json:"restored"`
	ReDerived  int                 `json:"re_derived"`
	Skipped    []string            `json:"skipped"` // session IDs left as they are (nothing to name them from)
	Failed     map[string]string   `json:"failed,omitempty"`
	Considered int                 `json:"considered"`
}

func (a *App) repairTitlesCommand() *cobra.Command {
	var apply bool
	cmd := &cobra.Command{
		Use:    "repair-titles",
		Short:  "Rename sessions automatically titled after a Pyre/plugin envelope (one-off repair)",
		Hidden: true,
		Long: `Find automatically titled sessions whose title is a delivery envelope
("The pyre-session plugin sent a message", "[pyre-msg:…]", …) and rename each
from its first real prompt, else to the title it had before (audit log).
Without --apply it only prints what it would do. Safe to run again.`,
		Args: cobra.NoArgs,
		RunE: a.run(func(cmd *cobra.Command, _ []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			res, err := ops.RepairTitles(cmd.Context(), apply, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			return a.emit(res, func(w io.Writer) {
				verb := "would rename"
				if apply {
					verb = "renamed"
				}
				for _, ch := range res.Changes {
					fmt.Fprintf(w, "%s %s %q -> %q (%s)\n", verb, ch.SessionID, ch.From, ch.To, ch.How)
				}
				for _, id := range res.Skipped {
					fmt.Fprintf(w, "skipped %s (no prompt or earlier title found)\n", id)
				}
				for id, e := range res.Failed {
					fmt.Fprintf(w, "failed %s: %s\n", id, e)
				}
				fmt.Fprintf(w, "%d envelope titles: %d re-derived, %d restored, %d skipped, %d failed\n",
					res.Considered, res.ReDerived, res.Restored, len(res.Skipped), len(res.Failed))
				if !apply && len(res.Changes) > 0 {
					fmt.Fprintln(w, "dry run: pass --apply to rename")
				}
			})
		}),
	}
	cmd.Flags().BoolVar(&apply, "apply", false, "rename the sessions (default: only print what would change)")
	return cmd
}

// RepairTitles renames sessions whose automatic title is an envelope (see
// above); progress lines go to log.
func (o *Ops) RepairTitles(ctx context.Context, apply bool, log io.Writer) (RepairTitlesResult, error) {
	res := RepairTitlesResult{Applied: apply, Changes: []RepairTitleChange{}, Skipped: []string{}, Failed: map[string]string{}}
	page, err := o.ListSessions(ctx, SessionFilter{IncludeArchived: true})
	if err != nil {
		return res, err
	}
	var bad []c.Session
	for _, v := range page.Items {
		if autotitle.IsAuto(v.Session.Tags) && autotitle.IsEnvelope(v.Session.Title) {
			bad = append(bad, v.Session)
		}
	}
	res.Considered = len(bad)
	if len(bad) == 0 {
		return res, nil
	}
	prev := o.previousAutoTitles(ctx)
	for i, s := range bad {
		fmt.Fprintf(log, "[%d/%d] %s\n", i+1, len(bad), s.SessionID)
		ch := RepairTitleChange{SessionID: s.SessionID, From: s.Title}
		if t := o.firstPromptTitle(ctx, s.SessionID); t != "" {
			ch.To, ch.How = t, "re-derived"
		} else if t := prev[s.SessionID]; t != "" {
			ch.To, ch.How = t, "restored"
		} else {
			res.Skipped = append(res.Skipped, s.SessionID)
			continue
		}
		if apply {
			if err := o.retitleKeepingAuto(ctx, s, ch.To); err != nil {
				res.Failed[s.SessionID] = err.Error()
				continue
			}
		}
		res.Changes = append(res.Changes, ch)
		if ch.How == "restored" {
			res.Restored++
		} else {
			res.ReDerived++
		}
	}
	return res, nil
}

// previousAutoTitles maps a session to the title it had before automatic
// naming gave it an envelope (the newest such audit entry wins).
func (o *Ops) previousAutoTitles(ctx context.Context) map[string]string {
	out := map[string]string{}
	page, err := o.Audit(ctx, AuditParams{Since: "all", Kind: "session", Limit: 2000}, time.Now())
	if err != nil {
		return out
	}
	for _, e := range page.Items { // newest first
		if e.Action != c.AuditSessionAutoTitled || !autotitle.IsEnvelope(e.To) || e.From == "" || autotitle.IsEnvelope(e.From) {
			continue
		}
		if _, ok := out[e.SessionID]; !ok {
			out[e.SessionID] = e.From
		}
	}
	return out
}

// firstPromptTitle names a session from the first prompt in its transcript
// that is not Pyre's own text ("" when the node cannot be read or none is).
func (o *Ops) firstPromptTitle(ctx context.Context, sessionID string) string {
	cursor := "0"
	for range 4 {
		resp, err := o.ReadSession(ctx, ReadParams{Session: sessionID, Cursor: cursor, Limit: 100, Full: true})
		if err != nil {
			return ""
		}
		for _, e := range resp.Entries {
			if e.Kind != "user" && e.Kind != "system" && e.Kind != "pyre_message" {
				continue
			}
			if e.Kind == "system" && !strings.Contains(strings.SplitN(e.Text, "\n", 2)[0], "plugin sent a message") {
				continue // hook output and the like, not a prompt
			}
			if t := autotitle.FromPrompt(e.Text); t != "" && !autotitle.IsEnvelope(t) {
				return t
			}
		}
		if resp.NextCursor == "" || resp.NextCursor == cursor {
			return ""
		}
		cursor = resp.NextCursor
	}
	return ""
}

// retitleKeepingAuto renames s and puts title:auto back (a rename drops it),
// so a later native title can still improve the name.
func (o *Ops) retitleKeepingAuto(ctx context.Context, s c.Session, title string) error {
	if _, err := o.API.UpdateSession(ctx, c.UpdateSessionRequest{Mutation: mut(""), SessionID: s.SessionID, Title: &title}); err != nil {
		return err
	}
	tags := autotitle.WithoutReserved(s.Tags)
	if !slices.Contains(tags, autotitle.TagAuto) {
		tags = append(tags, autotitle.TagAuto)
	}
	_, err := o.API.UpdateSession(ctx, c.UpdateSessionRequest{Mutation: mut(""), SessionID: s.SessionID, Tags: &tags})
	return err
}
