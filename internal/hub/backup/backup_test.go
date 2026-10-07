package backup

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"filippo.io/age"
	"filippo.io/age/armor"
	"golang.org/x/crypto/ssh"

	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

// makeHub creates a synthetic hub data directory: a database with the
// tables Create reads, some artifact blobs (one of blobSize bytes that
// starts with a unique marker) and a synthetic TLS directory. It returns
// the directory and the marker.
func makeHub(t testing.TB, base string, blobSize int) (string, []byte) {
	t.Helper()
	dir := filepath.Join(base, "hub")
	for _, d := range []string{BlobsDir + "/sha256/ab", TLSDir} {
		if err := os.MkdirAll(filepath.Join(dir, filepath.FromSlash(d)), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	db, err := sqlitex.Open(filepath.Join(dir, DBFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE hub_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`INSERT INTO hub_meta VALUES ('hub_id', 'hub_synthetic-backup-test')`,
		`CREATE TABLE schema_migrations (namespace TEXT, version INTEGER, name TEXT, applied_at TEXT, PRIMARY KEY (namespace, version))`,
		`INSERT INTO schema_migrations VALUES ('hub', 1, 'init', '2026-10-04T00:00:00Z'), ('hub', 2, 'more', '2026-10-04T00:00:00Z')`,
		`CREATE TABLE notes (body TEXT)`,
		`INSERT INTO notes VALUES ('synthetic database row SYNTH-DB-ROW')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	marker := []byte(fmt.Sprintf("SYNTHETIC-PLAINTEXT-MARKER-%d-", time.Now().UnixNano()))
	big := make([]byte, blobSize)
	rand.Read(big)
	copy(big, marker)
	// Repeat the marker throughout so any plaintext fragment is detectable.
	for off := 0; off+len(marker) <= len(big); off += 32 << 10 {
		copy(big[off:], marker)
	}
	files := map[string][]byte{
		BlobsDir + "/sha256/ab/big":   big,
		BlobsDir + "/sha256/ab/small": []byte("synthetic small blob"),
		BlobsDir + "/sha256/ab/empty": nil,
		TLSDir + "/hub.key":           []byte("SYNTHETIC TEST KEY, NOT A REAL PRIVATE KEY"),
		TLSDir + "/hub.crt":           []byte("synthetic certificate"),
	}
	for p, b := range files {
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(p)), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir, marker
}

func assertNoDir(t *testing.T, p string) {
	t.Helper()
	if _, err := os.Stat(p); err == nil {
		t.Fatalf("%s exists after a failed restore", p)
	}
}

// assertSameTree checks that every blob/TLS file of src is in dst with the
// same bytes and that dst's database opens with the synthetic row.
func assertSameTree(t *testing.T, src, dst string) {
	t.Helper()
	for _, d := range []string{BlobsDir, TLSDir} {
		filepath.WalkDir(filepath.Join(src, filepath.FromSlash(d)), func(p string, e fs.DirEntry, err error) error {
			if err != nil || e.IsDir() {
				return err
			}
			rel, _ := filepath.Rel(src, p)
			a, _ := os.ReadFile(p)
			b, err := os.ReadFile(filepath.Join(dst, rel))
			if err != nil || !bytes.Equal(a, b) {
				t.Fatalf("%s differs after restore: %v", rel, err)
			}
			return nil
		})
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dst, DBFile)+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var body string
	if err := db.QueryRow(`SELECT body FROM notes`).Scan(&body); err != nil || !strings.Contains(body, "SYNTH-DB-ROW") {
		t.Fatalf("restored database: %v %q", err, body)
	}
	if st, err := os.Stat(dst); err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("restored dir mode: %v", err)
	}
}

func assertAgeFile(t *testing.T, path string, marker []byte) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(raw, []byte(ageMagic)) {
		t.Fatalf("%s is not an age file: %q", path, raw[:min(len(raw), 40)])
	}
	for _, s := range [][]byte{marker, []byte(ManifestName), []byte("SYNTHETIC TEST KEY"), []byte("SYNTH-DB-ROW")} {
		if bytes.Contains(raw, s) {
			t.Fatalf("archive contains plaintext %q", s)
		}
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Fatalf("archive mode %v", st.Mode().Perm())
	}
	return raw
}

func mustX25519(t *testing.T) *age.X25519Identity {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func mustHybrid(t *testing.T) *age.HybridIdentity {
	id, err := age.GenerateHybridIdentity()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

const testLogN = 10 // fast scrypt for tests; production uses age's default

func TestRoundTrips(t *testing.T) {
	ctx := context.Background()
	x1, x2, x3 := mustX25519(t), mustX25519(t), mustX25519(t)
	h1, h2, h3 := mustHybrid(t), mustHybrid(t), mustHybrid(t)
	pass := []byte("synthetic backup passphrase")
	cases := []struct {
		name   string
		create Options
		good   []Options // each must restore alone
		bad    []Options // each must fail with ErrWrongKey
	}{
		{"passphrase", Options{Passphrase: pass, ScryptLogN: testLogN},
			[]Options{{Passphrase: pass}},
			[]Options{{Passphrase: []byte("wrong")}, {Identities: []age.Identity{x1}}}},
		{"x25519", Options{Recipients: []age.Recipient{x1.Recipient()}},
			[]Options{{Identities: []age.Identity{x1}}, {Identities: []age.Identity{x3, x1}}},
			[]Options{{Identities: []age.Identity{x2}}, {Passphrase: pass}}},
		{"hybrid-pq", Options{Recipients: []age.Recipient{h1.Recipient()}},
			[]Options{{Identities: []age.Identity{h1}}},
			[]Options{{Identities: []age.Identity{h2}}, {Identities: []age.Identity{x1}}}},
		{"multi-x25519", Options{Recipients: []age.Recipient{x1.Recipient(), x2.Recipient()}},
			[]Options{{Identities: []age.Identity{x1}}, {Identities: []age.Identity{x2}}},
			[]Options{{Identities: []age.Identity{x3}}}},
		{"multi-hybrid", Options{Recipients: []age.Recipient{h1.Recipient(), h2.Recipient()}},
			[]Options{{Identities: []age.Identity{h1}}, {Identities: []age.Identity{h2}}},
			[]Options{{Identities: []age.Identity{h3}}}},
		{"x25519-verify", Options{Recipients: []age.Recipient{x1.Recipient()}, Verify: true},
			[]Options{{Identities: []age.Identity{x1}}}, nil},
		{"hybrid-verify", Options{Recipients: []age.Recipient{h1.Recipient()}, Verify: true},
			[]Options{{Identities: []age.Identity{h1}}}, nil},
		{"passphrase-verify", Options{Passphrase: pass, ScryptLogN: testLogN, Verify: true},
			[]Options{{Passphrase: pass}}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			data, marker := makeHub(t, base, 200<<10)
			out := filepath.Join(base, "out", "b.tar.age")
			os.MkdirAll(filepath.Dir(out), 0o700)
			m, err := Create(ctx, data, out, tc.create)
			if err != nil {
				t.Fatal(err)
			}
			if m.HubID != "hub_synthetic-backup-test" || m.Schema["hub"] != 2 || len(m.Entries) != 6 {
				t.Fatalf("manifest: %+v", m)
			}
			assertAgeFile(t, out, marker)
			if ents, _ := os.ReadDir(filepath.Dir(out)); len(ents) != 1 {
				t.Fatalf("output dir has extra files: %v", ents)
			}
			if _, err := Create(ctx, data, out, tc.create); err == nil {
				t.Fatal("Create overwrote an existing archive")
			}
			if _, err := Restore(ctx, out, filepath.Join(base, "nokey"), Options{}); !errors.Is(err, ErrKeyRequired) {
				t.Fatalf("no key: %v", err)
			}
			for i, o := range tc.bad {
				target := filepath.Join(base, fmt.Sprintf("bad%d", i))
				_, err := Restore(ctx, out, target, o)
				if !errors.Is(err, ErrWrongKey) {
					t.Fatalf("bad key %d: %v", i, err)
				}
				if len(o.Passphrase) > 0 && tc.name == "passphrase" && !strings.Contains(err.Error(), "wrong passphrase") {
					t.Fatalf("wrong passphrase message: %v", err)
				}
				assertNoDir(t, target)
			}
			for i, o := range tc.good {
				if _, err := Verify(ctx, out, o); err != nil {
					t.Fatalf("verify %d: %v", i, err)
				}
				target := filepath.Join(base, fmt.Sprintf("restored%d", i))
				got, err := Restore(ctx, out, target, o)
				if err != nil {
					t.Fatalf("restore %d: %v", i, err)
				}
				if got.HubID != m.HubID {
					t.Fatal("hub id")
				}
				assertSameTree(t, data, target)
			}
			if matches, _ := filepath.Glob(filepath.Join(data, stagePrefix+"*")); len(matches) > 0 {
				t.Fatalf("snapshot stage left behind: %v", matches)
			}
		})
	}
}

func TestEncryptionOptionsRejected(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	data, _ := makeHub(t, base, 1024)
	x, h := mustX25519(t), mustHybrid(t)
	for name, o := range map[string]Options{
		"none":               {},
		"mixed-pq-classic":   {Recipients: []age.Recipient{x.Recipient(), h.Recipient()}},
		"passphrase+recip":   {Passphrase: []byte("p"), Recipients: []age.Recipient{x.Recipient()}},
		"unencrypted+recip":  {Unencrypted: true, Recipients: []age.Recipient{x.Recipient()}},
		"unencrypted+passph": {Unencrypted: true, Passphrase: []byte("p")},
	} {
		out := filepath.Join(base, name+".tar")
		_, err := Create(ctx, data, out, o)
		if err == nil || !(errors.Is(err, ErrNoEncryption) || errors.Is(err, ErrMixedRecipients)) {
			t.Fatalf("%s: %v", name, err)
		}
		if _, err := os.Stat(out); err == nil {
			t.Fatalf("%s: archive written", name)
		}
		if _, err := os.Stat(out + ".partial"); err == nil {
			t.Fatalf("%s: partial left", name)
		}
	}
	// Explicit plaintext still works and restores.
	out := filepath.Join(base, "plain.tar")
	if _, err := Create(ctx, data, out, Options{Unencrypted: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(ctx, out, filepath.Join(base, "r"), Options{}); err != nil {
		t.Fatal(err)
	}
}

func TestTamperAndTruncationRejected(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	data, _ := makeHub(t, base, 300<<10)
	id := mustX25519(t)
	out := filepath.Join(base, "b.tar.age")
	if _, err := Create(ctx, data, out, Options{Recipients: []age.Recipient{id.Recipient()}}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(out)
	hdrEnd := bytes.Index(raw, []byte("\n---"))
	hdrEnd += bytes.IndexByte(raw[hdrEnd+1:], '\n') + 2
	payload := hdrEnd + 16 // after the stream nonce
	const chunk = 64<<10 + 16
	variants := map[string][]byte{
		"flip-header":          flip(raw, hdrEnd-5),
		"flip-first-chunk":     flip(raw, payload+100),
		"flip-middle":          flip(raw, len(raw)/2),
		"flip-last-byte":       flip(raw, len(raw)-1),
		"truncate-header":      raw[:hdrEnd-3],
		"truncate-nonce":       raw[:payload-4],
		"truncate-chunk-bound": raw[:payload+2*chunk],
		"truncate-mid":         raw[:len(raw)/2],
		"truncate-last-byte":   raw[:len(raw)-1],
		"trailing-garbage":     append(append([]byte{}, raw...), []byte("garbage")...),
	}
	for name, b := range variants {
		p := filepath.Join(base, name+".age")
		os.WriteFile(p, b, 0o600)
		target := filepath.Join(base, "r-"+name)
		if _, err := Restore(ctx, p, target, Options{Identities: []age.Identity{id}}); err == nil {
			t.Fatalf("%s: restore succeeded", name)
		} else {
			t.Logf("%s: %v", name, err)
		}
		assertNoDir(t, target)
		if _, err := Verify(ctx, p, Options{Identities: []age.Identity{id}}); err == nil {
			t.Fatalf("%s: verify succeeded", name)
		}
	}
	// Leftover staging dirs are cleaned up.
	if m, _ := filepath.Glob(filepath.Join(base, ".pyre-restore-*")); len(m) > 0 {
		t.Fatalf("restore stage left: %v", m)
	}
}

// errAfter yields b, then err instead of io.EOF: what a decrypting reader
// does when everything up to the tar end marker authenticated but the
// final chunk is missing.
type errAfter struct {
	r   *bytes.Reader
	err error
}

func (e *errAfter) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	if errors.Is(err, io.EOF) {
		return n, e.err
	}
	return n, err
}

// The stream is read to its end after the last tar entry, so a missing
// final chunk (or trailing non-zero data) fails the restore even though
// every entry verified.
func TestArchiveEndIsAuthenticated(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	data, _ := makeHub(t, base, 10<<10)
	var tarBuf bytes.Buffer
	if _, err := CreateTo(ctx, data, &tarBuf, Options{Unencrypted: true}); err != nil {
		t.Fatal(err)
	}
	discard := func(Entry) (io.WriteCloser, error) { return nopWriteCloser{io.Discard}, nil }
	if _, err := readArchive(ctx, bytes.NewReader(tarBuf.Bytes()), discard); err != nil {
		t.Fatalf("clean stream: %v", err)
	}
	truncated := errors.New("synthetic: final chunk missing")
	if _, err := readArchive(ctx, &errAfter{bytes.NewReader(tarBuf.Bytes()), truncated}, discard); !errors.Is(err, truncated) {
		t.Fatalf("missing final chunk not detected: %v", err)
	}
	junk := append(append([]byte{}, tarBuf.Bytes()...), make([]byte, 4096)...)
	if _, err := readArchive(ctx, bytes.NewReader(junk), discard); err != nil {
		t.Fatalf("zero padding rejected: %v", err)
	}
	junk = append(junk, 'x')
	if _, err := readArchive(ctx, bytes.NewReader(junk), discard); err == nil {
		t.Fatal("trailing data accepted")
	}
}

func flip(b []byte, i int) []byte {
	c := append([]byte{}, b...)
	c[i] ^= 0x01
	return c
}

// No plaintext archive is ever written: while Create (and its verify pass)
// runs, a watcher scans the output directory and TMPDIR. Every file it sees
// must be ciphertext (age header) and must never contain the plaintext
// marker repeated through the large blob. The only plaintext staging is
// the snapshot inside the hub data directory, which is removed afterwards.
func TestNoPlaintextFileWritten(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	data, marker := makeHub(t, base, 24<<20)
	outDir := filepath.Join(base, "out")
	tmpDir := filepath.Join(base, "tmp")
	os.MkdirAll(outDir, 0o700)
	os.MkdirAll(tmpDir, 0o700)
	t.Setenv("TMPDIR", tmpDir)
	id := mustHybrid(t)

	var stop atomic.Bool
	var polls, sawPartial atomic.Int64
	var mu sync.Mutex
	var problems []string
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			polls.Add(1)
			for _, dir := range []string{outDir, tmpDir} {
				filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
					if err != nil || e.IsDir() {
						return nil
					}
					b, err := os.ReadFile(p)
					if err != nil || len(b) == 0 {
						return nil
					}
					if strings.HasSuffix(p, ".partial") {
						sawPartial.Add(1)
					}
					if !bytes.HasPrefix(b, []byte(ageMagic)[:min(len(b), len(ageMagic))]) || bytes.Contains(b, marker) {
						mu.Lock()
						problems = append(problems, fmt.Sprintf("%s (%d bytes) is not ciphertext", p, len(b)))
						mu.Unlock()
					}
					return nil
				})
			}
			time.Sleep(time.Millisecond)
		}
	}()
	out := filepath.Join(outDir, "b.tar.age")
	_, err := Create(ctx, data, out, Options{Recipients: []age.Recipient{id.Recipient()}, Verify: true})
	stop.Store(true)
	wg.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) > 0 {
		t.Fatalf("plaintext seen: %v", problems)
	}
	if sawPartial.Load() == 0 {
		t.Fatalf("watcher never observed the archive being written (%d polls); test is not meaningful", polls.Load())
	}
	assertAgeFile(t, out, marker)
	if ents, _ := os.ReadDir(tmpDir); len(ents) > 0 {
		t.Fatalf("TMPDIR not empty: %v", ents)
	}
	if m, _ := filepath.Glob(filepath.Join(data, stagePrefix+"*")); len(m) > 0 {
		t.Fatalf("snapshot stage left: %v", m)
	}
	t.Logf("watcher: %d polls, %d observations of the growing .partial (all ciphertext)", polls.Load(), sawPartial.Load())
}

// Create and Restore stream: peak Go heap stays far below the size of a
// large blob.
func TestLargeBlobStreamingBoundedMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("large blob")
	}
	ctx := context.Background()
	base := t.TempDir()
	const size = 96 << 20
	data, _ := makeHub(t, base, size)
	id := mustX25519(t)
	out := filepath.Join(base, "big.tar.age")

	measure := func(fn func() error) (uint64, error) {
		runtime.GC()
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		baseline := ms.HeapAlloc
		var peak atomic.Uint64
		done := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			var s runtime.MemStats
			for {
				select {
				case <-done:
					return
				case <-time.After(2 * time.Millisecond):
				}
				runtime.ReadMemStats(&s)
				if s.HeapAlloc > baseline && s.HeapAlloc-baseline > peak.Load() {
					peak.Store(s.HeapAlloc - baseline)
				}
			}
		}()
		err := fn()
		close(done)
		wg.Wait()
		return peak.Load(), err
	}
	const limit = 32 << 20
	pc, err := measure(func() error {
		_, err := Create(ctx, data, out, Options{Recipients: []age.Recipient{id.Recipient()}})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	pr, err := measure(func() error {
		_, err := Restore(ctx, out, filepath.Join(base, "restored"), Options{Identities: []age.Identity{id}})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("blob %d MiB: peak heap growth create %.1f MiB, restore %.1f MiB", size>>20, float64(pc)/(1<<20), float64(pr)/(1<<20))
	if pc > limit || pr > limit {
		t.Fatalf("memory not bounded: create %d, restore %d bytes over baseline", pc, pr)
	}
}

// Restore reads from a non-seekable stream (e.g. `gpg --decrypt |`), and
// an ASCII-armored age file is accepted.
func TestRestoreFromPipeAndArmor(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	data, _ := makeHub(t, base, 100<<10)
	id := mustX25519(t)
	// Plaintext tar over a pipe.
	pr, pw := io.Pipe()
	go func() {
		_, err := CreateTo(ctx, data, pw, Options{Unencrypted: true})
		pw.CloseWithError(err)
	}()
	if _, f, err := RestoreFrom(ctx, pr, filepath.Join(base, "piped"), Options{}); err != nil || f != FormatPlain {
		t.Fatalf("pipe: %v %s", err, f)
	}
	assertSameTree(t, data, filepath.Join(base, "piped"))
	// Armored age file.
	var buf bytes.Buffer
	aw := armor.NewWriter(&buf)
	if _, err := CreateTo(ctx, data, aw, Options{Recipients: []age.Recipient{id.Recipient()}}); err != nil {
		t.Fatal(err)
	}
	aw.Close()
	if _, f, err := RestoreFrom(ctx, &buf, filepath.Join(base, "armored"), Options{Identities: []age.Identity{id}}); err != nil || f != FormatAgeArmor {
		t.Fatalf("armor: %v %s", err, f)
	}
	assertSameTree(t, data, filepath.Join(base, "armored"))
}

func TestRecipientAndIdentityFiles(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	data, _ := makeHub(t, base, 10<<10)
	x, h := mustX25519(t), mustHybrid(t)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	sshPub, _ := ssh.NewPublicKey(pub)
	sshLine := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))) + " synthetic@pyre.invalid"
	blk, err := ssh.MarshalPrivateKey(priv, "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	sshKey := filepath.Join(base, "id_ed25519")
	os.WriteFile(sshKey, pem.EncodeToMemory(blk), 0o600)

	rf := filepath.Join(base, "recipients.txt")
	os.WriteFile(rf, []byte("# synthetic recipients\n\n"+x.Recipient().String()+"\n"+sshLine+"\n"), 0o600)
	rs, err := ReadRecipientsFile(rf)
	if err != nil || len(rs) != 2 {
		t.Fatalf("recipients file: %v %d", err, len(rs))
	}
	if _, err := ParseRecipient(x.String()); err == nil || !strings.Contains(err.Error(), "PRIVATE") {
		t.Fatalf("private key accepted as recipient: %v", err)
	}
	if _, err := ParseRecipient("bogus"); err == nil {
		t.Fatal("bogus recipient accepted")
	}
	if r, err := ParseRecipient(h.Recipient().String()); err != nil {
		t.Fatal(err)
	} else if _, ok := r.(*age.HybridRecipient); !ok {
		t.Fatalf("age1pq1 parsed as %T", r)
	}
	out := filepath.Join(base, "b.tar.age")
	if _, err := Create(ctx, data, out, Options{Recipients: rs}); err != nil {
		t.Fatal(err)
	}
	idf := filepath.Join(base, "key.txt")
	os.WriteFile(idf, []byte("# created: synthetic\n# public key: "+x.Recipient().String()+"\n"+x.String()+"\n"), 0o600)
	for _, f := range []string{idf, sshKey} {
		ids, err := ReadIdentityFile(f)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if _, err := Restore(ctx, out, filepath.Join(base, "r-"+filepath.Base(f)), Options{Identities: ids}); err != nil {
			t.Fatalf("restore with %s: %v", f, err)
		}
	}
}

// ageBin finds the stock age CLI: $PYRE_TEST_AGE_BIN_DIR, then PATH.
func ageBin(t *testing.T, name string) string {
	if d := os.Getenv("PYRE_TEST_AGE_BIN_DIR"); d != "" {
		if p := filepath.Join(d, name); fileExists(p) {
			return p
		}
	}
	p, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s CLI not installed (set PYRE_TEST_AGE_BIN_DIR)", name)
	}
	return p
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

// tarNames lists the entries of a plaintext tar.
func tarNames(t *testing.T, b []byte) []string {
	t.Helper()
	var names []string
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return names
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
	}
}

// Interop with the stock age CLI in both directions, for X25519, hybrid
// post-quantum and passphrase (via age-plugin-batchpass) archives.
func TestAgeCLIInterop(t *testing.T) {
	ageCLI := ageBin(t, "age")
	ctx := context.Background()
	base := t.TempDir()
	data, _ := makeHub(t, base, 100<<10)
	x, h := mustX25519(t), mustHybrid(t)
	for _, tc := range []struct {
		name string
		id   fmt.Stringer
		r    age.Recipient
	}{{"x25519", x, x.Recipient()}, {"hybrid", h, h.Recipient()}} {
		out := filepath.Join(base, tc.name+".tar.age")
		if _, err := Create(ctx, data, out, Options{Recipients: []age.Recipient{tc.r}}); err != nil {
			t.Fatal(err)
		}
		key := filepath.Join(base, tc.name+".key")
		os.WriteFile(key, []byte(tc.id.String()+"\n"), 0o600)
		cmd := exec.Command(ageCLI, "-d", "-i", key, out)
		plain, err := cmd.Output()
		if err != nil {
			t.Fatalf("%s: age -d: %v", tc.name, err)
		}
		names := tarNames(t, plain)
		if len(names) == 0 || names[0] != ManifestName {
			t.Fatalf("%s: age -d | tar -t: %v", tc.name, names)
		}
		t.Logf("%s: age -d -i key | tar -t -> %d entries, first %s", tc.name, len(names), names[0])
		// Reverse: the age CLI encrypts a plaintext tar; pyre restores it.
		var tarBuf bytes.Buffer
		if _, err := CreateTo(ctx, data, &tarBuf, Options{Unencrypted: true}); err != nil {
			t.Fatal(err)
		}
		cliOut := filepath.Join(base, tc.name+"-cli.tar.age")
		enc := exec.Command(ageCLI, "-r", tc.r.(fmt.Stringer).String(), "-o", cliOut)
		enc.Stdin = &tarBuf
		if b, err := enc.CombinedOutput(); err != nil {
			t.Fatalf("age -r: %v %s", err, b)
		}
		ids, _ := ReadIdentityFile(key)
		if _, err := Restore(ctx, cliOut, filepath.Join(base, "r-"+tc.name), Options{Identities: ids}); err != nil {
			t.Fatalf("%s: restore of age-CLI file: %v", tc.name, err)
		}
	}
	// Passphrase: needs age-plugin-batchpass beside age for non-interactive use.
	plugin := filepath.Join(filepath.Dir(ageCLI), "age-plugin-batchpass")
	if !fileExists(plugin) {
		t.Log("age-plugin-batchpass not found; passphrase CLI interop skipped")
		return
	}
	out := filepath.Join(base, "pass.tar.age")
	if _, err := Create(ctx, data, out, Options{Passphrase: []byte("synthetic interop passphrase"), ScryptLogN: testLogN}); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(ageCLI, "-d", "-j", "batchpass", out)
	cmd.Env = append(os.Environ(), "AGE_PASSPHRASE=synthetic interop passphrase", "PATH="+filepath.Dir(ageCLI)+string(os.PathListSeparator)+os.Getenv("PATH"))
	plain, err := cmd.Output()
	if err != nil {
		t.Fatalf("age -d -j batchpass: %v", err)
	}
	if names := tarNames(t, plain); len(names) == 0 || names[0] != ManifestName {
		t.Fatalf("passphrase: %v", names)
	}
	t.Log("passphrase: age -d -j batchpass | tar -t OK")
}
