package connections

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhh3/pyre/integrations/outfit"
	"github.com/jhh3/pyre/integrations/outfit/fakeoutfit"
	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/credentials"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

func TestMain(m *testing.M) {
	fakeoutfit.RunIfRequested()
	os.Exit(m.Run())
}

// Synthetic values whose disclosure the tests grep for.
const (
	outfitSecret = "SYNTHETIC-outfit-only-token-9d1e"
	pyreSecret   = "SYNTHETIC-pyre-provisioned-6b2c"
)

const (
	nodeMac    = "node_mac"
	nodeServer = "node_server"
	connGH     = "conn_github-work"
	sesA       = "ses_alpha"
	sesB       = "ses_bravo"
	sesC       = "ses_charlie"
	prjOne     = "prj_one"
	prjTwo     = "prj_two"
)

var owner = contracts.Principal{Kind: contracts.PrincipalOwner, ID: "cli_owner"}

type memEvents struct {
	mu     sync.Mutex
	events []contracts.Event
}

func (m *memEvents) AppendTx(_ context.Context, _ *sql.Tx, ev contracts.Event) (contracts.Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ev.Cursor = int64(len(m.events) + 1)
	m.events = append(m.events, ev)
	return ev, nil
}

type harness struct {
	t           *testing.T
	db          *sql.DB
	cat         *Catalog
	events      *memEvents
	sessions    map[string]SessionContext
	logs        *bytes.Buffer
	logger      *slog.Logger
	stateFile   string
	callLog     string
	state       fakeoutfit.State
	mac, server *NodeService
	macStore    *credentials.MemoryStore
	serverStore *credentials.MemoryStore
	wire        []string // every payload that would cross the hub
	op          int
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	db, err := sqlitex.Open(filepath.Join(dir, "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	h := &harness{t: t, db: db, events: &memEvents{}, logs: &bytes.Buffer{}, stateFile: filepath.Join(dir, "outfit-state.json"), callLog: filepath.Join(dir, "outfit-calls.log")}
	h.logger = slog.New(slog.NewJSONHandler(h.logs, nil))
	h.cat, err = Open(context.Background(), db, Options{Events: h.events})
	if err != nil {
		t.Fatal(err)
	}
	h.sessions = map[string]SessionContext{
		sesA: {NodeID: nodeServer},
		sesB: {NodeID: nodeServer, ProjectID: prjOne},
		sesC: {NodeID: nodeMac, ProjectID: prjTwo},
	}
	verified := time.Now().Add(-time.Minute).UTC()
	h.state = fakeoutfit.State{Version: "0.10.7", Connections: []fakeoutfit.Connection{{ID: "con_gh", Name: "github-work", Connector: "github", Mode: "rest", Method: "managed-token", Custody: "outfit", Status: "active", Env: []string{"GH_TOKEN"}, Secret: outfitSecret, VerifiedAt: &verified,
		Operations: map[string]json.RawMessage{"get_issue": json.RawMessage(`{"title":"synthetic issue 7"}`)}}}}
	h.saveState()
	lookup := SessionLookupFunc(func(_ context.Context, id string) (SessionContext, error) {
		sc, ok := h.sessions[id]
		if !ok {
			return SessionContext{}, contracts.Errorf(contracts.ErrNotFound, "no session %s", id)
		}
		return sc, nil
	})
	checker := h.cat.Checker(lookup)
	adapter := outfit.New(outfit.Options{Binary: os.Args[0], Home: filepath.Join(dir, "outfit-home"), ExtraEnv: fakeoutfit.Env(h.stateFile, h.callLog)})
	h.macStore, h.serverStore = credentials.NewMemoryStore(), credentials.NewMemoryStore()
	h.mac, err = NewNodeService(NodeConfig{NodeID: nodeMac, Outfit: adapter, Stores: credentials.Stores{}.Add(h.macStore), DefaultBackend: credentials.BackendMemory, Grants: checker, Logger: h.logger})
	if err != nil {
		t.Fatal(err)
	}
	h.server, err = NewNodeService(NodeConfig{NodeID: nodeServer, Stores: credentials.Stores{}.Add(h.serverStore), Index: credentials.OpenIndex(filepath.Join(dir, "server", "provisions.json")), DefaultBackend: credentials.BackendMemory, Grants: checker, Logger: h.logger})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *harness) saveState() {
	if err := fakeoutfit.Save(h.stateFile, h.state); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) mut() contracts.Mutation {
	h.op++
	return contracts.Mutation{OperationID: fmt.Sprintf("op_test-%d", h.op)}
}

// call runs a node handler the way the daemon would: JSON params in, result
// marshaled out. Both directions are recorded as hub-crossing payloads.
func (h *harness) call(n *NodeService, action string, params any) (json.RawMessage, error) {
	h.t.Helper()
	raw, _ := json.Marshal(params)
	h.wire = append(h.wire, string(raw))
	hs := n.Handlers()
	for k, v := range n.ProvisioningHandlers() {
		hs[k] = v
	}
	res, err := hs[action](context.Background(), raw)
	if err != nil {
		eb, _ := json.Marshal(contracts.AsError(err))
		h.wire = append(h.wire, string(eb))
		return nil, err
	}
	out, merr := json.Marshal(res)
	if merr != nil {
		h.t.Fatalf("%s result does not marshal: %v", action, merr)
	}
	h.wire = append(h.wire, string(out))
	return out, nil
}

func (h *harness) report(n *NodeService, nodeID string) {
	h.t.Helper()
	conns, err := n.Report(context.Background())
	if err != nil {
		h.t.Fatal(err)
	}
	raw, _ := json.Marshal(conns)
	h.wire = append(h.wire, string(raw))
	if err := h.cat.ApplyReport(context.Background(), nodeID, conns); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) grant(p contracts.ConnectionGrantParams) contracts.Connection {
	h.t.Helper()
	c, err := h.cat.Grant(context.Background(), owner, GrantRequest{Mutation: h.mut(), ConnectionGrantParams: p})
	if err != nil {
		h.t.Fatal(err)
	}
	return c
}

func (h *harness) revoke(p contracts.ConnectionGrantParams) {
	h.t.Helper()
	if _, err := h.cat.Revoke(context.Background(), owner, GrantRequest{Mutation: h.mut(), ConnectionGrantParams: p}); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) allowed(conn, ses string) bool {
	sc := h.sessions[ses]
	sc.SessionID = ses
	d, err := h.cat.CheckGrant(context.Background(), conn, sc)
	if err != nil {
		h.t.Fatal(err)
	}
	return d.Allowed
}

func TestCatalogGrantsGenerationsAndIdempotency(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.report(h.mac, nodeMac)
	conn, err := h.cat.Get(ctx, connGH)
	if err != nil {
		t.Fatal(err)
	}
	if conn.Service != "github" || len(conn.Availability) != 1 || conn.Availability[0].NodeID != nodeMac || conn.Availability[0].State != "verified" {
		t.Fatalf("catalog after report = %+v", conn)
	}
	gen0, _ := h.cat.Generation(ctx, connGH)
	req := GrantRequest{Mutation: contracts.Mutation{OperationID: "op_grant-a"}, ConnectionGrantParams: contracts.ConnectionGrantParams{ConnectionID: connGH, SessionID: sesA}}
	first, err := h.cat.Grant(ctx, owner, req)
	if err != nil || len(first.Scope.Sessions) != 1 {
		t.Fatalf("grant = %+v %v", first, err)
	}
	replay, err := h.cat.Grant(ctx, owner, req)
	if err != nil || len(replay.Scope.Sessions) != 1 {
		t.Fatalf("replay = %+v %v", replay, err)
	}
	gs, _ := h.cat.Grants(ctx, connGH, true)
	if len(gs) != 1 || gs[0].GrantedGeneration != gen0+1 {
		t.Fatalf("grants after replay = %+v", gs)
	}
	req.SessionID = sesB
	if _, err := h.cat.Grant(ctx, owner, req); !contracts.IsCode(err, contracts.ErrConflict) {
		t.Fatalf("reused operation id with different body: %v", err)
	}
	sessionPrincipal := contracts.Principal{Kind: contracts.PrincipalSession, ID: sesA}
	if _, err := h.cat.Grant(ctx, sessionPrincipal, GrantRequest{Mutation: h.mut(), ConnectionGrantParams: contracts.ConnectionGrantParams{ConnectionID: connGH, SessionID: sesA}}); !contracts.IsCode(err, contracts.ErrForbidden) {
		t.Fatalf("a session must not grant itself: %v", err)
	}
	if !h.allowed(connGH, sesA) || h.allowed(connGH, sesB) {
		t.Fatal("session grant must allow exactly that session")
	}
	h.revoke(contracts.ConnectionGrantParams{ConnectionID: connGH, SessionID: sesA})
	if h.allowed(connGH, sesA) {
		t.Fatal("revoked grant still allows")
	}
	gs, _ = h.cat.Grants(ctx, connGH, true)
	if len(gs) != 1 || gs[0].RevokedAt == nil || gs[0].RevokedGeneration <= gs[0].GrantedGeneration {
		t.Fatalf("revocation generation not recorded: %+v", gs)
	}
	nf := GrantRequest{Mutation: contracts.Mutation{OperationID: "op_revoke-none"}, ConnectionGrantParams: contracts.ConnectionGrantParams{ConnectionID: connGH, SessionID: sesC}}
	for i := 0; i < 2; i++ {
		if _, err := h.cat.Revoke(ctx, owner, nf); !contracts.IsCode(err, contracts.ErrNotFound) {
			t.Fatalf("revoke of absent grant (attempt %d): %v", i, err)
		}
	}
	if len(h.events.events) < 2 || h.events.events[0].Kind != EvConnectionChanged {
		t.Fatalf("events = %+v", h.events.events)
	}
	if _, err := h.cat.Grant(ctx, owner, GrantRequest{Mutation: h.mut(), ConnectionGrantParams: contracts.ConnectionGrantParams{ConnectionID: "conn_nope", SessionID: sesA}}); !contracts.IsCode(err, contracts.ErrNotFound) {
		t.Fatalf("grant on unknown connection: %v", err)
	}
}

func TestScopeEnforcement(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.report(h.mac, nodeMac)
	if h.allowed(connGH, sesA) || h.allowed(connGH, sesB) || h.allowed(connGH, sesC) {
		t.Fatal("no grants must mean no access")
	}
	h.grant(contracts.ConnectionGrantParams{ConnectionID: connGH, ProjectID: prjOne})
	if !h.allowed(connGH, sesB) || h.allowed(connGH, sesC) || h.allowed(connGH, sesA) {
		t.Fatal("project grant must allow only that project's sessions")
	}
	h.grant(contracts.ConnectionGrantParams{ConnectionID: connGH, NodeID: nodeMac})
	if !h.allowed(connGH, sesC) || h.allowed(connGH, sesA) {
		t.Fatal("node grant must allow only sessions on that node")
	}
	h.grant(contracts.ConnectionGrantParams{ConnectionID: connGH, NodeID: nodeServer, ProjectID: prjTwo})
	if h.allowed(connGH, sesA) {
		t.Fatal("combined node+project grant must require both")
	}
	c := h.grant(contracts.ConnectionGrantParams{ConnectionID: connGH})
	if !h.allowed(connGH, sesA) || len(c.Scope.Nodes)+len(c.Scope.Projects)+len(c.Scope.Sessions) != 0 {
		t.Fatalf("broad grant must allow all and render owner-broad scope: %+v", c.Scope)
	}
	h.revoke(contracts.ConnectionGrantParams{ConnectionID: connGH})
	if h.allowed(connGH, sesA) {
		t.Fatal("revoking the broad grant must not leave broad access")
	}
	got, _ := h.cat.Get(ctx, connGH)
	if strings.Join(got.Scope.Projects, ",") != "prj_one,prj_two" || strings.Join(got.Scope.Nodes, ",") != "node_mac,node_server" {
		t.Fatalf("scope projection = %+v", got.Scope)
	}
	sc := h.sessions[sesB]
	sc.SessionID, sc.Archived = sesB, true
	if d, _ := h.cat.CheckGrant(ctx, connGH, sc); d.Allowed {
		t.Fatal("archived sessions must be denied")
	}
	page, err := h.cat.List(ctx, contracts.ListConnectionsRequest{ProjectID: prjOne})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("project filter = %+v %v", page, err)
	}
	page, _ = h.cat.List(ctx, contracts.ListConnectionsRequest{ProjectID: "prj_other"})
	if len(page.Items) != 1 { // node_mac grant applies to every project on that node
		t.Fatalf("node-only grants apply to every project: %+v", page)
	}
	page, _ = h.cat.List(ctx, contracts.ListConnectionsRequest{NodeID: nodeServer})
	if len(page.Items) != 0 {
		t.Fatalf("node filter = %+v", page)
	}
}

func TestRemoteInvokeRechecksGrantAtUseTime(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.report(h.mac, nodeMac)
	h.grant(contracts.ConnectionGrantParams{ConnectionID: connGH, SessionID: sesA})
	sc := h.sessions[sesA]
	sc.SessionID = sesA
	route, err := h.cat.RouteInvoke(ctx, connGH, sc)
	if err != nil || route.NodeID != nodeMac {
		t.Fatalf("route = %+v %v", route, err)
	}
	params := contracts.ConnectionInvokeParams{ConnectionID: connGH, SessionID: sesA, Operation: "get_issue", Args: map[string]string{"number": "7"}}
	out, err := h.call(h.mac, contracts.ActionConnInvoke, params)
	if err != nil {
		t.Fatal(err)
	}
	var res contracts.ConnectionInvokeResult
	json.Unmarshal(out, &res)
	if res.ConnectionID != connGH || !strings.Contains(string(res.Output), "synthetic issue 7") {
		t.Fatalf("invoke result = %s", out)
	}
	// Revoke at the hub only: no message reaches the node, yet the very next
	// use is refused because the node re-checks the current grant.
	h.revoke(contracts.ConnectionGrantParams{ConnectionID: connGH, SessionID: sesA})
	if _, err := h.call(h.mac, contracts.ActionConnInvoke, params); !contracts.IsCode(err, contracts.ErrForbidden) {
		t.Fatalf("invoke after hub revoke = %v", err)
	}
	if _, err := h.cat.RouteInvoke(ctx, connGH, sc); !contracts.IsCode(err, contracts.ErrForbidden) {
		t.Fatalf("route after revoke = %v", err)
	}
	// Re-grant restores access.
	h.grant(contracts.ConnectionGrantParams{ConnectionID: connGH, SessionID: sesA})
	if _, err := h.call(h.mac, contracts.ActionConnInvoke, params); err != nil {
		t.Fatalf("invoke after re-grant = %v", err)
	}
	// Sessions without a grant, and unknown sessions, are refused.
	if _, err := h.call(h.mac, contracts.ActionConnInvoke, contracts.ConnectionInvokeParams{ConnectionID: connGH, SessionID: sesC, Operation: "get_issue"}); !contracts.IsCode(err, contracts.ErrForbidden) {
		t.Fatalf("ungranted session = %v", err)
	}
	if _, err := h.call(h.mac, contracts.ActionConnInvoke, contracts.ConnectionInvokeParams{ConnectionID: connGH, SessionID: "ses_ghost", Operation: "get_issue"}); !contracts.IsCode(err, contracts.ErrUnavailable) {
		t.Fatalf("unknown session must fail closed: %v", err)
	}
}

func TestNodeRevocationDeniesEvenWithStaleChecker(t *testing.T) {
	h := newHarness(t)
	stale := GrantCheckerFunc(func(_ context.Context, c, s string) (GrantDecision, error) {
		return GrantDecision{ConnectionID: c, SessionID: s, Allowed: true, Reason: "stale cache"}, nil
	})
	n, err := NewNodeService(NodeConfig{NodeID: nodeMac, Outfit: h.mac.cfg.Outfit, Grants: stale})
	if err != nil {
		t.Fatal(err)
	}
	params := contracts.ConnectionInvokeParams{ConnectionID: connGH, SessionID: sesA, Operation: "get_issue"}
	if _, err := h.call(n, contracts.ActionConnInvoke, params); err != nil {
		t.Fatal(err)
	}
	if _, err := h.call(n, contracts.ActionConnRevoke, contracts.ConnectionGrantParams{ConnectionID: connGH, SessionID: sesA}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.call(n, contracts.ActionConnInvoke, params); !contracts.IsCode(err, contracts.ErrForbidden) {
		t.Fatalf("node-pushed revocation must deny immediately: %v", err)
	}
	if _, err := h.call(n, contracts.ActionConnGrant, contracts.ConnectionGrantParams{ConnectionID: connGH, SessionID: sesA}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.call(n, contracts.ActionConnInvoke, params); err != nil {
		t.Fatalf("re-grant must restore: %v", err)
	}
	down := GrantCheckerFunc(func(context.Context, string, string) (GrantDecision, error) {
		return GrantDecision{}, contracts.Errorf(contracts.ErrUnavailable, "hub unreachable")
	})
	n2, _ := NewNodeService(NodeConfig{NodeID: nodeMac, Outfit: h.mac.cfg.Outfit, Grants: down})
	if _, err := h.call(n2, contracts.ActionConnInvoke, params); !contracts.IsCode(err, contracts.ErrUnavailable) {
		t.Fatalf("unreachable hub must fail closed: %v", err)
	}
	if _, err := NewNodeService(NodeConfig{NodeID: nodeMac}); err == nil {
		t.Fatal("a node service without a grant checker must be rejected")
	}
}

func TestCheckReauthAndAvailabilityReporting(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.report(h.mac, nodeMac)
	h.grant(contracts.ConnectionGrantParams{ConnectionID: connGH, SessionID: sesA})
	sc := h.sessions[sesA]
	sc.SessionID = sesA
	check := func() contracts.ConnectionAvailability {
		out, err := h.call(h.mac, contracts.ActionConnCheck, contracts.ConnectionParams{ConnectionID: connGH})
		if err != nil {
			t.Fatal(err)
		}
		var av contracts.ConnectionAvailability
		json.Unmarshal(out, &av)
		if err := h.cat.RecordCheck(ctx, nodeMac, connGH, av); err != nil {
			t.Fatal(err)
		}
		return av
	}
	h.state.Connections[0].Expired = true
	h.state.Leak = true // adversarial fake echoes the token in its failure text
	h.saveState()
	if av := check(); av.State != "expired" || strings.Contains(av.Detail, outfitSecret) {
		t.Fatalf("expired check = %+v", av)
	}
	if _, err := h.cat.RouteInvoke(ctx, connGH, sc); !contracts.IsCode(err, contracts.ErrUnavailable) {
		t.Fatalf("expired connection must not route: %v", err)
	}
	// The owner reauthenticates in Outfit on node_mac; the next check verifies.
	h.state.Connections[0].Expired = false
	h.saveState()
	if av := check(); av.State != "verified" {
		t.Fatalf("after reauth = %+v", av)
	}
	if r, err := h.cat.RouteInvoke(ctx, connGH, sc); err != nil || r.NodeID != nodeMac {
		t.Fatalf("route after reauth = %+v %v", r, err)
	}
	if err := h.cat.RecordCheck(ctx, nodeServer, connGH, contracts.ConnectionAvailability{NodeID: nodeMac, State: "verified", Mode: "remote_invoke"}); !contracts.IsCode(err, contracts.ErrForbidden) {
		t.Fatalf("a node must not report for another node: %v", err)
	}
	// A node that stops reporting a connection marks it missing there.
	h.state.Connections = nil
	h.saveState()
	h.report(h.mac, nodeMac)
	got, _ := h.cat.Get(ctx, connGH)
	if got.Availability[0].State != "missing" {
		t.Fatalf("omitted connection should be missing: %+v", got.Availability)
	}
}

// With an Outfit that cannot say when it last verified a connection (no
// `inventory`), reports carry "unknown"; a successful connection.check is
// remembered, pushed (OnAvailabilityChange) and overlaid on later reports so
// the hub catalog shows it verified. A newer, worse Outfit status still wins.
func TestCheckResultOverlaysReportedAvailability(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.state.Connections[0].VerifiedAt = nil
	h.saveState()
	notified := 0
	h.mac.cfg.OnAvailabilityChange = func() { notified++ }
	h.report(h.mac, nodeMac)
	if got, _ := h.cat.Get(ctx, connGH); got.Availability[0].State != "unknown" {
		t.Fatalf("before check: %+v", got.Availability)
	}
	if _, err := h.call(h.mac, contracts.ActionConnCheck, contracts.ConnectionParams{ConnectionID: connGH}); err != nil {
		t.Fatal(err)
	}
	if notified != 1 {
		t.Fatalf("OnAvailabilityChange called %d times", notified)
	}
	h.report(h.mac, nodeMac)
	got, _ := h.cat.Get(ctx, connGH)
	if a := got.Availability[0]; a.State != "verified" || a.NodeID != nodeMac || a.Detail != "verified by connection.check" {
		t.Fatalf("after check: %+v", got.Availability)
	}
	sc := h.sessions[sesA]
	sc.SessionID = sesA
	h.grant(contracts.ConnectionGrantParams{ConnectionID: connGH, SessionID: sesA})
	if r, err := h.cat.RouteInvoke(ctx, connGH, sc); err != nil || r.NodeID != nodeMac {
		t.Fatalf("route after check = %+v %v", r, err)
	}
	time.Sleep(10 * time.Millisecond)
	h.state.Connections[0].Status = "needs_reauth"
	h.saveState()
	h.report(h.mac, nodeMac)
	if got, _ := h.cat.Get(ctx, connGH); got.Availability[0].State != "expired" {
		t.Fatalf("newer outfit status must win over an older check: %+v", got.Availability)
	}
}

// provision runs the explicit owner-initiated cross-node flow: offer on the
// recipient, seal on the source, accept on the recipient. Only JSON payloads
// cross the "hub" (recorded in h.wire).
func (h *harness) provision(from, to *NodeService, sourceRef credentials.Ref) credentials.Provision {
	h.t.Helper()
	out, err := h.call(to, ActionConnProvisionOffer, ProvisionOfferParams{ConnectionID: connGH, EnvVar: "GH_TOKEN"})
	if err != nil {
		h.t.Fatal(err)
	}
	var offer credentials.ProvisionOffer
	json.Unmarshal(out, &offer)
	out, err = h.call(from, ActionConnProvisionSeal, ProvisionSealParams{Offer: offer, SourceRef: sourceRef.String()})
	if err != nil {
		h.t.Fatal(err)
	}
	var sealed credentials.SealedCredential
	json.Unmarshal(out, &sealed)
	out, err = h.call(to, ActionConnProvisionAccept, sealed)
	if err != nil {
		h.t.Fatal(err)
	}
	var prov credentials.Provision
	json.Unmarshal(out, &prov)
	return prov
}

func TestCrossNodeProvisioningLaunchRevokeAndLockedStore(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	src := credentials.NewRef(credentials.BackendMemory, connGH, "GH_TOKEN")
	if err := h.macStore.Put(ctx, src, credentials.NewSecret(pyreSecret)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.cat.Upsert(ctx, owner, ConnectionSpec{Mutation: h.mut(), Name: "github-work", Service: "github", Modes: []string{"local_credential", "remote_invoke"}}); err != nil {
		t.Fatal(err)
	}
	prov := h.provision(h.mac, h.server, src)
	if prov.SourceNode != nodeMac || prov.EnvVar != "GH_TOKEN" || h.serverStore.Len() != 1 {
		t.Fatalf("provision = %+v", prov)
	}
	h.report(h.server, nodeServer)
	out, err := h.call(h.server, contracts.ActionConnCheck, contracts.ConnectionParams{ConnectionID: connGH})
	if err != nil {
		t.Fatal(err)
	}
	var av contracts.ConnectionAvailability
	json.Unmarshal(out, &av)
	if av.State != "verified" || av.Mode != "local_credential" || av.NodeID != nodeServer {
		t.Fatalf("check on recipient = %+v", av)
	}
	h.cat.RecordCheck(ctx, nodeServer, connGH, av)

	plan, err := h.server.PrepareLaunch(ctx, sesB, []string{connGH})
	if err != nil || len(plan.Denied) != 1 || len(plan.Env) != 0 {
		t.Fatalf("launch without a grant must omit the connection: %+v %v", plan, err)
	}
	h.grant(contracts.ConnectionGrantParams{ConnectionID: connGH, ProjectID: prjOne})
	plan, err = h.server.PrepareLaunch(ctx, sesB, []string{connGH})
	if err != nil || plan.Env["GH_TOKEN"] != pyreSecret || strings.Join(plan.Manifest.EnvNames(), ",") != "GH_TOKEN" {
		t.Fatalf("granted launch = %+v %v", plan.Manifest, err)
	}
	pj, _ := json.Marshal(plan)
	h.wire = append(h.wire, string(pj))
	if strings.Contains(string(pj), pyreSecret) {
		t.Fatal("a serialized launch plan contains the value")
	}
	// Revocation applies to the next launch.
	h.revoke(contracts.ConnectionGrantParams{ConnectionID: connGH, ProjectID: prjOne})
	plan, _ = h.server.PrepareLaunch(ctx, sesB, []string{connGH})
	if len(plan.Env) != 0 || len(plan.Denied) != 1 {
		t.Fatalf("launch after revoke = %+v", plan)
	}
	// A locked store is reported and fails a granted launch with an actionable error.
	h.grant(contracts.ConnectionGrantParams{ConnectionID: connGH, ProjectID: prjOne})
	h.serverStore.SetLocked(true)
	out, err = h.call(h.server, contracts.ActionConnCheck, contracts.ConnectionParams{ConnectionID: connGH})
	if err != nil {
		t.Fatal(err)
	}
	json.Unmarshal(out, &av)
	if av.State != "locked_store" {
		t.Fatalf("locked check = %+v", av)
	}
	_, err = h.server.PrepareLaunch(ctx, sesB, []string{connGH})
	if !contracts.IsCode(err, contracts.ErrUnavailable) || !strings.Contains(err.Error(), "locked") || !strings.Contains(err.Error(), "GH_TOKEN") {
		t.Fatalf("locked launch = %v", err)
	}
	h.serverStore.SetLocked(false)
	// Deprovision removes the value and the index entry.
	if _, err := h.call(h.server, ActionConnDeprovision, contracts.ConnectionParams{ConnectionID: connGH}); err != nil {
		t.Fatal(err)
	}
	if h.serverStore.Len() != 0 {
		t.Fatal("deprovision left a value")
	}
	plan, err = h.server.PrepareLaunch(ctx, sesB, []string{connGH})
	if err != nil || len(plan.Remote) != 1 || len(plan.Env) != 0 {
		t.Fatalf("after deprovision the connection is remote-only: %+v %v", plan, err)
	}
	// Replayed sealed payloads are refused.
	offerOut, _ := h.call(h.server, ActionConnProvisionOffer, ProvisionOfferParams{ConnectionID: connGH, EnvVar: "GH_TOKEN"})
	var offer credentials.ProvisionOffer
	json.Unmarshal(offerOut, &offer)
	sealedOut, _ := h.call(h.mac, ActionConnProvisionSeal, ProvisionSealParams{Offer: offer, SourceRef: src.String()})
	if _, err := h.call(h.server, ActionConnProvisionAccept, json.RawMessage(sealedOut)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.call(h.server, ActionConnProvisionAccept, json.RawMessage(sealedOut)); err == nil {
		t.Fatal("sealed credential replay must fail")
	}
}

// TestNonDisclosure runs every flow and greps everything that leaves a
// credential store: handler params/results/errors (hub-crossing payloads),
// hub tables, events, logs, launch manifests, and Outfit argv.
func TestNonDisclosure(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	var errs []string
	src := credentials.NewRef(credentials.BackendMemory, connGH, "GH_TOKEN")
	h.macStore.Put(ctx, src, credentials.NewSecret(pyreSecret))
	h.cat.Upsert(ctx, owner, ConnectionSpec{Mutation: h.mut(), Name: "github-work", Service: "github", Modes: []string{"local_credential", "remote_invoke"}})
	h.report(h.mac, nodeMac)
	h.provision(h.mac, h.server, src)
	h.report(h.server, nodeServer)
	h.grant(contracts.ConnectionGrantParams{ConnectionID: connGH, SessionID: sesA})
	h.grant(contracts.ConnectionGrantParams{ConnectionID: connGH, ProjectID: prjOne})
	for _, n := range []*NodeService{h.mac, h.server} {
		h.call(n, contracts.ActionConnList, struct{}{})
		h.call(n, contracts.ActionConnCheck, contracts.ConnectionParams{ConnectionID: connGH})
	}
	h.call(h.mac, contracts.ActionConnInvoke, contracts.ConnectionInvokeParams{ConnectionID: connGH, SessionID: sesA, Operation: "get_issue"})
	plan, err := h.server.PrepareLaunch(ctx, sesB, []string{connGH})
	if err != nil || plan.Env["GH_TOKEN"] != pyreSecret {
		t.Fatalf("launch: %v", err)
	}
	mb, _ := json.Marshal(plan)
	h.wire = append(h.wire, string(mb))
	// Failure paths: expired with an adversarial fake that echoes its token,
	// a locked store, a revoked grant.
	h.state.Connections[0].Expired, h.state.Leak = true, true
	h.saveState()
	if _, err := h.call(h.mac, contracts.ActionConnInvoke, contracts.ConnectionInvokeParams{ConnectionID: connGH, SessionID: sesA, Operation: "get_issue"}); err != nil {
		errs = append(errs, err.Error())
	}
	h.call(h.mac, contracts.ActionConnCheck, contracts.ConnectionParams{ConnectionID: connGH})
	h.serverStore.SetLocked(true)
	if _, err := h.server.PrepareLaunch(ctx, sesB, []string{connGH}); err != nil {
		errs = append(errs, err.Error())
	}
	h.serverStore.SetLocked(false)
	h.revoke(contracts.ConnectionGrantParams{ConnectionID: connGH, SessionID: sesA})
	if _, err := h.call(h.mac, contracts.ActionConnInvoke, contracts.ConnectionInvokeParams{ConnectionID: connGH, SessionID: sesA, Operation: "get_issue"}); err != nil {
		errs = append(errs, err.Error())
	}
	if len(errs) < 3 {
		t.Fatalf("expected failure paths to produce errors, got %v", errs)
	}
	for _, e := range errs {
		t.Logf("redacted failure: %s", e)
	}

	var haystack bytes.Buffer
	for _, s := range h.wire {
		haystack.WriteString(s + "\n")
	}
	for _, s := range errs {
		haystack.WriteString(s + "\n")
	}
	for _, ev := range h.events.events {
		b, _ := json.Marshal(ev)
		haystack.Write(b)
	}
	haystack.Write(h.logs.Bytes())
	if b, err := os.ReadFile(h.callLog); err == nil {
		haystack.Write(b)
	}
	if b, err := os.ReadFile(filepath.Join(filepath.Dir(h.stateFile), "server", "provisions.json")); err == nil {
		haystack.Write(b)
	}
	for _, table := range []string{"connections", "connection_grants", "connection_availability", "operations"} {
		rows, err := h.db.Query("SELECT * FROM " + table)
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			rows.Scan(ptrs...)
			fmt.Fprintln(&haystack, vals...)
		}
		rows.Close()
	}
	if haystack.Len() < 2000 {
		t.Fatalf("haystack suspiciously small (%d bytes)", haystack.Len())
	}
	for _, secret := range []string{outfitSecret, pyreSecret} {
		if i := bytes.Index(haystack.Bytes(), []byte(secret)); i >= 0 {
			lo := i - 200
			if lo < 0 {
				lo = 0
			}
			t.Fatalf("synthetic credential %q disclosed near: %s", secret, haystack.Bytes()[lo:i+len(secret)])
		}
	}
	if !strings.Contains(haystack.String(), "GH_TOKEN") {
		t.Fatal("manifests/logs should still name the variable")
	}
}

type failingOutfit struct{ OutfitClient }

func (failingOutfit) Connections(context.Context) ([]outfit.Connection, error) {
	return nil, contracts.Errorf(contracts.ErrUnavailable, "outfit ls timed out")
}

// TestReportWithoutOutfitIncludesProvisions: a node whose Outfit binary is
// absent (production always wires the adapter) still reports the
// credentials Pyre provisioned on it, and pushes a report as soon as one is
// provisioned or deprovisioned (W-memconn live run: the server node had no
// Outfit and its local_credential never reached the hub catalog). An
// installed Outfit that fails still fails the report.
func TestReportWithoutOutfitIncludesProvisions(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	src := credentials.NewRef(credentials.BackendMemory, connGH, "GH_TOKEN")
	if err := h.macStore.Put(ctx, src, credentials.NewSecret(pyreSecret)); err != nil {
		t.Fatal(err)
	}
	pushes := 0
	store := credentials.NewMemoryStore()
	n, err := NewNodeService(NodeConfig{NodeID: nodeServer, Outfit: outfit.New(outfit.Options{Binary: filepath.Join(t.TempDir(), "no-outfit")}),
		Stores: credentials.Stores{}.Add(store), Index: credentials.OpenIndex(filepath.Join(t.TempDir(), "provisions.json")),
		DefaultBackend: credentials.BackendMemory, Grants: h.cat.Checker(nil), OnAvailabilityChange: func() { pushes++ }})
	if err != nil {
		t.Fatal(err)
	}
	if list, err := n.Report(ctx); err != nil || len(list) != 0 {
		t.Fatalf("empty node without outfit: %+v %v", list, err)
	}
	h.provision(h.mac, n, src)
	if pushes == 0 {
		t.Fatal("no connections report pushed after provision_accept")
	}
	list, err := n.Report(ctx)
	if err != nil || len(list) != 1 || list[0].ConnectionID != connGH || len(list[0].Availability) != 1 || list[0].Availability[0].Mode != "local_credential" {
		t.Fatalf("report without outfit = %+v %v", list, err)
	}
	before := pushes
	if _, err := h.call(n, ActionConnDeprovision, contracts.ConnectionParams{ConnectionID: connGH}); err != nil {
		t.Fatal(err)
	}
	if pushes == before {
		t.Fatal("no connections report pushed after deprovision")
	}
	if list, _ := n.Report(ctx); len(list) != 0 {
		t.Fatalf("deprovisioned connection still reported: %+v", list)
	}
	broken, _ := NewNodeService(NodeConfig{NodeID: nodeServer, Outfit: failingOutfit{}, Grants: h.cat.Checker(nil)})
	if _, err := broken.Report(ctx); err == nil {
		t.Fatal("an installed Outfit that fails must fail the report (the hub keeps the last good view)")
	}
}
