package hub

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/jhh3/pyre/internal/contracts"
)

// Project pages (W-web-inbox, contracts/webinbox.go): the owner-facing page
// of a project is its document with slug contracts.ProjectPageSlug. The
// owner or a top-level session in the project writes it directly: the
// first write creates the document, each later write is a new head
// revision (compare-and-swap on BaseRevisionID when given; otherwise on
// the head just read, retried once if another write won the race).
//
// Clients refresh on the documents domain's document.revised event, which
// carries the project ID (Event.ProjectID) and the document ref; no
// separate page event is emitted.

var _ contracts.ProjectPageAPI = (*Hub)(nil)

// pageProject resolves the request's project: explicit, else the calling
// session's.
func (h *Hub) pageProject(ctx context.Context, p contracts.Principal, projectID string) (string, *contracts.Session, error) {
	projectID = strings.TrimSpace(projectID)
	var s *contracts.Session
	if p.Kind == contracts.PrincipalSession {
		got, err := h.getSession(ctx, h.db, p.ID)
		if err != nil {
			return "", nil, err
		}
		s = &got
		if projectID == "" && got.ProjectID != nil {
			projectID = *got.ProjectID
		}
	}
	if projectID == "" {
		return "", s, contracts.Errorf(contracts.ErrInvalid, "project_id is required (this caller is not in a project)")
	}
	if h.cfg.Projects != nil {
		pr, err := h.cfg.Projects.GetProject(ctx, contracts.GetRequest{ID: projectID})
		if err != nil {
			return "", s, err
		}
		projectID = pr.ProjectID
	}
	return projectID, s, nil
}

// SetProjectPage implements projects.set_page.
func (h *Hub) SetProjectPage(ctx context.Context, req contracts.SetProjectPageRequest) (contracts.ProjectPage, error) {
	p, err := principal(ctx)
	if err != nil {
		return contracts.ProjectPage{}, err
	}
	if err := requireOp(req.Mutation); err != nil {
		return contracts.ProjectPage{}, err
	}
	if h.cfg.Documents == nil {
		return contracts.ProjectPage{}, notConfigured("documents")
	}
	if strings.TrimSpace(req.Content) == "" {
		return contracts.ProjectPage{}, contracts.Errorf(contracts.ErrInvalid, "content is required: the page in markdown (goal, current status, results with links, open decisions, what's next)")
	}
	if len(req.Content) > contracts.ProjectPageMaxSize {
		return contracts.ProjectPage{}, contracts.Errorf(contracts.ErrPayloadTooLarge, "the page is %d bytes; limit %d. Link to longer material (pyre publish) instead of inlining it", len(req.Content), contracts.ProjectPageMaxSize).
			WithDetail("max_bytes", contracts.ProjectPageMaxSize)
	}
	if !utf8.ValidString(req.Content) {
		return contracts.ProjectPage{}, contracts.Errorf(contracts.ErrInvalid, "content must be UTF-8 text")
	}
	projectID, s, err := h.pageProject(ctx, p, req.ProjectID)
	if err != nil {
		return contracts.ProjectPage{}, err
	}
	switch {
	case isOwner(p):
	case s != nil && s.Parent() == "" && s.ProjectID != nil && *s.ProjectID == projectID:
	case s != nil && s.Parent() != "":
		return contracts.ProjectPage{}, contracts.Errorf(contracts.ErrForbidden, "only the project's lead (a top-level session in it) or the owner writes the project page; report to your parent session instead")
	default:
		return contracts.ProjectPage{}, contracts.Errorf(contracts.ErrForbidden, "only the owner or a top-level session in project %s writes its page", projectID).WithRef(contracts.KindProject, projectID)
	}
	docs := h.cfg.Documents
	summary := strings.TrimSpace(req.Summary)
	cur, err := docs.GetDocument(ctx, contracts.GetDocumentRequest{ProjectID: projectID, Slug: contracts.ProjectPageSlug})
	switch {
	case contracts.IsCode(err, contracts.ErrNotFound):
		if req.BaseRevisionID != "" {
			return contracts.ProjectPage{}, contracts.Errorf(contracts.ErrConflict, "the project has no page yet, so base_revision_id %s cannot be its head; write without it to create the page", req.BaseRevisionID).
				WithDetail("current_head", "")
		}
		pid := projectID
		dr, err := docs.CreateDocument(ctx, contracts.CreateDocumentRequest{Mutation: contracts.Mutation{OperationID: req.OperationID + ":page-create"},
			ProjectID: &pid, Title: "Project page", Slug: contracts.ProjectPageSlug, Content: req.Content, Summary: summary})
		if err == nil {
			return pageOf(projectID, dr), nil
		}
		if !contracts.IsCode(err, contracts.ErrConflict) {
			return contracts.ProjectPage{}, err
		}
		// Created concurrently: write a revision on top of it.
		if cur, err = docs.GetDocument(ctx, contracts.GetDocumentRequest{ProjectID: projectID, Slug: contracts.ProjectPageSlug}); err != nil {
			return contracts.ProjectPage{}, err
		}
	case err != nil:
		return contracts.ProjectPage{}, err
	}
	base := strings.TrimSpace(req.BaseRevisionID)
	cas := base != ""
	if !cas {
		base = cur.Document.HeadRevisionID
	}
	dr, err := docs.ProposeRevision(ctx, contracts.ProposeRevisionRequest{Mutation: contracts.Mutation{OperationID: req.OperationID + ":page-rev"},
		DocumentID: cur.Document.DocumentID, BaseRevisionID: base, Content: req.Content, Summary: summary})
	if contracts.IsCode(err, contracts.ErrConflict) && !cas {
		// Another write landed between our read and write: last writer wins.
		if cur, err = docs.GetDocument(ctx, contracts.GetDocumentRequest{DocumentID: cur.Document.DocumentID}); err != nil {
			return contracts.ProjectPage{}, err
		}
		dr, err = docs.ProposeRevision(ctx, contracts.ProposeRevisionRequest{Mutation: contracts.Mutation{OperationID: req.OperationID + ":page-rev2"},
			DocumentID: cur.Document.DocumentID, BaseRevisionID: cur.Document.HeadRevisionID, Content: req.Content, Summary: summary})
	}
	if err != nil {
		return contracts.ProjectPage{}, err
	}
	return pageOf(projectID, dr), nil
}

// GetProjectPage implements projects.get_page; a project without a page
// returns an empty ProjectPage (Revision nil).
func (h *Hub) GetProjectPage(ctx context.Context, req contracts.GetProjectPageRequest) (contracts.ProjectPage, error) {
	p, err := principal(ctx)
	if err != nil {
		return contracts.ProjectPage{}, err
	}
	if h.cfg.Documents == nil {
		return contracts.ProjectPage{}, notConfigured("documents")
	}
	projectID, _, err := h.pageProject(ctx, p, req.ProjectID)
	if err != nil {
		return contracts.ProjectPage{}, err
	}
	dr, err := h.cfg.Documents.GetDocument(ctx, contracts.GetDocumentRequest{ProjectID: projectID, Slug: contracts.ProjectPageSlug})
	if contracts.IsCode(err, contracts.ErrNotFound) {
		return contracts.ProjectPage{ProjectID: projectID}, nil
	}
	if err != nil {
		return contracts.ProjectPage{}, err
	}
	return pageOf(projectID, dr), nil
}

func pageOf(projectID string, dr contracts.DocumentWithRevision) contracts.ProjectPage {
	d, r := dr.Document, dr.Revision
	return contracts.ProjectPage{ProjectID: projectID, Document: &d, Revision: &r, Content: r.Content}
}
