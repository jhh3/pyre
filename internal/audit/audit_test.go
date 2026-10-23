package audit

import (
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

func TestSummarizeSwitchAndNotes(t *testing.T) {
	obs := &contracts.AccountObservation{Windows: []contracts.UsageWindow{{Name: "five_hour", UsedPercent: 12}, {Name: "seven_day", UsedPercent: 95.6}}}
	if n := UsageNote(obs); n != "weekly 96%" {
		t.Fatalf("note %q", n)
	}
	if UsageNote(nil) != "" || UsageNote(&contracts.AccountObservation{}) != "" {
		t.Fatal("empty note")
	}
	e := contracts.AuditEntry{Action: contracts.AuditAccountSwitched, SessionTitle: "Credit perf", From: "code", FromNote: "weekly 96%", To: "me", ToNote: "weekly 0%",
		Reason: "usage_limit on acct_x; acct_y eligible"}
	if s := Summarize(e); s != "switched 'Credit perf' from code (weekly 96%) to me (weekly 0%) — limit reached" {
		t.Fatalf("summary %q", s)
	}
	e.NodeName = "server"
	e.Summary = Summarize(e)
	now := time.Date(2026, 10, 6, 14, 30, 0, 0, time.Local)
	e.At = time.Date(2026, 10, 6, 14, 5, 0, 0, time.Local)
	if l := Line(e, now); l != "14:05 server · switched 'Credit perf' from code (weekly 96%) to me (weekly 0%) — limit reached" {
		t.Fatalf("line %q", l)
	}
}

func TestEveryActionRenders(t *testing.T) {
	actions := []string{
		contracts.AuditAccountChosen, contracts.AuditAccountSwitched, contracts.AuditAccountWaiting, contracts.AuditAccountReauthNeeded,
		contracts.AuditAccountReauthed, contracts.AuditAccountCooldown, contracts.AuditAccountCooldownEnded, contracts.AuditAccountTokenAdded,
		contracts.AuditAccountTokenReplaced, contracts.AuditAccountTokenRemoved, contracts.AuditAccountTokenSynced, contracts.AuditAccountLoginAdded,
		contracts.AuditAccountAdded, contracts.AuditAccountFolded, contracts.AuditAccountRenamed, contracts.AuditAccountDisabled, contracts.AuditAccountEnabled, contracts.AuditAccountTierChanged,
		contracts.AuditSessionStarted, contracts.AuditSessionStopped, contracts.AuditSessionInterrupted, contracts.AuditSessionResumed,
		contracts.AuditSessionArchived, contracts.AuditSessionUnarchived, contracts.AuditSessionArchiveScheduled, contracts.AuditSessionArchiveCanceled, contracts.AuditSessionIconSet, contracts.AuditSessionRenamed, contracts.AuditSessionAutoTitled,
		contracts.AuditSessionLeaseTakenOver, contracts.AuditSessionReportedToParent,
		contracts.AuditNodeEnrolled, contracts.AuditNodeRevoked, contracts.AuditNodeConnected, contracts.AuditNodeDisconnected, contracts.AuditNodeUpgraded,
		contracts.AuditNodeUpdateFailed, contracts.AuditHubUpgraded, contracts.AuditNodeTrustAccepted, contracts.AuditNodeExitConfirmed,
	}
	for _, a := range actions {
		e := contracts.AuditEntry{Action: a, SessionTitle: "synth", AccountLabel: "synth-acct", From: "a", To: "b", Actor: contracts.AuditActor{Kind: "owner"}}
		s := Summarize(e)
		if s == "" || s == a {
			t.Errorf("%s renders as %q", a, s)
		}
		if KindOf(a) == "" {
			t.Errorf("%s has no kind", a)
		}
	}
	if KindOf(contracts.AuditNodeTrustAccepted) != contracts.AuditFleet || KindOf(contracts.AuditAccountChosen) != contracts.AuditAccount {
		t.Fatal("kinds")
	}
	if got := By(contracts.AuditActor{Kind: "session", Name: "parent"}); got != " — by session 'parent'" {
		t.Fatalf("by session %q", got)
	}
	if By(contracts.AuditActor{Kind: "pyre"}) != "" {
		t.Fatal("pyre actor suffix")
	}
}

func TestParsing(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	for in, want := range map[string]time.Duration{"24h": 24 * time.Hour, "90m": 90 * time.Minute, "7d": 7 * 24 * time.Hour} {
		got, err := ParseSince(in, now)
		if err != nil || !got.Equal(now.Add(-want)) {
			t.Errorf("%s: %v %v", in, got, err)
		}
	}
	if got, err := ParseSince("", now); err != nil || got != nil {
		t.Fatal("empty since")
	}
	if _, err := ParseSince("soon", now); err == nil {
		t.Fatal("bad since accepted")
	}
	if k, err := ParseKind("Accounts"); err != nil || k != contracts.AuditAccount {
		t.Fatalf("kind %v %v", k, err)
	}
	if _, err := ParseKind("money"); err == nil {
		t.Fatal("bad kind accepted")
	}
	if ShortReason("rate_limited by provider") != "rate limited" || ShortReason("auth_invalid: 401") != "login failed" {
		t.Fatal("short reasons")
	}
	day := time.Date(2026, 10, 6, 9, 0, 0, 0, time.Local)
	if When(day, day.Add(time.Hour)) != "09:00" || When(day.Add(-48*time.Hour), day) != day.Add(-48*time.Hour).Format("Mon 15:04") ||
		When(day.Add(-30*24*time.Hour), day) != day.Add(-30*24*time.Hour).Format("Jan 2 15:04") {
		t.Fatal("when")
	}
}

func TestSummarizeSwitchPaused(t *testing.T) {
	e := contracts.AuditEntry{Action: contracts.AuditAccountWaiting, SessionTitle: "Credit perf", From: "me", To: "switching paused",
		Reason: "account switching paused: repeated limits within 30m (last automatic switch 14:05Z)"}
	want := "'Credit perf' hit a limit on me again soon after a switch; automatic account switching paused (needs you) — account switching paused: repeated limits within 30m (last automatic switch 14:05Z)"
	if s := Summarize(e); s != want {
		t.Fatalf("summary %q", s)
	}
}
