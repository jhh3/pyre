package projects

import (
	"context"
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/projects/svcutil/svctest"
)

// Only the owner and the hub's approved-fact principal write a project's
// fact sheet, on every route: an explicit slug, a slug derived from the
// title, a revision, and a note promotion.
func TestFactSheetWritersOnEveryRoute(t *testing.T) {
	f := newFixture(t)
	owner := svctest.Owner()
	sess := svctest.Session("ses_synth-agent")
	hub := contracts.WithPrincipal(context.Background(), contracts.FactSheetPrincipal)
	p, _ := f.s.CreateProject(owner, contracts.CreateProjectRequest{Mutation: svctest.Op(), Name: "P"})
	pid := &p.ProjectID

	_, err := f.s.CreateDocument(sess, contracts.CreateDocumentRequest{Mutation: svctest.Op(), ProjectID: pid, Title: "x", Slug: "facts", Content: "- made up"})
	mustCode(t, err, contracts.ErrForbidden)
	_, err = f.s.CreateDocument(sess, contracts.CreateDocumentRequest{Mutation: svctest.Op(), ProjectID: pid, Title: "Facts", Content: "- made up"})
	mustCode(t, err, contracts.ErrForbidden) // slug derived from the title
	n, _ := f.s.CreateNote(sess, contracts.CreateNoteRequest{Mutation: svctest.Op(), Kind: contracts.NoteDecision, ProjectID: pid, Title: "Facts", Body: "- made up"})
	_, err = f.s.PromoteNote(sess, contracts.PromoteNoteRequest{Mutation: svctest.Op(), NoteID: n.NoteID})
	mustCode(t, err, contracts.ErrForbidden)
	_, err = f.s.PromoteNote(sess, contracts.PromoteNoteRequest{Mutation: svctest.Op(), NoteID: n.NoteID, Slug: "facts"})
	mustCode(t, err, contracts.ErrForbidden)

	sheet, err := f.s.CreateDocument(owner, contracts.CreateDocumentRequest{Mutation: svctest.Op(), ProjectID: pid, Title: contracts.FactSheetTitle, Slug: "facts", Content: "- a fact\n"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.s.ProposeRevision(sess, contracts.ProposeRevisionRequest{Mutation: svctest.Op(), DocumentID: sheet.Document.DocumentID, BaseRevisionID: sheet.Revision.RevisionID, Content: "- made up\n"})
	mustCode(t, err, contracts.ErrForbidden)
	_, err = f.s.PromoteNote(sess, contracts.PromoteNoteRequest{Mutation: svctest.Op(), NoteID: n.NoteID, DocumentID: sheet.Document.DocumentID, BaseRevisionID: sheet.Revision.RevisionID})
	mustCode(t, err, contracts.ErrForbidden)

	next, err := f.s.ProposeRevision(hub, contracts.ProposeRevisionRequest{Mutation: svctest.Op(), DocumentID: sheet.Document.DocumentID, BaseRevisionID: sheet.Revision.RevisionID, Content: "- a fact\n- approved\n"})
	if err != nil || next.Revision.Author != contracts.FactSheetPrincipal {
		t.Fatalf("approved fact: %v %+v", err, next.Revision.Author)
	}
	if _, err := f.s.CreateDocument(sess, contracts.CreateDocumentRequest{Mutation: svctest.Op(), ProjectID: pid, Title: "Notes", Content: "ok"}); err != nil {
		t.Fatalf("other documents stay open to sessions: %v", err)
	}
}
