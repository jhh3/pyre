package tui_test

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/repoindex"
	"github.com/jhh3/pyre/internal/runtime/mock"
	"github.com/jhh3/pyre/internal/tui"
	"github.com/jhh3/pyre/internal/tui/tuitest"
)

// New-session panel (W-newsession): a form with Machine first, then
// Where (scoped to the machine), Agent, Model, Effort, Workspace, Account,
// Prompt and Title; and its folder picker. The fixture's machines:
// synth-linux (1/16 runs, cpu 7%: the least loaded, so the default; Claude
// and Shell) and synth-mac (Claude, Codex, Shell).

// nsOpen opens the panel and waits for the repo catalog.
func nsOpen(t *testing.T, h *tuitest.Harness) {
	t.Helper()
	h.Key("n")
	if !h.M.QuickLaunchOpen() {
		t.Fatal("n did not open the new-session panel")
	}
	h.WaitFor("catalog", 3*sec, func() bool { return h.Contains("demo/atlas") || h.Contains("demo/tundra") })
	h.Settle()
}

var nsLabels = []string{"Machine", "Where", "Agent", "Model", "Effort", "Workspace", "Account", "Prompt", "Title"}

// nsRow is the highlighted Where (or picker) row.
func nsRow(view string) string {
	for _, l := range strings.Split(view, "\n") {
		c := strings.TrimSpace(strings.Trim(strings.TrimSpace(l), "│"))
		if !strings.HasPrefix(c, "› ") {
			continue
		}
		label := false
		for _, f := range nsLabels {
			label = label || strings.HasPrefix(c, "› "+f+" ")
		}
		if !label {
			return c
		}
	}
	return ""
}

// nsMachineLine is the Machine field's line for machine name.
func nsMachineLine(view, name string) string {
	for _, l := range strings.Split(view, "\n") {
		if strings.Contains(l, "● "+name+" ") || strings.Contains(l, "○ "+name+" ") {
			return l
		}
	}
	return ""
}

func startReq(t *testing.T, h *tuitest.Harness, rec *tuitest.Recorder) contracts.StartSessionRequest {
	t.Helper()
	h.WaitFor("start", 3*sec, func() bool { return len(rec.Calls("StartSession")) == 1 })
	return rec.Calls("StartSession")[0].Request.(contracts.StartSessionRequest)
}

// The form opens on Machine; typing there filters Where, which lists only
// that machine's repositories and recent folders.
func TestNewSessionFormStartsInMainCheckout(t *testing.T) {
	h, rec, _ := startMock(t, 120, 34, nil, nil)
	nsOpen(t, h)
	golden(t, "newsession-open-120", h.View())
	if !strings.Contains(h.View(), "› Machine") || !strings.Contains(nsMachineLine(h.View(), "synth-linux"), "● synth-linux") {
		t.Fatalf("not on Machine, or the least-loaded machine not chosen:\n%s", h.View())
	}
	for _, want := range []string{"1/16 runs · cpu 7% · mem 40%", "claude shell", "2/8 runs · cpu 18% · mem 63%", "claude codex shell"} {
		if !h.Contains(want) {
			t.Fatalf("machine load/agents %q missing:\n%s", want, h.View())
		}
	}
	h.Type("atl")
	if row := nsRow(h.View()); !strings.Contains(row, "atlas") || !strings.Contains(row, "~/src/atlas") || !strings.Contains(h.View(), "› Where") {
		t.Fatalf("typing on Machine did not filter Where: %q\n%s", row, h.View())
	}
	if !h.Contains("Claude on synth-linux · demo/atlas · main checkout · account auto") || !h.Contains("● Claude") || h.Contains("Codex") {
		t.Fatalf("summary / agents:\n%s", h.View())
	}
	golden(t, "newsession-120", h.View())
	h.Key("ctrl+s")
	req := startReq(t, h, rec)
	if req.Cwd != "/home/demo/src/atlas" || req.Harness != contracts.HarnessClaude || req.NodeID != "node_synth-linux" || req.InitialPrompt != "" ||
		req.AccountPolicy.Mode != contracts.AccountEligible || req.OperationID == "" {
		t.Fatalf("start request: %+v", req)
	}
	if len(rec.Calls("InvokeNodeAction:"+contracts.ActionWorktreeCreate))+len(rec.Calls("InvokeNodeAction:"+contracts.ActionRepoClone)) != 0 {
		t.Fatal("main-checkout launch created a worktree or cloned")
	}
	h.WaitFor("back on Sessions", 3*sec, func() bool {
		return !h.M.OverlayOpen() && h.Contains("launch requested: claude in atlas on synth-linux")
	})
}

// Changing the machine re-scopes Where (and the agents) and keeps the
// filter text; other machines' repositories are never mixed in.
func TestNewSessionMachineRescopesWhere(t *testing.T) {
	h, rec, _ := startMock(t, 120, 34, nil, nil)
	nsOpen(t, h)
	h.Type("tund")
	if row := nsRow(h.View()); !strings.Contains(row, "tundra-old") || h.Contains("demo/tundra ") || h.Contains("Repositories on synth-mac") {
		t.Fatalf("linux list: %q\n%s", row, h.View())
	}
	h.Key("shift+tab") // Machine
	h.Key("down")      // synth-mac
	if !strings.Contains(nsMachineLine(h.View(), "synth-mac"), "● synth-mac") || strings.Contains(nsMachineLine(h.View(), "synth-mac"), "auto") {
		t.Fatalf("down did not choose synth-mac:\n%s", h.View())
	}
	if !h.Contains("Repositories on synth-mac") || !strings.Contains(nsGroupOf(h.View(), "tundra-old"), "On other machines") || !h.Contains("tund") {
		t.Fatalf("not re-scoped (or the filter lost):\n%s", h.View())
	}
	if !h.Contains("○ Codex") || !h.Contains("Claude on synth-mac · demo/tundra · main checkout") {
		t.Fatalf("agents / summary on synth-mac:\n%s", h.View())
	}
	h.Key("up") // back to synth-linux
	if !h.Contains("tundra-old") || h.Contains("Codex") {
		t.Fatalf("back on linux:\n%s", h.View())
	}
	click(t, h, "○ synth-mac")
	h.Key("ctrl+s")
	req := startReq(t, h, rec)
	if req.NodeID != "node_synth-mac" || req.Cwd != "/Users/demo/code/tundra" {
		t.Fatalf("start request: %+v", req)
	}
}

// An offline machine is greyed out and cannot be chosen; this machine is
// the default when it is a node, else the last one used.
func TestNewSessionMachineDefaultsAndOffline(t *testing.T) {
	prep := func(b *mock.Backend) {
		if err := b.SetNodeConnected("node_synth-mac", false); err != nil {
			t.Fatal(err)
		}
	}
	h, _, _ := startMock(t, 120, 34, prep, nil)
	nsOpen(t, h)
	mac := nsMachineLine(h.View(), "synth-mac")
	if !strings.Contains(mac, "○ synth-mac") || !strings.Contains(mac, "offline") || strings.Contains(mac, "runs") {
		t.Fatalf("offline machine: %q\n%s", mac, h.View())
	}
	click(t, h, "○ synth-mac") // focuses Machine, chooses nothing
	h.Key("down")              // past the offline machine: on to Where
	if !strings.Contains(nsMachineLine(h.View(), "synth-linux"), "● synth-linux") || !strings.Contains(h.View(), "› Where") {
		t.Fatalf("offline machine chosen:\n%s", h.View())
	}

	// This machine first, when it is a node.
	h2, _, _ := startMock(t, 120, 34, nil, func(o *tui.Options) { o.ClientNodeID = "node_synth-mac" })
	nsOpen(t, h2)
	if !strings.Contains(nsMachineLine(h2.View(), "synth-mac"), "● synth-mac") {
		t.Fatalf("this machine not the default:\n%s", h2.View())
	}
}

// Moving through the Where list (keys and wheel) never changes another
// field's value.
func TestNewSessionListScrollKeepsChoices(t *testing.T) {
	h, _, _ := startMock(t, 120, 34, nil, nil)
	nsOpen(t, h)
	h.Key("down") // Machine: synth-mac
	h.Key("tab")  // Where
	h.Key("tab")  // Agent
	if !strings.Contains(h.View(), "› Agent") {
		t.Fatalf("tab did not reach Agent:\n%s", h.View())
	}
	h.Key("right")
	want := "Codex on synth-mac"
	if !h.Contains(want) || h.Contains("auto · ←→") {
		t.Fatalf("explicit choices (%q):\n%s", want, h.View())
	}
	h.Key("shift+tab") // back to Where
	before := nsRow(h.View())
	for i := 0; i < 6; i++ {
		h.Key("down")
		if !h.Contains(want) || !h.Contains("● Codex") {
			t.Fatalf("down #%d changed machine or agent:\n%s", i, h.View())
		}
	}
	if nsRow(h.View()) == before {
		t.Fatalf("down did not move the list:\n%s", h.View())
	}
	x, y := at(t, h, "Repositories on synth-mac")
	wheel(h, x, y+1, tea.MouseWheelUp)
	wheel(h, x, y+1, tea.MouseWheelUp)
	if !h.Contains(want) {
		t.Fatalf("wheel changed a choice:\n%s", h.View())
	}
	// Typing a new filter keeps them too.
	h.Type("notes")
	if !h.Contains(want + " · demo/notes-app") {
		t.Fatalf("filter changed a choice:\n%s", h.View())
	}
}

// Pickers change by click; Prompt and Title are fields; Enter on the last
// field starts.
func TestNewSessionClicksPromptTitle(t *testing.T) {
	h, rec, _ := startMock(t, 120, 34, nil, nil)
	nsOpen(t, h)
	h.Type("atlas")
	click(t, h, "○ synth-mac")
	click(t, h, "○ Codex")
	click(t, h, "○ fresh worktree")
	if !strings.Contains(h.View(), "› Workspace") {
		t.Fatalf("clicking an option did not focus its field:\n%s", h.View())
	}
	click(t, h, "Prompt")
	h.Type("fix the tile cache")
	h.Key("shift+enter")
	h.Type("and add a test")
	h.Key("enter") // Title
	h.Type("tile cache")
	if !h.Contains("Codex on synth-mac · demo/atlas · fresh worktree pyre/tile-cache · account auto") {
		t.Fatalf("summary:\n%s", h.View())
	}
	golden(t, "newsession-fields-120", h.View())
	h.Key("enter")
	req := startReq(t, h, rec)
	wt := rec.Calls("InvokeNodeAction:" + contracts.ActionWorktreeCreate)
	if len(wt) != 1 {
		t.Fatalf("worktree.create calls: %d", len(wt))
	}
	var p contracts.WorktreeCreateParams
	_ = json.Unmarshal(wt[0].Request.(contracts.NodeActionRequest).Params, &p)
	if len(p.Repos) != 1 || p.Repos[0] != "/Users/demo/code/atlas" || p.Slug != "tile cache" {
		t.Fatalf("create params: %+v", p)
	}
	if req.Harness != contracts.HarnessCodex || req.NodeID != "node_synth-mac" || req.Title != "tile cache" ||
		req.InitialPrompt != "fix the tile cache\nand add a test" || !strings.HasSuffix(req.Cwd, "/atlas/.worktrees/tile-cache") {
		t.Fatalf("start request: %+v", req)
	}
}

// A repository the machine lacks comes from "Clone a GitHub repo here…":
// a filter matching nothing here highlights it, Enter lists what can be
// cloned, and starting clones it first.
func TestNewSessionClonesMissingRepo(t *testing.T) {
	h, rec, _ := startMock(t, 120, 34, nil, nil)
	nsOpen(t, h)
	h.Type("orbit")
	if row := nsRow(h.View()); !strings.Contains(row, "Clone a GitHub repo here") || !strings.Contains(row, "1 not on synth-linux match “orbit”") {
		t.Fatalf("clone row not offered: %q\n%s", row, h.View())
	}
	h.Key("enter")
	if row := nsRow(h.View()); !strings.Contains(row, "orbit") || !h.Contains("Clone onto synth-linux") || !h.Contains("clone: orbit") {
		t.Fatalf("clone list: %q\n%s", row, h.View())
	}
	if !h.Contains("demo/orbit (clone first)") {
		t.Fatalf("clone not announced:\n%s", h.View())
	}
	golden(t, "newsession-clone-120", h.View())
	h.Key("ctrl+enter")
	req := startReq(t, h, rec)
	cl := rec.Calls("InvokeNodeAction:" + contracts.ActionRepoClone)
	if len(cl) != 1 {
		t.Fatalf("repo.clone calls: %d", len(cl))
	}
	var p contracts.RepoCloneParams
	_ = json.Unmarshal(cl[0].Request.(contracts.NodeActionRequest).Params, &p)
	if p.Remote != "https://example.invalid/demo/orbit.git" || cl[0].Request.(contracts.NodeActionRequest).NodeID != "node_synth-linux" {
		t.Fatalf("clone params: %+v", p)
	}
	if req.Cwd != "/home/demo/src/orbit" || req.NodeID != "node_synth-linux" {
		t.Fatalf("start request: %+v", req)
	}
	h.WaitFor("toast", 3*sec, func() bool { return !h.M.OverlayOpen() && h.Contains("launch requested") })
	// The clone is in the index now: the machine has it.
	nsOpen(t, h)
	h.Type("orbit")
	h.WaitFor("present", 3*sec, func() bool { return strings.Contains(nsRow(h.View()), "~/src/orbit") })
	// Esc leaves the clone list, not the panel; owner/name is offered directly.
	h.Key("ctrl+u")
	h.Type("someone/else")
	h.Key("enter")
	if row := nsRow(h.View()); !strings.Contains(row, "someone/else") {
		t.Fatalf("owner/name not offered: %q\n%s", row, h.View())
	}
	h.Key("esc")
	if !h.M.QuickLaunchOpen() || h.Contains("Clone onto") {
		t.Fatalf("esc did not leave the clone list:\n%s", h.View())
	}
}

// The agent and workspace used with a repository, and the machine, are
// remembered (marked auto).
func TestNewSessionAutoMachineAndMemory(t *testing.T) {
	h, rec, _ := startMock(t, 120, 34, nil, nil)
	nsOpen(t, h)
	click(t, h, "○ synth-mac")
	h.Type("notes")
	if !strings.Contains(nsRow(h.View()), "notes-app") {
		t.Fatalf("notes-app on synth-mac:\n%s", h.View())
	}
	h.Key("ctrl+t") // Codex (accelerator)
	click(t, h, "○ fresh worktree")
	h.Key("ctrl+s")
	req := startReq(t, h, rec)
	if req.NodeID != "node_synth-mac" || req.Harness != contracts.HarnessCodex {
		t.Fatalf("start request: %+v", req)
	}
	h.WaitFor("closed", 3*sec, func() bool { return !h.M.OverlayOpen() })
	nsOpen(t, h)
	if l := nsMachineLine(h.View(), "synth-mac"); !strings.Contains(l, "● synth-mac") || !strings.Contains(l, "auto") {
		t.Fatalf("last machine not the default: %q\n%s", l, h.View())
	}
	h.Type("notes")
	if !h.Contains("Codex on synth-mac · demo/notes-app · fresh worktree") || !h.Contains("● Codex   ○ Shell   auto") {
		t.Fatalf("not remembered:\n%s", h.View())
	}
}

// Several repositories: + adds the highlighted one; they get fresh
// worktrees.
func TestNewSessionMultiRepo(t *testing.T) {
	h, rec, _ := startMock(t, 120, 34, nil, nil)
	nsOpen(t, h)
	h.Type("api")
	h.Key("+")
	h.Type("web")
	if !h.Contains("demo/web + demo/api") && !h.Contains("demo/api + demo/web") {
		t.Fatalf("multi summary:\n%s", h.View())
	}
	if !h.Contains("○ main checkout") || !h.Contains("with  api ×") {
		t.Fatalf("multi workspace:\n%s", h.View())
	}
	h.Key("ctrl+s")
	req := startReq(t, h, rec)
	if req.NodeID != "node_synth-linux" || !strings.Contains(req.Cwd, "/pyre-work/") {
		t.Fatalf("multi start: %+v", req)
	}
}

// Problems are inline; nothing starts. ^a opens the step-by-step launch.
func TestNewSessionValidation(t *testing.T) {
	h, rec, _ := startMock(t, 120, 34, nil, nil)
	nsOpen(t, h)
	h.Type("zzz-nothing")
	if !h.Contains("nothing on synth-linux matches “zzz-nothing”") || !strings.Contains(nsRow(h.View()), "Browse folders") {
		t.Fatalf("unknown repo:\n%s", h.View())
	}
	h.Key("ctrl+s") // on Browse folders: opens the picker, starts nothing
	if !h.Contains("Use this folder") || len(rec.Calls("StartSession")) != 0 {
		t.Fatalf("unknown repo start:\n%s", h.View())
	}
	h.Key("esc")
	h.Key("ctrl+u")
	h.Type("atlas")
	click(t, h, "○ new branch")
	h.Key("ctrl+s")
	if !h.Contains("type the new branch name") || len(rec.Calls("StartSession")) != 0 {
		t.Fatalf("empty branch:\n%s", h.View())
	}
	h.Type("pyre/share-fix")
	h.Key("ctrl+s")
	startReq(t, h, rec)
	wt := rec.Calls("InvokeNodeAction:" + contracts.ActionWorktreeCreate)
	var p contracts.WorktreeCreateParams
	_ = json.Unmarshal(wt[0].Request.(contracts.NodeActionRequest).Params, &p)
	if p.Branch != "pyre/share-fix" {
		t.Fatalf("branch: %+v", p)
	}
	h.WaitFor("closed", 3*sec, func() bool { return !h.M.OverlayOpen() })
	h.Key("n")
	h.Key("ctrl+a")
	if h.M.ActiveView() != "Launch" {
		t.Fatalf("^a: %s", h.M.ActiveView())
	}
}

// Recent folders on the machine show by name with their path; another
// machine's do not.
func TestNewSessionRecentFolders(t *testing.T) {
	h, _, _ := startMock(t, 120, 34, nil, nil)
	nsOpen(t, h)
	if !h.Contains("Recent folders on synth-linux") || !strings.Contains(h.View(), "ses_synth-b             ~/work/ses_synth-b") || h.Contains("ses_synth-a ") {
		t.Fatalf("recent folders:\n%s", h.View())
	}
}

// agy is offered on a machine that has it; Model and Effort list the
// machine's choices for the agent and travel as the node's start args.
// Nothing chosen sends none (the harness default).
func TestNewSessionAgyModelAndEffort(t *testing.T) {
	h, rec, _ := startMock(t, 120, 40, func(b *mock.Backend) {
		_ = b.SetNodeHarnesses("node_synth-mac", []contracts.HarnessInfo{
			{Harness: contracts.HarnessClaude, Version: "2.1.289", Available: true, Models: []contracts.ModelChoice{{ID: "opus"}, {ID: "sonnet"}}, Efforts: []string{"low", "high"}},
			{Harness: contracts.HarnessAgy, Version: "1.4.2", Available: true, Models: []contracts.ModelChoice{{ID: "gemini-synthetic-medium", Default: true}, {ID: "claude-synthetic-high"}}, Efforts: []string{"low", "high", "max"}},
			{Harness: contracts.HarnessShell, Version: "tmux 3.5a", Available: true},
		})
	}, nil)
	nsOpen(t, h)
	click(t, h, "○ synth-mac")
	h.Type("tund")
	if !h.Contains("○ Agy") || h.Contains("Codex") {
		t.Fatalf("agents of synth-mac:\n%s", h.View())
	}
	click(t, h, "○ Agy")
	click(t, h, "○ claude-synthetic-high")
	click(t, h, "○ max")
	if !h.Contains("Agy on synth-mac · demo/tundra · main checkout · account auto · claude-synthetic-high max") {
		t.Fatalf("summary:\n%s", h.View())
	}
	h.Key("ctrl+s")
	req := startReq(t, h, rec)
	if req.Harness != contracts.HarnessAgy || strings.Join(req.Args, " ") != "--pyre-model=claude-synthetic-high --pyre-effort=max" {
		t.Fatalf("start request: harness %s args %v", req.Harness, req.Args)
	}
}

// A machine without agy (or codex) does not offer it.
func TestNewSessionNoAgyWithoutIt(t *testing.T) {
	h, rec, _ := startMock(t, 120, 34, nil, nil)
	nsOpen(t, h)
	h.Type("tund")
	if h.Contains("Agy") || h.Contains("Codex") {
		t.Fatalf("agent offered on a machine without it:\n%s", h.View())
	}
	h.Key("ctrl+s")
	if req := startReq(t, h, rec); len(req.Args) != 0 {
		t.Fatalf("default launch sent args %v", req.Args)
	}
}

// A session started in a fresh worktree shows it under i; archiving it
// offers removal (default keep), and y removes it through the node.
func TestArchiveOffersSessionWorktreeRemoval(t *testing.T) {
	var sid string
	prep := func(b *mock.Backend) {
		params, _ := json.Marshal(contracts.WorktreeCreateParams{Repos: []string{"/Users/demo/code/notes-app"}, Slug: "share link"})
		res, err := b.InvokeNodeAction(ctx(), contracts.NodeActionRequest{Mutation: contracts.Mutation{OperationID: "op_synth-wt"}, NodeID: "node_synth-mac", Action: contracts.ActionWorktreeCreate, Params: params})
		if err != nil || res.Error != nil {
			t.Fatalf("create: %v %v", err, res.Error)
		}
		var wt contracts.WorktreeCreateResult
		_ = json.Unmarshal(res.Result, &wt)
		st, err := b.StartSession(ctx(), contracts.StartSessionRequest{Mutation: contracts.Mutation{OperationID: "op_synth-start"}, NodeID: "node_synth-mac",
			Harness: contracts.HarnessShell, Title: "share link fix", Cwd: wt.Cwd})
		if err != nil {
			t.Fatal(err)
		}
		sid = st.Session.Session.SessionID
		b.Settle(10)
		if err := b.SetRunState(st.Session.Run.RunID, contracts.RunExited, "synthetic exit"); err != nil {
			t.Fatal(err)
		}
	}
	h, rec, _ := startMock(t, 120, 34, prep, nil)
	h.Key(".")
	selectSession(t, h, sid)
	h.Key("i")
	h.WaitFor("worktrees in info", 3*sec, func() bool { return h.Contains("Worktrees") && h.Contains("fresh") })
	if !h.Contains("~/code/notes-app/.worktrees/share-link") {
		t.Fatalf("info overlay:\n%s", h.View())
	}
	h.Key("esc")
	h.Key("X")
	h.WaitFor("archived", 3*sec, func() bool { return len(rec.Calls("UpdateSession")) == 1 })
	// The offer never pops up on its own (it would land seconds after the
	// archive, over whatever is selected then): a toast names it, W opens it.
	h.WaitFor("offer toast", 3*sec, func() bool { return h.Contains("W reviews / removes") })
	if h.Contains("Remove this session's worktrees?") {
		t.Fatalf("worktree offer opened by itself:\n%s", h.View())
	}
	h.Key("W")
	h.WaitFor("offer", 3*sec, func() bool { return h.Contains("Remove this session's worktrees?") })
	h.Settle()
	golden(t, "worktree-cleanup-120", h.View())
	h.Key("y")
	h.WaitFor("removed", 3*sec, func() bool { return h.Contains("removed 1 worktree") })
	rm := rec.Calls("InvokeNodeAction:" + contracts.ActionWorktreeRemove)
	if len(rm) != 1 {
		t.Fatalf("remove calls: %d", len(rm))
	}
	var p contracts.WorktreeRemoveParams
	_ = json.Unmarshal(rm[0].Request.(contracts.NodeActionRequest).Params, &p)
	if p.SessionID != sid || len(p.Paths) != 1 || p.Paths[0] != "/Users/demo/code/notes-app/.worktrees/share-link" {
		t.Fatalf("remove params: %+v", p)
	}
}

// ---- the folder picker ----

// pickerTree is a fake filesystem for synth-linux's dir.list.
func pickerTree(legacy bool) *mock.DirTree {
	return &mock.DirTree{Home: "/home/demo", WorktreeRoot: "/fast/worktrees", Legacy: legacy, Dirs: map[string][]string{
		"/":               {"fast", "home"},
		"/home":           {"demo"},
		"/fast":           {"worktrees"},
		"/fast/worktrees": {"pyre"},
		"~":               {"Documents", "scratch", "src", ".cache", ".config"},
		"~/src":           {"api", "apps", "atlas", "web"},
		"~/src/api":       {},
		"~/src/apps":      {},
		"~/src/atlas":     {},
		"~/scratch":       {},
		"~/.config":       {"pyre"},
	}}
}

// dirCounter counts dir.list calls per folder and can hold them (a slow
// machine).
type dirCounter struct {
	*mock.Backend
	mu    sync.Mutex
	n     map[string]int
	holds map[string]chan struct{}
}

func (d *dirCounter) InvokeNodeAction(ctx context.Context, req contracts.NodeActionRequest) (contracts.NodeActionResult, error) {
	if req.Action == contracts.ActionDirList {
		var p contracts.DirListParams
		_ = json.Unmarshal(req.Params, &p)
		d.mu.Lock()
		d.n[p.Path]++
		hold := d.holds[p.Path]
		d.mu.Unlock()
		if hold != nil {
			select {
			case <-hold:
			case <-ctx.Done():
				return contracts.NodeActionResult{}, ctx.Err()
			}
		}
	}
	return d.Backend.InvokeNodeAction(ctx, req)
}

func (d *dirCounter) count(p string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.n[p]
}

// startPicker starts the TUI on the mock with synthTree on synth-linux.
func startPicker(t *testing.T, legacy bool, holds map[string]chan struct{}) (*tuitest.Harness, *tuitest.Recorder, *dirCounter) {
	t.Helper()
	b, clk := tuitest.NewMock(t)
	b.SetDirTree("node_synth-linux", pickerTree(legacy))
	dc := &dirCounter{Backend: b, n: map[string]int{}, holds: holds}
	rec := tuitest.NewRecorder(dc)
	opts := tuitest.Options()
	opts.Clock = clk
	h := tuitest.New(t, rec, opts, 120, 34)
	h.WaitFor("initial sessions", 5*sec, func() bool { return h.M.Loaded("sessions") && h.M.Loaded("nodes") })
	h.Settle()
	return h, rec, dc
}

func pickerListed(h *tuitest.Harness) bool { return !h.Contains(" listing ") }

// The picker browses the chosen machine: Tab completes a unique prefix
// and lists the choices on ambiguity; Backspace on an empty segment and ←
// go up; dot-folders only for a segment starting with "."; ~ goes home;
// the worktree root is a shortcut; Enter on Use chooses.
func TestNewSessionPickerTabNavigateShortcuts(t *testing.T) {
	h, rec, dc := startPicker(t, false, nil)
	nsOpen(t, h)
	// Agent set first: choosing a folder must not change it.
	click(t, h, "○ Shell")
	click(t, h, "📁 Browse folders…")
	h.WaitFor("home listing", 3*sec, func() bool { return h.Contains("src/") && pickerListed(h) })
	if !h.Contains("in ~ on synth-linux  (~ is /home/demo)") || !strings.Contains(nsRow(h.View()), "✓ Use this folder") {
		t.Fatalf("picker at home:\n%s", h.View())
	}
	if !h.Contains("⑂ worktrees") || !h.Contains("/fast/worktrees") || !h.Contains("↺ ses_synth-b") || h.Contains(".config") || h.Contains("⌂ home") {
		t.Fatalf("shortcuts / dot-folders at home:\n%s", h.View())
	}
	h.Settle()
	golden(t, "newsession-picker-home-120", h.View())
	// Ambiguous: the choices are listed, nothing completes.
	h.Type("s")
	h.Key("tab")
	if !h.Contains("2 folders: scratch  src") || !h.Contains("📁 ~/s▏") {
		t.Fatalf("ambiguous tab:\n%s", h.View())
	}
	// Unique: completed and entered.
	h.Type("r")
	h.Key("tab")
	h.WaitFor("~/src", 3*sec, func() bool { return h.Contains("in ~/src on synth-linux") && pickerListed(h) })
	if !h.Contains("📁 ~/src/▏") || !h.Contains("atlas/") {
		t.Fatalf("unique tab:\n%s", h.View())
	}
	// A common prefix extends the segment.
	h.Type("a")
	h.Key("tab")
	if !h.Contains("📁 ~/src/a▏") || !h.Contains("3 folders: api  apps  atlas") || h.Contains("web/") {
		t.Fatalf("prefix tab:\n%s", h.View())
	}
	h.Type("p")
	h.Key("tab")
	if !h.Contains("📁 ~/src/ap▏") || !h.Contains("2 folders: api  apps") {
		t.Fatalf("lcp tab:\n%s", h.View())
	}
	h.Settle()
	golden(t, "newsession-picker-120", h.View())
	h.Type("i")
	h.Key("tab")
	h.WaitFor("~/src/api", 3*sec, func() bool { return h.Contains("in ~/src/api on") && pickerListed(h) })
	// Backspace on an empty segment goes up, the folder left highlighted.
	h.Key("backspace")
	h.WaitFor("up", 3*sec, func() bool { return h.Contains("in ~/src on") })
	if row := nsRow(h.View()); !strings.HasPrefix(row, "› api/") {
		t.Fatalf("up did not highlight api: %q\n%s", row, h.View())
	}
	// ← goes up too.
	h.Key("left")
	h.WaitFor("home", 3*sec, func() bool { return h.Contains("in ~ on") })
	if row := nsRow(h.View()); !strings.HasPrefix(row, "› src/") {
		t.Fatalf("left did not highlight src: %q\n%s", row, h.View())
	}
	// Dot-folders when the segment starts with ".".
	h.Type(".co")
	h.WaitFor("dot listing", 3*sec, func() bool { return h.Contains(".config/") && pickerListed(h) })
	if h.Contains(".cache/") {
		t.Fatalf("dot-folders:\n%s", h.View())
	}
	h.Key("tab")
	h.WaitFor("~/.config", 3*sec, func() bool { return h.Contains("in ~/.config on") && h.Contains("pyre/") })
	// ~ goes home; → enters the highlighted folder; ↓ reaches a shortcut.
	h.Type("~")
	h.WaitFor("home again", 3*sec, func() bool { return h.Contains("in ~ on") })
	h.Key("down") // ⑂ worktrees
	if row := nsRow(h.View()); !strings.Contains(row, "⑂ worktrees") {
		t.Fatalf("shortcut row: %q\n%s", row, h.View())
	}
	h.Key("right")
	h.WaitFor("worktree root", 3*sec, func() bool { return h.Contains("in /fast/worktrees on") && h.Contains("pyre/") })
	if !h.Contains("⌂ home") {
		t.Fatalf("home shortcut away from home:\n%s", h.View())
	}
	// Each folder was listed once (cached for the panel's lifetime).
	if n := dc.count("~/src"); n != 1 {
		t.Fatalf("~/src listed %d times", n)
	}
	h.Key("enter") // ✓ Use this folder
	if !h.Contains("Shell on synth-linux · /fast/worktrees · folder, no project") || !strings.Contains(h.View(), "› Agent") || h.Contains("in /fast/worktrees on") {
		t.Fatalf("chosen:\n%s", h.View())
	}
	if l := nsMachineLine(h.View(), "synth-linux"); strings.Contains(l, "auto") {
		t.Fatalf("machine not fixed to the browsed one: %q", l)
	}
	h.Key("ctrl+s")
	req := startReq(t, h, rec)
	if req.Cwd != "/fast/worktrees" || req.Harness != contracts.HarnessShell || req.NodeID != "node_synth-linux" {
		t.Fatalf("start: %+v", req)
	}
	if n := len(rec.Calls("InvokeNodeAction:" + contracts.ActionFsMkdir)); n != 0 {
		t.Fatalf("an existing folder was created: %d", n)
	}
}

// A path typed in Where opens the picker there; ^s uses the folder and
// starts; a folder that does not exist is created first.
func TestNewSessionPickerTypedPathAndCreate(t *testing.T) {
	h, rec, _ := startPicker(t, false, nil)
	nsOpen(t, h)
	h.Type("~/src/ne")
	h.WaitFor("listing", 3*sec, func() bool { return h.Contains("in ~/src on synth-linux") && pickerListed(h) })
	if !h.Contains("✓ Use ~/src/ne  new folder, created first") || !h.Contains("no folder starts with “ne”") {
		t.Fatalf("new folder:\n%s", h.View())
	}
	h.Type("w-tool")
	h.Key("ctrl+s")
	req := startReq(t, h, rec)
	mk := rec.Calls("InvokeNodeAction:" + contracts.ActionFsMkdir)
	if len(mk) != 1 || req.Cwd != "~/src/new-tool" || req.NodeID != "node_synth-linux" {
		t.Fatalf("mkdir %d, start %+v", len(mk), req)
	}
}

// Esc in the picker returns to the Where list; the panel stays open.
func TestNewSessionPickerEsc(t *testing.T) {
	h, _, _ := startPicker(t, false, nil)
	nsOpen(t, h)
	h.Type("~/src/")
	h.WaitFor("picker", 3*sec, func() bool { return h.Contains("in ~/src on") })
	h.Key("esc")
	if h.Contains("in ~/src on") || !h.M.QuickLaunchOpen() || !h.Contains("Repositories on synth-linux") {
		t.Fatalf("esc did not return to the list:\n%s", h.View())
	}
}

// A slow machine: a spinner while the listing loads, then the folders.
func TestNewSessionPickerSpinner(t *testing.T) {
	hold := make(chan struct{})
	h, _, _ := startPicker(t, false, map[string]chan struct{}{"~/src": hold})
	nsOpen(t, h)
	h.Type("~/src/")
	h.Settle()
	if !h.Contains("listing ~/src…") || h.Contains("atlas/") {
		t.Fatalf("no spinner while listing:\n%s", h.View())
	}
	// Use waits for the listing: whether the folder exists is not known yet.
	h.Key("enter")
	if !h.Contains("still listing ~/src…") || !h.Contains("in ~/src on") {
		t.Fatalf("Use chose before the listing:\n%s", h.View())
	}
	close(hold)
	h.WaitFor("listed", 3*sec, func() bool { return h.Contains("atlas/") && h.Contains("✓ Use this folder") })
}

// ^s takes the typed path, created when missing, unless the owner moved
// the highlight to a match or the match is exactly what was typed.
func TestNewSessionPickerStartTarget(t *testing.T) {
	for _, c := range []struct {
		name, typed string
		keys        []string
		cwd         string
		mkdir       bool
	}{
		{"typed new name, a match highlighted", "~/src/app", nil, "~/src/app", true},
		{"exact match", "~/src/apps", nil, "~/src/apps", false},
		{"moved to a match", "~/src/ap", []string{"down"}, "~/src/apps", false},
		{"tab-completed", "./src/at", []string{"tab"}, "~/src/atlas", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, rec, _ := startPicker(t, false, nil)
			nsOpen(t, h)
			h.Type(c.typed)
			h.WaitFor("listing", 3*sec, func() bool { return h.Contains("in ~/src on synth-linux") && pickerListed(h) })
			h.Key(c.keys...)
			h.WaitFor("listed", 3*sec, func() bool { return pickerListed(h) })
			h.Key("ctrl+s")
			req := startReq(t, h, rec)
			mk := len(rec.Calls("InvokeNodeAction:" + contracts.ActionFsMkdir))
			if req.Cwd != c.cwd || (mk == 1) != c.mkdir {
				t.Fatalf("start %q (mkdir %d), want %q (mkdir %v)", req.Cwd, mk, c.cwd, c.mkdir)
			}
		})
	}
}

// [ Start ^s ] while browsing starts in the browsed folder, never in
// Where's highlighted row.
func TestNewSessionStartButtonWhileBrowsing(t *testing.T) {
	for _, viaBrowse := range []bool{false, true} {
		h, rec, _ := startPicker(t, false, nil)
		nsOpen(t, h)
		want := "~/scratch"
		if viaBrowse {
			click(t, h, "📁 Browse folders…")
			h.WaitFor("home", 3*sec, func() bool { return h.Contains("src/") && pickerListed(h) })
			h.Type("sr")
			h.Key("tab")
			want = "~/src"
		} else {
			h.Type("~/scratch/")
		}
		h.WaitFor("browsing", 3*sec, func() bool { return h.Contains("in "+want+" on") && pickerListed(h) })
		click(t, h, "[ Start ^s ]")
		if req := startReq(t, h, rec); req.Cwd != want {
			t.Fatalf("browse=%v: Start began in %q, want %q", viaBrowse, req.Cwd, want)
		}
	}
}

// A lone "." is a filter, not a path; ./ and ../ open the picker resolved
// against the TUI's directory on this machine, at home on another one.
func TestNewSessionRelativePaths(t *testing.T) {
	wd, _ := os.Getwd()
	home, _ := os.UserHomeDir()
	norm := func(spec string) string {
		p, _ := repoindex.NormPath(spec, wd, home)
		if p == "/" {
			return p
		}
		return p + "/"
	}
	h, _, _ := startPicker(t, false, nil)
	nsOpen(t, h)
	h.Type("..")
	if h.Contains("▏  on synth-") || !h.Contains("› Where      ..▏") {
		t.Fatalf("a lone .. opened the picker:\n%s", h.View())
	}
	h.Key("ctrl+u")
	h.Type(".")
	if h.Contains("▏  on synth-") || !h.Contains("› Where      .▏") {
		t.Fatalf("a lone . opened the picker:\n%s", h.View())
	}
	h.Type("/sr")
	if !h.Contains("📁 ~/sr▏") { // remote: ./ is its home
		t.Fatalf("remote ./:\n%s", h.View())
	}

	local := func(t *testing.T, typed, want string) {
		b, clk := tuitest.NewMock(t)
		opts := tuitest.Options()
		opts.Clock = clk
		opts.ClientNodeID = "node_synth-linux"
		h := tuitest.New(t, tuitest.NewRecorder(b), opts, 120, 34)
		h.WaitFor("loaded", 5*sec, func() bool { return h.M.Loaded("sessions") && h.M.Loaded("nodes") })
		nsOpen(t, h)
		h.Type(typed)
		if !h.Contains("📁 " + want + "▏") {
			t.Fatalf("%q: want input %q:\n%s", typed, want, h.View())
		}
	}
	local(t, "./", norm("./"))
	local(t, "./src", norm("./")+"src")
	local(t, "../src", norm("../")+"src")
}

// A filter matching a repository only on another machine offers going
// there (remembered per repository) instead of cloning a second copy.
func TestNewSessionSwitchesToTheRepoMachine(t *testing.T) {
	h, rec, _ := startMock(t, 120, 34, nil, nil)
	nsOpen(t, h)
	h.Type("notes")
	row := nsRow(h.View())
	if !strings.Contains(row, "notes-app") || !strings.Contains(row, "on synth-mac") || nsGroupOf(h.View(), "notes-app") != "On other machines" {
		t.Fatalf("switch row: %q\n%s", row, h.View())
	}
	if !h.Contains("notes-app is on synth-mac: Enter (or ^s) switches there") {
		t.Fatalf("switch hint:\n%s", h.View())
	}
	h.Key("enter")
	if l := nsMachineLine(h.View(), "synth-mac"); !strings.Contains(l, "● synth-mac") || strings.Contains(l, "auto") {
		t.Fatalf("not switched: %q\n%s", l, h.View())
	}
	if row := nsRow(h.View()); !strings.Contains(row, "notes-app") || nsGroupOf(h.View(), "notes-app") != "Repositories on synth-mac" || !h.Contains("notes▏") {
		t.Fatalf("after switching: %q\n%s", row, h.View())
	}
	h.Key("ctrl+s")
	if req := startReq(t, h, rec); req.NodeID != "node_synth-mac" || req.Cwd != "/Users/demo/code/notes-app" {
		t.Fatalf("start: %+v", req)
	}
	if n := len(rec.Calls("InvokeNodeAction:" + contracts.ActionRepoClone)); n != 0 {
		t.Fatalf("cloned %d", n)
	}
	// ^s straight on a switch row switches and starts.
	h2, rec2, _ := startMock(t, 120, 34, nil, nil)
	nsOpen(t, h2)
	h2.Type("notes")
	h2.Key("ctrl+s")
	if req := startReq(t, h2, rec2); req.NodeID != "node_synth-mac" || req.Cwd != "/Users/demo/code/notes-app" {
		t.Fatalf("^s on a switch row: %+v", req)
	}
}

// nsGroupOf is the group header above the first row naming name.
func nsGroupOf(view, name string) string {
	group := ""
	for _, l := range strings.Split(view, "\n") {
		c := strings.TrimSpace(strings.Trim(strings.TrimSpace(l), "│"))
		switch {
		case strings.HasPrefix(c, "Repositories on ") || strings.HasPrefix(c, "Recent folders on ") || c == "On other machines" || strings.HasPrefix(c, "Clone onto "):
			group = c
		case strings.Contains(c, name+" ") && group != "":
			return group
		}
	}
	return ""
}

// An older pyred: no dot-folders, no worktree root; home still comes from
// its listing of ~, and browsing works as before.
func TestNewSessionPickerOlderNode(t *testing.T) {
	h, rec, _ := startPicker(t, true, nil)
	nsOpen(t, h)
	click(t, h, "📁 Browse folders…")
	h.WaitFor("home listing", 3*sec, func() bool { return h.Contains("src/") && pickerListed(h) })
	if h.Contains("⑂ worktrees") || !h.Contains("(~ is /home/demo)") {
		t.Fatalf("older node shortcuts:\n%s", h.View())
	}
	h.Type(".")
	h.WaitFor("dot listing", 3*sec, func() bool { return pickerListed(h) })
	if !h.Contains("does not list dot-folders") {
		t.Fatalf("older node dot-folders:\n%s", h.View())
	}
	h.Key("backspace")
	h.Key("left") // up from ~: the parent of home
	h.WaitFor("/home", 3*sec, func() bool { return h.Contains("in /home on") && h.Contains("demo/") })
	h.Type("~sr")
	h.Key("tab")
	h.WaitFor("~/src", 3*sec, func() bool { return h.Contains("in ~/src on") && h.Contains("atlas/") })
	h.Key("ctrl+enter")
	req := startReq(t, h, rec)
	if req.Cwd != "~/src" {
		t.Fatalf("start: %+v", req)
	}
}

// Folders without the picker's tree: "home" is the home folder (a shell
// scratch session); a pasted escaped path is unescaped; a new nested path
// is created first.
func TestNewSessionFolders(t *testing.T) {
	prep := func(b *mock.Backend) {
		for _, nid := range []string{"node_synth-mac", "node_synth-linux"} {
			params, _ := json.Marshal(contracts.FsMkdirParams{Path: "~/My Docs"})
			if _, err := b.InvokeNodeAction(ctx(), contracts.NodeActionRequest{Mutation: contracts.Mutation{OperationID: "op_synth-mk-" + nid}, NodeID: nid, Action: contracts.ActionFsMkdir, Params: params}); err != nil {
				t.Fatal(err)
			}
		}
		b.SetDirTree("node_synth-linux", &mock.DirTree{Home: "/home/demo", Dirs: map[string][]string{"~": {"src"}}})
	}
	cases := []struct {
		name, typed, paste, cwd string
		shell, mkdir            bool
	}{
		{name: "home", typed: "home", cwd: "~", shell: true},
		{name: "pasted escaped", paste: `~/My\ Docs`, cwd: "~/My Docs"},
		{name: "spaces new", typed: "~/New Stuff/two words", cwd: "~/New Stuff/two words", mkdir: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, rec, _ := startMock(t, 120, 34, prep, nil)
			nsOpen(t, h)
			h.Type(tc.typed)
			if tc.paste != "" {
				h.Paste(tc.paste)
			}
			h.Settle()
			if tc.shell {
				if !strings.Contains(nsRow(h.View()), "home") {
					t.Fatalf("home row:\n%s", h.View())
				}
				click(t, h, "○ Shell")
			}
			h.Key("ctrl+s")
			req := startReq(t, h, rec)
			want := contracts.HarnessClaude
			if tc.shell {
				want = contracts.HarnessShell
			}
			if req.Cwd != tc.cwd || req.Harness != want {
				t.Fatalf("start request: %+v", req)
			}
			if mk := rec.Calls("InvokeNodeAction:" + contracts.ActionFsMkdir); (len(mk) == 1) != tc.mkdir {
				t.Fatalf("fs.mkdir calls: %d (want create=%v)", len(mk), tc.mkdir)
			}
		})
	}
}
