package cli

// Project pages (W-web-inbox): each project has one owner-facing markdown
// page (document slug "page") that its lead keeps current; the web app
// shows it as the project's main view. update_project_page writes a new
// head revision directly (no proposal step); the hub allows the owner and
// top-level sessions in the project. The ops are optional
// (contracts.ProjectPageAPI).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	c "github.com/jhh3/pyre/internal/contracts"
)

func init() { mcpToolProviders = append(mcpToolProviders, projectPageMCPTools) }

func (o *Ops) projectPages() (c.ProjectPageAPI, error) {
	if a, ok := o.API.(c.ProjectPageAPI); ok {
		return a, nil
	}
	return nil, c.Errorf(c.ErrUnsupported, "this connection does not offer project pages (%s); upgrade pyre-hub", c.OpProjectsSetPage)
}

// ProjectPageParams are update_project_page's and get_project_page's
// arguments (and `pyre project page`'s flags).
type ProjectPageParams struct {
	Content        string `json:"content,omitempty"`
	Summary        string `json:"summary,omitempty"`
	Project        string `json:"project,omitempty"`
	BaseRevisionID string `json:"base_revision_id,omitempty"`
	OperationID    string `json:"operation_id,omitempty"`
	// Status replaces the page's status card (with or without Content).
	Status *c.ProjectStatus `json:"status,omitempty"`
}

func (o *Ops) pageProjectID(ctx context.Context, ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", nil // the hub uses the calling session's project
	}
	pid, err := o.projectPtr(ctx, ref)
	if err != nil || pid == nil {
		return "", err
	}
	return *pid, nil
}

// SetProjectPage writes the project page (a new head revision).
func (o *Ops) SetProjectPage(ctx context.Context, p ProjectPageParams) (c.ProjectPage, error) {
	api, err := o.projectPages()
	if err != nil {
		return c.ProjectPage{}, err
	}
	if strings.TrimSpace(p.Content) == "" && p.Status == nil {
		return c.ProjectPage{}, usagef("content or status is required: the whole page in markdown (goal, current status, results with links, open decisions, what's next) and/or the status card")
	}
	pid, err := o.pageProjectID(ctx, p.Project)
	if err != nil {
		return c.ProjectPage{}, err
	}
	return api.SetProjectPage(ctx, c.SetProjectPageRequest{Mutation: mut(p.OperationID), ProjectID: pid, Content: p.Content,
		Summary: strings.TrimSpace(p.Summary), BaseRevisionID: strings.TrimSpace(p.BaseRevisionID), Status: p.Status})
}

// GetProjectPage reads the project page (empty when none was written).
func (o *Ops) GetProjectPage(ctx context.Context, p ProjectPageParams) (c.ProjectPage, error) {
	api, err := o.projectPages()
	if err != nil {
		return c.ProjectPage{}, err
	}
	pid, err := o.pageProjectID(ctx, p.Project)
	if err != nil {
		return c.ProjectPage{}, err
	}
	return api.GetProjectPage(ctx, c.GetProjectPageRequest{ProjectID: pid})
}

func projectPageMCPTools() []MCPTool {
	return []MCPTool{
		compactly(tool("update_project_page", "Update project page",
			"Update your project's page (leads only): the markdown page and/or its status card (progress, phase, in flight, ready for the owner, shipped). "+
				"Create it early; update the card at each milestone. Stale after 30 idle min while work runs.",
			obj(props{"content": str("the whole page in markdown (replaces the previous version); omit to update only the status"),
				"summary":          str("one line: what changed"),
				"project":          str("project ID or name (default: this session's project)"),
				"base_revision_id": str("revision you edited (get_project_page); a newer page then fails instead of being replaced"),
				"status":           statusProp,
				"operation_id":     opIDProp}),
			hints("Update project page", additive, false),
			func(ctx context.Context, o *Ops, p ProjectPageParams) (c.ProjectPage, error) {
				return o.SetProjectPage(ctx, p)
			}),
			func(rc *renderCtx, pg c.ProjectPage) string { return projectPageWritten(pg, rc.now) }),
		compactly(tool("get_project_page", "Show project page",
			"Read your project's page (the owner's view of the project in the web app), with its revision ID for update_project_page.",
			obj(props{"project": str("project ID or name (default: this session's project)")}),
			hints("Show project page", readOnly, true),
			func(ctx context.Context, o *Ops, p ProjectPageParams) (c.ProjectPage, error) {
				return o.GetProjectPage(ctx, p)
			}),
			func(rc *renderCtx, pg c.ProjectPage) string { return projectPageText(pg, rc.now) }),
	}
}

var statusItemsProp = func(desc string) map[string]any {
	return map[string]any{"type": "array", "description": desc, "items": obj(props{
		"title":   str("what it is (\"PR #12 status card\")"),
		"url":     str("link (PR, deploy, release, artifact)"),
		"state":   str("e.g. review, ci failing, merged, deployed, released"),
		"note":    str("one line of detail"),
		"session": str("session ID doing or owning it"),
	}, "title")}
}

// statusProp is the status card's schema (contracts.ProjectStatus).
var statusProp = map[string]any{"type": "object", "additionalProperties": false,
	"description": "the status card; replaces the previous one (send every field you want kept; get_project_page shows the current card)",
	"properties": map[string]any{
		"progress":        map[string]any{"type": "integer", "minimum": 0, "maximum": 100, "description": "percent toward the goal"},
		"phase":           str("one line: where things stand now"),
		"eta":             str("when you expect to finish (free text)"),
		"in_flight":       statusItemsProp("work under way that live sessions do not show by themselves"),
		"ready_for_owner": statusItemsProp("waiting on the owner but not an ask: PRs to review or merge, a build to try"),
		"shipped":         statusItemsProp("merged, deployed or released items (state says which)"),
		"blocked":         statusItemsProp("blocked items and on what"),
	}}

func projectPageWritten(pg c.ProjectPage, now time.Time) string {
	if pg.Revision == nil {
		if pg.Status != nil {
			return fmt.Sprintf("project %s status card updated (the page itself is not written yet; create it with update_project_page content)\n", pg.ProjectID)
		}
		return fmt.Sprintf("project %s: page not written\n", pg.ProjectID)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "project %s page updated: revision %d (%s, %d bytes)", pg.ProjectID, pg.Revision.Seq, pg.Revision.RevisionID, pg.Revision.Size)
	if pg.Status != nil {
		b.WriteString("; status card set")
	}
	b.WriteString(". The owner sees it in the web app; keep the status current at milestones.\n")
	return b.String()
}

// statusText is the status card as text lines (empty when none).
func statusText(pg c.ProjectPage, now time.Time) string {
	st := pg.Status
	if st == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("status card")
	if pg.StatusUpdatedAt != nil {
		fmt.Fprintf(&b, " (updated %s)", agoFrom(now, *pg.StatusUpdatedAt))
	}
	b.WriteString(":\n")
	if st.Progress != nil {
		fmt.Fprintf(&b, "  progress: %d%%\n", *st.Progress)
	}
	if st.Phase != "" {
		fmt.Fprintf(&b, "  phase: %s\n", st.Phase)
	}
	if st.ETA != "" {
		fmt.Fprintf(&b, "  eta: %s\n", st.ETA)
	}
	for _, sec := range []struct {
		name  string
		items []c.StatusItem
	}{{"in flight", st.InFlight}, {"ready for owner", st.ReadyForOwner}, {"shipped", st.Shipped}, {"blocked", st.Blocked}} {
		if len(sec.items) == 0 {
			continue
		}
		fmt.Fprintf(&b, "  %s:\n", sec.name)
		for _, it := range sec.items {
			b.WriteString("    - " + it.Title)
			for _, x := range []string{it.State, it.URL, it.Note, it.Session} {
				if x != "" {
					b.WriteString(" · " + x)
				}
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}

func projectPageText(pg c.ProjectPage, now time.Time) string {
	if pg.Revision == nil {
		return fmt.Sprintf("project %s has no page yet; create it with update_project_page (goal, status, results with links, open decisions, next)\n", pg.ProjectID) + statusText(pg, now)
	}
	r := pg.Revision
	var b strings.Builder
	fmt.Fprintf(&b, "project %s page · revision %d (%s) · by %s %s", pg.ProjectID, r.Seq, r.RevisionID, principalText(r.Author), agoFrom(now, r.CreatedAt))
	if r.Summary != "" {
		fmt.Fprintf(&b, " · %s", r.Summary)
	}
	b.WriteString("\n\n")
	b.WriteString(strings.TrimRight(pg.Content, "\n"))
	b.WriteString("\n")
	if st := statusText(pg, now); st != "" {
		b.WriteString("\n" + st)
	}
	return b.String()
}

// projectPageCommand is `pyre project page [show|set]`.
func (a *App) projectPageCommand() *cobra.Command {
	var project string
	show := func(cmd *cobra.Command, args []string) error {
		ops, err := a.connect()
		if err != nil {
			return err
		}
		pg, err := ops.GetProjectPage(cmd.Context(), ProjectPageParams{Project: project})
		if err != nil {
			return err
		}
		return a.emit(pg, func(w io.Writer) { fmt.Fprint(w, projectPageText(pg, time.Now())) })
	}
	page := &cobra.Command{
		Use:   "page [show|set --file F|-|status ...]",
		Short: "The project's page: the owner's view of the project in the web app, kept current by its lead",
		Long: `Each project has one markdown page (document slug "page") that the project's
lead keeps current: goal, current status, results with links, open decisions,
what's next. Above it sits a status card (pyre project page status): progress,
phase, in-flight work, items ready for the owner, shipped items, blockers.
The owner and top-level sessions in the project write both; every write is a
new revision. --project defaults to this session's project.`,
		Args: cobra.NoArgs,
		RunE: a.run(show),
	}
	page.PersistentFlags().StringVar(&project, "project", "", "project ID or name (default: this session's)")
	page.AddCommand(&cobra.Command{Use: "show", Short: "Print the page", Args: cobra.NoArgs, RunE: a.run(show)})

	var file, summary, base string
	set := &cobra.Command{
		Use:   "set --file F|- [--summary TEXT] [--base REV]",
		Short: "Replace the page with a file's contents (- reads stdin)",
		Args:  cobra.MaximumNArgs(1),
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				if file != "" {
					return usagef("give the file once: --file F or a positional F")
				}
				file = args[0]
			}
			if file == "" {
				return usagef("--file F (or - for stdin) is required")
			}
			var content string
			if file == "-" {
				b, err := io.ReadAll(io.LimitReader(a.Stdin, c.ProjectPageMaxSize+1))
				if err != nil {
					return err
				}
				content = string(b)
			} else {
				b, err := os.ReadFile(file)
				if err != nil {
					return c.Errorf(c.ErrInvalid, "%v", err)
				}
				content = string(b)
			}
			ops, err := a.connect()
			if err != nil {
				return err
			}
			pg, err := ops.SetProjectPage(cmd.Context(), ProjectPageParams{Content: content, Summary: summary, Project: project, BaseRevisionID: base, OperationID: a.opID})
			if err != nil {
				return err
			}
			return a.emit(pg, func(w io.Writer) { fmt.Fprint(w, projectPageWritten(pg, time.Now())) })
		}),
	}
	set.Flags().StringVar(&file, "file", "", "markdown file with the whole page (- reads stdin)")
	set.Flags().StringVar(&summary, "summary", "", "one line: what changed")
	set.Flags().StringVar(&base, "base", "", "revision you edited; fails if the page changed since")
	page.AddCommand(set)

	var sfile, phase, eta, ssummary string
	var progress int
	status := &cobra.Command{
		Use:   "status [--file F|-] [--progress N] [--phase TEXT] [--eta TEXT]",
		Short: "Set the page's status card: --file replaces it; flags alone change those fields of the current card",
		Long: `The status card sits at the top of the project page: progress toward the
goal, phase, in-flight work, items ready for the owner (PRs to review),
shipped items and blockers. Pyre adds live sessions and open asks itself.
--file takes the JSON of update_project_page's status argument, e.g.
{"progress":40,"phase":"reviewing","ready_for_owner":[{"title":"PR #12","url":"https://..."}]}.`,
		Args: cobra.NoArgs,
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			st := &c.ProjectStatus{}
			if sfile == "" {
				// Flags only: change those fields of the current card.
				cur, err := ops.GetProjectPage(cmd.Context(), ProjectPageParams{Project: project})
				if err != nil {
					return err
				}
				if cur.Status != nil {
					st = cur.Status
				}
			} else {
				var b []byte
				var err error
				if sfile == "-" {
					b, err = io.ReadAll(io.LimitReader(a.Stdin, c.ProjectStatusMaxSize+1))
				} else {
					b, err = os.ReadFile(sfile)
				}
				if err != nil {
					return c.Errorf(c.ErrInvalid, "%v", err)
				}
				dec := json.NewDecoder(bytes.NewReader(b))
				dec.DisallowUnknownFields()
				if err := dec.Decode(st); err != nil {
					return usagef("status file: %v", err)
				}
			}
			if cmd.Flags().Changed("progress") {
				st.Progress = &progress
			}
			if cmd.Flags().Changed("phase") {
				st.Phase = phase
			}
			if cmd.Flags().Changed("eta") {
				st.ETA = eta
			}
			pg, err := ops.SetProjectPage(cmd.Context(), ProjectPageParams{Status: st, Summary: ssummary, Project: project, OperationID: a.opID})
			if err != nil {
				return err
			}
			return a.emit(pg, func(w io.Writer) { fmt.Fprint(w, projectPageWritten(pg, time.Now())) })
		}),
	}
	status.Flags().StringVar(&sfile, "file", "", "JSON with the status fields (- reads stdin)")
	status.Flags().IntVar(&progress, "progress", 0, "percent toward the goal (0-100)")
	status.Flags().StringVar(&phase, "phase", "", "one line: where things stand")
	status.Flags().StringVar(&eta, "eta", "", "when you expect to finish")
	status.Flags().StringVar(&ssummary, "summary", "", "one line: what changed")
	page.AddCommand(status)
	return page
}
