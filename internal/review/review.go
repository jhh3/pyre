// Package review implements contracts.ReviewAPI: comments attached to an
// exact document revision or artifact version, with optional anchors,
// replies, session mentions and resolve/unresolve, stored in the hub's
// SQLite database (migration namespace "review", plus
// sqlitex.OperationsMigration).
//
// # Composition (hub, WP05)
//
//	rev, err := review.New(ctx, db, sink, projStore, artStore, review.Options{Clock: clock})
//
// projStore (*projects.Store) satisfies RevisionLookup and artStore
// (*artifacts.Store) satisfies VersionLookup; they validate that a target
// exists and supply its parent document/artifact and project. Mutations run
// in sqlitex.Idempotent with the ctx principal and emit comment.created /
// comment.resolved through sink.AppendTx in the same transaction; both
// payloads are the contracts.ReviewComment (pinned v1 shape), whose
// Mentions (session IDs or "owner") drive attention/notifications.
//
// Comments stay attached to the version they reviewed: a newer artifact
// version or document revision never moves them. ListComments can list by
// exact target, by artifact (all versions), by document (all revisions), by
// project (nil = all, "" = standalone only) and unresolved only.
package review

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/projects/svcutil"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

// RevisionLookup resolves a document revision to its document and project.
type RevisionLookup interface {
	LookupRevision(ctx context.Context, revisionID string) (documentID string, projectID *string, err error)
}

// VersionLookup resolves an artifact version to its artifact and project.
type VersionLookup interface {
	LookupVersion(ctx context.Context, versionID string) (artifactID string, projectID *string, err error)
}

// Migrations returns this domain's schema steps.
func Migrations() []sqlitex.Migration {
	return []sqlitex.Migration{sqlitex.OperationsMigration, migrationV1}
}

var migrationV1 = sqlitex.Migration{Namespace: "review", Version: 1, Name: "review_comments", SQL: `
CREATE TABLE review_comments (
	comment_id TEXT PRIMARY KEY,
	target_kind TEXT NOT NULL,
	target_id TEXT NOT NULL,
	parent_kind TEXT NOT NULL,      -- document | artifact
	parent_id TEXT NOT NULL,
	project_id TEXT,
	anchor TEXT,
	author TEXT NOT NULL,
	body TEXT NOT NULL,
	in_reply_to TEXT NOT NULL DEFAULT '',
	mentions TEXT NOT NULL DEFAULT '[]',
	resolved INTEGER NOT NULL DEFAULT 0,
	resolved_by TEXT,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);
CREATE INDEX review_comments_target ON review_comments(target_kind, target_id, comment_id);
CREATE INDEX review_comments_parent ON review_comments(parent_kind, parent_id, comment_id);
CREATE INDEX review_comments_project ON review_comments(project_id, comment_id);
`}

// MentionOwner is the mention value that asks the human owner to look.
const MentionOwner = "owner"

// Limits.
const (
	MaxBodyBytes   = 64 << 10
	MaxAnchorBytes = 1024
	MaxMentions    = 50
)

// Options configures a Store.
type Options struct {
	Clock contracts.Clock
}

// Store implements contracts.ReviewAPI.
type Store struct {
	db    *sql.DB
	sink  contracts.EventSink
	clock contracts.Clock
	docs  RevisionLookup
	arts  VersionLookup
}

var _ contracts.ReviewAPI = (*Store)(nil)

// New applies migrations and returns a Store.
func New(ctx context.Context, db *sql.DB, sink contracts.EventSink, docs RevisionLookup, arts VersionLookup, opts Options) (*Store, error) {
	if db == nil || docs == nil || arts == nil {
		return nil, errors.New("review: db, revision lookup and version lookup are required")
	}
	if err := sqlitex.Migrate(ctx, db, Migrations()...); err != nil {
		return nil, err
	}
	s := &Store{db: db, sink: sink, clock: opts.Clock, docs: docs, arts: arts}
	if s.clock == nil {
		s.clock = contracts.SystemClock{}
	}
	return s, nil
}

type target struct {
	parentKind contracts.ObjectKind
	parentID   string
	projectID  *string
}

func (s *Store) resolveTarget(ctx context.Context, ref contracts.ObjectRef) (target, error) {
	switch ref.Kind {
	case contracts.KindDocumentRevision:
		id, proj, err := s.docs.LookupRevision(ctx, ref.ID)
		return target{contracts.KindDocument, id, proj}, err
	case contracts.KindArtifactVersion:
		id, proj, err := s.arts.LookupVersion(ctx, ref.ID)
		return target{contracts.KindArtifact, id, proj}, err
	}
	return target{}, contracts.Errorf(contracts.ErrInvalid, "comment target must be a document_revision or artifact_version")
}

const commentCols = `comment_id, target_kind, target_id, project_id, anchor, author, body, in_reply_to, mentions, resolved, resolved_by, created_at, updated_at`

func scanComment(sc interface{ Scan(...any) error }) (contracts.ReviewComment, error) {
	var c contracts.ReviewComment
	var kind, author, mentions, createdAt, updatedAt string
	var proj, anchor, resolvedBy sql.NullString
	var resolved int
	if err := sc.Scan(&c.CommentID, &kind, &c.Target.ID, &proj, &anchor, &author, &c.Body, &c.InReplyTo, &mentions, &resolved, &resolvedBy, &createdAt, &updatedAt); err != nil {
		return c, err
	}
	c.Target.Kind = contracts.ObjectKind(kind)
	c.ProjectID = svcutil.ScanProject(proj)
	if anchor.Valid && anchor.String != "" {
		var a contracts.Anchor
		if svcutil.FromJSON(anchor.String, &a) == nil {
			c.Anchor = &a
		}
	}
	_ = svcutil.FromJSON(author, &c.Author)
	_ = svcutil.FromJSON(mentions, &c.Mentions)
	if len(c.Mentions) == 0 {
		c.Mentions = nil
	}
	c.Resolved = resolved != 0
	if resolvedBy.Valid && resolvedBy.String != "" {
		var p contracts.Principal
		if svcutil.FromJSON(resolvedBy.String, &p) == nil {
			c.ResolvedBy = &p
		}
	}
	c.CreatedAt = svcutil.ParseTime(createdAt)
	c.UpdatedAt = svcutil.ParseTime(updatedAt)
	return c, nil
}

type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func getCommentQ(ctx context.Context, q queryer, id string) (contracts.ReviewComment, error) {
	c, err := scanComment(q.QueryRowContext(ctx, `SELECT `+commentCols+` FROM review_comments WHERE comment_id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return c, contracts.Errorf(contracts.ErrNotFound, "comment not found").WithRef(contracts.KindComment, id)
	}
	return c, err
}

// GetComment returns a comment by ID (deep links, notifications).
func (s *Store) GetComment(ctx context.Context, req contracts.GetRequest) (contracts.ReviewComment, error) {
	return getCommentQ(ctx, s.db, req.ID)
}

var anchorKinds = map[string]bool{"line": true, "range": true, "quote": true, "page": true, "region": true}

// CreateComment attaches a comment to an exact, existing document revision
// or artifact version. A reply (InReplyTo) must target the same version as
// its parent. Mentions are session IDs to notify.
func (s *Store) CreateComment(ctx context.Context, req contracts.CreateCommentRequest) (contracts.ReviewComment, error) {
	var out contracts.ReviewComment
	if _, err := svcutil.RequirePrincipal(ctx); err != nil {
		return out, err
	}
	body := strings.TrimSpace(req.Body)
	if body == "" {
		return out, contracts.Errorf(contracts.ErrInvalid, "comment body is required")
	}
	if len(req.Body) > MaxBodyBytes {
		return out, contracts.Errorf(contracts.ErrPayloadTooLarge, "comment body exceeds %d bytes", MaxBodyBytes).WithDetail("max_bytes", MaxBodyBytes)
	}
	if !utf8.ValidString(req.Body) {
		return out, contracts.Errorf(contracts.ErrInvalid, "comment body must be valid UTF-8")
	}
	var anchor *contracts.Anchor
	if req.Anchor != nil {
		a := *req.Anchor
		if !anchorKinds[a.Kind] {
			return out, contracts.Errorf(contracts.ErrInvalid, "anchor kind must be line, range, quote, page or region")
		}
		if a.Value == "" || len(a.Value) > MaxAnchorBytes {
			return out, contracts.Errorf(contracts.ErrInvalid, "anchor value must be 1-%d bytes", MaxAnchorBytes)
		}
		anchor = &a
	}
	if len(req.Mentions) > MaxMentions {
		return out, contracts.Errorf(contracts.ErrInvalid, "too many mentions (max %d)", MaxMentions)
	}
	var mentions []string
	seen := map[string]bool{}
	for _, m := range req.Mentions {
		if m != MentionOwner && !contracts.ValidID(m, contracts.PrefixSession) {
			return out, contracts.Errorf(contracts.ErrInvalid, "mention %q is not a session ID or %q", m, MentionOwner)
		}
		if !seen[m] {
			seen[m] = true
			mentions = append(mentions, m)
		}
	}
	// Targets are immutable and never deleted, so resolving them before the
	// write transaction is safe.
	tgt, err := s.resolveTarget(ctx, req.Target)
	if err != nil {
		return out, err
	}
	err = svcutil.Mutate(ctx, s.db, req.Mutation, contracts.OpCreateComment, req, &out, func(tx *sql.Tx, who contracts.Principal) (any, error) {
		if req.InReplyTo != "" {
			parent, err := getCommentQ(ctx, tx, req.InReplyTo)
			if err != nil {
				return nil, err
			}
			if parent.Target != req.Target {
				return nil, contracts.Errorf(contracts.ErrInvalid, "a reply must target the same version as its parent").WithRef(contracts.KindComment, parent.CommentID)
			}
		}
		now := s.clock.Now()
		c := contracts.ReviewComment{
			CommentID: contracts.NewID(contracts.PrefixComment),
			Target:    req.Target,
			Anchor:    anchor,
			Author:    who,
			Body:      req.Body,
			InReplyTo: req.InReplyTo,
			Mentions:  mentions,
			CreatedAt: now,
			UpdatedAt: now,
			ProjectID: svcutil.CopyProject(tgt.projectID),
		}
		var anchorJSON any
		if anchor != nil {
			anchorJSON = svcutil.JSON(anchor)
		}
		ms := mentions
		if ms == nil {
			ms = []string{}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO review_comments(comment_id, target_kind, target_id, parent_kind, parent_id, project_id, anchor, author, body, in_reply_to, mentions, resolved, created_at, updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,0,?,?)`,
			c.CommentID, string(c.Target.Kind), c.Target.ID, string(tgt.parentKind), tgt.parentID, svcutil.NullProject(c.ProjectID), anchorJSON,
			svcutil.JSON(who), c.Body, c.InReplyTo, svcutil.JSON(ms), svcutil.Time(now), svcutil.Time(now)); err != nil {
			return nil, err
		}
		if err := svcutil.Emit(ctx, tx, s.sink, now, contracts.EvCommentCreated, contracts.ObjectRef{Kind: contracts.KindComment, ID: c.CommentID}, c.ProjectID, sessionOf(who), c); err != nil {
			return nil, err
		}
		return c, nil
	})
	return out, err
}

// ResolveComment sets or clears a comment's resolved state. Setting the
// state it already has returns the comment unchanged (no event).
func (s *Store) ResolveComment(ctx context.Context, req contracts.ResolveCommentRequest) (contracts.ReviewComment, error) {
	var out contracts.ReviewComment
	err := svcutil.Mutate(ctx, s.db, req.Mutation, contracts.OpResolveComment, req, &out, func(tx *sql.Tx, who contracts.Principal) (any, error) {
		c, err := getCommentQ(ctx, tx, req.CommentID)
		if err != nil {
			return nil, err
		}
		if c.Resolved == req.Resolved {
			return c, nil
		}
		now := s.clock.Now()
		c.Resolved, c.UpdatedAt = req.Resolved, now
		var by any
		c.ResolvedBy = nil
		if req.Resolved {
			p := who
			c.ResolvedBy = &p
			by = svcutil.JSON(who)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE review_comments SET resolved=?, resolved_by=?, updated_at=? WHERE comment_id=?`,
			boolInt(req.Resolved), by, svcutil.Time(now), c.CommentID); err != nil {
			return nil, err
		}
		if err := svcutil.Emit(ctx, tx, s.sink, now, contracts.EvCommentResolved, contracts.ObjectRef{Kind: contracts.KindComment, ID: c.CommentID}, c.ProjectID, sessionOf(who), c); err != nil {
			return nil, err
		}
		return c, nil
	})
	return out, err
}

// ListComments lists comments oldest first (thread order). Filters combine:
// Target (exact version), ArtifactID (all versions), DocumentID (all
// revisions), ProjectID (nil = all, "" = standalone only), Unresolved.
func (s *Store) ListComments(ctx context.Context, req contracts.ListCommentsRequest) (contracts.Page[contracts.ReviewComment], error) {
	page := contracts.Page[contracts.ReviewComment]{Items: []contracts.ReviewComment{}, SnapshotAt: s.clock.Now()}
	after, err := svcutil.DecodeCursor(req.Cursor)
	if err != nil {
		return page, err
	}
	where := []string{"1=1"}
	var args []any
	if req.Target != nil {
		where = append(where, "target_kind=? AND target_id=?")
		args = append(args, string(req.Target.Kind), req.Target.ID)
	}
	if req.ArtifactID != "" {
		where = append(where, "parent_kind='artifact' AND parent_id=?")
		args = append(args, req.ArtifactID)
	}
	if req.DocumentID != "" {
		where = append(where, "parent_kind='document' AND parent_id=?")
		args = append(args, req.DocumentID)
	}
	if frag, a := svcutil.ProjectFilter("project_id", req.ProjectID); frag != "" {
		where = append(where, frag)
		args = append(args, a...)
	}
	if req.Unresolved {
		where = append(where, "resolved=0")
	}
	if after != "" {
		where = append(where, "comment_id > ?")
		args = append(args, after)
	}
	limit := svcutil.Limit(req.Limit)
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, `SELECT `+commentCols+` FROM review_comments WHERE `+strings.Join(where, " AND ")+` ORDER BY comment_id ASC LIMIT ?`, args...)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		c, err := scanComment(rows)
		if err != nil {
			return page, err
		}
		page.Items = append(page.Items, c)
	}
	if err := rows.Err(); err != nil {
		return page, err
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		page.NextCursor = svcutil.EncodeCursor(page.Items[limit-1].CommentID)
	}
	return page, nil
}

func sessionOf(p contracts.Principal) string {
	if p.Kind == contracts.PrincipalSession {
		return p.ID
	}
	return ""
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
