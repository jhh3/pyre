package header

import (
	"testing"
	"time"
)

func TestSent(t *testing.T) {
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Skip(err)
	}
	sent := time.Date(2026, 10, 7, 15, 5, 0, 0, la)
	cases := []struct {
		now  time.Time
		loc  *time.Location
		want string
	}{
		{sent.Add(8*time.Hour + 30*time.Minute), la, "sent 3:05 PM PT (8h ago)"},
		{sent.Add(20 * time.Second), la, "sent 3:05 PM PT (just now)"},
		{sent.Add(-time.Minute), la, "sent 3:05 PM PT (just now)"}, // clock skew
		{sent.Add(42 * time.Minute), la, "sent 3:05 PM PT (42m ago)"},
		{sent.Add(10 * time.Hour), la, "sent Oct 7, 3:05 PM PT (10h ago)"}, // next local day
		{sent.Add(75 * time.Hour), la, "sent Oct 7, 3:05 PM PT (3d ago)"},
		{sent.Add(time.Hour), time.UTC, "sent 10:05 PM UTC (1h ago)"},
		{sent.Add(time.Hour), time.FixedZone("", 5*3600+1800), "sent 3:35 AM UTC+05:30 (1h ago)"},
	}
	for _, c := range cases {
		if got := Sent(sent, c.now, c.loc); got != c.want {
			t.Errorf("Sent(now=%s, %s) = %q, want %q", c.now, c.loc, got, c.want)
		}
	}
	if Sent(time.Time{}, sent, la) != "" {
		t.Error("zero time must render nothing")
	}
}

func TestMarker(t *testing.T) {
	m := Marker("msg_1", "sent 3:05 PM PT (8h ago)")
	if m != "[pyre-msg:msg_1 · sent 3:05 PM PT (8h ago)]" {
		t.Fatal(m)
	}
	if !HasMarker(m+" from owner:x", "msg_1") || !HasMarker("[pyre-msg:msg_1] x", "msg_1") || HasMarker(m, "msg_12") || HasMarker("[pyre-msg:msg_12]", "msg_1") {
		t.Fatal("HasMarker")
	}
	if Marker("msg_1", "") != "[pyre-msg:msg_1]" {
		t.Fatal("bare marker")
	}
}
