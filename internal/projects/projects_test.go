package projects

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/projects/svcutil/svctest"
)

type fixture struct {
	s    *Store
	sink *svctest.Sink
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	db := svctest.OpenDB(t)
	sink := svctest.NewSink(t, db)
	s, err := New(context.Background(), db, sink, Options{Clock: &svctest.FixedClock{T: time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)}})
	if err != nil {
		t.Fatal(err)
	}
	return fixture{s: s, sink: sink}
}

func ptr(s string) *string { return &s }

func mustCode(t *testing.T, err error, code contracts.ErrorCode) *contracts.Error {
	t.Helper()
	if !contracts.IsCode(err, code) {
		t.Fatalf("want %s, got %v", code, err)
	}
	return contracts.AsError(err)
}

func TestCreateProjectMinimalAndRepos(t *testing.T) {
	f := newFixture(t)
	ctx := svctest.Owner()
	p, err := f.s.CreateProject(ctx, contracts.CreateProjectRequest{Mutation: svctest.Op(), Name: "Research only"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Version != 1 || len(p.Repos) != 0 || p.BriefDocumentID != "" || p.CreatedBy.ID != "cli_synth-laptop" {
		t.Fatalf("unexpected project %+v", p)
	}
	multi, err := f.s.CreateProject(ctx, contracts.CreateProjectRequest{Mutation: svctest.Op(), Name: "Multi", Repos: []contracts.RepoBinding{{RepoID: "outfit"}, {RepoID: "hive", Name: "Hive", URL: "https://example.invalid/hive.git"}}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := f.s.GetProject(ctx, contracts.GetRequest{ID: multi.ProjectID})
	if err != nil || len(got.Repos) != 2 || got.Repos[0].Name != "outfit" {
		t.Fatalf("get: %v %+v", err, got)
	}
	_, err = f.s.CreateProject(ctx, contracts.CreateProjectRequest{Mutation: svctest.Op(), Name: "  "})
	mustCode(t, err, contracts.ErrInvalid)
	_, err = f.s.CreateProject(ctx, contracts.CreateProjectRequest{Mutation: svctest.Op(), Name: "dup", Repos: []contracts.RepoBinding{{RepoID: "a"}, {RepoID: "a"}}})
	mustCode(t, err, contracts.ErrInvalid)
	_, err = f.s.GetProject(ctx, contracts.GetRequest{ID: "prj_missing"})
	mustCode(t, err, contracts.ErrNotFound)
	if n := len(f.sink.Events(t, contracts.EvProjectChanged)); n != 2 {
		t.Fatalf("want 2 project.changed events, got %d", n)
	}
}

func TestUnauthenticatedMutationRejected(t *testing.T) {
	f := newFixture(t)
	_, err := f.s.CreateProject(context.Background(), contracts.CreateProjectRequest{Mutation: svctest.Op(), Name: "x"})
	mustCode(t, err, contracts.ErrUnauthorized)
}

func TestCreateProjectWithBrief(t *testing.T) {
	f := newFixture(t)
	ctx := svctest.Owner()
	p, err := f.s.CreateProject(ctx, contracts.CreateProjectRequest{Mutation: svctest.Op(), Name: "Synthetic Outfit", Brief: "# Brief\n\nGoal: X.\n"})
	if err != nil {
		t.Fatal(err)
	}
	if p.BriefDocumentID == "" {
		t.Fatal("brief document not set")
	}
	d, err := f.s.GetDocument(ctx, contracts.GetDocumentRequest{ProjectID: p.ProjectID, Slug: "brief"})
	if err != nil {
		t.Fatal(err)
	}
	if d.Document.DocumentID != p.BriefDocumentID || d.Revision.Seq != 1 || d.Revision.Content != "# Brief\n\nGoal: X.\n" {
		t.Fatalf("brief: %+v", d)
	}
	if d.Revision.Digest != ContentDigest("# Brief\n\nGoal: X.\n") || d.Revision.Size != 18 {
		t.Fatalf("digest/size: %s %d", d.Revision.Digest, d.Revision.Size)
	}
	got, _ := f.s.GetProject(ctx, contracts.GetRequest{ID: p.ProjectID})
	if got.BriefDocumentID != p.BriefDocumentID {
		t.Fatal("stored project lacks brief id")
	}
	if n := len(f.sink.Events(t, contracts.EvDocumentRevised)); n != 1 {
		t.Fatalf("want 1 document.revised, got %d", n)
	}
}

func TestIdempotentReplay(t *testing.T) {
	f := newFixture(t)
	ctx := svctest.Owner()
	req := contracts.CreateProjectRequest{Mutation: svctest.Op(), Name: "Once", Brief: "b"}
	a, err := f.s.CreateProject(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	b, err := f.s.CreateProject(ctx, req)
	if err != nil || b.ProjectID != a.ProjectID || b.BriefDocumentID != a.BriefDocumentID {
		t.Fatalf("replay: %v %+v vs %+v", err, b, a)
	}
	page, _ := f.s.ListProjects(ctx, contracts.ListProjectsRequest{})
	if len(page.Items) != 1 {
		t.Fatalf("replay created duplicates: %d", len(page.Items))
	}
	if n := len(f.sink.Events(t)); n != 2 { // project.changed + document.revised, once
		t.Fatalf("replay emitted events again: %d", n)
	}
	req.Name = "Different"
	_, err = f.s.CreateProject(ctx, req)
	mustCode(t, err, contracts.ErrConflict)
}

func TestUpdateProjectCASCheckoutsArchive(t *testing.T) {
	f := newFixture(t)
	ctx := svctest.Owner()
	p, _ := f.s.CreateProject(ctx, contracts.CreateProjectRequest{Mutation: svctest.Op(), Name: "P", Repos: []contracts.RepoBinding{{RepoID: "outfit"}}})
	cos := []contracts.CheckoutBinding{
		{NodeID: "node_synth-mac", Path: "/synthetic/outfit", RepoID: "outfit", Mode: contracts.CheckoutShared},
		{NodeID: "node_synth-linux", Path: "/synthetic/wt/a", RepoID: "outfit", Mode: contracts.CheckoutWorktree, Branch: "a"},
		{NodeID: "node_synth-linux", Path: "/synthetic/notes", Mode: contracts.CheckoutPlain},
	}
	p2, err := f.s.UpdateProject(ctx, contracts.UpdateProjectRequest{Mutation: svctest.Op(), ProjectID: p.ProjectID, Version: 1, Checkouts: &cos, Name: ptr("P2")})
	if err != nil {
		t.Fatal(err)
	}
	if p2.Version != 2 || len(p2.Checkouts) != 3 || p2.Name != "P2" {
		t.Fatalf("update: %+v", p2)
	}
	_, err = f.s.UpdateProject(ctx, contracts.UpdateProjectRequest{Mutation: svctest.Op(), ProjectID: p.ProjectID, Version: 1, Name: ptr("stale")})
	e := mustCode(t, err, contracts.ErrConflict)
	if e.Details["current_version"] != float64(2) && e.Details["current_version"] != int64(2) {
		t.Fatalf("details: %v", e.Details)
	}
	bad := []contracts.CheckoutBinding{{NodeID: "n", Path: "/x", Mode: "copy"}}
	_, err = f.s.UpdateProject(ctx, contracts.UpdateProjectRequest{Mutation: svctest.Op(), ProjectID: p.ProjectID, Version: 2, Checkouts: &bad})
	mustCode(t, err, contracts.ErrInvalid)
	unknown := []contracts.CheckoutBinding{{NodeID: "n", Path: "/x", RepoID: "nope", Mode: contracts.CheckoutPlain}}
	_, err = f.s.UpdateProject(ctx, contracts.UpdateProjectRequest{Mutation: svctest.Op(), ProjectID: p.ProjectID, Version: 2, Checkouts: &unknown})
	mustCode(t, err, contracts.ErrInvalid)
	// Removing a repo still referenced by a checkout is rejected.
	none := []contracts.RepoBinding{}
	_, err = f.s.UpdateProject(ctx, contracts.UpdateProjectRequest{Mutation: svctest.Op(), ProjectID: p.ProjectID, Version: 2, Repos: &none})
	mustCode(t, err, contracts.ErrInvalid)
	secret := []string{"token=abc"}
	_, err = f.s.UpdateProject(ctx, contracts.UpdateProjectRequest{Mutation: svctest.Op(), ProjectID: p.ProjectID, Version: 2, Connections: &secret})
	mustCode(t, err, contracts.ErrInvalid)

	yes := true
	p3, err := f.s.UpdateProject(ctx, contracts.UpdateProjectRequest{Mutation: svctest.Op(), ProjectID: p.ProjectID, Version: 2, Archived: &yes})
	if err != nil || !p3.Archived || p3.Version != 3 {
		t.Fatalf("archive: %v %+v", err, p3)
	}
	page, _ := f.s.ListProjects(ctx, contracts.ListProjectsRequest{})
	if len(page.Items) != 0 {
		t.Fatal("archived project listed by default")
	}
	page, _ = f.s.ListProjects(ctx, contracts.ListProjectsRequest{IncludeArchived: true, Query: "p2"})
	if len(page.Items) != 1 {
		t.Fatal("archived project not listed with include_archived")
	}
}

func TestListProjectsPagination(t *testing.T) {
	f := newFixture(t)
	ctx := svctest.Owner()
	for i := 0; i < 5; i++ {
		if _, err := f.s.CreateProject(ctx, contracts.CreateProjectRequest{Mutation: svctest.Op(), Name: "p"}); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]bool{}
	cursor := ""
	for pages := 0; ; pages++ {
		page, err := f.s.ListProjects(ctx, contracts.ListProjectsRequest{PageRequest: contracts.PageRequest{Limit: 2, Cursor: cursor}})
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range page.Items {
			seen[p.ProjectID] = true
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
		if pages > 5 {
			t.Fatal("pagination does not terminate")
		}
	}
	if len(seen) != 5 {
		t.Fatalf("pagination saw %d projects", len(seen))
	}
}

func TestDocumentSlugsStandaloneAndProject(t *testing.T) {
	f := newFixture(t)
	ctx := svctest.Owner()
	p, _ := f.s.CreateProject(ctx, contracts.CreateProjectRequest{Mutation: svctest.Op(), Name: "P"})
	a, err := f.s.CreateDocument(ctx, contracts.CreateDocumentRequest{Mutation: svctest.Op(), Title: "Design Notes", Content: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if a.Document.ProjectID != nil || a.Document.Slug != "design-notes" || a.Document.MediaType != "text/markdown" {
		t.Fatalf("standalone doc: %+v", a.Document)
	}
	// Same derived slug standalone -> suffixed; in a project -> independent.
	b, err := f.s.CreateDocument(ctx, contracts.CreateDocumentRequest{Mutation: svctest.Op(), Title: "Design notes", Content: "y"})
	if err != nil || b.Document.Slug != "design-notes-2" {
		t.Fatalf("derived suffix: %v %+v", err, b.Document)
	}
	c, err := f.s.CreateDocument(ctx, contracts.CreateDocumentRequest{Mutation: svctest.Op(), ProjectID: &p.ProjectID, Title: "Design Notes", Content: "z"})
	if err != nil || c.Document.Slug != "design-notes" {
		t.Fatalf("project slug: %v %+v", err, c.Document)
	}
	_, err = f.s.CreateDocument(ctx, contracts.CreateDocumentRequest{Mutation: svctest.Op(), ProjectID: &p.ProjectID, Title: "Other", Slug: "design-notes", Content: "z"})
	mustCode(t, err, contracts.ErrConflict)
	_, err = f.s.CreateDocument(ctx, contracts.CreateDocumentRequest{Mutation: svctest.Op(), Title: "Bad", Slug: "../etc", Content: "z"})
	mustCode(t, err, contracts.ErrInvalid)
	_, err = f.s.CreateDocument(ctx, contracts.CreateDocumentRequest{Mutation: svctest.Op(), ProjectID: ptr("prj_missing"), Title: "x", Content: "z"})
	mustCode(t, err, contracts.ErrNotFound)
	_, err = f.s.CreateDocument(ctx, contracts.CreateDocumentRequest{Mutation: svctest.Op(), Title: "img", MediaType: "image/png", Content: "z"})
	mustCode(t, err, contracts.ErrInvalid)

	got, err := f.s.GetDocument(ctx, contracts.GetDocumentRequest{Slug: "design-notes"})
	if err != nil || got.Document.DocumentID != a.Document.DocumentID {
		t.Fatalf("standalone by slug: %v", err)
	}
	got, err = f.s.GetDocument(ctx, contracts.GetDocumentRequest{ProjectID: p.ProjectID, Slug: "design-notes"})
	if err != nil || got.Document.DocumentID != c.Document.DocumentID {
		t.Fatalf("project by slug: %v", err)
	}
	standalone, _ := f.s.ListDocuments(ctx, contracts.ListDocumentsRequest{ProjectID: ptr("")})
	inProject, _ := f.s.ListDocuments(ctx, contracts.ListDocumentsRequest{ProjectID: &p.ProjectID})
	all, _ := f.s.ListDocuments(ctx, contracts.ListDocumentsRequest{})
	if len(standalone.Items) != 2 || len(inProject.Items) != 1 || len(all.Items) != 3 {
		t.Fatalf("list filters: %d %d %d", len(standalone.Items), len(inProject.Items), len(all.Items))
	}
}

func TestDocumentSizeLimit(t *testing.T) {
	db := svctest.OpenDB(t)
	s, err := New(context.Background(), db, svctest.NewSink(t, db), Options{MaxDocumentBytes: 10})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.CreateDocument(svctest.Owner(), contracts.CreateDocumentRequest{Mutation: svctest.Op(), Title: "big", Content: strings.Repeat("x", 11)})
	mustCode(t, err, contracts.ErrPayloadTooLarge)
}

func TestConcurrentProposeOneWinsOnePreserved(t *testing.T) {
	f := newFixture(t)
	owner := svctest.Owner()
	p, _ := f.s.CreateProject(owner, contracts.CreateProjectRequest{Mutation: svctest.Op(), Name: "Synthetic Outfit", Brief: "# Brief\n\nGoal: X.\n"})
	base, _ := f.s.GetDocument(owner, contracts.GetDocumentRequest{DocumentID: p.BriefDocumentID})
	baseRev := base.Revision.RevisionID

	contents := map[string]string{
		"ses_synth-a": "# Brief\n\nGoal: X.\nConstraint: keep API stable.\n",
		"ses_synth-b": "# Brief\n\nGoal: X and Y.\n",
	}
	type result struct {
		who string
		out contracts.DocumentWithRevision
		err error
	}
	results := make(chan result, 2)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for who, content := range contents {
		wg.Add(1)
		go func(who, content string) {
			defer wg.Done()
			<-start
			out, err := f.s.ProposeRevision(svctest.Session(who), contracts.ProposeRevisionRequest{Mutation: svctest.Op(), DocumentID: p.BriefDocumentID, BaseRevisionID: baseRev, Content: content})
			results <- result{who, out, err}
		}(who, content)
	}
	close(start)
	wg.Wait()
	close(results)
	var winner, loser result
	wins := 0
	for r := range results {
		if r.err == nil {
			wins++
			winner = r
		} else {
			loser = r
		}
	}
	if wins != 1 {
		t.Fatalf("want exactly one winner, got %d (loser err %v)", wins, loser.err)
	}
	e := mustCode(t, loser.err, contracts.ErrConflict)
	if e.Details["current_head"] != winner.out.Revision.RevisionID {
		t.Fatalf("current_head %v != winner %s", e.Details["current_head"], winner.out.Revision.RevisionID)
	}
	if e.Ref == nil || e.Ref.Kind != contracts.KindDocument {
		t.Fatalf("ref: %+v", e.Ref)
	}
	proposalID, _ := e.Details["proposal_id"].(string)
	if winner.out.Revision.Seq != 2 || winner.out.Revision.ParentRevisionID != baseRev || winner.out.Revision.Provenance == nil || winner.out.Revision.Provenance.SessionID != winner.who {
		t.Fatalf("winner revision: %+v", winner.out.Revision)
	}
	revs, _ := f.s.ListRevisions(owner, contracts.ListRevisionsRequest{DocumentID: p.BriefDocumentID})
	if len(revs.Items) != 2 {
		t.Fatalf("want 2 revisions, got %d", len(revs.Items))
	}
	for _, r := range revs.Items {
		if r.Content != "" {
			t.Fatal("history listing includes content")
		}
	}
	props, _ := f.s.ListProposals(owner, contracts.ListProposalsRequest{DocumentID: p.BriefDocumentID, States: []contracts.ProposalState{contracts.ProposalConflicted}})
	if len(props.Items) != 1 {
		t.Fatalf("want 1 conflicted proposal, got %d", len(props.Items))
	}
	prp := props.Items[0]
	if prp.ProposalID != proposalID || prp.Content != contents[loser.who] || prp.BaseRevisionID != baseRev || prp.HeadAtConflict != winner.out.Revision.RevisionID || prp.Author.ID != loser.who {
		t.Fatalf("proposal not preserved faithfully: %+v", prp)
	}
	head, _ := f.s.GetDocument(owner, contracts.GetDocumentRequest{DocumentID: p.BriefDocumentID})
	if head.Revision.Content != contents[winner.who] {
		t.Fatal("head is not the winner's content")
	}
	old, _ := f.s.GetDocument(owner, contracts.GetDocumentRequest{DocumentID: p.BriefDocumentID, RevisionID: baseRev})
	if old.Revision.Content != "# Brief\n\nGoal: X.\n" {
		t.Fatal("old revision changed")
	}
	if n := len(f.sink.Events(t, contracts.EvDocumentConflict)); n != 1 {
		t.Fatalf("want 1 document.conflict event, got %d", n)
	}
	if n := len(f.sink.Events(t, contracts.EvDocumentRevised)); n != 2 {
		t.Fatalf("want 2 document.revised events, got %d", n)
	}
}

func TestProposeConflictReplayAndApply(t *testing.T) {
	f := newFixture(t)
	ctx := svctest.Owner()
	d, _ := f.s.CreateDocument(ctx, contracts.CreateDocumentRequest{Mutation: svctest.Op(), Title: "Plan", Content: "v1"})
	r1 := d.Revision.RevisionID
	if _, err := f.s.ProposeRevision(ctx, contracts.ProposeRevisionRequest{Mutation: svctest.Op(), DocumentID: d.Document.DocumentID, BaseRevisionID: r1, Content: "v2"}); err != nil {
		t.Fatal(err)
	}
	stale := contracts.ProposeRevisionRequest{Mutation: svctest.Op(), DocumentID: d.Document.DocumentID, BaseRevisionID: r1, Content: "mine"}
	_, err := f.s.ProposeRevision(ctx, stale)
	e1 := mustCode(t, err, contracts.ErrConflict)
	_, err = f.s.ProposeRevision(ctx, stale) // retry of the same operation
	e2 := mustCode(t, err, contracts.ErrConflict)
	if e1.Details["proposal_id"] != e2.Details["proposal_id"] {
		t.Fatalf("replay created a different proposal: %v vs %v", e1.Details, e2.Details)
	}
	props, _ := f.s.ListProposals(ctx, contracts.ListProposalsRequest{DocumentID: d.Document.DocumentID})
	if len(props.Items) != 1 {
		t.Fatalf("replay duplicated proposal: %d", len(props.Items))
	}
	// Resolve by re-proposing the preserved content on the current head.
	head, _ := f.s.GetDocument(ctx, contracts.GetDocumentRequest{DocumentID: d.Document.DocumentID})
	if _, err := f.s.ProposeRevision(ctx, contracts.ProposeRevisionRequest{Mutation: svctest.Op(), DocumentID: d.Document.DocumentID, BaseRevisionID: head.Revision.RevisionID, Content: props.Items[0].Content}); err != nil {
		t.Fatal(err)
	}
	props, _ = f.s.ListProposals(ctx, contracts.ListProposalsRequest{DocumentID: d.Document.DocumentID})
	if props.Items[0].State != contracts.ProposalApplied {
		t.Fatalf("proposal state %s", props.Items[0].State)
	}
	_, err = f.s.ProposeRevision(ctx, contracts.ProposeRevisionRequest{Mutation: svctest.Op(), DocumentID: d.Document.DocumentID, BaseRevisionID: "rev_other", Content: "x"})
	mustCode(t, err, contracts.ErrInvalid)
	_, err = f.s.ProposeRevision(ctx, contracts.ProposeRevisionRequest{Mutation: svctest.Op(), DocumentID: "doc_missing", BaseRevisionID: r1, Content: "x"})
	mustCode(t, err, contracts.ErrNotFound)
}

func TestRevisionsImmutable(t *testing.T) {
	f := newFixture(t)
	ctx := svctest.Owner()
	d, _ := f.s.CreateDocument(ctx, contracts.CreateDocumentRequest{Mutation: svctest.Op(), Title: "Plan", Content: "v1"})
	if _, err := f.s.db.Exec(`UPDATE document_revisions SET content='tampered' WHERE revision_id=?`, d.Revision.RevisionID); err == nil {
		t.Fatal("revision update was allowed")
	}
	if _, err := f.s.db.Exec(`DELETE FROM document_revisions WHERE revision_id=?`, d.Revision.RevisionID); err == nil {
		t.Fatal("revision delete was allowed")
	}
	got, _ := f.s.GetDocument(ctx, contracts.GetDocumentRequest{DocumentID: d.Document.DocumentID, RevisionID: d.Revision.RevisionID})
	if got.Revision.Content != "v1" {
		t.Fatal("revision content changed")
	}
}

func TestRevisionPagination(t *testing.T) {
	f := newFixture(t)
	ctx := svctest.Owner()
	d, _ := f.s.CreateDocument(ctx, contracts.CreateDocumentRequest{Mutation: svctest.Op(), Title: "Plan", Content: "0"})
	head := d.Revision.RevisionID
	for i := 1; i <= 4; i++ {
		out, err := f.s.ProposeRevision(ctx, contracts.ProposeRevisionRequest{Mutation: svctest.Op(), DocumentID: d.Document.DocumentID, BaseRevisionID: head, Content: strings.Repeat("x", i)})
		if err != nil {
			t.Fatal(err)
		}
		head = out.Revision.RevisionID
	}
	p1, _ := f.s.ListRevisions(ctx, contracts.ListRevisionsRequest{DocumentID: d.Document.DocumentID, PageRequest: contracts.PageRequest{Limit: 3}})
	p2, _ := f.s.ListRevisions(ctx, contracts.ListRevisionsRequest{DocumentID: d.Document.DocumentID, PageRequest: contracts.PageRequest{Limit: 3, Cursor: p1.NextCursor}})
	if len(p1.Items) != 3 || p1.Items[0].Seq != 5 || len(p2.Items) != 2 || p2.Items[1].Seq != 1 || p2.NextCursor != "" {
		t.Fatalf("pages: %d/%d next=%q", len(p1.Items), len(p2.Items), p2.NextCursor)
	}
}

func TestNotes(t *testing.T) {
	f := newFixture(t)
	owner := svctest.Owner()
	p, _ := f.s.CreateProject(owner, contracts.CreateProjectRequest{Mutation: svctest.Op(), Name: "P"})
	sess := svctest.Session("ses_synth-a")
	n, err := f.s.CreateNote(sess, contracts.CreateNoteRequest{Mutation: svctest.Op(), Kind: contracts.NoteHandoff, ProjectID: &p.ProjectID, Title: "Handoff: parser done", Body: "Next: wire CLI."})
	if err != nil {
		t.Fatal(err)
	}
	if n.SessionID != "ses_synth-a" || n.Provenance.SessionID != "ses_synth-a" || *n.Provenance.ProjectID != p.ProjectID || n.SyncState != SyncPending {
		t.Fatalf("note provenance: %+v", n)
	}
	_, err = f.s.CreateNote(sess, contracts.CreateNoteRequest{Mutation: svctest.Op(), Kind: contracts.NoteNote, SessionID: "ses_synth-other", Title: "spoof"})
	mustCode(t, err, contracts.ErrForbidden)
	_, err = f.s.CreateNote(owner, contracts.CreateNoteRequest{Mutation: svctest.Op(), Kind: "memo", Title: "x"})
	mustCode(t, err, contracts.ErrInvalid)
	standalone, err := f.s.CreateNote(owner, contracts.CreateNoteRequest{Mutation: svctest.Op(), Kind: contracts.NoteDecision, Title: "Use SQLite 100% of the time", Body: "because"})
	if err != nil || standalone.ProjectID != nil {
		t.Fatalf("standalone note: %v %+v", err, standalone)
	}
	q, _ := f.s.ListNotes(owner, contracts.ListNotesRequest{Query: "100%"})
	if len(q.Items) != 1 || q.Items[0].NoteID != standalone.NoteID {
		t.Fatalf("query: %+v", q.Items)
	}
	q, _ = f.s.ListNotes(owner, contracts.ListNotesRequest{ProjectID: ptr("")})
	if len(q.Items) != 1 {
		t.Fatalf("standalone filter: %d", len(q.Items))
	}
	q, _ = f.s.ListNotes(owner, contracts.ListNotesRequest{SessionID: "ses_synth-a", Kind: contracts.NoteHandoff})
	if len(q.Items) != 1 {
		t.Fatalf("session filter: %d", len(q.Items))
	}
	synced, err := f.s.UpdateNoteSync(svctest.Session("ses_synth-bridge"), contracts.UpdateNoteSyncRequest{Mutation: svctest.Op(), NoteID: n.NoteID, HiveRef: "hive:item-1", SyncState: SyncIndexed})
	if err != nil || synced.HiveRef != "hive:item-1" || synced.SyncState != SyncIndexed {
		t.Fatalf("sync: %v %+v", err, synced)
	}
	_, err = f.s.UpdateNoteSync(owner, contracts.UpdateNoteSyncRequest{Mutation: svctest.Op(), NoteID: n.NoteID, SyncState: "done"})
	mustCode(t, err, contracts.ErrInvalid)
	evs := f.sink.Events(t, contracts.EvNoteCreated)
	if len(evs) != 2 || evs[0].SessionID != "ses_synth-a" || evs[0].ProjectID == nil {
		t.Fatalf("note events: %+v", evs)
	}
}

type fakeArtifacts struct{ items []contracts.Artifact }

func (f fakeArtifacts) ListArtifacts(ctx context.Context, req contracts.ListArtifactsRequest) (contracts.Page[contracts.Artifact], error) {
	return contracts.Page[contracts.Artifact]{Items: f.items}, nil
}

func TestLaunchContextBounded(t *testing.T) {
	f := newFixture(t)
	owner := svctest.Owner()
	brief := "# Brief\n\n" + strings.Repeat("Goal line with détails.\n", 2000) // ~48 KiB
	p, _ := f.s.CreateProject(owner, contracts.CreateProjectRequest{Mutation: svctest.Op(), Name: "Synthetic Outfit", Description: "Shared config.\nSecond line.", Brief: brief, Repos: []contracts.RepoBinding{{RepoID: "outfit"}}})
	for i := 0; i < 20; i++ {
		f.s.CreateNote(owner, contracts.CreateNoteRequest{Mutation: svctest.Op(), Kind: contracts.NoteDecision, ProjectID: &p.ProjectID, Title: "Decision " + strings.Repeat("d", 50), Body: "SECRET-TRANSCRIPT-BODY"})
		f.s.CreateNote(owner, contracts.CreateNoteRequest{Mutation: svctest.Op(), Kind: contracts.NoteHandoff, ProjectID: &p.ProjectID, Title: "Handoff", Body: "SECRET-TRANSCRIPT-BODY"})
	}
	f.s.CreateDocument(owner, contracts.CreateDocumentRequest{Mutation: svctest.Op(), ProjectID: &p.ProjectID, Title: "Decisions log", Content: "SECRET-DOC-BODY"})
	f.s.SetArtifactIndex(fakeArtifacts{items: []contracts.Artifact{{ArtifactID: "art_synth-report", Title: "Report", LatestVersionID: "ver_synth-report-2", VersionCount: 2}}})

	lc, err := f.s.GetLaunchContext(owner, contracts.ContextRequest{ProjectID: &p.ProjectID})
	if err != nil {
		t.Fatal(err)
	}
	if lc.Bytes != len(lc.Text) || lc.Bytes > DefaultContextBytes {
		t.Fatalf("context not bounded: %d bytes", lc.Bytes)
	}
	if !lc.Truncated || !strings.Contains(lc.Text, "[brief truncated") || !strings.Contains(lc.Text, "[context truncated") {
		t.Fatalf("truncation not marked:\n%s", lc.Text)
	}
	head, _ := f.s.GetDocument(owner, contracts.GetDocumentRequest{DocumentID: p.BriefDocumentID})
	if lc.BriefRevision != head.Revision.RevisionID {
		t.Fatalf("brief revision %q", lc.BriefRevision)
	}
	if strings.Contains(lc.Text, "SECRET") || strings.Contains(lc.Text, "Second line") {
		t.Fatal("context includes bodies/transcripts")
	}
	refKinds := map[contracts.ObjectKind]int{}
	for _, r := range lc.References {
		refKinds[r.Kind]++
	}
	if refKinds[contracts.KindProject] != 1 || refKinds[contracts.KindDocumentRevision] != 1 || refKinds[contracts.KindNote] == 0 {
		t.Fatalf("references: %+v", refKinds)
	}

	// A generous budget includes documents and artifact pointers.
	big, err := f.s.GetLaunchContext(owner, contracts.ContextRequest{ProjectID: &p.ProjectID, MaxBytes: 64 << 10})
	if err != nil {
		t.Fatal(err)
	}
	if big.Bytes > 64<<10 || !strings.Contains(big.Text, "pyre://artifact_version/ver_synth-report-2") || !strings.Contains(big.Text, "Decisions log") {
		t.Fatalf("big context:\n%s", big.Text)
	}
	tiny, err := f.s.GetLaunchContext(owner, contracts.ContextRequest{ProjectID: &p.ProjectID, MaxBytes: 1})
	if err != nil || tiny.Bytes > MinContextBytes || !tiny.Truncated {
		t.Fatalf("tiny context: %v %d %v", err, tiny.Bytes, tiny.Truncated)
	}
}

func TestLaunchContextSessionOnlyAndEmpty(t *testing.T) {
	f := newFixture(t)
	sess := svctest.Session("ses_synth-solo")
	f.s.CreateNote(sess, contracts.CreateNoteRequest{Mutation: svctest.Op(), Kind: contracts.NoteHandoff, Title: "Solo handoff"})
	lc, err := f.s.GetLaunchContext(sess, contracts.ContextRequest{SessionID: "ses_synth-solo"})
	if err != nil || !strings.Contains(lc.Text, "Solo handoff") || lc.Truncated || len(lc.References) != 1 {
		t.Fatalf("session context: %v %+v", err, lc)
	}
	empty, err := f.s.GetLaunchContext(sess, contracts.ContextRequest{})
	if err != nil || empty.Text != "" || empty.References == nil {
		t.Fatalf("empty: %v %+v", err, empty)
	}
	_, err = f.s.GetLaunchContext(sess, contracts.ContextRequest{ProjectID: ptr("prj_missing")})
	mustCode(t, err, contracts.ErrNotFound)
}

func TestLookups(t *testing.T) {
	f := newFixture(t)
	ctx := svctest.Owner()
	p, _ := f.s.CreateProject(ctx, contracts.CreateProjectRequest{Mutation: svctest.Op(), Name: "P"})
	if err := f.s.ProjectExists(ctx, p.ProjectID); err != nil {
		t.Fatal(err)
	}
	mustCode(t, f.s.ProjectExists(ctx, "prj_nope"), contracts.ErrNotFound)
	d, _ := f.s.CreateDocument(ctx, contracts.CreateDocumentRequest{Mutation: svctest.Op(), ProjectID: &p.ProjectID, Title: "D", Content: "c"})
	docID, proj, err := f.s.LookupRevision(ctx, d.Revision.RevisionID)
	if err != nil || docID != d.Document.DocumentID || proj == nil || *proj != p.ProjectID {
		t.Fatalf("lookup: %v %s %v", err, docID, proj)
	}
	_, _, err = f.s.LookupRevision(ctx, "rev_nope")
	mustCode(t, err, contracts.ErrNotFound)
}

func TestDiscardProposal(t *testing.T) {
	f := newFixture(t)
	ctx := svctest.Owner()
	d, _ := f.s.CreateDocument(ctx, contracts.CreateDocumentRequest{Mutation: svctest.Op(), Title: "Plan", Content: "v1"})
	r1 := d.Revision.RevisionID
	f.s.ProposeRevision(ctx, contracts.ProposeRevisionRequest{Mutation: svctest.Op(), DocumentID: d.Document.DocumentID, BaseRevisionID: r1, Content: "v2"})
	_, err := f.s.ProposeRevision(ctx, contracts.ProposeRevisionRequest{Mutation: svctest.Op(), DocumentID: d.Document.DocumentID, BaseRevisionID: r1, Content: "loser"})
	prpID := mustCode(t, err, contracts.ErrConflict).Details["proposal_id"].(string)
	before := len(f.sink.Events(t))
	req := contracts.DiscardProposalRequest{Mutation: svctest.Op(), ProposalID: prpID}
	p, err := f.s.DiscardProposal(ctx, req)
	if err != nil || p.State != contracts.ProposalDiscarded || p.Content != "loser" {
		t.Fatalf("discard: %v %+v", err, p)
	}
	if p2, err := f.s.DiscardProposal(ctx, req); err != nil || p2.State != contracts.ProposalDiscarded {
		t.Fatalf("replay: %v", err)
	}
	if p3, err := f.s.DiscardProposal(ctx, contracts.DiscardProposalRequest{Mutation: svctest.Op(), ProposalID: prpID}); err != nil || p3.State != contracts.ProposalDiscarded {
		t.Fatalf("discard twice: %v", err)
	}
	if n := len(f.sink.Events(t)); n != before {
		t.Fatalf("discard emitted %d events", n-before)
	}
	conflicted, _ := f.s.ListProposals(ctx, contracts.ListProposalsRequest{DocumentID: d.Document.DocumentID, States: []contracts.ProposalState{contracts.ProposalConflicted}})
	if len(conflicted.Items) != 0 {
		t.Fatal("discarded proposal still conflicted")
	}
	// An applied proposal cannot be discarded.
	head, _ := f.s.GetDocument(ctx, contracts.GetDocumentRequest{DocumentID: d.Document.DocumentID})
	_, err = f.s.ProposeRevision(ctx, contracts.ProposeRevisionRequest{Mutation: svctest.Op(), DocumentID: d.Document.DocumentID, BaseRevisionID: r1, Content: "again"})
	applyID := mustCode(t, err, contracts.ErrConflict).Details["proposal_id"].(string)
	f.s.ProposeRevision(ctx, contracts.ProposeRevisionRequest{Mutation: svctest.Op(), DocumentID: d.Document.DocumentID, BaseRevisionID: head.Revision.RevisionID, Content: "again"})
	_, err = f.s.DiscardProposal(ctx, contracts.DiscardProposalRequest{Mutation: svctest.Op(), ProposalID: applyID})
	mustCode(t, err, contracts.ErrConflict)
	_, err = f.s.DiscardProposal(ctx, contracts.DiscardProposalRequest{Mutation: svctest.Op(), ProposalID: "prp_missing"})
	mustCode(t, err, contracts.ErrNotFound)
}

func TestPromoteNote(t *testing.T) {
	f := newFixture(t)
	owner := svctest.Owner()
	p, _ := f.s.CreateProject(owner, contracts.CreateProjectRequest{Mutation: svctest.Op(), Name: "P", Brief: "# Brief\n"})
	sess := svctest.Session("ses_synth-a")
	n, _ := f.s.CreateNote(sess, contracts.CreateNoteRequest{Mutation: svctest.Op(), Kind: contracts.NoteDecision, ProjectID: &p.ProjectID, Title: "Use SQLite", Body: "Decision: SQLite for the hub."})

	// New document; defaults to the note's project and title.
	req := contracts.PromoteNoteRequest{Mutation: svctest.Op(), NoteID: n.NoteID}
	dwr, err := f.s.PromoteNote(owner, req)
	if err != nil {
		t.Fatal(err)
	}
	if dwr.Document.ProjectID == nil || *dwr.Document.ProjectID != p.ProjectID || dwr.Document.Slug != "use-sqlite" || dwr.Revision.Content != n.Body ||
		dwr.Revision.Author.Kind != contracts.PrincipalOwner || dwr.Revision.Provenance == nil || dwr.Revision.Provenance.SessionID != "ses_synth-a" {
		t.Fatalf("promotion: %+v", dwr)
	}
	got, _ := f.s.GetNote(owner, contracts.GetRequest{ID: n.NoteID})
	if got.PromotedTo != dwr.Revision.RevisionID {
		t.Fatalf("promoted_to %q", got.PromotedTo)
	}
	if again, err := f.s.PromoteNote(owner, req); err != nil || again.Revision.RevisionID != dwr.Revision.RevisionID {
		t.Fatalf("replay: %v", err)
	}
	_, err = f.s.PromoteNote(owner, contracts.PromoteNoteRequest{Mutation: svctest.Op(), NoteID: n.NoteID})
	if e := mustCode(t, err, contracts.ErrConflict); e.Details["promoted_to"] != dwr.Revision.RevisionID {
		t.Fatalf("second promotion: %+v", e)
	}

	// Into an existing document with CAS: stale base preserves a proposal and leaves the note unpromoted.
	h1, _ := f.s.CreateNote(owner, contracts.CreateNoteRequest{Mutation: svctest.Op(), Kind: contracts.NoteHandoff, ProjectID: &p.ProjectID, Title: "Handoff", Body: "Brief addition."})
	brief, _ := f.s.GetDocument(owner, contracts.GetDocumentRequest{DocumentID: p.BriefDocumentID})
	f.s.ProposeRevision(owner, contracts.ProposeRevisionRequest{Mutation: svctest.Op(), DocumentID: p.BriefDocumentID, BaseRevisionID: brief.Revision.RevisionID, Content: "# Brief v2\n"})
	_, err = f.s.PromoteNote(owner, contracts.PromoteNoteRequest{Mutation: svctest.Op(), NoteID: h1.NoteID, DocumentID: p.BriefDocumentID, BaseRevisionID: brief.Revision.RevisionID})
	e := mustCode(t, err, contracts.ErrConflict)
	if _, ok := e.Details["proposal_id"]; !ok {
		t.Fatalf("conflict details %v", e.Details)
	}
	if got, _ := f.s.GetNote(owner, contracts.GetRequest{ID: h1.NoteID}); got.PromotedTo != "" {
		t.Fatal("note marked promoted on conflict")
	}
	head, _ := f.s.GetDocument(owner, contracts.GetDocumentRequest{DocumentID: p.BriefDocumentID})
	ok, err := f.s.PromoteNote(owner, contracts.PromoteNoteRequest{Mutation: svctest.Op(), NoteID: h1.NoteID, DocumentID: p.BriefDocumentID, BaseRevisionID: head.Revision.RevisionID})
	if err != nil || ok.Revision.Seq != 3 || ok.Revision.Content != "Brief addition." {
		t.Fatalf("promote into doc: %v %+v", err, ok.Revision)
	}
	props, _ := f.s.ListProposals(owner, contracts.ListProposalsRequest{DocumentID: p.BriefDocumentID})
	if len(props.Items) != 1 || props.Items[0].State != contracts.ProposalApplied {
		t.Fatalf("proposal from failed promotion not applied: %+v", props.Items)
	}

	// Validation.
	h2, _ := f.s.CreateNote(owner, contracts.CreateNoteRequest{Mutation: svctest.Op(), Kind: contracts.NoteNote, Title: "Standalone", Body: ""})
	_, err = f.s.PromoteNote(owner, contracts.PromoteNoteRequest{Mutation: svctest.Op(), NoteID: h2.NoteID, DocumentID: p.BriefDocumentID})
	mustCode(t, err, contracts.ErrInvalid)
	_, err = f.s.PromoteNote(owner, contracts.PromoteNoteRequest{Mutation: svctest.Op(), NoteID: h2.NoteID, DocumentID: p.BriefDocumentID, BaseRevisionID: head.Revision.RevisionID, Title: "x"})
	mustCode(t, err, contracts.ErrInvalid)
	_, err = f.s.PromoteNote(owner, contracts.PromoteNoteRequest{Mutation: svctest.Op(), NoteID: "note_missing"})
	mustCode(t, err, contracts.ErrNotFound)
	sd, err := f.s.PromoteNote(owner, contracts.PromoteNoteRequest{Mutation: svctest.Op(), NoteID: h2.NoteID, Slug: "standalone-note"})
	if err != nil || sd.Document.ProjectID != nil || sd.Revision.Content != "# Standalone\n" {
		t.Fatalf("standalone promotion: %v %+v", err, sd)
	}
	// Promotion emits no extra note events (no pinned note-update kind).
	if n := len(f.sink.Events(t, contracts.EvNoteCreated)); n != 3 {
		t.Fatalf("note events %d", n)
	}
}

func TestEventPayloadShapes(t *testing.T) {
	f := newFixture(t)
	ctx := svctest.Owner()
	p, _ := f.s.CreateProject(ctx, contracts.CreateProjectRequest{Mutation: svctest.Op(), Name: "P", Brief: "SECRET-BRIEF"})
	base, _ := f.s.GetDocument(ctx, contracts.GetDocumentRequest{DocumentID: p.BriefDocumentID})
	f.s.ProposeRevision(ctx, contracts.ProposeRevisionRequest{Mutation: svctest.Op(), DocumentID: p.BriefDocumentID, BaseRevisionID: base.Revision.RevisionID, Content: "SECRET-2"})
	f.s.ProposeRevision(ctx, contracts.ProposeRevisionRequest{Mutation: svctest.Op(), DocumentID: p.BriefDocumentID, BaseRevisionID: base.Revision.RevisionID, Content: "SECRET-3"})
	f.s.CreateNote(ctx, contracts.CreateNoteRequest{Mutation: svctest.Op(), Kind: contracts.NoteDecision, ProjectID: &p.ProjectID, Title: "D", Body: "SECRET-BODY"})
	for _, ev := range f.sink.Events(t) {
		if strings.Contains(string(ev.Payload), "SECRET") {
			t.Fatalf("%s payload carries content: %s", ev.Kind, ev.Payload)
		}
		switch ev.Kind {
		case contracts.EvProjectChanged:
			var v contracts.Project
			if json.Unmarshal(ev.Payload, &v) != nil || v.ProjectID != p.ProjectID || v.BriefDocumentID != p.BriefDocumentID {
				t.Fatalf("project payload %s", ev.Payload)
			}
		case contracts.EvDocumentRevised:
			var v contracts.DocumentRevision
			if json.Unmarshal(ev.Payload, &v) != nil || v.RevisionID == "" || v.DocumentID != p.BriefDocumentID || v.Digest == "" {
				t.Fatalf("revision payload %s", ev.Payload)
			}
		case contracts.EvDocumentConflict:
			var v contracts.DocumentProposal
			if json.Unmarshal(ev.Payload, &v) != nil || v.ProposalID == "" || v.State != contracts.ProposalConflicted || v.HeadAtConflict == "" {
				t.Fatalf("proposal payload %s", ev.Payload)
			}
		case contracts.EvNoteCreated:
			var v contracts.Note
			if json.Unmarshal(ev.Payload, &v) != nil || v.NoteID == "" || v.Title != "D" {
				t.Fatalf("note payload %s", ev.Payload)
			}
		default:
			t.Fatalf("unexpected event %s", ev.Kind)
		}
	}
}

func TestSlugify(t *testing.T) {
	for in, want := range map[string]string{"Design Notes!": "design-notes", "  ": "doc", "Ünïcode ok": "n-code-ok", strings.Repeat("a", 80): strings.Repeat("a", 64)} {
		if got := Slugify(in); got != want {
			t.Errorf("Slugify(%q)=%q want %q", in, got, want)
		}
	}
}

func TestListNotesSyncStates(t *testing.T) {
	f := newFixture(t)
	owner := svctest.Owner()
	mk := func(title string) contracts.Note {
		n, err := f.s.CreateNote(owner, contracts.CreateNoteRequest{Mutation: svctest.Op(), Kind: contracts.NoteNote, Title: title})
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	pending, indexed, failed, legacy := mk("pending"), mk("indexed"), mk("failed"), mk("legacy")
	for id, st := range map[string]string{indexed.NoteID: SyncIndexed, failed.NoteID: SyncFailed} {
		if _, err := f.s.UpdateNoteSync(owner, contracts.UpdateNoteSyncRequest{Mutation: svctest.Op(), NoteID: id, SyncState: st}); err != nil {
			t.Fatal(err)
		}
	}
	// A row with an empty sync_state counts as pending.
	if _, err := f.s.db.Exec(`UPDATE notes SET sync_state='' WHERE note_id=?`, legacy.NoteID); err != nil {
		t.Fatal(err)
	}
	ids := func(states ...string) map[string]bool {
		t.Helper()
		page, err := f.s.ListNotes(owner, contracts.ListNotesRequest{SyncStates: states})
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]bool{}
		for _, n := range page.Items {
			out[n.NoteID] = true
		}
		return out
	}
	if got := ids(SyncPending); len(got) != 2 || !got[pending.NoteID] || !got[legacy.NoteID] {
		t.Fatalf("pending: %v", got)
	}
	if got := ids(SyncFailed, SyncIndexed); len(got) != 2 || !got[failed.NoteID] || !got[indexed.NoteID] {
		t.Fatalf("failed+indexed: %v", got)
	}
	if got := ids(); len(got) != 4 {
		t.Fatalf("no filter: %v", got)
	}
}
