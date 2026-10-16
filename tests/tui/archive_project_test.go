package tui_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/mock"
)

// projectState reports whether the synthetic Outfit project is archived and
// which of its sessions are scheduled to archive when done.
func projectState(t *testing.T, b *mock.Backend) (archived bool, whenDone []string) {
	t.Helper()
	ps, err := b.ListProjects(ctx(), contracts.ListProjectsRequest{IncludeArchived: true})
	if err != nil {
		t.Fatal(err)
	}
	pid := ""
	for _, p := range ps.Items {
		if p.Name == "Synthetic Outfit" {
			pid, archived = p.ProjectID, p.Archived
		}
	}
	ss, err := b.ListSessions(ctx(), contracts.ListSessionsRequest{ProjectID: &pid, IncludeArchived: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range ss.Items {
		if slices.Contains(s.Session.Tags, contracts.TagArchiveWhenDone) {
			whenDone = append(whenDone, s.Session.Title)
		}
	}
	return archived, whenDone
}

// W-archive2: X on a project in the Projects tab confirms with counts and
// archives the project; its live session archives when done.
func TestProjectsTabArchiveProject(t *testing.T) {
	h, _, b := startMock(t, 100, 30, nil, nil)
	h.Key("2")
	h.Settle()
	h.Key("X")
	h.Settle()
	if !h.Contains("Archive project") || !h.Contains("1 live session will archive when done") {
		t.Fatalf("confirm with counts:\n%s", h.View())
	}
	h.Key("y")
	h.WaitFor("project archived", 5*sec, func() bool { a, _ := projectState(t, b); return a })
	if _, wd := projectState(t, b); len(wd) != 1 {
		t.Fatalf("the live session archives when done: %v", wd)
	}
}

// X on a folded project group in Sessions archives the project; the
// sidebar's toast is one truncated line until a click expands it.
func TestSessionsGroupArchiveProjectAndToastExpand(t *testing.T) {
	h, _, b := startMock(t, 100, 30, nil, nil)
	h.Key("z", "a") // fold the selected session's group (Synthetic Outfit)
	h.Settle()
	if !h.Contains("X archive project") {
		t.Fatalf("folded project group offers X:\n%s", h.View())
	}
	h.Key("X")
	h.Settle()
	if !h.Contains("1 live session will archive when done") {
		t.Fatalf("confirm:\n%s", h.View())
	}
	h.Key("y")
	h.WaitFor("project archived", 5*sec, func() bool { a, _ := projectState(t, b); return a })
	h.WaitFor("toast", 5*sec, func() bool { return h.Contains("archived project Synthetic") })
	rows := 0
	for _, l := range strings.Split(h.View(), "\n") {
		if strings.Contains(l, "archived project") {
			rows++
			if !strings.Contains(l, "…") {
				t.Fatalf("a long toast is one truncated line: %q", l)
			}
		}
	}
	if rows != 1 {
		t.Fatalf("toast on %d lines:\n%s", rows, h.View())
	}
	click(t, h, "archived project")
	if !h.Contains("archive when done") {
		t.Fatalf("a click shows the full toast:\n%s", h.View())
	}
	click(t, h, "archived project")
	if h.Contains("archived project") {
		t.Fatalf("a second click dismisses it:\n%s", h.View())
	}
}
