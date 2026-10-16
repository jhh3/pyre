package cli

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/mock"
)

// W-archive2: archive_session with no arguments archives the calling
// session when its turn ends; the answer says so at once.
func TestMCPArchiveSelfWhenDone(t *testing.T) {
	agent := c.Principal{Kind: c.PrincipalSession, ID: "ses_synth-b"}
	b := mock.New(mock.Options{Clock: mock.NewOffsetClock(mock.FixtureEpoch), DefaultPrincipal: &agent})
	cl := startMCP(t, &Ops{API: b, Self: Identity{SessionID: "ses_synth-b"}, PollInterval: 10 * time.Millisecond})
	cl.call("initialize", map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "test", "version": "0"}})

	out, isErr := cl.tool("archive_session", map[string]any{})
	var res ArchiveResult
	if isErr || json.Unmarshal(out, &res) != nil {
		t.Fatalf("self archive: %v %s", isErr, out)
	}
	if res.SessionID != "ses_synth-b" || res.Archived || !res.WhenDone || !strings.Contains(res.Note, "when this turn ends") {
		t.Fatalf("self archive of a live session is scheduled: %+v", res)
	}
	tl := toolByName(t, "archive_session")
	if !strings.Contains(tl.Description, "archive yourself with archive_session (no arguments)") {
		t.Fatalf("description: %s", tl.Description)
	}
	if req, _ := tl.InputSchema["required"].([]string); len(req) != 0 {
		t.Fatalf("session is optional: %v", req)
	}
}

func toolByName(t *testing.T, name string) MCPTool {
	t.Helper()
	for _, tl := range MCPTools() {
		if tl.Name == name {
			return tl
		}
	}
	t.Fatalf("no tool %s", name)
	return MCPTool{}
}

func TestSessionsArchiveWhenDoneAndProjectArchive(t *testing.T) {
	h := newHub(t)
	e := newEnv(t, h)
	// The owner archives a live session when done.
	r := run(t, e, nil, "--json", "sessions", "archive", "--when-done", "ses_synth-b")
	expectCode(t, r, 0)
	got := mustJSON[[]c.SessionView](t, r)
	if len(got) != 1 || got[0].Session.Archived || !slices.Contains(got[0].Session.Tags, c.TagArchiveWhenDone) {
		t.Fatalf("--when-done schedules: %s", r)
	}
	// Unarchive cancels the pending archive.
	r = run(t, e, nil, "sessions", "unarchive", "ses_synth-b")
	expectCode(t, r, 0)
	if v, _ := h.b.GetSession(ownerCtxFor(), c.GetRequest{ID: "ses_synth-b"}); slices.Contains(v.Session.Tags, c.TagArchiveWhenDone) {
		t.Fatal("unarchive cancels")
	}
	// No session outside a Pyre session is a usage error.
	expectCode(t, run(t, e, nil, "sessions", "archive"), ExitUsage)

	// A project with live sessions is refused without --with-sessions.
	r = run(t, e, nil, "project", "archive", "prj_synth-outfit")
	if r.code == 0 || !strings.Contains(r.stderr, "--with-sessions") {
		t.Fatalf("live sessions refuse: %s", r)
	}
	r = run(t, e, nil, "--json", "project", "archive", "prj_synth-outfit", "--with-sessions")
	expectCode(t, r, 0)
	res := mustJSON[ArchiveProjectResult](t, r)
	if !res.Archived || len(res.WhenDone) == 0 {
		t.Fatalf("--with-sessions: %+v", res)
	}
	for _, s := range res.WhenDone {
		v, _ := h.b.GetSession(ownerCtxFor(), c.GetRequest{ID: s.SessionID})
		if v.Session.Archived || !slices.Contains(v.Session.Tags, c.TagArchiveWhenDone) {
			t.Fatalf("live project session scheduled: %+v", v.Session)
		}
	}
}

func ownerCtxFor() context.Context { return c.WithPrincipal(context.Background(), testOwner) }

// W-archive2: session icons through the CLI and MCP set_session_icon.
func TestSessionIconCLIAndMCP(t *testing.T) {
	h := newHub(t)
	e := newEnv(t, h)
	r := run(t, e, nil, "--json", "sessions", "icon", "ses_synth-b", "🚀")
	expectCode(t, r, 0)
	if sv := mustJSON[c.SessionView](t, r); sv.Session.Icon != "🚀" {
		t.Fatalf("icon set: %s", r)
	}
	r = run(t, e, nil, "sessions", "icon", "ses_synth-b", "xy")
	if r.code == 0 {
		t.Fatalf("not an emoji is refused: %s", r)
	}
	r = run(t, e, nil, "--json", "sessions", "icon", "ses_synth-b")
	expectCode(t, r, 0)
	if sv := mustJSON[c.SessionView](t, r); sv.Session.Icon != "" {
		t.Fatalf("no emoji clears: %s", r)
	}

	agent := c.Principal{Kind: c.PrincipalSession, ID: "ses_synth-a"}
	b := mock.New(mock.Options{Clock: mock.NewOffsetClock(mock.FixtureEpoch), DefaultPrincipal: &agent})
	cl := startMCP(t, &Ops{API: b, Self: Identity{SessionID: "ses_synth-a"}, PollInterval: 10 * time.Millisecond})
	cl.call("initialize", map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "test", "version": "0"}})
	out, isErr := cl.tool("set_session_icon", map[string]any{"icon": "🐛"})
	var sv c.SessionView
	if isErr || json.Unmarshal(out, &sv) != nil || sv.Session.SessionID != "ses_synth-a" || sv.Session.Icon != "🐛" {
		t.Fatalf("self icon: %v %s", isErr, out)
	}
	out, isErr = cl.tool("rename_session", map[string]any{"session": "ses_synth-b", "title": "Synthetic peer", "icon": "🧪"})
	if isErr || json.Unmarshal(out, &sv) != nil || sv.Session.Icon != "🧪" || sv.Session.Title != "Synthetic peer" {
		t.Fatalf("rename with icon: %v %s", isErr, out)
	}
}
