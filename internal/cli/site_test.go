package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/site"
)

// TestSiteNewServeDeliversNotes: `pyre site new` inside a session names it
// on the page; `pyre site serve` wires note delivery to the hub, so a note
// left on the page lands in that session's Pyre inbox.
func TestSiteNewServeDeliversNotes(t *testing.T) {
	h := newHub(t)
	e := newEnv(t, h)
	root := filepath.Join(e["HOME"], "pages")
	e = e.with("PYRE_SITE_DIR", root, "PYRE_SESSION_ID", "ses_synth-a")

	r := run(t, e, nil, "--json", "site", "new", "launch-plan", "--title", "Launch plan")
	expectCode(t, r, 0)
	p, err := site.LoadPage(root, "launch-plan")
	if err != nil || p.Meta.Session != "ses_synth-a" || p.Meta.Title != "Launch plan" {
		t.Fatalf("page.json: %+v %v\n%s", p, err, r)
	}
	expectCode(t, run(t, e, nil, "site", "new", "launch-plan"), int(ExitConflict))
	if r := run(t, e, nil, "site", "new", "Bad Name"); r.code == 0 {
		t.Fatalf("bad name accepted:\n%s", r)
	}
	r = run(t, e, nil, "site", "new", "no-owner", "--no-session")
	expectCode(t, r, 0)
	if q, _ := site.LoadPage(root, "no-owner"); q.Meta.Session != "" {
		t.Fatalf("--no-session: %+v", q.Meta)
	}

	r = run(t, e, nil, "--json", "site", "build")
	expectCode(t, r, 0)
	if !strings.Contains(r.stdout, `"page": "launch-plan"`) {
		t.Fatalf("build:\n%s", r)
	}
	// A broken page makes build fail and name the problem.
	if err := os.WriteFile(filepath.Join(root, "launch-plan", site.HTMLFile), []byte(`<body><p data-note="a"></p><p data-note="a"></p></body>`), 0o644); err != nil {
		t.Fatal(err)
	}
	r = run(t, e, nil, "site", "build")
	if r.code == 0 || !strings.Contains(r.stdout, `data-note="a" is used twice`) {
		t.Fatalf("broken build:\n%s", r)
	}

	// serve: refuses without an allowlist or with a public address.
	var got []site.Options
	orig := serveSite
	serveSite = func(ctx context.Context, o site.Options) error { got = append(got, o); return nil }
	t.Cleanup(func() { serveSite = orig })
	if r := run(t, e, nil, "site", "serve"); r.code == 0 || !strings.Contains(r.stderr, "--allow-domain") {
		t.Fatalf("no allowlist:\n%s", r)
	}
	if r := run(t, e, nil, "site", "serve", "--dev", "--listen", "0.0.0.0:7393"); r.code == 0 || !strings.Contains(r.stderr, "loopback") {
		t.Fatalf("public listen:\n%s", r)
	}
	r = run(t, e, nil, "site", "serve", "--allow-domain", "example.org", "--allow-domain", "example.net", "--origin", "https://review.example.org")
	expectCode(t, r, 0)
	if len(got) != 1 {
		t.Fatalf("serve did not run:\n%s", r)
	}
	o := got[0]
	if o.Root != root || o.Listen != site.DefaultListen || o.Dev || len(o.AllowDomains) != 2 || o.Deliver == nil ||
		o.StateDir != filepath.Join(e["HOME"], ".local", "state", "pyre", "site") {
		t.Fatalf("serve options: %+v", o)
	}

	// Run the real server with the wired delivery and leave a note.
	if err := os.WriteFile(filepath.Join(root, "launch-plan", site.HTMLFile), []byte(`<body><p data-note="a">A</p></body>`), 0o644); err != nil {
		t.Fatal(err)
	}
	o.StateDir = t.TempDir()
	o.Logf = func(string, ...any) {}
	s, err := site.New(o)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/api/notes", strings.NewReader(`{"page":"launch-plan","item":"a","text":"Ship it Tuesday."}`))
	req.Host = "review.example.org"
	req.Header.Set(site.HeaderEmail, "alice@example.org")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://review.example.org")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("note: %d %s", w.Code, w.Body)
	}
	var n site.Note
	if err := json.Unmarshal(w.Body.Bytes(), &n); err != nil || n.Delivery == nil || n.Delivery.MessageID == "" || n.Delivery.Error != "" {
		t.Fatalf("delivery: %+v %v", n.Delivery, err)
	}
	inbox, err := h.b.ListMessages(c.WithPrincipal(context.Background(), testOwner), c.ListMessagesRequest{
		Box: "inbox", Address: &c.Address{Kind: c.AddrSession, ID: "ses_synth-a"}})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range inbox.Items {
		if m.Envelope.MessageID == n.Delivery.MessageID && strings.Contains(m.Envelope.Body.Text, "Ship it Tuesday.") &&
			strings.Contains(m.Envelope.Body.Text, "https://review.example.org/p/launch-plan/") {
			found = true
		}
	}
	if !found {
		t.Fatalf("note not in the session's inbox: %+v", inbox.Items)
	}

	// notes prints it back.
	r = run(t, e, nil, "site", "notes", "launch-plan", "--state-dir", o.StateDir)
	expectCode(t, r, 0)
	if !strings.Contains(r.stdout, "launch-plan / a · alice@example.org") || !strings.Contains(r.stdout, "    Ship it Tuesday.") {
		t.Fatalf("notes:\n%s", r)
	}
	r = run(t, e, nil, "--json", "site", "notes", "--state-dir", o.StateDir)
	expectCode(t, r, 0)
	if !strings.Contains(r.stdout, `"text": "Ship it Tuesday."`) {
		t.Fatalf("notes json:\n%s", r)
	}
}
