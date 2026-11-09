package site

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var synthNow = time.Date(2026, 11, 9, 18, 42, 0, 0, time.UTC)

func writePage(t *testing.T, root, slug, meta, page string) {
	t.Helper()
	d := filepath.Join(root, slug)
	if err := os.MkdirAll(filepath.Join(d, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, PageFile), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}
	if page != "" {
		if err := os.WriteFile(filepath.Join(d, HTMLFile), []byte(page), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestScaffoldCheckAndRender(t *testing.T) {
	root := t.TempDir()
	d, err := Scaffold(root, "file-viewer", Meta{Title: "File viewer", Session: "ses_synth-a"}, synthNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Scaffold(root, "file-viewer", Meta{}, synthNow); err == nil {
		t.Fatal("scaffolding over an existing page should fail")
	}
	if _, err := Scaffold(root, "Bad Name", Meta{}, synthNow); err == nil {
		t.Fatal("bad slug accepted")
	}
	p, err := LoadPage(root, "file-viewer")
	if err != nil || p.Meta.Session != "ses_synth-a" || p.Meta.Status != "draft" || p.Updated.IsZero() {
		t.Fatalf("scaffolded page.json: %+v %v", p, err)
	}
	pages, probs, err := Check(root)
	if err != nil || len(pages) != 1 || len(probs) != 0 {
		t.Fatalf("starter page should be clean: %v %v %v", pages, probs, err)
	}
	src, _ := os.ReadFile(filepath.Join(d, HTMLFile))
	if ids := NoteIDs(src); len(ids) != 2 || ids[0] != "decision-1" {
		t.Fatalf("starter note IDs: %v", ids)
	}

	// Problems: duplicate and malformed IDs, no index.html, bad page.json.
	writePage(t, root, "dupes", `{"title":"Dupes","status":"ready","order":1}`,
		`<html><body><div data-note="a"></div><div data-note='a'></div><p data-note="bad id"></p><p data-note="_page"></p></body></html>`)
	writePage(t, root, "no-html", `{"title":"No HTML"}`, "")
	writePage(t, root, "no-title", `{"summary":"x"}`, "<body></body>")
	if err := os.MkdirAll(filepath.Join(root, "not-a-page"), 0o755); err != nil {
		t.Fatal(err)
	}
	pages, probs, err = Check(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != 3 || pages[0].Slug != "dupes" {
		t.Fatalf("pages (order first): %+v", pages)
	}
	msgs := []string{}
	for _, p := range probs {
		msgs = append(msgs, p.String())
	}
	all := strings.Join(msgs, "\n")
	for _, want := range []string{`dupes: data-note="a" is used twice`, `dupes: data-note="bad id"`, `dupes: data-note="_page"`, "no-html: no index.html", "no-title: page.json has no title"} {
		if !strings.Contains(all, want) {
			t.Errorf("missing problem %q in:\n%s", want, all)
		}
	}

	// Rendering: stylesheet first in head, chrome after body, script before </body>.
	out := string(RenderPage(p, src, "Review"))
	if !strings.Contains(out, `<head>`+"\n"+`<meta charset`) && !strings.Contains(out, `<head><meta name="pyre-site-page" content="file-viewer"><link rel="stylesheet" href="../../_site/site.css">`) {
		t.Fatalf("head injection:\n%s", out)
	}
	if strings.Index(out, `_site/site.css`) > strings.Index(out, "<style>") {
		t.Fatal("site stylesheet must come before the page's own styles")
	}
	if !strings.Contains(out, `class="ps-bar"><a href="../../">Review</a>`) || !strings.Contains(out, `<script src="../../_site/notes.js"></script></body>`) {
		t.Fatalf("chrome/script injection:\n%s", out)
	}

	idx := string(RenderIndex("Review", pages, synthNow))
	if !strings.Contains(idx, `data-note="_page" data-note-page="dupes"`) || !strings.Contains(idx, `href="p/file-viewer/"`) || !strings.Contains(idx, "Ready for review") {
		t.Fatalf("index:\n%s", idx)
	}
	if !strings.Contains(idx, "Nov 9, 2026, 10:42 AM PT") {
		t.Fatalf("index times are Pacific:\n%s", idx)
	}

	// Export writes a browsable static copy.
	outDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(d, "assets", "shot.png"), []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Export(root, outDir, "Review", synthNow); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"index.html", "_site/site.css", "_site/notes.js", "p/file-viewer/index.html", "p/file-viewer/assets/shot.png"} {
		if _, err := os.Stat(filepath.Join(outDir, f)); err != nil {
			t.Errorf("export missing %s: %v", f, err)
		}
	}
}

type sent struct{ session, text string }

type fixture struct {
	t     *testing.T
	root  string
	state string
	srv   *Server
	h     http.Handler
	mu    sync.Mutex
	sent  []sent
	fail  bool
}

func newFixture(t *testing.T, opts Options) *fixture {
	f := &fixture{t: t, root: t.TempDir(), state: t.TempDir()}
	writePage(t, f.root, "plan", `{"title":"Plan","session":"ses_synth-a"}`,
		`<!doctype html><html><head><title>Plan</title></head><body><main><article data-note="d1">D1</article></main></body></html>`)
	writePage(t, f.root, "orphan", `{"title":"Orphan"}`, `<body><p data-note="x">x</p></body>`)
	opts.Root, opts.StateDir = f.root, f.state
	if opts.AllowDomains == nil && opts.AllowEmails == nil && !opts.Dev {
		opts.AllowDomains = []string{"example.org", "@Example.net"}
	}
	opts.Origins = append(opts.Origins, "https://review.example.org")
	opts.Now = func() time.Time { return synthNow }
	opts.Logf = func(string, ...any) {}
	opts.Deliver = func(_ context.Context, session, text string) (string, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.fail {
			return "", errors.New("hub unreachable")
		}
		f.sent = append(f.sent, sent{session, text})
		return "msg_synth-1", nil
	}
	s, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	f.srv, f.h = s, s.Handler()
	return f
}

// do sends a request; hdr pairs are header name/value.
func (f *fixture) do(method, target, body string, hdr ...string) (*http.Response, string) {
	f.t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, r)
	req.Host = "review.example.org"
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Add(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, req)
	b, _ := io.ReadAll(w.Result().Body)
	return w.Result(), string(b)
}

func signedIn(email string, extra ...string) []string {
	return append([]string{HeaderEmail, email}, extra...)
}

func writeHdr(email string) []string {
	return signedIn(email, "Content-Type", "application/json", "Origin", "https://review.example.org", "Sec-Fetch-Site", "same-origin")
}

func TestAuthGate(t *testing.T) {
	f := newFixture(t, Options{})
	cases := []struct {
		name   string
		hdr    []string
		remote string
		want   int
	}{
		{"no header", nil, "", http.StatusUnauthorized},
		{"no header from loopback without dev", nil, "127.0.0.1:5555", http.StatusUnauthorized},
		{"other domain", signedIn("eve@evil.example"), "", http.StatusForbidden},
		{"subdomain", signedIn("eve@sub.example.org"), "", http.StatusForbidden},
		{"suffix trick", signedIn("eve@notexample.org"), "", http.StatusForbidden},
		{"two headers", signedIn("a@example.org", HeaderEmail, "b@example.org"), "", http.StatusForbidden},
		{"two at signs", signedIn("a@b@example.org"), "", http.StatusForbidden},
		{"allowed", signedIn("Alice@Example.org"), "", http.StatusOK},
		{"second domain", signedIn("bob@example.net"), "", http.StatusOK},
	}
	for _, tc := range cases {
		for _, path := range []string{"/", "/p/plan/", "/_site/notes.js", "/api/notes"} {
			req := httptest.NewRequest("GET", path, nil)
			if tc.remote != "" {
				req.RemoteAddr = tc.remote
			}
			for i := 0; i+1 < len(tc.hdr); i += 2 {
				req.Header.Add(tc.hdr[i], tc.hdr[i+1])
			}
			w := httptest.NewRecorder()
			f.h.ServeHTTP(w, req)
			if w.Code != tc.want {
				t.Errorf("%s %s: got %d, want %d (%s)", tc.name, path, w.Code, tc.want, w.Body.String())
			}
			if w.Header().Get("Content-Security-Policy") == "" || w.Header().Get("X-Frame-Options") != "DENY" {
				t.Errorf("%s %s: security headers missing", tc.name, path)
			}
		}
	}

	// A single allowed email works without its domain.
	g := newFixture(t, Options{AllowEmails: []string{"carol@example.com"}})
	if r, _ := g.do("GET", "/", "", signedIn("carol@example.com")...); r.StatusCode != 200 {
		t.Fatalf("allowed email: %d", r.StatusCode)
	}
	if r, _ := g.do("GET", "/", "", signedIn("dan@example.com")...); r.StatusCode != 403 {
		t.Fatalf("email list is exact: %d", r.StatusCode)
	}

	// --dev admits loopback without the header, but not a remote client.
	d := newFixture(t, Options{Dev: true})
	req := httptest.NewRequest("GET", "/api/me", nil)
	req.RemoteAddr = "127.0.0.1:4000"
	w := httptest.NewRecorder()
	d.h.ServeHTTP(w, req)
	if w.Code != 200 || !strings.Contains(w.Body.String(), DevEmail) {
		t.Fatalf("dev loopback: %d %s", w.Code, w.Body)
	}
	req = httptest.NewRequest("GET", "/api/me", nil)
	req.RemoteAddr = "192.0.2.7:4000"
	w = httptest.NewRecorder()
	d.h.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatalf("dev remote: %d", w.Code)
	}

	// No allowlist without --dev, and no public listen address.
	if _, err := New(Options{Root: t.TempDir()}); err == nil {
		t.Fatal("a server with nobody allowed should not start")
	}
	if _, err := New(Options{Root: t.TempDir(), Dev: true, Listen: "0.0.0.0:7393"}); err == nil {
		t.Fatal("public listen accepted")
	}
}

func TestCSRFAndFiles(t *testing.T) {
	f := newFixture(t, Options{})
	body := `{"page":"plan","item":"d1","text":"hi"}`
	for name, hdr := range map[string][]string{
		"form post":    signedIn("a@example.org", "Content-Type", "application/x-www-form-urlencoded", "Origin", "https://review.example.org"),
		"no origin":    signedIn("a@example.org", "Content-Type", "application/json"),
		"other origin": signedIn("a@example.org", "Content-Type", "application/json", "Origin", "https://evil.example"),
		"cross-site":   signedIn("a@example.org", "Content-Type", "application/json", "Origin", "https://review.example.org", "Sec-Fetch-Site", "cross-site"),
	} {
		if r, b := f.do("POST", "/api/notes", body, hdr...); r.StatusCode < 400 {
			t.Errorf("%s accepted: %d %s", name, r.StatusCode, b)
		}
	}
	if n := len(f.srv.notes.List("")); n != 0 {
		t.Fatalf("refused writes stored %d notes", n)
	}

	// Page files: assets served; page.json, dot files and escapes are not.
	if err := os.WriteFile(filepath.Join(f.root, "plan", "assets", "a.txt"), []byte("asset"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.root, "plan", ".secret"), []byte("no"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.txt")
	_ = os.WriteFile(outside, []byte("outside"), 0o644)
	_ = os.Symlink(outside, filepath.Join(f.root, "plan", "assets", "link.txt"))
	if r, b := f.do("GET", "/p/plan/assets/a.txt", "", signedIn("a@example.org")...); r.StatusCode != 200 || b != "asset" {
		t.Fatalf("asset: %d %q", r.StatusCode, b)
	}
	for _, p := range []string{"/p/plan/page.json", "/p/plan/.secret", "/p/plan/assets/link.txt", "/p/plan/../orphan/page.json", "/p/nope/", "/p/Bad/"} {
		if r, b := f.do("GET", p, "", signedIn("a@example.org")...); r.StatusCode == 200 {
			t.Errorf("%s served: %q", p, b)
		}
	}
	r, b := f.do("GET", "/p/plan/", "", signedIn("a@example.org")...)
	if r.StatusCode != 200 || !strings.Contains(b, `<meta name="pyre-site-page" content="plan">`) || !strings.Contains(b, `notes.js`) {
		t.Fatalf("page render: %d\n%s", r.StatusCode, b)
	}
	// A change on disk is live on the next request (no rebuild, no restart).
	if err := os.WriteFile(filepath.Join(f.root, "plan", HTMLFile), []byte(`<body><p data-note="d2">Changed</p></body>`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, b := f.do("GET", "/p/plan/", "", signedIn("a@example.org")...); !strings.Contains(b, "Changed") {
		t.Fatal("page edit not live")
	}
}

func TestNotesRoundTripAndDelivery(t *testing.T) {
	f := newFixture(t, Options{})
	alice := writeHdr("alice@example.org")
	alice = append(alice, HeaderName, "Alice Synth")

	// New note on an item of a page that names a session: stored, then sent.
	r, b := f.do("POST", "/api/notes", `{"page":"plan","item":"d1","text":"  Prefer option B.  "}`, alice...)
	if r.StatusCode != 201 {
		t.Fatalf("add: %d %s", r.StatusCode, b)
	}
	var n noteView
	if err := json.Unmarshal([]byte(b), &n); err != nil {
		t.Fatal(err)
	}
	if n.Text != "Prefer option B." || n.Author != "alice@example.org" || n.Name != "Alice Synth" || !n.Mine ||
		n.Delivery == nil || n.Delivery.Session != "ses_synth-a" || n.Delivery.MessageID != "msg_synth-1" {
		t.Fatalf("note: %+v delivery %+v", n, n.Delivery)
	}
	if len(f.sent) != 1 || f.sent[0].session != "ses_synth-a" {
		t.Fatalf("sent: %+v", f.sent)
	}
	msg := f.sent[0].text
	for _, want := range []string{`"Plan"`, "item d1", "Alice Synth <alice@example.org>", "Prefer option B.", "https://review.example.org/p/plan/", "pyre site notes plan", "10:42 AM PT"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message lacks %q:\n%s", want, msg)
		}
	}

	// A note on the index card (the whole page) is sent too.
	if r, b := f.do("POST", "/api/notes", `{"page":"plan","item":"_page","text":"Overall fine."}`, writeHdr("bob@example.net")...); r.StatusCode != 201 {
		t.Fatalf("page note: %d %s", r.StatusCode, b)
	}
	if len(f.sent) != 2 || !strings.Contains(f.sent[1].text, "the page as a whole") {
		t.Fatalf("page note not sent: %+v", f.sent)
	}
	// A page without a session: stored, nothing sent, no delivery field.
	r, b = f.do("POST", "/api/notes", `{"page":"orphan","item":"x","text":"Nobody owns this."}`, alice...)
	if r.StatusCode != 201 || strings.Contains(b, "delivery") || len(f.sent) != 2 {
		t.Fatalf("orphan: %d %s sent=%d", r.StatusCode, b, len(f.sent))
	}
	// A failed send keeps the note and records why.
	f.fail = true
	r, b = f.do("POST", "/api/notes", `{"page":"plan","item":"d1","text":"Second thought."}`, alice...)
	if r.StatusCode != 201 || !strings.Contains(b, "hub unreachable") {
		t.Fatalf("failed delivery: %d %s", r.StatusCode, b)
	}
	f.fail = false

	// Validation.
	for _, body := range []string{`{"page":"plan","item":"d1","text":"   "}`, `{"page":"plan","item":"bad id","text":"x"}`,
		`{"page":"../x","item":"d1","text":"x"}`, `{"page":"plan","item":"d1","text":"` + strings.Repeat("x", MaxNoteText+1) + `"}`, `[1]`} {
		if r, b := f.do("POST", "/api/notes", body, alice...); r.StatusCode != 400 {
			t.Errorf("bad body accepted: %d %s", r.StatusCode, b)
		}
	}
	if r, _ := f.do("POST", "/api/notes", `{"page":"gone","item":"d1","text":"x"}`, alice...); r.StatusCode != 404 {
		t.Errorf("note on a missing page: %d", r.StatusCode)
	}

	// List: per page, with "mine" for the caller.
	r, b = f.do("GET", "/api/notes?page=plan", "", signedIn("bob@example.net")...)
	var list struct {
		Me    string     `json:"me"`
		Notes []noteView `json:"notes"`
	}
	if err := json.Unmarshal([]byte(b), &list); err != nil || r.StatusCode != 200 {
		t.Fatal(r.StatusCode, b, err)
	}
	if list.Me != "bob@example.net" || len(list.Notes) != 3 || list.Notes[0].Mine || !list.Notes[1].Mine {
		t.Fatalf("list: %+v", list)
	}

	// Only the author edits or deletes.
	if r, _ := f.do("PATCH", "/api/notes/"+n.ID, `{"text":"hijack"}`, writeHdr("bob@example.net")...); r.StatusCode != 403 {
		t.Fatalf("other's edit: %d", r.StatusCode)
	}
	if r, _ := f.do("DELETE", "/api/notes/"+n.ID, "", writeHdr("bob@example.net")...); r.StatusCode != 403 {
		t.Fatalf("other's delete: %d", r.StatusCode)
	}
	r, b = f.do("PATCH", "/api/notes/"+n.ID, `{"text":"Prefer option B, firmly."}`, alice...)
	if r.StatusCode != 200 || !strings.Contains(b, `"edited"`) || !strings.Contains(b, "firmly") {
		t.Fatalf("edit: %d %s", r.StatusCode, b)
	}
	if len(f.sent) != 2 {
		t.Fatalf("an edit should not resend: %d", len(f.sent))
	}

	// Persisted (0600) and reloaded by a new server and by ReadNotes.
	fi, err := os.Stat(filepath.Join(f.state, NotesFile))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("notes file: %v %v", fi, err)
	}
	db, err := ReadNotes(f.state)
	if err != nil || len(List(db, "plan", time.Time{})) != 3 || len(List(db, "", time.Time{})) != 4 || len(List(db, "", synthNow.Add(time.Minute))) != 0 {
		t.Fatalf("ReadNotes: %+v %v", db, err)
	}
	g, err := New(Options{Root: f.root, StateDir: f.state, AllowDomains: []string{"example.org"}, Logf: func(string, ...any) {}})
	if err != nil {
		t.Fatal(err)
	}
	if got := g.notes.List("plan"); len(got) != 3 || got[0].Text != "Prefer option B, firmly." || got[0].Delivery == nil {
		t.Fatalf("reload: %+v", got)
	}

	if r, _ := f.do("DELETE", "/api/notes/"+n.ID, "", alice...); r.StatusCode != 200 {
		t.Fatalf("delete: %d", r.StatusCode)
	}
	if r, _ := f.do("DELETE", "/api/notes/"+n.ID, "", alice...); r.StatusCode != 404 {
		t.Fatalf("delete twice: %d", r.StatusCode)
	}
}
