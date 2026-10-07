package daemon

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/client"
	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/mock"
)

// fakeHub records what reaches the hub and can be taken down.
type fakeHub struct {
	contracts.API // nil: unexpected calls panic
	down          atomic.Bool
	mu            sync.Mutex
	sent          []sentMsg
	byOp          map[string]contracts.MessageView
}

type sentMsg struct {
	bearer string
	req    contracts.SendMessageRequest
}

type hubClient struct {
	contracts.API
	h      *fakeHub
	bearer string
}

func (h *fakeHub) client(bearer string) contracts.API { return &hubClient{h: h, bearer: bearer} }

func (c *hubClient) unavailable() error {
	if c.h.down.Load() {
		return contracts.Errorf(contracts.ErrUnavailable, "hub unreachable (test)")
	}
	return nil
}

func (c *hubClient) WhoAmI(ctx context.Context) (contracts.Principal, error) {
	if err := c.unavailable(); err != nil {
		return contracts.Principal{}, err
	}
	return contracts.Principal{Kind: contracts.PrincipalOwner, ID: "cli_hub"}, nil
}

func (c *hubClient) Hello(ctx context.Context, r contracts.HelloRequest) (contracts.HelloResponse, error) {
	if err := c.unavailable(); err != nil {
		return contracts.HelloResponse{}, err
	}
	return contracts.HelloResponse{Protocol: 1, HubID: "hub_test"}, nil
}

func (c *hubClient) ListSessions(ctx context.Context, r contracts.ListSessionsRequest) (contracts.Page[contracts.SessionView], error) {
	if err := c.unavailable(); err != nil {
		return contracts.Page[contracts.SessionView]{}, err
	}
	return contracts.Page[contracts.SessionView]{Items: []contracts.SessionView{{Session: contracts.Session{SessionID: "ses_from_hub"}}}}, nil
}

func (c *hubClient) GetSession(ctx context.Context, r contracts.GetRequest) (contracts.SessionView, error) {
	if err := c.unavailable(); err != nil {
		return contracts.SessionView{}, err
	}
	return contracts.SessionView{}, contracts.Errorf(contracts.ErrNotFound, "x")
}

func (c *hubClient) ReadSession(ctx context.Context, r contracts.ReadSessionRequest) (contracts.ReadSessionResponse, error) {
	if err := c.unavailable(); err != nil {
		return contracts.ReadSessionResponse{}, err
	}
	return contracts.ReadSessionResponse{}, nil
}

func (c *hubClient) AttachSession(ctx context.Context, r contracts.AttachRequest) (contracts.AttachDescriptor, error) {
	if err := c.unavailable(); err != nil {
		return contracts.AttachDescriptor{}, err
	}
	return contracts.AttachDescriptor{}, nil
}

func (c *hubClient) GetMessage(ctx context.Context, r contracts.GetRequest) (contracts.MessageView, error) {
	if err := c.unavailable(); err != nil {
		return contracts.MessageView{}, err
	}
	return contracts.MessageView{}, contracts.Errorf(contracts.ErrNotFound, "x")
}

func (c *hubClient) ListProjects(ctx context.Context, r contracts.ListProjectsRequest) (contracts.Page[contracts.Project], error) {
	if err := c.unavailable(); err != nil {
		return contracts.Page[contracts.Project]{}, err
	}
	return contracts.Page[contracts.Project]{}, nil
}

func (c *hubClient) SendMessage(ctx context.Context, r contracts.SendMessageRequest) (contracts.MessageView, error) {
	if err := c.unavailable(); err != nil {
		return contracts.MessageView{}, err
	}
	c.h.mu.Lock()
	defer c.h.mu.Unlock()
	if v, ok := c.h.byOp[r.OperationID]; ok { // hub dedupe by operation id
		return v, nil
	}
	c.h.sent = append(c.h.sent, sentMsg{bearer: c.bearer, req: r})
	v := contracts.MessageView{Envelope: contracts.MessageEnvelope{MessageID: r.MessageID, OperationID: r.OperationID, Recipient: r.Recipient, Body: r.Body},
		Status: contracts.DeliveryStatus{Storage: contracts.StorageStored, Runtime: contracts.DeliveryQueued}}
	c.h.byOp[r.OperationID] = v
	return v, nil
}

func (h *fakeHub) sends() []sentMsg {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]sentMsg(nil), h.sent...)
}

func socketClient(sock, token string) *client.Client {
	hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	}}}
	return client.New("http://pyred.local", token, client.Options{HTTPClient: hc, MaxAttempts: 1})
}

func TestLocalSocketHubOutageFallbacksAndOutbox(t *testing.T) {
	dir := shortDir(t)
	hub := &fakeHub{byOp: map[string]contracts.MessageView{}}
	ma := mock.NewAdapter()
	n := startNode(t, dir, Config{Adapters: []contracts.RuntimeAdapter{ma}, Upstream: hub.client, OwnerToken: "pyre_owner_test_token_0000000000", NodeName: "testnode", OutboxFlush: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan error, 1)
	go func() { served <- n.d.ServeLocal(ctx) }()
	sock := n.d.SocketPath()
	waitFor(t, "socket", func() bool { _, err := os.Stat(sock); return err == nil })
	st, _ := os.Stat(sock)
	if st.Mode().Perm()&0o077 != 0 {
		t.Fatalf("socket mode %v", st.Mode().Perm())
	}

	sc := startCmd("ses_l", "run_l", contracts.HarnessMock, t.TempDir())
	sc.SessionToken = "pyre_s_session_token_for_tests_000000"
	n.start(sc)
	if p, _ := ma.ReadTranscript(ctx, contracts.RunHandle{RunID: "run_l"}, contracts.ReadSessionRequest{}); len(p.Entries) == 0 {
		t.Fatal("mock transcript empty")
	}

	owner := socketClient(sock, "") // no bearer: owner credential on the 0600 socket
	agent := socketClient(sock, sc.SessionToken)

	// Hub up: proxied.
	page, err := owner.ListSessions(ctx, contracts.ListSessionsRequest{})
	if err != nil || len(page.Items) != 1 || page.Items[0].Session.SessionID != "ses_from_hub" {
		t.Fatalf("proxied list = %+v %v", page, err)
	}

	// Hub down: local reads keep working, marked node-local and stale.
	hub.down.Store(true)
	page, err = owner.ListSessions(ctx, contracts.ListSessionsRequest{})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("local list = %+v %v", page, err)
	}
	v := page.Items[0]
	if v.Session.SessionID != "ses_l" || v.Run == nil || v.Run.RunID != "run_l" || !v.Freshness.Stale || !strings.HasPrefix(v.Freshness.Source, "node-local") {
		t.Fatalf("local view = %+v", v)
	}
	if _, err := owner.GetSession(ctx, contracts.GetRequest{ID: "ses_l"}); err != nil {
		t.Fatalf("local get: %v", err)
	}
	rd, err := agent.ReadSession(ctx, contracts.ReadSessionRequest{SessionID: "ses_l"})
	if err != nil || len(rd.Entries) == 0 || !strings.HasPrefix(rd.Freshness.Source, "node-local") {
		t.Fatalf("local read = %+v %v", rd, err)
	}
	if desc, err := owner.AttachSession(ctx, contracts.AttachRequest{Mutation: contracts.Mutation{OperationID: "op_att"}, SessionID: "ses_l"}); err != nil || desc.RunID != "run_l" {
		t.Fatalf("local attach = %+v %v", desc, err)
	}
	if _, err := owner.ListProjects(ctx, contracts.ListProjectsRequest{}); !contracts.IsCode(err, contracts.ErrUnavailable) {
		t.Fatalf("hub-only op while down = %v", err)
	}

	// Agent send while the hub is down -> durable outbox, pending_local.
	req := contracts.SendMessageRequest{Mutation: contracts.Mutation{OperationID: "op_send_1"},
		Recipient: contracts.Address{Kind: contracts.AddrSession, ID: "ses_peer"}, Body: contracts.MessageBody{Text: "results ready"}}
	mv, err := agent.SendMessage(ctx, req)
	if err != nil || mv.Status.Storage != contracts.StoragePendingLocal || mv.Envelope.MessageID == "" || mv.Envelope.Sender.Kind != contracts.PrincipalSession || mv.Envelope.Sender.ID != "ses_l" {
		t.Fatalf("outbox send = %+v %v", mv, err)
	}
	// Retry of the same operation returns the same message, no duplicate.
	mv2, err := agent.SendMessage(ctx, req)
	if err != nil || mv2.Envelope.MessageID != mv.Envelope.MessageID {
		t.Fatalf("retry = %+v %v", mv2, err)
	}
	// Same op with a different body is a conflict.
	bad := req
	bad.Body.Text = "something else"
	if _, err := agent.SendMessage(ctx, bad); !contracts.IsCode(err, contracts.ErrConflict) {
		t.Fatalf("conflicting reuse = %v", err)
	}
	// A second message while still down keeps order behind the first.
	req2 := req
	req2.OperationID, req2.Body.Text = "op_send_2", "second"
	mv3, _ := agent.SendMessage(ctx, req2)
	if got, _ := owner.GetMessage(ctx, contracts.GetRequest{ID: mv.Envelope.MessageID}); got.Status.Storage != contracts.StoragePendingLocal {
		t.Fatalf("local get message = %+v", got)
	}
	if n.d.FlushOutbox(ctx) != 0 || len(hub.sends()) != 0 {
		t.Fatal("flushed while hub down")
	}

	// Hub returns: flush once, same IDs, sent with the session's identity.
	hub.down.Store(false)
	n.d.db.Exec(`UPDATE daemon_outbox SET next_at=''`)
	if got := n.d.FlushOutbox(ctx); got != 2 {
		t.Fatalf("flushed %d, want 2", got)
	}
	n.d.FlushOutbox(ctx)
	sends := hub.sends()
	if len(sends) != 2 {
		t.Fatalf("hub received %d messages, want 2", len(sends))
	}
	if sends[0].req.MessageID != mv.Envelope.MessageID || sends[0].req.OperationID != "op_send_1" || sends[0].bearer != sc.SessionToken {
		t.Fatalf("first flushed = %+v", sends[0])
	}
	if sends[1].req.MessageID != mv3.Envelope.MessageID {
		t.Fatalf("order not preserved: %+v", sends)
	}
	// After flush, a retry of the original op returns the stored hub view.
	mv4, err := agent.SendMessage(ctx, req)
	if err != nil || mv4.Status.Storage != contracts.StorageStored || mv4.Envelope.MessageID != mv.Envelope.MessageID {
		t.Fatalf("post-flush retry = %+v %v", mv4, err)
	}
	if len(hub.sends()) != 2 {
		t.Fatal("post-flush retry created a new hub message")
	}
	// Direct send with the hub up goes straight through with a stable ID.
	req5 := req
	req5.OperationID = "op_send_5"
	mv5, err := agent.SendMessage(ctx, req5)
	if err != nil || mv5.Status.Storage != contracts.StorageStored || mv5.Envelope.MessageID == "" {
		t.Fatalf("direct send = %+v %v", mv5, err)
	}
	// Unknown bearer while the hub is down cannot be verified.
	hub.down.Store(true)
	if _, err := socketClient(sock, "pyre_unknown_token_xxxxxxxxxxxxxxxx").ListSessions(ctx, contracts.ListSessionsRequest{}); !contracts.IsCode(err, contracts.ErrUnauthorized) {
		t.Fatalf("unknown bearer while down = %v", err)
	}
	cancel()
	if err := <-served; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "pyred.sock")); !os.IsNotExist(err) {
		t.Fatal("socket not removed on shutdown")
	}
}

func TestStandaloneNodeLocalOnly(t *testing.T) {
	dir := shortDir(t)
	n := startNode(t, dir, Config{Adapters: []contracts.RuntimeAdapter{mock.NewAdapter()}})
	n.start(startCmd("ses_s", "run_s", contracts.HarnessMock, t.TempDir()))
	api := n.d.LocalAPI()
	ctx := contracts.WithPrincipal(context.Background(), contracts.Principal{Kind: contracts.PrincipalOwner, ID: "local"})
	page, err := api.ListSessions(ctx, contracts.ListSessionsRequest{})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("standalone list = %+v %v", page, err)
	}
	mv, err := api.SendMessage(ctx, contracts.SendMessageRequest{Mutation: contracts.Mutation{OperationID: "op_1"}, Recipient: contracts.Address{Kind: contracts.AddrOwner, ID: "owner"}, Body: contracts.MessageBody{Text: "x"}})
	if err != nil || mv.Status.Storage != contracts.StoragePendingLocal {
		t.Fatalf("standalone send = %+v %v", mv, err)
	}
	if _, err := api.ListProjects(ctx, contracts.ListProjectsRequest{}); !contracts.IsCode(err, contracts.ErrUnavailable) {
		t.Fatalf("hub-only op = %v", err)
	}
	// Without a hub the node still lists (only) itself.
	nodes, err := api.ListNodes(ctx, contracts.ListNodesRequest{})
	if err != nil || len(nodes.Items) != 1 || nodes.Items[0].NodeID != n.d.NodeID() {
		t.Fatalf("standalone nodes = %+v %v", nodes, err)
	}
}

func (c *hubClient) InvokeNodeAction(ctx context.Context, r contracts.NodeActionRequest) (contracts.NodeActionResult, error) {
	if err := c.unavailable(); err != nil {
		return contracts.NodeActionResult{}, err
	}
	return contracts.NodeActionResult{Outcome: contracts.OutcomeSucceeded, Result: contracts.RawJSON(`{"from":"hub"}`)}, nil
}

func (c *hubClient) GetNode(ctx context.Context, r contracts.GetRequest) (contracts.Node, error) {
	if err := c.unavailable(); err != nil {
		return contracts.Node{}, err
	}
	return contracts.Node{NodeID: r.ID, Name: "from-hub"}, nil
}

// During a hub outage, read-only memory actions for this node are answered
// by its own handlers (marked node-local); other actions and other nodes
// still report the outage.
func TestLocalMemoryActionsDuringHubOutage(t *testing.T) {
	dir := shortDir(t)
	hub := &fakeHub{byOp: map[string]contracts.MessageView{}}
	n := startNode(t, dir, Config{Adapters: []contracts.RuntimeAdapter{mock.NewAdapter()}, Upstream: hub.client, NodeName: "memnode"})
	n.d.RegisterAction(contracts.ActionMemorySearch, func(ctx context.Context, p json.RawMessage) (any, error) {
		return contracts.MemorySearchResult{Hits: []contracts.MemoryHit{{ID: "note:1:synthetic", Title: "synthetic hit"}},
			Freshness: contracts.Freshness{Source: "hive:cli", Reason: "no hive tick recorded"}}, nil
	})
	n.d.RegisterAction(contracts.ActionMemoryStatus, func(ctx context.Context, p json.RawMessage) (any, error) {
		return contracts.MemoryStatus{Backend: "hive", Detail: "stale"}, nil
	})
	n.d.RegisterAction("connection.list", func(ctx context.Context, p json.RawMessage) (any, error) { return []string{}, nil })
	api := n.d.LocalAPI()
	ctx := contracts.WithPrincipal(context.Background(), contracts.Principal{Kind: contracts.PrincipalSession, ID: "ses_mem"})
	req := func(node, action string) contracts.NodeActionRequest {
		return contracts.NodeActionRequest{Mutation: contracts.Mutation{OperationID: "op_" + action}, NodeID: node, Action: action, Params: contracts.RawJSON(`{"query":"synthetic"}`)}
	}
	if res, err := api.InvokeNodeAction(ctx, req(n.d.NodeID(), contracts.ActionMemorySearch)); err != nil || string(res.Result) != `{"from":"hub"}` {
		t.Fatalf("hub up: %+v %v", res, err)
	}
	hub.down.Store(true)
	res, err := api.InvokeNodeAction(ctx, req(n.d.NodeID(), contracts.ActionMemorySearch))
	if err != nil || res.Outcome != contracts.OutcomeSucceeded {
		t.Fatalf("local search: %+v %v", res, err)
	}
	var sr contracts.MemorySearchResult
	if err := json.Unmarshal(res.Result, &sr); err != nil || len(sr.Hits) != 1 || sr.Freshness.Source != "hive:cli" || !strings.Contains(sr.Freshness.Reason, "answered by node") {
		t.Fatalf("local search result: %s %v", res.Result, err)
	}
	res, err = api.InvokeNodeAction(ctx, req("", contracts.ActionMemoryStatus))
	var ms contracts.MemoryStatus
	if err != nil || json.Unmarshal(res.Result, &ms) != nil || ms.Backend != "hive" || !strings.Contains(ms.Detail, "hub unreachable") {
		t.Fatalf("local status: %s %v", res.Result, err)
	}
	if _, err := api.InvokeNodeAction(ctx, req("node_01OTHERNODE0000000000000000", contracts.ActionMemorySearch)); !contracts.IsCode(err, contracts.ErrUnavailable) {
		t.Fatalf("other node while down: %v", err)
	}
	if _, err := api.InvokeNodeAction(ctx, req(n.d.NodeID(), "connection.list")); !contracts.IsCode(err, contracts.ErrUnavailable) {
		t.Fatalf("non-memory action while down: %v", err)
	}
	owner := contracts.WithPrincipal(context.Background(), contracts.Principal{Kind: contracts.PrincipalOwner, ID: "local"})
	if nd, err := api.GetNode(owner, contracts.GetRequest{ID: n.d.NodeID()}); err != nil || nd.Name != "memnode" {
		t.Fatalf("local node while down: %+v %v", nd, err)
	}
}
