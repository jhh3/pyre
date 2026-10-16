//go:build !windows

package security

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/diagnostics"
)

// TestLeakScannerControl proves the at-rest scan is not vacuous: a token
// written to the database is found in the file or WAL.
func TestLeakScannerControl(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	if _, err := f.db.Exec(`CREATE TABLE leak_control(v TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`INSERT INTO leak_control(v) VALUES(?)`, "oops "+f.owner.Token); err != nil {
		t.Fatal(err)
	}
	found, err := f.scanner().ScanSQLite(f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	hit := false
	for _, x := range diagnostics.Secrets(found) {
		if strings.HasPrefix(x.Rule, diagnostics.RuleKnown) {
			hit = true
		}
	}
	if !hit {
		t.Fatalf("control token not found in %s(+wal): %v", f.dbPath, found)
	}
}

// TestNodePrincipalCannotActAsOwner: a node credential cannot perform owner
// operations over the API, and the real fleet store refuses it directly.
func TestNodePrincipalCannotActAsOwner(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	nodeID, nodeTok := f.enrollNode("not-an-owner")
	for op, body := range map[string]any{
		contracts.OpCreateEnrollment: contracts.EnrollmentRequest{Mutation: op(), Name: "evil"},
		contracts.OpRevokeNode:       contracts.RevokeRequest{Mutation: op(), ID: nodeID},
		contracts.OpCreatePairing:    contracts.PairingRequest{Mutation: op(), ClientName: "evil", Kind: "cli"},
		contracts.OpRevokeClient:     contracts.RevokeRequest{Mutation: op(), ID: f.owner.ClientID},
		contracts.OpAnswerApproval:   contracts.AnswerApprovalRequest{Mutation: op(), ApprovalID: "apr_x", RunID: "run_x", OptionID: "allow"},
		contracts.OpSendMessage:      contracts.SendMessageRequest{Mutation: op(), Recipient: contracts.Address{Kind: contracts.AddrOwner, ID: "owner"}, Body: contracts.MessageBody{Text: "x"}},
	} {
		r := f.rpc(nodeTok, op, body)
		if r.status != http.StatusForbidden {
			t.Errorf("node token %s: status %d %s (want 403)", op, r.status, r.body)
		}
	}
	nodeCtx := contracts.WithPrincipal(context.Background(), contracts.Principal{Kind: contracts.PrincipalNode, ID: nodeID})
	if _, err := f.fleet.CreateEnrollment(nodeCtx, contracts.EnrollmentRequest{Mutation: op(), Name: "evil"}); !contracts.IsCode(err, contracts.ErrForbidden) {
		t.Errorf("fleet.CreateEnrollment as node = %v", err)
	}
	if _, err := f.fleet.RevokeClient(nodeCtx, contracts.RevokeRequest{Mutation: op(), ID: f.owner.ClientID}); !contracts.IsCode(err, contracts.ErrForbidden) {
		t.Errorf("fleet.RevokeClient as node = %v", err)
	}
	f.assertNoLeaks(nil)
}

func dialNode(t *testing.T, f *fixture, token string, hello contracts.NodeHello) (contracts.NodeFrame, int, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	hdr := http.Header{}
	if token != "" {
		hdr.Set("Authorization", "Bearer "+token)
	}
	ws, resp, err := websocket.Dial(ctx, "wss"+strings.TrimPrefix(f.srv.URL, "https")+contracts.PathNodeConnect, &websocket.DialOptions{HTTPClient: f.httpClient(), HTTPHeader: hdr})
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		return contracts.NodeFrame{}, status, err
	}
	defer ws.CloseNow()
	hello.Protocol, hello.MinProtocol = contracts.ProtocolVersion, contracts.MinProtocolVersion
	b, _ := json.Marshal(contracts.NodeFrame{Type: contracts.FrameHello, Hello: &hello, At: time.Now()})
	if err := ws.Write(ctx, websocket.MessageText, b); err != nil {
		return contracts.NodeFrame{}, 101, err
	}
	_, data, err := ws.Read(ctx)
	if err != nil {
		return contracts.NodeFrame{}, 101, err
	}
	var fr contracts.NodeFrame
	_ = json.Unmarshal(data, &fr)
	return fr, 101, nil
}

// TestNodeCannotSpeakForAnotherNode: the node channel binds the connection
// to the credential's node. A hello claiming another node ID is refused,
// owner and session credentials cannot open the node channel, and a revoked
// node is cut off.
func TestNodeCannotSpeakForAnotherNode(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	nodeA, tokA := f.enrollNode("node-a")
	nodeB, _ := f.enrollNode("node-b")
	_, sessTok := f.startSession("not-a-node")

	fr, _, err := dialNode(t, f, tokA, contracts.NodeHello{NodeID: nodeB, Hostname: "evil"})
	if err != nil || fr.Type != contracts.FrameError || fr.Error == nil || fr.Error.Code != contracts.ErrForbidden {
		t.Fatalf("hello as another node: frame %+v err %v (want forbidden error frame)", fr, err)
	}
	b, err := f.fleet.GetNode(ownerCtx(), contracts.GetRequest{ID: nodeB})
	if err != nil {
		t.Fatal(err)
	}
	if b.Connection.State != contracts.ConnNever || b.Hostname == "evil" {
		t.Fatalf("node B's record changed by node A: %+v host=%q", b.Connection, b.Hostname)
	}
	for name, tok := range map[string]string{"owner": f.owner.Token, "session": sessTok, "none": ""} {
		_, status, err := dialNode(t, f, tok, contracts.NodeHello{NodeID: nodeA})
		if err == nil || (status != http.StatusForbidden && status != http.StatusUnauthorized) {
			t.Errorf("%s credential on the node channel: status %d err %v", name, status, err)
		}
	}
	// Legitimate connection, then revocation closes it.
	fr, _, err = dialNode(t, f, tokA, contracts.NodeHello{NodeID: nodeA, Hostname: "a"})
	if err != nil || fr.Type != contracts.FrameWelcome {
		t.Fatalf("legit hello: %+v %v", fr, err)
	}
	if _, err := f.fleet.RevokeNode(contracts.WithPrincipal(context.Background(), contracts.Principal{Kind: contracts.PrincipalOwner, ID: f.owner.ClientID}),
		contracts.RevokeRequest{Mutation: op(), ID: nodeA, Reason: "test"}); err != nil {
		t.Fatal(err)
	}
	_, status, err := dialNode(t, f, tokA, contracts.NodeHello{NodeID: nodeA})
	if err == nil || status != http.StatusUnauthorized {
		t.Fatalf("revoked node reconnect: status %d err %v", status, err)
	}
	f.assertNoLeaks(nil)
}

// TestSessionCannotSpoofSenderOrAnswerApprovals: identity comes from the
// credential, never the body. A "sender" field is rejected by strict
// decoding; stored messages carry the authenticated session; a session can
// neither answer an approval nor acknowledge someone else's message.
func TestSessionCannotSpoofSenderOrAnswerApprovals(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	alice, aliceTok := f.startSession("alice")
	bob, bobTok := f.startSession("bob")

	spoof := `{"operation_id":"` + contracts.NewID(contracts.PrefixOperation) + `","recipient":{"kind":"session","id":"` + bob.Session.SessionID +
		`"},"body":{"text":"hi"},"sender":{"kind":"owner","id":"` + f.owner.ClientID + `"}}`
	if r := f.rpc(aliceTok, contracts.OpSendMessage, spoof); r.status != http.StatusBadRequest || r.code() != contracts.ErrInvalid {
		t.Fatalf("body-claimed sender: %d %s", r.status, r.body)
	}
	r := f.rpc(aliceTok, contracts.OpSendMessage, contracts.SendMessageRequest{Mutation: op(), Recipient: contracts.Address{Kind: contracts.AddrSession, ID: bob.Session.SessionID}, Body: contracts.MessageBody{Text: "hello bob"}})
	if r.status != 200 {
		t.Fatalf("send: %d %s", r.status, r.body)
	}
	var mv contracts.MessageView
	mustJSON(t, r.body, &mv)
	if mv.Envelope.Sender.Kind != contracts.PrincipalSession || mv.Envelope.Sender.ID != alice.Session.SessionID {
		t.Fatalf("stored sender = %+v", mv.Envelope.Sender)
	}
	// Alice cannot acknowledge a message addressed to Bob (her own).
	if r := f.rpc(aliceTok, contracts.OpAckMessage, contracts.AckMessageRequest{Mutation: op(), MessageID: mv.Envelope.MessageID}); r.status != http.StatusForbidden {
		t.Fatalf("non-recipient ack: %d %s", r.status, r.body)
	}
	if r := f.rpc(bobTok, contracts.OpAckMessage, contracts.AckMessageRequest{Mutation: op(), MessageID: mv.Envelope.MessageID}); r.status != 200 {
		t.Fatalf("recipient ack: %d %s", r.status, r.body)
	}

	ap, err := f.mock.RequestApproval(bob.Run.RunID, mockApproval())
	if err != nil {
		t.Fatal(err)
	}
	for name, tok := range map[string]string{"own-session": bobTok, "other-session": aliceTok} {
		r := f.rpc(tok, contracts.OpAnswerApproval, contracts.AnswerApprovalRequest{Mutation: op(), ApprovalID: ap.ApprovalID, RunID: bob.Run.RunID, OptionID: ap.Options[0].ID})
		if r.status != http.StatusForbidden {
			t.Errorf("%s answering an approval: %d %s", name, r.status, r.body)
		}
	}
	got, _ := f.mock.GetApproval(ownerCtx(), contracts.GetRequest{ID: ap.ApprovalID})
	if got.State != contracts.ApprovalPending {
		t.Fatalf("approval state after refused answers = %s", got.State)
	}
	// Owner may answer, but only for the approval's own run.
	if r := f.rpc(f.owner.Token, contracts.OpAnswerApproval, contracts.AnswerApprovalRequest{Mutation: op(), ApprovalID: ap.ApprovalID, RunID: alice.Run.RunID, OptionID: ap.Options[0].ID}); r.status != http.StatusConflict {
		t.Fatalf("owner answer for the wrong run: %d %s", r.status, r.body)
	}
	if r := f.rpc(f.owner.Token, contracts.OpAnswerApproval, contracts.AnswerApprovalRequest{Mutation: op(), ApprovalID: ap.ApprovalID, RunID: bob.Run.RunID, OptionID: ap.Options[0].ID}); r.status != 200 {
		t.Fatalf("owner answer: %d %s", r.status, r.body)
	}
	f.assertNoLeaks(nil)
}

// TestSessionTokenRevokedWithSession: a session's credential stops working
// once revoked (archive/revoke path of TokenIssuer).
func TestSessionTokenRevokedWithSession(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	s, tok := f.startSession("short-lived")
	if r := f.rpc(tok, contracts.OpWhoAmI, nil); r.status != 200 {
		t.Fatalf("session whoami: %d", r.status)
	}
	if err := f.auth.RevokeSessionTokens(context.Background(), s.Session.SessionID); err != nil {
		t.Fatal(err)
	}
	if r := f.rpc(tok, contracts.OpWhoAmI, nil); r.status != http.StatusUnauthorized {
		t.Fatalf("revoked session token: %d", r.status)
	}
}
