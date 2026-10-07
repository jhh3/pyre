package conformance

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	c "github.com/jhh3/pyre/internal/contracts"
)

// ConnectionReporter makes a connection known to the target's catalog, as a
// node's Outfit connection report would (metadata only, no grants).
type ConnectionReporter interface {
	ReportConnection(ctx context.Context, conn c.Connection) error
}

// v1.2: attention.get returns one item in any state, owner only.
func testAttentionGet(t *testing.T, s *suite) {
	sv := s.start("asker-get")
	m, err := s.api.SendMessage(s.session(sv.Session.SessionID), c.SendMessageRequest{Mutation: newOp(), Recipient: c.Address{Kind: c.AddrOwner, ID: "owner"}, Body: c.MessageBody{Text: "status?"}})
	s.must(err, "message to owner")
	att, err := s.api.ListAttention(s.owner(), c.ListAttentionRequest{})
	s.must(err, "attention")
	var id string
	for _, it := range att.Items {
		if it.Object.ID == m.Envelope.MessageID {
			id = it.AttentionID
		}
	}
	if id == "" {
		t.Fatal("owner message raised no attention item")
	}
	it, err := s.api.GetAttention(s.owner(), c.GetRequest{ID: id})
	s.must(err, "get attention")
	if it.AttentionID != id || it.State != c.AttnOpen || it.DeepLink != c.DeepLink(it.Object) {
		t.Fatalf("get: %+v", it)
	}
	_, err = s.api.UpdateAttention(s.owner(), c.UpdateAttentionRequest{Mutation: newOp(), AttentionID: id, State: c.AttnHandled})
	s.must(err, "handle")
	it, err = s.api.GetAttention(s.owner(), c.GetRequest{ID: id})
	s.must(err, "get handled")
	if it.State != c.AttnHandled {
		t.Fatalf("handled item state %s", it.State)
	}
	_, err = s.api.GetAttention(s.owner(), c.GetRequest{ID: c.NewID(c.PrefixAttention)})
	s.wantCode(err, c.ErrNotFound, "missing item")
	_, err = s.api.GetAttention(s.session(sv.Session.SessionID), c.GetRequest{ID: id})
	s.wantCode(err, c.ErrForbidden, "session reads owner inbox")
}

// v1.2: artifacts.preview_url mints a short-lived link bound to a version.
func testPreviewURL(t *testing.T, s *suite) {
	r, err := s.publish(s.owner(), "", []byte("<p>synthetic preview</p>"), "")
	s.must(err, "publish")
	vid := r.Version.VersionID
	pu, err := s.api.MintPreviewURL(s.owner(), c.MintPreviewRequest{VersionID: vid})
	s.must(err, "mint preview")
	if !strings.Contains(pu.URL, "/preview/"+vid+"/") || strings.HasSuffix(pu.URL, "{token}") {
		t.Fatalf("preview url %q", pu.URL)
	}
	// Targets may run a fixed clock: the link must outlive the version's
	// creation time, not wall time.
	if !pu.ExpiresAt.After(r.Version.CreatedAt) {
		t.Fatalf("expires_at %v", pu.ExpiresAt)
	}
	again, err := s.api.MintPreviewURL(s.owner(), c.MintPreviewRequest{VersionID: vid, TTLSeconds: 60})
	s.must(err, "mint again")
	if again.URL == pu.URL {
		t.Fatal("two mints returned the same one-time link")
	}
	_, err = s.api.MintPreviewURL(s.owner(), c.MintPreviewRequest{VersionID: c.NewID(c.PrefixVersion)})
	s.wantCode(err, c.ErrNotFound, "missing version")
	_, err = s.api.MintPreviewURL(s.owner(), c.MintPreviewRequest{VersionID: vid, TTLSeconds: -1})
	s.wantCode(err, c.ErrInvalid, "negative ttl")
}

// v1.2: connections.check_grant reflects grants and revocations made
// through connection.grant/revoke node actions, with a generation that
// advances on each, and is scoped by caller.
func testConnectionGrantCheck(t *testing.T, s *suite) {
	rep := need[ConnectionReporter](t, s.tg, "ConnectionReporter")
	conn := c.Connection{ConnectionID: c.NewID(c.PrefixConnection), Name: "conformance-" + strings.ToLower(c.NewID("x")), Service: "github", Modes: []string{"remote_invoke"}}
	s.must(rep.ReportConnection(s.ctx, conn), "report connection")
	sv := s.start("grant-user")
	other := s.start("grant-other")
	sid := sv.Session.SessionID
	node := s.tg.As(s.ctx, c.Principal{Kind: c.PrincipalNode, ID: s.tg.NodeID})
	check := func(ctxName string, want bool) c.GrantDecision {
		t.Helper()
		d, err := s.api.CheckConnectionGrant(node, c.CheckGrantRequest{ConnectionID: conn.ConnectionID, SessionID: sid})
		s.must(err, "check as node ("+ctxName+")")
		if d.Allowed != want || d.Reason == "" {
			t.Fatalf("%s: decision %+v, want allowed=%v", ctxName, d, want)
		}
		return d
	}
	d0 := check("before grant", false)

	action := func(name string) {
		t.Helper()
		params, _ := json.Marshal(c.ConnectionGrantParams{ConnectionID: conn.ConnectionID, SessionID: sid})
		res, err := s.api.InvokeNodeAction(s.owner(), c.NodeActionRequest{Mutation: newOp(), NodeID: s.tg.NodeID, Action: name, Params: params, TimeoutSeconds: 5})
		s.must(err, name)
		if res.Error != nil {
			t.Fatalf("%s: %v", name, res.Error)
		}
	}
	action(c.ActionConnGrant)
	d1 := check("after grant", true)
	if d1.Generation <= d0.Generation {
		t.Fatalf("generation did not advance on grant: %d -> %d", d0.Generation, d1.Generation)
	}
	// The session may check itself; another session may not check it.
	self, err := s.api.CheckConnectionGrant(s.session(sid), c.CheckGrantRequest{ConnectionID: conn.ConnectionID, SessionID: sid})
	s.must(err, "check as self")
	if !self.Allowed {
		t.Fatalf("self check %+v", self)
	}
	_, err = s.api.CheckConnectionGrant(s.session(other.Session.SessionID), c.CheckGrantRequest{ConnectionID: conn.ConnectionID, SessionID: sid})
	s.wantCode(err, c.ErrForbidden, "session checks another session")
	od, err := s.api.CheckConnectionGrant(node, c.CheckGrantRequest{ConnectionID: conn.ConnectionID, SessionID: other.Session.SessionID})
	s.must(err, "check other session")
	if od.Allowed {
		t.Fatalf("grant leaked to another session: %+v", od)
	}
	action(c.ActionConnRevoke)
	d2 := check("after revoke", false)
	if d2.Generation <= d1.Generation {
		t.Fatalf("generation did not advance on revoke: %d -> %d", d1.Generation, d2.Generation)
	}
	unknown, err := s.api.CheckConnectionGrant(node, c.CheckGrantRequest{ConnectionID: c.NewID(c.PrefixConnection), SessionID: sid})
	s.must(err, "unknown connection")
	if unknown.Allowed {
		t.Fatal("unknown connection allowed")
	}
	_, err = s.api.CheckConnectionGrant(node, c.CheckGrantRequest{ConnectionID: conn.ConnectionID})
	s.wantCode(err, c.ErrInvalid, "missing session_id")
}
