// Package backup writes and restores coherent hub backups: a transactionally
// consistent SQLite snapshot (VACUUM INTO), the published artifact blobs,
// and the hub TLS identity, packed in a tar with a digest manifest.
//
// Coherence: the database snapshot is taken first. Artifact blobs are
// content addressed and immutable, and a version is committed only after
// its blob is complete, so every blob referenced by the snapshot exists when
// the blob directory is copied afterwards; blobs published after the
// snapshot are harmless extras. Unfinished uploads are never included.
//
// The archive contains the hub's TLS private key and private project
// material. It is an age file (crypt.go) encrypted while the tar is
// produced, so no plaintext archive ever reaches the disk; the only
// plaintext staging is the database snapshot, kept inside the hub data
// directory (same protection as the live database) and removed afterwards.
// Restore detects age, the legacy pyre format and plaintext tars from the
// archive header, and verifies every digest before the restored directory
// appears.
package backup

import (
	"archive/tar"
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"filippo.io/age"

	"github.com/jhh3/pyre/internal/buildinfo"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

// Layout inside DataDir (mirrors internal/hub/assemble).
const (
	DBFile       = "hub.db"
	BlobsDir     = "artifacts/blobs"
	TLSDir       = "tls"
	ManifestName = "MANIFEST.json"
	FormatV1     = "pyre-hub-backup/1"
	// stagePrefix names the snapshot staging directory inside DataDir.
	stagePrefix = ".pyre-backup-"
)

// Entry is one archived file.
type Entry struct {
	Path   string `json:"path"` // slash-separated, relative to DataDir
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	Mode   uint32 `json:"mode"`
}

// Manifest describes an archive.
type Manifest struct {
	Format     string            `json:"format"`
	CreatedAt  time.Time         `json:"created_at"`
	HubID      string            `json:"hub_id"`
	HubVersion string            `json:"hub_version"`
	Schema     map[string]int    `json:"schema"` // migration namespace -> version
	Entries    []Entry           `json:"entries"`
	Notes      map[string]string `json:"notes,omitempty"`
}

func sha256File(p string) (string, int64, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// Options configures Create, Restore and Verify.
type Options struct {
	// Passphrase: Create encrypts to an age scrypt recipient; Restore and
	// Verify try it as an age scrypt identity and for legacy archives.
	Passphrase []byte
	// Recipients (Create): age public keys; every one can decrypt.
	Recipients []age.Recipient
	// Unencrypted (Create) explicitly allows a plaintext archive. Without
	// it, Create refuses when there is no passphrase and no recipient.
	Unencrypted bool
	// Identities (Restore, Verify): age private keys to try.
	Identities []age.Identity
	// ScryptLogN overrides the scrypt work factor (2^N) of new passphrase
	// archives; 0 uses age's default (2^18, ~1 s and 256 MiB). Tests only.
	ScryptLogN int
	// Verify (Create, file output only): after writing, decrypt the archive
	// again as a stream and check every digest, writing nothing. Passphrase
	// archives use the passphrase. Recipient archives get one extra,
	// ephemeral recipient whose private key exists only in this process's
	// memory for the check and is then dropped.
	Verify bool
}

// Create writes a backup of dataDir to out (created 0600, must not exist).
// The archive is written as out+".partial" (already encrypted) and renamed
// into place once complete and, with opts.Verify, verified.
func Create(ctx context.Context, dataDir, out string, opts Options) (Manifest, error) {
	if _, err := os.Stat(out); err == nil {
		return Manifest{}, fmt.Errorf("backup: %s already exists", out)
	}
	var verifyID age.Identity
	if opts.Verify && len(opts.Recipients) > 0 {
		var err error
		var r age.Recipient
		if verifyID, r, err = ephemeralRecipient(opts.Recipients); err != nil {
			return Manifest{}, err
		}
		opts.Recipients = append(append([]age.Recipient(nil), opts.Recipients...), r)
	}
	tmp := out + ".partial"
	fo, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return Manifest{}, err
	}
	ok := false
	defer func() {
		if !ok {
			fo.Close()
			os.Remove(tmp)
		}
	}()
	bw := bufio.NewWriterSize(fo, 256<<10)
	m, err := CreateTo(ctx, dataDir, bw, opts)
	if err != nil {
		return m, err
	}
	if err := bw.Flush(); err != nil {
		return m, err
	}
	if err := fo.Sync(); err != nil {
		return m, err
	}
	if err := fo.Close(); err != nil {
		return m, err
	}
	if opts.Verify {
		vo := Options{Passphrase: opts.Passphrase}
		if verifyID != nil {
			vo.Identities = []age.Identity{verifyID}
		}
		vm, err := Verify(ctx, tmp, vo)
		if err != nil {
			return m, fmt.Errorf("backup: verification of the written archive failed: %w", err)
		}
		if vm.HubID != m.HubID || len(vm.Entries) != len(m.Entries) {
			return m, errors.New("backup: verification read a different manifest")
		}
	}
	if _, err := os.Stat(out); err == nil {
		return m, fmt.Errorf("backup: %s already exists", out)
	}
	if err := os.Rename(tmp, out); err != nil {
		return m, err
	}
	ok = true
	return m, nil
}

// ephemeralRecipient returns an in-memory key pair compatible with rs
// (hybrid when rs are post-quantum, X25519 otherwise) for Verify.
func ephemeralRecipient(rs []age.Recipient) (age.Identity, age.Recipient, error) {
	if hasHybrid(rs) {
		id, err := age.GenerateHybridIdentity()
		if err != nil {
			return nil, nil, err
		}
		return id, id.Recipient(), nil
	}
	id, err := age.GenerateX25519Identity()
	if err != nil {
		return nil, nil, err
	}
	return id, id.Recipient(), nil
}

// CreateTo streams a backup of dataDir to w (for example stdout or a pipe
// into another encryption tool). The tar is encrypted as it is produced.
func CreateTo(ctx context.Context, dataDir string, w io.Writer, opts Options) (Manifest, error) {
	var m Manifest
	recips, err := opts.recipients()
	if err != nil {
		return m, err
	}
	dbPath := filepath.Join(dataDir, DBFile)
	if _, err := os.Stat(dbPath); err != nil {
		return m, fmt.Errorf("backup: no hub database at %s: %w", dbPath, err)
	}
	// The snapshot is plaintext: keep it next to the live database (same
	// directory permissions), never next to the output, which may be a
	// synced or shared directory.
	stage, err := os.MkdirTemp(dataDir, stagePrefix+"*")
	if err != nil {
		return m, err
	}
	defer os.RemoveAll(stage)
	snap := filepath.Join(stage, DBFile)
	db, err := sqlitex.Open(dbPath)
	if err != nil {
		return m, err
	}
	err = sqlitex.Backup(ctx, db, snap)
	db.Close()
	if err != nil {
		return m, fmt.Errorf("backup: snapshot database: %w", err)
	}
	m = Manifest{Format: FormatV1, CreatedAt: time.Now().UTC(), HubVersion: buildinfo.Version, Schema: map[string]int{}}
	if err := readMeta(ctx, snap, &m); err != nil {
		return m, err
	}
	m.Notes = map[string]string{"db_integrity_check": "ok"}

	type src struct{ rel, abs string }
	files := []src{{DBFile, snap}}
	for _, dir := range []string{BlobsDir, TLSDir} {
		root := filepath.Join(dataDir, filepath.FromSlash(dir))
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return nil
				}
				return err
			}
			if d.IsDir() || !d.Type().IsRegular() {
				return nil
			}
			rel, err := filepath.Rel(dataDir, p)
			if err != nil {
				return err
			}
			files = append(files, src{filepath.ToSlash(rel), p})
			return nil
		})
		if err != nil {
			return m, err
		}
	}
	sort.Slice(files[1:], func(i, j int) bool { return files[1+i].rel < files[1+j].rel })
	for _, f := range files {
		sum, size, err := sha256File(f.abs)
		if err != nil {
			return m, err
		}
		st, err := os.Stat(f.abs)
		if err != nil {
			return m, err
		}
		m.Entries = append(m.Entries, Entry{Path: f.rel, Size: size, SHA256: sum, Mode: uint32(st.Mode().Perm())})
	}

	sink := w
	var aw io.WriteCloser
	if len(recips) > 0 {
		if aw, err = age.Encrypt(w, recips...); err != nil {
			return m, fmt.Errorf("backup: age: %w", err)
		}
		sink = aw
	}
	tw := tar.NewWriter(sink)
	mb, _ := json.MarshalIndent(m, "", "  ")
	if err := tw.WriteHeader(&tar.Header{Name: ManifestName, Mode: 0o600, Size: int64(len(mb)), ModTime: m.CreatedAt, Typeflag: tar.TypeReg}); err != nil {
		return m, err
	}
	if _, err := tw.Write(mb); err != nil {
		return m, err
	}
	for i, f := range files {
		if err := ctx.Err(); err != nil {
			return m, err
		}
		if err := addFile(tw, f.abs, m.Entries[i]); err != nil {
			return m, err
		}
	}
	if err := tw.Close(); err != nil {
		return m, err
	}
	if aw != nil {
		// Writes the final, authenticated age chunk.
		if err := aw.Close(); err != nil {
			return m, err
		}
	}
	return m, nil
}

func addFile(tw *tar.Writer, abs string, e Entry) error {
	f, err := os.Open(abs)
	if err != nil {
		return err
	}
	defer f.Close()
	// Re-verify while copying: a file changing under us must not produce a
	// manifest that lies.
	h := sha256.New()
	if err := tw.WriteHeader(&tar.Header{Name: e.Path, Mode: int64(e.Mode), Size: e.Size, Typeflag: tar.TypeReg, ModTime: time.Now().UTC()}); err != nil {
		return err
	}
	n, err := io.Copy(io.MultiWriter(tw, h), io.LimitReader(f, e.Size))
	if err != nil {
		return err
	}
	if n != e.Size || hex.EncodeToString(h.Sum(nil)) != e.SHA256 {
		return fmt.Errorf("backup: %s changed while archiving", e.Path)
	}
	return nil
}

func readMeta(ctx context.Context, dbPath string, m *Manifest) error {
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro")
	if err != nil {
		return err
	}
	defer db.Close()
	var integrity string
	if err := db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
		return fmt.Errorf("backup: snapshot integrity check failed: %v %s", err, integrity)
	}
	_ = db.QueryRowContext(ctx, `SELECT value FROM hub_meta WHERE key='hub_id'`).Scan(&m.HubID)
	rows, err := db.QueryContext(ctx, `SELECT namespace, MAX(version) FROM schema_migrations GROUP BY namespace`)
	if err != nil {
		return fmt.Errorf("backup: read schema versions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var ns string
		var v int
		if err := rows.Scan(&ns, &v); err != nil {
			return err
		}
		m.Schema[ns] = v
	}
	return rows.Err()
}

// safeRel validates an archive path.
func safeRel(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") {
		return false
	}
	c := path.Clean(p)
	return c == p && c != "." && !strings.HasPrefix(c, "../") && c != ".."
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

// readArchive reads a (decrypted) archive stream, checking the manifest and
// every entry's size and digest. open returns where an entry's bytes go.
// After the last entry the stream is drained so truncation of an encrypted
// archive (a missing final chunk) and trailing garbage are detected.
func readArchive(ctx context.Context, src io.Reader, open func(Entry) (io.WriteCloser, error)) (Manifest, error) {
	var m Manifest
	tr := tar.NewReader(src)
	hdr, err := tr.Next()
	if err != nil {
		return m, fmt.Errorf("read manifest: %w", err)
	}
	if hdr.Name != ManifestName || hdr.Size > 64<<20 {
		return m, fmt.Errorf("archive does not start with %s", ManifestName)
	}
	if err := json.NewDecoder(io.LimitReader(tr, hdr.Size)).Decode(&m); err != nil {
		return m, fmt.Errorf("decode manifest: %w", err)
	}
	if m.Format != FormatV1 {
		return m, fmt.Errorf("unsupported backup format %q", m.Format)
	}
	want := map[string]Entry{}
	for _, e := range m.Entries {
		if !safeRel(e.Path) {
			return m, fmt.Errorf("unsafe path %q in manifest", e.Path)
		}
		want[e.Path] = e
	}
	if _, ok := want[DBFile]; !ok {
		return m, fmt.Errorf("manifest lacks %s", DBFile)
	}
	seen := map[string]bool{}
	for {
		if err := ctx.Err(); err != nil {
			return m, err
		}
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return m, fmt.Errorf("read archive: %w", err)
		}
		e, ok := want[hdr.Name]
		if !ok || hdr.Typeflag != tar.TypeReg || seen[hdr.Name] {
			return m, fmt.Errorf("unexpected archive entry %q", hdr.Name)
		}
		seen[hdr.Name] = true
		out, err := open(e)
		if err != nil {
			return m, err
		}
		h := sha256.New()
		n, err := io.Copy(io.MultiWriter(out, h), io.LimitReader(tr, e.Size+1))
		cerr := out.Close()
		if err != nil {
			return m, fmt.Errorf("read %s: %w", e.Path, err)
		}
		if cerr != nil {
			return m, cerr
		}
		if n != e.Size || hex.EncodeToString(h.Sum(nil)) != e.SHA256 {
			return m, fmt.Errorf("%s failed verification (size %d/%d)", e.Path, n, e.Size)
		}
	}
	for p := range want {
		if !seen[p] {
			return m, fmt.Errorf("archive is missing %s", p)
		}
	}
	// Authenticate the end of the stream (age/legacy final chunk). A plain
	// tar may carry zero padding after the end marker; nothing else.
	if _, err := io.Copy(zeroChecker{}, src); err != nil {
		return m, fmt.Errorf("archive end: %w", err)
	}
	return m, nil
}

type zeroChecker struct{}

func (zeroChecker) Write(p []byte) (int, error) {
	for _, b := range p {
		if b != 0 {
			return 0, errors.New("unexpected data after the end of the archive")
		}
	}
	return len(p), nil
}

// Verify decrypts archive as a stream and checks the manifest and every
// digest without writing anything to disk. (The database integrity check
// ran on the snapshot when the archive was created; Restore runs it again
// on the restored copy.)
func Verify(ctx context.Context, archive string, opts Options) (Manifest, error) {
	f, err := os.Open(archive)
	if err != nil {
		return Manifest{}, err
	}
	defer f.Close()
	m, _, err := VerifyFrom(ctx, f, opts)
	return m, err
}

// VerifyFrom is Verify reading from r; it also returns the detected format.
func VerifyFrom(ctx context.Context, r io.Reader, opts Options) (Manifest, Format, error) {
	src, format, err := openArchive(r, opts)
	if err != nil {
		return Manifest{}, format, fmt.Errorf("verify: %w", err)
	}
	m, err := readArchive(ctx, src, func(Entry) (io.WriteCloser, error) { return nopWriteCloser{io.Discard}, nil })
	if err != nil {
		return m, format, fmt.Errorf("verify: %w", err)
	}
	return m, format, nil
}

// Restore extracts archive into dataDir (which must not exist or be
// empty), verifying every file against the manifest. Nothing is placed in
// dataDir unless the whole archive verifies. Encrypted archives need a
// matching opts.Identities entry or opts.Passphrase.
func Restore(ctx context.Context, archive, dataDir string, opts Options) (Manifest, error) {
	f, err := os.Open(archive)
	if err != nil {
		return Manifest{}, err
	}
	defer f.Close()
	m, _, err := RestoreFrom(ctx, f, dataDir, opts)
	return m, err
}

// RestoreFrom is Restore reading the archive from r (a file or a pipe,
// e.g. from `gpg --decrypt`); it also returns the detected format.
func RestoreFrom(ctx context.Context, r io.Reader, dataDir string, opts Options) (Manifest, Format, error) {
	var m Manifest
	if ents, err := os.ReadDir(dataDir); err == nil && len(ents) > 0 {
		return m, "", fmt.Errorf("restore: %s is not empty; restore into a new directory", dataDir)
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return m, "", err
	}
	src, format, err := openArchive(r, opts)
	if err != nil {
		return m, format, fmt.Errorf("restore: %w", err)
	}
	parent := filepath.Dir(filepath.Clean(dataDir))
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return m, format, err
	}
	// The stage is a sibling of dataDir (0700) so the final rename is
	// atomic; it holds the restored, not-yet-verified files.
	stage, err := os.MkdirTemp(parent, ".pyre-restore-*")
	if err != nil {
		return m, format, err
	}
	defer os.RemoveAll(stage)
	m, err = readArchive(ctx, src, func(e Entry) (io.WriteCloser, error) {
		dst := filepath.Join(stage, filepath.FromSlash(e.Path))
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return nil, err
		}
		mode := os.FileMode(e.Mode).Perm()
		if mode == 0 {
			mode = 0o600
		}
		return os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	})
	if err != nil {
		return m, format, fmt.Errorf("restore: %w", err)
	}
	// The database must be intact.
	db, err := sql.Open("sqlite", "file:"+filepath.Join(stage, DBFile)+"?mode=ro")
	if err != nil {
		return m, format, err
	}
	var integrity string
	err = db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&integrity)
	db.Close()
	if err != nil || integrity != "ok" {
		return m, format, fmt.Errorf("restore: database integrity check failed: %v %s", err, integrity)
	}
	if err := os.Remove(dataDir); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return m, format, err
	}
	if err := os.Chmod(stage, 0o700); err != nil {
		return m, format, err
	}
	if err := os.Rename(stage, dataDir); err != nil {
		return m, format, err
	}
	return m, format, nil
}
