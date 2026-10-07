package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

func dg(b []byte) contracts.Digest {
	s := sha256.Sum256(b)
	return contracts.Digest("sha256:" + hex.EncodeToString(s[:]))
}

func open(t *testing.T, max int64) *Cache {
	t.Helper()
	c, err := Open(filepath.Join(t.TempDir(), "cache"), max)
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
	c.now = func() time.Time { clock = clock.Add(time.Second); return clock }
	return c
}

func TestVersionRoundTripAndVerification(t *testing.T) {
	c := open(t, 0)
	data := []byte("# Report\n\nAll synthetic tests pass.\n")
	v := contracts.ArtifactVersion{VersionID: "ver_synth-report-1", ArtifactID: "art_synth-report", Digest: dg(data), Size: int64(len(data)), Filename: "../../report.md"}
	if err := c.PutVersion(v, strings.NewReader(string(data))); err != nil {
		t.Fatal(err)
	}
	got, rc, err := c.GetVersion(v.VersionID)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != string(data) || got.Digest != v.Digest {
		t.Fatal("round trip mismatch")
	}
	// Wrong bytes are refused and nothing is stored.
	bad := contracts.ArtifactVersion{VersionID: "ver_synth-bad", Digest: dg([]byte("other")), Size: 5}
	if err := c.PutVersion(bad, strings.NewReader("wrong")); !contracts.IsCode(err, contracts.ErrConflict) {
		t.Fatalf("want conflict, got %v", err)
	}
	if c.Has(bad.Digest) {
		t.Fatal("mismatched bytes cached")
	}
	if _, _, err := c.GetVersion("ver_synth-bad"); !contracts.IsCode(err, contracts.ErrNotFound) {
		t.Fatalf("want not found, got %v", err)
	}
	// Corruption on disk is detected on read and the entry discarded.
	h := string(v.Digest)[7:]
	p := filepath.Join(c.dir, "blobs", "sha256", h[:2], h[2:])
	os.WriteFile(p, []byte("tampered"), 0o600)
	if _, _, err := c.GetVersion(v.VersionID); !contracts.IsCode(err, contracts.ErrNotFound) {
		t.Fatalf("corrupt read: %v", err)
	}
	if c.Has(v.Digest) {
		t.Fatal("corrupt blob kept")
	}
	// IDs cannot traverse.
	if _, _, err := c.GetVersion("../../etc/passwd"); !contracts.IsCode(err, contracts.ErrInvalid) {
		t.Fatalf("traversal id: %v", err)
	}
	if _, err := c.Put("sha256:../../x", strings.NewReader("")); !contracts.IsCode(err, contracts.ErrInvalid) {
		t.Fatalf("traversal digest: %v", err)
	}
}

func TestRevisionRoundTrip(t *testing.T) {
	c := open(t, 0)
	content := "# Brief\n\nGoal: X.\n"
	rev := contracts.DocumentRevision{RevisionID: "rev_synth-brief-1", DocumentID: "doc_synth-brief", Seq: 1, Digest: dg([]byte(content)), Size: int64(len(content)), Content: content}
	if err := c.PutRevision(rev); err != nil {
		t.Fatal(err)
	}
	got, err := c.GetRevision(rev.RevisionID)
	if err != nil || got.Content != content || got.Seq != 1 {
		t.Fatalf("revision: %v %+v", err, got)
	}
	rev.Content = "tampered"
	if err := c.PutRevision(rev); !contracts.IsCode(err, contracts.ErrConflict) {
		t.Fatalf("want conflict, got %v", err)
	}
}

func TestLRUEviction(t *testing.T) {
	c := open(t, 25)
	a, b, d := []byte(strings.Repeat("a", 10)), []byte(strings.Repeat("b", 10)), []byte(strings.Repeat("d", 10))
	for _, x := range [][]byte{a, b} {
		if _, err := c.Put(dg(x), strings.NewReader(string(x))); err != nil {
			t.Fatal(err)
		}
	}
	// Touch a so b becomes least recently used.
	rc, _, err := c.Open(dg(a))
	if err != nil {
		t.Fatal(err)
	}
	rc.Close()
	if _, err := c.Put(dg(d), strings.NewReader(string(d))); err != nil {
		t.Fatal(err)
	}
	if !c.Has(dg(a)) || c.Has(dg(b)) || !c.Has(dg(d)) {
		t.Fatalf("eviction order wrong: a=%v b=%v d=%v", c.Has(dg(a)), c.Has(dg(b)), c.Has(dg(d)))
	}
	if u, _ := c.Usage(); u > 25 {
		t.Fatalf("usage %d over bound", u)
	}
	big := []byte(strings.Repeat("z", 26))
	if _, err := c.Put(dg(big), strings.NewReader(string(big))); !contracts.IsCode(err, contracts.ErrPayloadTooLarge) {
		t.Fatalf("oversize: %v", err)
	}
}
