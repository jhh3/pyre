package usage

import "testing"

// One rule for an unreported window, shared with the accounts near-limit
// check: it does not count when the other window is reported.
func TestPressureIgnoresUnreportedWindow(t *testing.T) {
	weeklyOnly := Headroom{Weekly: 20, WeekOK: true}
	both := Headroom{FiveHour: 10, FiveOK: true, Weekly: 40, WeekOK: true}
	none := Headroom{}
	if weeklyOnly.Pressure() != 20 || both.Pressure() != 40 || none.Pressure() != 100 {
		t.Fatalf("pressure %d %d %d", weeklyOnly.Pressure(), both.Pressure(), none.Pressure())
	}
	if weeklyOnly.Compare(both) >= 0 || both.Compare(none) >= 0 {
		t.Fatal("a weekly-only account at 20% must outrank one at 40%, and both outrank no windows")
	}
}
