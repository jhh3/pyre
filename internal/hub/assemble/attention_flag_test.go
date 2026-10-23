package assemble

import (
	"context"
	"testing"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
)

// W-attn end to end through the assembled hub (real attention store and
// deriver, simulated node). Live case 2026-10-06: a session's message to
// the owner kept it "! needs you" after the owner typed into the session.
func TestSessionFlagEndToEnd(t *testing.T) {
	t0 := time.Now()
	st, n := newStack(t)
	t.Logf("stack at %v", time.Since(t0))
	bg := context.Background()
	owner := c.WithPrincipal(bg, ownerP)
	op := func() c.Mutation { return c.Mutation{OperationID: c.NewID(c.PrefixOperation)} }
	resp, err := st.Hub.StartSession(owner, c.StartSessionRequest{Mutation: op(), NodeID: n.nodeID, Harness: c.HarnessMock, Cwd: "/tmp/synth", Title: "atlas-wt-retry"})
	if err != nil {
		t.Fatal(err)
	}
	n.idle()
	sid := resp.Session.Session.SessionID
	var runID string
	waitFor(t, "run", func() bool {
		sv, err := st.Hub.GetSession(owner, c.GetRequest{ID: sid})
		if err == nil && sv.Run != nil {
			runID = sv.Run.RunID
		}
		return runID != ""
	})
	sess := c.WithPrincipal(bg, c.Principal{Kind: c.PrincipalSession, ID: sid})
	send := func() c.MessageView {
		t.Helper()
		m, err := st.Hub.SendMessage(sess, c.SendMessageRequest{Mutation: op(), Recipient: c.Address{Kind: c.AddrOwner, ID: "owner"}, Body: c.MessageBody{Text: "synthetic: watermark done"}, RequestAck: true})
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	flag := func() *c.SessionFlag {
		t.Helper()
		sv, err := st.Hub.GetSession(owner, c.GetRequest{ID: sid})
		if err != nil {
			t.Fatal(err)
		}
		return sv.Flag
	}
	cleared := func(what string) {
		t.Helper()
		waitFor(t, what, func() bool { return flag() == nil })
	}
	isMessage := func(what string) {
		t.Helper()
		if f := flag(); f == nil || f.Level != c.FlagMessage || f.Reason != "message" {
			t.Fatalf("%s: flag = %+v", what, f)
		}
	}

	// A message for the owner is informational and named by session title.
	send()
	f := flag()
	if f == nil || f.Level != c.FlagMessage || f.Text != "Message from atlas-wt-retry" {
		t.Fatalf("message flag = %+v", f)
	}
	page, err := st.Hub.ListAttention(owner, c.ListAttentionRequest{SessionID: sid})
	if err != nil || len(page.Items) != 1 || page.Items[0].Title != "Message from atlas-wt-retry" {
		t.Fatalf("inbox title: %+v %v", page.Items, err)
	}
	// Renames apply at render time.
	nt := "watermark v2"
	if _, err := st.Hub.UpdateSession(owner, c.UpdateSessionRequest{Mutation: op(), SessionID: sid, Title: &nt}); err != nil {
		t.Fatal(err)
	}
	if it, err := st.Hub.GetAttention(owner, c.GetRequest{ID: page.Items[0].AttentionID}); err != nil || it.Title != "Message from watermark v2" {
		t.Fatalf("renamed title: %q %v", it.Title, err)
	}

	t.Logf("step 1 at %v", time.Since(t0))
	// 1. The owner taking the input lease (typing in the embedded pane).
	l, err := st.Hub.AcquireLease(owner, c.LeaseRequest{Mutation: op(), RunID: runID, TTLSeconds: 60})
	if err != nil {
		t.Fatal(err)
	}
	cleared("lease clears the message")
	if _, err := st.Hub.ReleaseLease(owner, c.LeaseReleaseRequest{Mutation: op(), LeaseID: l.LeaseID}); err != nil {
		t.Fatal(err)
	}

	t.Logf("step 2 at %v", time.Since(t0))
	// 2. Typing never answers an approval; answering it does.
	send()
	ap, err := n.RequestApproval(bg, runID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if f := flag(); f == nil || f.Level != c.FlagNeedsYou || f.Reason != "approval" || f.Messages != 1 {
		t.Fatalf("approval flag = %+v", f)
	}
	if err := st.Hub.TerminalInputAllowed(owner, ownerP, runID); err != nil { // 3. raw terminal input
		t.Fatal(err)
	}
	waitFor(t, "typing answers the message, not the approval", func() bool {
		f := flag()
		return f != nil && f.Reason == "approval" && f.Messages == 0
	})
	if err := n.ResolveApprovalNatively(bg, ap.ApprovalID, "allow"); err != nil {
		t.Fatal(err)
	}
	cleared("answered approval")

	t.Logf("step 4 at %v", time.Since(t0))
	// 4. The owner messaging the session.
	send()
	isMessage("before owner message")
	if _, err := st.Hub.SendMessage(owner, c.SendMessageRequest{Mutation: op(), Recipient: c.Address{Kind: c.AddrSession, ID: sid}, Body: c.MessageBody{Text: "synthetic: thanks"}}); err != nil {
		t.Fatal(err)
	}
	cleared("owner message")

	t.Logf("step 5 at %v", time.Since(t0))
	// 5. The owner viewing the message; the session reading its own does not.
	m := send()
	if _, err := st.Hub.GetMessage(sess, c.GetRequest{ID: m.Envelope.MessageID}); err != nil {
		t.Fatal(err)
	}
	isMessage("session read its own message")
	if _, err := st.Hub.GetMessage(owner, c.GetRequest{ID: m.Envelope.MessageID}); err != nil {
		t.Fatal(err)
	}
	cleared("owner viewed the message")
	// Items are handled, never deleted.
	all, err := st.Hub.ListAttention(owner, c.ListAttentionRequest{SessionID: sid, States: []c.AttentionState{c.AttnHandled}})
	if err != nil || len(all.Items) == 0 {
		t.Fatalf("handled items: %d %v", len(all.Items), err)
	}

	t.Logf("step 6 at %v", time.Since(t0))
	// 6. The session flags itself, clears it, flags again; the run ending
	// withdraws it.
	it, err := st.Hub.FlagAttention(sess, c.FlagAttentionRequest{Mutation: op(), Text: "pick a schema: A or B"})
	if err != nil || it.Kind != c.AttnFlag {
		t.Fatalf("flag: %+v %v", it, err)
	}
	if f := flag(); f == nil || f.Level != c.FlagNeedsYou || f.Reason != "asked" || f.Text != "pick a schema: A or B" {
		t.Fatalf("asked flag = %+v", f)
	}
	other := c.WithPrincipal(bg, c.Principal{Kind: c.PrincipalSession, ID: c.NewID(c.PrefixSession)})
	if _, err := st.Hub.FlagAttention(other, c.FlagAttentionRequest{Mutation: op(), SessionID: sid, Text: "x"}); !c.IsCode(err, c.ErrForbidden) {
		t.Fatalf("another session flagged it: %v", err)
	}
	if _, err := st.Hub.FlagAttention(sess, c.FlagAttentionRequest{Mutation: op(), Clear: true}); err != nil {
		t.Fatal(err)
	}
	cleared("session cleared its flag")
	if _, err := st.Hub.FlagAttention(sess, c.FlagAttentionRequest{Mutation: op(), Text: "still need you"}); err != nil {
		t.Fatal(err)
	}
	if err := n.SetRunState(bg, runID, c.RunExited); err != nil {
		t.Fatal(err)
	}
	cleared("run ended")
}
