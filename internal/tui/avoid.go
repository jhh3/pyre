package tui

import (
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// Node avoid flags (contracts/node_avoid.go): a machine the owner or a
// session asked new work to stay off until a time. The Machines view and
// the new-session machine picker show a badge, and the picker's default
// skips the machine while another is connected.

// avoidBadge is a short badge for a node with a flag in force for any
// launch ("" when none): "avoid until 18:00" or "hard avoid until …".
func avoidBadge(n contracts.Node, now time.Time) string {
	a, ok := contracts.EffectiveAvoid(n.Avoid, "", now)
	if !ok {
		return ""
	}
	s := "avoid until " + a.ExpiresAt.Local().Format("15:04")
	if a.ExpiresAt.Sub(now) >= 24*time.Hour {
		s = "avoid until " + a.ExpiresAt.Local().Format("Mon 15:04")
	}
	if a.Hard {
		s = "hard " + s
	}
	return s
}

// avoidDetail describes one flag for the machine detail pane.
func avoidDetail(a contracts.NodeAvoid) string {
	s := "soft"
	if a.Hard {
		s = "hard"
	}
	s += " until " + a.ExpiresAt.Local().Format("Mon Jan 2 15:04 MST")
	if a.ProjectID != nil && *a.ProjectID != "" {
		s += ", project " + *a.ProjectID
	}
	if a.OwnerSet {
		s += ", by you"
	} else {
		s += ", by session " + a.SetBy.ID
	}
	if a.Reason != "" {
		s += " — " + a.Reason
	}
	return s
}

// avoidedNow reports a node with a flag in force for a launch outside any
// project.
func avoidedNow(n contracts.Node, now time.Time) bool {
	_, ok := contracts.EffectiveAvoid(n.Avoid, "", now)
	return ok
}
