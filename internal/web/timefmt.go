package web

import (
	"time"
)

// pacific is where the owner reads times (America/Los_Angeles); the server
// may run in another zone. Falls back to a fixed -08:00 zone when the
// system has no tz database.
var pacific = func() *time.Location {
	if l, err := time.LoadLocation("America/Los_Angeles"); err == nil {
		return l
	}
	return time.FixedZone("PT", -8*3600)
}()

// clock formats t for a delivery line: "9:41 AM" today, "Yesterday 4:10 PM",
// "Oct 12 4:10 PM" this year, "Oct 12 2025 4:10 PM" before.
func clock(t, now time.Time) string {
	if t.IsZero() {
		return ""
	}
	t, now = t.In(pacific), now.In(pacific)
	ty, tm, td := t.Date()
	ny, nm, nd := now.Date()
	switch {
	case ty == ny && tm == nm && td == nd:
		return t.Format("3:04 PM")
	case sameDay(t, now.AddDate(0, 0, -1)):
		return "Yesterday " + t.Format("3:04 PM")
	case ty == ny:
		return t.Format("Jan 2 3:04 PM")
	}
	return t.Format("Jan 2 2006 3:04 PM")
}

func sameDay(a, b time.Time) bool {
	ay, am, ad := a.Date()
	by, bm, bd := b.In(a.Location()).Date()
	return ay == by && am == bm && ad == bd
}
