//go:build !windows

package security

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/artifacts"
	"github.com/jhh3/pyre/internal/client"
	"github.com/jhh3/pyre/internal/contracts"
)

func digestOf(b []byte) contracts.Digest {
	s := sha256.Sum256(b)
	return contracts.Digest("sha256:" + hex.EncodeToString(s[:]))
}

func (f *fixture) apiClient(token string) *client.Client {
	return client.New(f.srv.URL, token, client.Options{CertSHA256: f.pin, MaxAttempts: 1})
}

func (f *fixture) get(base, path, token string, hdr map[string]string) rpcResult {
	f.t.Helper()
	req, _ := http.NewRequest(http.MethodGet, base+path, nil)
	req.Header.Set(contracts.ProtocolHeader, strconv.Itoa(contracts.ProtocolVersion))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	return f.do(req)
}

// TestArtifactFilenamesCannotTraverse: hostile display filenames are
// sanitized for display and never become storage paths; every file the
// store writes stays under its data directory with content-addressed names.
func TestArtifactFilenamesCannotTraverse(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	cl := f.apiClient(f.owner.Token)
	ctx := context.Background()
	outside := filepath.Join(f.dir, "escaped.txt")
	hostile := []string{
		"../../../../" + outside, "..\\..\\windows\\system32\\evil.dll", "/etc/passwd", "a/../../b.txt",
		"name\x00.txt", "‮txt.exe", "CON", "..", "report\r\nSet-Cookie: x=y.html", `quote".html`,
	}
	for i, name := range hostile {
		data := []byte("synthetic artifact " + strconv.Itoa(i))
		rec, err := cl.Publish(ctx, bytes.NewReader(data), client.PublishMeta{Title: "traversal " + strconv.Itoa(i), Filename: name})
		if err != nil {
			t.Fatalf("publish %q: %v", name, err)
		}
		fn := rec.Version.Filename
		if strings.ContainsAny(fn, "/\\\x00\r\n\"") || strings.Contains(fn, "..") || fn == "" {
			t.Errorf("stored display filename %q from %q is unsafe", fn, name)
		}
		r := f.get(f.srv.URL, "/v1/content/"+rec.Version.VersionID, f.owner.Token, nil)
		cd := r.header.Get("Content-Disposition")
		if r.status != 200 || !strings.HasPrefix(cd, "attachment") || strings.ContainsAny(cd, "\r\n/\\") {
			t.Errorf("content-disposition for %q = %q (status %d)", name, cd, r.status)
		}
		if r.header.Get("Set-Cookie") != "" {
			t.Errorf("header injection via filename %q", name)
		}
	}
	if _, err := os.Stat(outside); err == nil {
		t.Fatal("a file was written outside the artifact store")
	}
	root := filepath.Join(f.dir, "artifacts")
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		if strings.HasPrefix(rel, "..") {
			t.Errorf("path outside store: %s", p)
		}
		if d.Type().IsRegular() && !strings.HasPrefix(rel, filepath.Join("blobs", "sha256")) && !strings.HasPrefix(rel, "uploads") {
			t.Errorf("unexpected stored file %s", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Traversal in the version id of content/preview URLs.
	for _, p := range []string{"/v1/content/..%2F..%2Fhub.db", "/v1/content/ver_..", "/v1/content/../../hub.db", "/v1/content/%2e%2e%2fhub.db"} {
		if r := f.get(f.srv.URL, p, f.owner.Token, nil); r.status == 200 {
			t.Errorf("GET %s returned 200", p)
		}
	}
	for _, p := range []string{"/preview/..%2F..%2Fhub.db/pyt_x", "/preview/ver_x/..%2F..%2Fhub.db"} {
		if r := f.get(f.preview.URL, p, "", nil); r.status == 200 {
			t.Errorf("GET preview %s returned 200", p)
		}
	}
}

// TestArtifactContentRequiresAuth: downloads need a credential and are
// never rendered actively by the management origin.
func TestArtifactContentRequiresAuth(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	cl := f.apiClient(f.owner.Token)
	html := []byte(`<html><script>fetch("/v1/system.whoami")</script></html>`)
	rec, err := cl.Publish(context.Background(), bytes.NewReader(html), client.PublishMeta{Title: "page", Filename: "page.html", MediaType: "text/html"})
	if err != nil {
		t.Fatal(err)
	}
	path := "/v1/content/" + rec.Version.VersionID
	if r := f.get(f.srv.URL, path, "", nil); r.status != http.StatusUnauthorized {
		t.Fatalf("anonymous download: %d", r.status)
	}
	if r := f.get(f.srv.URL, path, "", map[string]string{"Cookie": "pyre_session=" + f.owner.Token}); r.status != http.StatusUnauthorized {
		t.Fatalf("cookie-only download: %d (cookies must never authenticate)", r.status)
	}
	r := f.get(f.srv.URL, path, f.owner.Token, nil)
	if r.status != 200 || !bytes.Equal(r.body, html) {
		t.Fatalf("owner download: %d", r.status)
	}
	csp := r.header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "sandbox") || !strings.Contains(csp, "default-src 'none'") || r.header.Get("X-Content-Type-Options") != "nosniff" ||
		!strings.HasPrefix(r.header.Get("Content-Disposition"), "attachment") {
		t.Fatalf("download headers allow active rendering: %v", r.header)
	}
	// Known limitation (see README): any authenticated principal — including
	// an agent session outside the artifact's project — can read content;
	// per-project scoping of session credentials is not implemented yet.
	_, sessTok := f.startSession("reader")
	if r := f.get(f.srv.URL, path, sessTok, nil); r.status == 200 {
		t.Logf("NOTE: session principal can read artifact content outside any project scope (status %d)", r.status)
	}
}

// TestPreviewIsolation: active content previews authenticate only with a
// one-time ticket bound to the exact version, carry a sandbox CSP with no
// script/same-origin allowance, set no cookies, and the preview origin
// exposes no management API. A management bearer is never accepted on the
// preview path, and a preview ticket is never accepted by the API.
func TestPreviewIsolation(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	cl := f.apiClient(f.owner.Token)
	ctx := context.Background()
	page := []byte(`<html><body><script>fetch("/v1/projects.list",{method:"POST"})</script><img src="https://evil.invalid/x"></body></html>`)
	rec, err := cl.Publish(ctx, bytes.NewReader(page), client.PublishMeta{Title: "html", Filename: "x.html", MediaType: "text/html"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := cl.Publish(ctx, bytes.NewReader([]byte("<p>other</p>")), client.PublishMeta{Title: "other", Filename: "o.html", MediaType: "text/html"})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Version.PreviewURL == "" {
		t.Fatal("HTML version has no preview URL")
	}
	mint := func(ver string) (string, string) {
		u, _, err := artifacts.MintPreviewURL(ctx, f.auth, f.preview.URL, ver, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		tk := u[strings.LastIndex(u, "/")+1:]
		f.remember(tk)
		return strings.TrimPrefix(u, f.preview.URL), tk
	}
	path, _ := mint(rec.Version.VersionID)
	r := f.get(f.preview.URL, path, "", nil)
	if r.status != 200 || !bytes.Equal(r.body, page) {
		t.Fatalf("preview: %d", r.status)
	}
	csp := r.header.Get("Content-Security-Policy")
	for _, want := range []string{"sandbox", "default-src 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("preview CSP %q lacks %q", csp, want)
		}
	}
	for _, bad := range []string{"allow-scripts", "allow-same-origin", "connect-src", "script-src", "allow-top-navigation"} {
		if strings.Contains(csp, bad) {
			t.Errorf("preview CSP %q allows %q", csp, bad)
		}
	}
	if r.header.Get("Set-Cookie") != "" || r.header.Get("X-Content-Type-Options") != "nosniff" || r.header.Get("Referrer-Policy") != "no-referrer" || r.header.Get("Cache-Control") != "no-store" {
		t.Errorf("preview headers: %v", r.header)
	}
	if r := f.get(f.preview.URL, path, "", nil); r.status != http.StatusForbidden {
		t.Errorf("preview ticket reuse: %d (one-time)", r.status)
	}
	// Ticket bound to its version.
	_, t2 := mint(rec.Version.VersionID)
	if r := f.get(f.preview.URL, "/preview/"+other.Version.VersionID+"/"+t2, "", nil); r.status != http.StatusForbidden {
		t.Errorf("ticket for version A opened version B: %d", r.status)
	}
	// Bearer never accepted on preview.
	if r := f.get(f.preview.URL, "/preview/"+rec.Version.VersionID+"/"+f.owner.Token, f.owner.Token, nil); r.status != http.StatusForbidden {
		t.Errorf("owner token as preview ticket: %d", r.status)
	}
	if r := f.get(f.preview.URL, "/preview/"+rec.Version.VersionID+"/pyt_invalid", f.owner.Token, nil); r.status != http.StatusForbidden {
		t.Errorf("bearer header with bad ticket: %d", r.status)
	}
	// Preview ticket never accepted by the management API.
	_, t3 := mint(rec.Version.VersionID)
	if r := f.rpc(t3, contracts.OpListProjects, nil); r.status != http.StatusUnauthorized {
		t.Errorf("preview ticket as API bearer: %d", r.status)
	}
	if r := f.get(f.srv.URL, "/v1/content/"+rec.Version.VersionID, t3, nil); r.status != http.StatusUnauthorized {
		t.Errorf("preview ticket on content API: %d", r.status)
	}
	// The preview origin has no management API, and the management API
	// ignores ambient cookies, so script in a preview has nothing to call.
	for _, p := range []string{contracts.RPCPath(contracts.OpListProjects), contracts.RPCPath(contracts.OpWhoAmI), "/v1/content/" + rec.Version.VersionID, contracts.PathEventStream} {
		req, _ := http.NewRequest(http.MethodPost, f.preview.URL+p, strings.NewReader("{}"))
		if rr := f.do(req); rr.status == 200 {
			t.Errorf("preview origin serves %s", p)
		}
	}
	if r := f.rpc("", contracts.OpListProjects, nil); r.status != http.StatusUnauthorized {
		t.Errorf("management API without bearer: %d", r.status)
	}
	if f.preview.URL == f.srv.URL {
		t.Fatal("preview shares the management origin")
	}
	f.assertNoLeaks(nil)
}

// TestUploadDigestMismatchNeverPublished: bytes that do not match the
// declared sha256 (or are short) are never published: finalize fails,
// no version or blob exists, no artifact.published event is stored, and
// the failed upload cannot be revived.
func TestUploadDigestMismatchNeverPublished(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	cl := f.apiClient(f.owner.Token)
	ctx := context.Background()
	declared := []byte("the bytes the producer meant to publish")
	actual := []byte("THE BYTES AN ATTACKER SWAPPED IN!!!!!!!")
	if len(declared) != len(actual) {
		t.Fatal("test bug: lengths must match so only the digest differs")
	}
	up, err := cl.BeginUpload(ctx, contracts.BeginUploadRequest{Mutation: op(), Title: "swapped", Filename: "s.txt", Size: int64(len(declared)), Digest: digestOf(declared)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cl.UploadChunk(ctx, up.UploadID, 0, bytes.NewReader(actual)); err != nil {
		t.Fatal(err)
	}
	_, err = cl.FinalizeUpload(ctx, contracts.FinalizeUploadRequest{Mutation: op(), UploadID: up.UploadID})
	if !contracts.IsCode(err, contracts.ErrConflict) {
		t.Fatalf("finalize with mismatched digest = %v, want conflict", err)
	}
	got, err := cl.GetUpload(ctx, contracts.GetRequest{ID: up.UploadID})
	if err != nil || got.State != "failed" {
		t.Fatalf("upload state = %+v %v", got, err)
	}
	if _, err := cl.UploadChunk(ctx, up.UploadID, 0, bytes.NewReader(declared)); err == nil {
		t.Fatal("failed upload accepted new bytes")
	}
	if _, err := cl.FinalizeUpload(ctx, contracts.FinalizeUploadRequest{Mutation: op(), UploadID: up.UploadID}); err == nil {
		t.Fatal("failed upload finalized on retry")
	}

	// Short upload: never published.
	up2, err := cl.BeginUpload(ctx, contracts.BeginUploadRequest{Mutation: op(), Title: "short", Filename: "short.txt", Size: int64(len(declared)), Digest: digestOf(declared)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cl.UploadChunk(ctx, up2.UploadID, 0, bytes.NewReader(declared[:10])); err != nil {
		t.Fatal(err)
	}
	if _, err := cl.FinalizeUpload(ctx, contracts.FinalizeUploadRequest{Mutation: op(), UploadID: up2.UploadID}); !contracts.IsCode(err, contracts.ErrConflict) {
		t.Fatalf("finalize short upload = %v, want conflict", err)
	}

	arts, err := cl.ListArtifacts(ctx, contracts.ListArtifactsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(arts.Items) != 0 {
		t.Fatalf("artifacts published from bad uploads: %+v", arts.Items)
	}
	var n int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM sec_events WHERE kind=?`, string(contracts.EvArtifactPublished)).Scan(&n); err != nil || n != 0 {
		t.Fatalf("artifact.published events: %d %v", n, err)
	}
	blobs := 0
	_ = filepath.WalkDir(filepath.Join(f.dir, "artifacts", "blobs"), func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			blobs++
		}
		return nil
	})
	if blobs != 0 {
		t.Fatalf("%d blobs stored from unverified uploads", blobs)
	}
}

// TestUploadOwnershipEnforced: another principal cannot write to, finalize,
// or inspect someone else's open upload.
func TestUploadOwnershipEnforced(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	ctx := context.Background()
	_, aliceTok := f.startSession("alice-up")
	_, bobTok := f.startSession("bob-up")
	alice, bob := f.apiClient(aliceTok), f.apiClient(bobTok)
	data := []byte("alice's synthetic bytes")
	up, err := alice.BeginUpload(ctx, contracts.BeginUploadRequest{Mutation: op(), Title: "alice", Filename: "a.txt", Size: int64(len(data)), Digest: digestOf(data)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bob.UploadChunk(ctx, up.UploadID, 0, bytes.NewReader(data)); !contracts.IsCode(err, contracts.ErrForbidden) {
		t.Errorf("bob writing alice's upload = %v", err)
	}
	if _, err := bob.FinalizeUpload(ctx, contracts.FinalizeUploadRequest{Mutation: op(), UploadID: up.UploadID}); !contracts.IsCode(err, contracts.ErrForbidden) {
		t.Errorf("bob finalizing alice's upload = %v", err)
	}
	if _, err := bob.GetUpload(ctx, contracts.GetRequest{ID: up.UploadID}); !contracts.IsCode(err, contracts.ErrForbidden) {
		t.Errorf("bob reading alice's upload = %v, want forbidden", err)
	}
	if got, err := alice.GetUpload(ctx, contracts.GetRequest{ID: up.UploadID}); err != nil || got.UploadID != up.UploadID {
		t.Errorf("alice reading her own upload: %+v %v", got, err)
	}
}
