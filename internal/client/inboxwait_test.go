package client

import (
	"context"
	"testing"

	c "github.com/jhh3/pyre/internal/contracts"
)

// A hub (or older pyred) without inbox.wait answers 404: the client reports
// it as unsupported so the doorbell falls back to a hub long-poll.
func TestInboxWaitUnsupportedOnHub(t *testing.T) {
	srv := serve(t, &fake{}, false)
	cl := New(srv.URL, "tok", fastOpts())
	_, err := cl.InboxWait(context.Background(), c.InboxWaitRequest{SessionID: "ses_x", WaitSeconds: 1})
	if !c.IsCode(err, c.ErrUnsupported) {
		t.Fatalf("inbox.wait against a hub = %v", err)
	}
}
