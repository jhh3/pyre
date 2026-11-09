package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

func TestAvoidBadge(t *testing.T) {
	now := time.Date(2026, 10, 23, 14, 0, 0, 0, time.Local)
	n := contracts.Node{NodeID: "node_synth-a"}
	if avoidBadge(n, now) != "" || avoidedNow(n, now) {
		t.Fatal("no flag, badge")
	}
	pid := "prj_synth"
	n.Avoid = []contracts.NodeAvoid{{ProjectID: &pid, Hard: true, ExpiresAt: now.Add(time.Hour)}}
	if avoidBadge(n, now) != "" {
		t.Fatal("a project flag badges the machine for every launch")
	}
	n.Avoid = append(n.Avoid, contracts.NodeAvoid{ExpiresAt: now.Add(4 * time.Hour), Reason: "synthetic", OwnerSet: true})
	if got := avoidBadge(n, now); got != "avoid until 18:00" || !avoidedNow(n, now) {
		t.Fatalf("soft: %q", got)
	}
	n.Avoid[1].Hard = true
	if got := avoidBadge(n, now); got != "hard avoid until 18:00" {
		t.Fatalf("hard: %q", got)
	}
	if got := avoidBadge(n, now.Add(4*time.Hour)); got != "" {
		t.Fatalf("expired: %q", got)
	}
	if d := avoidDetail(n.Avoid[1]); !strings.HasPrefix(d, "hard until ") || !strings.Contains(d, "by you — synthetic") {
		t.Fatalf("detail: %q", d)
	}
}
