package cli

import (
	"context"
	"testing"

	c "github.com/jhh3/pyre/internal/contracts"
)

// fakeInbox offers ListMessages, mark_read and child_reports.release and
// records the order of calls.
type fakeInbox struct {
	c.API
	calls []string
}

func (f *fakeInbox) ListMessages(_ context.Context, req c.ListMessagesRequest) (c.Page[c.MessageView], error) {
	f.calls = append(f.calls, "list")
	return c.Page[c.MessageView]{}, nil
}

func (f *fakeInbox) MarkMessagesRead(_ context.Context, req c.MarkMessagesReadRequest) (c.MarkMessagesReadResponse, error) {
	f.calls = append(f.calls, "read")
	return c.MarkMessagesReadResponse{}, nil
}

func (f *fakeInbox) ReleaseChildReports(_ context.Context, req c.ReleaseChildReportsRequest) (c.ReleaseChildReportsResponse, error) {
	f.calls = append(f.calls, "release")
	return c.ReleaseChildReportsResponse{}, nil
}

// A session reading its own inbox first asks the hub for the child reports
// it holds while the session is mid-turn; other listings do not.
func TestInboxReleasesHeldChildReportsFirst(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		p    InboxParams
		self string
		want string
	}{
		{"own inbox", InboxParams{}, "ses_self", "release,list"},
		{"own inbox, long-poll", InboxParams{WaitSeconds: 5}, "ses_self", "release,list"},
		{"own inbox by id", InboxParams{Session: "ses_self"}, "ses_self", "release,list"},
		{"outbox", InboxParams{Box: "outbox"}, "ses_self", "list"},
		{"thread", InboxParams{Thread: "thr_x"}, "ses_self", "list"},
		{"owner client", InboxParams{}, "", "list"},
		{"owner mailbox", InboxParams{Session: "owner"}, "ses_self", "list"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeInbox{}
			o := &Ops{API: f}
			o.Self.SessionID = tc.self
			if _, err := o.Inbox(ctx, tc.p); err != nil {
				t.Fatal(err)
			}
			got := ""
			for _, call := range f.calls {
				if call == "read" {
					continue
				}
				if got != "" {
					got += ","
				}
				got += call
			}
			if got != tc.want {
				t.Fatalf("calls %v, want %s", f.calls, tc.want)
			}
		})
	}
}
