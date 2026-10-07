package notifications

import (
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

func TestQuietHoursTZ(t *testing.T) {
	ny := contracts.NotificationPrefs{QuietHoursStart: "22:00", QuietHoursEnd: "07:00", TZ: "America/New_York"}
	day := contracts.NotificationPrefs{QuietHoursStart: "09:00", QuietHoursEnd: "17:30", TZ: "UTC"}
	cases := []struct {
		name string
		p    contracts.NotificationPrefs
		at   time.Time
		want bool
	}{
		// 02:30 UTC = 22:30 EDT (inside, spans midnight).
		{"ny late evening", ny, time.Date(2026, 10, 5, 2, 30, 0, 0, time.UTC), true},
		// 10:59 UTC = 06:59 EDT (inside).
		{"ny early morning", ny, time.Date(2026, 10, 5, 10, 59, 0, 0, time.UTC), true},
		// 11:00 UTC = 07:00 EDT (end is exclusive).
		{"ny end exclusive", ny, time.Date(2026, 10, 5, 11, 0, 0, 0, time.UTC), false},
		// 22:30 UTC = 18:30 EDT (outside although 22:30 in UTC).
		{"ny uses tz not utc", ny, time.Date(2026, 10, 5, 22, 30, 0, 0, time.UTC), false},
		// Winter (EST, UTC-5): 03:00 UTC = 22:00 EST.
		{"ny winter start", ny, time.Date(2026, 1, 15, 3, 0, 0, 0, time.UTC), true},
		{"daytime inside", day, time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC), true},
		{"daytime outside", day, time.Date(2026, 10, 5, 18, 0, 0, 0, time.UTC), false},
		{"equal start/end disables", contracts.NotificationPrefs{QuietHoursStart: "08:00", QuietHoursEnd: "08:00", TZ: "UTC"}, time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC), false},
		{"unset", contracts.NotificationPrefs{}, time.Date(2026, 10, 5, 3, 0, 0, 0, time.UTC), false},
	}
	for _, tc := range cases {
		if got := InQuietHours(tc.p, tc.at); got != tc.want {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}

func TestEvaluate(t *testing.T) {
	prj := "prj_synth-muted"
	other := "prj_synth-other"
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	base := contracts.AttentionItem{Kind: contracts.AttnApproval, Severity: "urgent"}
	p := contracts.NotificationPrefs{MutedProjects: []string{prj}, MinSeverity: "normal", Kinds: map[contracts.AttentionKind]bool{contracts.AttnMessage: false, contracts.AttnApproval: true}}

	check := func(name string, p contracts.NotificationPrefs, it contracts.AttentionItem, want string) {
		t.Helper()
		if got, _ := Evaluate(p, it, now); got != want {
			t.Errorf("%s: got %q want %q", name, got, want)
		}
	}
	check("allowed", p, base, "")
	muted := base
	muted.ProjectID = &prj
	check("muted project", p, muted, StateDisabled)
	unmuted := base
	unmuted.ProjectID = &other
	check("other project", p, unmuted, "")
	msg := base
	msg.Kind = contracts.AttnMessage
	check("kind disabled", p, msg, StateDisabled)
	info := base
	info.Severity = "info"
	check("below min severity", p, info, StateDisabled)
	quiet := contracts.NotificationPrefs{QuietHoursStart: "11:00", QuietHoursEnd: "13:00", TZ: "UTC"}
	check("quiet hours", quiet, base, StateQuietHours)
}

func TestValidatePrefs(t *testing.T) {
	ok := []contracts.NotificationPrefs{
		{},
		{QuietHoursStart: "7:05", QuietHoursEnd: "23:59", TZ: "Europe/Berlin", MinSeverity: "urgent"},
	}
	for i, p := range ok {
		if err := ValidatePrefs(p); err != nil {
			t.Errorf("ok %d: %v", i, err)
		}
	}
	bad := []contracts.NotificationPrefs{
		{QuietHoursEnd: "07:00"},
		{QuietHoursStart: "24:00", QuietHoursEnd: "07:00"},
		{QuietHoursStart: "22:0", QuietHoursEnd: "07:00"},
		{TZ: "Nowhere/Land"},
		{MinSeverity: "low"},
		{Kinds: map[contracts.AttentionKind]bool{"spam": false}},
	}
	for i, p := range bad {
		if err := ValidatePrefs(p); !contracts.IsCode(err, contracts.ErrInvalid) {
			t.Errorf("bad %d accepted: %v", i, err)
		}
	}
}

func TestGenericTitlesAreGeneric(t *testing.T) {
	for _, k := range knownKinds {
		if title := GenericTitle(k); len(title) < 6 || title[:6] != "Pyre: " {
			t.Errorf("%s: %q", k, title)
		}
	}
}
