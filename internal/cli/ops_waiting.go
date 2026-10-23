package cli

// Waiting on own job and the project fact sheet (setup review F3/F5/F8).
//
// waiting_on: a session about to end its turn while its own CI run, review
// or watcher is still pending says so; its parent then hears "waiting", not
// "idle", and hears again if nothing resumed it by the expected time
// (contracts/waiting_on.go). With quiet, the parent hears nothing for that
// turn (a lead waiting on its own workers or CI ends its turn instead of
// polling its inbox).
//
// `pyre fact add` (the owner): append a settled project fact to the
// project's fact sheet document (slug "facts"); the hub puts the sheet into
// every session started in the project (contracts/factsheet.go) and lets
// only the owner (and its own approved-proposal path) write it. Sessions
// propose facts with propose_fact, which asks the owner.

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	c "github.com/jhh3/pyre/internal/contracts"
)

func init() { mcpToolProviders = append(mcpToolProviders, waitingFactMCPTools) }

// WaitingOnParams are waiting_on's arguments.
type WaitingOnParams struct {
	What          string `json:"what"`
	ExpectMinutes int    `json:"expect_minutes,omitempty"`
	Clear         bool   `json:"clear,omitempty"`
	Quiet         bool   `json:"quiet,omitempty"`
	OperationID   string `json:"operation_id,omitempty"`
}

// WaitingOn records (or clears) what this session waits on for its turn.
func (o *Ops) WaitingOn(ctx context.Context, p WaitingOnParams) (c.SetWaitingOnResponse, error) {
	api, ok := o.API.(c.WaitingOnAPI)
	if !ok {
		return c.SetWaitingOnResponse{}, c.Errorf(c.ErrUnsupported, "this connection does not offer %s; upgrade pyre-hub", c.OpSessionsWaitingOn)
	}
	if o.Self.SessionID == "" {
		return c.SetWaitingOnResponse{}, usagef("waiting_on is for agent sessions ($PYRE_SESSION_ID is not set)")
	}
	return api.SetWaitingOn(ctx, c.SetWaitingOnRequest{Mutation: mut(p.OperationID), What: strings.TrimSpace(p.What), ExpectMinutes: p.ExpectMinutes, Clear: p.Clear, Quiet: p.Quiet})
}

// FactParams are add_fact's arguments.
type FactParams struct {
	Fact        string `json:"fact"`
	Quote       string `json:"quote"`
	Project     string `json:"project,omitempty"`
	OperationID string `json:"operation_id,omitempty"`
}

// factSheetHead opens a new fact sheet.
const factSheetHead = "# " + c.FactSheetTitle + "\n\nSettled facts for this project, each with the owner's own words. Pyre puts this sheet into every session started in the project. The owner adds facts with `pyre fact add` or approves ones sessions propose (propose_fact).\n\n"

// factLine renders one fact.
func factLine(fact, quote string, now time.Time) string {
	fact = strings.Join(strings.Fields(fact), " ")
	quote = strings.Join(strings.Fields(quote), " ")
	line := "- " + fact + " — added by the owner"
	if quote != "" {
		line += ": \"" + quote + "\""
	}
	return line + " (" + now.In(askZone).Format("Jan 2 2006 3:04 PM MST") + ")\n"
}

// AddFact appends a fact to the project's fact sheet, creating the sheet
// on first use. A concurrent edit is retried on the new head.
func (o *Ops) AddFact(ctx context.Context, p FactParams) (c.DocumentWithRevision, error) {
	if strings.TrimSpace(p.Fact) == "" {
		return c.DocumentWithRevision{}, usagef("a fact is required")
	}
	pid, err := o.factProject(ctx, p.Project)
	if err != nil {
		return c.DocumentWithRevision{}, err
	}
	line := factLine(p.Fact, p.Quote, time.Now())
	m := mut(p.OperationID)
	for attempt := 0; ; attempt++ {
		cur, err := o.API.GetDocument(ctx, c.GetDocumentRequest{ProjectID: pid, Slug: c.FactSheetSlug})
		if c.IsCode(err, c.ErrNotFound) {
			d, err := o.API.CreateDocument(ctx, c.CreateDocumentRequest{Mutation: c.Mutation{OperationID: m.OperationID + ":create"}, ProjectID: &pid,
				Title: c.FactSheetTitle, Slug: c.FactSheetSlug, Content: factSheetHead + line, Summary: "first fact"})
			if c.IsCode(err, c.ErrConflict) && attempt < 3 {
				continue // created concurrently: append to it
			}
			return d, err
		}
		if err != nil {
			return c.DocumentWithRevision{}, err
		}
		content := cur.Revision.Content
		if strings.Contains(content, line) {
			return cur, nil // a retried call
		}
		if content != "" && !strings.HasSuffix(content, "\n") {
			content += "\n"
		}
		d, err := o.API.ProposeRevision(ctx, c.ProposeRevisionRequest{Mutation: c.Mutation{OperationID: fmt.Sprintf("%s:%d", m.OperationID, attempt)},
			DocumentID: cur.Document.DocumentID, BaseRevisionID: cur.Revision.RevisionID, Content: content + line, Summary: "fact: " + clip(p.Fact, 80)})
		if c.IsCode(err, c.ErrConflict) && attempt < 3 {
			continue
		}
		return d, err
	}
}

// factProject is the named project, else the calling session's.
func (o *Ops) factProject(ctx context.Context, ref string) (string, error) {
	if strings.TrimSpace(ref) != "" {
		pr, err := o.ResolveProject(ctx, ref)
		return pr.ProjectID, err
	}
	if o.Self.SessionID != "" {
		s, err := o.API.GetSession(ctx, c.GetRequest{ID: o.Self.SessionID})
		if err != nil {
			return "", err
		}
		if s.Session.ProjectID != nil && *s.Session.ProjectID != "" {
			return *s.Session.ProjectID, nil
		}
	}
	return "", usagef("no project: pass project (this session is not in one)")
}

// ListFacts returns the project's fact sheet.
func (o *Ops) ListFacts(ctx context.Context, p FactParams) (c.DocumentWithRevision, error) {
	pid, err := o.factProject(ctx, p.Project)
	if err != nil {
		return c.DocumentWithRevision{}, err
	}
	return o.API.GetDocument(ctx, c.GetDocumentRequest{ProjectID: pid, Slug: c.FactSheetSlug})
}

func waitingFactMCPTools() []MCPTool {
	return []MCPTool{
		compactly(tool("waiting_on", "Waiting on own job",
			"Before ending your turn while your own CI, review, workers or watcher are pending: your parent hears you wait (not idle), and again if not woken by expect_minutes. quiet: it hears nothing now; end your turn, don't poll inbox.",
			obj(props{"what": str("the job, with its URL, e.g. \"CI on PR #12 https://github.com/o/r/pull/12\""),
				"expect_minutes": num("when it should be done (default 60, max 1440)"), "clear": boo("drop the marker"),
				"quiet": boo("don't report this turn's end to your parent; a Monitor, a child's report or a message wakes you (it still hears if you are not woken by expect_minutes, and when your run exits)"), "operation_id": opIDProp}),
			hints("Waiting on own job", idempotent, true),
			func(ctx context.Context, o *Ops, p WaitingOnParams) (c.SetWaitingOnResponse, error) {
				return o.WaitingOn(ctx, p)
			}),
			func(rc *renderCtx, r c.SetWaitingOnResponse) string { return r.Detail + "\n" }),
	}
}

// addWaitingFactCommands adds `pyre waiting-on` and `pyre fact`.
func (a *App) addWaitingFactCommands(root *cobra.Command) {
	var wp WaitingOnParams
	wait := &cobra.Command{
		Use:   "waiting-on <what...>",
		Short: "Before ending a turn: say this session waits on its own CI/review/watcher (parent hears \"waiting\", not idle)",
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			wp.What = strings.Join(args, " ")
			if wp.What == "" && !wp.Clear {
				return usagef("say what you wait on, e.g. pyre waiting-on \"CI on PR #12 <url>\"")
			}
			ops, err := a.connect()
			if err != nil {
				return err
			}
			r, err := ops.WaitingOn(cmd.Context(), wp)
			if err != nil {
				return err
			}
			return a.emit(r, func(w io.Writer) { fmt.Fprintln(w, r.Detail) })
		}),
	}
	wait.Flags().IntVar(&wp.ExpectMinutes, "expect", 0, "minutes until it should be done (default 60)")
	wait.Flags().BoolVar(&wp.Clear, "clear", false, "drop the marker")
	wait.Flags().BoolVar(&wp.Quiet, "quiet", false, "don't report this turn's end to the parent (overdue and exit still are)")
	root.AddCommand(wait)

	fact := &cobra.Command{
		Use:   "fact [add|ls]",
		Short: "The project fact sheet: settled facts quoting the owner, given to every session started in the project",
		Args:  cobra.NoArgs,
	}
	var fp FactParams
	add := &cobra.Command{
		Use:   "add <fact...> --quote \"<owner's words>\"",
		Short: "Append a fact to the project's fact sheet (owner only; sessions use propose_fact)",
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			fp.Fact = strings.Join(args, " ")
			ops, err := a.connect()
			if err != nil {
				return err
			}
			d, err := ops.AddFact(cmd.Context(), fp)
			if err != nil {
				return err
			}
			return a.emit(d, func(w io.Writer) {
				fmt.Fprintf(w, "fact sheet %s revision %d\n", d.Document.DocumentID, d.Revision.Seq)
			})
		}),
	}
	add.Flags().StringVar(&fp.Quote, "quote", "", "optional: the words to keep with it")
	add.Flags().StringVar(&fp.Project, "project", "", "project ID or name (default: this session's)")
	fact.AddCommand(add)
	var lp FactParams
	ls := &cobra.Command{
		Use:   "ls",
		Short: "Show the project's fact sheet",
		Args:  cobra.NoArgs,
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			d, err := ops.ListFacts(cmd.Context(), lp)
			if err != nil {
				return err
			}
			return a.emit(d, func(w io.Writer) { fmt.Fprint(w, d.Revision.Content) })
		}),
	}
	ls.Flags().StringVar(&lp.Project, "project", "", "project ID or name (default: this session's)")
	fact.AddCommand(ls)
	root.AddCommand(fact)
}
