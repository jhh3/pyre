package assemble

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/connections"
	c "github.com/jhh3/pyre/internal/contracts"
)

func publishHTML(t *testing.T, st *Stack, ctx context.Context) c.PublishReceipt {
	t.Helper()
	data := []byte("<p>synthetic preview</p>")
	sum := sha256.Sum256(data)
	up, err := st.Hub.BeginUpload(ctx, c.BeginUploadRequest{Mutation: c.Mutation{OperationID: c.NewID(c.PrefixOperation)}, Title: "p", Filename: "p.html",
		Size: int64(len(data)), Digest: c.Digest("sha256:" + hex.EncodeToString(sum[:]))})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Hub.UploadChunk(ctx, up.UploadID, 0, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	r, err := st.Hub.FinalizeUpload(ctx, c.FinalizeUploadRequest{Mutation: c.Mutation{OperationID: c.NewID(c.PrefixOperation)}, UploadID: up.UploadID})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// Without a separate preview listener, minted links use the hub URL and the
// main handler serves them (one use each).
func TestPreviewSameOriginWhenNoPreviewListener(t *testing.T) {
	st, _ := newStack(t)
	ctx := c.WithPrincipal(context.Background(), ownerP)
	r := publishHTML(t, st, ctx)
	pu, err := st.Hub.MintPreviewURL(ctx, c.MintPreviewRequest{VersionID: r.Version.VersionID})
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(pu.URL)
	if err != nil || u.Scheme != "https" || u.Host != "hub.synth.invalid:7443" || !strings.HasPrefix(u.Path, "/preview/") {
		t.Fatalf("preview url %q", pu.URL)
	}
	srv := httptest.NewServer(st.Handler)
	defer srv.Close()
	for i, want := range []int{http.StatusOK, http.StatusForbidden} {
		resp, err := http.Get(srv.URL + u.EscapedPath())
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("fetch %d: %d, want %d", i, resp.StatusCode, want)
		}
		if i == 0 && !strings.Contains(resp.Header.Get("Content-Security-Policy"), "sandbox") {
			t.Fatal("preview served without sandbox CSP")
		}
	}
}

// With a separate preview listener, links point at it and the API handler
// does not serve previews.
func TestPreviewSeparateOrigin(t *testing.T) {
	k := &testClock{t: time.Now().UTC().Truncate(time.Second)}
	n := newSimNode(t, k)
	st, err := Open(context.Background(), Config{DataDir: t.TempDir(), Clock: k, Nodes: n, HubURL: "https://hub.synth.invalid:7443", PreviewURL: "https://hub.synth.invalid:8444/"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	t.Cleanup(n.shutdown)
	ctx := c.WithPrincipal(context.Background(), ownerP)
	r := publishHTML(t, st, ctx)
	pu, err := st.Hub.MintPreviewURL(ctx, c.MintPreviewRequest{VersionID: r.Version.VersionID})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(pu.URL, "https://hub.synth.invalid:8444/preview/"+r.Version.VersionID+"/") {
		t.Fatalf("preview url %q", pu.URL)
	}
	u, _ := url.Parse(pu.URL)
	api := httptest.NewServer(st.Handler)
	defer api.Close()
	resp, err := http.Get(api.URL + u.EscapedPath())
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatal("API origin served a preview although a separate preview origin is configured")
	}
	prev := httptest.NewServer(st.Preview)
	defer prev.Close()
	if resp, err = http.Get(prev.URL + u.EscapedPath()); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("preview origin: %d", resp.StatusCode)
	}
}

// A node may only check grants for sessions running on it; unknown sessions
// are denied without an error.
func TestCheckConnectionGrantNodeScope(t *testing.T) {
	st, n := newStack(t)
	ctx := c.WithPrincipal(context.Background(), ownerP)
	resp, err := st.Hub.StartSession(ctx, c.StartSessionRequest{Mutation: c.Mutation{OperationID: c.NewID(c.PrefixOperation)}, NodeID: n.nodeID, Harness: c.HarnessMock, Cwd: "/tmp/synth"})
	if err != nil {
		t.Fatal(err)
	}
	n.idle()
	sid := resp.Session.Session.SessionID
	conn := c.NewID(c.PrefixConnection)
	otherNode := c.WithPrincipal(context.Background(), c.Principal{Kind: c.PrincipalNode, ID: c.NewID(c.PrefixNode)})
	if _, err := st.Hub.CheckConnectionGrant(otherNode, c.CheckGrantRequest{ConnectionID: conn, SessionID: sid}); !c.IsCode(err, c.ErrForbidden) {
		t.Fatalf("other node: %v", err)
	}
	d, err := st.Hub.CheckConnectionGrant(n.ctx(), c.CheckGrantRequest{ConnectionID: conn, SessionID: c.NewID(c.PrefixSession)})
	if err != nil || d.Allowed {
		t.Fatalf("unknown session: %+v %v", d, err)
	}
	other := c.WithPrincipal(context.Background(), c.Principal{Kind: c.PrincipalSession, ID: c.NewID(c.PrefixSession)})
	if _, err := st.Hub.CheckConnectionGrant(other, c.CheckGrantRequest{ConnectionID: conn, SessionID: sid}); !c.IsCode(err, c.ErrForbidden) {
		t.Fatalf("other session: %v", err)
	}
}

// The node that holds a connection (reported availability) executes
// cross-node connection.invoke and may check a remote session's grant; a
// node that does not hold it still may not.
func TestCheckConnectionGrantHoldingNode(t *testing.T) {
	st, n := newStack(t)
	ctx := c.WithPrincipal(context.Background(), ownerP)
	resp, err := st.Hub.StartSession(ctx, c.StartSessionRequest{Mutation: c.Mutation{OperationID: c.NewID(c.PrefixOperation)}, NodeID: n.nodeID, Harness: c.HarnessMock, Cwd: "/tmp/synth"})
	if err != nil {
		t.Fatal(err)
	}
	n.idle()
	sid := resp.Session.Session.SessionID
	conn := "conn_synth-held"
	holder := c.NewID(c.PrefixNode)
	if err := st.Connections.ApplyReport(context.Background(), holder, []c.Connection{{ConnectionID: conn, Name: "synth-held", Modes: []string{"remote_invoke"},
		Availability: []c.ConnectionAvailability{{NodeID: holder, Mode: "remote_invoke", State: "unknown", VerifiedAt: time.Now().UTC()}}}}); err != nil {
		t.Fatal(err)
	}
	holderCtx := c.WithPrincipal(context.Background(), c.Principal{Kind: c.PrincipalNode, ID: holder})
	d, err := st.Hub.CheckConnectionGrant(holderCtx, c.CheckGrantRequest{ConnectionID: conn, SessionID: sid})
	if err != nil || d.Allowed {
		t.Fatalf("holding node, no grant: %+v %v", d, err)
	}
	if _, err := st.Connections.Grant(context.Background(), ownerP, connGrantReq(conn, n.nodeID, sid)); err != nil {
		t.Fatal(err)
	}
	if d, err = st.Hub.CheckConnectionGrant(holderCtx, c.CheckGrantRequest{ConnectionID: conn, SessionID: sid}); err != nil || !d.Allowed {
		t.Fatalf("holding node after grant: %+v %v", d, err)
	}
	stranger := c.WithPrincipal(context.Background(), c.Principal{Kind: c.PrincipalNode, ID: c.NewID(c.PrefixNode)})
	if _, err := st.Hub.CheckConnectionGrant(stranger, c.CheckGrantRequest{ConnectionID: conn, SessionID: sid}); !c.IsCode(err, c.ErrForbidden) {
		t.Fatalf("non-holding node: %v", err)
	}
}

// A successful connection.check routed through the hub updates the
// catalog's availability for that node right away.
func TestConnectionCheckResultUpdatesCatalog(t *testing.T) {
	st, n := newStack(t)
	ctx := c.WithPrincipal(context.Background(), ownerP)
	conn := "conn_synth-check"
	if err := st.Connections.ApplyReport(context.Background(), n.nodeID, []c.Connection{{ConnectionID: conn, Name: "synth-check", Modes: []string{"remote_invoke"},
		Availability: []c.ConnectionAvailability{{NodeID: n.nodeID, Mode: "remote_invoke", State: "unknown"}}}}); err != nil {
		t.Fatal(err)
	}
	verifiedAt := time.Now().UTC().Truncate(time.Second)
	n.mu.Lock()
	n.onAction = func(ar c.NodeActionRequest) (any, error) {
		if ar.Action != c.ActionConnCheck {
			return nil, c.Errorf(c.ErrUnsupported, "unexpected %s", ar.Action)
		}
		return c.ConnectionAvailability{NodeID: n.nodeID, Mode: "remote_invoke", State: "verified", VerifiedAt: verifiedAt}, nil
	}
	n.mu.Unlock()
	params, _ := json.Marshal(c.ConnectionParams{ConnectionID: conn})
	res, err := st.Hub.InvokeNodeAction(ctx, c.NodeActionRequest{Mutation: c.Mutation{OperationID: c.NewID(c.PrefixOperation)}, NodeID: n.nodeID, Action: c.ActionConnCheck, Params: params, TimeoutSeconds: 5})
	if err != nil || res.Outcome != c.OutcomeSucceeded {
		t.Fatalf("check: %+v %v", res, err)
	}
	got, err := st.Connections.Get(context.Background(), conn)
	if err != nil || len(got.Availability) != 1 || got.Availability[0].State != "verified" {
		t.Fatalf("catalog after check: %+v %v", got, err)
	}
}

// connection.invoke without node_id is routed by the hub to the node with
// verified remote_invoke availability, after the session's grant check.
func TestConnectionInvokeRoutedByHub(t *testing.T) {
	st, n := newStack(t)
	ctx := c.WithPrincipal(context.Background(), ownerP)
	resp, err := st.Hub.StartSession(ctx, c.StartSessionRequest{Mutation: c.Mutation{OperationID: c.NewID(c.PrefixOperation)}, NodeID: n.nodeID, Harness: c.HarnessMock, Cwd: "/tmp/synth"})
	if err != nil {
		t.Fatal(err)
	}
	n.idle()
	sid := resp.Session.Session.SessionID
	conn := "conn_synth-route"
	report := func(state string) {
		t.Helper()
		if err := st.Connections.ApplyReport(context.Background(), n.nodeID, []c.Connection{{ConnectionID: conn, Name: "synth-route", Modes: []string{"remote_invoke"},
			Availability: []c.ConnectionAvailability{{NodeID: n.nodeID, Mode: "remote_invoke", State: state, VerifiedAt: time.Now().UTC()}}}}); err != nil {
			t.Fatal(err)
		}
	}
	var gotNode string
	n.mu.Lock()
	n.onAction = func(ar c.NodeActionRequest) (any, error) {
		gotNode = ar.NodeID
		return c.ConnectionInvokeResult{ConnectionID: conn, Output: c.RawJSON(`{"data":"synthetic-routed"}`)}, nil
	}
	n.mu.Unlock()
	params, _ := json.Marshal(c.ConnectionInvokeParams{ConnectionID: conn, SessionID: sid, Operation: "hello"})
	invoke := func() (c.NodeActionResult, error) {
		return st.Hub.InvokeNodeAction(ctx, c.NodeActionRequest{Mutation: c.Mutation{OperationID: c.NewID(c.PrefixOperation)}, Action: c.ActionConnInvoke, Params: params, TimeoutSeconds: 5})
	}
	report("verified")
	if _, err := invoke(); !c.IsCode(err, c.ErrForbidden) {
		t.Fatalf("no grant: %v", err)
	}
	if _, err := st.Connections.Grant(context.Background(), ownerP, connGrantReq(conn, n.nodeID, sid)); err != nil {
		t.Fatal(err)
	}
	report("unknown")
	if _, err := invoke(); !c.IsCode(err, c.ErrUnavailable) {
		t.Fatalf("unverified availability must not route: %v", err)
	}
	report("verified")
	res, err := invoke()
	if err != nil || res.Outcome != c.OutcomeSucceeded || gotNode != n.nodeID || !strings.Contains(string(res.Result), "synthetic-routed") {
		t.Fatalf("routed invoke: %+v %v node=%q", res, err, gotNode)
	}
}

func connGrantReq(conn, node, session string) connections.GrantRequest {
	return connections.GrantRequest{Mutation: c.Mutation{OperationID: c.NewID(c.PrefixOperation)},
		ConnectionGrantParams: c.ConnectionGrantParams{ConnectionID: conn, NodeID: node, SessionID: session}}
}

// An unredeemed enrollment is listed (state never) but cannot be targeted.
func TestPendingEnrollmentListedNotLaunchable(t *testing.T) {
	st, _ := newStack(t)
	ctx := c.WithPrincipal(context.Background(), ownerP)
	tk, err := st.Hub.CreateEnrollment(ctx, c.EnrollmentRequest{Mutation: c.Mutation{OperationID: c.NewID(c.PrefixOperation)}, Name: "synth-pending"})
	if err != nil {
		t.Fatal(err)
	}
	page, err := st.Hub.ListNodes(ctx, c.ListNodesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, n := range page.Items {
		if n.NodeID == tk.NodeID {
			found = n.Connection.State == c.ConnNever && n.Labels[c.NodeLabelEnrollment] == "pending"
		}
	}
	if !found || len(page.Items) != 2 {
		t.Fatalf("pending node not listed as never: %+v", page.Items)
	}
	_, err = st.Hub.StartSession(ctx, c.StartSessionRequest{Mutation: c.Mutation{OperationID: c.NewID(c.PrefixOperation)}, NodeID: tk.NodeID, Harness: c.HarnessMock, Cwd: "/tmp/synth"})
	if !c.IsCode(err, c.ErrNotFound) || !strings.Contains(err.Error(), "not completed enrollment") {
		t.Fatalf("launch on pending node: %v", err)
	}
}
