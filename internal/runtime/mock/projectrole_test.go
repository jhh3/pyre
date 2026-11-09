package mock

import (
	"context"
	"slices"
	"testing"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
)

// The mock reports a session's live project role in get_context like the
// hub (iss_…): a child started in a project by a
// parent outside it leads until the parent joins the project.
func TestLaunchContextRoleLikeTheHub(t *testing.T) {
	b, _, node := newEmpty(t)
	pr, err := b.CreateProject(as(owner), c.CreateProjectRequest{Mutation: op(), Name: "synthetic-role"})
	if err != nil {
		t.Fatal(err)
	}
	p := pr.ProjectID
	parent, child := start(t, b, node).Session.SessionID, start(t, b, node).Session.SessionID
	b.mu.Lock()
	b.sessions[child].ProjectID = &p
	b.sessions[child].Lineage.ParentSessionID = parent
	b.mu.Unlock()
	role := func(id string) *c.ProjectRole {
		t.Helper()
		lc, err := b.GetLaunchContext(as(c.Principal{Kind: c.PrincipalSession, ID: id}), c.ContextRequest{})
		if err != nil {
			t.Fatal(err)
		}
		return lc.Role
	}
	if r := role(child); r == nil || !r.Lead || r.LeadSessionID != child || r.ProjectID != p {
		t.Fatalf("child, parent outside: %+v", r)
	}
	if r := role(parent); r == nil || r.Lead || r.ProjectID != "" {
		t.Fatalf("parent in no project: %+v", r)
	}
	b.mu.Lock()
	b.sessions[parent].ProjectID = &p
	b.mu.Unlock()
	if r := role(child); r == nil || r.Lead || r.LeadSessionID != parent {
		t.Fatalf("child, parent joined: %+v", r)
	}
	if r := role(parent); r == nil || !r.Lead || r.LeadSessionID != parent {
		t.Fatalf("parent joined: %+v", r)
	}
	if lc, err := b.GetLaunchContext(as(owner), c.ContextRequest{ProjectID: &p}); err != nil || lc.Role != nil {
		t.Fatalf("owner: %+v %v", lc.Role, err)
	}
}

// Several leads (I63): the mock names one page owner among the live leads
// like the hub (the last page writer, else the longest-running), lists the
// co-leads, and never makes a bounded worker (TagRoleWorker) a lead.
func TestLaunchContextCoLeadsLikeTheHub(t *testing.T) {
	b, _, node := newEmpty(t)
	pr, err := b.CreateProject(as(owner), c.CreateProjectRequest{Mutation: op(), Name: "synthetic-co-leads"})
	if err != nil {
		t.Fatal(err)
	}
	p := pr.ProjectID
	coord, build, docs := start(t, b, node).Session.SessionID, start(t, b, node).Session.SessionID, start(t, b, node).Session.SessionID
	b.mu.Lock()
	for i, id := range []string{build, docs} {
		b.sessions[id].ProjectID = &p
		b.sessions[id].Lineage.ParentSessionID = coord
		b.sessions[id].CreatedAt = b.sessions[coord].CreatedAt.Add(time.Duration(i+1) * time.Minute)
	}
	b.mu.Unlock()
	ses := func(id string) context.Context { return as(c.Principal{Kind: c.PrincipalSession, ID: id}) }
	role := func(id string) *c.ProjectRole {
		t.Helper()
		lc, err := b.GetLaunchContext(ses(id), c.ContextRequest{})
		if err != nil {
			t.Fatal(err)
		}
		return lc.Role
	}
	if r := role(docs); r == nil || !r.Lead || r.PageOwner || r.PageOwnerSessionID != build || !slices.Equal(r.CoLeads, []string{build}) || r.PageUpdatedAt != nil {
		t.Fatalf("docs helper, no page: %+v", r)
	}
	if _, err := b.SetProjectPage(ses(docs), c.SetProjectPageRequest{Mutation: op(), Content: "# Synthetic"}); err != nil {
		t.Fatal(err)
	}
	if r := role(build); r == nil || r.PageOwner || r.PageOwnerSessionID != docs || r.PageUpdatedAt == nil {
		t.Fatalf("build lead after the helper wrote the page: %+v", r)
	}
	b.mu.Lock()
	b.sessions[docs].Tags = append(b.sessions[docs].Tags, c.TagRoleWorker)
	b.mu.Unlock()
	if r := role(docs); r == nil || r.Lead || r.LeadSessionID != "" || r.PageOwnerSessionID != build {
		t.Fatalf("worker: %+v", r)
	}
	if r := role(build); r == nil || !r.PageOwner || len(r.CoLeads) != 0 {
		t.Fatalf("build lead, the helper a worker: %+v", r)
	}
	if _, err := b.SetProjectPage(ses(docs), c.SetProjectPageRequest{Mutation: op(), Content: "# Synthetic 2"}); !c.IsCode(err, c.ErrForbidden) {
		t.Fatalf("worker page write: %v", err)
	}
}
