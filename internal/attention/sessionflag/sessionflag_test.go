package sessionflag

import (
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

var t0 = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

func item(id string, k contracts.AttentionKind, st contracts.AttentionState, title string, age time.Duration) contracts.AttentionItem {
	return contracts.AttentionItem{AttentionID: id, Kind: k, State: st, Title: title, UpdatedAt: t0.Add(-age)}
}

func TestLevelMapping(t *testing.T) {
	cases := []struct {
		k             contracts.AttentionKind
		level, reason string
	}{
		{contracts.AttnApproval, contracts.FlagNeedsYou, "approval"},
		{contracts.AttnQuestion, contracts.FlagNeedsYou, "question"},
		{contracts.AttnFlag, contracts.FlagNeedsYou, "asked"},
		{contracts.AttnFailure, contracts.FlagNeedsYou, "failed"},
		{contracts.AttnAccountPool, contracts.FlagNeedsYou, "reauth"},
		{contracts.AttnIndeterminate, contracts.FlagNeedsYou, "delivery"},
		{contracts.AttnConflict, contracts.FlagNeedsYou, "conflict"},
		{contracts.AttnMessage, contracts.FlagMessage, "message"},
		{contracts.AttnComment, contracts.FlagMessage, "comment"},
		{contracts.AttnReviewReady, contracts.FlagMessage, "review"},
		{contracts.AttnUpdate, contracts.FlagMessage, "update"},
	}
	for _, c := range cases {
		if l, r := Level(c.k); l != c.level || r != c.reason {
			t.Errorf("%s: %s/%s, want %s/%s", c.k, l, r, c.level, c.reason)
		}
	}
}

func TestComputeRanksBlockingOverMessages(t *testing.T) {
	if f := Compute(nil); f != nil {
		t.Fatalf("empty: %+v", f)
	}
	only := Compute([]contracts.AttentionItem{item("att_1", contracts.AttnMessage, contracts.AttnOpen, "Message from ses_x", 0)})
	if only.Level != contracts.FlagMessage || only.Count != 1 || only.Messages != 1 || only.Text != "Message from ses_x" {
		t.Fatalf("message only: %+v", only)
	}
	f := Compute([]contracts.AttentionItem{
		item("att_1", contracts.AttnMessage, contracts.AttnOpen, "Message", 0),
		item("att_2", contracts.AttnFailure, contracts.AttnOpen, "Session run failed", time.Minute),
		item("att_3", contracts.AttnQuestion, contracts.AttnOpen, "Question: which DB?", 2*time.Minute),
		item("att_4", contracts.AttnApproval, contracts.AttnHandled, "Approve: rm", 0), // resolved: ignored
		item("att_5", contracts.AttnFlag, contracts.AttnSnoozed, "asked", 0),           // snoozed: ignored
	})
	if f.Level != contracts.FlagNeedsYou || f.Reason != "question" || f.AttentionID != "att_3" || f.Count != 3 || f.Messages != 1 {
		t.Fatalf("mixed: %+v", f)
	}
}

func TestReadStates(t *testing.T) {
	if f := Compute([]contracts.AttentionItem{item("att_1", contracts.AttnMessage, contracts.AttnRead, "m", 0)}); f != nil {
		t.Fatalf("a read message still flags: %+v", f)
	}
	if f := Compute([]contracts.AttentionItem{item("att_1", contracts.AttnFailure, contracts.AttnRead, "f", 0)}); f != nil {
		t.Fatalf("a seen failure still flags: %+v", f)
	}
	if f := Compute([]contracts.AttentionItem{item("att_1", contracts.AttnApproval, contracts.AttnRead, "a", 0)}); f == nil || f.Reason != "approval" {
		t.Fatalf("a read approval is still pending: %+v", f)
	}
}

func TestWithRunBlocked(t *testing.T) {
	r := &contracts.Run{State: contracts.RunWaitingApproval, Freshness: contracts.Freshness{ObservedAt: t0}}
	if f := WithRun(nil, r); f == nil || f.Level != contracts.FlagNeedsYou || f.Reason != "blocked" || f.Text != BlockedText {
		t.Fatalf("blocked: %+v", f)
	}
	msg := Compute([]contracts.AttentionItem{item("att_1", contracts.AttnMessage, contracts.AttnOpen, "m", 0)})
	if f := WithRun(msg, r); f.Reason != "blocked" || f.Count != 2 || f.Messages != 1 {
		t.Fatalf("blocked over message: %+v", f)
	}
	appr := Compute([]contracts.AttentionItem{item("att_1", contracts.AttnApproval, contracts.AttnOpen, "Approve: x", 0)})
	if f := WithRun(appr, r); f.Reason != "approval" {
		t.Fatalf("approval item wins over the run state: %+v", f)
	}
	if f := WithRun(nil, &contracts.Run{State: contracts.RunRunning}); f != nil {
		t.Fatalf("running: %+v", f)
	}
	if f := WithRun(msg, nil); f != msg {
		t.Fatal("nil run changed the flag")
	}
}

func TestResolveTitle(t *testing.T) {
	cases := []struct{ title, sid, st, want string }{
		{"Message from ses_01M49FMAXZGB21V7ZSHN3GA2H2", "ses_01M49FMAXZGB21V7ZSHN3GA2H2", "atlas-wt-retry", "Message from atlas-wt-retry"},
		{"Message from ses_x", "ses_x", "", "Message from ses_x"},
		{"Message from ses_x", "", "t", "Message from ses_x"},
		{"Approve: run tests", "ses_x", "t", "Approve: run tests"},
		{"Message from ses_x", "ses_x", "a very long session title that goes on and on beyond forty-eight runes", "Message from a very long session title that goes on and on b…"},
	}
	for _, c := range cases {
		if got := ResolveTitle(c.title, c.sid, c.st); got != c.want {
			t.Errorf("ResolveTitle(%q,%q,%q) = %q, want %q", c.title, c.sid, c.st, got, c.want)
		}
	}
}
