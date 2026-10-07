package projects

import (
	"context"
	"database/sql"
	"errors"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/projects/svcutil"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

// Migrations returns this domain's schema steps (namespace "projects") plus
// the shared operations table. New applies them; the hub may also apply
// them up front.
func Migrations() []sqlitex.Migration {
	return []sqlitex.Migration{sqlitex.OperationsMigration, migrationV1}
}

var migrationV1 = sqlitex.Migration{Namespace: "projects", Version: 1, Name: "projects_documents_notes", SQL: `
CREATE TABLE projects (
	project_id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	description TEXT NOT NULL DEFAULT '',
	brief_document_id TEXT NOT NULL DEFAULT '',
	repos TEXT NOT NULL DEFAULT '[]',
	checkouts TEXT NOT NULL DEFAULT '[]',
	links TEXT NOT NULL DEFAULT '[]',
	env_profile TEXT NOT NULL DEFAULT '',
	connections TEXT NOT NULL DEFAULT '[]',
	archived INTEGER NOT NULL DEFAULT 0,
	version INTEGER NOT NULL,
	created_by TEXT NOT NULL,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);

CREATE TABLE documents (
	document_id TEXT PRIMARY KEY,
	project_id TEXT REFERENCES projects(project_id),
	scope TEXT NOT NULL,            -- project_id, or '' for standalone (slug uniqueness)
	title TEXT NOT NULL,
	slug TEXT NOT NULL,
	media_type TEXT NOT NULL,
	head_revision_id TEXT NOT NULL,
	head_seq INTEGER NOT NULL,
	created_by TEXT NOT NULL,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	UNIQUE (scope, slug)
);
CREATE INDEX documents_project ON documents(project_id);

CREATE TABLE document_revisions (
	revision_id TEXT PRIMARY KEY,
	document_id TEXT NOT NULL REFERENCES documents(document_id),
	seq INTEGER NOT NULL,
	parent_revision_id TEXT NOT NULL DEFAULT '',
	digest TEXT NOT NULL,
	size INTEGER NOT NULL,
	author TEXT NOT NULL,
	created_at TEXT NOT NULL,
	summary TEXT NOT NULL DEFAULT '',
	content TEXT NOT NULL,
	provenance TEXT,
	UNIQUE (document_id, seq)
);
CREATE TRIGGER document_revisions_immutable_u BEFORE UPDATE ON document_revisions
BEGIN SELECT RAISE(ABORT, 'document revisions are immutable'); END;
CREATE TRIGGER document_revisions_immutable_d BEFORE DELETE ON document_revisions
BEGIN SELECT RAISE(ABORT, 'document revisions are immutable'); END;

CREATE TABLE document_proposals (
	proposal_id TEXT PRIMARY KEY,
	document_id TEXT NOT NULL REFERENCES documents(document_id),
	base_revision_id TEXT NOT NULL,
	content TEXT NOT NULL,
	digest TEXT NOT NULL,
	summary TEXT NOT NULL DEFAULT '',
	author TEXT NOT NULL,
	created_at TEXT NOT NULL,
	state TEXT NOT NULL,
	head_at_conflict TEXT NOT NULL DEFAULT ''
);
CREATE INDEX document_proposals_doc ON document_proposals(document_id, proposal_id);

CREATE TABLE notes (
	note_id TEXT PRIMARY KEY,
	kind TEXT NOT NULL,
	project_id TEXT REFERENCES projects(project_id),
	session_id TEXT NOT NULL DEFAULT '',
	title TEXT NOT NULL,
	body TEXT NOT NULL,
	author TEXT NOT NULL,
	created_at TEXT NOT NULL,
	provenance TEXT NOT NULL,
	hive_ref TEXT NOT NULL DEFAULT '',
	sync_state TEXT NOT NULL DEFAULT 'pending',
	promoted_to TEXT NOT NULL DEFAULT ''
);
CREATE INDEX notes_project ON notes(project_id, note_id);
CREATE INDEX notes_session ON notes(session_id, note_id);
`}

// ArtifactIndex is the read-only slice of the artifact service used to put
// artifact pointers into launch context. *artifacts.Store satisfies it.
type ArtifactIndex interface {
	ListArtifacts(ctx context.Context, req contracts.ListArtifactsRequest) (contracts.Page[contracts.Artifact], error)
}

// Options configures a Store. Zero values select defaults.
type Options struct {
	Clock contracts.Clock
	// MaxDocumentBytes bounds one revision's content (default 4 MiB).
	MaxDocumentBytes int
	// MaxNoteBytes bounds a note body (default 256 KiB).
	MaxNoteBytes int
	// Artifacts, when set, adds artifact pointers to launch context. It may
	// also be supplied later with SetArtifactIndex (construction order).
	Artifacts ArtifactIndex
}

// Store implements contracts.ProjectAPI, contracts.DocumentAPI and
// contracts.NoteAPI on the hub's SQLite database.
type Store struct {
	db        *sql.DB
	sink      contracts.EventSink
	clock     contracts.Clock
	maxDoc    int
	maxNote   int
	artifacts ArtifactIndex
}

var (
	_ contracts.ProjectAPI  = (*Store)(nil)
	_ contracts.DocumentAPI = (*Store)(nil)
	_ contracts.NoteAPI     = (*Store)(nil)
)

// New applies migrations and returns a Store. sink receives project.changed,
// document.revised, document.conflict and note.created events inside the
// writing transaction.
func New(ctx context.Context, db *sql.DB, sink contracts.EventSink, opts Options) (*Store, error) {
	if db == nil {
		return nil, errors.New("projects: nil db")
	}
	if err := sqlitex.Migrate(ctx, db, Migrations()...); err != nil {
		return nil, err
	}
	s := &Store{db: db, sink: sink, clock: opts.Clock, maxDoc: opts.MaxDocumentBytes, maxNote: opts.MaxNoteBytes, artifacts: opts.Artifacts}
	if s.clock == nil {
		s.clock = contracts.SystemClock{}
	}
	if s.maxDoc <= 0 {
		s.maxDoc = 4 << 20
	}
	if s.maxNote <= 0 {
		s.maxNote = 256 << 10
	}
	return s, nil
}

// SetArtifactIndex wires the artifact service after construction (the
// artifact service itself depends on ProjectExists).
func (s *Store) SetArtifactIndex(a ArtifactIndex) { s.artifacts = a }

// ProjectExists returns nil if the project exists, else ErrNotFound. Used
// by the artifact service to validate project associations.
func (s *Store) ProjectExists(ctx context.Context, projectID string) error {
	return projectExistsQ(ctx, s.db, projectID)
}

// LookupRevision resolves a document revision for review targeting. It
// returns the owning document and its project.
func (s *Store) LookupRevision(ctx context.Context, revisionID string) (documentID string, projectID *string, err error) {
	var proj sql.NullString
	err = s.db.QueryRowContext(ctx, `SELECT r.document_id, d.project_id FROM document_revisions r JOIN documents d ON d.document_id=r.document_id WHERE r.revision_id=?`, revisionID).Scan(&documentID, &proj)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, contracts.Errorf(contracts.ErrNotFound, "document revision not found").WithRef(contracts.KindDocumentRevision, revisionID)
	}
	if err != nil {
		return "", nil, err
	}
	return documentID, svcutil.ScanProject(proj), nil
}

type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func projectExistsQ(ctx context.Context, q queryer, projectID string) error {
	var one int
	err := q.QueryRowContext(ctx, `SELECT 1 FROM projects WHERE project_id=?`, projectID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return contracts.Errorf(contracts.ErrNotFound, "project not found").WithRef(contracts.KindProject, projectID)
	}
	return err
}
