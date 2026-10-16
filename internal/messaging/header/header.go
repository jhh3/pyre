// Package header formats the first line of a Pyre message as a runtime
// sees it:
//
//	[pyre-msg:<id> · sent 3:05 PM PT (8h ago)] from session:<title or id>
//
// The sent time is the node's local time; the age is relative to delivery.
package header

import (
	"fmt"
	"strings"
	"time"
)

// Marker is the opening of a message header: "[pyre-msg:<id>" followed by
// "]" or " · sent …]".
func Marker(id, sent string) string {
	if sent == "" {
		return "[pyre-msg:" + id + "]"
	}
	return "[pyre-msg:" + id + " · " + sent + "]"
}

// HasMarker reports whether text carries the header marker of message id,
// in either form ("[pyre-msg:<id>]" or "[pyre-msg:<id> · …]").
func HasMarker(text, id string) bool {
	return strings.Contains(text, "[pyre-msg:"+id+"]") || strings.Contains(text, "[pyre-msg:"+id+" · ")
}

// Sent renders "sent 3:05 PM PT (8h ago)" in loc; a sent time on another
// local day carries its date ("sent Oct 6, 11:40 PM PT (10h ago)"). A zero
// time renders "".
func Sent(sent, now time.Time, loc *time.Location) string {
	if sent.IsZero() {
		return ""
	}
	if loc == nil {
		loc = time.Local
	}
	t, n := sent.In(loc), now.In(loc)
	when := t.Format("3:04 PM") + " " + zone(t)
	if t.Format("2006-01-02") != n.Format("2006-01-02") {
		when = t.Format("Jan 2") + ", " + when
	}
	return "sent " + when + " (" + Age(now.Sub(sent)) + ")"
}

// Age renders a duration as "just now", "5m ago", "8h ago" or "3d ago".
func Age(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d/time.Hour))
	default:
		return fmt.Sprintf("%dd ago", int(d/(24*time.Hour)))
	}
}

// generic maps daylight/standard abbreviations to the generic zone people
// say ("PT", not "PDT").
var generic = map[string]string{
	"PST": "PT", "PDT": "PT", "MST": "MT", "MDT": "MT", "CST": "CT", "CDT": "CT",
	"EST": "ET", "EDT": "ET", "AKST": "AKT", "AKDT": "AKT", "HST": "HT", "HDT": "HT",
}

func zone(t time.Time) string {
	name, off := t.Zone()
	if g, ok := generic[name]; ok {
		return g
	}
	if name == "" || name[0] == '+' || name[0] == '-' {
		if off == 0 {
			return "UTC"
		}
		return "UTC" + t.Format("-07:00")
	}
	return name
}
