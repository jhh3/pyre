package tui_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/mock"
	"github.com/jhh3/pyre/internal/tui/tuitest"
)

// Sidebar markers (owner request 2026-10-13): "can we indicate in the
// sidebar if a conversation (the top level parent (if there's a big
// project)) or single conversation or one I've recently interacted with
// (i.e. actually sent a message to)". A lead is bold with a faint "▾N" /
// "+N" descendant count, a single session is plain, and the cursor slot of
// a session the owner messaged or typed into carries a bar: accent within
// the hour, faint within the day, nothing older.

// markersAPI is the tree fixture (treeAPI) plus Session.OwnerActiveAt set
// to the harness clock minus ago[id].
type markersAPI struct {
	treeAPI
	ago map[string]time.Duration
}

// markersNow is the harness clock (tuitest.NewMock).
var markersNow = mock.FixtureEpoch.Add(30 * time.Second)

func (a markersAPI) mark(s *contracts.SessionView) {
	if d, ok := a.ago[s.Session.SessionID]; ok {
		at := markersNow.Add(-d)
		s.Session.OwnerActiveAt = &at
	}
}

func (a markersAPI) ListSessions(ctx context.Context, r contracts.ListSessionsRequest) (contracts.Page[contracts.SessionView], error) {
	p, err := a.treeAPI.ListSessions(ctx, r)
	for i := range p.Items {
		a.mark(&p.Items[i])
	}
	return p, err
}

func (a markersAPI) GetSession(ctx context.Context, r contracts.GetRequest) (contracts.SessionView, error) {
	s, err := a.treeAPI.GetSession(ctx, r)
	if par, ok := a.parents[s.Session.SessionID]; ok {
		s.Session.Lineage.ParentSessionID = par
	}
	a.mark(&s)
	return s, err
}

var synthYours = map[string]time.Duration{
	"ses_synth-fb":     10 * time.Minute, // migration work: accent bar
	"ses_synth-apr":    5 * time.Hour,    // deploy prep: faint bar
	"ses_synth-leased": 72 * time.Hour,   // pairing session: no bar
}

func startMarkers(t *testing.T, w int) *tuitest.Harness {
	h, _ := start(t, w, 30, func(api contracts.API) contracts.API {
		return markersAPI{treeAPI{api, synthTree}, synthYours}
	}, nil)
	// Park the cursor on a row without markers so every marked row shows
	// its unselected form.
	selectSession(t, h, "ses_synth-standalone")
	h.WaitFor("preview", 3*sec, func() bool { return !h.Contains("loading preview…") })
	// The pane settles on its read-only snapshot; a frame taken while it is
	// still "connecting…" made the golden depend on load.
	h.WaitFor("terminal pane", 5*sec, func() bool { return h.Contains("read-only snapshot") })
	h.Settle()
	return h
}

// rowLine is the line of view whose list row is titled title (not a
// "↳ title" hint on another row, nor the preview pane's header).
func rowLine(view, title string) string {
	for _, l := range strings.Split(view, "\n") {
		if i := strings.Index(l, title); i >= 0 && !strings.HasSuffix(l[:i], "↳ ") && ansi.StringWidth(l[:i]) < 60 {
			return l
		}
	}
	return ""
}

// rawLine is title's row as rendered (styled; NO_COLOR has attributes only).
func rawLine(h *tuitest.Harness, title string) string { return rowLine(h.M.View().Content, title) }

const (
	sgrBold  = "\x1b[1m" // accent and bold under NO_COLOR
	sgrFaint = "\x1b[2m"
)

func TestSidebarMarkers(t *testing.T) {
	h := startMarkers(t, 160)
	v := h.View()
	golden(t, "sessions-markers-160", v)

	// Lead: bold title, faint open count; children keep their glyphs.
	if !strings.Contains(rowLine(v, "coordinator"), "coordinator ▾3") {
		t.Fatalf("lead lacks its descendant count:\n%s", v)
	}
	if raw := rawLine(h, "coordinator"); !strings.Contains(raw, sgrBold+"coordinator") || !strings.Contains(raw, sgrFaint+" ▾3") {
		t.Fatalf("lead title not bold or count not faint: %q", raw)
	}
	if !strings.Contains(rowLine(v, "migration work"), "└ migration work") || strings.Contains(rawLine(h, "migration work"), sgrBold+"migration work") {
		t.Fatalf("child row changed: %q", rawLine(h, "migration work"))
	}
	// Single session: plain, no count, no bold.
	for _, single := range []string{"pairing session", "api work (handoff successor)"} {
		l, raw := rowLine(v, single), rawLine(h, single)
		if strings.ContainsAny(l, "▾+├└↳") || strings.Contains(raw, sgrBold+single) {
			t.Fatalf("single session %q is decorated: %q", single, raw)
		}
	}
	// A child whose parent is elsewhere keeps its hint, not a lead count.
	if !strings.Contains(lineOf(v, "outfit API client"), "outfit API client ↳ coordinator") {
		t.Fatalf("cross-group child lost its hint:\n%s", v)
	}

	// Recently yours: accent bar (10m), faint bar (5h), nothing (3d).
	for _, c := range []struct {
		title, sgr string
	}{{"migration work", sgrBold}, {"deploy prep", sgrFaint}} {
		if l := rowLine(v, c.title); !strings.HasPrefix(l, "▎ ") {
			t.Fatalf("%s lacks the yours bar: %q", c.title, l)
		}
		if raw := rawLine(h, c.title); !strings.Contains(raw, c.sgr+"▎") {
			t.Fatalf("%s bar has the wrong weight: %q", c.title, raw)
		}
	}
	for _, title := range []string{"pairing session", "coordinator", "worker"} {
		if l := rowLine(v, title); !strings.HasPrefix(l, "  ") {
			t.Fatalf("%s should have no bar: %q", title, l)
		}
	}
	// The selected row keeps its cursor.
	selectSession(t, h, "ses_synth-fb")
	if l := rowLine(h.View(), "└ migration work"); !strings.HasPrefix(l, "› ") {
		t.Fatalf("selected row lost its cursor: %q", l)
	}

	// Folded, the lead's count reads "+3".
	selectSession(t, h, "ses_synth-a")
	h.Key("z", "a")
	h.Settle()
	if l := rowLine(h.View(), "coordinator"); !strings.Contains(l, "coordinator +3") || strings.Contains(l, "▾") {
		t.Fatalf("folded lead: %q", l)
	}
	if raw := rawLine(h, "coordinator"); !strings.Contains(raw, sgrBold+"coordinator") {
		t.Fatalf("folded lead not bold: %q", raw)
	}
	// A click on "▾3" folds as "+3" unfolds.
	x, y := textAt(t, h, "+3")
	clickAt(h, x, y)
	if !slices.Contains(h.M.SessionRows(), "ses_synth-fb") {
		t.Fatalf("click on +3 did not unfold:\n%s", h.View())
	}
	x, y = textAt(t, h, "▾3")
	clickAt(h, x, y)
	if slices.Contains(h.M.SessionRows(), "ses_synth-fb") || !h.Contains("coordinator +3") {
		t.Fatalf("click on ▾3 did not fold:\n%s", h.View())
	}
}

// The table carries the same markers; the facts say when you last
// messaged or typed; Grid captions show "you 10m"; g s sorts by it.
func TestSidebarMarkersTableFactsGridSort(t *testing.T) {
	h := startMarkers(t, 140)
	h.Key("v")
	h.Settle()
	v := h.View()
	if !strings.Contains(rowLine(v, "coordinator"), "coordinator ▾3") || !strings.HasPrefix(rowLine(v, "migration work"), "▎ ") ||
		!strings.HasPrefix(rowLine(v, "deploy prep"), "▎ ") || !strings.HasPrefix(rowLine(v, "pairing session"), "  ") {
		t.Fatalf("table lacks the markers:\n%s", v)
	}
	h.Key(":")
	h.Type("view sidenav")
	h.Key("enter")
	h.Settle()

	selectSession(t, h, "ses_synth-fb")
	h.Key("i")
	h.Settle()
	if l := lineOf(h.View(), "messaged or typed"); !strings.Contains(l, "You") || !strings.Contains(l, "messaged or typed 10m ago") {
		t.Fatalf("facts lack the yours line:\n%s", h.View())
	}
	h.Key("esc")
	h.Settle()
	selectSession(t, h, "ses_synth-standalone")
	h.Key("i")
	h.Settle()
	if h.Contains("messaged or typed") {
		t.Fatalf("a session you never messaged shows a yours line:\n%s", h.View())
	}
	h.Key("esc")
	h.Settle()

	// :sort yours puts the sessions you touched most recently first
	// (children stay under their parents).
	h.Key(":")
	h.Type("sort yours")
	h.Key("enter")
	h.Settle()
	if h.M.SortMode() != "yours" {
		t.Fatalf("sort %s, want yours", h.M.SortMode())
	}
	rows := h.M.SessionRows()
	apr, leased, standalone := slices.Index(rows, "ses_synth-apr"), slices.Index(rows, "ses_synth-leased"), slices.Index(rows, "ses_synth-standalone")
	if apr < 0 || leased < 0 || standalone < 0 || apr > leased || leased > standalone {
		t.Fatalf("yours sort order wrong: %v\n%s", rows, h.View())
	}
}

// Grid captions name how recently you messaged or typed ("you 10m").
func TestSidebarMarkersGridCaption(t *testing.T) {
	h, _ := start(t, 240, 65, func(api contracts.API) contracts.API {
		return tuitest.NewEmbeddable(markersAPI{treeAPI{api, synthTree}, synthYours})
	}, nil)
	openGrid(t, h)
	v := h.View()
	if !strings.Contains(v, "migration work · ") || !strings.Contains(v, " working · you 10m · claude@synth-mac") || !strings.Contains(v, "deploy prep · ! approval · you 5h") || strings.Contains(v, "you 3d") {
		t.Fatalf("grid captions lack the yours age:\n%s", v)
	}
}
