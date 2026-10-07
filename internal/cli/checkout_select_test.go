package cli

import (
	"context"
	"os"
	"strings"
	"testing"

	c "github.com/jhh3/pyre/internal/contracts"
)

// A multi-repo project records checkouts at creation; `pyre start --project
// --repo` launches into the node's recorded checkout (shared), --worktree
// uses it as the worktree source, and a second session in the same shared
// checkout is told who else works there.
func TestStartUsesProjectCheckoutBinding(t *testing.T) {
	h := newHub(t)
	e := newEnv(t, h)
	r := run(t, e, nil, "--json", "project", "new", "synthetic-multi",
		"--repo", "app=https://example.invalid/synth/app.git", "--repo", "lib=https://example.invalid/synth/lib.git",
		"--checkout", "synth-mac:/srv/synth/app:app:shared:main",
		"--checkout", "synth-mac:/srv/synth/lib:lib:plain",
		"--checkout", "synth-linux:/home/synth/app:app:shared")
	expectCode(t, r, 0)
	p := mustJSON[c.Project](t, r)
	if len(p.Repos) != 2 || len(p.Checkouts) != 3 || p.Checkouts[0].NodeID != "node_synth-mac" || p.Checkouts[2].NodeID != "node_synth-linux" {
		t.Fatalf("project: repos=%+v checkouts=%+v", p.Repos, p.Checkouts)
	}

	start := func(args ...string) c.StartSessionResponse {
		t.Helper()
		r := run(t, e, nil, append([]string{"--json", "start", "--harness", "shell", "--project", "synthetic-multi"}, args...)...)
		expectCode(t, r, 0)
		return mustJSON[c.StartSessionResponse](t, r)
	}

	// Shared: the mac binding of app, no --cwd needed.
	a := start("--node", "synth-mac", "--repo", "app", "--title", "writer-a")
	if ck := a.Session.Run.Checkout; ck == nil || ck.Mode != c.CheckoutShared || ck.Path != "/srv/synth/app" || ck.RepoID != "app" || ck.Branch != "main" || a.Session.Run.Cwd != "/srv/synth/app" {
		t.Fatalf("shared launch: run=%+v checkout=%+v", a.Session.Run, a.Session.Run.Checkout)
	}
	// Same repo on another node resolves that node's binding.
	l := start("--node", "synth-linux", "--repo", "app")
	if ck := l.Session.Run.Checkout; ck == nil || ck.Path != "/home/synth/app" {
		t.Fatalf("linux launch: %+v", ck)
	}
	// A plain binding keeps plain mode (second repo of the project).
	lib := start("--node", "synth-mac", "--repo", "lib")
	if ck := lib.Session.Run.Checkout; ck == nil || ck.Mode != c.CheckoutPlain || ck.Path != "/srv/synth/lib" {
		t.Fatalf("plain launch: %+v", ck)
	}
	// Worktree: the binding is the source (cwd); the checkout is a worktree.
	wt := start("--node", "synth-mac", "--repo", "app", "--worktree", "feature-x")
	if ck := wt.Session.Run.Checkout; ck == nil || ck.Mode != c.CheckoutWorktree || ck.Branch != "feature-x" || wt.Session.Run.Cwd != "/srv/synth/app" {
		t.Fatalf("worktree launch: run=%+v checkout=%+v", wt.Session.Run, ck)
	}

	// Second writer in the same shared checkout: warned with the first one's ID (stderr only).
	r = run(t, e, nil, "--json", "start", "--harness", "shell", "--project", "synthetic-multi", "--node", "synth-mac", "--repo", "app", "--title", "writer-b")
	expectCode(t, r, 0)
	b := mustJSON[c.StartSessionResponse](t, r)
	if !strings.Contains(r.stderr, "shared checkout /srv/synth/app") || !strings.Contains(r.stderr, a.Session.Session.SessionID) {
		t.Fatalf("no shared-checkout notice naming %s: %s", a.Session.Session.SessionID, r)
	}
	if strings.Contains(r.stderr, wt.Session.Session.SessionID) || strings.Contains(r.stderr, lib.Session.Session.SessionID) {
		t.Fatalf("notice lists isolated/other-dir sessions: %s", r.stderr)
	}
	if b.Session.Run.Checkout == nil || b.Session.Run.Checkout.Path != "/srv/synth/app" {
		t.Fatalf("writer b: %+v", b.Session.Run.Checkout)
	}

	// Errors: unknown repo, no binding on that node without --cwd, --repo without project.
	expectCode(t, run(t, e, nil, "start", "--harness", "shell", "--project", "synthetic-multi", "--node", "synth-mac", "--repo", "nope"), ExitUsage)
	r = run(t, e, nil, "start", "--harness", "shell", "--project", "synthetic-multi", "--node", "synth-linux", "--repo", "lib")
	expectCode(t, r, ExitUsage)
	if !strings.Contains(r.stderr, "project update synthetic-multi --checkout") {
		t.Fatalf("missing binding hint: %s", r)
	}
	expectCode(t, run(t, e, nil, "start", "--harness", "shell", "--node", "synth-mac", "--repo", "app"), ExitUsage)
	// An explicit --cwd still works where no binding exists (unrecorded shared dir).
	x := start("--node", "synth-linux", "--repo", "lib", "--cwd", "/home/synth/lib")
	if ck := x.Session.Run.Checkout; ck == nil || ck.Mode != c.CheckoutShared || ck.Path != "/home/synth/lib" {
		t.Fatalf("explicit cwd: %+v", ck)
	}
	// A checkout of a repo the project does not have is refused before creating anything.
	expectCode(t, run(t, e, nil, "project", "new", "synthetic-bad", "--repo", "app=https://example.invalid/a.git", "--checkout", "synth-mac:/srv/x:other:shared"), ExitUsage)
	r = run(t, e, nil, "--json", "project", "ls")
	if strings.Contains(r.stdout, "synthetic-bad") {
		t.Fatalf("invalid project new created a project: %s", r.stdout)
	}
}

func TestPickBindingAmbiguity(t *testing.T) {
	p := c.Project{Name: "synth", Checkouts: []c.CheckoutBinding{
		{NodeID: "n1", Path: "/a", RepoID: "app", Mode: c.CheckoutShared},
		{NodeID: "n1", Path: "/b", RepoID: "app", Mode: c.CheckoutShared},
		{NodeID: "n1", Path: "/w", RepoID: "app", Mode: c.CheckoutWorktree},
	}}
	if _, _, err := pickBinding(p, "n1", "app", ""); err == nil || !strings.Contains(err.Error(), "--cwd") {
		t.Fatalf("two shared bindings must be ambiguous: %v", err)
	}
	if b, ok, err := pickBinding(p, "n1", "app", "/b/"); err != nil || !ok || b.Path != "/b" {
		t.Fatalf("cwd selects: %+v %v %v", b, ok, err)
	}
	if _, ok, err := pickBinding(p, "n1", "app", "/elsewhere"); err != nil || ok {
		t.Fatalf("unrecorded cwd: %v %v", ok, err)
	}
	p.Checkouts = p.Checkouts[2:]
	if b, ok, _ := pickBinding(p, "n1", "app", ""); !ok || b.Path != "/w" {
		t.Fatalf("worktree binding as last resort: %+v", b)
	}
}

// subagentAPI lists one Claude session whose run observed a native subagent.
type subagentAPI struct{ c.API }

func (subagentAPI) ListSessions(ctx context.Context, req c.ListSessionsRequest) (c.Page[c.SessionView], error) {
	v := c.SessionView{Session: c.Session{SessionID: "ses_synth-parent", Harness: c.HarnessClaude, Title: "parent"},
		Run: &c.Run{RunID: "run_synth-parent", State: c.RunRunning, Subagents: []c.SubagentObservation{{ID: "a0synthsub", Kind: "general-purpose", State: "stopped"}}}}
	if req.Query != "" {
		return c.Page[c.SessionView]{}, nil // the title search finds nothing
	}
	return c.Page[c.SessionView]{Items: []c.SessionView{v}}, nil
}

// A03: a native subagent is reported, never attachable; addressing it says
// so (and names the parent) instead of a bare "not found".
func TestSubagentReferenceIsExplained(t *testing.T) {
	e := newEnv(t, nil)
	for _, args := range [][]string{{"attach", "a0synthsub", "--print"}, {"sessions", "show", "a0synthsub"}} {
		r := run(t, e, &App{API: subagentAPI{}}, args...)
		if r.code == 0 || !strings.Contains(r.stderr, "native claude subagent of session ses_synth-parent") || !strings.Contains(r.stderr, "not independently attachable") {
			t.Fatalf("%v: %s", args, r)
		}
	}
	r := run(t, e, &App{API: subagentAPI{}}, "sessions", "show", "no-such-thing")
	expectCode(t, r, ExitNotFound)
}

// uploadSpy records the media type the CLI declares when it begins an upload.
type uploadSpy struct {
	c.API
	got *[]string
}

func (u uploadSpy) BeginUpload(ctx context.Context, req c.BeginUploadRequest) (c.Upload, error) {
	*u.got = append(*u.got, req.MediaType)
	return u.API.BeginUpload(ctx, req)
}

// A09 (found live): the CLI used to declare mime.TypeByExtension's answer,
// which reads platform tables, so the same .yaml/.toml/.log/.diff/.md got
// different media types from macOS and Linux. Without --media-type the CLI
// now declares nothing and the hub applies its one deterministic table.
func TestPublishLeavesMediaTypeToTheHub(t *testing.T) {
	h := newHub(t)
	e := newEnv(t, h)
	var got []string
	app := &App{API: uploadSpy{API: h.b, got: &got}}
	dir := t.TempDir()
	f := dir + "/synthetic.yaml"
	if err := os.WriteFile(f, []byte("synthetic: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	expectCode(t, run(t, e, app, "publish", f, "--title", "synthetic-yaml"), 0)
	expectCode(t, run(t, e, &App{API: uploadSpy{API: h.b, got: &got}}, "publish", f, "--title", "synthetic-yaml-2", "--media-type", "application/x-synthetic"), 0)
	if len(got) != 2 || got[0] != "" || got[1] != "application/x-synthetic" {
		t.Fatalf("declared media types: %q", got)
	}
}
