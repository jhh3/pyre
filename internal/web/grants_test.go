package web

import (
	"context"
	"net/http"
	"testing"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/mock"
)

// grantsHub is the mock plus a synthetic standing-grants store.
type grantsHub struct {
	*mock.Backend
	grants  []c.StandingGrant
	listed  []c.ListGrantsRequest
	revoked []c.RevokeGrantRequest
}

func (g *grantsHub) CreateGrant(context.Context, c.CreateGrantRequest) (c.StandingGrant, error) {
	return c.StandingGrant{}, c.Errorf(c.ErrForbidden, "synthetic")
}

func (g *grantsHub) ListGrants(_ context.Context, req c.ListGrantsRequest) (c.ListGrantsResponse, error) {
	g.listed = append(g.listed, req)
	return c.ListGrantsResponse{Grants: g.grants}, nil
}

func (g *grantsHub) RevokeGrant(_ context.Context, req c.RevokeGrantRequest) (c.StandingGrant, error) {
	g.revoked = append(g.revoked, req)
	out := g.grants[0]
	now := time.Now()
	out.RevokedAt, out.RevokeReason = &now, req.Reason
	return out, nil
}

// The project page lists the project's standing grants and revokes one.
func TestProjectGrantsListAndRevoke(t *testing.T) {
	e := newTestEnv(t, true)
	pid := testProject
	hub := &grantsHub{Backend: e.b, grants: []c.StandingGrant{{GrantID: "sgr_synth-1", Scope: c.GrantScopeProject, ProjectID: &pid, Action: "merge", Option: "a",
		AskID: "ask_synth-p", RecordedBy: owner, ExpiresAt: e.clk.Now().Add(6 * 24 * time.Hour), CreatedAt: e.clk.Now()}}}
	s, err := New(Options{API: hub, StateDir: t.TempDir(), Now: e.clk.Now, Debounce: 5 * time.Millisecond, Heartbeat: 40 * time.Millisecond, Logf: func(string, ...any) {}})
	if err != nil {
		t.Fatal(err)
	}
	e.srv, e.h = s, s.Handler()

	var d struct {
		Grants []c.StandingGrant `json:"grants"`
	}
	e.json("GET", "/api/projects/"+testProject, nil, &d)
	if len(d.Grants) != 1 || d.Grants[0].GrantID != "sgr_synth-1" || len(hub.listed) != 1 || hub.listed[0].ProjectID == nil || *hub.listed[0].ProjectID != testProject {
		t.Fatalf("grants: %+v (listed %+v)", d.Grants, hub.listed)
	}
	var r struct {
		Grant c.StandingGrant `json:"grant"`
	}
	e.json("POST", "/api/grants/sgr_synth-1/revoke", map[string]string{"reason": "synthetic: done", "operation_id": "op-synth-revoke"}, &r)
	if len(hub.revoked) != 1 || hub.revoked[0].GrantID != "sgr_synth-1" || hub.revoked[0].Reason != "synthetic: done" || r.Grant.RevokedAt == nil {
		t.Fatalf("revoke: %+v %+v", hub.revoked, r)
	}
	// CSRF applies like every POST.
	if w := e.req("POST", "/api/grants/sgr_synth-1/revoke", map[string]string{}, func(r *http.Request) { r.Header.Set("Origin", "https://evil.invalid") }); w.Code != http.StatusForbidden {
		t.Fatalf("cross-origin revoke: %d", w.Code)
	}

	// A hub without grants: the page shows none.
	plain := newTestEnv(t, false)
	var none struct {
		Grants []c.StandingGrant `json:"grants"`
	}
	plain.json("GET", "/api/projects/"+testProject, nil, &none)
	if none.Grants == nil || len(none.Grants) != 0 {
		t.Fatalf("no grants API: %+v", none)
	}
}
