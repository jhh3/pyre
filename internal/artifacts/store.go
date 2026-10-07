package artifacts

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/projects/svcutil"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

// Migrations returns this domain's schema steps (namespace "artifacts")
// plus the shared operations table.
func Migrations() []sqlitex.Migration {
	return []sqlitex.Migration{sqlitex.OperationsMigration, migrationV1, migrationV2}
}

// migrationV2 carries BeginUploadRequest.ReviewRequested through to the
// immutable version (adding a column is not an UPDATE, so the
// immutability triggers are unaffected).
var migrationV2 = sqlitex.Migration{Namespace: "artifacts", Version: 2, Name: "review_requested", SQL: `
ALTER TABLE artifact_uploads ADD COLUMN review_requested INTEGER NOT NULL DEFAULT 0;
ALTER TABLE artifact_versions ADD COLUMN review_requested INTEGER NOT NULL DEFAULT 0;
`}

var migrationV1 = sqlitex.Migration{Namespace: "artifacts", Version: 1, Name: "artifacts_versions_uploads_blobs", SQL: `
CREATE TABLE artifacts (
	artifact_id TEXT PRIMARY KEY,
	project_id TEXT,
	session_id TEXT NOT NULL DEFAULT '',
	title TEXT NOT NULL,
	description TEXT NOT NULL DEFAULT '',
	latest_version_id TEXT NOT NULL DEFAULT '',
	version_count INTEGER NOT NULL DEFAULT 0,
	created_by TEXT NOT NULL,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);
CREATE INDEX artifacts_project ON artifacts(project_id, artifact_id);
CREATE INDEX artifacts_session ON artifacts(session_id, artifact_id);

CREATE TABLE artifact_versions (
	version_id TEXT PRIMARY KEY,
	artifact_id TEXT NOT NULL REFERENCES artifacts(artifact_id),
	seq INTEGER NOT NULL,
	previous_version_id TEXT NOT NULL DEFAULT '',
	digest TEXT NOT NULL,
	size INTEGER NOT NULL,
	media_type TEXT NOT NULL,
	filename TEXT NOT NULL,
	author TEXT NOT NULL,
	provenance TEXT NOT NULL,
	created_at TEXT NOT NULL,
	note TEXT NOT NULL DEFAULT '',
	UNIQUE (artifact_id, seq)
);
CREATE TRIGGER artifact_versions_immutable_u BEFORE UPDATE ON artifact_versions
BEGIN SELECT RAISE(ABORT, 'artifact versions are immutable'); END;
CREATE TRIGGER artifact_versions_immutable_d BEFORE DELETE ON artifact_versions
BEGIN SELECT RAISE(ABORT, 'artifact versions are immutable'); END;

CREATE TABLE artifact_blobs (
	digest TEXT PRIMARY KEY,
	size INTEGER NOT NULL,
	created_at TEXT NOT NULL,
	last_verified_at TEXT NOT NULL,
	state TEXT NOT NULL DEFAULT 'ok'   -- ok | corrupt
);

CREATE TABLE artifact_uploads (
	upload_id TEXT PRIMARY KEY,
	artifact_id TEXT NOT NULL DEFAULT '',
	project_id TEXT,
	title TEXT NOT NULL DEFAULT '',
	description TEXT NOT NULL DEFAULT '',
	filename TEXT NOT NULL,
	media_type TEXT NOT NULL DEFAULT '',
	size INTEGER NOT NULL,
	digest TEXT NOT NULL,
	note TEXT NOT NULL DEFAULT '',
	provenance TEXT NOT NULL,
	principal TEXT NOT NULL,
	state TEXT NOT NULL,
	received INTEGER NOT NULL DEFAULT 0,
	expires_at TEXT NOT NULL,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	version_id TEXT NOT NULL DEFAULT '',
	failure TEXT NOT NULL DEFAULT ''
);
CREATE INDEX artifact_uploads_state ON artifact_uploads(state, expires_at);
`}

// ProjectLookup validates project associations. *projects.Store satisfies it.
type ProjectLookup interface {
	ProjectExists(ctx context.Context, projectID string) error
}

// Defaults.
const (
	DefaultMaxBytes  = 512 << 20
	DefaultUploadTTL = 24 * time.Hour
)

// Options configures a Store.
type Options struct {
	// DataDir holds blobs/sha256/<2>/<62> and uploads/<upload_id>. Required.
	DataDir string
	Clock   contracts.Clock
	// MaxBytes bounds a single version (default 512 MiB). Exceeding it
	// returns ErrPayloadTooLarge; it is reported in Upload.MaxBytes.
	MaxBytes int64
	// MaxTotalBytes bounds stored blobs plus open uploads (0 = unlimited);
	// exceeding it returns ErrCapacityExhausted.
	MaxTotalBytes int64
	// UploadTTL is the idle expiry of an open upload (default 24h; each
	// accepted chunk extends it).
	UploadTTL time.Duration
	// VerifyTTL: OpenContent re-hashes a blob before serving unless it was
	// verified within VerifyTTL. 0 (default) verifies on every open.
	// Sequential reads are additionally verified at EOF regardless.
	VerifyTTL time.Duration
	// Projects validates project IDs (optional; nil skips the check).
	Projects ProjectLookup
	// Tickets issues one-time preview tickets for MintPreviewURL (nil:
	// MintPreviewURL returns ErrUnsupported).
	Tickets contracts.TokenIssuer
	// PreviewBase prefixes minted preview links: the separate preview
	// listener's origin (recommended), else the hub's public URL (previews
	// then share the API origin, still sandboxed), else "" (hub-relative).
	PreviewBase string
}

// Store implements contracts.ArtifactAPI with a content-addressed blob
// store under Options.DataDir and manifests in the hub SQLite database.
type Store struct {
	db       *sql.DB
	sink     contracts.EventSink
	clock    contracts.Clock
	dir      string
	maxBytes int64
	maxTotal int64
	ttl      time.Duration

	tickets     contracts.TokenIssuer
	previewBase string
	vttl        time.Duration
	projects    ProjectLookup
	locks       [64]sync.Mutex
}

var _ contracts.ArtifactAPI = (*Store)(nil)

// New applies migrations, creates the data directories (0700) and returns a
// Store. sink receives artifact.published inside the publishing transaction.
func New(ctx context.Context, db *sql.DB, sink contracts.EventSink, opts Options) (*Store, error) {
	if db == nil {
		return nil, errors.New("artifacts: nil db")
	}
	if opts.DataDir == "" {
		return nil, errors.New("artifacts: DataDir is required")
	}
	dir, err := filepath.Abs(opts.DataDir)
	if err != nil {
		return nil, err
	}
	for _, d := range []string{filepath.Join(dir, "blobs", "sha256"), filepath.Join(dir, "uploads")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	if err := sqlitex.Migrate(ctx, db, Migrations()...); err != nil {
		return nil, err
	}
	s := &Store{db: db, sink: sink, clock: opts.Clock, dir: dir, maxBytes: opts.MaxBytes, maxTotal: opts.MaxTotalBytes,
		ttl: opts.UploadTTL, vttl: opts.VerifyTTL, projects: opts.Projects, tickets: opts.Tickets, previewBase: opts.PreviewBase}
	if s.clock == nil {
		s.clock = contracts.SystemClock{}
	}
	if s.maxBytes <= 0 {
		s.maxBytes = DefaultMaxBytes
	}
	if s.ttl <= 0 {
		s.ttl = DefaultUploadTTL
	}
	return s, nil
}

// MaxBytes reports the per-version size limit.
func (s *Store) MaxBytes() int64 { return s.maxBytes }

func (s *Store) lock(id string) func() {
	h := fnv.New32a()
	h.Write([]byte(id))
	m := &s.locks[h.Sum32()%uint32(len(s.locks))]
	m.Lock()
	return m.Unlock
}

// blobPath maps a validated hex digest to its storage path.
func (s *Store) blobPath(hexDigest string) string {
	return filepath.Join(s.dir, "blobs", "sha256", hexDigest[:2], hexDigest[2:])
}

// stagedPath maps an upload ID (always one the store generated and looked
// up) to its staging file.
func (s *Store) stagedPath(uploadID string) string {
	return filepath.Join(s.dir, "uploads", filepath.Base(uploadID))
}

// ContentURL is the hub-relative download path of a version.
func ContentURL(versionID string) string { return "/v1/content/" + versionID }

// PreviewURLTemplate is the preview path with a "{token}" placeholder, set
// on versions with active media types. Mint a real URL with MintPreviewURL.
func PreviewURLTemplate(versionID string) string { return "/preview/" + versionID + "/{token}" }

// ---- reads ----

type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

const artifactCols = `artifact_id, project_id, session_id, title, description, latest_version_id, version_count, created_by, created_at, updated_at`

func scanArtifact(sc interface{ Scan(...any) error }) (contracts.Artifact, error) {
	var a contracts.Artifact
	var proj sql.NullString
	var createdBy, createdAt, updatedAt string
	if err := sc.Scan(&a.ArtifactID, &proj, &a.SessionID, &a.Title, &a.Description, &a.LatestVersionID, &a.VersionCount, &createdBy, &createdAt, &updatedAt); err != nil {
		return a, err
	}
	a.ProjectID = svcutil.ScanProject(proj)
	_ = svcutil.FromJSON(createdBy, &a.CreatedBy)
	a.CreatedAt = svcutil.ParseTime(createdAt)
	a.UpdatedAt = svcutil.ParseTime(updatedAt)
	return a, nil
}

const versionCols = `version_id, artifact_id, seq, previous_version_id, digest, size, media_type, filename, author, provenance, created_at, note, review_requested`

func scanVersion(sc interface{ Scan(...any) error }) (contracts.ArtifactVersion, error) {
	var v contracts.ArtifactVersion
	var author, prov, createdAt string
	var review int
	if err := sc.Scan(&v.VersionID, &v.ArtifactID, &v.Seq, &v.PreviousVersionID, &v.Digest, &v.Size, &v.MediaType, &v.Filename, &author, &prov, &createdAt, &v.Note, &review); err != nil {
		return v, err
	}
	v.ReviewRequested = review != 0
	_ = svcutil.FromJSON(author, &v.Author)
	_ = svcutil.FromJSON(prov, &v.Provenance)
	v.CreatedAt = svcutil.ParseTime(createdAt)
	v.ContentURL = ContentURL(v.VersionID)
	if IsActiveType(v.MediaType) {
		v.PreviewURL = PreviewURLTemplate(v.VersionID)
	}
	return v, nil
}

func getArtifactQ(ctx context.Context, q queryer, id string) (contracts.Artifact, error) {
	a, err := scanArtifact(q.QueryRowContext(ctx, `SELECT `+artifactCols+` FROM artifacts WHERE artifact_id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return a, contracts.Errorf(contracts.ErrNotFound, "artifact not found").WithRef(contracts.KindArtifact, id)
	}
	return a, err
}

func getVersionQ(ctx context.Context, q queryer, id string) (contracts.ArtifactVersion, error) {
	v, err := scanVersion(q.QueryRowContext(ctx, `SELECT `+versionCols+` FROM artifact_versions WHERE version_id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return v, contracts.Errorf(contracts.ErrNotFound, "artifact version not found").WithRef(contracts.KindArtifactVersion, id)
	}
	return v, err
}

// GetArtifact returns an artifact by ID.
func (s *Store) GetArtifact(ctx context.Context, req contracts.GetRequest) (contracts.Artifact, error) {
	return getArtifactQ(ctx, s.db, req.ID)
}

// GetVersion returns an immutable version manifest by ID.
func (s *Store) GetVersion(ctx context.Context, req contracts.GetRequest) (contracts.ArtifactVersion, error) {
	return getVersionQ(ctx, s.db, req.ID)
}

// LookupVersion resolves a version for review targeting: its artifact and
// the artifact's project.
func (s *Store) LookupVersion(ctx context.Context, versionID string) (artifactID string, projectID *string, err error) {
	var proj sql.NullString
	err = s.db.QueryRowContext(ctx, `SELECT v.artifact_id, a.project_id FROM artifact_versions v JOIN artifacts a ON a.artifact_id=v.artifact_id WHERE v.version_id=?`, versionID).Scan(&artifactID, &proj)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, contracts.Errorf(contracts.ErrNotFound, "artifact version not found").WithRef(contracts.KindArtifactVersion, versionID)
	}
	if err != nil {
		return "", nil, err
	}
	return artifactID, svcutil.ScanProject(proj), nil
}

// ListArtifacts lists artifacts newest first. ProjectID: nil = all,
// "" = standalone only; SessionID filters by producing session.
func (s *Store) ListArtifacts(ctx context.Context, req contracts.ListArtifactsRequest) (contracts.Page[contracts.Artifact], error) {
	page := contracts.Page[contracts.Artifact]{Items: []contracts.Artifact{}, SnapshotAt: s.clock.Now()}
	after, err := svcutil.DecodeCursor(req.Cursor)
	if err != nil {
		return page, err
	}
	where := []string{"version_count > 0"}
	var args []any
	if frag, a := svcutil.ProjectFilter("project_id", req.ProjectID); frag != "" {
		where = append(where, frag)
		args = append(args, a...)
	}
	if req.SessionID != "" {
		where = append(where, "session_id=?")
		args = append(args, req.SessionID)
	}
	if after != "" {
		where = append(where, "artifact_id < ?")
		args = append(args, after)
	}
	limit := svcutil.Limit(req.Limit)
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, `SELECT `+artifactCols+` FROM artifacts WHERE `+strings.Join(where, " AND ")+` ORDER BY artifact_id DESC LIMIT ?`, args...)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		a, err := scanArtifact(rows)
		if err != nil {
			return page, err
		}
		page.Items = append(page.Items, a)
	}
	if err := rows.Err(); err != nil {
		return page, err
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		page.NextCursor = svcutil.EncodeCursor(page.Items[limit-1].ArtifactID)
	}
	return page, nil
}

// ListVersions lists an artifact's versions newest first.
func (s *Store) ListVersions(ctx context.Context, req contracts.ListVersionsRequest) (contracts.Page[contracts.ArtifactVersion], error) {
	page := contracts.Page[contracts.ArtifactVersion]{Items: []contracts.ArtifactVersion{}, SnapshotAt: s.clock.Now()}
	if _, err := getArtifactQ(ctx, s.db, req.ArtifactID); err != nil {
		return page, err
	}
	after, err := svcutil.DecodeCursor(req.Cursor)
	if err != nil {
		return page, err
	}
	where := "artifact_id=?"
	args := []any{req.ArtifactID}
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
	rows, err := s.db.QueryContext(ctx, `SELECT `+versionCols+` FROM artifact_versions WHERE `+where+` ORDER BY seq DESC LIMIT ?`, args...)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		v, err := scanVersion(rows)
		if err != nil {
			return page, err
		}
		page.Items = append(page.Items, v)
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

// StorageUsage reports bytes held by blobs and declared by open uploads.
func (s *Store) StorageUsage(ctx context.Context) (blobs, openUploads int64, err error) {
	if err = s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(size),0) FROM artifact_blobs`).Scan(&blobs); err != nil {
		return
	}
	err = s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(size),0) FROM artifact_uploads WHERE state='open'`).Scan(&openUploads)
	return
}

func internalf(format string, args ...any) error { return fmt.Errorf("artifacts: "+format, args...) }
