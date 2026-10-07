package artifacts

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/projects/svcutil"
)

// Upload states.
const (
	StateOpen      = "open"
	StateFinalized = "finalized"
	StateFailed    = "failed"
	StateExpired   = "expired"
)

const chunkBuf = 64 << 10

type uploadRow struct {
	contracts.Upload
	ProjectID   *string
	Title       string
	Description string
	Filename    string
	MediaType   string
	Note        string
	Review      bool
	Provenance  contracts.Provenance
	Principal   contracts.Principal
	VersionID   string
	Failure     string
}

const uploadCols = `upload_id, artifact_id, project_id, title, description, filename, media_type, size, digest, note, provenance, principal, state, received, expires_at, version_id, failure, review_requested`

func (s *Store) getUpload(ctx context.Context, q queryer, id string) (uploadRow, error) {
	var u uploadRow
	var proj sql.NullString
	var prov, who, expires string
	var review int
	err := q.QueryRowContext(ctx, `SELECT `+uploadCols+` FROM artifact_uploads WHERE upload_id=?`, id).Scan(
		&u.UploadID, &u.ArtifactID, &proj, &u.Title, &u.Description, &u.Filename, &u.MediaType, &u.Size, &u.Digest, &u.Note, &prov, &who, &u.State, &u.Received, &expires, &u.VersionID, &u.Failure, &review)
	if errors.Is(err, sql.ErrNoRows) {
		return u, contracts.Errorf(contracts.ErrNotFound, "upload not found").WithRef(contracts.KindUpload, id)
	}
	if err != nil {
		return u, err
	}
	u.ProjectID = svcutil.ScanProject(proj)
	u.Review = review != 0
	_ = svcutil.FromJSON(prov, &u.Provenance)
	_ = svcutil.FromJSON(who, &u.Principal)
	u.ExpiresAt = svcutil.ParseTime(expires)
	u.MaxBytes = s.maxBytes
	return u, nil
}

func canWrite(u uploadRow, p contracts.Principal) error {
	if p.Kind == contracts.PrincipalOwner || (p.Kind == u.Principal.Kind && p.ID == u.Principal.ID) {
		return nil
	}
	return contracts.Errorf(contracts.ErrForbidden, "upload belongs to another principal").WithRef(contracts.KindUpload, u.UploadID)
}

// BeginUpload validates the declared size, sha256 digest and filename and
// opens a resumable upload. Either ArtifactID (new version of an existing
// artifact) or Title (new artifact, created only when the first version is
// published) identifies the target. The filename is display-only.
func (s *Store) BeginUpload(ctx context.Context, req contracts.BeginUploadRequest) (contracts.Upload, error) {
	var out contracts.Upload
	// Projects are never deleted, so validating outside the write
	// transaction is safe and avoids a cross-service read while holding it.
	if p := svcutil.CopyProject(req.ProjectID); p != nil && req.ArtifactID == "" && s.projects != nil {
		if err := s.projects.ProjectExists(ctx, *p); err != nil {
			return out, err
		}
	}
	err := svcutil.Mutate(ctx, s.db, req.Mutation, contracts.OpBeginUpload, req, &out, func(tx *sql.Tx, who contracts.Principal) (any, error) {
		if strings.TrimSpace(req.Filename) == "" {
			return nil, contracts.Errorf(contracts.ErrInvalid, "filename is required")
		}
		filename := SafeFilename(req.Filename)
		if req.Size < 0 {
			return nil, contracts.Errorf(contracts.ErrInvalid, "size must be >= 0")
		}
		if req.Size > s.maxBytes {
			return nil, contracts.Errorf(contracts.ErrPayloadTooLarge, "artifact is %d bytes; limit is %d", req.Size, s.maxBytes).WithDetail("max_bytes", s.maxBytes)
		}
		digest, _, err := parseDigest(req.Digest)
		if err != nil {
			return nil, err
		}
		mt, err := normalizeMediaType(req.MediaType)
		if err != nil {
			return nil, err
		}
		if mt == "" {
			mt = typeByFilename(filename) // may stay "" -> sniffed at finalize
		}
		if len(req.Note) > 4096 || len(req.Description) > 8192 || len(req.Title) > 200 {
			return nil, contracts.Errorf(contracts.ErrInvalid, "title/description/note too long")
		}
		prov := req.Provenance
		if who.Kind == contracts.PrincipalSession {
			if prov.SessionID != "" && prov.SessionID != who.ID {
				return nil, contracts.Errorf(contracts.ErrForbidden, "a session may only publish with its own provenance")
			}
			prov.SessionID = who.ID
		}
		prov.SourcePath = truncate(prov.SourcePath, 1024)
		var projectID *string
		title := strings.TrimSpace(req.Title)
		if req.ArtifactID != "" {
			a, err := getArtifactQ(ctx, tx, req.ArtifactID)
			if err != nil {
				return nil, err
			}
			if p := svcutil.CopyProject(req.ProjectID); p != nil && (a.ProjectID == nil || *a.ProjectID != *p) {
				return nil, contracts.Errorf(contracts.ErrInvalid, "project_id does not match the artifact's project")
			}
			projectID = a.ProjectID
			title = a.Title
		} else {
			if title == "" {
				title = filename
			}
			projectID = svcutil.CopyProject(req.ProjectID) // existence checked before the transaction
		}
		if prov.ProjectID == nil {
			prov.ProjectID = svcutil.CopyProject(projectID)
		}
		if s.maxTotal > 0 {
			var used int64
			if err := tx.QueryRowContext(ctx, `SELECT (SELECT COALESCE(SUM(size),0) FROM artifact_blobs) + (SELECT COALESCE(SUM(size),0) FROM artifact_uploads WHERE state='open')`).Scan(&used); err != nil {
				return nil, err
			}
			if used+req.Size > s.maxTotal {
				return nil, contracts.Errorf(contracts.ErrCapacityExhausted, "artifact storage limit reached (%d of %d bytes used)", used, s.maxTotal).
					WithDetail("used_bytes", used).WithDetail("max_total_bytes", s.maxTotal)
			}
		}
		now := s.clock.Now()
		u := contracts.Upload{
			UploadID:   contracts.NewID(contracts.PrefixUpload),
			ArtifactID: req.ArtifactID,
			Size:       req.Size,
			Digest:     digest,
			ExpiresAt:  now.Add(s.ttl),
			MaxBytes:   s.maxBytes,
			State:      StateOpen,
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO artifact_uploads(upload_id, artifact_id, project_id, title, description, filename, media_type, size, digest, note, provenance, principal, state, received, expires_at, created_at, updated_at, review_requested) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			u.UploadID, u.ArtifactID, svcutil.NullProject(projectID), title, req.Description, filename, mt, u.Size, string(digest), req.Note,
			svcutil.JSON(prov), svcutil.JSON(who), u.State, 0, svcutil.Time(u.ExpiresAt), svcutil.Time(now), svcutil.Time(now), boolInt(req.ReviewRequested)); err != nil {
			return nil, err
		}
		return u, nil
	})
	return out, err
}

// UploadChunk writes bytes at offset into the staged upload. It is
// idempotent for a re-sent range whose bytes match what was received; an
// overlapping range with different bytes returns ErrConflict and changes
// nothing. offset must be <= the received count (resume from Received).
// A chunk extending past the declared size is rejected whole with
// ErrPayloadTooLarge and leaves Received unchanged. If the reader
// fails mid-chunk the bytes received so far are kept and the error is
// returned; the caller resumes from the returned/queried Received.
func (s *Store) UploadChunk(ctx context.Context, uploadID string, offset int64, r io.Reader) (contracts.Upload, error) {
	who, err := svcutil.RequirePrincipal(ctx)
	if err != nil {
		return contracts.Upload{}, err
	}
	defer s.lock(uploadID)()
	u, err := s.getUpload(ctx, s.db, uploadID)
	if err != nil {
		return contracts.Upload{}, err
	}
	if err := canWrite(u, who); err != nil {
		return contracts.Upload{}, err
	}
	now := s.clock.Now()
	switch u.State {
	case StateOpen:
	case StateExpired:
		return u.Upload, contracts.Errorf(contracts.ErrExpired, "upload expired").WithRef(contracts.KindUpload, u.UploadID)
	default:
		return u.Upload, contracts.Errorf(contracts.ErrConflict, "upload is %s", u.State).WithRef(contracts.KindUpload, u.UploadID)
	}
	if now.After(u.ExpiresAt) {
		if _, err := s.db.ExecContext(ctx, `UPDATE artifact_uploads SET state=?, updated_at=? WHERE upload_id=? AND state='open'`, StateExpired, svcutil.Time(now), u.UploadID); err != nil {
			return u.Upload, err
		}
		_ = os.Remove(s.stagedPath(u.UploadID))
		u.State = StateExpired
		return u.Upload, contracts.Errorf(contracts.ErrExpired, "upload expired").WithRef(contracts.KindUpload, u.UploadID)
	}
	if offset < 0 {
		return u.Upload, contracts.Errorf(contracts.ErrInvalid, "offset must be >= 0")
	}
	if offset > u.Received {
		return u.Upload, contracts.Errorf(contracts.ErrConflict, "offset %d is past received bytes %d; resume from received", offset, u.Received).
			WithRef(contracts.KindUpload, u.UploadID).WithDetail("received", u.Received)
	}
	f, err := os.OpenFile(s.stagedPath(u.UploadID), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return u.Upload, err
	}
	defer f.Close()

	pos := offset
	received := u.Received
	var chunkErr error
	buf := make([]byte, chunkBuf)
	cmp := make([]byte, chunkBuf)
	src := io.LimitReader(r, u.Size-offset+1)
	for chunkErr == nil {
		n, rerr := src.Read(buf)
		data := buf[:n]
		if pos+int64(n) > u.Size {
			// The whole chunk is rejected: bytes of it already written
			// past the previous received count do not count as received
			// and are discarded below.
			received = u.Received
			chunkErr = contracts.Errorf(contracts.ErrPayloadTooLarge, "chunk extends past the declared size %d; nothing from it was stored", u.Size).
				WithRef(contracts.KindUpload, u.UploadID).WithDetail("size", u.Size)
			break
		}
		if pos < received && len(data) > 0 {
			ov := min(int64(len(data)), received-pos)
			if _, err := f.ReadAt(cmp[:ov], pos); err != nil {
				return u.Upload, internalf("read staged upload: %v", err)
			}
			if !bytes.Equal(cmp[:ov], data[:ov]) {
				return u.Upload, contracts.Errorf(contracts.ErrConflict, "re-sent range at offset %d differs from bytes already received", pos).
					WithRef(contracts.KindUpload, u.UploadID).WithDetail("received", received)
			}
			data = data[ov:]
			pos += ov
		}
		if len(data) > 0 {
			if _, err := f.WriteAt(data, pos); err != nil {
				return u.Upload, internalf("write staged upload: %v", err)
			}
			pos += int64(len(data))
			received = pos
		}
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil && chunkErr == nil {
			chunkErr = contracts.Errorf(contracts.ErrUnavailable, "chunk interrupted: %v", rerr).
				WithRef(contracts.KindUpload, u.UploadID)
		}
	}
	if contracts.IsCode(chunkErr, contracts.ErrPayloadTooLarge) {
		if fi, err := f.Stat(); err == nil && fi.Size() > u.Received {
			if err := f.Truncate(u.Received); err != nil {
				return u.Upload, internalf("truncate staged upload: %v", err)
			}
		}
	}
	if received > u.Received {
		if err := f.Sync(); err != nil {
			return u.Upload, internalf("sync staged upload: %v", err)
		}
		u.Received = received
	}
	u.ExpiresAt = now.Add(s.ttl)
	if _, err := s.db.ExecContext(ctx, `UPDATE artifact_uploads SET received=?, expires_at=?, updated_at=? WHERE upload_id=? AND state='open'`,
		u.Received, svcutil.Time(u.ExpiresAt), svcutil.Time(now), u.UploadID); err != nil {
		return u.Upload, err
	}
	if ce := contracts.AsError(chunkErr); ce != nil {
		ce.WithDetail("received", u.Received)
	}
	return u.Upload, chunkErr
}

// verified is the result of hashing staged (or already-placed) bytes.
type verified struct {
	digest  contracts.Digest
	size    int64
	head    []byte
	inBlob  bool // bytes are already at the blob path (a previous attempt placed them)
	missing bool
}

func hashFile(path string, limit int64) (verified, error) {
	f, err := os.Open(path)
	if err != nil {
		return verified{}, err
	}
	defer f.Close()
	h := sha256.New()
	head := make([]byte, 512)
	hn, _ := io.ReadFull(f, head)
	head = head[:hn]
	h.Write(head)
	n, err := io.Copy(h, io.LimitReader(f, limit+1-int64(hn)))
	if err != nil {
		return verified{}, err
	}
	return verified{digest: contracts.Digest("sha256:" + hex.EncodeToString(h.Sum(nil))), size: int64(hn) + n, head: head}, nil
}

// prepare hashes the bytes of a complete open upload outside the write
// transaction (the per-upload lock is held by the caller).
func (s *Store) prepare(u uploadRow) (*verified, error) {
	if u.State != StateOpen || u.Received != u.Size {
		return nil, nil
	}
	v, err := hashFile(s.stagedPath(u.UploadID), u.Size)
	if err == nil {
		return &v, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if u.Size == 0 {
		return &verified{digest: contracts.Digest("sha256:" + hex.EncodeToString(sha256.New().Sum(nil))), missing: true}, nil
	}
	// A previous finalize may have moved the bytes into place before its
	// transaction failed; re-verify them there.
	_, hexd, perr := parseDigest(u.Digest)
	if perr != nil {
		return nil, perr
	}
	v, err = hashFile(s.blobPath(hexd), u.Size)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &verified{missing: true, size: -1}, nil
		}
		return nil, err
	}
	v.inBlob = true
	return &v, nil
}

// FinalizeUpload verifies the received bytes against the declared size and
// sha256 BEFORE publishing. On mismatch nothing is published, the upload
// becomes "failed" and ErrConflict is returned. On success the blob is
// moved into content-addressed storage and the version manifest is
// committed in one transaction, so readers see either no version or a
// complete one. Finalizing an already-finalized upload returns its receipt.
func (s *Store) FinalizeUpload(ctx context.Context, req contracts.FinalizeUploadRequest) (contracts.PublishReceipt, error) {
	var out contracts.PublishReceipt
	if _, err := svcutil.RequirePrincipal(ctx); err != nil {
		return out, err
	}
	defer s.lock(req.UploadID)()
	var pre *verified
	if u, err := s.getUpload(ctx, s.db, req.UploadID); err == nil && !s.clock.Now().After(u.ExpiresAt) {
		if pre, err = s.prepare(u); err != nil {
			return out, internalf("verify upload: %v", err)
		}
	}
	err := svcutil.Mutate(ctx, s.db, req.Mutation, contracts.OpFinalizeUpload, req, &out, func(tx *sql.Tx, who contracts.Principal) (any, error) {
		return s.finalizeTx(ctx, tx, who, req.UploadID, pre)
	})
	// Staged bytes of a terminal upload are no longer needed.
	if u, gerr := s.getUpload(ctx, s.db, req.UploadID); gerr == nil && u.State != StateOpen {
		_ = os.Remove(s.stagedPath(u.UploadID))
	}
	return out, err
}

func (s *Store) finalizeTx(ctx context.Context, tx *sql.Tx, who contracts.Principal, uploadID string, pre *verified) (any, error) {
	u, err := s.getUpload(ctx, tx, uploadID)
	if err != nil {
		return nil, err
	}
	if err := canWrite(u, who); err != nil {
		return nil, err
	}
	now := s.clock.Now()
	ref := func(e *contracts.Error) *contracts.Error { return e.WithRef(contracts.KindUpload, u.UploadID) }
	switch u.State {
	case StateFinalized:
		return s.receipt(ctx, tx, u.VersionID)
	case StateFailed:
		return nil, ref(contracts.Errorf(contracts.ErrConflict, "upload failed verification (%s); nothing was published", u.Failure))
	case StateExpired:
		return nil, ref(contracts.Errorf(contracts.ErrExpired, "upload expired; nothing was published"))
	}
	if now.After(u.ExpiresAt) {
		if _, err := tx.ExecContext(ctx, `UPDATE artifact_uploads SET state=?, updated_at=? WHERE upload_id=?`, StateExpired, svcutil.Time(now), u.UploadID); err != nil {
			return nil, err
		}
		return nil, svcutil.CommitWith(ref(contracts.Errorf(contracts.ErrExpired, "upload expired; nothing was published")))
	}
	if u.Received < u.Size {
		// Retryable: not stored as the operation's result, so the same
		// operation ID finalizes once the remaining bytes are uploaded.
		e := ref(contracts.Errorf(contracts.ErrConflict, "upload incomplete: received %d of %d bytes", u.Received, u.Size).
			WithDetail("received", u.Received).WithDetail("size", u.Size))
		e.Retryable = true
		return nil, e
	}
	if pre == nil {
		if pre, err = s.prepare(u); err != nil || pre == nil {
			return nil, internalf("verify upload: %v", err)
		}
	}
	if pre.missing && u.Size > 0 {
		return nil, internalf("staged bytes for %s are missing", u.UploadID)
	}
	if pre.size != u.Size || pre.digest != u.Digest {
		reason := "sha256 mismatch"
		if pre.size != u.Size {
			reason = "size mismatch"
		}
		if _, err := tx.ExecContext(ctx, `UPDATE artifact_uploads SET state=?, failure=?, updated_at=? WHERE upload_id=?`, StateFailed, reason, svcutil.Time(now), u.UploadID); err != nil {
			return nil, err
		}
		return nil, svcutil.CommitWith(ref(contracts.Errorf(contracts.ErrConflict, "uploaded bytes do not match declared sha256; nothing was published").
			WithDetail("reason", reason)))
	}

	// Verified: place the blob (content-addressed, so an existing blob with
	// this digest is the same bytes; OpenContent re-verifies on read).
	_, hexd, _ := parseDigest(u.Digest)
	dst := s.blobPath(hexd)
	if !pre.inBlob {
		if err := s.placeBlob(u, dst); err != nil {
			return nil, err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO artifact_blobs(digest, size, created_at, last_verified_at, state) VALUES(?,?,?,?, 'ok')
		ON CONFLICT(digest) DO UPDATE SET last_verified_at=excluded.last_verified_at, state='ok'`,
		string(u.Digest), u.Size, svcutil.Time(now), svcutil.Time(now)); err != nil {
		return nil, err
	}

	mt := u.MediaType
	if mt == "" {
		mt = sniffMediaType(pre.head)
	}
	var art contracts.Artifact
	if u.ArtifactID == "" {
		art = contracts.Artifact{
			ArtifactID:  contracts.NewID(contracts.PrefixArtifact),
			ProjectID:   u.ProjectID,
			SessionID:   u.Provenance.SessionID,
			Title:       u.Title,
			Description: u.Description,
			CreatedBy:   who,
			CreatedAt:   now,
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO artifacts(`+artifactCols+`) VALUES(?,?,?,?,?,?,?,?,?,?)`,
			art.ArtifactID, svcutil.NullProject(art.ProjectID), art.SessionID, art.Title, art.Description, "", 0, svcutil.JSON(who), svcutil.Time(now), svcutil.Time(now)); err != nil {
			return nil, err
		}
	} else if art, err = getArtifactQ(ctx, tx, u.ArtifactID); err != nil {
		return nil, err
	}
	ver := contracts.ArtifactVersion{
		VersionID:         contracts.NewID(contracts.PrefixVersion),
		ArtifactID:        art.ArtifactID,
		Seq:               int64(art.VersionCount) + 1,
		PreviousVersionID: art.LatestVersionID,
		Digest:            u.Digest,
		Size:              u.Size,
		MediaType:         mt,
		Filename:          u.Filename,
		Author:            who,
		Provenance:        u.Provenance,
		CreatedAt:         now,
		Note:              u.Note,
		ReviewRequested:   u.Review,
	}
	ver.ContentURL = ContentURL(ver.VersionID)
	if IsActiveType(ver.MediaType) {
		ver.PreviewURL = PreviewURLTemplate(ver.VersionID)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO artifact_versions(`+versionCols+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		ver.VersionID, ver.ArtifactID, ver.Seq, ver.PreviousVersionID, string(ver.Digest), ver.Size, ver.MediaType, ver.Filename,
		svcutil.JSON(who), svcutil.JSON(ver.Provenance), svcutil.Time(now), ver.Note, boolInt(ver.ReviewRequested)); err != nil {
		return nil, err
	}
	art.LatestVersionID, art.VersionCount, art.UpdatedAt = ver.VersionID, art.VersionCount+1, now
	if _, err := tx.ExecContext(ctx, `UPDATE artifacts SET latest_version_id=?, version_count=?, updated_at=? WHERE artifact_id=?`,
		art.LatestVersionID, art.VersionCount, svcutil.Time(now), art.ArtifactID); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE artifact_uploads SET state=?, artifact_id=?, version_id=?, media_type=?, updated_at=? WHERE upload_id=?`,
		StateFinalized, art.ArtifactID, ver.VersionID, mt, svcutil.Time(now), u.UploadID); err != nil {
		return nil, err
	}
	receipt := contracts.PublishReceipt{Artifact: art, Version: ver}
	if err := svcutil.Emit(ctx, tx, s.sink, now, contracts.EvArtifactPublished, contracts.ObjectRef{Kind: contracts.KindArtifactVersion, ID: ver.VersionID}, art.ProjectID, ver.Provenance.SessionID, receipt); err != nil {
		return nil, err
	}
	return receipt, nil
}

// placeBlob moves verified staged bytes to dst (or discards them if an
// identical blob already exists) and syncs the directory.
func (s *Store) placeBlob(u uploadRow, dst string) error {
	src := s.stagedPath(u.UploadID)
	if fi, err := os.Stat(dst); err == nil && fi.Size() == u.Size {
		if v, herr := hashFile(dst, u.Size); herr == nil && v.digest == u.Digest && v.size == u.Size {
			_ = os.Remove(src)
			return nil
		}
		// Otherwise the file at this content address is corrupt; the
		// verified bytes replace it atomically below.
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	if u.Size == 0 {
		if _, err := os.Stat(src); errors.Is(err, os.ErrNotExist) {
			if err := os.WriteFile(src, nil, 0o600); err != nil {
				return err
			}
		}
	}
	if err := os.Truncate(src, u.Size); err != nil {
		return err
	}
	if err := os.Chmod(src, 0o400); err != nil {
		return err
	}
	if err := os.Rename(src, dst); err != nil {
		return err
	}
	if d, err := os.Open(filepath.Dir(dst)); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}

func (s *Store) receipt(ctx context.Context, q queryer, versionID string) (contracts.PublishReceipt, error) {
	v, err := getVersionQ(ctx, q, versionID)
	if err != nil {
		return contracts.PublishReceipt{}, err
	}
	a, err := getArtifactQ(ctx, q, v.ArtifactID)
	if err != nil {
		return contracts.PublishReceipt{}, err
	}
	return contracts.PublishReceipt{Artifact: a, Version: v}, nil
}

// GetUpload returns an upload's current state (resume offset) to the
// principal that began it or the owner (the same rule as the write paths).
func (s *Store) GetUpload(ctx context.Context, req contracts.GetRequest) (contracts.Upload, error) {
	who, err := svcutil.RequirePrincipal(ctx)
	if err != nil {
		return contracts.Upload{}, err
	}
	u, err := s.getUpload(ctx, s.db, req.ID)
	if err != nil {
		return contracts.Upload{}, err
	}
	if err := canWrite(u, who); err != nil {
		return contracts.Upload{}, err
	}
	return u.Upload, nil
}

// UpdateArtifact edits title/description or (re)associates the artifact
// with a project (ProjectID "" detaches to standalone). Versions, their
// bytes and their recorded provenance are never modified; existing review
// comments keep the project they were created under.
//
// No event is emitted: the pinned v1 event table has no artifact-metadata
// kind, and reusing artifact.published (PublishReceipt) would falsely
// signal a new version / re-raise review attention. Clients refresh via
// GetArtifact/ListArtifacts.
func (s *Store) UpdateArtifact(ctx context.Context, req contracts.UpdateArtifactRequest) (contracts.Artifact, error) {
	var out contracts.Artifact
	if req.ProjectID != nil && *req.ProjectID != "" && s.projects != nil {
		if err := s.projects.ProjectExists(ctx, *req.ProjectID); err != nil {
			return out, err
		}
	}
	err := svcutil.Mutate(ctx, s.db, req.Mutation, contracts.OpUpdateArtifact, req, &out, func(tx *sql.Tx, who contracts.Principal) (any, error) {
		a, err := getArtifactQ(ctx, tx, req.ArtifactID)
		if err != nil {
			return nil, err
		}
		if req.Title != nil {
			t := strings.TrimSpace(*req.Title)
			if t == "" || len(t) > 200 || strings.ContainsAny(t, "\r\n") {
				return nil, contracts.Errorf(contracts.ErrInvalid, "title must be a single line of 1-200 bytes")
			}
			a.Title = t
		}
		if req.Description != nil {
			if len(*req.Description) > 8192 {
				return nil, contracts.Errorf(contracts.ErrInvalid, "description too long")
			}
			a.Description = *req.Description
		}
		if req.ProjectID != nil {
			a.ProjectID = svcutil.CopyProject(req.ProjectID)
		}
		a.UpdatedAt = s.clock.Now()
		if _, err := tx.ExecContext(ctx, `UPDATE artifacts SET title=?, description=?, project_id=?, updated_at=? WHERE artifact_id=?`,
			a.Title, a.Description, svcutil.NullProject(a.ProjectID), svcutil.Time(a.UpdatedAt), a.ArtifactID); err != nil {
			return nil, err
		}
		return a, nil
	})
	return out, err
}

// CleanupExpired marks open uploads past their expiry as expired and
// removes staging files that no open upload owns. Call it periodically.
func (s *Store) CleanupExpired(ctx context.Context) (expired int, err error) {
	now := s.clock.Now()
	rows, err := s.db.QueryContext(ctx, `SELECT upload_id FROM artifact_uploads WHERE state='open' AND expires_at < ?`, svcutil.Time(now))
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		unlock := s.lock(id)
		res, err := s.db.ExecContext(ctx, `UPDATE artifact_uploads SET state=?, updated_at=? WHERE upload_id=? AND state='open' AND expires_at < ?`,
			StateExpired, svcutil.Time(now), id, svcutil.Time(now))
		if err == nil {
			if n, _ := res.RowsAffected(); n == 1 {
				expired++
				_ = os.Remove(s.stagedPath(id))
			}
		}
		unlock()
		if err != nil {
			return expired, err
		}
	}
	entries, err := os.ReadDir(filepath.Join(s.dir, "uploads"))
	if err != nil {
		return expired, err
	}
	for _, e := range entries {
		var state string
		qerr := s.db.QueryRowContext(ctx, `SELECT state FROM artifact_uploads WHERE upload_id=?`, e.Name()).Scan(&state)
		if errors.Is(qerr, sql.ErrNoRows) || (qerr == nil && state != StateOpen) {
			unlock := s.lock(e.Name())
			_ = os.Remove(filepath.Join(s.dir, "uploads", e.Name()))
			unlock()
		}
	}
	return expired, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
