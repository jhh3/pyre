package tui_test

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/client"
	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/httpapi"
	"github.com/jhh3/pyre/internal/runtime/mock"
	"github.com/jhh3/pyre/internal/tui/tuitest"
)

// TestOverHTTPClient drives the TUI through the real HTTP client and the
// event-stream WebSocket (client.WatchEvents), as cmd/pyre will, against
// the mock served by httpapi.
func TestOverHTTPClient(t *testing.T) {
	b, clk := tuitest.NewMock(t)
	srv := httptest.NewServer(httpapi.NewServer(b, httpapi.NewStaticTokens(map[string]contracts.Principal{"dev-token": tuitest.Owner}), httpapi.Options{StreamWait: time.Second}))
	defer srv.Close()
	cl := client.New(srv.URL, "dev-token", client.Options{RetryBase: time.Millisecond})

	opts := tuitest.Options()
	opts.Clock = clk
	opts.Watch = cl.WatchEvents
	h := tuitest.New(t, cl, opts, 100, 30)
	h.WaitFor("sessions over HTTP", 5*sec, func() bool { return h.Contains("coordinator") })
	h.WaitFor("session flags over HTTP", 5*sec, func() bool { return h.Contains("needs you") })
	h.Key("ctrl+p") // the finder lists what needs you
	h.Type("Synthetic HTTP")

	// A live change arrives over the WebSocket and is reflected without a
	// manual refresh.
	run := runOf(t, b, "ses_synth-a")
	if _, err := b.RequestApproval(run, mock.ApprovalSpec{Title: "Synthetic HTTP-path request"}); err != nil {
		t.Fatal(err)
	}
	h.WaitFor("event-driven refresh", 5*sec, func() bool { return h.Contains("Synthetic HTTP-path request") })
	if h.M.Stats().Events == 0 {
		t.Fatal("no events received over the stream")
	}
	// Attach through the client.
	h.Key("esc", "S")
	selectSession(t, h, "ses_synth-a")
	h.Key("F")
	h.WaitFor("attach over HTTP", 5*sec, func() bool { return len(h.Attaches()) == 1 })
}
