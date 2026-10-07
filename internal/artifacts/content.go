package artifacts

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"hash"
	"io"
	"os"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/projects/svcutil"
)

func corruptErr(versionID string) error {
	return contracts.Errorf(contracts.ErrInternal, "artifact content failed integrity verification; refusing to serve it").
		WithRef(contracts.KindArtifactVersion, versionID)
}

// OpenContent returns the verified immutable bytes of a version. The blob's
// size is always checked and its sha256 is re-verified before returning
// (unless verified within Options.VerifyTTL); a corrupt blob is marked and
// refused. The returned reader also implements io.Seeker (for HTTP Range)
// and verifies the digest again at EOF when read sequentially from the
// start, failing the read instead of reporting a clean EOF on mismatch.
func (s *Store) OpenContent(ctx context.Context, versionID string) (io.ReadCloser, contracts.ArtifactVersion, error) {
	v, err := getVersionQ(ctx, s.db, versionID)
	if err != nil {
		return nil, v, err
	}
	_, hexd, err := parseDigest(v.Digest)
	if err != nil {
		return nil, v, corruptErr(versionID)
	}
	var state, lastVerified string
	if err := s.db.QueryRowContext(ctx, `SELECT state, last_verified_at FROM artifact_blobs WHERE digest=?`, string(v.Digest)).Scan(&state, &lastVerified); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, v, err
	}
	if state == "corrupt" {
		return nil, v, corruptErr(versionID)
	}
	path := s.blobPath(hexd)
	f, err := os.Open(path)
	if err != nil {
		s.markCorrupt(ctx, v.Digest)
		return nil, v, corruptErr(versionID)
	}
	fi, err := f.Stat()
	if err != nil || fi.Size() != v.Size {
		f.Close()
		s.markCorrupt(ctx, v.Digest)
		return nil, v, corruptErr(versionID)
	}
	now := s.clock.Now()
	if s.vttl <= 0 || now.Sub(svcutil.ParseTime(lastVerified)) > s.vttl {
		h := sha256.New()
		if _, err := io.Copy(h, f); err != nil {
			f.Close()
			return nil, v, err
		}
		if "sha256:"+hex.EncodeToString(h.Sum(nil)) != string(v.Digest) {
			f.Close()
			s.markCorrupt(ctx, v.Digest)
			return nil, v, corruptErr(versionID)
		}
		_, _ = s.db.ExecContext(ctx, `UPDATE artifact_blobs SET last_verified_at=? WHERE digest=?`, svcutil.Time(now), string(v.Digest))
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			f.Close()
			return nil, v, err
		}
	}
	return &Content{f: f, want: string(v.Digest), size: v.Size, h: sha256.New(), seq: true, onCorrupt: func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s.markCorrupt(ctx, v.Digest)
	}, versionID: versionID}, v, nil
}

func (s *Store) markCorrupt(ctx context.Context, d contracts.Digest) {
	_, _ = s.db.ExecContext(ctx, `UPDATE artifact_blobs SET state='corrupt' WHERE digest=?`, string(d))
}

// Content is a verified, seekable reader over an immutable blob.
type Content struct {
	f         *os.File
	want      string
	size      int64
	h         hash.Hash
	n         int64
	seq       bool
	onCorrupt func()
	versionID string
}

// Read implements io.Reader. A sequential read whose bytes do not hash to
// the version digest fails at EOF.
func (c *Content) Read(p []byte) (int, error) {
	n, err := c.f.Read(p)
	if c.seq && n > 0 {
		c.h.Write(p[:n])
		c.n += int64(n)
	}
	if errors.Is(err, io.EOF) && c.seq {
		if c.n != c.size || "sha256:"+hex.EncodeToString(c.h.Sum(nil)) != c.want {
			if c.onCorrupt != nil {
				c.onCorrupt()
			}
			return n, corruptErr(c.versionID)
		}
	}
	return n, err
}

// Seek implements io.Seeker. Seeking anywhere but the start disables the
// EOF verification (Range requests are served from an already verified
// blob).
func (c *Content) Seek(offset int64, whence int) (int64, error) {
	pos, err := c.f.Seek(offset, whence)
	if err != nil {
		return pos, err
	}
	if pos == 0 {
		c.h.Reset()
		c.n = 0
		c.seq = true
	} else {
		c.seq = false
	}
	return pos, nil
}

// Close implements io.Closer.
func (c *Content) Close() error { return c.f.Close() }

// VerifyBlobs re-hashes every stored blob (for a periodic integrity sweep),
// marking mismatches corrupt. It returns counts checked and corrupt.
func (s *Store) VerifyBlobs(ctx context.Context) (checked, corrupt int, err error) {
	rows, err := s.db.QueryContext(ctx, `SELECT digest, size FROM artifact_blobs`)
	if err != nil {
		return 0, 0, err
	}
	type blob struct {
		d    contracts.Digest
		size int64
	}
	var blobs []blob
	for rows.Next() {
		var b blob
		if err := rows.Scan(&b.d, &b.size); err != nil {
			rows.Close()
			return 0, 0, err
		}
		blobs = append(blobs, b)
	}
	rows.Close()
	for _, b := range blobs {
		if ctx.Err() != nil {
			return checked, corrupt, ctx.Err()
		}
		_, hexd, perr := parseDigest(b.d)
		ok := perr == nil
		if ok {
			v, herr := hashFile(s.blobPath(hexd), b.size)
			ok = herr == nil && v.size == b.size && v.digest == b.d
		}
		checked++
		if !ok {
			corrupt++
			s.markCorrupt(ctx, b.d)
			continue
		}
		_, _ = s.db.ExecContext(ctx, `UPDATE artifact_blobs SET last_verified_at=?, state='ok' WHERE digest=?`, svcutil.Time(s.clock.Now()), string(b.d))
	}
	return checked, corrupt, nil
}
