package hub

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// avoidFleet is a fleet registry with two synthetic nodes.
type avoidFleet struct {
	contracts.FleetAPI
	nodes []contracts.Node
}

func (f avoidFleet) ListNodes(ctx context.Context, req contracts.ListNodesRequest) (contracts.Page[contracts.Node], error) {
	return contracts.Page[contracts.Node]{Items: append([]contracts.Node(nil), f.nodes...)}, nil
}

func (f avoidFleet) GetNode(ctx context.Context, req contracts.GetRequest) (contracts.Node, error) {
	for _, n := range f.nodes {
		if n.NodeID == req.ID {
			return n, nil
		}
	}
	return contracts.Node{}, contracts.Errorf(contracts.ErrNotFound, "no node %s", req.ID)
}

func avoidHarness(t *testing.T) *harness {
	t.Helper()
	x := newHarness(t)
	x.fleet = avoidFleet{nodes: []contracts.Node{{NodeID: nodeMac, Name: "synth-mac", Platform: "linux"}, {NodeID: nodeLin, Name: "synth-linux", Platform: "linux"}}}
	x.restart()
	for _, id := range []string{nodeMac, nodeLin} {
		x.ch.setConnected(id, true)
		if _, err := x.h.HandleHello(nodeCtx(id), id, helloFor(id, 0)); err != nil { // unlimited runs
			t.Fatal(err)
		}
	}
	return x
}

var avoidOps int

func (x *harness) avoid(p contracts.Principal, req contracts.AvoidNodeRequest) (contracts.AvoidNodeResponse, error) {
	avoidOps++
	req.OperationID = "op_synth-avoid-" + strconv.Itoa(avoidOps)
	return x.h.AvoidNode(as(p), req)
}

func (x *harness) in(d time.Duration) *time.Time {
	t := x.clock.Now().Add(d)
	return &t
}

func (x *harness) nodeAvoids(id string) []contracts.NodeAvoid {
	x.t.Helper()
	page, err := x.h.ListNodes(as(owner), contracts.ListNodesRequest{})
	if err != nil {
		x.t.Fatal(err)
	}
	for _, n := range page.Items {
		if n.NodeID == id {
			n2, err := x.h.GetNode(as(owner), contracts.GetRequest{ID: id})
			if err != nil || len(n2.Avoid) != len(n.Avoid) {
				x.t.Fatalf("nodes.get avoid %+v (err %v) differs from nodes.list %+v", n2.Avoid, err, n.Avoid)
			}
			return n.Avoid
		}
	}
	x.t.Fatalf("node %s not listed", id)
	return nil
}

func (x *harness) startOn(p contracts.Principal, nodeID string, mod func(*contracts.StartSessionRequest)) (contracts.StartSessionResponse, error) {
	avoidOps++
	req := contracts.StartSessionRequest{Mutation: contracts.Mutation{OperationID: "op_synth-avoid-start-" + strconv.Itoa(avoidOps)}, NodeID: nodeID,
		Harness: contracts.HarnessClaude, Cwd: "/home/synth/work"}
	if mod != nil {
		mod(&req)
	}
	return x.h.StartSession(as(p), req)
}

// A flag is listed on the node (nodes.list and nodes.get), survives a hub
// restart, and lifts by itself at its expiry (injected clock).
func TestNodeAvoidListedSurvivesRestartAndExpires(t *testing.T) {
	x := avoidHarness(t)
	res, err := x.avoid(owner, contracts.AvoidNodeRequest{NodeID: nodeMac, Reason: "synthetic: presenting", ExpiresAt: x.in(2 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if res.NodeName != "synth-mac" || len(res.Avoid) != 1 || res.Avoid[0].Hard || !res.Avoid[0].OwnerSet || res.Avoid[0].SetBy.ID != owner.ID {
		t.Fatalf("set: %+v", res)
	}
	if a := x.nodeAvoids(nodeMac); len(a) != 1 || a[0].Reason != "synthetic: presenting" || !a[0].ExpiresAt.Equal(*x.in(2 * time.Hour)) {
		t.Fatalf("listed: %+v", a)
	}
	if a := x.nodeAvoids(nodeLin); len(a) != 0 {
		t.Fatalf("other node listed a flag: %+v", a)
	}
	x.restart()
	if a := x.nodeAvoids(nodeMac); len(a) != 1 {
		t.Fatalf("after restart: %+v", a)
	}
	x.clock.Add(2*time.Hour - time.Second)
	if a := x.nodeAvoids(nodeMac); len(a) != 1 {
		t.Fatalf("a second before expiry: %+v", a)
	}
	x.clock.Add(time.Second)
	if a := x.nodeAvoids(nodeMac); len(a) != 0 {
		t.Fatalf("expired flag still listed: %+v", a)
	}
	if text := x.h.avoidContext(context.Background(), nil); text != "" {
		t.Fatalf("expired flag in context: %q", text)
	}
	// The change was audited.
	var n int
	x.db.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action=? AND node_id=?`, contracts.AuditNodeAvoided, nodeMac).Scan(&n)
	if n != 1 {
		t.Fatalf("audit rows %d", n)
	}
}

// Setting needs an expiry within 7 days on an enrolled node, from the owner
// or a session.
func TestNodeAvoidValidation(t *testing.T) {
	x := avoidHarness(t)
	cases := []struct {
		name string
		p    contracts.Principal
		req  contracts.AvoidNodeRequest
		code contracts.ErrorCode
	}{
		{"unbounded", owner, contracts.AvoidNodeRequest{NodeID: nodeMac}, contracts.ErrInvalid},
		{"past", owner, contracts.AvoidNodeRequest{NodeID: nodeMac, ExpiresAt: x.in(-time.Minute)}, contracts.ErrInvalid},
		{"too long", owner, contracts.AvoidNodeRequest{NodeID: nodeMac, ExpiresAt: x.in(contracts.NodeAvoidMaxTTL + time.Hour)}, contracts.ErrInvalid},
		{"long reason", owner, contracts.AvoidNodeRequest{NodeID: nodeMac, ExpiresAt: x.in(time.Hour), Reason: strings.Repeat("x", contracts.NodeAvoidMaxReason+1)}, contracts.ErrInvalid},
		{"unknown node", owner, contracts.AvoidNodeRequest{NodeID: "node_synth-nope", ExpiresAt: x.in(time.Hour)}, contracts.ErrNotFound},
		{"node principal", contracts.Principal{Kind: contracts.PrincipalNode, ID: nodeLin}, contracts.AvoidNodeRequest{NodeID: nodeMac, ExpiresAt: x.in(time.Hour)}, contracts.ErrForbidden},
	}
	for _, tc := range cases {
		if _, err := x.avoid(tc.p, tc.req); !contracts.IsCode(err, tc.code) {
			t.Errorf("%s: want %s, got %v", tc.name, tc.code, err)
		}
	}
	if _, err := x.avoid(owner, contracts.AvoidNodeRequest{NodeID: nodeMac, ExpiresAt: x.in(contracts.NodeAvoidMaxTTL)}); err != nil {
		t.Fatalf("7 days: %v", err)
	}
}

// A hard flag refuses sessions.start on the node; the override works for
// the owner always and for a session only over a flag the owner did not
// set. Soft flags, other nodes, other projects, monitors and headless
// records are never refused.
func TestNodeAvoidHardRefusesUnlessOverridden(t *testing.T) {
	x := avoidHarness(t)
	sid := x.start(owner, "op_synth-avoid-sess", nodeLin).Session.SessionID
	sess := sesPrincipal(sid)
	ignore := func(r *contracts.StartSessionRequest) { r.IgnoreAvoid = true }

	if _, err := x.avoid(owner, contracts.AvoidNodeRequest{NodeID: nodeMac, ExpiresAt: x.in(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := x.startOn(owner, nodeMac, nil); err != nil {
		t.Fatalf("soft flag refused a named start: %v", err)
	}

	if _, err := x.avoid(owner, contracts.AvoidNodeRequest{NodeID: nodeMac, Hard: true, Reason: "synthetic benchmark", ExpiresAt: x.in(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	_, err := x.startOn(owner, nodeMac, nil)
	wantCode(t, err, contracts.ErrForbidden)
	if !strings.Contains(err.Error(), "avoided (hard)") || !strings.Contains(err.Error(), "synthetic benchmark") || !strings.Contains(err.Error(), "--ignore-avoid") {
		t.Fatalf("refusal: %v", err)
	}
	if _, err := x.startOn(owner, nodeLin, nil); err != nil {
		t.Fatalf("other node refused: %v", err)
	}
	if _, err := x.startOn(owner, nodeMac, ignore); err != nil {
		t.Fatalf("owner override: %v", err)
	}
	_, err = x.startOn(sess, nodeMac, ignore)
	wantCode(t, err, contracts.ErrForbidden)
	if !strings.Contains(err.Error(), "only the owner") {
		t.Fatalf("session override of the owner's flag: %v", err)
	}
	// Monitors and headless records place no agent work.
	if _, err := x.startOn(sess, nodeMac, func(r *contracts.StartSessionRequest) {
		r.Harness, r.Tags = contracts.HarnessShell, []string{contracts.TagMonitor}
	}); err != nil {
		t.Fatalf("monitor refused: %v", err)
	}

	// A session's hard flag yields to any caller's override.
	if _, err := x.avoid(owner, contracts.AvoidNodeRequest{NodeID: nodeMac, Clear: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := x.avoid(sess, contracts.AvoidNodeRequest{NodeID: nodeMac, Hard: true, ExpiresAt: x.in(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	_, err = x.startOn(sess, nodeMac, nil)
	wantCode(t, err, contracts.ErrForbidden)
	if _, err := x.startOn(sess, nodeMac, ignore); err != nil {
		t.Fatalf("session override of a session's flag: %v", err)
	}

	// A project flag covers only that project's launches.
	if _, err := x.avoid(owner, contracts.AvoidNodeRequest{NodeID: nodeMac, Clear: true}); err != nil {
		t.Fatal(err)
	}
	pid := "prj_synth-avoid"
	if _, err := x.avoid(owner, contracts.AvoidNodeRequest{NodeID: nodeMac, Hard: true, ProjectID: &pid, ExpiresAt: x.in(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	_, err = x.startOn(owner, nodeMac, func(r *contracts.StartSessionRequest) { r.ProjectID = &pid })
	wantCode(t, err, contracts.ErrForbidden)
	if _, err := x.startOn(owner, nodeMac, nil); err != nil {
		t.Fatalf("project flag refused a launch outside the project: %v", err)
	}
	// Expired: lifted.
	x.clock.Add(time.Hour)
	if _, err := x.startOn(owner, nodeMac, func(r *contracts.StartSessionRequest) { r.ProjectID = &pid }); err != nil {
		t.Fatalf("expired flag refused: %v", err)
	}
}

// The owner's flag cannot be cleared or weakened by a session: a softer or
// shorter request keeps it (with a note), a harder or longer one tightens
// it and keeps the owner's attribution. The owner replaces and clears it.
func TestNodeAvoidOwnerPrecedence(t *testing.T) {
	x := avoidHarness(t)
	sa := sesPrincipal(x.start(owner, "op_synth-avoid-a", nodeLin).Session.SessionID)
	sb := sesPrincipal(x.start(owner, "op_synth-avoid-b", nodeLin).Session.SessionID)
	t0 := x.clock.Now()

	if _, err := x.avoid(owner, contracts.AvoidNodeRequest{NodeID: nodeMac, Reason: "synthetic: owner presenting", ExpiresAt: x.in(3 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	_, err := x.avoid(sa, contracts.AvoidNodeRequest{NodeID: nodeMac, Clear: true})
	wantCode(t, err, contracts.ErrForbidden)

	res, err := x.avoid(sa, contracts.AvoidNodeRequest{NodeID: nodeMac, Reason: "synthetic: session", ExpiresAt: x.in(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	a := res.Avoid[0]
	if !strings.Contains(res.Note, "kept") || a.Hard || !a.ExpiresAt.Equal(t0.Add(3*time.Hour)) || !a.OwnerSet || a.SetBy.ID != owner.ID || a.Reason != "synthetic: owner presenting" {
		t.Fatalf("weaker session request changed the owner's flag: %+v %q", a, res.Note)
	}

	res, err = x.avoid(sa, contracts.AvoidNodeRequest{NodeID: nodeMac, Hard: true, ExpiresAt: x.in(5 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	a = res.Avoid[0]
	if res.Note != "" || !a.Hard || !a.ExpiresAt.Equal(t0.Add(5*time.Hour)) || !a.OwnerSet || a.SetBy.ID != owner.ID || a.UpdatedBy == nil || a.UpdatedBy.ID != sa.ID {
		t.Fatalf("tightened: %+v %q", a, res.Note)
	}
	// Still the owner's: sessions cannot clear or override it.
	_, err = x.avoid(sb, contracts.AvoidNodeRequest{NodeID: nodeMac, Clear: true})
	wantCode(t, err, contracts.ErrForbidden)
	_, err = x.startOn(sb, nodeMac, func(r *contracts.StartSessionRequest) { r.IgnoreAvoid = true })
	wantCode(t, err, contracts.ErrForbidden)

	// The owner replaces it outright, then clears it.
	res, err = x.avoid(owner, contracts.AvoidNodeRequest{NodeID: nodeMac, ExpiresAt: x.in(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if a = res.Avoid[0]; a.Hard || !a.ExpiresAt.Equal(t0.Add(time.Hour)) || a.UpdatedBy != nil || a.Reason != "" {
		t.Fatalf("owner replace: %+v", a)
	}
	if res, err = x.avoid(owner, contracts.AvoidNodeRequest{NodeID: nodeMac, Clear: true}); err != nil || res.Cleared != 1 || len(res.Avoid) != 0 {
		t.Fatalf("owner clear: %+v %v", res, err)
	}
	if res, err = x.avoid(owner, contracts.AvoidNodeRequest{NodeID: nodeMac, Clear: true}); err != nil || res.Cleared != 0 {
		t.Fatalf("second clear: %+v %v", res, err)
	}

	// A session's flag: any session (or the owner) replaces or clears it.
	if _, err := x.avoid(sa, contracts.AvoidNodeRequest{NodeID: nodeMac, Hard: true, ExpiresAt: x.in(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if res, err = x.avoid(sb, contracts.AvoidNodeRequest{NodeID: nodeMac, Clear: true}); err != nil || res.Cleared != 1 {
		t.Fatalf("session clears a session's flag: %+v %v", res, err)
	}
}

// The hub's NodeSelector placement puts soft-avoided nodes last and skips
// hard-avoided ones unless overridden.
func TestNodeAvoidSelector(t *testing.T) {
	x := avoidHarness(t)
	sel := &contracts.NodeSelector{LeastLoaded: true}
	pick := func(override bool) (string, error) {
		res, err := x.startOn(owner, "", func(r *contracts.StartSessionRequest) { r.NodeSelector, r.IgnoreAvoid = sel, override })
		if err != nil {
			return "", err
		}
		return res.Session.Run.NodeID, nil
	}
	if _, err := x.avoid(owner, contracts.AvoidNodeRequest{NodeID: nodeLin, ExpiresAt: x.in(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	// Make the avoided node the least loaded: it still goes last.
	x.start(owner, "op_synth-avoid-load", nodeMac)
	if got, err := pick(false); err != nil || got != nodeMac {
		t.Fatalf("soft: placed on %s (%v), want %s", got, err, nodeMac)
	}
	if _, err := x.avoid(owner, contracts.AvoidNodeRequest{NodeID: nodeMac, Hard: true, ExpiresAt: x.in(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if got, err := pick(false); err != nil || got != nodeLin {
		t.Fatalf("only the soft-avoided node fits: placed on %s (%v)", got, err)
	}
	if _, err := x.avoid(owner, contracts.AvoidNodeRequest{NodeID: nodeLin, Hard: true, ExpiresAt: x.in(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := pick(false); !contracts.IsCode(err, contracts.ErrForbidden) {
		t.Fatalf("every node hard-avoided: %v", err)
	}
	if _, err := pick(true); err != nil {
		t.Fatalf("owner override: %v", err)
	}
}

// Launch context and get_context's text name the avoided machines a
// session should keep new work off, by name, with the expiry and reason.
func TestNodeAvoidContextText(t *testing.T) {
	x := avoidHarness(t)
	if text := x.h.avoidContext(context.Background(), nil); text != "" {
		t.Fatalf("no flags: %q", text)
	}
	if _, err := x.avoid(owner, contracts.AvoidNodeRequest{NodeID: nodeMac, Reason: "synthetic: presenting", ExpiresAt: x.in(2 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	pid := "prj_synth-other"
	if _, err := x.avoid(owner, contracts.AvoidNodeRequest{NodeID: nodeLin, Hard: true, ProjectID: &pid, ExpiresAt: x.in(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	text := x.h.avoidContext(context.Background(), nil)
	for _, want := range []string{"Machines to keep new work off", "synth-mac: soft, until", "synthetic: presenting", "set by the owner", "keep running"} {
		if !strings.Contains(text, want) {
			t.Errorf("context lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "synth-linux") {
		t.Errorf("another project's flag listed:\n%s", text)
	}
	if text := x.h.avoidContext(context.Background(), &pid); !strings.Contains(text, "synth-linux: hard") {
		t.Errorf("project flag missing for its project:\n%s", text)
	}
	if text, _ := x.h.standingContext(context.Background(), nil, ""); !strings.Contains(text, "synth-mac: soft") {
		t.Errorf("launch context lacks the avoided machine:\n%s", text)
	}
}
