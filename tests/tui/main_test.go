package tui_test

import (
	"os"
	"testing"
	"time"
)

// TestMain pins the local time zone. The TUI shows wall-clock times in
// time.Local (the digest's "since 02:00"), and the goldens were recorded
// at UTC-7; CI runners run in UTC. A fixed zone (no DST) keeps every
// golden stable on any machine and in any season.
func TestMain(m *testing.M) {
	time.Local = time.FixedZone("synthetic-UTC-7", -7*60*60)
	os.Exit(m.Run())
}
