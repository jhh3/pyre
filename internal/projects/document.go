package projects

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/projects/svcutil"
)

const defaultDocMediaType = "text/markdown"

// ContentDigest returns the canonical "sha256:<hex>" digest of text.
func ContentDigest(content string) contracts.Digest {
	sum := sha256.Sum256([]byte(content))
	return contracts.Digest("sha256:" + hex.EncodeToString(sum[:]))
}

const docCols = `document_id, project_id, title, slug, media_type, head_revision_id, head_seq, created_by, created_at, updated_at`

func scanDocument(sc interface{ Scan(...any) error }) (contracts.Document, error) {
	var d contracts.Document
	var proj sql.NullString
	var createdBy, createdAt, updatedAt string
	if err := sc.Scan(&d.DocumentID, &proj, &d.Title, &d.Slug, &d.MediaType, &d.HeadRevisionID, &d.HeadSeq, &createdBy, &createdAt, &updatedAt); err != nil {
		return d, err
	}
	d.ProjectID = svcutil.ScanProject(proj)
	_ = svcutil.FromJSON(createdBy, &d.CreatedBy)
	d.CreatedAt = svcutil.ParseTime(createdAt)
	d.UpdatedAt = svcutil.ParseTime(updatedAt)
	return d, nil
}

const revCols = `revision_id, document_id, seq, parent_revision_id, digest, size, author, created_at, summary, provenance`

func scanRevision(sc interface{ Scan(...any) error }, withContent bool) (contracts.DocumentRevision, error) {
	var r contracts.DocumentRevision
	var author, createdAt string
	var prov sql.NullString
	dest := []any{&r.RevisionID, &r.DocumentID, &r.Seq, &r.ParentRevisionID, &r.Digest, &r.Size, &author, &createdAt, &r.Summary, &prov}
	if withContent {
		dest = append(dest, &r.Content)
	}
	if err := sc.Scan(dest...); err != nil {
		return r, err
	}
	_ = svcutil.FromJSON(author, &r.Author)
	r.CreatedAt = svcutil.ParseTime(createdAt)
	if prov.Valid && prov.String != "" && prov.String != "null" {
		var p contracts.Provenance
		if svcutil.FromJSON(prov.String, &p) == nil {
			r.Provenance = &p
		}
	}
	return r, nil
}

func getDocumentQ(ctx context.Context, q queryer, id string) (contracts.Document, error) {
	d, err := scanDocument(q.QueryRowContext(ctx, `SELECT `+docCols+` FROM documents WHERE document_id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return d, contracts.Errorf(contracts.ErrNotFound, "document not found").WithRef(contracts.KindDocument, id)
	}
	return d, err
}

func getRevisionQ(ctx context.Context, q queryer, docID, revID string) (contracts.DocumentRevision, error) {
	r, err := scanRevision(q.QueryRowContext(ctx, `SELECT `+revCols+`, content FROM document_revisions WHERE revision_id=? AND document_id=?`, revID, docID), true)
	if errors.Is(err, sql.ErrNoRows) {
		return r, contracts.Errorf(contracts.ErrNotFound, "revision not found for document").WithRef(contracts.KindDocumentRevision, revID)
	}
	return r, err
}

// CreateDocument creates a document (in a project or standalone) with its
// first revision. Slugs are unique per project, and separately among
// standalone documents. An explicit slug that is taken returns ErrConflict;
// a slug derived from the title gets a numeric suffix instead.
func (s *Store) CreateDocument(ctx context.Context, req contracts.CreateDocumentRequest) (contracts.DocumentWithRevision, error) {
	var out contracts.DocumentWithRevision
	err := svcutil.Mutate(ctx, s.db, req.Mutation, contracts.OpCreateDocument, req, &out, func(tx *sql.Tx, who contracts.Principal) (any, error) {
		return s.insertDocument(ctx, tx, who, s.clock.Now(), req.ProjectID, req.Title, req.Slug, req.MediaType, req.Content, req.Summary, nil)
	})
	return out, err
}

func (s *Store) insertDocument(ctx context.Context, tx *sql.Tx, who contracts.Principal, now time.Time, projectID *string, title, slug, mediaType, content, summary string, prov *contracts.Provenance) (contracts.DocumentWithRevision, error) {
	var out contracts.DocumentWithRevision
	projectID = svcutil.CopyProject(projectID)
	title = strings.TrimSpace(title)
	if title == "" && slug != "" {
		title = slug
	}
	if err := validName(title); err != nil {
		return out, contracts.Errorf(contracts.ErrInvalid, "title: %s", contracts.AsError(err).Message)
	}
	mt, err := normalizeDocMediaType(mediaType)
	if err != nil {
		return out, err
	}
	if err := s.checkContent(content, summary); err != nil {
		return out, err
	}
	scope := ""
	if projectID != nil {
		if err := projectExistsQ(ctx, tx, *projectID); err != nil {
			return out, err
		}
		scope = *projectID
	}
	explicit := slug != ""
	if explicit {
		if !validSlug(slug) {
			return out, contracts.Errorf(contracts.ErrInvalid, "slug %q must be 1-64 chars of [a-z0-9-], starting with a letter or digit", slug)
		}
	} else {
		slug = Slugify(title)
	}
	doc := contracts.Document{
		DocumentID: contracts.NewID(contracts.PrefixDocument),
		ProjectID:  projectID,
		Title:      title,
		MediaType:  mt,
		HeadSeq:    1,
		CreatedBy:  who,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	rev := s.newRevision(doc.DocumentID, 1, "", content, summary, who, projectID, now)
	if prov != nil {
		rev.Provenance = prov
	}
	doc.HeadRevisionID = rev.RevisionID

	candidate := slug
	for i := 1; ; i++ {
		if i > 1 {
			suffix := "-" + strconv.Itoa(i)
			candidate = strings.TrimRight(svcutil.TruncateUTF8(slug, 64-len(suffix)), "-") + suffix
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO documents(document_id, project_id, scope, title, slug, media_type, head_revision_id, head_seq, created_by, created_at, updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
			doc.DocumentID, svcutil.NullProject(projectID), scope, doc.Title, candidate, doc.MediaType, doc.HeadRevisionID, doc.HeadSeq, svcutil.JSON(who), svcutil.Time(now), svcutil.Time(now))
		if err == nil {
			break
		}
		if !svcutil.IsUniqueViolation(err) {
			return out, err
		}
		if explicit || i >= 100 {
			var existing string
			_ = tx.QueryRowContext(ctx, `SELECT document_id FROM documents WHERE scope=? AND slug=?`, scope, candidate).Scan(&existing)
			return out, contracts.Errorf(contracts.ErrConflict, "slug %q already exists in this scope", candidate).
				WithRef(contracts.KindDocument, existing).WithDetail("document_id", existing)
		}
	}
	doc.Slug = candidate
	if err := insertRevision(ctx, tx, rev); err != nil {
		return out, err
	}
	if err := svcutil.Emit(ctx, tx, s.sink, now, contracts.EvDocumentRevised, contracts.ObjectRef{Kind: contracts.KindDocument, ID: doc.DocumentID}, projectID, sessionOf(who), revisionPayload(rev)); err != nil {
		return out, err
	}
	return contracts.DocumentWithRevision{Document: doc, Revision: rev}, nil
}

func (s *Store) checkContent(content, summary string) error {
	if len(content) > s.maxDoc {
		return contracts.Errorf(contracts.ErrPayloadTooLarge, "document content is %d bytes; limit is %d", len(content), s.maxDoc).WithDetail("max_bytes", s.maxDoc)
	}
	if !utf8.ValidString(content) {
		return contracts.Errorf(contracts.ErrInvalid, "document content must be valid UTF-8 text; publish binary files as artifacts")
	}
	if len(summary) > 1024 {
		return contracts.Errorf(contracts.ErrInvalid, "summary exceeds 1024 bytes")
	}
	return nil
}

func (s *Store) newRevision(docID string, seq int64, parent, content, summary string, who contracts.Principal, projectID *string, now time.Time) contracts.DocumentRevision {
	rev := contracts.DocumentRevision{
		RevisionID:       contracts.NewID(contracts.PrefixRevision),
		DocumentID:       docID,
		Seq:              seq,
		ParentRevisionID: parent,
		Digest:           ContentDigest(content),
		Size:             int64(len(content)),
		Author:           who,
		CreatedAt:        now,
		Summary:          summary,
		Content:          content,
	}
	if who.Kind == contracts.PrincipalSession {
		rev.Provenance = &contracts.Provenance{SessionID: who.ID, ProjectID: svcutil.CopyProject(projectID)}
	}
	return rev
}

// revisionPayload is the pinned document.revised payload: the revision
// without its content.
func revisionPayload(r contracts.DocumentRevision) contracts.DocumentRevision {
	r.Content = ""
	return r
}

func insertRevision(ctx context.Context, tx *sql.Tx, r contracts.DocumentRevision) error {
	var prov any
	if r.Provenance != nil {
		prov = svcutil.JSON(r.Provenance)
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO document_revisions(`+revCols+`, content) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		r.RevisionID, r.DocumentID, r.Seq, r.ParentRevisionID, string(r.Digest), r.Size, svcutil.JSON(r.Author), svcutil.Time(r.CreatedAt), r.Summary, prov, r.Content)
	return err
}

// GetDocument returns a document with one revision (head by default). The
// document is addressed by DocumentID, or by ProjectID+Slug (an empty
// ProjectID with a Slug addresses a standalone document).
func (s *Store) GetDocument(ctx context.Context, req contracts.GetDocumentRequest) (contracts.DocumentWithRevision, error) {
	var out contracts.DocumentWithRevision
	var doc contracts.Document
	var err error
	switch {
	case req.DocumentID != "":
		doc, err = getDocumentQ(ctx, s.db, req.DocumentID)
	case req.Slug != "":
		doc, err = scanDocument(s.db.QueryRowContext(ctx, `SELECT `+docCols+` FROM documents WHERE scope=? AND slug=?`, req.ProjectID, req.Slug))
		if errors.Is(err, sql.ErrNoRows) {
			err = contracts.Errorf(contracts.ErrNotFound, "no document with slug %q in this scope", req.Slug)
		}
	default:
		return out, contracts.Errorf(contracts.ErrInvalid, "document_id or slug is required")
	}
	if err != nil {
		return out, err
	}
	revID := req.RevisionID
	if revID == "" {
		revID = doc.HeadRevisionID
	}
	rev, err := getRevisionQ(ctx, s.db, doc.DocumentID, revID)
	if err != nil {
		return out, err
	}
	return contracts.DocumentWithRevision{Document: doc, Revision: rev}, nil
}

// ListDocuments lists documents newest first. ProjectID: nil = all,
// "" = standalone only, otherwise that project.
func (s *Store) ListDocuments(ctx context.Context, req contracts.ListDocumentsRequest) (contracts.Page[contracts.Document], error) {
	page := contracts.Page[contracts.Document]{Items: []contracts.Document{}, SnapshotAt: s.clock.Now()}
	after, err := svcutil.DecodeCursor(req.Cursor)
	if err != nil {
		return page, err
	}
	where := []string{"1=1"}
	var args []any
	if frag, a := svcutil.ProjectFilter("project_id", req.ProjectID); frag != "" {
		where = append(where, frag)
		args = append(args, a...)
	}
	if after != "" {
		where = append(where, "document_id < ?")
		args = append(args, after)
	}
	limit := svcutil.Limit(req.Limit)
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, `SELECT `+docCols+` FROM documents WHERE `+strings.Join(where, " AND ")+` ORDER BY document_id DESC LIMIT ?`, args...)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		d, err := scanDocument(rows)
		if err != nil {
			return page, err
		}
		page.Items = append(page.Items, d)
	}
	if err := rows.Err(); err != nil {
		return page, err
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		page.NextCursor = svcutil.EncodeCursor(page.Items[limit-1].DocumentID)
	}
	return page, nil
}

// ListRevisions lists a document's revision history newest first, without
// content.
func (s *Store) ListRevisions(ctx context.Context, req contracts.ListRevisionsRequest) (contracts.Page[contracts.DocumentRevision], error) {
	page := contracts.Page[contracts.DocumentRevision]{Items: []contracts.DocumentRevision{}, SnapshotAt: s.clock.Now()}
	if _, err := getDocumentQ(ctx, s.db, req.DocumentID); err != nil {
		return page, err
	}
	after, err := svcutil.DecodeCursor(req.Cursor)
	if err != nil {
		return page, err
	}
	args := []any{req.DocumentID}
	where := "document_id=?"
	if after != "" {
		seq, err := strconv.ParseInt(after, 10, 64)
		if err != nil {
			return page, contracts.Errorf(contracts.ErrInvalid, "malformed cursor")
		}
		where += " AND seq < ?"
		args = append(args, seq)
	}
	limit := svcutil.Limit(req.Limit)
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, `SELECT `+revCols+` FROM document_revisions WHERE `+where+` ORDER BY seq DESC LIMIT ?`, args...)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		r, err := scanRevision(rows, false)
		if err != nil {
			return page, err
		}
		page.Items = append(page.Items, r)
	}
	if err := rows.Err(); err != nil {
		return page, err
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		page.NextCursor = svcutil.EncodeCursor(strconv.FormatInt(page.Items[limit-1].Seq, 10))
	}
	return page, nil
}

// ProposeRevision commits a successor revision iff the document head still
// equals BaseRevisionID. Otherwise the caller's content is stored as a
// conflicted DocumentProposal (never lost) and ErrConflict is returned with
// details {"current_head": <rev id>, "proposal_id": <id>}. Committing
// content identical to an earlier conflicted proposal marks it applied.
func (s *Store) ProposeRevision(ctx context.Context, req contracts.ProposeRevisionRequest) (contracts.DocumentWithRevision, error) {
	var out contracts.DocumentWithRevision
	err := svcutil.Mutate(ctx, s.db, req.Mutation, contracts.OpProposeRevision, req, &out, func(tx *sql.Tx, who contracts.Principal) (any, error) {
		return s.proposeTx(ctx, tx, who, req.DocumentID, req.BaseRevisionID, req.Content, req.Summary, nil)
	})
	return out, err
}

// proposeTx is the CAS core of ProposeRevision (also used by PromoteNote).
// A conflict stores the proposal and returns svcutil.CommitWith(ErrConflict).
// prov, when non-nil, overrides the revision provenance.
func (s *Store) proposeTx(ctx context.Context, tx *sql.Tx, who contracts.Principal, docID, baseID, content, summary string, prov *contracts.Provenance) (contracts.DocumentWithRevision, error) {
	doc, err := getDocumentQ(ctx, tx, docID)
	if err != nil {
		return contracts.DocumentWithRevision{}, err
	}
	if err := s.checkContent(content, summary); err != nil {
		return contracts.DocumentWithRevision{}, err
	}
	if baseID == "" {
		return contracts.DocumentWithRevision{}, contracts.Errorf(contracts.ErrInvalid, "base_revision_id is required")
	}
	var one int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM document_revisions WHERE revision_id=? AND document_id=?`, baseID, doc.DocumentID).Scan(&one); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return contracts.DocumentWithRevision{}, contracts.Errorf(contracts.ErrInvalid, "base_revision_id is not a revision of this document").WithRef(contracts.KindDocumentRevision, baseID)
		}
		return contracts.DocumentWithRevision{}, err
	}
	now := s.clock.Now()
	docRef := contracts.ObjectRef{Kind: contracts.KindDocument, ID: doc.DocumentID}
	digest := ContentDigest(content)
	if doc.HeadRevisionID != baseID {
		prp := contracts.DocumentProposal{
			ProposalID:     contracts.NewID(contracts.PrefixProposal),
			DocumentID:     doc.DocumentID,
			BaseRevisionID: baseID,
			Content:        content,
			Author:         who,
			CreatedAt:      now,
			State:          contracts.ProposalConflicted,
			HeadAtConflict: doc.HeadRevisionID,
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO document_proposals(proposal_id, document_id, base_revision_id, content, digest, summary, author, created_at, state, head_at_conflict) VALUES(?,?,?,?,?,?,?,?,?,?)`,
			prp.ProposalID, prp.DocumentID, prp.BaseRevisionID, prp.Content, string(digest), summary, svcutil.JSON(who), svcutil.Time(now), string(prp.State), prp.HeadAtConflict); err != nil {
			return contracts.DocumentWithRevision{}, err
		}
		payload := prp
		payload.Content = "" // pinned v1 shape: DocumentProposal without content
		if err := svcutil.Emit(ctx, tx, s.sink, now, contracts.EvDocumentConflict, docRef, doc.ProjectID, sessionOf(who), payload); err != nil {
			return contracts.DocumentWithRevision{}, err
		}
		return contracts.DocumentWithRevision{}, svcutil.CommitWith(contracts.Errorf(contracts.ErrConflict, "document head moved").
			WithRef(contracts.KindDocument, doc.DocumentID).
			WithDetail("current_head", doc.HeadRevisionID).
			WithDetail("proposal_id", prp.ProposalID))
	}
	rev := s.newRevision(doc.DocumentID, doc.HeadSeq+1, doc.HeadRevisionID, content, summary, who, doc.ProjectID, now)
	if prov != nil {
		rev.Provenance = prov
	}
	if err := insertRevision(ctx, tx, rev); err != nil {
		return contracts.DocumentWithRevision{}, err
	}
	res, err := tx.ExecContext(ctx, `UPDATE documents SET head_revision_id=?, head_seq=?, updated_at=? WHERE document_id=? AND head_revision_id=?`,
		rev.RevisionID, rev.Seq, svcutil.Time(now), doc.DocumentID, baseID)
	if err != nil {
		return contracts.DocumentWithRevision{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return contracts.DocumentWithRevision{}, fmt.Errorf("projects: document %s head changed inside transaction", doc.DocumentID)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE document_proposals SET state=? WHERE document_id=? AND state=? AND digest=?`,
		string(contracts.ProposalApplied), doc.DocumentID, string(contracts.ProposalConflicted), string(digest)); err != nil {
		return contracts.DocumentWithRevision{}, err
	}
	doc.HeadRevisionID, doc.HeadSeq, doc.UpdatedAt = rev.RevisionID, rev.Seq, now
	if err := svcutil.Emit(ctx, tx, s.sink, now, contracts.EvDocumentRevised, docRef, doc.ProjectID, sessionOf(who), revisionPayload(rev)); err != nil {
		return contracts.DocumentWithRevision{}, err
	}
	return contracts.DocumentWithRevision{Document: doc, Revision: rev}, nil
}

func getProposalQ(ctx context.Context, q queryer, id string) (contracts.DocumentProposal, error) {
	var p contracts.DocumentProposal
	var author, createdAt, state string
	err := q.QueryRowContext(ctx, `SELECT proposal_id, document_id, base_revision_id, content, author, created_at, state, head_at_conflict FROM document_proposals WHERE proposal_id=?`, id).
		Scan(&p.ProposalID, &p.DocumentID, &p.BaseRevisionID, &p.Content, &author, &createdAt, &state, &p.HeadAtConflict)
	if errors.Is(err, sql.ErrNoRows) {
		return p, contracts.Errorf(contracts.ErrNotFound, "proposal not found").WithRef(contracts.KindProposal, id)
	}
	if err != nil {
		return p, err
	}
	_ = svcutil.FromJSON(author, &p.Author)
	p.CreatedAt = svcutil.ParseTime(createdAt)
	p.State = contracts.ProposalState(state)
	return p, nil
}

// DiscardProposal marks a conflicted proposal discarded. Its content is
// kept for audit. Discarding an already-discarded proposal returns it
// unchanged; an applied proposal cannot be discarded (ErrConflict).
//
// No event is emitted: the pinned v1 table has no proposal-state kind, and
// re-emitting document.conflict would re-raise conflict attention.
func (s *Store) DiscardProposal(ctx context.Context, req contracts.DiscardProposalRequest) (contracts.DocumentProposal, error) {
	var out contracts.DocumentProposal
	err := svcutil.Mutate(ctx, s.db, req.Mutation, contracts.OpDiscardProposal, req, &out, func(tx *sql.Tx, who contracts.Principal) (any, error) {
		p, err := getProposalQ(ctx, tx, req.ProposalID)
		if err != nil {
			return nil, err
		}
		switch p.State {
		case contracts.ProposalDiscarded:
			return p, nil
		case contracts.ProposalApplied:
			return nil, contracts.Errorf(contracts.ErrConflict, "proposal was already applied").WithRef(contracts.KindProposal, p.ProposalID)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE document_proposals SET state=? WHERE proposal_id=? AND state=?`,
			string(contracts.ProposalDiscarded), p.ProposalID, string(contracts.ProposalConflicted)); err != nil {
			return nil, err
		}
		p.State = contracts.ProposalDiscarded
		return p, nil
	})
	return out, err
}

// ListProposals lists a document's preserved proposals newest first,
// optionally filtered by state.
func (s *Store) ListProposals(ctx context.Context, req contracts.ListProposalsRequest) (contracts.Page[contracts.DocumentProposal], error) {
	page := contracts.Page[contracts.DocumentProposal]{Items: []contracts.DocumentProposal{}, SnapshotAt: s.clock.Now()}
	if _, err := getDocumentQ(ctx, s.db, req.DocumentID); err != nil {
		return page, err
	}
	after, err := svcutil.DecodeCursor(req.Cursor)
	if err != nil {
		return page, err
	}
	where := []string{"document_id=?"}
	args := []any{req.DocumentID}
	if len(req.States) > 0 {
		ph := make([]string, len(req.States))
		for i, st := range req.States {
			ph[i] = "?"
			args = append(args, string(st))
		}
		where = append(where, "state IN ("+strings.Join(ph, ",")+")")
	}
	if after != "" {
		where = append(where, "proposal_id < ?")
		args = append(args, after)
	}
	limit := svcutil.Limit(req.Limit)
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, `SELECT proposal_id, document_id, base_revision_id, content, author, created_at, state, head_at_conflict FROM document_proposals WHERE `+strings.Join(where, " AND ")+` ORDER BY proposal_id DESC LIMIT ?`, args...)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		var p contracts.DocumentProposal
		var author, createdAt, state string
		if err := rows.Scan(&p.ProposalID, &p.DocumentID, &p.BaseRevisionID, &p.Content, &author, &createdAt, &state, &p.HeadAtConflict); err != nil {
			return page, err
		}
		_ = svcutil.FromJSON(author, &p.Author)
		p.CreatedAt = svcutil.ParseTime(createdAt)
		p.State = contracts.ProposalState(state)
		page.Items = append(page.Items, p)
	}
	if err := rows.Err(); err != nil {
		return page, err
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		page.NextCursor = svcutil.EncodeCursor(page.Items[limit-1].ProposalID)
	}
	return page, nil
}

// ---- slugs and media types ----

func validSlug(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for i, c := range s {
		ok := c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || (c == '-' && i > 0)
		if !ok {
			return false
		}
	}
	return true
}

// Slugify derives a slug from a title ("Design Notes!" -> "design-notes").
func Slugify(title string) string {
	var b strings.Builder
	dash := false
	for _, c := range strings.ToLower(title) {
		switch {
		case c >= 'a' && c <= 'z' || c >= '0' && c <= '9':
			b.WriteRune(c)
			dash = false
		case b.Len() > 0 && !dash:
			b.WriteByte('-')
			dash = true
		}
		if b.Len() >= 64 {
			break
		}
	}
	s := strings.TrimRight(svcutil.TruncateUTF8(b.String(), 64), "-")
	if s == "" {
		s = "doc"
	}
	return s
}

func normalizeDocMediaType(mt string) (string, error) {
	mt = strings.TrimSpace(mt)
	if mt == "" {
		return defaultDocMediaType, nil
	}
	base, params, err := mime.ParseMediaType(mt)
	if err != nil {
		return "", contracts.Errorf(contracts.ErrInvalid, "media_type %q is not a valid media type", mt)
	}
	switch {
	case strings.HasPrefix(base, "text/"),
		base == "application/json", base == "application/yaml", base == "application/x-yaml",
		base == "application/toml", base == "application/xml":
	default:
		return "", contracts.Errorf(contracts.ErrInvalid, "documents hold text; media_type %q should be published as an artifact", base)
	}
	return mime.FormatMediaType(base, params), nil
}
