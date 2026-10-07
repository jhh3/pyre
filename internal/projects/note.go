package projects

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/projects/svcutil"
)

// Note sync states recorded by the Hive bridge.
const (
	SyncPending = "pending"
	SyncIndexed = "indexed"
	SyncFailed  = "failed"
)

const noteCols = `note_id, kind, project_id, session_id, title, body, author, created_at, provenance, hive_ref, sync_state, promoted_to`

func scanNote(sc interface{ Scan(...any) error }) (contracts.Note, error) {
	var n contracts.Note
	var kind, author, createdAt, prov string
	var proj sql.NullString
	if err := sc.Scan(&n.NoteID, &kind, &proj, &n.SessionID, &n.Title, &n.Body, &author, &createdAt, &prov, &n.HiveRef, &n.SyncState, &n.PromotedTo); err != nil {
		return n, err
	}
	n.Kind = contracts.NoteKind(kind)
	n.ProjectID = svcutil.ScanProject(proj)
	_ = svcutil.FromJSON(author, &n.Author)
	_ = svcutil.FromJSON(prov, &n.Provenance)
	n.CreatedAt = svcutil.ParseTime(createdAt)
	return n, nil
}

func getNoteQ(ctx context.Context, q queryer, id string) (contracts.Note, error) {
	n, err := scanNote(q.QueryRowContext(ctx, `SELECT `+noteCols+` FROM notes WHERE note_id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return n, contracts.Errorf(contracts.ErrNotFound, "note not found").WithRef(contracts.KindNote, id)
	}
	return n, err
}

// CreateNote publishes a note, handoff or decision. Provenance is derived
// from the authenticated principal: a session principal may only attribute
// notes to itself. The note starts in sync_state "pending" for the Hive
// bridge. Notes are memory, not authoritative project documents.
func (s *Store) CreateNote(ctx context.Context, req contracts.CreateNoteRequest) (contracts.Note, error) {
	var out contracts.Note
	err := svcutil.Mutate(ctx, s.db, req.Mutation, contracts.OpCreateNote, req, &out, func(tx *sql.Tx, who contracts.Principal) (any, error) {
		switch req.Kind {
		case contracts.NoteNote, contracts.NoteHandoff, contracts.NoteDecision:
		default:
			return nil, contracts.Errorf(contracts.ErrInvalid, "kind must be note, handoff or decision")
		}
		title := strings.TrimSpace(req.Title)
		if err := validName(title); err != nil {
			return nil, contracts.Errorf(contracts.ErrInvalid, "title: %s", contracts.AsError(err).Message)
		}
		if len(req.Body) > s.maxNote {
			return nil, contracts.Errorf(contracts.ErrPayloadTooLarge, "note body is %d bytes; limit is %d", len(req.Body), s.maxNote).WithDetail("max_bytes", s.maxNote)
		}
		if !utf8.ValidString(req.Body) {
			return nil, contracts.Errorf(contracts.ErrInvalid, "note body must be valid UTF-8")
		}
		sessionID := req.SessionID
		if who.Kind == contracts.PrincipalSession {
			if sessionID != "" && sessionID != who.ID {
				return nil, contracts.Errorf(contracts.ErrForbidden, "a session may only publish notes as itself")
			}
			sessionID = who.ID
		}
		if sessionID != "" && !contracts.ValidID(sessionID, contracts.PrefixSession) {
			return nil, contracts.Errorf(contracts.ErrInvalid, "session_id %q is not a session ID", sessionID)
		}
		projectID := svcutil.CopyProject(req.ProjectID)
		if projectID != nil {
			if err := projectExistsQ(ctx, tx, *projectID); err != nil {
				return nil, err
			}
		}
		now := s.clock.Now()
		n := contracts.Note{
			NoteID:     contracts.NewID(contracts.PrefixNote),
			Kind:       req.Kind,
			ProjectID:  projectID,
			SessionID:  sessionID,
			Title:      title,
			Body:       req.Body,
			Author:     who,
			CreatedAt:  now,
			Provenance: contracts.Provenance{SessionID: sessionID, ProjectID: svcutil.CopyProject(projectID)},
			SyncState:  SyncPending,
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO notes(`+noteCols+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
			n.NoteID, string(n.Kind), svcutil.NullProject(projectID), n.SessionID, n.Title, n.Body, svcutil.JSON(who), svcutil.Time(now), svcutil.JSON(n.Provenance), "", n.SyncState, ""); err != nil {
			return nil, err
		}
		payload := n
		payload.Body = "" // pinned v1 shape: Note without body
		if err := svcutil.Emit(ctx, tx, s.sink, now, contracts.EvNoteCreated, contracts.ObjectRef{Kind: contracts.KindNote, ID: n.NoteID}, projectID, sessionID, payload); err != nil {
			return nil, err
		}
		return n, nil
	})
	return out, err
}

// GetNote returns a note by ID.
func (s *Store) GetNote(ctx context.Context, req contracts.GetRequest) (contracts.Note, error) {
	return getNoteQ(ctx, s.db, req.ID)
}

// ListNotes lists notes newest first. ProjectID: nil = all, "" = standalone
// only. Query is a simple case-insensitive substring match on title/body
// (Hive provides ranked search).
func (s *Store) ListNotes(ctx context.Context, req contracts.ListNotesRequest) (contracts.Page[contracts.Note], error) {
	page := contracts.Page[contracts.Note]{Items: []contracts.Note{}, SnapshotAt: s.clock.Now()}
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
	if req.SessionID != "" {
		where = append(where, "session_id=?")
		args = append(args, req.SessionID)
	}
	if req.Kind != "" {
		where = append(where, "kind=?")
		args = append(args, string(req.Kind))
	}
	if len(req.SyncStates) > 0 {
		// An empty stored sync_state is treated as pending.
		ph := make([]string, len(req.SyncStates))
		for i, st := range req.SyncStates {
			ph[i] = "?"
			args = append(args, st)
		}
		where = append(where, `(CASE WHEN sync_state='' THEN '`+SyncPending+`' ELSE sync_state END) IN (`+strings.Join(ph, ",")+`)`)
	}
	if q := strings.TrimSpace(req.Query); q != "" {
		where = append(where, `(title LIKE ? ESCAPE '\' OR body LIKE ? ESCAPE '\')`)
		args = append(args, svcutil.LikeEscape(q), svcutil.LikeEscape(q))
	}
	if after != "" {
		where = append(where, "note_id < ?")
		args = append(args, after)
	}
	limit := svcutil.Limit(req.Limit)
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, `SELECT `+noteCols+` FROM notes WHERE `+strings.Join(where, " AND ")+` ORDER BY note_id DESC LIMIT ?`, args...)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		n, err := scanNote(rows)
		if err != nil {
			return page, err
		}
		page.Items = append(page.Items, n)
	}
	if err := rows.Err(); err != nil {
		return page, err
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		page.NextCursor = svcutil.EncodeCursor(page.Items[limit-1].NoteID)
	}
	return page, nil
}

// PromoteNote explicitly turns a note into an authoritative document
// revision. With DocumentID+BaseRevisionID it proposes the note body on an
// existing document with the same compare-and-swap semantics as
// ProposeRevision (a stale base preserves a conflicted proposal and
// returns ErrConflict; the note stays unpromoted). Otherwise it creates a
// new document (Title defaults to the note title, ProjectID to the note's
// project). The revision's author is the promoting principal and its
// provenance is the note's provenance. On success Note.PromotedTo records
// the revision ID; a note is promoted at most once (ErrConflict after).
//
// Events: document.revised (or document.conflict) from the document path.
// The pinned v1 table has no note-update kind, so none is emitted for the
// note itself; GetNote shows PromotedTo.
func (s *Store) PromoteNote(ctx context.Context, req contracts.PromoteNoteRequest) (contracts.DocumentWithRevision, error) {
	var out contracts.DocumentWithRevision
	err := svcutil.Mutate(ctx, s.db, req.Mutation, contracts.OpPromoteNote, req, &out, func(tx *sql.Tx, who contracts.Principal) (any, error) {
		n, err := getNoteQ(ctx, tx, req.NoteID)
		if err != nil {
			return nil, err
		}
		if n.PromotedTo != "" {
			return nil, contracts.Errorf(contracts.ErrConflict, "note was already promoted").
				WithRef(contracts.KindNote, n.NoteID).WithDetail("promoted_to", n.PromotedTo)
		}
		prov := n.Provenance
		prov.ProjectID = svcutil.CopyProject(prov.ProjectID)
		summary := "promoted from " + n.NoteID
		content := n.Body
		if strings.TrimSpace(content) == "" {
			content = "# " + n.Title + "\n"
		}
		var dwr contracts.DocumentWithRevision
		if req.DocumentID != "" {
			if req.BaseRevisionID == "" {
				return nil, contracts.Errorf(contracts.ErrInvalid, "base_revision_id is required when promoting into an existing document")
			}
			if req.Title != "" || req.Slug != "" || req.ProjectID != nil {
				return nil, contracts.Errorf(contracts.ErrInvalid, "give either document_id+base_revision_id or title/slug/project_id, not both")
			}
			dwr, err = s.proposeTx(ctx, tx, who, req.DocumentID, req.BaseRevisionID, content, summary, &prov)
		} else {
			projectID := req.ProjectID
			if projectID == nil {
				projectID = n.ProjectID
			}
			title := strings.TrimSpace(req.Title)
			if title == "" {
				title = n.Title
			}
			dwr, err = s.insertDocument(ctx, tx, who, s.clock.Now(), projectID, title, req.Slug, "", content, summary, &prov)
		}
		if err != nil {
			return nil, err // includes CommitWith(conflict): proposal kept, note unpromoted
		}
		if _, err := tx.ExecContext(ctx, `UPDATE notes SET promoted_to=? WHERE note_id=? AND promoted_to=''`, dwr.Revision.RevisionID, n.NoteID); err != nil {
			return nil, err
		}
		return dwr, nil
	})
	return out, err
}

// UpdateNoteSync records Hive indexing state for a note (used by the Hive
// bridge). sync_state must be pending, indexed or failed.
func (s *Store) UpdateNoteSync(ctx context.Context, req contracts.UpdateNoteSyncRequest) (contracts.Note, error) {
	var out contracts.Note
	err := svcutil.Mutate(ctx, s.db, req.Mutation, contracts.OpUpdateNoteSync, req, &out, func(tx *sql.Tx, who contracts.Principal) (any, error) {
		switch req.SyncState {
		case SyncPending, SyncIndexed, SyncFailed:
		default:
			return nil, contracts.Errorf(contracts.ErrInvalid, "sync_state must be pending, indexed or failed")
		}
		if len(req.HiveRef) > 512 {
			return nil, contracts.Errorf(contracts.ErrInvalid, "hive_ref too long")
		}
		n, err := getNoteQ(ctx, tx, req.NoteID)
		if err != nil {
			return nil, err
		}
		if req.HiveRef != "" {
			n.HiveRef = req.HiveRef
		}
		n.SyncState = req.SyncState
		if _, err := tx.ExecContext(ctx, `UPDATE notes SET hive_ref=?, sync_state=? WHERE note_id=?`, n.HiveRef, n.SyncState, n.NoteID); err != nil {
			return nil, err
		}
		return n, nil
	})
	return out, err
}
