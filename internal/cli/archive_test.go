package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/mock"
)

func sessionIDs(ss []c.SessionView) map[string]c.SessionView {
	out := map[string]c.SessionView{}
	for _, s := range ss {
		out[s.Session.SessionID] = s
	}
	return out
}

func TestSessionsArchiveUnarchiveAndLsFlags(t *testing.T) {
	h := newHub(t)
	e := newEnv(t, h)

	ls := func(args ...string) map[string]c.SessionView {
		t.Helper()
		r := run(t, e, nil, append([]string{"--json", "sessions", "ls"}, args...)...)
		expectCode(t, r, 0)
		return sessionIDs(mustJSON[c.Page[c.SessionView]](t, r).Items)
	}
	def := ls()
	if _, ok := def["ses_synth-fbx"]; !ok {
		t.Fatal("default ls must keep showing ended sessions")
	}
	ended := ls("--ended")
	if _, ok := ended["ses_synth-fbx"]; !ok || len(ended) == 0 || len(ended) >= len(def) {
		t.Fatalf("--ended: %d of %d", len(ended), len(def))
	}
	for id, s := range ended {
		if !sessionEnded(s) {
			t.Fatalf("--ended listed live %s (%s)", id, s.Run.State)
		}
	}

	// A live session is refused without --force.
	r := run(t, e, nil, "sessions", "archive", "ses_synth-b")
	if r.code == 0 || !strings.Contains(r.stderr, "stop it first") {
		t.Fatalf("archiving a live session must be refused: %s", r)
	}
	// An ended one is archived; --json prints an array.
	r = run(t, e, nil, "--json", "sessions", "archive", "ses_synth-fbx")
	expectCode(t, r, 0)
	got := mustJSON[[]c.SessionView](t, r)
	if len(got) != 1 || !got[0].Session.Archived {
		t.Fatalf("archive output: %s", r)
	}
	if _, ok := ls()["ses_synth-fbx"]; ok {
		t.Fatal("archived session still in default ls")
	}
	if _, ok := ls("--ended")["ses_synth-fbx"]; ok {
		t.Fatal("archived session in --ended")
	}
	if s, ok := ls("--all")["ses_synth-fbx"]; !ok || !s.Session.Archived {
		t.Fatal("--all must include archived sessions")
	}
	r = run(t, e, nil, "sessions", "ls", "--ended", "--all")
	expectCode(t, r, ExitUsage)

	// Unarchive by exact title (ResolveSession alone cannot see it).
	title := got[0].Session.Title
	r = run(t, e, nil, "sessions", "unarchive", title)
	expectCode(t, r, 0)
	if !strings.Contains(r.stdout, "restored ses_synth-fbx") {
		t.Fatalf("unarchive output: %s", r)
	}
	if _, ok := ls()["ses_synth-fbx"]; !ok {
		t.Fatal("unarchived session missing from ls")
	}

	// --force archives a live session (it keeps running).
	r = run(t, e, nil, "sessions", "archive", "--force", "ses_synth-b")
	expectCode(t, r, 0)
	sv, err := h.b.GetSession(t.Context(), c.GetRequest{ID: "ses_synth-b"})
	if err != nil || !sv.Session.Archived || sv.Run == nil || sv.Run.State.Terminal() {
		t.Fatalf("forced archive: %+v %v", sv.Session, err)
	}
}

func TestSessionsPrune(t *testing.T) {
	h := newHub(t)
	e := newEnv(t, h)
	before, err := h.b.ListSessions(t.Context(), c.ListSessionsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var wantEnded []string
	for _, s := range before.Items {
		if sessionEnded(s) {
			wantEnded = append(wantEnded, s.Session.SessionID)
		}
	}
	if len(wantEnded) == 0 {
		t.Fatal("fixture has no ended sessions")
	}

	// Dry run changes nothing.
	r := run(t, e, nil, "--json", "sessions", "prune", "--dry-run")
	expectCode(t, r, 0)
	dry := mustJSON[PruneResult](t, r)
	if !dry.DryRun || len(dry.Archived) != len(wantEnded) {
		t.Fatalf("dry run listed %d, want %d: %s", len(dry.Archived), len(wantEnded), r)
	}
	// A huge --older-than keeps everything.
	r = run(t, e, nil, "--json", "sessions", "prune", "--older-than", "87600h")
	expectCode(t, r, 0)
	if res := mustJSON[PruneResult](t, r); len(res.Archived) != 0 {
		t.Fatalf("--older-than 10y archived %d", len(res.Archived))
	}
	// --node limits to one machine.
	r = run(t, e, nil, "--json", "sessions", "prune", "--node", "synth-linux", "--dry-run")
	expectCode(t, r, 0)
	for _, s := range mustJSON[PruneResult](t, r).Archived {
		if s.Run != nil && s.Run.NodeID != "node_synth-linux" {
			t.Fatalf("--node synth-linux listed %s on %s", s.Session.SessionID, s.Run.NodeID)
		}
	}
	// The real thing archives every ended session and nothing live.
	r = run(t, e, nil, "sessions", "prune")
	expectCode(t, r, 0)
	if !strings.Contains(r.stdout, "archived "+itoa(len(wantEnded))+" ended session(s)") {
		t.Fatalf("prune output: %s", r)
	}
	after, err := h.b.ListSessions(t.Context(), c.ListSessionsRequest{IncludeArchived: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range after.Items {
		if s.Session.Archived != sessionEnded(s) {
			t.Fatalf("%s: archived=%v ended=%v", s.Session.SessionID, s.Session.Archived, sessionEnded(s))
		}
	}
}

// archive_session is the CLI's Archive: an ended session is archived at
// once, a live one when its current turn ends (W-archive2).
func TestMCPArchiveSession(t *testing.T) {
	b := mock.New(mock.Options{Clock: mock.NewOffsetClock(mock.FixtureEpoch), DefaultPrincipal: &testOwner})
	cl := startMCP(t, &Ops{API: b, PollInterval: 10 * time.Millisecond})
	cl.call("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "test", "version": "0"}})
	out, isErr := cl.tool("archive_session", map[string]any{"session": "ses_synth-b"})
	var res ArchiveResult
	if isErr || json.Unmarshal(out, &res) != nil || res.Archived || !res.WhenDone {
		t.Fatalf("a live session archives when done: %s", out)
	}
	out, isErr = cl.tool("archive_session", map[string]any{"session": "ses_synth-fbx"})
	if isErr || json.Unmarshal(out, &res) != nil || !res.Archived {
		t.Fatalf("archive_session: %s", out)
	}
	if out, isErr = cl.tool("archive_session", map[string]any{"session": "ses_synth-fbx", "force": true}); !isErr {
		t.Fatalf("force must not be an MCP argument: %s", out)
	}
}

// An agent may archive its own session and the sessions it started, not an
// unrelated one (W-childnotify).
func TestAgentArchiveScope(t *testing.T) {
	h := newHub(t)
	agentA := newEnv(t, h).with(EnvToken, "", EnvSessionToken, sessAToken, EnvSessionID, "ses_synth-a", EnvNodeID, "node_synth-mac")
	r := run(t, agentA, nil, "sessions", "archive", "ses_synth-fbx", "--force")
	if r.code == 0 || !strings.Contains(r.stderr, "archive only its own session") {
		t.Fatalf("agent archived an unrelated session: %s", r)
	}
	r = run(t, agentA, nil, "--json", "start", "--harness", "claude", "--cwd", "/home/synth/child", "--title", "synthetic child")
	expectCode(t, r, 0)
	child := mustJSON[c.StartSessionResponse](t, r).Session.Session.SessionID
	r = run(t, agentA, nil, "--json", "sessions", "archive", child, "--force")
	expectCode(t, r, 0)
	if got := mustJSON[[]c.SessionView](t, r); len(got) != 1 || !got[0].Session.Archived {
		t.Fatalf("agent archiving its child: %s", r)
	}
	// And rename the child it started (its launch title is agent-chosen).
	r = run(t, agentA, nil, "--json", "sessions", "rename", child, "synthetic reviewer")
	expectCode(t, r, 0)
}
