package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	c "github.com/jhh3/pyre/internal/contracts"
)

func parsePairs(vals []string, what string) ([][2]string, error) {
	var out [][2]string
	for _, v := range vals {
		k, val, ok := strings.Cut(v, "=")
		if !ok || k == "" || val == "" {
			return nil, usagef("%s must look like name=value, got %q", what, v)
		}
		out = append(out, [2]string{k, val})
	}
	return out, nil
}

func (a *App) addProjectCommands(root *cobra.Command) {
	project := &cobra.Command{Use: "project", Aliases: []string{"projects"}, Short: "Optional shared workspaces: brief, docs, artifacts, sessions", Args: cobra.NoArgs}

	var desc, brief, briefFile string
	var repos, links, newCheckouts []string
	newCmd := &cobra.Command{
		Use:   "new <name> [--repo id=url]... [--checkout node:path:repo:mode[:branch]]...",
		Short: "Create a project (only a name is required; no repository needed)",
		Long: `Create a project. Only the name is required: zero, one or several
repositories may be bound, and each repository may have checkouts recorded on
any node (node:/abs/path:repo:mode[:branch], mode shared|worktree|plain).
Sessions then launch into a recorded checkout with
` + "`pyre start --project P --repo R`" + ` (shared) or add --worktree B for an
isolated worktree created from it.`,
		Args: cobra.ExactArgs(1),
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			cp := CreateProjectParams{Name: args[0], Description: desc, Repos: repos, Links: links, Checkouts: newCheckouts, OperationID: a.opID}
			if cp.Brief, err = a.textOrFile(brief, briefFile); err != nil {
				return err
			}
			p, err := ops.CreateProject(cmd.Context(), cp)
			if err != nil {
				if p.ProjectID != "" {
					a.warnf("project %s was created, but recording its checkouts failed; retry with `pyre project update %s --checkout ...`", p.ProjectID, p.ProjectID)
				}
				return err
			}
			return a.emit(p, func(w io.Writer) {
				fmt.Fprintf(w, "created project %s %q (%d repos, %d checkouts)\n", p.ProjectID, p.Name, len(p.Repos), len(p.Checkouts))
			})
		}),
	}
	newCmd.Flags().StringVar(&desc, "description", "", "short description")
	newCmd.Flags().StringVar(&brief, "brief", "", "brief text (- reads stdin); becomes the brief document")
	newCmd.Flags().StringVar(&briefFile, "brief-file", "", "read the brief from a file")
	newCmd.Flags().StringSliceVar(&repos, "repo", nil, "repo binding id=url (repeatable)")
	newCmd.Flags().StringSliceVar(&links, "link", nil, "link title=url (repeatable)")
	newCmd.Flags().StringArrayVar(&newCheckouts, "checkout", nil, "record a checkout node:path:repo:mode[:branch] (repeatable; mode shared|worktree|plain; node ID or name)")
	project.AddCommand(newCmd)

	var lsAll bool
	var lsQuery string
	lsCmd := &cobra.Command{
		Use:   "ls",
		Short: "List projects",
		Args:  cobra.NoArgs,
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			page, err := listAll(cmd.Context(), func(ctx context.Context, cur string) (c.Page[c.Project], error) {
				return ops.API.ListProjects(ctx, c.ListProjectsRequest{PageRequest: c.PageRequest{Cursor: cur}, IncludeArchived: lsAll, Query: lsQuery})
			})
			if err != nil {
				return err
			}
			return a.emit(page, func(w io.Writer) {
				var rows [][]string
				for _, p := range page.Items {
					st := "active"
					if p.Archived {
						st = "archived"
					}
					rows = append(rows, []string{p.ProjectID, p.Name, st, fmt.Sprint(len(p.Repos)), ago(p.UpdatedAt), clip(p.Description, 40)})
				}
				table(w, "ID\tNAME\tSTATE\tREPOS\tUPDATED\tDESCRIPTION", rows)
			})
		}),
	}
	lsCmd.Flags().BoolVar(&lsAll, "all", false, "include archived projects")
	lsCmd.Flags().StringVar(&lsQuery, "query", "", "text search")
	project.AddCommand(lsCmd)

	project.AddCommand(&cobra.Command{
		Use:   "show <project>",
		Short: "Show a project (ID or name)",
		Args:  cobra.ExactArgs(1),
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			p, err := ops.ResolveProject(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return a.emit(p, func(w io.Writer) { printProject(w, p) })
		}),
	})

	var uName, uDesc, uEnv string
	var uRepos, uLinks, uCheckouts []string
	update := &cobra.Command{
		Use:   "update <project> [--name N] [--description D] [--env-profile P] [--repo id=url] [--link title=url] [--checkout node:path:repo:mode[:branch]]",
		Short: "Edit project metadata (compare-and-swap on the project version)",
		Args:  cobra.ExactArgs(1),
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			p, err := ops.ResolveProject(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			req := c.UpdateProjectRequest{Mutation: mut(a.opID), ProjectID: p.ProjectID, Version: p.Version}
			fl := cmd.Flags()
			if fl.Changed("name") {
				req.Name = &uName
			}
			if fl.Changed("description") {
				req.Description = &uDesc
			}
			if fl.Changed("env-profile") {
				req.EnvProfile = &uEnv
			}
			if len(uRepos) > 0 {
				rp, err := parsePairs(uRepos, "--repo")
				if err != nil {
					return err
				}
				rs := append([]c.RepoBinding{}, p.Repos...)
				for _, r := range rp {
					rs = append(rs, c.RepoBinding{RepoID: r[0], Name: r[0], URL: r[1]})
				}
				req.Repos = &rs
			}
			if len(uLinks) > 0 {
				lp, err := parsePairs(uLinks, "--link")
				if err != nil {
					return err
				}
				ls := append([]c.Link{}, p.Links...)
				for _, l := range lp {
					ls = append(ls, c.Link{Title: l[0], URL: l[1]})
				}
				req.Links = &ls
			}
			if len(uCheckouts) > 0 {
				cs := append([]c.CheckoutBinding{}, p.Checkouts...)
				for _, s := range uCheckouts {
					b, err := ops.parseCheckout(cmd.Context(), s)
					if err != nil {
						return err
					}
					// One binding per node+path: a repeat replaces it.
					replaced := false
					for i := range cs {
						if cs[i].NodeID == b.NodeID && cs[i].Path == b.Path {
							cs[i], replaced = b, true
						}
					}
					if !replaced {
						cs = append(cs, b)
					}
				}
				req.Checkouts = &cs
			}
			out, err := ops.API.UpdateProject(cmd.Context(), req)
			if err != nil {
				return err
			}
			return a.emit(out, func(w io.Writer) { fmt.Fprintf(w, "updated project %s (version %d)\n", out.ProjectID, out.Version) })
		}),
	}
	update.Flags().StringVar(&uName, "name", "", "new name")
	update.Flags().StringVar(&uDesc, "description", "", "new description")
	update.Flags().StringVar(&uEnv, "env-profile", "", "environment profile reference")
	update.Flags().StringSliceVar(&uRepos, "repo", nil, "add a repo binding id=url")
	update.Flags().StringSliceVar(&uLinks, "link", nil, "add a link title=url")
	update.Flags().StringArrayVar(&uCheckouts, "checkout", nil, "add or replace a checkout node:path:repo:mode[:branch] (mode shared|worktree|plain; node ID or name)")
	project.AddCommand(update)

	var unarchive bool
	archive := &cobra.Command{
		Use:   "archive <project>",
		Short: "Archive a project (or --unarchive)",
		Args:  cobra.ExactArgs(1),
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			p, err := ops.ResolveProject(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			v := !unarchive
			out, err := ops.API.UpdateProject(cmd.Context(), c.UpdateProjectRequest{Mutation: mut(a.opID), ProjectID: p.ProjectID, Version: p.Version, Archived: &v})
			if err != nil {
				return err
			}
			return a.emit(out, func(w io.Writer) { fmt.Fprintf(w, "project %s archived=%v\n", out.ProjectID, out.Archived) })
		}),
	}
	archive.Flags().BoolVar(&unarchive, "unarchive", false, "restore an archived project")
	project.AddCommand(archive)
	root.AddCommand(project)

	a.addDocCommands(root)

	var cp ContextParams
	ctxCmd := &cobra.Command{
		Use:   "context [--project P]",
		Short: "Print the bounded launch context (brief + retrieval pointers)",
		Args:  cobra.NoArgs,
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			lc, err := ops.Context(cmd.Context(), cp)
			if err != nil {
				return err
			}
			return a.emit(lc, func(w io.Writer) {
				fmt.Fprintln(w, strings.TrimRight(lc.Text, "\n"))
				fmt.Fprintf(w, "\n-- %d bytes", lc.Bytes)
				if lc.Truncated {
					fmt.Fprint(w, " (truncated)")
				}
				if lc.BriefRevision != "" {
					fmt.Fprintf(w, ", brief revision %s", lc.BriefRevision)
				}
				if lc.MemoryFreshness != nil {
					fmt.Fprintf(w, ", memory observed %s", ago(lc.MemoryFreshness.ObservedAt))
					if lc.MemoryFreshness.Stale {
						fmt.Fprint(w, " (stale)")
					}
				}
				fmt.Fprintln(w)
				for _, r := range lc.References {
					fmt.Fprintf(w, "   %s %s\n", r.Kind, r.ID)
				}
			})
		}),
	}
	ctxCmd.Flags().StringVar(&cp.Project, "project", "", "project ID or name (default: this session's project)")
	ctxCmd.Flags().IntVar(&cp.MaxBytes, "max-bytes", 0, "size bound (hub default 8 KiB)")
	root.AddCommand(ctxCmd)

	var np NoteParams
	var handoff, decision bool
	var bodyFile string
	note := &cobra.Command{
		Use:   `note "title" --body TEXT [--handoff|--decision] [--project P]`,
		Short: "Publish a note, handoff or decision to shared memory (promptly available to others)",
		Args:  cobra.ExactArgs(1),
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			if handoff && decision {
				return usagef("--handoff and --decision are mutually exclusive")
			}
			ops, err := a.connect()
			if err != nil {
				return err
			}
			p := np
			p.Title, p.OperationID = args[0], a.opID
			if p.Body, err = a.textOrFile(np.Body, bodyFile); err != nil {
				return err
			}
			switch {
			case handoff:
				p.Kind = string(c.NoteHandoff)
			case decision:
				p.Kind = string(c.NoteDecision)
			}
			n, err := ops.CreateNote(cmd.Context(), p)
			if err != nil {
				return err
			}
			return a.emit(n, func(w io.Writer) {
				fmt.Fprintf(w, "%s %s %q recorded (memory sync: %s)\n", n.Kind, n.NoteID, n.Title, orDash(n.SyncState))
			})
		}),
	}
	note.Flags().StringVar(&np.Body, "body", "", "note body (- reads stdin)")
	note.Flags().StringVar(&bodyFile, "body-file", "", "read the body from a file")
	note.Flags().BoolVar(&handoff, "handoff", false, "this is a handoff (include pointers: version/note IDs, branches)")
	note.Flags().BoolVar(&decision, "decision", false, "this is a decision")
	note.Flags().StringVar(&np.Project, "project", "", "project ID or name")
	a.addNotePromote(note) // `pyre note promote` (cmd_connections.go)
	root.AddCommand(note)

	var nlProject, nlKind, nlQuery string
	notes := &cobra.Command{
		Use:   "notes [--project P] [--kind K] [--query Q]",
		Short: "List notes, handoffs and decisions",
		Args:  cobra.NoArgs,
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			pid, err := ops.projectPtr(cmd.Context(), nlProject)
			if err != nil {
				return err
			}
			page, err := listAll(cmd.Context(), func(ctx context.Context, cur string) (c.Page[c.Note], error) {
				return ops.API.ListNotes(ctx, c.ListNotesRequest{PageRequest: c.PageRequest{Cursor: cur}, ProjectID: pid, Kind: c.NoteKind(nlKind), Query: nlQuery})
			})
			if err != nil {
				return err
			}
			return a.emit(page, func(w io.Writer) {
				var rows [][]string
				for _, n := range page.Items {
					rows = append(rows, []string{n.NoteID, string(n.Kind), ago(n.CreatedAt), principalText(n.Author), strPtr(n.ProjectID), clip(n.Title, 50)})
				}
				table(w, "ID\tKIND\tAGE\tAUTHOR\tPROJECT\tTITLE", rows)
			})
		}),
	}
	notes.Flags().StringVar(&nlProject, "project", "", "project ID or name")
	notes.Flags().StringVar(&nlKind, "kind", "", "note, handoff or decision")
	notes.Flags().StringVar(&nlQuery, "query", "", "text match")
	root.AddCommand(notes)
}

// textOrFile returns text (or stdin for "-") or the contents of file.
func (a *App) textOrFile(text, file string) (string, error) {
	if file != "" {
		if text != "" {
			return "", usagef("give text or a file, not both")
		}
		b, err := os.ReadFile(file)
		if err != nil {
			return "", c.Errorf(c.ErrInvalid, "%v", err)
		}
		return string(b), nil
	}
	return a.readArg(text)
}

// CreateProjectParams creates a project (`pyre project new`, MCP
// create_project). Only Name is required. Repos are id=url, Links
// title=url, Checkouts node:path:repo:mode[:branch] for one of Repos.
type CreateProjectParams struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Brief       string   `json:"brief,omitempty"`
	Repos       []string `json:"repos,omitempty"`
	Links       []string `json:"links,omitempty"`
	Checkouts   []string `json:"checkouts,omitempty"`
	OperationID string   `json:"operation_id,omitempty"`
}

// CreateProject validates every binding before creating anything. When
// the project is created but recording its checkouts fails, it returns
// the created project together with the error.
func (o *Ops) CreateProject(ctx context.Context, p CreateProjectParams) (c.Project, error) {
	if strings.TrimSpace(p.Name) == "" {
		return c.Project{}, usagef("a project name is required")
	}
	req := c.CreateProjectRequest{Mutation: mut(p.OperationID), Name: p.Name, Description: p.Description, Brief: p.Brief}
	rp, err := parsePairs(p.Repos, "--repo")
	if err != nil {
		return c.Project{}, err
	}
	for _, r := range rp {
		req.Repos = append(req.Repos, c.RepoBinding{RepoID: r[0], Name: r[0], URL: r[1]})
	}
	lp, err := parsePairs(p.Links, "--link")
	if err != nil {
		return c.Project{}, err
	}
	for _, l := range lp {
		req.Links = append(req.Links, c.Link{Title: l[0], URL: l[1]})
	}
	var cs []c.CheckoutBinding
	for _, v := range p.Checkouts {
		b, err := o.parseCheckout(ctx, v)
		if err != nil {
			return c.Project{}, err
		}
		if !hasRepo(req.Repos, b.RepoID) {
			return c.Project{}, usagef("--checkout %q: repo %q is not one of this project's --repo bindings", v, b.RepoID)
		}
		cs = append(cs, b)
	}
	prj, err := o.API.CreateProject(ctx, req)
	if err != nil || len(cs) == 0 {
		return prj, err
	}
	// Create has no checkout field; record them as the project's first
	// metadata update (same operation-ID lineage).
	op := ""
	if p.OperationID != "" {
		op = p.OperationID + ":checkouts"
	}
	up, err := o.API.UpdateProject(ctx, c.UpdateProjectRequest{Mutation: mut(op), ProjectID: prj.ProjectID, Version: prj.Version, Checkouts: &cs})
	if err != nil {
		return prj, err
	}
	return up, nil
}

func hasRepo(rs []c.RepoBinding, id string) bool {
	for _, r := range rs {
		if r.RepoID == id {
			return true
		}
	}
	return false
}

func printProject(w io.Writer, p c.Project) {
	fmt.Fprintf(w, "%s  %s", p.ProjectID, p.Name)
	if p.Archived {
		fmt.Fprint(w, "  (archived)")
	}
	fmt.Fprintln(w)
	if p.Description != "" {
		fmt.Fprintf(w, "  %s\n", p.Description)
	}
	fmt.Fprintf(w, "  brief: %s   env profile: %s   version %d   updated %s\n", orDash(p.BriefDocumentID), orDash(p.EnvProfile), p.Version, ago(p.UpdatedAt))
	for _, r := range p.Repos {
		fmt.Fprintf(w, "  repo %s %s\n", r.RepoID, orDash(r.URL))
	}
	for _, ck := range p.Checkouts {
		fmt.Fprintf(w, "  checkout %s %s %s:%s branch %s\n", orDash(ck.RepoID), ck.Mode, ck.NodeID, ck.Path, orDash(ck.Branch))
	}
	for _, l := range p.Links {
		fmt.Fprintf(w, "  link %s %s\n", l.Title, l.URL)
	}
	if len(p.Connections) > 0 {
		fmt.Fprintf(w, "  connections: %s\n", strings.Join(p.Connections, ", "))
	}
}

// ---- documents ----

// DocEditResult is `pyre doc edit --json` on conflict (with exit 3).
type DocEditResult struct {
	Error       *c.Error `json:"error"`
	ProposalID  string   `json:"proposal_id,omitempty"`
	CurrentHead string   `json:"current_head,omitempty"`
	DraftPath   string   `json:"draft_path,omitempty"`
}

func (a *App) addDocCommands(root *cobra.Command) {
	doc := &cobra.Command{Use: "doc", Aliases: []string{"docs"}, Short: "Versioned project documents (read, propose edits, history)", Args: cobra.NoArgs}
	var project string
	doc.PersistentFlags().StringVar(&project, "project", "", "project ID or name (needed to address a document by slug)")

	doc.AddCommand(&cobra.Command{
		Use:   "ls",
		Short: "List documents",
		Args:  cobra.NoArgs,
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			page, err := ops.ListDocuments(cmd.Context(), DocFilter{Project: project})
			if err != nil {
				return err
			}
			return a.emit(page, func(w io.Writer) {
				var rows [][]string
				for _, d := range page.Items {
					rows = append(rows, []string{d.DocumentID, d.Slug, clip(d.Title, 40), strPtr(d.ProjectID), d.HeadRevisionID, fmt.Sprint(d.HeadSeq), ago(d.UpdatedAt)})
				}
				table(w, "ID\tSLUG\tTITLE\tPROJECT\tHEAD\tSEQ\tUPDATED", rows)
			})
		}),
	})

	var revision string
	show := &cobra.Command{
		Use:   "show <doc>",
		Short: "Print a document (head, or --revision)",
		Args:  cobra.ExactArgs(1),
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			d, err := ops.GetDocument(cmd.Context(), DocRef{Document: args[0], Project: project, Revision: revision})
			if err != nil {
				return err
			}
			a.warnf("%s %q revision %s (seq %d) by %s %s", d.Document.DocumentID, d.Document.Title, d.Revision.RevisionID, d.Revision.Seq, principalText(d.Revision.Author), ago(d.Revision.CreatedAt))
			return a.emit(d, func(w io.Writer) {
				fmt.Fprint(w, d.Revision.Content)
				if !strings.HasSuffix(d.Revision.Content, "\n") {
					fmt.Fprintln(w)
				}
			})
		}),
	}
	show.Flags().StringVar(&revision, "revision", "", "a specific revision ID")
	doc.AddCommand(show)

	var nTitle, nSlug, nContent, nFile, nSummary string
	newDoc := &cobra.Command{
		Use:   "new --title T [--slug S] [--file F | --content TEXT]",
		Short: "Create a document (project optional)",
		Args:  cobra.NoArgs,
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			if nTitle == "" {
				return usagef("--title is required")
			}
			ops, err := a.connect()
			if err != nil {
				return err
			}
			content, err := a.textOrFile(nContent, nFile)
			if err != nil {
				return err
			}
			pid, err := ops.projectPtr(cmd.Context(), project)
			if err != nil {
				return err
			}
			mt := ""
			if nFile != "" {
				mt = detectMediaType(nFile)
				if !strings.HasPrefix(mt, "text/") {
					mt = ""
				}
			}
			d, err := ops.API.CreateDocument(cmd.Context(), c.CreateDocumentRequest{Mutation: mut(a.opID), ProjectID: pid, Title: nTitle, Slug: nSlug, MediaType: mt, Content: content, Summary: nSummary})
			if err != nil {
				return err
			}
			return a.emit(d, func(w io.Writer) {
				fmt.Fprintf(w, "created document %s (%s) revision %s\n", d.Document.DocumentID, d.Document.Slug, d.Revision.RevisionID)
			})
		}),
	}
	newDoc.Flags().StringVar(&nTitle, "title", "", "document title")
	newDoc.Flags().StringVar(&nSlug, "slug", "", "slug unique within the project")
	newDoc.Flags().StringVar(&nContent, "content", "", "content (- reads stdin)")
	newDoc.Flags().StringVar(&nFile, "file", "", "read content from a file")
	newDoc.Flags().StringVar(&nSummary, "summary", "", "revision summary")
	doc.AddCommand(newDoc)

	var eFile, eBase, eSummary string
	edit := &cobra.Command{
		Use:   "edit <doc> [--file F --base REV]",
		Short: "Edit a document in $EDITOR and propose it against the revision you started from",
		Long: `Opens the head revision in $VISUAL/$EDITOR, then proposes your text as the
successor of exactly that revision. If someone else changed the document
meanwhile, nothing is overwritten: the hub keeps your text as a proposal,
your draft file is kept, and the command exits 3 with the proposal ID.

Non-interactive (agents): --file F proposes F's contents; pass --base with
the revision you read so a concurrent change is detected.`,
		Args: cobra.ExactArgs(1),
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			return a.docEdit(cmd.Context(), ops, DocRef{Document: args[0], Project: project}, eFile, eBase, eSummary)
		}),
	}
	edit.Flags().StringVar(&eFile, "file", "", "propose this file's contents instead of opening an editor (- = stdin)")
	edit.Flags().StringVar(&eBase, "base", "", "revision the edit is based on (default: the head when the command starts)")
	edit.Flags().StringVar(&eSummary, "summary", "", "revision summary")
	doc.AddCommand(edit)

	doc.AddCommand(&cobra.Command{
		Use:   "history <doc>",
		Short: "List a document's revisions",
		Args:  cobra.ExactArgs(1),
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			d, err := ops.GetDocument(cmd.Context(), DocRef{Document: args[0], Project: project})
			if err != nil {
				return err
			}
			page, err := listAll(cmd.Context(), func(ctx context.Context, cur string) (c.Page[c.DocumentRevision], error) {
				return ops.API.ListRevisions(ctx, c.ListRevisionsRequest{PageRequest: c.PageRequest{Cursor: cur}, DocumentID: d.Document.DocumentID})
			})
			if err != nil {
				return err
			}
			return a.emit(page, func(w io.Writer) {
				var rows [][]string
				for _, r := range page.Items {
					head := ""
					if r.RevisionID == d.Document.HeadRevisionID {
						head = "head"
					}
					rows = append(rows, []string{fmt.Sprint(r.Seq), r.RevisionID, orDash(r.ParentRevisionID), principalText(r.Author), ago(r.CreatedAt), fmt.Sprint(r.Size), head, clip(r.Summary, 40)})
				}
				table(w, "SEQ\tREVISION\tPARENT\tAUTHOR\tAGE\tBYTES\t\tSUMMARY", rows)
			})
		}),
	})

	var pStates []string
	proposals := &cobra.Command{
		Use:   "proposals <doc>",
		Short: "List preserved proposals (e.g. conflicted edits)",
		Args:  cobra.ExactArgs(1),
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			d, err := ops.GetDocument(cmd.Context(), DocRef{Document: args[0], Project: project})
			if err != nil {
				return err
			}
			var states []c.ProposalState
			for _, s := range pStates {
				states = append(states, c.ProposalState(s))
			}
			page, err := listAll(cmd.Context(), func(ctx context.Context, cur string) (c.Page[c.DocumentProposal], error) {
				return ops.API.ListProposals(ctx, c.ListProposalsRequest{PageRequest: c.PageRequest{Cursor: cur}, DocumentID: d.Document.DocumentID, States: states})
			})
			if err != nil {
				return err
			}
			return a.emit(page, func(w io.Writer) {
				var rows [][]string
				for _, p := range page.Items {
					rows = append(rows, []string{p.ProposalID, string(p.State), p.BaseRevisionID, orDash(p.HeadAtConflict), principalText(p.Author), ago(p.CreatedAt), fmt.Sprint(len(p.Content))})
				}
				table(w, "PROPOSAL\tSTATE\tBASE\tHEAD AT CONFLICT\tAUTHOR\tAGE\tBYTES", rows)
			})
		}),
	}
	proposals.Flags().StringSliceVar(&pStates, "state", nil, "conflicted, applied, discarded")
	doc.AddCommand(proposals)

	doc.AddCommand(&cobra.Command{
		Use:   "discard <proposal_id>",
		Short: "Discard a preserved proposal (content kept for audit)",
		Args:  cobra.ExactArgs(1),
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			p, err := ops.API.DiscardProposal(cmd.Context(), c.DiscardProposalRequest{Mutation: mut(a.opID), ProposalID: args[0]})
			if err != nil {
				return err
			}
			return a.emit(p, func(w io.Writer) { fmt.Fprintf(w, "proposal %s: %s\n", p.ProposalID, p.State) })
		}),
	})
	root.AddCommand(doc)
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func (a *App) docEdit(ctx context.Context, ops *Ops, ref DocRef, file, base, summary string) error {
	d, err := ops.GetDocument(ctx, ref)
	if err != nil {
		return err
	}
	if base == "" {
		base = d.Revision.RevisionID
	}
	var content, draft string
	if file != "" {
		if file == "-" {
			b, err := io.ReadAll(io.LimitReader(a.Stdin, 16<<20))
			if err != nil {
				return err
			}
			content = string(b)
		} else {
			b, err := os.ReadFile(file)
			if err != nil {
				return c.Errorf(c.ErrInvalid, "%v", err)
			}
			content, draft = string(b), file
		}
	} else {
		if base != d.Revision.RevisionID {
			if d, err = ops.GetDocument(ctx, DocRef{Document: d.Document.DocumentID, Revision: base}); err != nil {
				return err
			}
		}
		dir := filepath.Join(DefaultStateDir(a.Getenv), "drafts")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		ext := ".md"
		if !strings.Contains(d.Document.MediaType, "markdown") && d.Document.MediaType != "" {
			ext = ".txt"
		}
		draft = filepath.Join(dir, unsafeName.ReplaceAllString(d.Document.DocumentID+"-"+base+"-"+d.Document.Slug, "_")+ext)
		if err := os.WriteFile(draft, []byte(d.Revision.Content), 0o600); err != nil {
			return err
		}
		editor := a.Getenv("VISUAL")
		if editor == "" {
			editor = a.Getenv("EDITOR")
		}
		if editor == "" {
			editor = "vi"
		}
		argv := append(strings.Fields(editor), draft)
		if err := a.Exec(argv); err != nil {
			return fmt.Errorf("editor %q failed; your draft is kept at %s: %w", editor, draft, err)
		}
		b, err := os.ReadFile(draft)
		if err != nil {
			return err
		}
		content = string(b)
		if content == d.Revision.Content {
			os.Remove(draft)
			a.warnf("no changes; nothing proposed")
			return a.emit(d, func(io.Writer) {})
		}
	}
	res, err := ops.Propose(ctx, ProposeParams{DocRef: DocRef{Document: d.Document.DocumentID}, Base: base, Content: content, Summary: summary, OperationID: a.opID})
	if err != nil {
		var ce *c.Error
		if errors.As(err, &ce) && ce.Code == c.ErrConflict {
			out := DocEditResult{Error: ce, ProposalID: detailString(ce, "proposal_id"), CurrentHead: detailString(ce, "current_head"), DraftPath: draft}
			if a.jsonOut {
				_ = a.emit(out, func(io.Writer) {})
			} else {
				fmt.Fprintf(a.Stdout, "conflict: %s changed since revision %s (head is now %s)\n", d.Document.DocumentID, base, orDash(out.CurrentHead))
				fmt.Fprintf(a.Stdout, "your edit was NOT applied; it is preserved as proposal %s\n", orDash(out.ProposalID))
				if draft != "" {
					fmt.Fprintf(a.Stdout, "your text is also kept at %s\n", draft)
				}
				fmt.Fprintf(a.Stdout, "compare with `pyre doc show %s`, then re-run `pyre doc edit %s --file %s --base %s`\n", d.Document.DocumentID, d.Document.DocumentID, orDash(draft), orDash(out.CurrentHead))
			}
			return withStatus(ExitConflict, err)
		}
		if draft != "" && file == "" {
			return fmt.Errorf("%w (your draft is kept at %s)", err, draft)
		}
		return err
	}
	if file == "" && draft != "" {
		os.Remove(draft)
	}
	return a.emit(res, func(w io.Writer) {
		fmt.Fprintf(w, "published %s revision %s (seq %d), based on %s\n", res.Document.DocumentID, res.Revision.RevisionID, res.Revision.Seq, base)
	})
}

func detailString(e *c.Error, key string) string {
	if e.Details == nil {
		return ""
	}
	if s, ok := e.Details[key].(string); ok {
		return s
	}
	return ""
}
