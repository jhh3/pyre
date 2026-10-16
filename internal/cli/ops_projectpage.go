package cli

// Project pages (W-web-inbox): each project has one owner-facing markdown
// page (document slug "page") that its lead keeps current; the web app
// shows it as the project's main view. update_project_page writes a new
// head revision directly (no proposal step); the hub allows the owner and
// top-level sessions in the project. The ops are optional
// (contracts.ProjectPageAPI).

import (
	"context"
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
	if strings.TrimSpace(p.Content) == "" {
		return c.ProjectPage{}, usagef("content is required: the whole page in markdown (goal, current status, results with links, open decisions, what's next)")
	}
	pid, err := o.pageProjectID(ctx, p.Project)
	if err != nil {
		return c.ProjectPage{}, err
	}
	return api.SetProjectPage(ctx, c.SetProjectPageRequest{Mutation: mut(p.OperationID), ProjectID: pid, Content: p.Content,
		Summary: strings.TrimSpace(p.Summary), BaseRevisionID: strings.TrimSpace(p.BaseRevisionID)})
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
			"Replace your project's page, the owner's main view of it in the web app (leads only). Short, current markdown: goal, status, results with links, open decisions, next. Create it early; update at milestones.",
			obj(props{"content": str("the whole page in markdown (replaces the previous version)"),
				"summary":          str("one line: what changed"),
				"project":          str("project ID or name (default: this session's project)"),
				"base_revision_id": str("revision you edited (get_project_page); a newer page then fails instead of being replaced"),
				"operation_id":     opIDProp}, "content"),
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

func projectPageWritten(pg c.ProjectPage, now time.Time) string {
	if pg.Revision == nil {
		return fmt.Sprintf("project %s: page not written\n", pg.ProjectID)
	}
	return fmt.Sprintf("project %s page updated: revision %d (%s, %d bytes). The owner sees it in the web app; keep it current at milestones.\n",
		pg.ProjectID, pg.Revision.Seq, pg.Revision.RevisionID, pg.Revision.Size)
}

func projectPageText(pg c.ProjectPage, now time.Time) string {
	if pg.Revision == nil {
		return fmt.Sprintf("project %s has no page yet; create it with update_project_page (goal, status, results with links, open decisions, next)\n", pg.ProjectID)
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
		Use:   "page [show|set --file F|-]",
		Short: "The project's page: the owner's view of the project in the web app, kept current by its lead",
		Long: `Each project has one markdown page (document slug "page") that the project's
lead keeps current: goal, current status, results with links, open decisions,
what's next. The owner and top-level sessions in the project write it; every
write is a new revision. --project defaults to this session's project.`,
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
	return page
}
