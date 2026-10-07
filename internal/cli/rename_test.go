package cli

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/autotitle"
	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/mock"
)

func TestSessionsRenameCLI(t *testing.T) {
	h := newHub(t)
	e := newEnv(t, h)
	r := run(t, e, nil, "--json", "sessions", "rename", "ses_synth-a", "fix", "the", "synthetic", "uploader")
	expectCode(t, r, 0)
	sv := mustJSON[c.SessionView](t, r)
	if sv.Session.Title != "fix the synthetic uploader" || slices.ContainsFunc(sv.Session.Tags, autotitle.Reserved) {
		t.Fatalf("rename: %q %v", sv.Session.Title, sv.Session.Tags)
	}
	r = run(t, e, nil, "sessions", "rename", "ses_synth-a", "renamed again")
	expectCode(t, r, 0)
	if !strings.Contains(r.stdout, `renamed ses_synth-a "renamed again"`) {
		t.Fatalf("text output: %s", r)
	}
	expectCode(t, run(t, e, nil, "sessions", "rename", "ses_synth-a"), ExitUsage)
}

// rename_session is pre-approved for agents and works on any session (owner
// decision, W-childnotify), but never over a title the owner chose.
func TestMCPRenameSessionAnySessionNotOverOwner(t *testing.T) {
	agent := c.Principal{Kind: c.PrincipalSession, ID: "ses_synth-a"}
	b := mock.New(mock.Options{Clock: mock.NewOffsetClock(mock.FixtureEpoch), DefaultPrincipal: &agent})
	ownerCtx := c.WithPrincipal(context.Background(), testOwner)
	tags := []string{autotitle.TagAuto}
	if _, err := b.UpdateSession(ownerCtx, c.UpdateSessionRequest{Mutation: mut(""), SessionID: "ses_synth-a", Tags: &tags}); err != nil {
		t.Fatal(err)
	}
	cl := startMCP(t, &Ops{API: b, Self: Identity{SessionID: "ses_synth-a"}, PollInterval: 10 * time.Millisecond})
	cl.call("initialize", map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "test", "version": "0"}})

	out, isErr := cl.tool("rename_session", map[string]any{"title": "Synthetic uploader retries"})
	var sv c.SessionView
	if isErr || json.Unmarshal(out, &sv) != nil || sv.Session.Title != "Synthetic uploader retries" || !slices.Contains(sv.Session.Tags, autotitle.TagAgent) {
		t.Fatalf("self rename: %v %s", isErr, out)
	}
	// Another session whose title the owner chose: allowed too.
	if out, isErr := cl.tool("rename_session", map[string]any{"session": "ses_synth-b", "title": "Peer name"}); isErr {
		t.Fatalf("rename over the owner's title of another session: %s", out)
	}
	// Another session with an automatic title: allowed.
	if _, err := b.UpdateSession(ownerCtx, c.UpdateSessionRequest{Mutation: mut(""), SessionID: "ses_synth-b", Tags: &tags}); err != nil {
		t.Fatal(err)
	}
	out, isErr = cl.tool("rename_session", map[string]any{"session": "ses_synth-b", "title": "Peer name"})
	if isErr || json.Unmarshal(out, &sv) != nil || sv.Session.SessionID != "ses_synth-b" || sv.Session.Title != "Peer name" {
		t.Fatalf("rename another session: %v %s", isErr, out)
	}
	// The owner's rename ends auto-naming; an agent may still rename on request.
	mine := "Owner name"
	if _, err := b.UpdateSession(ownerCtx, c.UpdateSessionRequest{Mutation: mut(""), SessionID: "ses_synth-a", Title: &mine}); err != nil {
		t.Fatal(err)
	}
	if out, isErr := cl.tool("rename_session", map[string]any{"title": "Agent again"}); isErr {
		t.Fatalf("agent rename over the owner's title: %s", out)
	}
}
