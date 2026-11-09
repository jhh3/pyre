package cli

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/mock"
)

// updateSpy records sessions.update requests on the way to the mock.
type updateSpy struct {
	*mock.Backend
	mu   sync.Mutex
	reqs []c.UpdateSessionRequest
}

func (s *updateSpy) UpdateSession(ctx context.Context, req c.UpdateSessionRequest) (c.SessionView, error) {
	s.mu.Lock()
	s.reqs = append(s.reqs, req)
	s.mu.Unlock()
	return s.Backend.UpdateSession(ctx, req)
}

func (s *updateSpy) last() c.UpdateSessionRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reqs[len(s.reqs)-1]
}

// iss_…: archive_session passes include_children,
// and cancel: true cancels the pending archive without unarchiving; the
// description says both.
func TestMCPArchiveIncludeChildrenAndCancel(t *testing.T) {
	agent := c.Principal{Kind: c.PrincipalSession, ID: "ses_synth-b"}
	spy := &updateSpy{Backend: mock.New(mock.Options{Clock: mock.NewOffsetClock(mock.FixtureEpoch), DefaultPrincipal: &agent})}
	cl := startMCP(t, &Ops{API: spy, Self: Identity{SessionID: "ses_synth-b"}, PollInterval: 10 * time.Millisecond})
	cl.call("initialize", map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "test", "version": "0"}})

	if out, isErr := cl.tool("archive_session", map[string]any{"include_children": true}); isErr {
		t.Fatalf("archive: %s", out)
	}
	if req := spy.last(); !req.IncludeChildren || req.Archived == nil || !*req.Archived {
		t.Fatalf("include_children reaches the hub: %+v", req)
	}
	out, isErr := cl.tool("archive_session", map[string]any{"cancel": true})
	var res ArchiveResult
	if isErr || json.Unmarshal(out, &res) != nil {
		t.Fatalf("cancel: %v %s", isErr, out)
	}
	if req := spy.last(); !req.CancelArchive || req.Archived != nil || req.SessionID != "ses_synth-b" {
		t.Fatalf("cancel reaches the hub as cancel_archive on self: %+v", req)
	}
	if res.Archived || res.WhenDone || res.SessionID != "ses_synth-b" {
		t.Fatalf("cancel answer: %+v", res)
	}
	v, _ := spy.GetSession(context.Background(), c.GetRequest{ID: "ses_synth-b"})
	if v.ArchivePending {
		t.Fatal("no longer pending")
	}
	d := toolByName(t, "archive_session").Description
	if !strings.Contains(d, "include_children: true") || !strings.Contains(d, "cancel: true") {
		t.Fatalf("description: %s", d)
	}
}

// pyre sessions show/ls mark a pending archive; --cancel cancels it.
func TestSessionsArchivePendingShownAndCanceled(t *testing.T) {
	h := newHub(t)
	e := newEnv(t, h)
	expectCode(t, run(t, e, nil, "sessions", "archive", "--when-done", "ses_synth-b"), 0)
	r := run(t, e, nil, "sessions", "show", "ses_synth-b")
	expectCode(t, r, 0)
	if !strings.Contains(r.stdout, "archive:   pending: archives when its current turn ends") || !strings.Contains(r.stdout, "pyre sessions archive --cancel ses_synth-b") {
		t.Fatalf("show marks the pending archive: %s", r)
	}
	r = run(t, e, nil, "--json", "sessions", "show", "ses_synth-b")
	if v := mustJSON[c.SessionView](t, r); !v.ArchivePending {
		t.Fatalf("json archive_pending: %s", r)
	}
	r = run(t, e, nil, "sessions", "ls")
	if !strings.Contains(r.stdout, "archives when its turn ends") {
		t.Fatalf("ls marks it: %s", r)
	}
	r = run(t, e, nil, "sessions", "archive", "--cancel", "ses_synth-b")
	expectCode(t, r, 0)
	r = run(t, e, nil, "--json", "sessions", "show", "ses_synth-b")
	if v := mustJSON[c.SessionView](t, r); v.ArchivePending || v.Session.Archived {
		t.Fatalf("canceled, not archived: %s", r)
	}
	if r := run(t, e, nil, "sessions", "show", "ses_synth-b"); strings.Contains(r.stdout, "archive:") {
		t.Fatalf("no marker once canceled: %s", r)
	}
}
