package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/daemon/daemontest"
	"github.com/jhh3/pyre/internal/runtime/claude"
)

const syntheticNodeToken = "pyn_synthetic-node-token-for-tests-only-000000"

// execHub is a hub that records node-credential starts.
type execHub struct {
	contracts.API // nil: unexpected calls panic
	mu            sync.Mutex
	starts        []execHubStart
}

type execHubStart struct {
	bearer string
	req    contracts.StartSessionRequest
}

type execHubClient struct {
	contracts.API
	h      *execHub
	bearer string
}

func (h *execHub) client(bearer string) contracts.API { return &execHubClient{h: h, bearer: bearer} }

func (c *execHubClient) StartSession(_ context.Context, r contracts.StartSessionRequest) (contracts.StartSessionResponse, error) {
	c.h.mu.Lock()
	defer c.h.mu.Unlock()
	c.h.starts = append(c.h.starts, execHubStart{bearer: c.bearer, req: r})
	return contracts.StartSessionResponse{Session: contracts.SessionView{Session: contracts.Session{SessionID: "ses_node", Tags: r.Tags},
		Run: &contracts.Run{RunID: "run_node", SessionID: "ses_node", NodeID: r.NodeID}}}, nil
}

func (c *execHubClient) GetProject(_ context.Context, r contracts.GetRequest) (contracts.Project, error) {
	if r.ID == "prj_sentry" {
		return contracts.Project{ProjectID: "prj_sentry", Name: "sentry"}, nil
	}
	return contracts.Project{}, contracts.Errorf(contracts.ErrNotFound, "no project %s", r.ID)
}

func (c *execHubClient) ListProjects(context.Context, contracts.ListProjectsRequest) (contracts.Page[contracts.Project], error) {
	return contracts.Page[contracts.Project]{Items: []contracts.Project{{ProjectID: "prj_sentry", Name: "sentry"}, {ProjectID: "prj_other", Name: "Other"},
		{ProjectID: "prj_dup1", Name: "dup"}, {ProjectID: "prj_dup2", Name: "dup"}}}, nil
}

func (c *execHubClient) ListAccounts(context.Context, contracts.ListAccountsRequest) (contracts.Page[contracts.AccountView], error) {
	return contracts.Page[contracts.AccountView]{Items: []contracts.AccountView{{Account: contracts.Account{AccountID: "acct_work", Label: "Work", RotaRef: "work"}}}}, nil
}

func (h *execHub) recorded() []execHubStart {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.starts)
}

func socketHTTP(sock string) *http.Client {
	return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	}}}
}

func postSock(t *testing.T, hc *http.Client, path string, body, out any) (int, *contracts.Error) {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := hc.Post("http://pyred"+path, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error *contracts.Error `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		return resp.StatusCode, e.Error
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatal(err)
		}
	}
	return resp.StatusCode, nil
}

func execNode(t *testing.T, hub *execHub, nodeToken string) (*node, *http.Client) {
	t.Helper()
	w := daemontest.NewWorld()
	base, err := claude.New(claude.Options{NodeID: "node_test", TmuxSocket: shortSock(t), HookCommand: []string{"true"}, ProfileAsConfigDir: true})
	if err != nil {
		t.Fatal(err)
	}
	n := startNode(t, shortDir(t), Config{Adapters: []contracts.RuntimeAdapter{daemontest.New(w, contracts.HarnessClaude)},
		ExecAdapters: []contracts.RuntimeAdapter{claude.NewExec(base)}, Accounts: &fakeBinder{}, Upstream: hub.client, NodeToken: nodeToken})
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- n.d.ServeLocal(ctx) }()
	t.Cleanup(func() { cancel(); <-served })
	hc := socketHTTP(n.d.SocketPath())
	waitFor(t, "socket", func() bool {
		resp, err := hc.Get("http://pyred" + PathLocalExec)
		if err == nil {
			resp.Body.Close()
		}
		return err == nil
	})
	return n, hc
}

// pyre run without a hub login: the node records the headless run with its
// own credential (exactly the shape the hub allows a node), resolving the
// project by name; the CLI then claims the binding as for any headless run.
func TestExecStartWithNodeCredential(t *testing.T) {
	hub := &execHub{}
	n, hc := execNode(t, hub, syntheticNodeToken)

	resp, err := hc.Get("http://pyred" + PathLocalExec)
	if err != nil {
		t.Fatal(err)
	}
	var info ExecInfo
	_ = json.NewDecoder(resp.Body).Decode(&info)
	resp.Body.Close()
	if !info.Start || info.NodeID != "node_test" {
		t.Fatalf("info = %+v", info)
	}

	secret := "synthetic-node-claim-secret"
	cwd := t.TempDir()
	var res ExecStartResponse
	code, ce := postSock(t, hc, PathLocalExecStart, ExecStartRequest{ClaimSHA256: claimHash(secret), Title: "sentry triage", Cwd: cwd, Project: "sentry"}, &res)
	if code != 200 {
		t.Fatalf("start: %d %v", code, ce)
	}
	if res.RunID != "run_node" || res.SessionID != "ses_node" || res.ProjectID == nil || *res.ProjectID != "prj_sentry" {
		t.Fatalf("response = %+v", res)
	}
	got := hub.recorded()
	if len(got) != 1 {
		t.Fatalf("starts = %+v", got)
	}
	r := got[0].req
	wantTags := []string{contracts.TagPyreRun, contracts.TagReportNone, contracts.TagArchiveWhenDone}
	if got[0].bearer != syntheticNodeToken || r.NodeID != "node_test" || r.Harness != contracts.HarnessClaude || r.Title != "sentry triage" || r.Cwd != cwd ||
		!slices.Equal(r.Args, []string{ExecArg + "=" + claimHash(secret)}) || !slices.Equal(r.Tags, wantTags) || r.ReportToParent == nil || *r.ReportToParent ||
		r.InitialPrompt != "" || r.ParentSessionID != "" || r.AccountPolicy.Mode != contracts.AccountEligible || r.OperationID == "" {
		t.Fatalf("hub request = %+v (bearer %q)", r, got[0].bearer)
	}

	// The hub's start command reaches the node; the CLI claims with its secret.
	sc := startCmd("ses_node", "run_node", contracts.HarnessClaude, cwd)
	sc.Session.Tags, sc.Args, sc.AccountPolicy = r.Tags, r.Args, r.AccountPolicy
	if res := n.do(command(contracts.CmdStartRun, sc)); res.Error != nil {
		t.Fatal(res.Error)
	}
	var b ExecBinding
	if code, ce := postSock(t, hc, PathLocalExecClaim, ExecClaimRequest{RunID: "run_node", Secret: secret, ClientPID: os.Getpid()}, &b); code != 200 {
		t.Fatalf("claim: %d %v", code, ce)
	}
	if b.AccountID != "acct_1" || b.Env["CLAUDE_CONFIG_DIR"] != "/profiles/acct_1" {
		t.Fatalf("binding = %+v", b)
	}

	// --account by a unique label prefix (the CLI's resolution,
	// internal/refs), --resume, and project "none".
	code, ce = postSock(t, hc, PathLocalExecStart, ExecStartRequest{ClaimSHA256: claimHash("s2"), Cwd: cwd, Account: "wor", Project: "none",
		Resume: "0b7c1f3e-1111-4222-8333-444455556666"}, &res)
	if code != 200 {
		t.Fatalf("start 2: %d %v", code, ce)
	}
	r = hub.recorded()[1].req
	if r.AccountPolicy.Mode != contracts.AccountExplicit || r.AccountPolicy.AccountID != "acct_work" || r.ProjectID != nil ||
		!slices.Equal(r.Args, []string{ExecArg + "=" + claimHash("s2"), ExecResumeArg + "=0b7c1f3e-1111-4222-8333-444455556666"}) {
		t.Fatalf("hub request 2 = %+v", r)
	}

	// Refusals: nothing reaches the hub.
	for name, tc := range map[string]struct {
		req  ExecStartRequest
		code int
	}{
		"bad hash":          {ExecStartRequest{ClaimSHA256: "abc", Cwd: cwd}, http.StatusBadRequest},
		"codex, no adapter": {ExecStartRequest{ClaimSHA256: claimHash("x"), Cwd: cwd, Harness: contracts.HarnessCodex}, http.StatusBadRequest},
		"agy":               {ExecStartRequest{ClaimSHA256: claimHash("x"), Cwd: cwd, Harness: contracts.HarnessAgy}, http.StatusBadRequest},
		"relative cwd":      {ExecStartRequest{ClaimSHA256: claimHash("x"), Cwd: "work"}, http.StatusBadRequest},
		"resume flag":       {ExecStartRequest{ClaimSHA256: claimHash("x"), Cwd: cwd, Resume: "--dangerously-skip-permissions"}, http.StatusBadRequest},
		"unknown project":   {ExecStartRequest{ClaimSHA256: claimHash("x"), Cwd: cwd, Project: "nope"}, http.StatusNotFound},
		"ambiguous project": {ExecStartRequest{ClaimSHA256: claimHash("x"), Cwd: cwd, Project: "dup"}, http.StatusConflict},
		"unknown account":   {ExecStartRequest{ClaimSHA256: claimHash("x"), Cwd: cwd, Account: "nope"}, http.StatusNotFound},
	} {
		if code, ce := postSock(t, hc, PathLocalExecStart, tc.req, nil); code != tc.code {
			t.Errorf("%s: %d %v", name, code, ce)
		}
	}
	if n := len(hub.recorded()); n != 2 {
		t.Fatalf("%d hub starts after refusals", n)
	}
}

// A node without its own hub credential does not offer node-started runs.
func TestExecStartNeedsNodeCredential(t *testing.T) {
	hub := &execHub{}
	_, hc := execNode(t, hub, "")
	resp, err := hc.Get("http://pyred" + PathLocalExec)
	if err != nil {
		t.Fatal(err)
	}
	var info ExecInfo
	_ = json.NewDecoder(resp.Body).Decode(&info)
	resp.Body.Close()
	if info.Start {
		t.Fatal("start offered without a node credential")
	}
	if code, ce := postSock(t, hc, PathLocalExecStart, ExecStartRequest{ClaimSHA256: claimHash("x"), Cwd: t.TempDir()}, nil); code != http.StatusServiceUnavailable {
		t.Fatalf("start: %d %v", code, ce)
	}
	if len(hub.recorded()) != 0 {
		t.Fatal("reached the hub")
	}
}

// The start endpoint checks the connecting uid on the real socket.
func TestExecStartPeerCheck(t *testing.T) {
	if !peerCredSupported {
		t.Skip("no peer credentials on this platform")
	}
	sock := shortSock(t)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := net.Dial("unix", sock)
		if err == nil {
			defer c.Close()
			time.Sleep(time.Second)
		}
	}()
	c, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := withPeer(context.Background(), c)
	if p, ok := ctx.Value(peerKey{}).(peerInfo); !ok || p.err != nil || p.uid != os.Getuid() {
		t.Fatalf("peer = %+v %v", p, ok)
	}
	if err := checkPeer(ctx); err != nil {
		t.Fatal(err)
	}
	other := context.WithValue(context.Background(), peerKey{}, peerInfo{uid: os.Getuid() + 1})
	if err := checkPeer(other); !contracts.IsCode(err, contracts.ErrForbidden) {
		t.Fatalf("other uid: %v", err)
	}
	if err := checkPeer(context.Background()); !contracts.IsCode(err, contracts.ErrForbidden) {
		t.Fatalf("no peer info: %v", err)
	}
	unknown := context.WithValue(context.Background(), peerKey{}, peerInfo{err: os.ErrPermission})
	if err := checkPeer(unknown); !contracts.IsCode(err, contracts.ErrForbidden) {
		t.Fatalf("unknown peer: %v", err)
	}
}
