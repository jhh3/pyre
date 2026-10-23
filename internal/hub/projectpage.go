package hub

import (
	"context"
	"encoding/json"
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
	hasContent := strings.TrimSpace(req.Content) != ""
	if !hasContent && req.Status == nil {
		return contracts.ProjectPage{}, contracts.Errorf(contracts.ErrInvalid, "content or status is required: the page in markdown (goal, current status, results with links, open decisions, what's next) and/or the status card")
	}
	if !hasContent && req.BaseRevisionID != "" {
		return contracts.ProjectPage{}, contracts.Errorf(contracts.ErrInvalid, "base_revision_id applies to content; a status-only write has no base")
	}
	var statusJSON []byte
	if req.Status != nil {
		var err error
		if statusJSON, err = contracts.ValidateProjectStatus(req.Status); err != nil {
			return contracts.ProjectPage{}, err
		}
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
	summary := strings.TrimSpace(req.Summary)
	var out contracts.ProjectPage
	if hasContent {
		dr, err := h.writePageDoc(ctx, projectID, contracts.ProjectPageSlug, "Project page", "", req.Content, summary, req.BaseRevisionID, req.OperationID+":page")
		if err != nil {
			return contracts.ProjectPage{}, err
		}
		out = pageOf(projectID, dr)
	} else if out, err = h.readPage(ctx, projectID); err != nil {
		return contracts.ProjectPage{}, err
	}
	if req.Status != nil {
		dr, err := h.writePageDoc(ctx, projectID, contracts.ProjectStatusSlug, "Project status", "application/json", string(statusJSON), summary, "", req.OperationID+":status")
		if err != nil {
			return contracts.ProjectPage{}, err
		}
		st := *req.Status
		at := dr.Revision.CreatedAt
		out.Status, out.StatusUpdatedAt = &st, &at
	} else if err := h.readStatus(ctx, &out); err != nil {
		return contracts.ProjectPage{}, err
	}
	return out, nil
}

// writePageDoc writes a project document by slug: the first write creates
// it, later ones are head revisions (compare-and-swap on base when given;
// otherwise on the head just read, retried once if another write won).
func (h *Hub) writePageDoc(ctx context.Context, projectID, slug, title, mediaType, content, summary, base, opID string) (contracts.DocumentWithRevision, error) {
	docs := h.cfg.Documents
	cur, err := docs.GetDocument(ctx, contracts.GetDocumentRequest{ProjectID: projectID, Slug: slug})
	switch {
	case contracts.IsCode(err, contracts.ErrNotFound):
		if base != "" {
			return contracts.DocumentWithRevision{}, contracts.Errorf(contracts.ErrConflict, "the project has no page yet, so base_revision_id %s cannot be its head; write without it to create the page", base).
				WithDetail("current_head", "")
		}
		pid := projectID
		dr, err := docs.CreateDocument(ctx, contracts.CreateDocumentRequest{Mutation: contracts.Mutation{OperationID: opID + "-create"},
			ProjectID: &pid, Title: title, Slug: slug, MediaType: mediaType, Content: content, Summary: summary})
		if err == nil {
			return dr, nil
		}
		if !contracts.IsCode(err, contracts.ErrConflict) {
			return contracts.DocumentWithRevision{}, err
		}
		// Created concurrently: write a revision on top of it.
		if cur, err = docs.GetDocument(ctx, contracts.GetDocumentRequest{ProjectID: projectID, Slug: slug}); err != nil {
			return contracts.DocumentWithRevision{}, err
		}
	case err != nil:
		return contracts.DocumentWithRevision{}, err
	}
	base = strings.TrimSpace(base)
	cas := base != ""
	if !cas {
		base = cur.Document.HeadRevisionID
	}
	dr, err := docs.ProposeRevision(ctx, contracts.ProposeRevisionRequest{Mutation: contracts.Mutation{OperationID: opID + "-rev"},
		DocumentID: cur.Document.DocumentID, BaseRevisionID: base, Content: content, Summary: summary})
	if contracts.IsCode(err, contracts.ErrConflict) && !cas {
		// Another write landed between our read and write: last writer wins.
		if cur, err = docs.GetDocument(ctx, contracts.GetDocumentRequest{DocumentID: cur.Document.DocumentID}); err != nil {
			return contracts.DocumentWithRevision{}, err
		}
		dr, err = docs.ProposeRevision(ctx, contracts.ProposeRevisionRequest{Mutation: contracts.Mutation{OperationID: opID + "-rev2"},
			DocumentID: cur.Document.DocumentID, BaseRevisionID: cur.Document.HeadRevisionID, Content: content, Summary: summary})
	}
	return dr, err
}

// GetProjectPage implements projects.get_page; a project without a page
// returns an empty ProjectPage (Revision nil); Status is the status card.
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
	pg, err := h.readPage(ctx, projectID)
	if err != nil {
		return contracts.ProjectPage{}, err
	}
	if err := h.readStatus(ctx, &pg); err != nil {
		return contracts.ProjectPage{}, err
	}
	return pg, nil
}

// readPage is the page head (Revision nil when none was written).
func (h *Hub) readPage(ctx context.Context, projectID string) (contracts.ProjectPage, error) {
	dr, err := h.cfg.Documents.GetDocument(ctx, contracts.GetDocumentRequest{ProjectID: projectID, Slug: contracts.ProjectPageSlug})
	if contracts.IsCode(err, contracts.ErrNotFound) {
		return contracts.ProjectPage{ProjectID: projectID}, nil
	}
	if err != nil {
		return contracts.ProjectPage{}, err
	}
	return pageOf(projectID, dr), nil
}

// readStatus fills pg's status card from its document (none: unchanged).
func (h *Hub) readStatus(ctx context.Context, pg *contracts.ProjectPage) error {
	dr, err := h.cfg.Documents.GetDocument(ctx, contracts.GetDocumentRequest{ProjectID: pg.ProjectID, Slug: contracts.ProjectStatusSlug})
	if contracts.IsCode(err, contracts.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	var st contracts.ProjectStatus
	if json.Unmarshal([]byte(dr.Revision.Content), &st) != nil {
		return nil // hand-edited into something else: no card rather than a broken page
	}
	at := dr.Revision.CreatedAt
	pg.Status, pg.StatusUpdatedAt = &st, &at
	return nil
}

func pageOf(projectID string, dr contracts.DocumentWithRevision) contracts.ProjectPage {
	d, r := dr.Document, dr.Revision
	return contracts.ProjectPage{ProjectID: projectID, Document: &d, Revision: &r, Content: r.Content}
}
