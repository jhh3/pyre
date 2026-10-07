package artifacts

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/projects/svcutil/svctest"
)

type fixture struct {
	s     *Store
	sink  *svctest.Sink
	clock *svctest.FixedClock
	dir   string
}

type fakeProjects map[string]bool

func (f fakeProjects) ProjectExists(ctx context.Context, id string) error {
	if f[id] {
		return nil
	}
	return contracts.Errorf(contracts.ErrNotFound, "project not found")
}

func newFixture(t *testing.T, mod func(*Options)) fixture {
	t.Helper()
	db := svctest.OpenDB(t)
	sink := svctest.NewSink(t, db)
	clock := &svctest.FixedClock{T: time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)}
	dir := filepath.Join(t.TempDir(), "artifacts")
	opts := Options{DataDir: dir, Clock: clock, Projects: fakeProjects{"prj_synth-outfit": true}}
	if mod != nil {
		mod(&opts)
	}
	s, err := New(context.Background(), db, sink, opts)
	if err != nil {
		t.Fatal(err)
	}
	return fixture{s: s, sink: sink, clock: clock, dir: dir}
}

func digestOf(b []byte) contracts.Digest {
	sum := sha256.Sum256(b)
	return contracts.Digest("sha256:" + hex.EncodeToString(sum[:]))
}

func mustCode(t *testing.T, err error, code contracts.ErrorCode) *contracts.Error {
	t.Helper()
	if !contracts.IsCode(err, code) {
		t.Fatalf("want %s, got %v", code, err)
	}
	return contracts.AsError(err)
}

func ptr(s string) *string { return &s }

func (f fixture) publish(t *testing.T, ctx context.Context, req contracts.BeginUploadRequest, data []byte) contracts.PublishReceipt {
	t.Helper()
	req.Mutation = svctest.Op()
	req.Size = int64(len(data))
	if req.Digest == "" {
		req.Digest = digestOf(data)
	}
	u, err := f.s.BeginUpload(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.UploadChunk(ctx, u.UploadID, 0, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	r, err := f.s.FinalizeUpload(ctx, contracts.FinalizeUploadRequest{Mutation: svctest.Op(), UploadID: u.UploadID})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func readAll(t *testing.T, s *Store, versionID string) []byte {
	t.Helper()
	rc, _, err := s.OpenContent(context.Background(), versionID)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestPublishStandaloneAndRead(t *testing.T) {
	f := newFixture(t, nil)
	ctx := svctest.Session("ses_synth-b")
	data := []byte("# Report\n\nAll synthetic tests pass.\n")
	r := f.publish(t, ctx, contracts.BeginUploadRequest{Title: "Synthetic test report", Filename: "report.md",
		Provenance: contracts.Provenance{RunID: "run_synth-b-1", NodeID: "node_synth-linux", SourcePath: "/synthetic/report.md"}}, data)
	v := r.Version
	if v.Seq != 1 || v.PreviousVersionID != "" || v.Digest != digestOf(data) || v.Size != int64(len(data)) || v.MediaType != "text/markdown" ||
		v.Filename != "report.md" || v.ContentURL != "/v1/content/"+v.VersionID || v.PreviewURL != "" {
		t.Fatalf("version: %+v", v)
	}
	if v.Provenance.SessionID != "ses_synth-b" || v.Provenance.NodeID != "node_synth-linux" || v.Author.ID != "ses_synth-b" {
		t.Fatalf("provenance: %+v", v.Provenance)
	}
	if r.Artifact.ProjectID != nil || r.Artifact.SessionID != "ses_synth-b" || r.Artifact.VersionCount != 1 || r.Artifact.LatestVersionID != v.VersionID {
		t.Fatalf("artifact: %+v", r.Artifact)
	}
	if got := readAll(t, f.s, v.VersionID); !bytes.Equal(got, data) {
		t.Fatalf("content mismatch: %q", got)
	}
	evs := f.sink.Events(t, contracts.EvArtifactPublished)
	if len(evs) != 1 || evs[0].Object.ID != v.VersionID || evs[0].ProjectID != nil || evs[0].SessionID != "ses_synth-b" {
		t.Fatalf("events: %+v", evs)
	}
	var receipt contracts.PublishReceipt // pinned v1 payload shape
	if err := json.Unmarshal(evs[0].Payload, &receipt); err != nil || receipt.Version.VersionID != v.VersionID || receipt.Artifact.ArtifactID != r.Artifact.ArtifactID || receipt.Version.ReviewRequested {
		t.Fatalf("payload: %v %s", err, evs[0].Payload)
	}
	rr := f.publish(t, ctx, contracts.BeginUploadRequest{ArtifactID: r.Artifact.ArtifactID, Filename: "report.md", ReviewRequested: true}, []byte("v2"))
	if !rr.Version.ReviewRequested {
		t.Fatal("review_requested not carried to the version")
	}
	if got, _ := f.s.GetVersion(ctx, contracts.GetRequest{ID: rr.Version.VersionID}); !got.ReviewRequested {
		t.Fatal("review_requested not stored")
	}
	evs = f.sink.Events(t, contracts.EvArtifactPublished)
	if err := json.Unmarshal(evs[1].Payload, &receipt); err != nil || !receipt.Version.ReviewRequested {
		t.Fatalf("review_requested payload: %s", evs[1].Payload)
	}
	got, err := f.s.GetVersion(ctx, contracts.GetRequest{ID: v.VersionID})
	if err != nil || got.Digest != v.Digest {
		t.Fatalf("get version: %v", err)
	}
	standalone, _ := f.s.ListArtifacts(ctx, contracts.ListArtifactsRequest{ProjectID: ptr("")})
	bySession, _ := f.s.ListArtifacts(ctx, contracts.ListArtifactsRequest{SessionID: "ses_synth-b"})
	inProject, _ := f.s.ListArtifacts(ctx, contracts.ListArtifactsRequest{ProjectID: ptr("prj_synth-outfit")})
	if len(standalone.Items) != 1 || len(bySession.Items) != 1 || len(inProject.Items) != 0 {
		t.Fatal("list filters wrong")
	}
	// Reachable with no producer involvement: the bytes live in hub storage.
	if _, err := os.Stat(filepath.Join(f.dir, "blobs", "sha256", string(v.Digest)[7:9], string(v.Digest)[9:])); err != nil {
		t.Fatalf("blob not in content-addressed store: %v", err)
	}
}

func TestProjectAssociation(t *testing.T) {
	f := newFixture(t, nil)
	ctx := svctest.Owner()
	r := f.publish(t, ctx, contracts.BeginUploadRequest{Title: "x", ProjectID: ptr("prj_synth-outfit"), Filename: "a.txt"}, []byte("a"))
	if r.Artifact.ProjectID == nil || *r.Version.Provenance.ProjectID != "prj_synth-outfit" {
		t.Fatalf("project: %+v", r)
	}
	_, err := f.s.BeginUpload(ctx, contracts.BeginUploadRequest{Mutation: svctest.Op(), Title: "x", ProjectID: ptr("prj_nope"), Filename: "a", Digest: digestOf(nil)})
	mustCode(t, err, contracts.ErrNotFound)
	_, err = f.s.BeginUpload(ctx, contracts.BeginUploadRequest{Mutation: svctest.Op(), ArtifactID: r.Artifact.ArtifactID, ProjectID: ptr("prj_other"), Filename: "a", Digest: digestOf(nil)})
	mustCode(t, err, contracts.ErrInvalid)
	_, err = f.s.BeginUpload(ctx, contracts.BeginUploadRequest{Mutation: svctest.Op(), ArtifactID: "art_missing", Filename: "a", Digest: digestOf(nil)})
	mustCode(t, err, contracts.ErrNotFound)
}

func TestUpdateArtifactAttachLater(t *testing.T) {
	f := newFixture(t, nil)
	ctx := svctest.Session("ses_synth-b")
	r := f.publish(t, ctx, contracts.BeginUploadRequest{Title: "scratch", Filename: "a.txt"}, []byte("standalone bytes"))
	before := len(f.sink.Events(t))
	req := contracts.UpdateArtifactRequest{Mutation: svctest.Op(), ArtifactID: r.Artifact.ArtifactID, ProjectID: ptr("prj_synth-outfit"), Title: ptr("Promoted report")}
	a, err := f.s.UpdateArtifact(ctx, req)
	if err != nil || a.ProjectID == nil || *a.ProjectID != "prj_synth-outfit" || a.Title != "Promoted report" || a.LatestVersionID != r.Version.VersionID {
		t.Fatalf("update: %v %+v", err, a)
	}
	if again, err := f.s.UpdateArtifact(ctx, req); err != nil || again.UpdatedAt != a.UpdatedAt {
		t.Fatalf("replay: %v", err)
	}
	inProject, _ := f.s.ListArtifacts(ctx, contracts.ListArtifactsRequest{ProjectID: ptr("prj_synth-outfit")})
	if len(inProject.Items) != 1 {
		t.Fatal("attached artifact not listed in project")
	}
	v, _ := f.s.GetVersion(ctx, contracts.GetRequest{ID: r.Version.VersionID})
	if v.Provenance.ProjectID != nil || v.Digest != r.Version.Digest || string(readAll(t, f.s, v.VersionID)) != "standalone bytes" {
		t.Fatalf("version changed: %+v", v)
	}
	if n := len(f.sink.Events(t)); n != before {
		t.Fatalf("metadata update emitted %d events", n-before)
	}
	_, err = f.s.UpdateArtifact(ctx, contracts.UpdateArtifactRequest{Mutation: svctest.Op(), ArtifactID: r.Artifact.ArtifactID, ProjectID: ptr("prj_nope")})
	mustCode(t, err, contracts.ErrNotFound)
	_, err = f.s.UpdateArtifact(ctx, contracts.UpdateArtifactRequest{Mutation: svctest.Op(), ArtifactID: r.Artifact.ArtifactID, Title: ptr(" ")})
	mustCode(t, err, contracts.ErrInvalid)
	_, err = f.s.UpdateArtifact(ctx, contracts.UpdateArtifactRequest{Mutation: svctest.Op(), ArtifactID: "art_missing", Title: ptr("x")})
	mustCode(t, err, contracts.ErrNotFound)
	d, err := f.s.UpdateArtifact(ctx, contracts.UpdateArtifactRequest{Mutation: svctest.Op(), ArtifactID: r.Artifact.ArtifactID, ProjectID: ptr("")})
	if err != nil || d.ProjectID != nil {
		t.Fatalf("detach: %v %+v", err, d)
	}
	// A later version is published into the artifact's current project.
	r2 := f.publish(t, ctx, contracts.BeginUploadRequest{ArtifactID: r.Artifact.ArtifactID, Filename: "a.txt"}, []byte("v2"))
	if r2.Artifact.ProjectID != nil || r2.Version.Seq != 2 {
		t.Fatalf("v2 after detach: %+v", r2)
	}
}

type failingReader struct {
	data  []byte
	after int
	pos   int
}

func (r *failingReader) Read(p []byte) (int, error) {
	if r.pos >= r.after {
		return 0, errors.New("synthetic network drop")
	}
	n := copy(p, r.data[r.pos:r.after])
	r.pos += n
	return n, nil
}

func TestResumableUpload(t *testing.T) {
	f := newFixture(t, nil)
	ctx := svctest.Owner()
	data := bytes.Repeat([]byte("0123456789abcdef"), 20000) // 320 KB, several buffers
	u, err := f.s.BeginUpload(ctx, contracts.BeginUploadRequest{Mutation: svctest.Op(), Title: "big", Filename: "big.bin", Size: int64(len(data)), Digest: digestOf(data)})
	if err != nil {
		t.Fatal(err)
	}
	// Interrupted mid-chunk: progress is kept.
	up, err := f.s.UploadChunk(ctx, u.UploadID, 0, &failingReader{data: data, after: 100000})
	mustCode(t, err, contracts.ErrUnavailable)
	if up.Received != 100000 {
		t.Fatalf("received after drop = %d", up.Received)
	}
	// Re-sending an identical overlapping range is idempotent.
	up, err = f.s.UploadChunk(ctx, u.UploadID, 50000, bytes.NewReader(data[50000:100000]))
	if err != nil || up.Received != 100000 {
		t.Fatalf("identical resend: %v %d", err, up.Received)
	}
	// Overlapping different bytes conflict and change nothing.
	bad := append([]byte{}, data[90000:110000]...)
	bad[0] ^= 0xff
	_, err = f.s.UploadChunk(ctx, u.UploadID, 90000, bytes.NewReader(bad))
	mustCode(t, err, contracts.ErrConflict)
	// A gap is refused.
	_, err = f.s.UploadChunk(ctx, u.UploadID, 200000, bytes.NewReader(data[200000:]))
	e := mustCode(t, err, contracts.ErrConflict)
	if e.Details["received"] != int64(100000) {
		t.Fatalf("gap details: %v", e.Details)
	}
	// Finalize before completion: not published, not stored as the op result.
	fin := contracts.FinalizeUploadRequest{Mutation: svctest.Op(), UploadID: u.UploadID}
	_, err = f.s.FinalizeUpload(ctx, fin)
	mustCode(t, err, contracts.ErrConflict)
	if page, _ := f.s.ListArtifacts(ctx, contracts.ListArtifactsRequest{}); len(page.Items) != 0 {
		t.Fatal("incomplete upload visible to readers")
	}
	// Resume from an overlapping offset and finish.
	up, err = f.s.UploadChunk(ctx, u.UploadID, 99000, bytes.NewReader(data[99000:]))
	if err != nil || up.Received != int64(len(data)) {
		t.Fatalf("resume: %v %d", err, up.Received)
	}
	// Extra bytes beyond the declared size are refused.
	_, err = f.s.UploadChunk(ctx, u.UploadID, int64(len(data))-1, bytes.NewReader([]byte("ff")))
	mustCode(t, err, contracts.ErrPayloadTooLarge)
	r, err := f.s.FinalizeUpload(ctx, fin) // same operation ID now succeeds
	if err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, f.s, r.Version.VersionID); !bytes.Equal(got, data) {
		t.Fatal("resumed content mismatch")
	}
	// Replays return the same receipt; staging file is gone.
	r2, err := f.s.FinalizeUpload(ctx, fin)
	if err != nil || r2.Version.VersionID != r.Version.VersionID {
		t.Fatalf("replay: %v", err)
	}
	r3, err := f.s.FinalizeUpload(ctx, contracts.FinalizeUploadRequest{Mutation: svctest.Op(), UploadID: u.UploadID})
	if err != nil || r3.Version.VersionID != r.Version.VersionID {
		t.Fatalf("finalize again with new op: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "uploads", u.UploadID)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("staging file remains: %v", err)
	}
	if n := len(f.sink.Events(t, contracts.EvArtifactPublished)); n != 1 {
		t.Fatalf("published events = %d", n)
	}
}

// A chunk that extends past the declared size is rejected whole: none of
// its bytes count as received, and the upload can still complete
// correctly afterwards.
func TestOversizeChunkRejectedWhole(t *testing.T) {
	f := newFixture(t, nil)
	ctx := svctest.Owner()
	data := []byte("synth-0123")
	u, err := f.s.BeginUpload(ctx, contracts.BeginUploadRequest{Mutation: svctest.Op(), Title: "o", Filename: "o.bin", Size: int64(len(data)), Digest: digestOf(data)})
	if err != nil {
		t.Fatal(err)
	}
	over := append(append([]byte{}, data...), "XXXX"...)
	up, err := f.s.UploadChunk(ctx, u.UploadID, 0, bytes.NewReader(over))
	e := mustCode(t, err, contracts.ErrPayloadTooLarge)
	if up.Received != 0 || e.Details["received"] != int64(0) {
		t.Fatalf("oversize chunk stored bytes: received=%d details=%v", up.Received, e.Details)
	}
	if g, _ := f.s.GetUpload(ctx, contracts.GetRequest{ID: u.UploadID}); g.Received != 0 {
		t.Fatalf("persisted received = %d", g.Received)
	}
	// Partially received, then an oversize chunk: received is unchanged.
	if up, err = f.s.UploadChunk(ctx, u.UploadID, 0, bytes.NewReader(data[:4])); err != nil || up.Received != 4 {
		t.Fatalf("prefix: %v %d", err, up.Received)
	}
	big := bytes.Repeat([]byte("Z"), 3*chunkBuf)
	up, err = f.s.UploadChunk(ctx, u.UploadID, 4, bytes.NewReader(append(append([]byte{}, data[4:]...), big...)))
	mustCode(t, err, contracts.ErrPayloadTooLarge)
	if up.Received != 4 {
		t.Fatalf("received after oversize = %d", up.Received)
	}
	if up, err = f.s.UploadChunk(ctx, u.UploadID, 4, bytes.NewReader(data[4:])); err != nil || up.Received != int64(len(data)) {
		t.Fatalf("finish: %v %d", err, up.Received)
	}
	r, err := f.s.FinalizeUpload(ctx, contracts.FinalizeUploadRequest{Mutation: svctest.Op(), UploadID: u.UploadID})
	if err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, f.s, r.Version.VersionID); !bytes.Equal(got, data) {
		t.Fatalf("content = %q", got)
	}
}

func TestDigestMismatchNeverPublished(t *testing.T) {
	f := newFixture(t, nil)
	ctx := svctest.Session("ses_synth-b")
	data := []byte("<h1>Report</h1>")
	u, err := f.s.BeginUpload(ctx, contracts.BeginUploadRequest{Mutation: svctest.Op(), Title: "bad", Filename: "r.html", Size: int64(len(data)), Digest: digestOf([]byte("something else"))})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.UploadChunk(ctx, u.UploadID, 0, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	fin := contracts.FinalizeUploadRequest{Mutation: svctest.Op(), UploadID: u.UploadID}
	_, err = f.s.FinalizeUpload(ctx, fin)
	e := mustCode(t, err, contracts.ErrConflict)
	if e.Ref == nil || e.Ref.Kind != contracts.KindUpload || !strings.Contains(e.Message, "nothing was published") {
		t.Fatalf("error: %+v", e)
	}
	got, _ := f.s.GetUpload(ctx, contracts.GetRequest{ID: u.UploadID})
	if got.State != StateFailed {
		t.Fatalf("upload state %s", got.State)
	}
	if page, _ := f.s.ListArtifacts(ctx, contracts.ListArtifactsRequest{}); len(page.Items) != 0 {
		t.Fatal("artifact visible after failed verification")
	}
	if n := len(f.sink.Events(t)); n != 0 {
		t.Fatalf("events emitted for failed upload: %d", n)
	}
	var blobs int
	filepath.WalkDir(filepath.Join(f.dir, "blobs"), func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			blobs++
		}
		return nil
	})
	if blobs != 0 {
		t.Fatalf("%d blobs stored for failed upload", blobs)
	}
	_, err = f.s.FinalizeUpload(ctx, fin) // replay
	mustCode(t, err, contracts.ErrConflict)
	_, err = f.s.FinalizeUpload(ctx, contracts.FinalizeUploadRequest{Mutation: svctest.Op(), UploadID: u.UploadID})
	mustCode(t, err, contracts.ErrConflict)
	_, err = f.s.UploadChunk(ctx, u.UploadID, 0, bytes.NewReader(data))
	mustCode(t, err, contracts.ErrConflict)
}

func TestVersionsImmutable(t *testing.T) {
	f := newFixture(t, nil)
	ctx := svctest.Owner()
	v1data := []byte("version one")
	r1 := f.publish(t, ctx, contracts.BeginUploadRequest{Title: "Doc", Filename: "doc.txt"}, v1data)
	r2 := f.publish(t, ctx, contracts.BeginUploadRequest{ArtifactID: r1.Artifact.ArtifactID, Filename: "doc.txt", Note: "second"}, []byte("version two, longer"))
	if r2.Version.Seq != 2 || r2.Version.PreviousVersionID != r1.Version.VersionID || r2.Artifact.VersionCount != 2 || r2.Artifact.LatestVersionID != r2.Version.VersionID {
		t.Fatalf("v2: %+v %+v", r2.Version, r2.Artifact)
	}
	if got := readAll(t, f.s, r1.Version.VersionID); !bytes.Equal(got, v1data) {
		t.Fatal("old version bytes changed")
	}
	if _, err := f.s.db.Exec(`UPDATE artifact_versions SET digest='x' WHERE version_id=?`, r1.Version.VersionID); err == nil {
		t.Fatal("version update allowed")
	}
	if _, err := f.s.db.Exec(`DELETE FROM artifact_versions WHERE version_id=?`, r1.Version.VersionID); err == nil {
		t.Fatal("version delete allowed")
	}
	_, hexd, _ := parseDigest(r1.Version.Digest)
	fi, err := os.Stat(f.s.blobPath(hexd))
	if err != nil || fi.Mode().Perm()&0o222 != 0 {
		t.Fatalf("blob is writable: %v %v", err, fi.Mode())
	}
	vs, _ := f.s.ListVersions(ctx, contracts.ListVersionsRequest{ArtifactID: r1.Artifact.ArtifactID})
	if len(vs.Items) != 2 || vs.Items[0].Seq != 2 {
		t.Fatalf("versions: %+v", vs.Items)
	}
	// Same bytes published again share the blob and stay readable.
	r3 := f.publish(t, ctx, contracts.BeginUploadRequest{Title: "Copy", Filename: "copy.txt"}, v1data)
	if r3.Version.Digest != r1.Version.Digest || !bytes.Equal(readAll(t, f.s, r3.Version.VersionID), v1data) {
		t.Fatal("dedup publish broken")
	}
}

func TestPathTraversalFilenames(t *testing.T) {
	f := newFixture(t, nil)
	ctx := svctest.Owner()
	outside := filepath.Dir(f.dir)
	for _, name := range []string{"../../escape.txt", `..\..\escape.txt`, "/etc/passwd", "..", "a/../../b"} {
		r := f.publish(t, ctx, contracts.BeginUploadRequest{Title: "t", Filename: name}, []byte("payload "+name))
		if strings.ContainsAny(r.Version.Filename, `/\`) || r.Version.Filename == ".." {
			t.Fatalf("unsafe display filename %q", r.Version.Filename)
		}
	}
	entries, _ := os.ReadDir(outside)
	for _, e := range entries {
		if e.Name() != "artifacts" && e.Name() != "hub.db" && !strings.HasPrefix(e.Name(), "hub.db-") {
			t.Fatalf("file written outside the data dir: %s", e.Name())
		}
	}
	cases := map[string]string{
		"../../etc/passwd":         "passwd",
		`..\..\x.txt`:              "x.txt",
		"..":                       "artifact",
		"":                         "artifact",
		"evil\u202Etxt.exe":        "eviltxt.exe",
		"a\x00b\nc.md":             "abc.md",
		" .hidden. ":               "hidden",
		`we"ird:na*me?.txt`:        "we_ird_na_me_.txt",
		strings.Repeat("é", 200):   strings.Repeat("é", 127),
		"report final (v2).pdf":    "report final (v2).pdf",
		"C:\\Users\\x\\secret.key": "secret.key",
	}
	for in, want := range cases {
		if got := SafeFilename(in); got != want {
			t.Errorf("SafeFilename(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLimits(t *testing.T) {
	f := newFixture(t, func(o *Options) { o.MaxBytes = 1000; o.MaxTotalBytes = 1500 })
	ctx := svctest.Owner()
	_, err := f.s.BeginUpload(ctx, contracts.BeginUploadRequest{Mutation: svctest.Op(), Title: "x", Filename: "x", Size: 1001, Digest: digestOf(nil)})
	e := mustCode(t, err, contracts.ErrPayloadTooLarge)
	if e.Details["max_bytes"] != int64(1000) {
		t.Fatalf("details %v", e.Details)
	}
	u, err := f.s.BeginUpload(ctx, contracts.BeginUploadRequest{Mutation: svctest.Op(), Title: "x", Filename: "x", Size: 900, Digest: digestOf(nil)})
	if err != nil || u.MaxBytes != 1000 {
		t.Fatalf("begin: %v %+v", err, u)
	}
	_, err = f.s.BeginUpload(ctx, contracts.BeginUploadRequest{Mutation: svctest.Op(), Title: "y", Filename: "y", Size: 700, Digest: digestOf(nil)})
	mustCode(t, err, contracts.ErrCapacityExhausted)
	_, err = f.s.BeginUpload(ctx, contracts.BeginUploadRequest{Mutation: svctest.Op(), Title: "y", Filename: "y", Size: 1, Digest: "md5:abc"})
	mustCode(t, err, contracts.ErrInvalid)
	_, err = f.s.BeginUpload(ctx, contracts.BeginUploadRequest{Mutation: svctest.Op(), Title: "y", Size: 1, Digest: digestOf(nil)})
	mustCode(t, err, contracts.ErrInvalid)
	blobs, open, err := f.s.StorageUsage(ctx)
	if err != nil || blobs != 0 || open != 900 {
		t.Fatalf("usage %d %d %v", blobs, open, err)
	}
}

func TestExpiryAndCleanup(t *testing.T) {
	f := newFixture(t, func(o *Options) { o.UploadTTL = time.Hour })
	ctx := svctest.Owner()
	data := []byte("abcdef")
	u, _ := f.s.BeginUpload(ctx, contracts.BeginUploadRequest{Mutation: svctest.Op(), Title: "x", Filename: "x", Size: 6, Digest: digestOf(data)})
	if _, err := f.s.UploadChunk(ctx, u.UploadID, 0, bytes.NewReader(data[:3])); err != nil {
		t.Fatal(err)
	}
	u2, _ := f.s.BeginUpload(ctx, contracts.BeginUploadRequest{Mutation: svctest.Op(), Title: "y", Filename: "y", Size: 6, Digest: digestOf(data)})
	f.s.UploadChunk(ctx, u2.UploadID, 0, bytes.NewReader(data[:2]))
	f.clock.Advance(59 * time.Minute)
	f.s.UploadChunk(ctx, u2.UploadID, 2, bytes.NewReader(data[2:4])) // activity extends u2
	f.clock.Advance(2 * time.Minute)
	n, err := f.s.CleanupExpired(ctx)
	if err != nil || n != 1 {
		t.Fatalf("cleanup: %v %d", err, n)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "uploads", u.UploadID)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("expired staging file kept")
	}
	_, err = f.s.UploadChunk(ctx, u.UploadID, 3, bytes.NewReader(data[3:]))
	mustCode(t, err, contracts.ErrExpired)
	_, err = f.s.FinalizeUpload(ctx, contracts.FinalizeUploadRequest{Mutation: svctest.Op(), UploadID: u.UploadID})
	mustCode(t, err, contracts.ErrExpired)
	if _, err := f.s.UploadChunk(ctx, u2.UploadID, 4, bytes.NewReader(data[4:])); err != nil {
		t.Fatalf("active upload expired: %v", err)
	}
	// Orphaned staging files are removed.
	orphan := filepath.Join(f.dir, "uploads", "upl_orphan")
	os.WriteFile(orphan, []byte("x"), 0o600)
	f.s.CleanupExpired(ctx)
	if _, err := os.Stat(orphan); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("orphan staging file kept")
	}
}

func TestMediaTypeDetectionAndPreviewURL(t *testing.T) {
	f := newFixture(t, nil)
	ctx := svctest.Owner()
	png := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0}, 32)...)
	r := f.publish(t, ctx, contracts.BeginUploadRequest{Title: "img", Filename: "screenshot"}, png)
	if r.Version.MediaType != "image/png" {
		t.Fatalf("sniffed %q", r.Version.MediaType)
	}
	r = f.publish(t, ctx, contracts.BeginUploadRequest{Title: "html", Filename: "report.html"}, []byte("<h1>Report</h1><script>/*sandboxed*/</script>"))
	if r.Version.MediaType != "text/html" || r.Version.PreviewURL != "/preview/"+r.Version.VersionID+"/{token}" {
		t.Fatalf("html: %+v", r.Version)
	}
	r = f.publish(t, ctx, contracts.BeginUploadRequest{Title: "pdf", Filename: "x.bin", MediaType: "application/pdf"}, []byte("%PDF-1.4 synthetic"))
	if r.Version.MediaType != "application/pdf" || r.Version.PreviewURL != "" {
		t.Fatalf("declared: %+v", r.Version)
	}
	r = f.publish(t, ctx, contracts.BeginUploadRequest{Title: "empty", Filename: "empty.dat"}, nil)
	if r.Version.Size != 0 || len(readAll(t, f.s, r.Version.VersionID)) != 0 {
		t.Fatal("empty artifact")
	}
	_, err := f.s.BeginUpload(ctx, contracts.BeginUploadRequest{Mutation: svctest.Op(), Title: "x", Filename: "x", MediaType: "not a type", Digest: digestOf(nil)})
	mustCode(t, err, contracts.ErrInvalid)
}

func TestCorruptBlobRefused(t *testing.T) {
	f := newFixture(t, nil)
	ctx := svctest.Owner()
	r := f.publish(t, ctx, contracts.BeginUploadRequest{Title: "x", Filename: "x.txt"}, []byte("pristine bytes"))
	_, hexd, _ := parseDigest(r.Version.Digest)
	p := f.s.blobPath(hexd)
	os.Chmod(p, 0o600)
	if err := os.WriteFile(p, []byte("tampered bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := f.s.OpenContent(ctx, r.Version.VersionID)
	mustCode(t, err, contracts.ErrInternal)
	checked, corrupt, err := f.s.VerifyBlobs(ctx)
	if err != nil || checked != 1 || corrupt != 1 {
		t.Fatalf("verify: %d %d %v", checked, corrupt, err)
	}
	// Re-publishing the genuine bytes repairs the content address.
	f.publish(t, ctx, contracts.BeginUploadRequest{Title: "y", Filename: "y.txt"}, []byte("pristine bytes"))
	if got := readAll(t, f.s, r.Version.VersionID); string(got) != "pristine bytes" {
		t.Fatalf("repair failed: %q", got)
	}
}

func TestVerifyingReaderDetectsLateCorruption(t *testing.T) {
	f := newFixture(t, func(o *Options) { o.VerifyTTL = time.Hour })
	ctx := svctest.Owner()
	r := f.publish(t, ctx, contracts.BeginUploadRequest{Title: "x", Filename: "x.txt"}, []byte("pristine bytes"))
	_, hexd, _ := parseDigest(r.Version.Digest)
	p := f.s.blobPath(hexd)
	os.Chmod(p, 0o600)
	os.WriteFile(p, []byte("tampered bytes"), 0o600) // same size; recently verified
	rc, _, err := f.s.OpenContent(ctx, r.Version.VersionID)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	_, err = io.ReadAll(rc)
	mustCode(t, err, contracts.ErrInternal)
	_, _, err = f.s.OpenContent(ctx, r.Version.VersionID)
	mustCode(t, err, contracts.ErrInternal) // now marked corrupt
}

func TestIdempotencyAndAuthorization(t *testing.T) {
	f := newFixture(t, nil)
	owner := svctest.Owner()
	sess := svctest.Session("ses_synth-b")
	req := contracts.BeginUploadRequest{Mutation: svctest.Op(), Title: "x", Filename: "x", Size: 1, Digest: digestOf([]byte("x"))}
	a, err := f.s.BeginUpload(sess, req)
	if err != nil {
		t.Fatal(err)
	}
	b, err := f.s.BeginUpload(sess, req)
	if err != nil || a.UploadID != b.UploadID {
		t.Fatalf("replay: %v %s %s", err, a.UploadID, b.UploadID)
	}
	_, err = f.s.UploadChunk(svctest.Session("ses_synth-other"), a.UploadID, 0, strings.NewReader("x"))
	mustCode(t, err, contracts.ErrForbidden)
	_, err = f.s.UploadChunk(context.Background(), a.UploadID, 0, strings.NewReader("x"))
	mustCode(t, err, contracts.ErrUnauthorized)
	if _, err := f.s.UploadChunk(owner, a.UploadID, 0, strings.NewReader("x")); err != nil {
		t.Fatalf("owner may complete: %v", err)
	}
	_, err = f.s.FinalizeUpload(svctest.Session("ses_synth-other"), contracts.FinalizeUploadRequest{Mutation: svctest.Op(), UploadID: a.UploadID})
	mustCode(t, err, contracts.ErrForbidden)
	_, err = f.s.BeginUpload(sess, contracts.BeginUploadRequest{Mutation: svctest.Op(), Title: "x", Filename: "x", Digest: digestOf(nil), Provenance: contracts.Provenance{SessionID: "ses_synth-spoof"}})
	mustCode(t, err, contracts.ErrForbidden)
	_, err = f.s.UploadChunk(owner, "upl_missing", 0, strings.NewReader("x"))
	mustCode(t, err, contracts.ErrNotFound)
}

func TestConcurrentFinalizeSameArtifact(t *testing.T) {
	f := newFixture(t, nil)
	ctx := svctest.Owner()
	r := f.publish(t, ctx, contracts.BeginUploadRequest{Title: "x", Filename: "x.txt"}, []byte("v1"))
	var ids []string
	for i := 0; i < 4; i++ {
		data := []byte(strings.Repeat("v", i+2))
		u, err := f.s.BeginUpload(ctx, contracts.BeginUploadRequest{Mutation: svctest.Op(), ArtifactID: r.Artifact.ArtifactID, Filename: "x.txt", Size: int64(len(data)), Digest: digestOf(data)})
		if err != nil {
			t.Fatal(err)
		}
		f.s.UploadChunk(ctx, u.UploadID, 0, bytes.NewReader(data))
		ids = append(ids, u.UploadID)
	}
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			if _, err := f.s.FinalizeUpload(ctx, contracts.FinalizeUploadRequest{Mutation: svctest.Op(), UploadID: id}); err != nil {
				t.Error(err)
			}
		}(id)
	}
	wg.Wait()
	vs, _ := f.s.ListVersions(ctx, contracts.ListVersionsRequest{ArtifactID: r.Artifact.ArtifactID})
	if len(vs.Items) != 5 {
		t.Fatalf("versions %d", len(vs.Items))
	}
	for i, v := range vs.Items {
		if v.Seq != int64(5-i) || (i < 4 && v.PreviousVersionID != vs.Items[i+1].VersionID) {
			t.Fatalf("version chain broken at %d: %+v", i, v)
		}
	}
}

// ---- HTTP ----

type fakeTickets struct {
	mu      sync.Mutex
	n       int
	tickets map[string]fakeTicket
}

type fakeTicket struct {
	purpose string
	ref     contracts.ObjectRef
	used    bool
}

func (f *fakeTickets) IssueSessionToken(context.Context, string) (string, error) { return "", nil }
func (f *fakeTickets) RevokeSessionTokens(context.Context, string) error         { return nil }
func (f *fakeTickets) IssueTicket(_ context.Context, purpose string, ref contracts.ObjectRef, ttl time.Duration) (string, time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tickets == nil {
		f.tickets = map[string]fakeTicket{}
	}
	f.n++
	t := "tkt/synthetic+" + strings.Repeat("x", f.n) // exercises escaping
	f.tickets[t] = fakeTicket{purpose: purpose, ref: ref}
	return t, time.Now().Add(ttl), nil
}
func (f *fakeTickets) RedeemTicket(_ context.Context, ticket, purpose string) (contracts.ObjectRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tickets[ticket]
	if !ok || t.used || t.purpose != purpose {
		return contracts.ObjectRef{}, contracts.Errorf(contracts.ErrUnauthorized, "bad ticket")
	}
	t.used = true
	f.tickets[ticket] = t
	return t.ref, nil
}

func TestPreviewHandlerIsolation(t *testing.T) {
	f := newFixture(t, nil)
	ctx := svctest.Owner()
	html := []byte("<h1>Report</h1><script>fetch('/v1/sessions.list')</script>")
	r := f.publish(t, ctx, contracts.BeginUploadRequest{Title: "html", Filename: "report.html"}, html)
	other := f.publish(t, ctx, contracts.BeginUploadRequest{Title: "other", Filename: "o.html"}, []byte("<p>o</p>"))
	tickets := &fakeTickets{}
	srv := httptest.NewServer(PreviewHandler(f.s, tickets))
	defer srv.Close()

	u, _, err := MintPreviewURL(ctx, tickets, srv.URL, r.Version.VersionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("Authorization", "Bearer should-be-ignored")
	req.AddCookie(&http.Cookie{Name: "session", Value: "x"})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !bytes.Equal(body, html) {
		t.Fatalf("preview: %d %q", resp.StatusCode, body)
	}
	want := map[string]string{
		"Content-Security-Policy":      "sandbox; default-src 'none'; img-src data: 'self'; style-src 'unsafe-inline'",
		"X-Content-Type-Options":       "nosniff",
		"Cross-Origin-Resource-Policy": "same-origin",
		"Referrer-Policy":              "no-referrer",
		"Content-Type":                 "text/html",
		"Cache-Control":                "no-store",
	}
	for k, v := range want {
		if got := resp.Header.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if len(resp.Cookies()) != 0 || resp.Header.Get("Set-Cookie") != "" {
		t.Fatal("preview set a cookie")
	}
	// One-time ticket.
	resp, _ = http.Get(u)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || resp.Header.Get("Content-Security-Policy") == "" {
		t.Fatalf("reused ticket: %d", resp.StatusCode)
	}
	// Ticket bound to a different version.
	u2, _, _ := MintPreviewURL(ctx, tickets, srv.URL, other.Version.VersionID, 0)
	swapped := strings.Replace(u2, other.Version.VersionID, r.Version.VersionID, 1)
	resp, _ = http.Get(swapped)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-version ticket: %d", resp.StatusCode)
	}
	// Wrong purpose.
	tk, _, _ := tickets.IssueTicket(ctx, "terminal", contracts.ObjectRef{Kind: contracts.KindArtifactVersion, ID: r.Version.VersionID}, time.Minute)
	resp, _ = http.Get(srv.URL + "/preview/" + r.Version.VersionID + "/" + strings.ReplaceAll(tk, "/", "%2F"))
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("wrong purpose: %d", resp.StatusCode)
	}
	for _, p := range []string{"/preview/", "/preview/x", "/preview/a/b/c", "/other"} {
		resp, _ = http.Get(srv.URL + p)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: %d", p, resp.StatusCode)
		}
	}
	resp, _ = http.Post(u2, "text/plain", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST: %d", resp.StatusCode)
	}
}

func TestContentHandler(t *testing.T) {
	f := newFixture(t, nil)
	ctx := svctest.Owner()
	data := []byte("0123456789")
	r := f.publish(t, ctx, contracts.BeginUploadRequest{Title: "x", Filename: "../weird \"name\".html"}, data)
	h := ContentHandler(f.s)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", r.Version.ContentURL, nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", r.Version.ContentURL, nil).WithContext(ctx))
	if rec.Code != 200 || rec.Body.String() != string(data) {
		t.Fatalf("get: %d %q", rec.Code, rec.Body.String())
	}
	cd := rec.Header().Get("Content-Disposition")
	if !strings.HasPrefix(cd, "attachment") || strings.Contains(cd, "..") || strings.Contains(cd, "/") {
		t.Fatalf("disposition %q", cd)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" || !strings.HasPrefix(rec.Header().Get("Content-Security-Policy"), "sandbox") {
		t.Fatal("content served without isolation headers")
	}
	req := httptest.NewRequest("GET", r.Version.ContentURL, nil).WithContext(ctx)
	req.Header.Set("Range", "bytes=2-4")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusPartialContent || rec.Body.String() != "234" {
		t.Fatalf("range: %d %q", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/content/ver_missing", nil).WithContext(ctx))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing: %d", rec.Code)
	}
}

func TestStoreMintPreviewURL(t *testing.T) {
	tickets := &fakeTickets{}
	var srvURL string
	f := newFixture(t, func(o *Options) { o.Tickets = tickets; o.PreviewBase = "https://preview.synth.invalid:8444" })
	ctx := svctest.Owner()
	r := f.publish(t, ctx, contracts.BeginUploadRequest{Title: "html", Filename: "report.html"}, []byte("<p>synthetic</p>"))
	now := time.Now()
	pu, err := f.s.MintPreviewURL(ctx, contracts.MintPreviewRequest{VersionID: r.Version.VersionID, TTLSeconds: 7200})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(pu.URL, "https://preview.synth.invalid:8444/preview/"+r.Version.VersionID+"/") {
		t.Fatalf("url %q", pu.URL)
	}
	if pu.ExpiresAt.After(now.Add(MaxPreviewTTL + time.Minute)) {
		t.Fatalf("ttl not capped: %v", pu.ExpiresAt)
	}
	// The minted link works against the preview handler exactly once.
	srv := httptest.NewServer(PreviewHandler(f.s, tickets))
	defer srv.Close()
	srvURL = srv.URL + strings.TrimPrefix(pu.URL, "https://preview.synth.invalid:8444")
	for i, want := range []int{200, 403} {
		resp, err := http.Get(srvURL)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("fetch %d: status %d, want %d", i, resp.StatusCode, want)
		}
	}
	_, err = f.s.MintPreviewURL(ctx, contracts.MintPreviewRequest{VersionID: "ver_synth-missing"})
	mustCode(t, err, contracts.ErrNotFound)
	_, err = f.s.MintPreviewURL(context.Background(), contracts.MintPreviewRequest{VersionID: r.Version.VersionID})
	mustCode(t, err, contracts.ErrUnauthorized)
	// Without a ticket issuer the operation is unsupported.
	g := newFixture(t, nil)
	_, err = g.s.MintPreviewURL(ctx, contracts.MintPreviewRequest{VersionID: r.Version.VersionID})
	mustCode(t, err, contracts.ErrUnsupported)
}
