package assemble

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
)

// W-web-inbox project pages through the assembled hub (real projects and
// documents store): the lead writes its project's page directly, others
// may not, and every write is a new head revision of slug "page".
func TestProjectPageEndToEnd(t *testing.T) {
	st, n := newStack(t)
	bg := context.Background()
	owner := c.WithPrincipal(bg, ownerP)
	op := func() c.Mutation { return c.Mutation{OperationID: c.NewID(c.PrefixOperation)} }
	project := func(name string) string {
		p, err := st.Hub.CreateProject(owner, c.CreateProjectRequest{Mutation: op(), Name: name})
		if err != nil {
			t.Fatal(err)
		}
		return p.ProjectID
	}
	pa, pb := project("synthetic-alpha"), project("synthetic-beta")
	start := func(ctx context.Context, title string, projectID *string) context.Context {
		t.Helper()
		resp, err := st.Hub.StartSession(ctx, c.StartSessionRequest{Mutation: op(), NodeID: n.nodeID, Harness: c.HarnessMock, Cwd: "/tmp/synth", Title: title, ProjectID: projectID})
		if err != nil {
			t.Fatal(err)
		}
		n.idle()
		return c.WithPrincipal(bg, c.Principal{Kind: c.PrincipalSession, ID: resp.Session.Session.SessionID})
	}
	lead := start(owner, "synthetic lead", &pa)
	child := start(lead, "synthetic child", nil) // joins the parent's project
	loner := start(owner, "synthetic loner", nil)

	// No page yet.
	pg, err := st.Hub.GetProjectPage(lead, c.GetProjectPageRequest{})
	if err != nil || pg.ProjectID != pa || pg.Revision != nil || pg.Content != "" {
		t.Fatalf("empty page: %+v %v", pg, err)
	}
	// The lead creates it (default project: its own) and revises it.
	v1, err := st.Hub.SetProjectPage(lead, c.SetProjectPageRequest{Mutation: op(), Content: "# Synthetic alpha\n\nGoal: synthetic.", Summary: "first"})
	if err != nil || v1.Document == nil || v1.Document.Slug != c.ProjectPageSlug || v1.Revision.Seq != 1 || v1.Content == "" {
		t.Fatalf("create: %+v %v", v1, err)
	}
	v2, err := st.Hub.SetProjectPage(lead, c.SetProjectPageRequest{Mutation: op(), Content: "# Synthetic alpha\n\nStatus: done.", BaseRevisionID: v1.Revision.RevisionID})
	if err != nil || v2.Revision.Seq != 2 || v2.Document.DocumentID != v1.Document.DocumentID {
		t.Fatalf("revise: %+v %v", v2, err)
	}
	// A stale base is a conflict; without a base the write lands on the head.
	if _, err := st.Hub.SetProjectPage(lead, c.SetProjectPageRequest{Mutation: op(), Content: "stale", BaseRevisionID: v1.Revision.RevisionID}); !c.IsCode(err, c.ErrConflict) {
		t.Fatalf("stale base: %v", err)
	}
	v3, err := st.Hub.SetProjectPage(owner, c.SetProjectPageRequest{Mutation: op(), ProjectID: pa, Content: "# Synthetic alpha\n\nOwner note."})
	if err != nil || v3.Revision.Seq != 3 || v3.Revision.Author.Kind != c.PrincipalOwner {
		t.Fatalf("owner write: %+v %v", v3, err)
	}
	if got, err := st.Hub.GetProjectPage(child, c.GetProjectPageRequest{}); err != nil || got.Revision == nil || got.Revision.RevisionID != v3.Revision.RevisionID || got.Content != v3.Content {
		t.Fatalf("child reads the page: %+v %v", got, err)
	}

	// Permissions.
	if _, err := st.Hub.SetProjectPage(child, c.SetProjectPageRequest{Mutation: op(), Content: "child"}); !c.IsCode(err, c.ErrForbidden) {
		t.Fatalf("child write: %v", err)
	}
	if _, err := st.Hub.SetProjectPage(lead, c.SetProjectPageRequest{Mutation: op(), ProjectID: pb, Content: "other project"}); !c.IsCode(err, c.ErrForbidden) {
		t.Fatalf("other project: %v", err)
	}
	if _, err := st.Hub.SetProjectPage(loner, c.SetProjectPageRequest{Mutation: op(), Content: "no project"}); !c.IsCode(err, c.ErrInvalid) {
		t.Fatalf("no project: %v", err)
	}
	if _, err := st.Hub.SetProjectPage(owner, c.SetProjectPageRequest{Mutation: op(), Content: "no project"}); !c.IsCode(err, c.ErrInvalid) {
		t.Fatalf("owner without project: %v", err)
	}
	if _, err := st.Hub.SetProjectPage(owner, c.SetProjectPageRequest{Mutation: op(), ProjectID: "prj_synthmissing", Content: "x"}); !c.IsCode(err, c.ErrNotFound) {
		t.Fatalf("missing project: %v", err)
	}
	if _, err := st.Hub.SetProjectPage(lead, c.SetProjectPageRequest{Mutation: op(), Content: "  "}); !c.IsCode(err, c.ErrInvalid) {
		t.Fatalf("empty content: %v", err)
	}

	// Clients refresh on document.revised for the project.
	evs, err := st.Hub.ListEvents(owner, c.ListEventsRequest{Kinds: []c.EventKind{c.EvDocumentRevised}, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, ev := range evs.Events {
		if ev.ProjectID != nil && *ev.ProjectID == pa {
			seen++
		}
	}
	if seen != 3 {
		t.Fatalf("document.revised events for the project: %d", seen)
	}

	// The status card: written alone (the page is unchanged), read back
	// with the page, replaced whole by the next write, validated.
	pct := 40
	sv, err := st.Hub.SetProjectPage(lead, c.SetProjectPageRequest{Mutation: op(), Summary: "status", Status: &c.ProjectStatus{Progress: &pct, Phase: "synthetic phase",
		ReadyForOwner: []c.StatusItem{{Title: "synthetic PR", URL: "https://example.invalid/pr/1", State: "review"}}}})
	if err != nil || sv.Status == nil || *sv.Status.Progress != 40 || sv.StatusUpdatedAt == nil || sv.Revision == nil || sv.Revision.RevisionID != v3.Revision.RevisionID {
		t.Fatalf("status write: %+v %v", sv, err)
	}
	got, err := st.Hub.GetProjectPage(child, c.GetProjectPageRequest{})
	if err != nil || got.Status == nil || got.Status.Phase != "synthetic phase" || len(got.Status.ReadyForOwner) != 1 || got.Content != v3.Content {
		t.Fatalf("status read: %+v %v", got, err)
	}
	pct = 70
	both, err := st.Hub.SetProjectPage(lead, c.SetProjectPageRequest{Mutation: op(), Content: "# Synthetic alpha\n\nNearly there.", Status: &c.ProjectStatus{Progress: &pct}})
	if err != nil || both.Revision.Seq != 4 || *both.Status.Progress != 70 || both.Status.Phase != "" || both.Status.ReadyForOwner != nil {
		t.Fatalf("page and status: %+v %v", both, err)
	}
	bad := 101
	if _, err := st.Hub.SetProjectPage(lead, c.SetProjectPageRequest{Mutation: op(), Status: &c.ProjectStatus{Progress: &bad}}); !c.IsCode(err, c.ErrInvalid) {
		t.Fatalf("progress 101: %v", err)
	}
	if _, err := st.Hub.SetProjectPage(lead, c.SetProjectPageRequest{Mutation: op(), Status: &c.ProjectStatus{Shipped: []c.StatusItem{{URL: "x"}}}}); !c.IsCode(err, c.ErrInvalid) {
		t.Fatalf("untitled item: %v", err)
	}
	if _, err := st.Hub.SetProjectPage(lead, c.SetProjectPageRequest{Mutation: op(), Status: &c.ProjectStatus{Shipped: []c.StatusItem{{Title: "x", URL: "javascript:alert(1)"}}}}); !c.IsCode(err, c.ErrInvalid) {
		t.Fatalf("non-http url: %v", err)
	}
	if _, err := st.Hub.SetProjectPage(child, c.SetProjectPageRequest{Mutation: op(), Status: &c.ProjectStatus{Phase: "child"}}); !c.IsCode(err, c.ErrForbidden) {
		t.Fatalf("child status write: %v", err)
	}
}

// A delegated lead (iss_…): a coordinator in one
// project starts a session in another and appoints it that project's lead.
// The delegated lead writes the page and the status card; a child it
// starts in the same project may not; a top-level lead, an ancestor that
// leads the project and the owner still may.
func TestProjectPageDelegatedLead(t *testing.T) {
	st, n := newStack(t)
	bg := context.Background()
	owner := c.WithPrincipal(bg, ownerP)
	op := func() c.Mutation { return c.Mutation{OperationID: c.NewID(c.PrefixOperation)} }
	project := func(name string) string {
		p, err := st.Hub.CreateProject(owner, c.CreateProjectRequest{Mutation: op(), Name: name})
		if err != nil {
			t.Fatal(err)
		}
		return p.ProjectID
	}
	coordP, ticket, other := project("synthetic-coordinator"), project("synthetic-ticket"), project("synthetic-other")
	ids := map[context.Context]string{}
	start := func(ctx context.Context, title string, projectID *string) context.Context {
		t.Helper()
		resp, err := st.Hub.StartSession(ctx, c.StartSessionRequest{Mutation: op(), NodeID: n.nodeID, Harness: c.HarnessMock, Cwd: "/tmp/synth", Title: title, ProjectID: projectID})
		if err != nil {
			t.Fatal(err)
		}
		n.idle()
		id := resp.Session.Session.SessionID
		sctx := c.WithPrincipal(bg, c.Principal{Kind: c.PrincipalSession, ID: id})
		ids[sctx] = id
		return sctx
	}
	pct := 10
	write := func(ctx context.Context, projectID, content string) error {
		_, err := st.Hub.SetProjectPage(ctx, c.SetProjectPageRequest{Mutation: op(), ProjectID: projectID, Content: content})
		return err
	}
	status := func(ctx context.Context, projectID string) error {
		_, err := st.Hub.SetProjectPage(ctx, c.SetProjectPageRequest{Mutation: op(), ProjectID: projectID, Status: &c.ProjectStatus{Progress: &pct, Phase: "synthetic"}})
		return err
	}

	coord := start(owner, "synthetic coordinator", &coordP)       // top-level, its own project
	lead := start(coord, "synthetic ticket lead", &ticket)        // delegated: its parent is outside the ticket project
	helper := start(lead, "synthetic ticket helper", nil)         // joins the lead's project
	grand := start(helper, "synthetic ticket grandchild", nil)    // deeper in the same project
	elsewhere := start(lead, "synthetic other lead", &other)      // the lead delegates another project
	back := start(elsewhere, "synthetic back in ticket", &ticket) // its parent is outside the ticket: a lead too

	// The delegated lead owns the page and the card (its default project).
	if err := write(lead, "", "# Synthetic ticket\n\nGoal: synthetic."); err != nil {
		t.Fatalf("delegated lead page: %v", err)
	}
	if err := status(lead, ""); err != nil {
		t.Fatalf("delegated lead status: %v", err)
	}
	// Children within the same project may not, and are told who leads.
	for name, ctx := range map[string]context.Context{"helper": helper, "grandchild": grand} {
		err := write(ctx, "", "child")
		if !c.IsCode(err, c.ErrForbidden) || !strings.Contains(err.Error(), "report to your parent session") || !strings.Contains(err.Error(), ids[lead]) {
			t.Fatalf("%s page: %v", name, err)
		}
		if err := status(ctx, ""); !c.IsCode(err, c.ErrForbidden) {
			t.Fatalf("%s status: %v", name, err)
		}
	}
	// An ancestor of back (lead) leads the ticket, and so does back.
	if err := write(back, "", "# Synthetic ticket\n\nFrom the other side."); err != nil {
		t.Fatalf("lead via another project: %v", err)
	}
	if err := write(lead, ticket, "# Synthetic ticket\n\nAgain."); err != nil {
		t.Fatalf("ancestor lead: %v", err)
	}
	// A top-level lead writes its own project's page, not the ticket's.
	if err := write(coord, "", "# Synthetic coordinator"); err != nil {
		t.Fatalf("top-level lead: %v", err)
	}
	if err := write(coord, ticket, "not mine"); !c.IsCode(err, c.ErrForbidden) {
		t.Fatalf("coordinator on the ticket page: %v", err)
	}
	// The ticket lead does not lead the project it delegated.
	if err := write(lead, other, "not mine"); !c.IsCode(err, c.ErrForbidden) {
		t.Fatalf("lead on another project: %v", err)
	}
	if err := status(elsewhere, ""); err != nil {
		t.Fatalf("second delegated lead: %v", err)
	}
	// The owner still writes any page.
	if err := write(owner, ticket, "# Synthetic ticket\n\nOwner note."); err != nil {
		t.Fatalf("owner: %v", err)
	}
	if err := status(owner, ticket); err != nil {
		t.Fatalf("owner status: %v", err)
	}
}

// iss_…: a session outside any project creates a
// project and starts a child in it (the child leads: its parent is
// outside), then joins the project itself. From then on the parent leads:
// get_context reports each session's live role by the same rule the page
// write checks, so the child is told it no longer leads, and the hub
// refuses its page write naming the parent.
func TestProjectRoleFollowsParentJoiningLater(t *testing.T) {
	st, n := newStack(t)
	bg := context.Background()
	owner := c.WithPrincipal(bg, ownerP)
	op := func() c.Mutation { return c.Mutation{OperationID: c.NewID(c.PrefixOperation)} }
	start := func(ctx context.Context, title string, projectID *string) (context.Context, string) {
		t.Helper()
		resp, err := st.Hub.StartSession(ctx, c.StartSessionRequest{Mutation: op(), NodeID: n.nodeID, Harness: c.HarnessMock, Cwd: "/tmp/synth", Title: title, ProjectID: projectID})
		if err != nil {
			t.Fatal(err)
		}
		n.idle()
		id := resp.Session.Session.SessionID
		return c.WithPrincipal(bg, c.Principal{Kind: c.PrincipalSession, ID: id}), id
	}
	role := func(ctx context.Context) (*c.ProjectRole, string) {
		t.Helper()
		lc, err := st.Hub.GetLaunchContext(ctx, c.ContextRequest{})
		if err != nil {
			t.Fatal(err)
		}
		return lc.Role, lc.Text
	}
	write := func(ctx context.Context) error {
		_, err := st.Hub.SetProjectPage(ctx, c.SetProjectPageRequest{Mutation: op(), Content: "# Synthetic\n\nStatus."})
		return err
	}

	parent, parentID := start(owner, "synthetic coordinator", nil)
	pr, err := st.Hub.CreateProject(parent, c.CreateProjectRequest{Mutation: op(), Name: "synthetic-joined-later"})
	if err != nil {
		t.Fatal(err)
	}
	p := pr.ProjectID
	child, childID := start(parent, "synthetic worker", &p)

	// The parent is outside the project: the child leads it.
	if r, text := role(child); r == nil || !r.Lead || r.LeadSessionID != childID || r.ProjectID != p || !strings.Contains(text, "You lead project "+p) {
		t.Fatalf("child before the parent joins: %+v\n%s", r, text)
	}
	if r, _ := role(parent); r == nil || r.ProjectID != "" || r.Lead {
		t.Fatalf("parent in no project: %+v", r)
	}

	// The parent joins: it leads now, and the child is told so.
	if _, err := st.Hub.AssociateSession(parent, c.AssociateRequest{Mutation: op(), SessionID: parentID, ProjectID: &p}); err != nil {
		t.Fatal(err)
	}
	r, text := role(child)
	if r == nil || r.Lead || r.LeadSessionID != parentID || r.ProjectID != p || !strings.Contains(text, "Session "+parentID+" leads project "+p+" above you") {
		t.Fatalf("child after the parent joined: %+v\n%s", r, text)
	}
	if err := write(child); !c.IsCode(err, c.ErrForbidden) || !strings.Contains(err.Error(), parentID) {
		t.Fatalf("child page write: %v", err)
	}
	if r, text := role(parent); r == nil || !r.Lead || r.LeadSessionID != parentID || !strings.Contains(text, "You lead project "+p) {
		t.Fatalf("parent after joining: %+v\n%s", r, text)
	}
	if err := write(parent); err != nil {
		t.Fatalf("parent page write: %v", err)
	}
	// Asked about another project it is not in, a session has no lead there.
	other, err := st.Hub.CreateProject(owner, c.CreateProjectRequest{Mutation: op(), Name: "synthetic-elsewhere"})
	if err != nil {
		t.Fatal(err)
	}
	if lc, err := st.Hub.GetLaunchContext(child, c.ContextRequest{ProjectID: &other.ProjectID}); err != nil || lc.Role == nil || lc.Role.Lead || lc.Role.LeadSessionID != "" || lc.Role.ProjectID != other.ProjectID {
		t.Fatalf("another project: %+v %v", lc.Role, err)
	}
	// The owner has no role.
	if lc, err := st.Hub.GetLaunchContext(owner, c.ContextRequest{ProjectID: &p}); err != nil || lc.Role != nil {
		t.Fatalf("owner: %+v %v", lc.Role, err)
	}
}

// iss_… (I63): a coordinator outside a project
// starts a build lead and a bounded docs helper in it, so both lead. The
// role names one page owner among the live leads (the last to write the
// page or its status card, else the longest-running), lists the co-leads
// and says when the page was last written by anyone. A --no-lead worker
// (TagRoleWorker) never leads and may not write the page.
func TestProjectRoleCoLeadsAndPageOwner(t *testing.T) {
	st, n := newStack(t)
	bg := context.Background()
	owner := c.WithPrincipal(bg, ownerP)
	op := func() c.Mutation { return c.Mutation{OperationID: c.NewID(c.PrefixOperation)} }
	type sess struct {
		ctx     context.Context
		id, run string
	}
	start := func(ctx context.Context, title string, projectID *string, tags ...string) sess {
		t.Helper()
		n.clock.Advance(time.Minute)
		resp, err := st.Hub.StartSession(ctx, c.StartSessionRequest{Mutation: op(), NodeID: n.nodeID, Harness: c.HarnessMock, Cwd: "/tmp/synth", Title: title, ProjectID: projectID, Tags: tags})
		if err != nil {
			t.Fatal(err)
		}
		n.idle()
		id := resp.Session.Session.SessionID
		return sess{c.WithPrincipal(bg, c.Principal{Kind: c.PrincipalSession, ID: id}), id, resp.Session.Run.RunID}
	}
	role := func(s sess) (c.ProjectRole, string) {
		t.Helper()
		lc, err := st.Hub.GetLaunchContext(s.ctx, c.ContextRequest{})
		if err != nil || lc.Role == nil {
			t.Fatalf("context: %+v %v", lc.Role, err)
		}
		return *lc.Role, lc.Text
	}
	write := func(s sess) error {
		n.clock.Advance(time.Minute)
		_, err := st.Hub.SetProjectPage(s.ctx, c.SetProjectPageRequest{Mutation: op(), Content: "# Synthetic\n\nStatus."})
		return err
	}

	coord := start(owner, "synthetic coordinator", nil)
	pr, err := st.Hub.CreateProject(coord.ctx, c.CreateProjectRequest{Mutation: op(), Name: "synthetic-co-leads"})
	if err != nil {
		t.Fatal(err)
	}
	p := pr.ProjectID
	build := start(coord.ctx, "synthetic build lead", &p)
	docs := start(coord.ctx, "synthetic hourly docs check", &p)

	// No page yet: the longest-running live lead keeps it.
	r, text := role(docs)
	if !r.Lead || r.PageOwner || r.PageOwnerSessionID != build.id || !slices.Equal(r.CoLeads, []string{build.id}) || r.PageUpdatedAt != nil ||
		!strings.Contains(text, "together with "+build.id) || !strings.Contains(text, "Session "+build.id+" maintains its project page and status card: leave them to it") {
		t.Fatalf("docs helper, no page: %+v\n%s", r, text)
	}
	if r, text := role(build); !r.Lead || !r.PageOwner || r.PageOwnerSessionID != build.id || !slices.Equal(r.CoLeads, []string{docs.id}) ||
		!strings.Contains(text, "You maintain its project page and status card") {
		t.Fatalf("build lead, no page: %+v\n%s", r, text)
	}

	// The helper writes the page once (still allowed: authorization is
	// unchanged), so it is the last writer; then the build lead writes it.
	if err := write(docs); err != nil {
		t.Fatalf("a co-lead may still write the page: %v", err)
	}
	if r, _ := role(docs); !r.PageOwner || r.PageOwnerSessionID != docs.id {
		t.Fatalf("after the helper wrote the page: %+v", r)
	}
	if err := write(build); err != nil {
		t.Fatal(err)
	}
	r, _ = role(docs)
	if r.PageOwner || r.PageOwnerSessionID != build.id || r.PageUpdatedAt == nil || !r.PageUpdatedAt.Equal(n.clock.Now()) {
		t.Fatalf("after the build lead wrote the page: %+v (now %v)", r, n.clock.Now())
	}
	// A status-card write counts as a page write.
	n.clock.Advance(time.Minute)
	if _, err := st.Hub.SetProjectPage(docs.ctx, c.SetProjectPageRequest{Mutation: op(), Status: &c.ProjectStatus{Phase: "synthetic"}}); err != nil {
		t.Fatal(err)
	}
	if r, _ := role(build); r.PageOwner || r.PageOwnerSessionID != docs.id || !r.PageUpdatedAt.Equal(n.clock.Now()) {
		t.Fatalf("after the helper set the status card: %+v", r)
	}

	// A lead whose run ended is no longer a candidate.
	if err := n.SetRunState(bg, docs.run, c.RunExited); err != nil {
		t.Fatal(err)
	}
	n.idle()
	if r, text := role(build); !r.PageOwner || len(r.CoLeads) != 0 || !strings.Contains(text, "You lead project "+p+" (as of now): you own") {
		t.Fatalf("the helper exited: %+v\n%s", r, text)
	}

	// A bounded worker never leads: its role names the page owner, its page
	// write is refused, and it is not a co-lead.
	worker := start(coord.ctx, "synthetic bounded worker", &p, c.TagRoleWorker)
	r, text = role(worker)
	if r.Lead || r.LeadSessionID != "" || r.PageOwner || r.PageOwnerSessionID != build.id || len(r.CoLeads) != 0 ||
		!strings.Contains(text, "without leading it") || !strings.Contains(text, "session "+build.id+" maintains its project page") {
		t.Fatalf("worker: %+v\n%s", r, text)
	}
	if err := write(worker); !c.IsCode(err, c.ErrForbidden) || !strings.Contains(err.Error(), "bounded worker") {
		t.Fatalf("worker page write: %v", err)
	}
	if r, _ := role(build); len(r.CoLeads) != 0 {
		t.Fatalf("a worker is not a co-lead: %+v", r)
	}
	// A child of the worker has no lead above it either.
	child := start(worker.ctx, "synthetic worker child", &p)
	if r, _ := role(child); r.Lead || r.LeadSessionID != "" || r.PageOwnerSessionID != build.id {
		t.Fatalf("worker's child: %+v", r)
	}
}
