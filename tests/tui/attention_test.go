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

// sendToOwner has a session message the owner (synthetic text).
func sendToOwner(t *testing.T, api contracts.API, sid, text string) contracts.MessageView {
	t.Helper()
	ctx := contracts.WithPrincipal(context.Background(), contracts.Principal{Kind: contracts.PrincipalSession, ID: sid})
	mv, err := api.SendMessage(ctx, contracts.SendMessageRequest{Mutation: contracts.Mutation{OperationID: contracts.NewID(contracts.PrefixOperation)},
		Recipient: contracts.Address{Kind: contracts.AddrOwner, ID: "owner"}, Mode: contracts.ModeInfo, Body: contracts.MessageBody{Text: text}})
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

func TestStripNothingNeedsYouButAMessage(t *testing.T) {
	h, api := startWatched(t, 100, 30)
	sendToOwner(t, api, pairing, "Synthetic: FYI only.")
	h.Key("/")
	h.Type("pairing")
	h.Key("enter")
	h.WaitFor("strip", 5*sec, func() bool { return h.Contains("✓ Nothing needs you · ✉ 1 message") })
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
