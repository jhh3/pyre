package mock

import (
	"context"
	"strings"

	c "github.com/jhh3/pyre/internal/contracts"
)

// Contracts v1.1 operations.

func (b *Backend) DiscardProposal(ctx context.Context, req c.DiscardProposalRequest) (c.DocumentProposal, error) {
	return mutate(b, ctx, c.OpDiscardProposal, req.Mutation, req, func(p c.Principal) (c.DocumentProposal, error) {
		if err := requireNotNode(p); err != nil {
			return c.DocumentProposal{}, err
		}
		pp, ok := b.proposals[req.ProposalID]
		if !ok {
			return c.DocumentProposal{}, notFound(c.KindProposal, req.ProposalID)
		}
		switch pp.State {
		case c.ProposalDiscarded:
			return *pp, nil
		case c.ProposalApplied:
			return c.DocumentProposal{}, c.Errorf(c.ErrConflict, "proposal was already applied").WithRef(c.KindProposal, pp.ProposalID)
		}
		pp.State = c.ProposalDiscarded // content retained for audit
		b.withdrawAttentionLocked("conflict:"+pp.ProposalID, c.AttnHandled)
		return *pp, nil
	})
}

func (b *Backend) GetUpload(ctx context.Context, req c.GetRequest) (c.Upload, error) {
	return query(b, ctx, func(p c.Principal) (c.Upload, error) {
		u, ok := b.uploads[req.ID]
		if !ok {
			return c.Upload{}, notFound(c.KindUpload, req.ID)
		}
		if !samePrincipal(u.owner, p) && p.Kind != c.PrincipalOwner {
			return c.Upload{}, c.Errorf(c.ErrForbidden, "upload belongs to another principal").WithRef(c.KindUpload, req.ID)
		}
		up := u.up
		if up.State == "finalized" {
			up.Received = up.Size
		}
		return up, nil
	})
}

func (b *Backend) UpdateArtifact(ctx context.Context, req c.UpdateArtifactRequest) (c.Artifact, error) {
	return mutate(b, ctx, c.OpUpdateArtifact, req.Mutation, req, func(p c.Principal) (c.Artifact, error) {
		if err := requireNotNode(p); err != nil {
			return c.Artifact{}, err
		}
		a, ok := b.artifacts[req.ArtifactID]
		if !ok {
			return c.Artifact{}, notFound(c.KindArtifact, req.ArtifactID)
		}
		if req.ProjectID != nil {
			if *req.ProjectID == "" {
				a.ProjectID = nil
			} else {
				if _, ok := b.projects[*req.ProjectID]; !ok {
					return c.Artifact{}, notFound(c.KindProject, *req.ProjectID)
				}
				a.ProjectID = cloneStrPtr(req.ProjectID)
			}
		}
		if req.Title != nil {
			if strings.TrimSpace(*req.Title) == "" {
				return c.Artifact{}, c.Errorf(c.ErrInvalid, "title must not be empty")
			}
			a.Title = *req.Title
		}
		if req.Description != nil {
			a.Description = *req.Description
		}
		a.UpdatedAt = b.now()
		return *a, nil
	})
}

func (b *Backend) GetComment(ctx context.Context, req c.GetRequest) (c.ReviewComment, error) {
	return query(b, ctx, func(p c.Principal) (c.ReviewComment, error) {
		cm, ok := b.comments[req.ID]
		if !ok {
			return c.ReviewComment{}, notFound(c.KindComment, req.ID)
		}
		return *cm, nil
	})
}

func (b *Backend) PromoteNote(ctx context.Context, req c.PromoteNoteRequest) (c.DocumentWithRevision, error) {
	return mutate(b, ctx, c.OpPromoteNote, req.Mutation, req, func(p c.Principal) (c.DocumentWithRevision, error) {
		if err := requireNotNode(p); err != nil {
			return c.DocumentWithRevision{}, err
		}
		n, ok := b.notes[req.NoteID]
		if !ok {
			return c.DocumentWithRevision{}, notFound(c.KindNote, req.NoteID)
		}
		if n.PromotedTo != "" {
			return c.DocumentWithRevision{}, c.Errorf(c.ErrConflict, "note already promoted to %s", n.PromotedTo).WithRef(c.KindNote, n.NoteID).WithDetail("promoted_to", n.PromotedTo)
		}
		content := n.Body
		now := b.now()
		if req.DocumentID != "" {
			d, ok := b.documents[req.DocumentID]
			if !ok {
				return c.DocumentWithRevision{}, notFound(c.KindDocument, req.DocumentID)
			}
			if req.BaseRevisionID == "" {
				return c.DocumentWithRevision{}, c.Errorf(c.ErrInvalid, "base_revision_id is required to promote into an existing document")
			}
			if d.HeadRevisionID != req.BaseRevisionID {
				pp := &c.DocumentProposal{ProposalID: c.NewID(c.PrefixProposal), DocumentID: d.DocumentID, BaseRevisionID: req.BaseRevisionID,
					Content: content, Author: p, CreatedAt: now, State: c.ProposalConflicted, HeadAtConflict: d.HeadRevisionID}
				b.proposals[pp.ProposalID] = pp
				ppc := *pp
				ppc.Content = ""
				b.emit(c.EvDocumentConflict, ref(c.KindProposal, pp.ProposalID), "", d.ProjectID, ppc)
				return c.DocumentWithRevision{}, c.Errorf(c.ErrConflict, "document head moved").WithRef(c.KindDocument, d.DocumentID).
					WithDetail("current_head", d.HeadRevisionID).WithDetail("proposal_id", pp.ProposalID)
			}
			r := &c.DocumentRevision{RevisionID: c.NewID(c.PrefixRevision), DocumentID: d.DocumentID, Seq: d.HeadSeq + 1, ParentRevisionID: d.HeadRevisionID,
				Digest: sha256Digest([]byte(content)), Size: int64(len(content)), Author: p, CreatedAt: now, Summary: "promoted from " + n.NoteID, Content: content,
				Provenance: &n.Provenance}
			b.revisions[r.RevisionID] = r
			d.HeadRevisionID, d.HeadSeq, d.UpdatedAt = r.RevisionID, r.Seq, now
			n.PromotedTo = r.RevisionID
			b.emit(c.EvDocumentRevised, ref(c.KindDocumentRevision, r.RevisionID), "", d.ProjectID, withoutContent(*r))
			return c.DocumentWithRevision{Document: *d, Revision: *r}, nil
		}
		pid := req.ProjectID
		if pid == nil {
			pid = n.ProjectID
		}
		if err := b.checkProject(pid); err != nil {
			return c.DocumentWithRevision{}, err
		}
		title := req.Title
		if title == "" {
			title = n.Title
		}
		if strings.TrimSpace(title) == "" {
			return c.DocumentWithRevision{}, c.Errorf(c.ErrInvalid, "title is required")
		}
		slug := req.Slug
		if slug == "" {
			slug = slugify(title)
		}
		if b.slugTaken(pid, slug) {
			return c.DocumentWithRevision{}, c.Errorf(c.ErrConflict, "a document with slug %q already exists here", slug)
		}
		d := b.createDocLocked(p, pid, title, slug, "text/markdown", content, "promoted from "+n.NoteID)
		r := b.revisions[d.HeadRevisionID]
		prov := n.Provenance
		r.Provenance = &prov
		n.PromotedTo = r.RevisionID
		return c.DocumentWithRevision{Document: *d, Revision: *r}, nil
	})
}
