package tui_test

import (
	"strings"
	"testing"

	"github.com/jhh3/pyre/internal/runtime/mock"
)

func TestGlobalSearchFindsOpensAtMatchAndAttaches(t *testing.T) {
	h, _, _ := startMock(t, 160, 34, func(b *mock.Backend) {
		if err := b.SetNodeConnected("node_synth-linux", false); err != nil {
			t.Fatal(err)
		}
	}, nil)
	h.Key("space", "/")
	if !h.M.OverlayOpen() || !h.Contains("Search ›") {
		t.Fatalf("<leader>/ should open global search:\n%s", h.View())
	}
	h.Type("coordinator")
	h.WaitFor("live results while typing", 5*sec, func() bool { return h.Contains("in 1 session") })
	v := h.View()
	for _, want := range []string{"you", "Synthetic task:", "synth-linux not searched (offline)", "enter opens the transcript at this match"} {
		if !strings.Contains(v, want) {
			t.Fatalf("results missing %q:\n%s", want, v)
		}
	}
	// enter moves to the results, enter again opens the reader at the match.
	h.Key("enter")
	h.Key("enter")
	h.WaitFor("reader at the match", 5*sec, func() bool {
		return h.M.ActiveView() == "Transcript" && h.Contains("/coordinator") && h.Contains("1/")
	})
	h.Key("esc", "esc")
	if h.M.ActiveView() != "Sessions" {
		t.Fatalf("esc should leave the reader, got %s", h.M.ActiveView())
	}
	// a attaches the hit's session.
	h.Key("space", "/")
	h.Type("coordinator")
	h.Key("enter")
	h.WaitFor("results", 5*sec, func() bool { return h.Contains("in 1 session") })
	h.Key("a")
	h.WaitFor("attach from search", 3*sec, func() bool { return len(h.Attaches()) == 1 })
	if h.M.SelectedSession() != "ses_synth-a" {
		t.Errorf("selection after a = %s", h.M.SelectedSession())
	}
}

func TestFinderQuestionMarkSwitchesToSearch(t *testing.T) {
	h, _ := start(t, 160, 34, nil, nil)
	h.Key("ctrl+p")
	h.Type("?task")
	if !h.Contains("Search ›") || !h.Contains("task") {
		t.Fatalf("?text in the finder should open search:\n%s", h.View())
	}
	h.WaitFor("results", 5*sec, func() bool { return h.Contains("matches in") })
	// typing refines; no matches says so plainly
	h.Type("zzqx")
	h.Key("enter")
	h.WaitFor("no matches", 5*sec, func() bool { return h.Contains("no matches") })
	h.Key("esc")
	if h.M.OverlayOpen() {
		t.Fatalf("esc should close search:\n%s", h.View())
	}
}
