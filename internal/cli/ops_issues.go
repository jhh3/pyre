package cli

// Issues: sessions file what went wrong with Pyre itself (pyre_bug), with a
// machine (machine), or with Pyre's own instructions (guidance), and keep
// working around it. Repeats fold into one issue on the hub; a triage
// session (`pyre issue triage set`) hears about new and recurring issues
// and closes them. report_issue, list_issues and get_issue are
// pre-approved; update_issue too (the hub allows only the owner and the
// triage session). The ops are optional (contracts.IssuesAPI).

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/jhh3/pyre/internal/buildinfo"
	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/credentials"
)

func init() { mcpToolProviders = append(mcpToolProviders, issueMCPTools) }

func (o *Ops) issues() (c.IssuesAPI, error) {
	if a, ok := o.API.(c.IssuesAPI); ok {
		return a, nil
	}
	return nil, c.Errorf(c.ErrUnsupported, "this connection does not offer issues (%s); upgrade pyre-hub", c.OpIssuesFile)
}

// ReportIssueParams are report_issue's arguments (`pyre issue file`).
type ReportIssueParams struct {
	Kind        string   `json:"kind"`
	Title       string   `json:"title"`
	Body        string   `json:"body,omitempty"`
	Related     []string `json:"related,omitempty"`
	Node        string   `json:"node,omitempty"`
	OperationID string   `json:"operation_id,omitempty"`
}

// ReportIssue files an issue. Inside a session it attaches the last lines
// of this machine's pyred log that mention the session's run.
func (o *Ops) ReportIssue(ctx context.Context, p ReportIssueParams) (c.FileIssueResponse, error) {
	api, err := o.issues()
	if err != nil {
		return c.FileIssueResponse{}, err
	}
	req := c.FileIssueRequest{Mutation: mut(p.OperationID), Kind: c.IssueKind(strings.ToLower(strings.TrimSpace(p.Kind))),
		Title: strings.TrimSpace(p.Title), Body: strings.TrimSpace(p.Body), Related: p.Related, PyreVersion: buildinfo.Version}
	switch {
	case !slices.Contains(c.IssueKinds, req.Kind):
		return c.FileIssueResponse{}, usagef("kind is pyre_bug (Pyre misbehaved), machine (a machine is broken), guidance (Pyre's instructions were wrong or confusing) or other")
	case req.Title == "":
		return c.FileIssueResponse{}, usagef("title is required: the problem in one line")
	}
	if n := strings.TrimSpace(p.Node); n != "" {
		if req.NodeID, err = o.NodeID(ctx, n); err != nil {
			return c.FileIssueResponse{}, err
		}
	}
	if !o.ClientCredential && o.Self.SessionID != "" {
		req.RunID = o.Self.RunID
		if lines := pyredLogLines(pyredLogPath(os.Getenv), o.Self.RunID, 10); lines != "" {
			req.Context = "pyred log on this machine (lines naming " + o.Self.RunID + "):\n" + lines
		}
	}
	return api.FileIssue(ctx, req)
}

// pyredLogPath is <pyred state dir>/logs/pyred.log: inside a session the
// state dir holds $PYRE_RUN_DIR (<state>/runs/<run>); otherwise it is
// resolved as daemon.DefaultStateDir does.
func pyredLogPath(getenv func(string) string) string {
	if rd := getenv("PYRE_RUN_DIR"); rd != "" {
		return filepath.Join(filepath.Dir(filepath.Dir(rd)), "logs", "pyred.log")
	}
	dir := getenv("PYRE_STATE_DIR")
	if dir == "" {
		home := getenv("HOME")
		if home == "" {
			home, _ = os.UserHomeDir()
		}
		switch {
		case runtime.GOOS == "windows" && getenv("LOCALAPPDATA") != "":
			dir = filepath.Join(getenv("LOCALAPPDATA"), "Pyre")
		case runtime.GOOS == "darwin":
			dir = filepath.Join(home, "Library", "Application Support", "pyre")
		case getenv("XDG_STATE_HOME") != "":
			dir = filepath.Join(getenv("XDG_STATE_HOME"), "pyre")
		default:
			dir = filepath.Join(home, ".local", "state", "pyre")
		}
	}
	return filepath.Join(dir, "logs", "pyred.log")
}

// pyredLogLines returns the last n lines of the log's final 256 KiB that
// name runID, redacted ("" when unreadable or none). Cheap by design: one
// bounded read, no rotation backups.
func pyredLogLines(path, runID string, n int) string {
	if runID == "" {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	const window = 256 << 10
	if st, err := f.Stat(); err == nil && st.Size() > window {
		if _, err := f.Seek(st.Size()-window, io.SeekStart); err != nil {
			return ""
		}
	}
	b, err := io.ReadAll(io.LimitReader(f, window))
	if err != nil {
		return ""
	}
	var out []string
	for _, line := range bytes.Split(b, []byte("\n")) {
		if bytes.Contains(line, []byte(runID)) {
			out = append(out, clip(string(line), 400))
		}
	}
	if len(out) > n {
		out = out[len(out)-n:]
	}
	return credentials.RedactPatterns(strings.Join(out, "\n"))
}

// ListIssuesParams are list_issues' arguments.
type ListIssuesParams struct {
	Status string `json:"status,omitempty"` // a status, or "all" (default: open and triaged)
	Kind   string `json:"kind,omitempty"`
	Node   string `json:"node,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

// ListIssues lists issues, most recently seen first.
func (o *Ops) ListIssues(ctx context.Context, p ListIssuesParams) (c.ListIssuesResponse, error) {
	api, err := o.issues()
	if err != nil {
		return c.ListIssuesResponse{}, err
	}
	req := c.ListIssuesRequest{Kind: c.IssueKind(strings.ToLower(strings.TrimSpace(p.Kind))), Limit: p.Limit}
	for _, s := range strings.Split(strings.ToLower(p.Status), ",") {
		switch s = strings.TrimSpace(s); {
		case s == "":
		case s == "all":
			req.All = true
		case slices.Contains(c.IssueStatuses, c.IssueStatus(s)):
			req.Statuses = append(req.Statuses, c.IssueStatus(s))
		default:
			return c.ListIssuesResponse{}, usagef("status is open, triaged, fixed, wontfix, duplicate or all")
		}
	}
	if n := strings.TrimSpace(p.Node); n != "" {
		if req.NodeID, err = o.NodeID(ctx, n); err != nil {
			return c.ListIssuesResponse{}, err
		}
	}
	return api.ListIssues(ctx, req)
}

// GetIssueParams are get_issue's arguments.
type GetIssueParams struct {
	IssueID string `json:"issue_id"`
}

// GetIssue shows an issue with its occurrences and notes.
func (o *Ops) GetIssue(ctx context.Context, p GetIssueParams) (c.IssueDetail, error) {
	api, err := o.issues()
	if err != nil {
		return c.IssueDetail{}, err
	}
	id := strings.TrimSpace(p.IssueID)
	if !c.ValidID(id, c.PrefixIssue) {
		return c.IssueDetail{}, usagef("issue_id must be an issue ID (iss_...); list_issues lists them")
	}
	return api.GetIssue(ctx, c.GetIssueRequest{IssueID: id})
}

// UpdateIssueParams are update_issue's arguments (`pyre issue close`,
// `pyre issue note`).
type UpdateIssueParams struct {
	IssueID     string   `json:"issue_id"`
	Status      string   `json:"status,omitempty"`
	Note        string   `json:"note,omitempty"`
	Links       []string `json:"links,omitempty"`
	DuplicateOf string   `json:"duplicate_of,omitempty"`
	OperationID string   `json:"operation_id,omitempty"`
}

// UpdateIssue changes an issue's status, adds a note or links (the owner
// or the triage session).
func (o *Ops) UpdateIssue(ctx context.Context, p UpdateIssueParams) (c.Issue, error) {
	api, err := o.issues()
	if err != nil {
		return c.Issue{}, err
	}
	id := strings.TrimSpace(p.IssueID)
	if !c.ValidID(id, c.PrefixIssue) {
		return c.Issue{}, usagef("issue_id must be an issue ID (iss_...); list_issues lists them")
	}
	st := c.IssueStatus(strings.ToLower(strings.TrimSpace(p.Status)))
	if st != "" && !slices.Contains(c.IssueStatuses, st) {
		return c.Issue{}, usagef("status is open, triaged, fixed, wontfix or duplicate")
	}
	dup := strings.TrimSpace(p.DuplicateOf)
	if dup != "" && st == "" {
		st = c.IssueDuplicate
	}
	return api.UpdateIssue(ctx, c.UpdateIssueRequest{Mutation: mut(p.OperationID), IssueID: id, Status: st,
		Note: strings.TrimSpace(p.Note), Links: p.Links, DuplicateOf: dup})
}

// IssueTriage shows the triage setting.
func (o *Ops) IssueTriage(ctx context.Context) (c.IssueTriage, error) {
	api, err := o.issues()
	if err != nil {
		return c.IssueTriage{}, err
	}
	return api.GetIssueTriage(ctx, struct{}{})
}

// SetIssueTriage names the triage session ("off" or "" turns it off).
func (o *Ops) SetIssueTriage(ctx context.Context, ref, opID string) (c.IssueTriage, error) {
	api, err := o.issues()
	if err != nil {
		return c.IssueTriage{}, err
	}
	sid := ""
	switch ref = strings.TrimSpace(ref); ref {
	case "", "off", "none":
	case "self":
		if sid = o.Self.SessionID; sid == "" {
			return c.IssueTriage{}, usagef("self names the calling session; run this inside a session or name one")
		}
	default:
		if sid, err = o.sessionID(ctx, ref); err != nil {
			return c.IssueTriage{}, err
		}
	}
	return api.SetIssueTriage(ctx, c.SetIssueTriageRequest{Mutation: mut(opID), SessionID: sid})
}

// ---- MCP ----

func issueMCPTools() []MCPTool {
	statusDesc := "open, triaged, fixed, wontfix or duplicate"
	return []MCPTool{
		compactly(tool("report_issue", "Report a Pyre or machine problem",
			"Report Pyre misbehaving (pyre_bug), a broken machine (machine) or wrong/confusing Pyre instructions (guidance), with evidence. Not for your own mistakes or the project's build/test failures. Keep working around it.",
			obj(props{"kind": map[string]any{"type": "string", "enum": []any{"pyre_bug", "machine", "guidance", "other"}, "description": "pyre_bug, machine, guidance or other"},
				"title":        str("the problem in one line (\"send_message to a stopped session reported delivered\")"),
				"body":         str("what you did, what happened, what you expected; evidence (exact errors, IDs, commands) in markdown"),
				"related":      strs("session or run IDs involved (yours are added automatically)"),
				"node":         str("the machine a machine issue is about (default: yours)"),
				"operation_id": opIDProp}, "kind", "title"),
			hints("Report a Pyre or machine problem", additive, false),
			func(ctx context.Context, o *Ops, p ReportIssueParams) (c.FileIssueResponse, error) {
				return o.ReportIssue(ctx, p)
			}),
			func(rc *renderCtx, r c.FileIssueResponse) string { return issueFiledText(r) }),
		compactly(tool("list_issues", "List issues",
			"List issues sessions reported (Pyre bugs, machine problems, guidance problems), most recently seen first. Default: open and triaged.",
			obj(props{"status": str(statusDesc + ", comma-separated, or all (default: open,triaged)"), "kind": str("pyre_bug, machine, guidance or other"),
				"node": str("only issues about this machine"), "limit": num("at most this many (default 100)")}),
			hints("List issues", readOnly, true),
			func(ctx context.Context, o *Ops, p ListIssuesParams) (c.ListIssuesResponse, error) {
				return o.ListIssues(ctx, p)
			}),
			func(rc *renderCtx, r c.ListIssuesResponse) string { return issuesText(r.Issues, rc.now) }),
		compactly(tool("get_issue", "Show an issue",
			"Show an issue with its reports (each with its evidence and context) and triage notes.",
			obj(props{"issue_id": str("issue ID (iss_...)")}, "issue_id"),
			hints("Show an issue", readOnly, true),
			func(ctx context.Context, o *Ops, p GetIssueParams) (c.IssueDetail, error) {
				return o.GetIssue(ctx, p)
			}),
			func(rc *renderCtx, d c.IssueDetail) string { return issueDetailText(d, rc.now) }),
		compactly(tool("update_issue", "Triage an issue",
			"Triage session or owner only: set an issue's status (fixed, wontfix, duplicate close it), add a note or links (PR URLs).",
			obj(props{"issue_id": str("issue ID (iss_...)"), "status": str(statusDesc), "note": str("what was found or done (shown as the resolution when closing)"),
				"links": strs("URLs to add (PRs, docs)"), "duplicate_of": str("with status duplicate: the issue it duplicates (later reports go there)"),
				"operation_id": opIDProp}, "issue_id"),
			hints("Triage an issue", idempotent, true),
			func(ctx context.Context, o *Ops, p UpdateIssueParams) (c.Issue, error) {
				return o.UpdateIssue(ctx, p)
			}),
			func(rc *renderCtx, is c.Issue) string { return issueLine(is, rc.now) + "\n" }),
	}
}

// ---- rendering ----

func issueFiledText(r c.FileIssueResponse) string {
	is := r.Issue
	var b strings.Builder
	switch r.Outcome {
	case c.IssueCreated:
		fmt.Fprintf(&b, "filed %s (%s): %s\n", is.IssueID, is.Kind, is.Title)
	case c.IssueReopened:
		fmt.Fprintf(&b, "added to %s, which was fixed and is open again (reported %d×): %s\n", is.IssueID, is.Occurrences, is.Title)
	default:
		fmt.Fprintf(&b, "added to existing %s (%s, reported %d×): %s\n", is.IssueID, is.Status, is.Occurrences, is.Title)
	}
	switch {
	case r.Throttled:
		b.WriteString("You filed many new issues in the last hour; this one is stored without telling the triage session. File only real Pyre or machine problems. ")
	case r.TriageSessionID != "":
		b.WriteString("The triage session " + r.TriageSessionID + " was told. ")
	}
	b.WriteString("Keep working around it if you can; contact the owner only if it blocks you and needs a human.\n")
	return b.String()
}

func issueWhere(is c.Issue) string {
	if is.NodeName != "" {
		return is.NodeName
	}
	return is.NodeID
}

// issueLine: `iss_… · open · pyre_bug · 3× · seen 2h ago · server · title`.
func issueLine(is c.Issue, now time.Time) string {
	parts := []string{is.IssueID, string(is.Status), string(is.Kind), fmt.Sprintf("%d×", is.Occurrences), "seen " + agoFrom(now, is.LastSeenAt)}
	if w := issueWhere(is); w != "" {
		parts = append(parts, w)
	}
	if is.PyreVersion != "" {
		parts = append(parts, "pyre "+is.PyreVersion)
	}
	parts = append(parts, clip(is.Title, 160))
	return strings.Join(parts, " · ")
}

func issuesText(issues []c.Issue, now time.Time) string {
	if len(issues) == 0 {
		return "no issues\n"
	}
	var b strings.Builder
	for _, is := range issues {
		b.WriteString(issueLine(is, now) + "\n")
	}
	fmt.Fprintf(&b, "%d issue", len(issues))
	if len(issues) != 1 {
		b.WriteString("s")
	}
	b.WriteString(" · details: get_issue / `pyre issue show <id>`\n")
	return b.String()
}

func indent(s, pre string) string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return ""
	}
	return pre + strings.ReplaceAll(s, "\n", "\n"+pre) + "\n"
}

func issueDetailText(d c.IssueDetail, now time.Time) string {
	is := d.Issue
	var b strings.Builder
	fmt.Fprintf(&b, "%s · %s · %s · reported %d× (first %s, last %s)\n%s\n", is.IssueID, is.Status, is.Kind, is.Occurrences,
		agoFrom(now, is.FirstSeenAt), agoFrom(now, is.LastSeenAt), is.Title)
	who := is.ReporterSessionID
	if is.ReporterTitle != "" {
		who = fmt.Sprintf("%q (%s)", is.ReporterTitle, is.ReporterSessionID)
	}
	if who == "" {
		who = string(is.ReportedBy.Kind)
	}
	fmt.Fprintf(&b, "first reported by %s", who)
	if w := issueWhere(is); w != "" {
		b.WriteString(" on " + w)
	}
	if is.Harness != "" {
		b.WriteString(" · " + strings.TrimSpace(string(is.Harness)+" "+is.HarnessVersion))
	}
	if is.PyreVersion != "" {
		b.WriteString(" · pyre " + is.PyreVersion)
	}
	if is.ProjectID != nil {
		b.WriteString(" · project " + *is.ProjectID)
	}
	b.WriteString("\n")
	if len(is.Links) > 0 {
		b.WriteString("links: " + strings.Join(is.Links, " ") + "\n")
	}
	if is.DuplicateOf != "" {
		b.WriteString("duplicate of " + is.DuplicateOf + "\n")
	}
	if is.Resolution != "" {
		b.WriteString("resolution: " + is.Resolution + "\n")
	}
	for _, o := range d.Occurrences {
		fmt.Fprintf(&b, "\n#%d · %s", o.Seq, agoFrom(now, o.At))
		if o.SessionID != "" {
			fmt.Fprintf(&b, " · %s", o.SessionID)
		} else {
			fmt.Fprintf(&b, " · %s", o.ReportedBy.Kind)
		}
		if o.RunID != "" {
			b.WriteString(" " + o.RunID)
		}
		if w := firstNonEmptyStr(o.NodeName, o.NodeID); w != "" {
			b.WriteString(" on " + w)
		}
		if o.PyreVersion != "" {
			b.WriteString(" · pyre " + o.PyreVersion)
		}
		b.WriteString("\n")
		if o.Seq > 1 && o.Title != is.Title {
			b.WriteString("  title: " + o.Title + "\n")
		}
		b.WriteString(indent(o.Body, "  "))
		if o.Context != "" {
			b.WriteString("  context:\n" + indent(o.Context, "    "))
		}
		if len(o.Related) > 0 {
			b.WriteString("  related: " + strings.Join(o.Related, " ") + "\n")
		}
	}
	if len(d.Occurrences) < is.Occurrences {
		fmt.Fprintf(&b, "\n(%d middle reports not shown)\n", is.Occurrences-len(d.Occurrences))
	}
	if len(d.Notes) > 0 {
		b.WriteString("\nnotes:\n")
	}
	for _, n := range d.Notes {
		fmt.Fprintf(&b, "- %s · %s", agoFrom(now, n.At), n.By.Kind)
		if n.By.Kind == c.PrincipalSession {
			b.WriteString(" " + n.By.ID)
		}
		if n.ToStatus != "" {
			fmt.Fprintf(&b, " · %s → %s", orDash(string(n.FromStatus)), n.ToStatus)
		}
		if n.Text != "" {
			b.WriteString(": " + n.Text)
		}
		if len(n.Links) > 0 {
			b.WriteString(" " + strings.Join(n.Links, " "))
		}
		b.WriteString("\n")
	}
	return b.String()
}

func firstNonEmptyStr(s ...string) string {
	for _, x := range s {
		if x != "" {
			return x
		}
	}
	return ""
}

func issueTriageText(t c.IssueTriage) string {
	if t.SessionID == "" {
		return "no triage session: issues are stored only (set one with `pyre issue triage set <session>`)\n"
	}
	s := fmt.Sprintf("triage session: %s", t.SessionID)
	if t.SessionTitle != "" {
		s += fmt.Sprintf(" %q", t.SessionTitle)
	}
	if t.Archived {
		s += " (archived or gone: nothing is sent; set another)"
	}
	return s + "\n"
}

// ---- CLI: pyre issue ----

func (a *App) addIssueCommands(root *cobra.Command) {
	issue := &cobra.Command{
		Use:     "issue [file|ls|show|close|note|triage]",
		Aliases: []string{"issues"},
		Short:   "Problems sessions reported with Pyre, machines or Pyre's guidance",
		Long: `Sessions report Pyre misbehaving (pyre_bug), a broken machine (machine) or
Pyre instructions that were wrong or confusing (guidance) with ` + "`pyre issue file`" + `
(MCP report_issue). Repeats of the same problem join one issue. A triage
session (` + "`pyre issue triage set <session>`" + `) is messaged about new issues, issues
reported again after a fix, and issues reaching 3, 10 and 50 reports; the owner's
inbox never is.`,
	}

	var fp ReportIssueParams
	var bodyFile string
	file := &cobra.Command{
		Use:   "file --kind pyre_bug|machine|guidance|other --title T [--body TEXT|-] [--body-file F|-] [--related ID]... [--node N]",
		Short: "Report a problem with Pyre, a machine or Pyre's guidance",
		Args:  cobra.NoArgs,
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			p := fp
			p.OperationID = a.opID
			if bodyFile == "-" {
				if p.Body != "" {
					return usagef("give --body or --body-file, not both")
				}
				p.Body, err = a.readArg("-")
			} else {
				p.Body, err = a.textOrFile(p.Body, bodyFile)
			}
			if err != nil {
				return err
			}
			r, err := ops.ReportIssue(cmd.Context(), p)
			if err != nil {
				return err
			}
			return a.emit(r, func(w io.Writer) { fmt.Fprint(w, issueFiledText(r)) })
		}),
	}
	ff := file.Flags()
	ff.StringVar(&fp.Kind, "kind", "", "pyre_bug, machine, guidance or other")
	ff.StringVar(&fp.Title, "title", "", "the problem in one line")
	ff.StringVar(&fp.Body, "body", "", "what happened, with evidence (markdown; - reads stdin)")
	ff.StringVar(&bodyFile, "body-file", "", "read the body from a file (- = stdin)")
	ff.StringSliceVar(&fp.Related, "related", nil, "session or run IDs involved (repeatable)")
	ff.StringVar(&fp.Node, "node", "", "the machine a machine issue is about (default: this session's)")
	issue.AddCommand(file)

	var lp ListIssuesParams
	ls := &cobra.Command{
		Use:   "ls [--status S[,S]|all] [--kind K] [--node N] [--limit N]",
		Short: "List issues (default: open and triaged), most recently seen first",
		Args:  cobra.NoArgs,
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			resp, err := ops.ListIssues(cmd.Context(), lp)
			if err != nil {
				return err
			}
			return a.emit(resp, func(w io.Writer) { fmt.Fprint(w, issuesText(resp.Issues, time.Now())) })
		}),
	}
	lf := ls.Flags()
	lf.StringVar(&lp.Status, "status", "", "open, triaged, fixed, wontfix, duplicate (comma-separated) or all")
	lf.StringVar(&lp.Kind, "kind", "", "pyre_bug, machine, guidance or other")
	lf.StringVar(&lp.Node, "node", "", "only issues about this machine")
	lf.IntVar(&lp.Limit, "limit", 0, "at most this many (default 100)")
	issue.AddCommand(ls)

	show := &cobra.Command{
		Use:   "show <issue_id>",
		Short: "Show an issue with its reports and notes",
		Args:  cobra.ExactArgs(1),
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			d, err := ops.GetIssue(cmd.Context(), GetIssueParams{IssueID: args[0]})
			if err != nil {
				return err
			}
			return a.emit(d, func(w io.Writer) { fmt.Fprint(w, issueDetailText(d, time.Now())) })
		}),
	}
	issue.AddCommand(show)

	update := func(cmd *cobra.Command, p UpdateIssueParams) error {
		ops, err := a.connect()
		if err != nil {
			return err
		}
		p.OperationID = a.opID
		is, err := ops.UpdateIssue(cmd.Context(), p)
		if err != nil {
			return err
		}
		return a.emit(is, func(w io.Writer) { fmt.Fprintln(w, issueLine(is, time.Now())) })
	}
	var cp UpdateIssueParams
	closeCmd := &cobra.Command{
		Use:   "close <issue_id> --status fixed|wontfix|duplicate [--note TEXT] [--link URL]... [--of iss_…]",
		Short: "Close an issue (owner or the triage session)",
		Args:  cobra.ExactArgs(1),
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			p := cp
			p.IssueID = args[0]
			switch c.IssueStatus(strings.ToLower(p.Status)) {
			case c.IssueFixed, c.IssueWontfix, c.IssueDuplicate:
			case "":
				if p.DuplicateOf == "" {
					return usagef("--status is fixed, wontfix or duplicate")
				}
			default:
				return usagef("--status is fixed, wontfix or duplicate (reopen or mark triaged with `pyre issue note --status`)")
			}
			return update(cmd, p)
		}),
	}
	cf := closeCmd.Flags()
	cf.StringVar(&cp.Status, "status", "", "fixed, wontfix or duplicate")
	cf.StringVar(&cp.Note, "note", "", "what was done (the resolution)")
	cf.StringSliceVar(&cp.Links, "link", nil, "URL to add, e.g. the PR (repeatable)")
	cf.StringVar(&cp.DuplicateOf, "of", "", "with --status duplicate: the issue it duplicates")
	issue.AddCommand(closeCmd)

	var np UpdateIssueParams
	note := &cobra.Command{
		Use:   "note <issue_id> \"text\" [--link URL]... [--status open|triaged]",
		Short: "Add a triage note, links or a status to an issue (owner or the triage session)",
		Args:  cobra.RangeArgs(1, 2),
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			p := np
			p.IssueID = args[0]
			if len(args) == 2 {
				text, err := a.readArg(args[1])
				if err != nil {
					return err
				}
				p.Note = text
			}
			return update(cmd, p)
		}),
	}
	nf := note.Flags()
	nf.StringSliceVar(&np.Links, "link", nil, "URL to add (repeatable)")
	nf.StringVar(&np.Status, "status", "", "open or triaged (close with `pyre issue close`)")
	issue.AddCommand(note)

	triage := &cobra.Command{
		Use:   "triage [show|set]",
		Short: "The session told about new and recurring issues",
	}
	triage.AddCommand(&cobra.Command{
		Use:   "show",
		Short: "Show the triage session",
		Args:  cobra.NoArgs,
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			t, err := ops.IssueTriage(cmd.Context())
			if err != nil {
				return err
			}
			return a.emit(t, func(w io.Writer) { fmt.Fprint(w, issueTriageText(t)) })
		}),
	})
	triage.AddCommand(&cobra.Command{
		Use:   "set <session>|self|off",
		Short: "Name the triage session, or turn triage off (owner, or the current triage session)",
		Args:  cobra.ExactArgs(1),
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			t, err := ops.SetIssueTriage(cmd.Context(), args[0], a.opID)
			if err != nil {
				return err
			}
			return a.emit(t, func(w io.Writer) { fmt.Fprint(w, issueTriageText(t)) })
		}),
	})
	issue.AddCommand(triage)
	root.AddCommand(issue)
}
