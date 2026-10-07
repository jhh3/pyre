package tui_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/mock"
)

func TestQuickLaunchBarStartsInMainCheckout(t *testing.T) {
	h, rec, _ := startMock(t, 120, 34, nil, nil)
	h.Key("n")
	if !h.M.QuickLaunchOpen() {
		t.Fatal("n did not open the quick-launch bar")
	}
	h.WaitFor("repo index", 3*sec, func() bool { return h.Contains("notes-app") })
	h.Type("tund")
	if row := qlCurrentRow(h.View()); !strings.Contains(row, "tundra") || strings.Contains(row, "tundra-old") {
		t.Fatalf("best completion not selected: %q\n%s", row, h.View())
	}
	if !h.Contains("Claude on synth-mac · ~/code/tundra · main checkout") || !h.Contains("● claude") || !h.Contains("● synth-mac") {
		t.Fatalf("plan not shown:\n%s", h.View())
	}
	golden(t, "quicklaunch-120", h.View())
	h.Key("enter")
	h.WaitFor("start", 3*sec, func() bool { return len(rec.Calls("StartSession")) == 1 })
	req := rec.Calls("StartSession")[0].Request.(contracts.StartSessionRequest)
	if req.Cwd != "/Users/demo/code/tundra" || req.Harness != contracts.HarnessClaude || req.NodeID != "node_synth-mac" || req.InitialPrompt != "" || req.OperationID == "" {
		t.Fatalf("start request: %+v", req)
	}
	if len(rec.Calls("InvokeNodeAction:"+contracts.ActionWorktreeCreate)) != 0 {
		t.Fatal("main-checkout launch created a worktree")
	}
	h.WaitFor("back on Sessions", 3*sec, func() bool {
		return !h.M.OverlayOpen() && h.Contains("launch requested: claude in tundra on synth-mac")
	})
}

func TestQuickLaunchFreshMachineHarness(t *testing.T) {
	h, rec, _ := startMock(t, 120, 34, nil, nil)
	h.Key("n")
	h.WaitFor("repo index", 3*sec, func() bool { return h.Contains("notes-app") })
	h.Type("atlas")
	first := ""
	if strings.Contains(h.View(), "● synth-linux") {
		first = "synth-linux"
	} else if strings.Contains(h.View(), "● synth-mac") {
		first = "synth-mac"
	}
	h.Key("ctrl+o") // the other machine
	if first == "" || h.Contains("● "+first) {
		t.Fatalf("machine chip did not cycle from %q:\n%s", first, h.View())
	}
	h.Key("ctrl+t") // codex
	h.Key("ctrl+f") // fresh worktree
	h.Type(" fix the tile cache")
	if !h.Contains("● codex") || !h.Contains("fresh worktree on branch pyre/fix-tile-cache") || !h.Contains("fix the tile cache  (from the line)") {
		t.Fatalf("plan:\n%s", h.View())
	}
	golden(t, "quicklaunch-fresh-120", h.View())
	h.Key("enter")
	h.WaitFor("start", 3*sec, func() bool { return len(rec.Calls("StartSession")) == 1 })
	wt := rec.Calls("InvokeNodeAction:" + contracts.ActionWorktreeCreate)
	if len(wt) != 1 {
		t.Fatalf("worktree.create calls: %d", len(wt))
	}
	var p contracts.WorktreeCreateParams
	_ = json.Unmarshal(wt[0].Request.(contracts.NodeActionRequest).Params, &p)
	if len(p.Repos) != 1 || p.Slug != "fix the tile cache" {
		t.Fatalf("create params: %+v", p)
	}
	req := rec.Calls("StartSession")[0].Request.(contracts.StartSessionRequest)
	if req.Harness != contracts.HarnessCodex || !strings.HasSuffix(req.Cwd, "/atlas/.worktrees/fix-tile-cache") || req.InitialPrompt != "fix the tile cache" || req.NodeID == "node_"+first {
		t.Fatalf("start request: %+v", req)
	}
}

func TestQuickLaunchMultiRepoAndAdvanced(t *testing.T) {
	h, rec, _ := startMock(t, 120, 34, nil, nil)
	h.Key("n")
	h.WaitFor("repo index", 3*sec, func() bool { return h.Contains("notes-app") })
	h.Type("api+web")
	if !h.Contains("fresh worktrees of api + web") || !h.Contains("● synth-linux") || h.Contains("synth-mac (") {
		t.Fatalf("multi plan:\n%s", h.View())
	}
	h.Key("enter")
	h.WaitFor("start", 3*sec, func() bool { return len(rec.Calls("StartSession")) == 1 })
	req := rec.Calls("StartSession")[0].Request.(contracts.StartSessionRequest)
	if req.NodeID != "node_synth-linux" || !strings.Contains(req.Cwd, "/pyre-work/") {
		t.Fatalf("multi start: %+v", req)
	}
	h.WaitFor("launched", 3*sec, func() bool { return !h.M.OverlayOpen() })
	// ^a opens the step-by-step launch; an unknown name says so on Enter.
	h.Key("n")
	h.Type("zzz-nothing")
	h.Key("enter")
	if !h.Contains("no repository matches “zzz-nothing”") || len(rec.Calls("StartSession")) != 1 {
		t.Fatalf("unknown repo:\n%s", h.View())
	}
	h.Key("ctrl+a")
	if h.M.ActiveView() != "Launch" {
		t.Fatalf("tab: %s", h.M.ActiveView())
	}
}

func TestQuickLaunchEmptyLineStartsRecentRepo(t *testing.T) {
	h, rec, _ := startMock(t, 120, 34, nil, nil)
	h.Key("n")
	h.WaitFor("repo index", 3*sec, func() bool { return h.Contains("notes-app") })
	h.Settle()
	golden(t, "quicklaunch-open-120", h.View())
	row := qlCurrentRow(h.View())
	h.Key("enter")
	h.WaitFor("start", 3*sec, func() bool { return len(rec.Calls("StartSession")) == 1 })
	req := rec.Calls("StartSession")[0].Request.(contracts.StartSessionRequest)
	name := req.Cwd[strings.LastIndexByte(req.Cwd, '/')+1:]
	if !strings.Contains(row, name) {
		t.Fatalf("started %s, selected row %q", req.Cwd, row)
	}
}

// qlCurrentRow is the selected suggestion in the new-session panel (the
// input line, always first, also starts with ›).
func qlCurrentRow(view string) string {
	n := 0
	for _, l := range strings.Split(view, "\n") {
		c := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l), "│"))
		if strings.HasPrefix(c, "›") {
			if n++; n == 2 {
				return c
			}
		}
	}
	return ""
}

// A path launches in a plain folder on the chosen machine: Tab completes
// a folder segment from the machine's listing, Enter starts there.
func TestQuickLaunchFolderPath(t *testing.T) {
	h, rec, _ := startMock(t, 120, 34, nil, nil)
	h.Key("n")
	h.Type("~/scr")
	h.WaitFor("listing", 3*sec, func() bool { return strings.Contains(qlCurrentRow(h.View()), "scratch") })
	if !h.Contains("create scr") {
		t.Fatalf("missing folder not offered for creation:\n%s", h.View())
	}
	h.Key("tab")
	h.WaitFor("completed", 3*sec, func() bool { return h.Contains("~/scratch · folder, no project") })
	if !h.Contains("› ~/scratch/") {
		t.Fatalf("tab did not complete the segment:\n%s", h.View())
	}
	h.Settle()
	golden(t, "quicklaunch-path-120", h.View())
	h.Key("tab") // nothing left to complete: the prompt field
	h.Type("tidy up")
	h.Key("enter")
	h.WaitFor("start", 3*sec, func() bool { return len(rec.Calls("StartSession")) == 1 })
	req := rec.Calls("StartSession")[0].Request.(contracts.StartSessionRequest)
	if req.Cwd != "~/scratch" || req.InitialPrompt != "tidy up" || req.Harness != contracts.HarnessClaude {
		t.Fatalf("start request: %+v", req)
	}
	if len(rec.Calls("InvokeNodeAction:"+contracts.ActionFsMkdir)) != 0 {
		t.Fatal("existing folder was created")
	}
}

// A folder that does not exist is created through the machine first.
func TestQuickLaunchCreatesFolder(t *testing.T) {
	h, rec, _ := startMock(t, 120, 34, nil, nil)
	h.Key("n")
	h.Type("~/vid-convert @synth-mac codex")
	h.WaitFor("create offer", 3*sec, func() bool { return h.Contains("new folder, created on start") })
	if row := qlCurrentRow(h.View()); !strings.Contains(row, "create vid-convert") {
		t.Fatalf("create row not selected: %q\n%s", row, h.View())
	}
	h.Key("enter")
	h.WaitFor("start", 3*sec, func() bool { return len(rec.Calls("StartSession")) == 1 })
	mk := rec.Calls("InvokeNodeAction:" + contracts.ActionFsMkdir)
	if len(mk) != 1 {
		t.Fatalf("fs.mkdir calls: %d", len(mk))
	}
	var p contracts.FsMkdirParams
	_ = json.Unmarshal(mk[0].Request.(contracts.NodeActionRequest).Params, &p)
	req := rec.Calls("StartSession")[0].Request.(contracts.StartSessionRequest)
	if p.Path != "~/vid-convert" || req.Cwd != "~/vid-convert" || req.Harness != contracts.HarnessCodex || req.NodeID != mk[0].Request.(contracts.NodeActionRequest).NodeID {
		t.Fatalf("mkdir %+v start %+v", p, req)
	}
	h.WaitFor("toast", 3*sec, func() bool { return h.Contains("launch requested: codex in ~/vid-convert (new folder)") })
}

// "home" (or ~) is the home folder: a projectless scratch session; chips
// click.
func TestQuickLaunchHomeAndChipClicks(t *testing.T) {
	h, rec, _ := startMock(t, 120, 34, nil, nil)
	h.Key("n")
	h.Type("home")
	h.WaitFor("home", 3*sec, func() bool { return h.Contains("home folder · scratch session, no project") })
	click(t, h, "○ shell")
	if !h.Contains("● shell") {
		t.Fatalf("clicking the shell chip:\n%s", h.View())
	}
	h.Key("enter")
	h.WaitFor("start", 3*sec, func() bool { return len(rec.Calls("StartSession")) == 1 })
	req := rec.Calls("StartSession")[0].Request.(contracts.StartSessionRequest)
	if req.Cwd != "~" || req.Harness != contracts.HarnessShell {
		t.Fatalf("start request: %+v", req)
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
	h.WaitFor("offer toast", 3*sec, func() bool { return h.Contains("W reviews / removes them") })
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

// Paths with spaces: an escaped (~/My\ Docs, as terminals paste a dragged
// folder) or quoted target, and ^l, where the whole line is the path.
func TestQuickLaunchPathWithSpaces(t *testing.T) {
	prep := func(b *mock.Backend) {
		for _, nid := range []string{"node_synth-mac", "node_synth-linux"} {
			params, _ := json.Marshal(contracts.FsMkdirParams{Path: "~/My Docs"})
			if _, err := b.InvokeNodeAction(ctx(), contracts.NodeActionRequest{Mutation: contracts.Mutation{OperationID: "op_synth-mk-" + nid}, NodeID: nid, Action: contracts.ActionFsMkdir, Params: params}); err != nil {
				t.Fatal(err)
			}
		}
	}
	cases := []struct {
		name, typed, cwd string
		whole            bool
		harness          contracts.Harness
		prompt           string
	}{
		{"escaped+tab", `~/My\ D`, "~/My Docs", false, contracts.HarnessCodex, "tidy"},
		{"quoted", `"~/My Docs" shell look around`, "~/My Docs", false, contracts.HarnessShell, "look around"},
		{"whole line", `~/My Docs`, "~/My Docs", true, contracts.HarnessClaude, ""},
		{"whole line new", `~/New Stuff/two words`, "~/New Stuff/two words", true, contracts.HarnessClaude, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, rec, _ := startMock(t, 120, 34, prep, nil)
			h.Key("n")
			if tc.whole {
				h.Key("ctrl+l")
				if !h.Contains("the whole line is one folder path") {
					t.Fatalf("^l not shown:\n%s", h.View())
				}
			}
			h.Type(tc.typed)
			if tc.name == "escaped+tab" {
				h.WaitFor("listing", 3*sec, func() bool { return strings.Contains(qlCurrentRow(h.View()), "My Docs") })
				h.Key("tab")
				if !h.Contains(`› ~/My\ Docs/`) {
					t.Fatalf("tab did not complete with an escape:\n%s", h.View())
				}
				h.Type(" codex tidy")
			}
			h.WaitFor("target", 3*sec, func() bool { return h.Contains(tc.cwd+" · ") && !h.Contains("checking") })
			h.Key("enter")
			h.WaitFor("start", 3*sec, func() bool { return len(rec.Calls("StartSession")) == 1 })
			req := rec.Calls("StartSession")[0].Request.(contracts.StartSessionRequest)
			if req.Cwd != tc.cwd || req.Harness != tc.harness || req.InitialPrompt != tc.prompt {
				t.Fatalf("start request: %+v", req)
			}
			mk := rec.Calls("InvokeNodeAction:" + contracts.ActionFsMkdir)
			if want := tc.name == "whole line new"; (len(mk) == 1) != want {
				t.Fatalf("fs.mkdir calls: %d (want create=%v)", len(mk), want)
			}
		})
	}
}
