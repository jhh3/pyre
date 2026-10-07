// Package cache is a client-side, digest-verified, size-bounded cache of
// published artifact versions and document revisions, used by the CLI (and
// other clients) for offline reading when the hub is unreachable.
//
// Layout under the cache directory (0700):
//
//	blobs/sha256/<hex[0:2]>/<hex[2:]>   verified bytes, addressed by digest
//	refs/artifact_version/<id>.json     ArtifactVersion manifest -> digest
//	refs/document_revision/<id>.json    DocumentRevision metadata (no content) -> digest
//
// Bytes are verified against their sha256 digest when stored and again
// whenever they are read; a corrupt entry is deleted and reported as a
// miss. Total blob bytes are bounded; the least recently used blobs (by
// access time recorded in the file mtime) are evicted first. Paths derive
// only from validated digests and IDs, never from filenames.
//
//	c, err := cache.Open(filepath.Join(stateDir, "cache"), 512<<20)
//	c.PutVersion(version, body)        // after a download from /v1/content/<id>
//	v, rc, err := c.GetVersion(id)     // offline
package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// DefaultMaxBytes bounds the cache when Open is given maxBytes <= 0.
const DefaultMaxBytes = 1 << 30

// Cache is safe for concurrent use within a process; cross-process writers
// are safe because every file is written to a temp name and renamed.
type Cache struct {
	dir string
	max int64
	mu  sync.Mutex
	now func() time.Time
}

// Open creates (if needed) and opens a cache rooted at dir bounded to
// maxBytes of blob data.
func Open(dir string, maxBytes int64) (*Cache, error) {
	if dir == "" {
		return nil, errors.New("cache: empty dir")
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	for _, d := range []string{"blobs/sha256", "refs/artifact_version", "refs/document_revision", "tmp"} {
		if err := os.MkdirAll(filepath.Join(dir, filepath.FromSlash(d)), 0o700); err != nil {
			return nil, err
		}
	}
	return &Cache{dir: dir, max: maxBytes, now: time.Now}, nil
}

func hexOf(d contracts.Digest) (string, error) {
	s := strings.ToLower(string(d))
	h, ok := strings.CutPrefix(s, "sha256:")
	if !ok || len(h) != 64 {
		return "", contracts.Errorf(contracts.ErrInvalid, "cache: digest must be sha256:<64 hex>")
	}
	if _, err := hex.DecodeString(h); err != nil {
		return "", contracts.Errorf(contracts.ErrInvalid, "cache: digest must be sha256:<64 hex>")
	}
	return h, nil
}

func (c *Cache) blobPath(h string) string {
	return filepath.Join(c.dir, "blobs", "sha256", h[:2], h[2:])
}

func (c *Cache) refPath(kind contracts.ObjectKind, prefix, id string) (string, error) {
	if !contracts.ValidID(id, prefix) {
		return "", contracts.Errorf(contracts.ErrInvalid, "cache: invalid %s id %q", kind, id)
	}
	return filepath.Join(c.dir, "refs", string(kind), id+".json"), nil
}

func (c *Cache) writeAtomic(dst string, write func(*os.File) error) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Join(c.dir, "tmp"), "put-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := write(tmp); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, dst)
}

// Put verifies r's bytes against digest and stores them. A mismatch stores
// nothing and returns ErrConflict; data larger than the cache bound returns
// ErrPayloadTooLarge.
func (c *Cache) Put(d contracts.Digest, r io.Reader) (int64, error) {
	h, err := hexOf(d)
	if err != nil {
		return 0, err
	}
	var n int64
	err = c.writeAtomic(c.blobPath(h), func(f *os.File) error {
		hh := sha256.New()
		var cerr error
		n, cerr = io.Copy(io.MultiWriter(f, hh), io.LimitReader(r, c.max+1))
		if cerr != nil {
			return cerr
		}
		if n > c.max {
			return contracts.Errorf(contracts.ErrPayloadTooLarge, "cache: object exceeds cache bound %d", c.max)
		}
		if hex.EncodeToString(hh.Sum(nil)) != h {
			return contracts.Errorf(contracts.ErrConflict, "cache: bytes do not match digest %s", d)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	now := c.now()
	_ = os.Chtimes(c.blobPath(h), now, now)
	c.mu.Lock()
	defer c.mu.Unlock()
	return n, c.evictLocked(h)
}

// Has reports whether a blob is present (not verified).
func (c *Cache) Has(d contracts.Digest) bool {
	h, err := hexOf(d)
	if err != nil {
		return false
	}
	_, err = os.Stat(c.blobPath(h))
	return err == nil
}

// Open returns the verified bytes for digest. A missing or corrupt entry
// returns ErrNotFound (corrupt entries are removed).
func (c *Cache) Open(d contracts.Digest) (io.ReadCloser, int64, error) {
	h, err := hexOf(d)
	if err != nil {
		return nil, 0, err
	}
	p := c.blobPath(h)
	f, err := os.Open(p)
	if err != nil {
		return nil, 0, contracts.Errorf(contracts.ErrNotFound, "cache: %s not cached", d)
	}
	hh := sha256.New()
	n, err := io.Copy(hh, f)
	if err != nil || hex.EncodeToString(hh.Sum(nil)) != h {
		f.Close()
		os.Remove(p)
		return nil, 0, contracts.Errorf(contracts.ErrNotFound, "cache: %s was corrupt and has been discarded", d)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		f.Close()
		return nil, 0, err
	}
	now := c.now()
	_ = os.Chtimes(p, now, now) // LRU access time
	return f, n, nil
}

type versionRef struct {
	Version contracts.ArtifactVersion `json:"version"`
}

// PutVersion verifies and stores a version's bytes plus its manifest.
func (c *Cache) PutVersion(v contracts.ArtifactVersion, r io.Reader) error {
	p, err := c.refPath(contracts.KindArtifactVersion, contracts.PrefixVersion, v.VersionID)
	if err != nil {
		return err
	}
	n, err := c.Put(v.Digest, r)
	if err != nil {
		return err
	}
	if n != v.Size {
		return contracts.Errorf(contracts.ErrConflict, "cache: size %d does not match manifest %d", n, v.Size)
	}
	return c.writeJSON(p, versionRef{Version: v})
}

// GetVersion returns a cached version manifest and its verified bytes.
func (c *Cache) GetVersion(versionID string) (contracts.ArtifactVersion, io.ReadCloser, error) {
	var ref versionRef
	p, err := c.refPath(contracts.KindArtifactVersion, contracts.PrefixVersion, versionID)
	if err != nil {
		return ref.Version, nil, err
	}
	if err := c.readJSON(p, &ref); err != nil {
		return ref.Version, nil, err
	}
	rc, _, err := c.Open(ref.Version.Digest)
	return ref.Version, rc, err
}

// PutRevision stores a document revision; its Content must hash to Digest.
func (c *Cache) PutRevision(rev contracts.DocumentRevision) error {
	p, err := c.refPath(contracts.KindDocumentRevision, contracts.PrefixRevision, rev.RevisionID)
	if err != nil {
		return err
	}
	if _, err := c.Put(rev.Digest, strings.NewReader(rev.Content)); err != nil {
		return err
	}
	meta := rev
	meta.Content = ""
	return c.writeJSON(p, meta)
}

// GetRevision returns a cached revision with verified content.
func (c *Cache) GetRevision(revisionID string) (contracts.DocumentRevision, error) {
	var rev contracts.DocumentRevision
	p, err := c.refPath(contracts.KindDocumentRevision, contracts.PrefixRevision, revisionID)
	if err != nil {
		return rev, err
	}
	if err := c.readJSON(p, &rev); err != nil {
		return rev, err
	}
	rc, _, err := c.Open(rev.Digest)
	if err != nil {
		return rev, err
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		return rev, err
	}
	rev.Content = string(b)
	return rev, nil
}

func (c *Cache) writeJSON(p string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.writeAtomic(p, func(f *os.File) error { _, err := f.Write(b); return err })
}

func (c *Cache) readJSON(p string, v any) error {
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return contracts.Errorf(contracts.ErrNotFound, "cache: %s not cached", strings.TrimSuffix(filepath.Base(p), ".json"))
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

type blobInfo struct {
	path  string
	hex   string
	size  int64
	atime time.Time
}

func (c *Cache) blobs() ([]blobInfo, int64, error) {
	var out []blobInfo
	var total int64
	root := filepath.Join(c.dir, "blobs", "sha256")
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		out = append(out, blobInfo{path: p, hex: strings.ReplaceAll(filepath.ToSlash(rel), "/", ""), size: fi.Size(), atime: fi.ModTime()})
		total += fi.Size()
		return nil
	})
	return out, total, err
}

// Usage returns the total cached blob bytes.
func (c *Cache) Usage() (int64, error) {
	_, total, err := c.blobs()
	return total, err
}

// evictLocked removes least-recently-used blobs until the cache is within
// its bound, never evicting keep (the blob just stored).
func (c *Cache) evictLocked(keep string) error {
	list, total, err := c.blobs()
	if err != nil || total <= c.max {
		return err
	}
	sort.Slice(list, func(i, j int) bool { return list[i].atime.Before(list[j].atime) })
	for _, b := range list {
		if total <= c.max {
			break
		}
		if b.hex == keep {
			continue
		}
		if err := os.Remove(b.path); err == nil {
			total -= b.size
		}
	}
	return nil
}
