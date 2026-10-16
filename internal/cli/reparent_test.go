package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/mock"
)

func TestSessionsReparentCLI(t *testing.T) {
	h := newHub(t)
	e := newEnv(t, h)
	r := run(t, e, nil, "--json", "sessions", "reparent", "ses_synth-a", "ses_synth-b", "--to", "ses_synth-standalone")
	expectCode(t, r, 0)
	res := mustJSON[ReparentResult](t, r)
	if len(res.Sessions) != 2 {
		t.Fatalf("bulk: %+v", res)
	}
	for _, it := range res.Sessions {
		if it.Error != "" || it.ParentID != "ses_synth-standalone" {
			t.Fatalf("item: %+v", it)
		}
		s, _ := h.b.GetSession(ownerCtxFor(), c.GetRequest{ID: it.SessionID})
		if s.Session.Parent() != "ses_synth-standalone" || s.Session.Lineage.ReparentedAt == nil {
			t.Fatalf("%s lineage: %+v", it.SessionID, s.Session.Lineage)
		}
	}
	// A cycle is refused with the hub's reason.
	r = run(t, e, nil, "sessions", "reparent", "ses_synth-standalone", "--to", "ses_synth-a")
	if r.code == 0 || !strings.Contains(r.stdout+r.stderr, "cycle") {
		t.Fatalf("cycle not refused: %s", r)
	}
	// --to none moves to the top level; --to is required.
	r = run(t, e, nil, "sessions", "reparent", "ses_synth-a", "--to", "none")
	expectCode(t, r, 0)
	if !strings.Contains(r.stdout, "top level") {
		t.Fatalf("text output: %s", r)
	}
	if s, _ := h.b.GetSession(ownerCtxFor(), c.GetRequest{ID: "ses_synth-a"}); s.Session.Parent() != "" {
		t.Fatalf("not detached: %+v", s.Session.Lineage)
	}
	expectCode(t, run(t, e, nil, "sessions", "reparent", "ses_synth-a"), ExitUsage)
}

// reparent_session is pre-approved; bulk moves go to one parent; parent ""
// is the top level.
func TestMCPReparentSession(t *testing.T) {
	agent := c.Principal{Kind: c.PrincipalSession, ID: "ses_synth-a"}
	b := mock.New(mock.Options{Clock: mock.NewOffsetClock(mock.FixtureEpoch), DefaultPrincipal: &agent})
	cl := startMCP(t, &Ops{API: b, Self: Identity{SessionID: "ses_synth-a"}, PollInterval: 10 * time.Millisecond})
	cl.call("initialize", map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "test", "version": "0"}})
	out, isErr := cl.tool("reparent_session", map[string]any{"sessions": []string{"ses_synth-b", "ses_synth-standalone"}, "parent": "self"})
	var res ReparentResult
	if isErr || json.Unmarshal(out, &res) != nil || len(res.Sessions) != 2 || res.Sessions[1].ParentID != "ses_synth-a" {
		t.Fatalf("bulk adopt: %v %s", isErr, out)
	}
	out, isErr = cl.tool("reparent_session", map[string]any{"session": "ses_synth-b", "parent": ""})
	res = ReparentResult{}
	if isErr || json.Unmarshal(out, &res) != nil || len(res.Sessions) != 1 || res.Sessions[0].ParentID != "" {
		t.Fatalf("to top level: %v %s", isErr, out)
	}
	if s, _ := b.GetSession(ownerCtxFor(), c.GetRequest{ID: "ses_synth-b"}); s.Session.Parent() != "" {
		t.Fatalf("not detached: %+v", s.Session.Lineage)
	}
}
