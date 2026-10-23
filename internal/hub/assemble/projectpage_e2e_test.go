package assemble

import (
	"context"
	"testing"

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
