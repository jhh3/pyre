package tui_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/tui"
	"github.com/jhh3/pyre/internal/tui/tuitest"
)

// Session attention flags (W-attn): no Inbox view; a session with only a
// message for you reads "✉ message", never "! needs you", and the message
// shows in the session's own view.

const pairing = "ses_synth-leased" // "pairing session": idle, no flag in the fixture

// sendToOwner has a session message the owner (synthetic text), asking
// for his acknowledgement: a report that asks nothing is an update, which
// flags nothing (internal/messaging/ownermail).
func sendToOwner(t *testing.T, api contracts.API, sid, text string) contracts.MessageView {
	t.Helper()
	ctx := contracts.WithPrincipal(context.Background(), contracts.Principal{Kind: contracts.PrincipalSession, ID: sid})
	mv, err := api.SendMessage(ctx, contracts.SendMessageRequest{Mutation: contracts.Mutation{OperationID: contracts.NewID(contracts.PrefixOperation)},
		Recipient: contracts.Address{Kind: contracts.AddrOwner, ID: "owner"}, Body: contracts.MessageBody{Text: text}, RequestAck: true})
	if err != nil {
		t.Fatal(err)
	}
	return mv
}

// startWatched is start with the mock's event stream, so flags change
// live as they do against a hub.
func startWatched(t *testing.T, w, ht int) (*tuitest.Harness, contracts.API) {
	t.Helper()
	b, clk := tuitest.NewMock(t)
	opts := tuitest.Options()
	opts.Clock = clk
	opts.Watch = tui.PollWatcher(b)
	h := tuitest.New(t, b, opts, w, ht)
	h.WaitFor("initial sessions", 5*sec, func() bool { return h.M.Loaded("sessions") && h.M.Loaded("nodes") && h.M.Loaded("attention") })
	h.Settle()
	return h, b
}

func rowOf(view, title string) string {
	for _, l := range strings.Split(view, "\n") {
		if strings.Contains(l, title) && strings.Contains(l, "│") {
			return l[:strings.Index(l, "│")]
		}
	}
	return ""
}

func TestMessageForYouIsNotNeedsYou(t *testing.T) {
	h, api := startWatched(t, 160, 34)
	sendToOwner(t, api, pairing, "Synthetic: the build is green, ready when you are.\nsecond line not shown")
	h.WaitFor("message flag", 5*sec, func() bool { return strings.Contains(rowOf(h.View(), "pairing session"), "✉ message") })
	if row := rowOf(h.View(), "pairing session"); strings.Contains(row, "needs you") {
		t.Fatalf("a message is informational, not needs you: %q", row)
	}
	if !h.Contains("· ✉ 1 message") {
		t.Fatalf("the strip counts the message:\n%s", h.View())
	}
	head := strings.Split(h.View(), "\n")[0]
	if !strings.Contains(head, "S Sessions 3") {
		t.Fatalf("the header counts sessions that need you (a message does not): %s", head)
	}
	// The message shows in the session's own view (no GetMessage: listing
	// only, so browsing does not count as reading it).
	selectSession(t, h, pairing)
	h.WaitFor("message line", 5*sec, func() bool {
		return h.Contains("✉ message for you: Synthetic: the build is green")
	})
	if h.Contains("second line not shown") {
		t.Fatal("only the first line of the body shows")
	}
	h.WaitFor("preview", 5*sec, func() bool { return h.M.Loaded("preview:"+pairing) && !h.Contains("loading preview") })
	h.Settle()
	golden(t, "session-message-160", h.View())

	// m replies threaded to it; esc leaves the composer.
	h.Key("m")
	if !h.M.OverlayOpen() || !h.Contains("Synthetic: the build is green") {
		t.Fatalf("m should open the composer replying to the message:\n%s", h.View())
	}
	h.Key("esc")
	// d dismisses it (acknowledge, no reply): the flag and the line go.
	h.Key("d")
	h.WaitFor("message dismissed", 5*sec, func() bool {
		return !h.Contains("message for you:") && !strings.Contains(rowOf(h.View(), "pairing session"), "✉ message")
	})
}

// A report that asks nothing (an update) flags nothing: no "✉ message",
// no message line (owner report 2026-10-19).
func TestUpdateToOwnerFlagsNothing(t *testing.T) {
	h, api := startWatched(t, 160, 34)
	ctx := contracts.WithPrincipal(context.Background(), contracts.Principal{Kind: contracts.PrincipalSession, ID: pairing})
	mv, err := api.SendMessage(ctx, contracts.SendMessageRequest{Mutation: contracts.Mutation{OperationID: contracts.NewID(contracts.PrefixOperation)},
		Recipient: contracts.Address{Kind: contracts.AddrOwner, ID: "owner"}, Body: contracts.MessageBody{Text: "Synthetic digest: two PRs merged. Nothing urgent."}})
	if err != nil || mv.Status.AckRef != contracts.AckRefUpdate {
		t.Fatalf("update: %v %+v", err, mv.Status)
	}
	h.Settle()
	selectSession(t, h, pairing)
	h.Settle()
	if strings.Contains(rowOf(h.View(), "pairing session"), "✉ message") || h.Contains("message for you:") {
		t.Fatalf("an update flagged the session:\n%s", h.View())
	}
}

func TestStripNothingNeedsYouButAMessage(t *testing.T) {
	h, api := startWatched(t, 100, 30)
	sendToOwner(t, api, pairing, "Synthetic: FYI only.")
	h.Key("/")
	h.Type("pairing")
	h.Key("enter")
	h.WaitFor("strip", 5*sec, func() bool { return h.Contains("✓ Nothing needs you · ✉ 1 message") })
	// The owner's messages load with the Sessions view (the inbox panel
	// and the session's message line read them).
	h.WaitFor("message line", 5*sec, func() bool { return h.Contains("✉ message for you: ") })
	h.Settle()
	golden(t, "sessions-message-only-100", h.View())
}

func TestNeedsYouListLabelAndStrip(t *testing.T) {
	h, _ := start(t, 100, 30, nil, nil)
	for _, title := range []string{"worker", "scratch (standalone)"} {
		if row := rowOf(h.View(), title); !strings.Contains(row, "! needs you") {
			t.Errorf("%s should read needs you: %q", title, row)
		}
	}
	v := h.View()
	if !strings.Contains(v, " Needs you ") || strings.Contains(v, "I inbox") {
		t.Fatalf("strip:\n%s", v)
	}
}

// Owner request 2026-10-19 ("why are a few sessions stuck with needs you
// state??"): d on a session dismisses its own flag (or "waiting on you"),
// and then, after a confirmation, its open asks (no option, no default).
func TestDismissFlagThenAsks(t *testing.T) {
	h, api := startWatched(t, 160, 34)
	if _, err := api.FlagAttention(sessCtx(pairing), contracts.FlagAttentionRequest{Mutation: contracts.Mutation{OperationID: contracts.NewID(contracts.PrefixOperation)},
		Text: "Synthetic: need the staging key"}); err != nil {
		t.Fatal(err)
	}
	a := ask(t, api, pairing, "synthetic-key-rotation", "Rotate the synthetic staging key?", "The synthetic key is old.", "a", false, 0, "Rotate", "Keep")
	h.WaitFor("needs you", 5*sec, func() bool { return strings.Contains(rowOf(h.View(), "pairing session"), "needs you") })
	selectSession(t, h, pairing)
	// The flag shows the ask (it outranks the session's own flag): the first
	// d dismisses it, after a confirmation.
	h.WaitFor("ask line with keys", 5*sec, func() bool { return h.Contains("! Rotate the synthetic staging key? — m reply · d dismiss") })
	pressUntil(t, h, "d", "confirm", func() bool {
		return h.M.OverlayOpen() && h.Contains("Dismiss 1 open ask from") && h.Contains("Rotate the synthetic staging key?")
	})
	h.Key("y")
	h.WaitFor("ask dismissed", 5*sec, func() bool {
		got, err := api.(contracts.AsksAPI).GetAsk(context.Background(), contracts.GetRequest{ID: a.AskID})
		return err == nil && got.State == contracts.AskWithdrawn
	})
	// Then the session's own flag shows; d marks it handled.
	h.WaitFor("flag line with keys", 5*sec, func() bool { return h.Contains("! Synthetic: need the staging key — m reply · d dismiss") })
	h.Key("d")
	h.WaitFor("flag dismissed", 5*sec, func() bool { return h.Contains("dismissed its flag") })
	h.WaitFor("nothing left", 5*sec, func() bool {
		return !h.Contains("d dismiss") && !strings.Contains(rowOf(h.View(), "› "), "needs you") && !h.Contains("● 1 ask")
	})
	pressUntil(t, h, "d", "nothing to dismiss", func() bool { return h.Contains("has nothing for you to dismiss") })
}

// pressUntil presses key until cond holds (a cache the key reads may load
// a moment after the screen shows what it acts on).
func pressUntil(t *testing.T, h *tuitest.Harness, key, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 10; i++ {
		h.Key(key)
		h.Settle()
		if cond() {
			return
		}
	}
	h.WaitFor(what, 5*sec, cond)
}
