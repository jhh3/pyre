package mock

import (
	"bytes"
	"context"
	"io"
	"mime"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
)

func (b *Backend) checkProject(id *string) error {
	if id == nil {
		return nil
	}
	if _, ok := b.projects[*id]; !ok {
		return notFound(c.KindProject, *id)
	}
	return nil
}

// ---- projects ----

func (b *Backend) CreateProject(ctx context.Context, req c.CreateProjectRequest) (c.Project, error) {
	return mutate(b, ctx, c.OpCreateProject, req.Mutation, req, func(p c.Principal) (c.Project, error) {
		if err := requireNotNode(p); err != nil {
			return c.Project{}, err
		}
		if strings.TrimSpace(req.Name) == "" {
			return c.Project{}, c.Errorf(c.ErrInvalid, "name is required")
		}
		now := b.now()
		pr := &c.Project{
			ProjectID: c.NewID(c.PrefixProject), Name: req.Name, Description: req.Description,
			Repos: append([]c.RepoBinding{}, req.Repos...), Checkouts: []c.CheckoutBinding{}, Links: req.Links,
			Version: 1, CreatedBy: p, CreatedAt: now, UpdatedAt: now,
		}
		b.projects[pr.ProjectID] = pr
		if req.Brief != "" {
			d := b.createDocLocked(p, &pr.ProjectID, "Brief", "brief", "text/markdown", req.Brief, "initial brief")
			pr.BriefDocumentID = d.DocumentID
		}
		b.emit(c.EvProjectChanged, ref(c.KindProject, pr.ProjectID), "", &pr.ProjectID, pr)
		return *pr, nil
	})
}

func (b *Backend) GetProject(ctx context.Context, req c.GetRequest) (c.Project, error) {
	return query(b, ctx, func(p c.Principal) (c.Project, error) {
		pr, ok := b.projects[req.ID]
		if !ok {
			return c.Project{}, notFound(c.KindProject, req.ID)
		}
		return *pr, nil
	})
}

func (b *Backend) ListProjects(ctx context.Context, req c.ListProjectsRequest) (c.Page[c.Project], error) {
	return query(b, ctx, func(p c.Principal) (c.Page[c.Project], error) {
		q := strings.ToLower(req.Query)
		var out []c.Project
		for _, id := range sortedKeys(b.projects) {
			pr := b.projects[id]
			if pr.Archived && !req.IncludeArchived {
				continue
			}
			if q != "" && !strings.Contains(strings.ToLower(pr.Name+" "+pr.Description), q) {
				continue
			}
			out = append(out, *pr)
		}
		sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
		return paginate(out, req.PageRequest, b.now())
	})
}

func (b *Backend) UpdateProject(ctx context.Context, req c.UpdateProjectRequest) (c.Project, error) {
	return mutate(b, ctx, c.OpUpdateProject, req.Mutation, req, func(p c.Principal) (c.Project, error) {
		if err := requireNotNode(p); err != nil {
			return c.Project{}, err
		}
		pr, ok := b.projects[req.ProjectID]
		if !ok {
			return c.Project{}, notFound(c.KindProject, req.ProjectID)
		}
		if req.Version != pr.Version {
			return c.Project{}, c.Errorf(c.ErrConflict, "project version is %d, not %d", pr.Version, req.Version).WithRef(c.KindProject, pr.ProjectID).WithDetail("current_version", pr.Version)
		}
		if req.Name != nil {
			if strings.TrimSpace(*req.Name) == "" {
				return c.Project{}, c.Errorf(c.ErrInvalid, "name must not be empty")
			}
			pr.Name = *req.Name
		}
		if req.Description != nil {
			pr.Description = *req.Description
		}
		if req.Repos != nil {
			pr.Repos = append([]c.RepoBinding{}, (*req.Repos)...)
		}
		if req.Checkouts != nil {
			pr.Checkouts = append([]c.CheckoutBinding{}, (*req.Checkouts)...)
		}
		if req.Links != nil {
			pr.Links = append([]c.Link(nil), (*req.Links)...)
		}
		if req.EnvProfile != nil {
			pr.EnvProfile = *req.EnvProfile
		}
		if req.Connections != nil {
			pr.Connections = append([]string(nil), (*req.Connections)...)
		}
		if req.Archived != nil {
			pr.Archived = *req.Archived
		}
		pr.Version++
		pr.UpdatedAt = b.now()
		b.emit(c.EvProjectChanged, ref(c.KindProject, pr.ProjectID), "", &pr.ProjectID, pr)
		return *pr, nil
	})
}

// ---- documents ----

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

func slugify(s string) string {
	s = strings.Trim(slugRe.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if s == "" {
		s = "doc"
	}
	if len(s) > 60 {
		s = s[:60]
	}
	return s
}

func (b *Backend) slugTaken(projectID *string, slug string) bool {
	for _, d := range b.documents {
		if d.Slug == slug && eqStrPtr(d.ProjectID, projectID) {
			return true
		}
	}
	return false
}

func (b *Backend) createDocLocked(p c.Principal, projectID *string, title, slug, mediaType, content, summary string) *c.Document {
	now := b.now()
	d := &c.Document{DocumentID: c.NewID(c.PrefixDocument), ProjectID: cloneStrPtr(projectID), Title: title, Slug: slug, MediaType: mediaType, CreatedBy: p, CreatedAt: now, UpdatedAt: now}
	r := &c.DocumentRevision{RevisionID: c.NewID(c.PrefixRevision), DocumentID: d.DocumentID, Seq: 1, Digest: sha256Digest([]byte(content)), Size: int64(len(content)), Author: p, CreatedAt: now, Summary: summary, Content: content}
	d.HeadRevisionID, d.HeadSeq = r.RevisionID, 1
	b.documents[d.DocumentID] = d
	b.revisions[r.RevisionID] = r
	b.emit(c.EvDocumentRevised, ref(c.KindDocumentRevision, r.RevisionID), "", projectID, withoutContent(*r))
	return d
}

const maxDocBytes = 1 << 20

func (b *Backend) CreateDocument(ctx context.Context, req c.CreateDocumentRequest) (c.DocumentWithRevision, error) {
	return mutate(b, ctx, c.OpCreateDocument, req.Mutation, req, func(p c.Principal) (c.DocumentWithRevision, error) {
		if err := requireNotNode(p); err != nil {
			return c.DocumentWithRevision{}, err
		}
		if strings.TrimSpace(req.Title) == "" {
			return c.DocumentWithRevision{}, c.Errorf(c.ErrInvalid, "title is required")
		}
		if len(req.Content) > maxDocBytes {
			return c.DocumentWithRevision{}, c.Errorf(c.ErrPayloadTooLarge, "document exceeds %d bytes", maxDocBytes)
		}
		if err := b.checkProject(req.ProjectID); err != nil {
			return c.DocumentWithRevision{}, err
		}
		slug := req.Slug
		if slug == "" {
			slug = slugify(req.Title)
		} else if slugify(slug) != slug {
			return c.DocumentWithRevision{}, c.Errorf(c.ErrInvalid, "slug must be lowercase letters, digits and dashes")
		}
		if b.slugTaken(req.ProjectID, slug) {
			return c.DocumentWithRevision{}, c.Errorf(c.ErrConflict, "a document with slug %q already exists here", slug)
		}
		mt := req.MediaType
		if mt == "" {
			mt = "text/markdown"
		}
		d := b.createDocLocked(p, req.ProjectID, req.Title, slug, mt, req.Content, req.Summary)
		return c.DocumentWithRevision{Document: *d, Revision: *b.revisions[d.HeadRevisionID]}, nil
	})
}

func (b *Backend) GetDocument(ctx context.Context, req c.GetDocumentRequest) (c.DocumentWithRevision, error) {
	return query(b, ctx, func(p c.Principal) (c.DocumentWithRevision, error) {
		var d *c.Document
		switch {
		case req.DocumentID != "":
			d = b.documents[req.DocumentID]
		case req.ProjectID != "" && req.Slug != "":
			for _, x := range b.documents {
				if x.ProjectID != nil && *x.ProjectID == req.ProjectID && x.Slug == req.Slug {
					d = x
				}
			}
		default:
			return c.DocumentWithRevision{}, c.Errorf(c.ErrInvalid, "document_id or project_id+slug is required")
		}
		if d == nil {
			return c.DocumentWithRevision{}, notFound(c.KindDocument, req.DocumentID+req.Slug)
		}
		revID := req.RevisionID
		if revID == "" {
			revID = d.HeadRevisionID
		}
		r, ok := b.revisions[revID]
		if !ok || r.DocumentID != d.DocumentID {
			return c.DocumentWithRevision{}, notFound(c.KindDocumentRevision, revID)
		}
		return c.DocumentWithRevision{Document: *d, Revision: *r}, nil
	})
}

func (b *Backend) ListDocuments(ctx context.Context, req c.ListDocumentsRequest) (c.Page[c.Document], error) {
	return query(b, ctx, func(p c.Principal) (c.Page[c.Document], error) {
		var out []c.Document
		for _, id := range sortedKeys(b.documents) {
			d := b.documents[id]
			if req.ProjectID != nil {
				if *req.ProjectID == "" && d.ProjectID != nil {
					continue
				}
				if *req.ProjectID != "" && !eqStrPtr(d.ProjectID, req.ProjectID) {
					continue
				}
			}
			out = append(out, *d)
		}
		return paginate(out, req.PageRequest, b.now())
	})
}

func (b *Backend) ListRevisions(ctx context.Context, req c.ListRevisionsRequest) (c.Page[c.DocumentRevision], error) {
	return query(b, ctx, func(p c.Principal) (c.Page[c.DocumentRevision], error) {
		if _, ok := b.documents[req.DocumentID]; !ok {
			return c.Page[c.DocumentRevision]{}, notFound(c.KindDocument, req.DocumentID)
		}
		var out []c.DocumentRevision
		for _, r := range b.revisions {
			if r.DocumentID == req.DocumentID {
				rc := *r
				rc.Content = "" // omitted in history listings
				out = append(out, rc)
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Seq > out[j].Seq })
		return paginate(out, req.PageRequest, b.now())
	})
}

func (b *Backend) ProposeRevision(ctx context.Context, req c.ProposeRevisionRequest) (c.DocumentWithRevision, error) {
	return mutate(b, ctx, c.OpProposeRevision, req.Mutation, req, func(p c.Principal) (c.DocumentWithRevision, error) {
		if err := requireNotNode(p); err != nil {
			return c.DocumentWithRevision{}, err
		}
		d, ok := b.documents[req.DocumentID]
		if !ok {
			return c.DocumentWithRevision{}, notFound(c.KindDocument, req.DocumentID)
		}
		if req.BaseRevisionID == "" {
			return c.DocumentWithRevision{}, c.Errorf(c.ErrInvalid, "base_revision_id is required")
		}
		if len(req.Content) > maxDocBytes {
			return c.DocumentWithRevision{}, c.Errorf(c.ErrPayloadTooLarge, "document exceeds %d bytes", maxDocBytes)
		}
		now := b.now()
		if d.HeadRevisionID != req.BaseRevisionID {
			pp := &c.DocumentProposal{ProposalID: c.NewID(c.PrefixProposal), DocumentID: d.DocumentID, BaseRevisionID: req.BaseRevisionID,
				Content: req.Content, Author: p, CreatedAt: now, State: c.ProposalConflicted, HeadAtConflict: d.HeadRevisionID}
			b.proposals[pp.ProposalID] = pp
			ppc := *pp
			ppc.Content = ""
			b.emit(c.EvDocumentConflict, ref(c.KindProposal, pp.ProposalID), "", d.ProjectID, ppc)
			b.raiseAttentionLocked(c.AttnConflict, "normal", "conflict:"+pp.ProposalID, ref(c.KindProposal, pp.ProposalID), sessionOf(p), d.ProjectID,
				"Edit conflict on "+d.Title, "A proposal based on an older revision was preserved for review.")
			return c.DocumentWithRevision{}, c.Errorf(c.ErrConflict, "document head moved").WithRef(c.KindDocument, d.DocumentID).
				WithDetail("current_head", d.HeadRevisionID).WithDetail("proposal_id", pp.ProposalID)
		}
		r := &c.DocumentRevision{RevisionID: c.NewID(c.PrefixRevision), DocumentID: d.DocumentID, Seq: d.HeadSeq + 1, ParentRevisionID: d.HeadRevisionID,
			Digest: sha256Digest([]byte(req.Content)), Size: int64(len(req.Content)), Author: p, CreatedAt: now, Summary: req.Summary, Content: req.Content}
		b.revisions[r.RevisionID] = r
		d.HeadRevisionID, d.HeadSeq, d.UpdatedAt = r.RevisionID, r.Seq, now
		b.emit(c.EvDocumentRevised, ref(c.KindDocumentRevision, r.RevisionID), "", d.ProjectID, withoutContent(*r))
		return c.DocumentWithRevision{Document: *d, Revision: *r}, nil
	})
}

func withoutContent(r c.DocumentRevision) c.DocumentRevision {
	r.Content = ""
	return r
}

func sessionOf(p c.Principal) string {
	if p.Kind == c.PrincipalSession {
		return p.ID
	}
	return ""
}

func (b *Backend) ListProposals(ctx context.Context, req c.ListProposalsRequest) (c.Page[c.DocumentProposal], error) {
	return query(b, ctx, func(p c.Principal) (c.Page[c.DocumentProposal], error) {
		if _, ok := b.documents[req.DocumentID]; !ok {
			return c.Page[c.DocumentProposal]{}, notFound(c.KindDocument, req.DocumentID)
		}
		var out []c.DocumentProposal
		for _, id := range sortedKeys(b.proposals) {
			pp := b.proposals[id]
			if pp.DocumentID != req.DocumentID {
				continue
			}
			if len(req.States) > 0 {
				ok := false
				for _, s := range req.States {
					if pp.State == s {
						ok = true
					}
				}
				if !ok {
					continue
				}
			}
			out = append(out, *pp)
		}
		return paginate(out, req.PageRequest, b.now())
	})
}

// ---- artifacts ----

func validDigest(d c.Digest) bool {
	s := string(d)
	if !strings.HasPrefix(s, "sha256:") || len(s) != len("sha256:")+64 {
		return false
	}
	for _, ch := range s[7:] {
		if !(ch >= '0' && ch <= '9' || ch >= 'a' && ch <= 'f') {
			return false
		}
	}
	return true
}

func isActiveContent(mt string) bool {
	base, _, _ := mime.ParseMediaType(mt)
	switch base {
	case "text/html", "application/xhtml+xml", "image/svg+xml", "application/javascript", "text/javascript", "application/xml", "text/xml":
		return true
	}
	return false
}

func (b *Backend) BeginUpload(ctx context.Context, req c.BeginUploadRequest) (c.Upload, error) {
	return mutate(b, ctx, c.OpBeginUpload, req.Mutation, req, func(p c.Principal) (c.Upload, error) {
		if err := requireNotNode(p); err != nil {
			return c.Upload{}, err
		}
		if strings.TrimSpace(req.Filename) == "" {
			return c.Upload{}, c.Errorf(c.ErrInvalid, "filename is required")
		}
		if req.Size < 0 {
			return c.Upload{}, c.Errorf(c.ErrInvalid, "size must be >= 0")
		}
		if req.Size > b.opts.MaxUploadBytes {
			return c.Upload{}, c.Errorf(c.ErrPayloadTooLarge, "upload exceeds %d bytes", b.opts.MaxUploadBytes)
		}
		if !validDigest(req.Digest) {
			return c.Upload{}, c.Errorf(c.ErrInvalid, "digest must be sha256:<64 lowercase hex>")
		}
		if req.ArtifactID != "" {
			if _, ok := b.artifacts[req.ArtifactID]; !ok {
				return c.Upload{}, notFound(c.KindArtifact, req.ArtifactID)
			}
		} else if strings.TrimSpace(req.Title) == "" {
			return c.Upload{}, c.Errorf(c.ErrInvalid, "title (new artifact) or artifact_id (new version) is required")
		}
		if err := b.checkProject(req.ProjectID); err != nil {
			return c.Upload{}, err
		}
		u := &upload{req: req, owner: p, up: c.Upload{UploadID: c.NewID(c.PrefixUpload), ArtifactID: req.ArtifactID, Size: req.Size, Digest: req.Digest,
			ExpiresAt: b.now().Add(24 * time.Hour), MaxBytes: b.opts.MaxUploadBytes, State: "open"}}
		b.uploads[u.up.UploadID] = u
		return u.up, nil
	})
}

func (b *Backend) UploadChunk(ctx context.Context, uploadID string, offset int64, r io.Reader) (c.Upload, error) {
	p, err := b.principal(ctx)
	if err != nil {
		return c.Upload{}, err
	}
	b.mu.Lock()
	u, ok := b.uploads[uploadID]
	if !ok {
		b.mu.Unlock()
		return c.Upload{}, notFound(c.KindUpload, uploadID)
	}
	if !samePrincipal(u.owner, p) {
		b.mu.Unlock()
		return c.Upload{}, c.Errorf(c.ErrForbidden, "upload belongs to another principal").WithRef(c.KindUpload, uploadID)
	}
	state, received, size := u.up.State, int64(len(u.data)), u.up.Size
	snapshot := u.up
	b.mu.Unlock()
	switch state {
	case "finalized":
		snapshot.Received = size
		return snapshot, nil // idempotent replay after publish
	case "open":
	default:
		return c.Upload{}, c.Errorf(c.ErrConflict, "upload is %s", state).WithRef(c.KindUpload, uploadID)
	}
	if offset < 0 || offset > received {
		return c.Upload{}, c.Errorf(c.ErrConflict, "offset %d does not match received bytes %d", offset, received).WithRef(c.KindUpload, uploadID).WithDetail("received", received)
	}
	// Read outside the lock, bounded by the declared size.
	data, err := io.ReadAll(io.LimitReader(r, size-offset+1))
	if err != nil {
		var ce *c.Error
		if asErr(err, &ce) {
			return c.Upload{}, ce
		}
		return c.Upload{}, c.Errorf(c.ErrUnavailable, "upload body interrupted").WithRef(c.KindUpload, uploadID)
	}
	if offset+int64(len(data)) > size {
		return c.Upload{}, c.Errorf(c.ErrPayloadTooLarge, "chunk extends past declared size %d", size).WithRef(c.KindUpload, uploadID)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if u.up.State != "open" {
		return c.Upload{}, c.Errorf(c.ErrConflict, "upload is %s", u.up.State).WithRef(c.KindUpload, uploadID)
	}
	if offset > int64(len(u.data)) {
		return c.Upload{}, c.Errorf(c.ErrConflict, "offset %d does not match received bytes %d", offset, len(u.data)).WithRef(c.KindUpload, uploadID).WithDetail("received", int64(len(u.data)))
	}
	end := offset + int64(len(data))
	if end > int64(len(u.data)) {
		u.data = append(u.data[:offset:offset], data...)
	} else {
		copy(u.data[offset:end], data) // same-range retry; finalize verifies digest
	}
	u.up.Received = int64(len(u.data))
	return u.up, nil
}

func asErr(err error, target **c.Error) bool {
	ce, ok := err.(*c.Error)
	if ok {
		*target = ce
	}
	return ok
}

func (b *Backend) FinalizeUpload(ctx context.Context, req c.FinalizeUploadRequest) (c.PublishReceipt, error) {
	return mutate(b, ctx, c.OpFinalizeUpload, req.Mutation, req, func(p c.Principal) (c.PublishReceipt, error) {
		u, ok := b.uploads[req.UploadID]
		if !ok {
			return c.PublishReceipt{}, notFound(c.KindUpload, req.UploadID)
		}
		if !samePrincipal(u.owner, p) {
			return c.PublishReceipt{}, c.Errorf(c.ErrForbidden, "upload belongs to another principal").WithRef(c.KindUpload, req.UploadID)
		}
		if u.receipt != nil {
			return *u.receipt, nil
		}
		if u.up.State != "open" {
			return c.PublishReceipt{}, c.Errorf(c.ErrConflict, "upload is %s; nothing was published", u.up.State).WithRef(c.KindUpload, req.UploadID)
		}
		if int64(len(u.data)) != u.up.Size {
			return c.PublishReceipt{}, c.Errorf(c.ErrConflict, "upload incomplete: received %d of %d bytes", len(u.data), u.up.Size).WithRef(c.KindUpload, req.UploadID).WithDetail("received", int64(len(u.data)))
		}
		if sha256Digest(u.data) != u.up.Digest {
			u.up.State = "failed"
			u.data = nil
			return c.PublishReceipt{}, c.Errorf(c.ErrConflict, "uploaded bytes do not match declared sha256; nothing was published").WithRef(c.KindUpload, req.UploadID)
		}
		now := b.now()
		rq := u.req
		var art *c.Artifact
		if rq.ArtifactID != "" {
			art = b.artifacts[rq.ArtifactID]
		} else {
			art = &c.Artifact{ArtifactID: c.NewID(c.PrefixArtifact), ProjectID: cloneStrPtr(rq.ProjectID), SessionID: rq.Provenance.SessionID,
				Title: rq.Title, Description: rq.Description, CreatedBy: p, CreatedAt: now}
			if art.SessionID == "" {
				art.SessionID = sessionOf(p)
			}
			b.artifacts[art.ArtifactID] = art
		}
		mt := rq.MediaType
		if mt == "" {
			mt = mime.TypeByExtension(path.Ext(rq.Filename))
			if mt == "" {
				mt = "application/octet-stream"
			}
		}
		prov := rq.Provenance
		if prov.SessionID == "" && p.Kind == c.PrincipalSession {
			prov.SessionID = p.ID
			if r := b.currentRun(p.ID); r != nil {
				prov.RunID, prov.NodeID = r.RunID, r.NodeID
			}
		}
		if prov.ProjectID == nil {
			prov.ProjectID = cloneStrPtr(art.ProjectID)
		}
		v := &c.ArtifactVersion{VersionID: c.NewID(c.PrefixVersion), ArtifactID: art.ArtifactID, Seq: int64(art.VersionCount + 1), PreviousVersionID: art.LatestVersionID,
			Digest: u.up.Digest, Size: u.up.Size, MediaType: mt, Filename: path.Base(strings.ReplaceAll(rq.Filename, "\\", "/")), Author: p, Provenance: prov, CreatedAt: now, Note: rq.Note,
			ReviewRequested: rq.ReviewRequested}
		v.ContentURL = "/v1/content/" + v.VersionID
		if isActiveContent(mt) {
			v.PreviewURL = "/preview/" + v.VersionID + "/{token}"
		}
		b.versions[v.VersionID] = v
		b.contents[v.VersionID] = bytes.Clone(u.data)
		art.LatestVersionID = v.VersionID
		art.VersionCount++
		art.UpdatedAt = now
		u.up.State = "finalized"
		u.up.ArtifactID = art.ArtifactID
		u.data = nil
		rec := c.PublishReceipt{Artifact: *art, Version: *v}
		u.receipt = &rec
		b.emit(c.EvArtifactPublished, ref(c.KindArtifactVersion, v.VersionID), art.SessionID, art.ProjectID, rec)
		if rq.ReviewRequested {
			b.raiseAttentionLocked(c.AttnReviewReady, "normal", "review:"+art.ArtifactID, ref(c.KindArtifactVersion, v.VersionID), art.SessionID, art.ProjectID,
				art.Title+" v"+formatInt64(v.Seq)+" ready for review", truncate(rq.Note, 120))
		}
		return rec, nil
	})
}

func (b *Backend) GetArtifact(ctx context.Context, req c.GetRequest) (c.Artifact, error) {
	return query(b, ctx, func(p c.Principal) (c.Artifact, error) {
		a, ok := b.artifacts[req.ID]
		if !ok {
			return c.Artifact{}, notFound(c.KindArtifact, req.ID)
		}
		return *a, nil
	})
}

func (b *Backend) ListArtifacts(ctx context.Context, req c.ListArtifactsRequest) (c.Page[c.Artifact], error) {
	return query(b, ctx, func(p c.Principal) (c.Page[c.Artifact], error) {
		var out []c.Artifact
		for _, id := range sortedKeys(b.artifacts) {
			a := b.artifacts[id]
			if req.ProjectID != nil && !eqStrPtr(a.ProjectID, req.ProjectID) && !(*req.ProjectID == "" && a.ProjectID == nil) {
				continue
			}
			if req.SessionID != "" && a.SessionID != req.SessionID {
				continue
			}
			out = append(out, *a)
		}
		return paginate(out, req.PageRequest, b.now())
	})
}

func (b *Backend) ListVersions(ctx context.Context, req c.ListVersionsRequest) (c.Page[c.ArtifactVersion], error) {
	return query(b, ctx, func(p c.Principal) (c.Page[c.ArtifactVersion], error) {
		if _, ok := b.artifacts[req.ArtifactID]; !ok {
			return c.Page[c.ArtifactVersion]{}, notFound(c.KindArtifact, req.ArtifactID)
		}
		var out []c.ArtifactVersion
		for _, v := range b.versions {
			if v.ArtifactID == req.ArtifactID {
				out = append(out, *v)
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Seq > out[j].Seq })
		return paginate(out, req.PageRequest, b.now())
	})
}

func (b *Backend) GetVersion(ctx context.Context, req c.GetRequest) (c.ArtifactVersion, error) {
	return query(b, ctx, func(p c.Principal) (c.ArtifactVersion, error) {
		v, ok := b.versions[req.ID]
		if !ok {
			return c.ArtifactVersion{}, notFound(c.KindArtifactVersion, req.ID)
		}
		return *v, nil
	})
}

type contentReader struct{ *bytes.Reader }

func (contentReader) Close() error { return nil }

// OpenContent returns an io.ReadSeekCloser over the immutable bytes (the
// HTTP server uses Seek for Range requests). Bytes are re-verified against
// the recorded digest on every open.
func (b *Backend) OpenContent(ctx context.Context, versionID string) (io.ReadCloser, c.ArtifactVersion, error) {
	if _, err := b.principal(ctx); err != nil {
		return nil, c.ArtifactVersion{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	v, ok := b.versions[versionID]
	if !ok {
		return nil, c.ArtifactVersion{}, notFound(c.KindArtifactVersion, versionID)
	}
	data := b.contents[versionID]
	if sha256Digest(data) != v.Digest || int64(len(data)) != v.Size {
		return nil, c.ArtifactVersion{}, c.Errorf(c.ErrInternal, "stored content failed verification")
	}
	return contentReader{bytes.NewReader(data)}, clone(*v), nil
}

// ---- comments ----

func (b *Backend) targetProject(t c.ObjectRef) (*string, error) {
	switch t.Kind {
	case c.KindArtifactVersion:
		v, ok := b.versions[t.ID]
		if !ok {
			return nil, notFound(t.Kind, t.ID)
		}
		if a := b.artifacts[v.ArtifactID]; a != nil {
			return cloneStrPtr(a.ProjectID), nil
		}
		return nil, nil
	case c.KindDocumentRevision:
		r, ok := b.revisions[t.ID]
		if !ok {
			return nil, notFound(t.Kind, t.ID)
		}
		if d := b.documents[r.DocumentID]; d != nil {
			return cloneStrPtr(d.ProjectID), nil
		}
		return nil, nil
	}
	return nil, c.Errorf(c.ErrInvalid, "comment target must be an artifact_version or document_revision")
}

func (b *Backend) CreateComment(ctx context.Context, req c.CreateCommentRequest) (c.ReviewComment, error) {
	return mutate(b, ctx, c.OpCreateComment, req.Mutation, req, func(p c.Principal) (c.ReviewComment, error) {
		if err := requireNotNode(p); err != nil {
			return c.ReviewComment{}, err
		}
		pid, err := b.targetProject(req.Target)
		if err != nil {
			return c.ReviewComment{}, err
		}
		if strings.TrimSpace(req.Body) == "" {
			return c.ReviewComment{}, c.Errorf(c.ErrInvalid, "comment body is empty")
		}
		if req.InReplyTo != "" {
			parent, ok := b.comments[req.InReplyTo]
			if !ok {
				return c.ReviewComment{}, notFound(c.KindComment, req.InReplyTo)
			}
			if parent.Target != req.Target {
				return c.ReviewComment{}, c.Errorf(c.ErrInvalid, "a reply must target the same version as its parent")
			}
		}
		for _, m := range req.Mentions {
			if m == "owner" {
				continue
			}
			if _, ok := b.sessions[m]; !ok {
				return c.ReviewComment{}, notFound(c.KindSession, m)
			}
		}
		now := b.now()
		cm := &c.ReviewComment{CommentID: c.NewID(c.PrefixComment), Target: req.Target, Anchor: req.Anchor, Author: p, Body: req.Body,
			InReplyTo: req.InReplyTo, Mentions: req.Mentions, CreatedAt: now, UpdatedAt: now, ProjectID: pid}
		b.comments[cm.CommentID] = cm
		b.emit(c.EvCommentCreated, ref(c.KindComment, cm.CommentID), sessionOf(p), pid, cm)
		for _, m := range req.Mentions {
			if m == "owner" && p.Kind != c.PrincipalOwner {
				b.raiseAttentionLocked(c.AttnComment, "normal", "comment:"+cm.CommentID, ref(c.KindComment, cm.CommentID), sessionOf(p), pid,
					"Comment from "+principalName(p), truncate(cm.Body, 120))
			}
		}
		return *cm, nil
	})
}

func (b *Backend) ListComments(ctx context.Context, req c.ListCommentsRequest) (c.Page[c.ReviewComment], error) {
	return query(b, ctx, func(p c.Principal) (c.Page[c.ReviewComment], error) {
		var out []c.ReviewComment
		for _, cm := range b.comments {
			if req.Target != nil && cm.Target != *req.Target {
				continue
			}
			if req.ArtifactID != "" {
				v := b.versions[cm.Target.ID]
				if cm.Target.Kind != c.KindArtifactVersion || v == nil || v.ArtifactID != req.ArtifactID {
					continue
				}
			}
			if req.DocumentID != "" {
				r := b.revisions[cm.Target.ID]
				if cm.Target.Kind != c.KindDocumentRevision || r == nil || r.DocumentID != req.DocumentID {
					continue
				}
			}
			if req.ProjectID != nil && !eqStrPtr(cm.ProjectID, req.ProjectID) {
				continue
			}
			if req.Unresolved && cm.Resolved {
				continue
			}
			out = append(out, *cm)
		}
		sort.Slice(out, func(i, j int) bool {
			if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
				return out[i].CreatedAt.Before(out[j].CreatedAt)
			}
			return out[i].CommentID < out[j].CommentID
		})
		return paginate(out, req.PageRequest, b.now())
	})
}

func (b *Backend) ResolveComment(ctx context.Context, req c.ResolveCommentRequest) (c.ReviewComment, error) {
	return mutate(b, ctx, c.OpResolveComment, req.Mutation, req, func(p c.Principal) (c.ReviewComment, error) {
		cm, ok := b.comments[req.CommentID]
		if !ok {
			return c.ReviewComment{}, notFound(c.KindComment, req.CommentID)
		}
		cm.Resolved = req.Resolved
		if req.Resolved {
			pc := p
			cm.ResolvedBy = &pc
		} else {
			cm.ResolvedBy = nil
		}
		cm.UpdatedAt = b.now()
		b.emit(c.EvCommentResolved, ref(c.KindComment, cm.CommentID), "", cm.ProjectID, cm)
		return *cm, nil
	})
}

// ---- notes & launch context ----

func (b *Backend) CreateNote(ctx context.Context, req c.CreateNoteRequest) (c.Note, error) {
	return mutate(b, ctx, c.OpCreateNote, req.Mutation, req, func(p c.Principal) (c.Note, error) {
		if err := requireNotNode(p); err != nil {
			return c.Note{}, err
		}
		kind := req.Kind
		if kind == "" {
			kind = c.NoteNote
		}
		if kind != c.NoteNote && kind != c.NoteHandoff && kind != c.NoteDecision {
			return c.Note{}, c.Errorf(c.ErrInvalid, "kind must be note, handoff or decision")
		}
		if strings.TrimSpace(req.Title) == "" && strings.TrimSpace(req.Body) == "" {
			return c.Note{}, c.Errorf(c.ErrInvalid, "note is empty")
		}
		if len(req.Body) > maxDocBytes {
			return c.Note{}, c.Errorf(c.ErrPayloadTooLarge, "note exceeds %d bytes", maxDocBytes)
		}
		if err := b.checkProject(req.ProjectID); err != nil {
			return c.Note{}, err
		}
		sid := req.SessionID
		if sid == "" {
			sid = sessionOf(p)
		}
		if sid != "" {
			if _, ok := b.sessions[sid]; !ok {
				return c.Note{}, notFound(c.KindSession, sid)
			}
		}
		prov := c.Provenance{SessionID: sid, ProjectID: cloneStrPtr(req.ProjectID)}
		if r := b.currentRun(sid); r != nil {
			prov.RunID, prov.NodeID = r.RunID, r.NodeID
		}
		n := &c.Note{NoteID: c.NewID(c.PrefixNote), Kind: kind, ProjectID: cloneStrPtr(req.ProjectID), SessionID: sid, Title: req.Title, Body: req.Body,
			Author: p, CreatedAt: b.now(), Provenance: prov, SyncState: "pending"}
		b.notes[n.NoteID] = n
		nc := *n
		nc.Body = ""
		b.emit(c.EvNoteCreated, ref(c.KindNote, n.NoteID), sid, n.ProjectID, nc)
		return *n, nil
	})
}

func (b *Backend) GetNote(ctx context.Context, req c.GetRequest) (c.Note, error) {
	return query(b, ctx, func(p c.Principal) (c.Note, error) {
		n, ok := b.notes[req.ID]
		if !ok {
			return c.Note{}, notFound(c.KindNote, req.ID)
		}
		return *n, nil
	})
}

func (b *Backend) ListNotes(ctx context.Context, req c.ListNotesRequest) (c.Page[c.Note], error) {
	return query(b, ctx, func(p c.Principal) (c.Page[c.Note], error) {
		q := strings.ToLower(req.Query)
		var out []c.Note
		for _, n := range b.notes {
			if req.ProjectID != nil && !eqStrPtr(n.ProjectID, req.ProjectID) && !(*req.ProjectID == "" && n.ProjectID == nil) {
				continue
			}
			if req.SessionID != "" && n.SessionID != req.SessionID {
				continue
			}
			if req.Kind != "" && n.Kind != req.Kind {
				continue
			}
			if q != "" && !strings.Contains(strings.ToLower(n.Title+"\n"+n.Body), q) {
				continue
			}
			out = append(out, *n)
		}
		sort.Slice(out, func(i, j int) bool {
			if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
				return out[i].CreatedAt.After(out[j].CreatedAt)
			}
			return out[i].NoteID < out[j].NoteID
		})
		return paginate(out, req.PageRequest, b.now())
	})
}

func (b *Backend) UpdateNoteSync(ctx context.Context, req c.UpdateNoteSyncRequest) (c.Note, error) {
	return mutate(b, ctx, c.OpUpdateNoteSync, req.Mutation, req, func(p c.Principal) (c.Note, error) {
		n, ok := b.notes[req.NoteID]
		if !ok {
			return c.Note{}, notFound(c.KindNote, req.NoteID)
		}
		switch req.SyncState {
		case "pending", "indexed", "failed":
		default:
			return c.Note{}, c.Errorf(c.ErrInvalid, "sync_state must be pending, indexed or failed")
		}
		n.SyncState = req.SyncState
		if req.HiveRef != "" {
			n.HiveRef = req.HiveRef
		}
		return *n, nil
	})
}

func (b *Backend) GetLaunchContext(ctx context.Context, req c.ContextRequest) (c.LaunchContext, error) {
	return query(b, ctx, func(p c.Principal) (c.LaunchContext, error) {
		max := req.MaxBytes
		if max <= 0 {
			max = 8 << 10
		}
		if max > 64<<10 {
			max = 64 << 10
		}
		lc := c.LaunchContext{References: []c.ObjectRef{}, MemoryFreshness: &c.Freshness{ObservedAt: b.now(), Source: "mock", Reason: "mock backend: no Hive index"}}
		var sb strings.Builder
		pid := req.ProjectID
		if pid == nil && req.SessionID != "" {
			if s, ok := b.sessions[req.SessionID]; ok {
				pid = s.ProjectID
			} else {
				return c.LaunchContext{}, notFound(c.KindSession, req.SessionID)
			}
		}
		if pid != nil {
			pr, ok := b.projects[*pid]
			if !ok {
				return c.LaunchContext{}, notFound(c.KindProject, *pid)
			}
			sb.WriteString("# Project: " + pr.Name + "\n")
			if pr.Description != "" {
				sb.WriteString(pr.Description + "\n")
			}
			if d, ok := b.documents[pr.BriefDocumentID]; ok {
				if r, ok := b.revisions[d.HeadRevisionID]; ok {
					sb.WriteString("\n## Brief (" + r.RevisionID + ")\n" + r.Content + "\n")
					lc.BriefRevision = r.RevisionID
					lc.References = append(lc.References, ref(c.KindDocumentRevision, r.RevisionID))
				}
			}
		}
		var notes []*c.Note
		for _, n := range b.notes {
			if (pid != nil && eqStrPtr(n.ProjectID, pid)) || (req.SessionID != "" && n.SessionID == req.SessionID) {
				if n.Kind == c.NoteDecision || n.Kind == c.NoteHandoff {
					notes = append(notes, n)
				}
			}
		}
		sort.Slice(notes, func(i, j int) bool { return notes[i].CreatedAt.After(notes[j].CreatedAt) })
		for _, n := range notes {
			sb.WriteString("\n## " + string(n.Kind) + ": " + n.Title + " (" + n.NoteID + ")\n" + n.Body + "\n")
			lc.References = append(lc.References, ref(c.KindNote, n.NoteID))
		}
		text := sb.String()
		if len(text) > max {
			text = truncate(text, max-len("…"))
			lc.Truncated = true
		}
		lc.Text = text
		lc.Bytes = len(text)
		return lc, nil
	})
}
