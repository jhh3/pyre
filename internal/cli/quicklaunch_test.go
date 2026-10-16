package cli

import (
	"context"
	"strings"
	"testing"

	c "github.com/jhh3/pyre/internal/contracts"
)

func TestParseNewArgs(t *testing.T) {
	for _, tc := range []struct {
		args                  []string
		repos                 string
		node, harness, prompt string
	}{
		{[]string{"pyre"}, "pyre", "", "", ""},
		{[]string{"api+web", "@server", "codex", "rename", "the", "endpoint"}, "api,web", "server", "codex", "rename the endpoint"},
		{[]string{"tundra", "shell", "@mac"}, "tundra", "mac", "shell", ""},
		{[]string{"tundra", "fix the claude hooks"}, "tundra", "", "", "fix the claude hooks"},
		{[]string{"tundra", "claude", "claude is great"}, "tundra", "", "claude", "claude is great"},
		{[]string{"tundra", "@", "x"}, "tundra", "", "", "@ x"},
	} {
		repos, node, harness, prompt := parseNewArgs(tc.args)
		if strings.Join(repos, ",") != tc.repos || node != tc.node || harness != tc.harness || prompt != tc.prompt {
			t.Errorf("parseNewArgs(%q) = %v %q %q %q", tc.args, repos, node, harness, prompt)
		}
	}
}

func TestNewStartsInMainCheckoutByDefault(t *testing.T) {
	h := newHub(t)
	e := newEnv(t, h)
	r := run(t, e, nil, "--json", "new", "notes", "fix", "the", "share", "link")
	expectCode(t, r, 0)
	res := mustJSON[QuickLaunchResult](t, r)
	if res.Session.Session.Harness != c.HarnessClaude || res.Session.NodeName != "synth-mac" || res.Session.Run.Cwd != "/Users/demo/code/notes-app" || res.Worktrees != nil {
		t.Fatalf("default launch: harness=%s node=%s run=%+v wt=%+v", res.Session.Session.Harness, res.Session.NodeName, res.Session.Run, res.Worktrees)
	}
	if len(res.Repos) != 1 || res.Repos[0].Name != "notes-app" {
		t.Fatalf("repos: %+v", res.Repos)
	}
	// Human output says where it started and how to get a worktree instead.
	r = run(t, e, nil, "new", "web", "shell")
	expectCode(t, r, 0)
	if !strings.Contains(r.stdout, "in web on synth-linux (shell)") || !strings.Contains(r.stdout, "main checkout /home/demo/src/web") || !strings.Contains(r.stdout, "--fresh") {
		t.Fatalf("human output: %s", r)
	}
	// A second session in the same main checkout gets the shared-checkout warning.
	r = run(t, e, nil, "new", "web", "shell")
	expectCode(t, r, 0)
	if !strings.Contains(r.stderr, "shared checkout /home/demo/src/web") || !strings.Contains(r.stderr, "--fresh") {
		t.Fatalf("no shared-checkout warning: %s", r)
	}
}

func TestNewMachineChoice(t *testing.T) {
	h := newHub(t)
	e := newEnv(t, h)
	// atlas is on both machines: @machine picks one.
	r := run(t, e, nil, "--json", "new", "atlas", "@synth-linux", "shell")
	expectCode(t, r, 0)
	res := mustJSON[QuickLaunchResult](t, r)
	if res.Session.NodeName != "synth-linux" || res.Session.Run.Cwd != "/home/demo/src/atlas" {
		t.Fatalf("@machine: %s %s", res.Session.NodeName, res.Session.Run.Cwd)
	}
	// Without @machine, the machine it was used on most recently wins.
	r = run(t, e, nil, "--json", "new", "atlas", "shell")
	expectCode(t, r, 0)
	if res = mustJSON[QuickLaunchResult](t, r); res.Session.NodeName != "synth-linux" {
		t.Fatalf("most recently used machine: %s", res.Session.NodeName)
	}
	// A repository not on the named machine is cloned there first (a
	// confirmation line on stderr), unless --no-clone.
	r = run(t, e, nil, "new", "web", "@synth-mac", "--no-clone")
	expectCode(t, r, ExitUsage)
	if !strings.Contains(r.stderr, "web is not on synth-mac; drop --no-clone") {
		t.Fatalf("--no-clone: %s", r)
	}
	r = run(t, e, nil, "--json", "new", "web", "@synth-mac", "shell")
	expectCode(t, r, 0)
	res = mustJSON[QuickLaunchResult](t, r)
	if !strings.Contains(r.stderr, "cloning demo/web on synth-mac (not there yet)") || !strings.Contains(r.stderr, "cloned into /Users/demo/code/web") {
		t.Fatalf("clone confirmation: %s", r)
	}
	if len(res.Cloned) != 1 || !res.Cloned[0].Cloned || res.Session.NodeName != "synth-mac" || res.Session.Run.Cwd != "/Users/demo/code/web" {
		t.Fatalf("clone launch: cloned=%+v node=%s cwd=%s", res.Cloned, res.Session.NodeName, res.Session.Run.Cwd)
	}
}

// owner/name from GitHub (the catalog), or any owner/name, is cloned onto
// the chosen machine.
func TestNewClonesGitHubRepo(t *testing.T) {
	h := newHub(t)
	e := newEnv(t, h)
	// Two machines and none named: say which.
	r := run(t, e, nil, "new", "demo/orbit", "shell")
	expectCode(t, r, ExitUsage)
	if !strings.Contains(r.stderr, "say which machine gets the clone") {
		t.Fatalf("no machine: %s", r)
	}
	r = run(t, e, nil, "--json", "new", "demo/orbit", "@synth-linux", "shell")
	expectCode(t, r, 0)
	res := mustJSON[QuickLaunchResult](t, r)
	if len(res.Cloned) != 1 || res.Cloned[0].Remote != "https://example.invalid/demo/orbit.git" || res.Session.Run.Cwd != "/home/demo/src/orbit" {
		t.Fatalf("catalog clone: %+v cwd=%s", res.Cloned, res.Session.Run.Cwd)
	}
	// Now indexed there: no second clone.
	r = run(t, e, nil, "--json", "new", "orbit", "shell")
	expectCode(t, r, 0)
	if res = mustJSON[QuickLaunchResult](t, r); len(res.Cloned) != 0 || res.Session.NodeName != "synth-linux" {
		t.Fatalf("second launch: %+v on %s", res.Cloned, res.Session.NodeName)
	}
	// An owner/name nobody indexed: its GitHub URL.
	r = run(t, e, nil, "--json", "new", "synth-org/brand-new", "@synth-mac", "shell")
	expectCode(t, r, 0)
	if res = mustJSON[QuickLaunchResult](t, r); len(res.Cloned) != 1 || res.Cloned[0].Remote != "https://github.com/synth-org/brand-new.git" || res.Session.Run.Cwd != "/Users/demo/code/brand-new" {
		t.Fatalf("plain owner/name: %+v cwd=%s", res.Cloned, res.Session.Run.Cwd)
	}
}

func TestNewAmbiguousAndUnknown(t *testing.T) {
	h := newHub(t)
	e := newEnv(t, h)
	r := run(t, e, nil, "new", "tu")
	expectCode(t, r, ExitUsage)
	if !strings.Contains(r.stderr, "matches several repositories") || !strings.Contains(r.stderr, "tundra (synth-mac)") || !strings.Contains(r.stderr, "tundra-old (synth-linux)") {
		t.Fatalf("ambiguous: %s", r)
	}
	// On a terminal, an inline picker chooses.
	app := &App{IsTerminal: func() bool { return true }, Stdin: strings.NewReader("2\n")}
	r = run(t, e, app, "new", "tu", "shell")
	expectCode(t, r, 0)
	if !strings.Contains(r.stderr, "choose 1-2") || !strings.Contains(r.stdout, "in tundra-old on synth-linux") {
		t.Fatalf("picker: %s", r)
	}
	// An exact name wins over longer matches without asking.
	r = run(t, e, nil, "--json", "new", "tundra", "shell")
	expectCode(t, r, 0)
	if res := mustJSON[QuickLaunchResult](t, r); res.Repos[0].Name != "tundra" {
		t.Fatalf("exact: %+v", res.Repos)
	}
	expectCode(t, run(t, e, nil, "new", "zzz-nothing"), ExitNotFound)
	expectCode(t, run(t, e, nil, "new"), ExitUsage)
	// pyre start --repo without a project points at pyre new.
	r = run(t, e, nil, "start", "--harness", "shell", "--node", "synth-mac", "--repo", "tundra")
	expectCode(t, r, ExitUsage)
	if !strings.Contains(r.stderr, "pyre new tundra") {
		t.Fatalf("start --repo hint: %s", r)
	}
}

func TestNewFreshAndMultiRepo(t *testing.T) {
	h := newHub(t)
	e := newEnv(t, h)
	r := run(t, e, nil, "--json", "new", "notes", "--fresh", "shell", "Fix the share link redirect")
	expectCode(t, r, 0)
	res := mustJSON[QuickLaunchResult](t, r)
	if res.Worktrees == nil || res.Worktrees.Branch != "pyre/fix-share-link-redirect" || res.Session.Run.Cwd != "/Users/demo/code/notes-app/.worktrees/fix-share-link-redirect" {
		t.Fatalf("fresh: wt=%+v cwd=%s", res.Worktrees, res.Session.Run.Cwd)
	}
	// --branch implies --fresh.
	r = run(t, e, nil, "--json", "new", "notes", "--branch", "pyre/custom", "shell")
	expectCode(t, r, 0)
	if res = mustJSON[QuickLaunchResult](t, r); res.Worktrees == nil || res.Worktrees.Branch != "pyre/custom" {
		t.Fatalf("branch: %+v", res.Worktrees)
	}
	// Several repositories: one folder, one branch, always fresh, on the
	// machine that has all of them.
	r = run(t, e, nil, "--json", "new", "api+web", "shell", "rename the share endpoint")
	expectCode(t, r, 0)
	res = mustJSON[QuickLaunchResult](t, r)
	if res.Session.NodeName != "synth-linux" || res.Session.Run.Cwd != "/home/demo/pyre-work/rename-share-endpoint" || len(res.Worktrees.Worktrees) != 2 {
		t.Fatalf("multi: node=%s cwd=%s wt=%+v", res.Session.NodeName, res.Session.Run.Cwd, res.Worktrees)
	}
	// Repositories on different machines: --no-clone refuses; otherwise
	// the machine with most of them clones the rest.
	r = run(t, e, nil, "new", "notes+web", "--no-clone")
	expectCode(t, r, ExitUsage)
	if !strings.Contains(r.stderr, "no single machine has all of these repositories") {
		t.Fatalf("split repos: %s", r)
	}
	r = run(t, e, nil, "--json", "new", "notes+web", "shell")
	expectCode(t, r, 0)
	if res = mustJSON[QuickLaunchResult](t, r); len(res.Cloned) != 1 || len(res.Repos) != 2 || res.Worktrees == nil {
		t.Fatalf("split repos cloned: %+v", res)
	}
}

func TestReposListAndMCPStart(t *testing.T) {
	h := newHub(t)
	e := newEnv(t, h)
	r := run(t, e, nil, "repos", "tundra")
	expectCode(t, r, 0)
	if !strings.Contains(r.stdout, "REPO") || !strings.Contains(r.stdout, "tundra-old") || strings.Contains(r.stdout, "notes-app") {
		t.Fatalf("repos: %s", r)
	}
	r = run(t, e, nil, "--json", "repos", "--node", "synth-mac")
	expectCode(t, r, 0)
	list := mustJSON[c.RepoList](t, r)
	if len(list.Items) != 4 {
		t.Fatalf("repos --node: %+v", list.Items)
	}
	// MCP start_session (shared ops): repo without project quick-launches.
	ops := &Ops{API: h.b}
	ctx := c.WithPrincipal(context.Background(), testOwner)
	resp, err := ops.StartSession(ctx, StartParams{Repo: "notes", Harness: "shell", Fresh: true, Prompt: "tidy imports"})
	if err != nil || resp.Session.Run == nil || resp.Session.Run.Cwd != "/Users/demo/code/notes-app/.worktrees/tidy-imports" {
		t.Fatalf("mcp start: %+v %v", resp.Session.Run, err)
	}
	if _, err := ops.StartSession(ctx, StartParams{Repo: "tu", Harness: "shell"}); err == nil || !strings.Contains(err.Error(), "matches several") {
		t.Fatalf("mcp ambiguity: %v", err)
	}
}

func TestArchiveOffersWorktreeCleanup(t *testing.T) {
	h := newHub(t)
	e := newEnv(t, h)
	start := func() string {
		r := run(t, e, nil, "--json", "new", "notes", "--fresh", "shell", "share link")
		expectCode(t, r, 0)
		return mustJSON[QuickLaunchResult](t, r).Session.Session.SessionID
	}
	sid := start()
	// sessions show lists the session's worktrees.
	r := run(t, e, nil, "sessions", "show", sid)
	expectCode(t, r, 0)
	if !strings.Contains(r.stdout, "worktrees of notes-app") || !strings.Contains(r.stdout, "pyre  /Users/demo/code/notes-app/.worktrees/share-link") {
		t.Fatalf("show: %s", r)
	}
	// Not a terminal, no flag: kept, and says how to remove.
	r = run(t, e, nil, "sessions", "archive", "--force", sid)
	expectCode(t, r, 0)
	if !strings.Contains(r.stderr, "worktrees created by "+sid) || !strings.Contains(r.stderr, "kept; --remove-worktrees") {
		t.Fatalf("archive default: %s", r)
	}
	// On a terminal the default answer is keep.
	sid = start()
	app := &App{IsTerminal: func() bool { return true }, Stdin: strings.NewReader("\n")}
	r = run(t, e, app, "sessions", "archive", "--force", sid)
	if !strings.Contains(r.stderr, "[y/N]") || !strings.Contains(r.stderr, "kept the worktrees") {
		t.Fatalf("archive tty default: %s", r)
	}
	// y removes the clean ones.
	sid = start()
	app = &App{IsTerminal: func() bool { return true }, Stdin: strings.NewReader("y\n")}
	r = run(t, e, app, "sessions", "archive", "--force", sid)
	if !strings.Contains(r.stderr, "removed share-link-") && !strings.Contains(r.stderr, "removed share-link and its branch") {
		t.Fatalf("archive tty yes: %s", r)
	}
	// --keep-worktrees never asks.
	sid = start()
	r = run(t, e, nil, "sessions", "archive", "--force", "--keep-worktrees", sid)
	if strings.Contains(r.stderr, "worktrees created by") {
		t.Fatalf("keep: %s", r)
	}
}

func TestWorktreeCandidatesRules(t *testing.T) {
	sw := c.SessionWorktrees{Repos: []c.RepoWorktrees{{Worktrees: []c.Worktree{
		{Path: "/r", Main: true, CreatedInSession: true},
		{Path: "/r/.worktrees/old"},
		{Path: "/r/.claude/worktrees/a", CreatedInSession: true},
		{Path: "/r/.claude/worktrees/b", CreatedInSession: true, Dirty: true},
	}}}}
	rm, dirty := worktreeCandidates(sw)
	if len(rm) != 1 || rm[0].Path != "/r/.claude/worktrees/a" || len(dirty) != 1 {
		t.Fatalf("rm=%+v dirty=%+v", rm, dirty)
	}
	if l := wtLine(c.Worktree{Path: "/x", Branch: "b"}); !strings.Contains(l, "branch will be kept") {
		t.Fatalf("unmerged line: %s", l)
	}
	if l := removalLine(c.WorktreeRemoval{Path: "/x/y", Outcome: "removed", BranchOutcome: "kept_unmerged", Branch: "b"}); l != "removed y; kept branch b (not merged or pushed)" {
		t.Fatalf("removal line: %s", l)
	}
}

// `pyre new <path>` starts in a plain folder; --mkdir creates it first.
func TestNewInFolder(t *testing.T) {
	h := newHub(t)
	e := newEnv(t, h)
	r := run(t, e, nil, "--json", "new", "~/scratch", "@synth-mac", "shell", "look", "around")
	expectCode(t, r, 0)
	res := mustJSON[QuickLaunchResult](t, r)
	if res.Folder != "~/scratch" || res.Session.Run.Cwd != "~/scratch" || res.Session.NodeName != "synth-mac" || res.Created || len(res.Repos) != 0 {
		t.Fatalf("folder launch: %+v run=%+v", res, res.Session.Run)
	}
	r = run(t, e, nil, "new", "~/clips/new", "@synth-linux", "shell", "--mkdir")
	expectCode(t, r, 0)
	if !strings.Contains(r.stdout, "in ~/clips/new (new folder) on synth-linux (shell)") {
		t.Fatalf("mkdir launch: %s", r)
	}
	r = run(t, e, nil, "--json", "new", "home", "@synth-mac", "shell")
	expectCode(t, r, 0)
	if res = mustJSON[QuickLaunchResult](t, r); res.Folder != "~" {
		t.Fatalf("home: %+v", res)
	}
	r = run(t, e, nil, "new", "notes", "--mkdir")
	if r.code == 0 || !strings.Contains(r.stderr, "--mkdir needs a path") {
		t.Fatalf("--mkdir with a repo: %s", r)
	}
}

// repos --rescan runs repos.reindex on every connected machine (or --node)
// and lists once the hub has the new index.
func TestReposRescan(t *testing.T) {
	h := newHub(t)
	e := newEnv(t, h)
	r := run(t, e, nil, "--json", "repos", "--rescan")
	expectCode(t, r, 0)
	list := mustJSON[c.RepoList](t, r)
	if len(list.Items) == 0 || !strings.Contains(r.stderr, "rescanned synth-mac:") || !strings.Contains(r.stderr, "rescanned synth-linux:") || strings.Contains(r.stderr, "not listed") {
		t.Fatalf("rescan all: %d items\n%s", len(list.Items), r)
	}
	r = run(t, e, nil, "repos", "--rescan", "--node", "synth-mac")
	expectCode(t, r, 0)
	if !strings.Contains(r.stderr, "rescanned synth-mac:") || strings.Contains(r.stderr, "synth-linux") || !strings.Contains(r.stdout, "REPO") {
		t.Fatalf("rescan one: %s", r)
	}
}
