package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"

	"github.com/jhh3/pyre/internal/contracts"
)

// switchModel is a model over synthetic sessions (no API needed: switching
// in the list pane never calls one).
func switchModel(t *testing.T, ss []contracts.SessionView) *Model {
	t.Helper()
	m := New(t.Context(), nil, Options{NoColor: true, KeysFile: "none", StateFile: "none"})
	m.width, m.height = 200, 55
	m.loads["sessions"] = &loadState{val: ss, have: true, seq: 1}
	m.syncSessions()
	return m
}

// decode turns raw terminal input into the key press Bubble Tea delivers.
func decode(t *testing.T, raw string) tea.KeyPressMsg {
	t.Helper()
	var d uv.EventDecoder
	n, ev := d.Decode([]byte(raw))
	k, ok := ev.(uv.KeyPressEvent)
	if !ok || n != len(raw) {
		t.Fatalf("%q decoded as %T (%d of %d bytes)", raw, ev, n, len(raw))
	}
	return tea.KeyPressMsg{Code: k.Code, Mod: tea.KeyMod(k.Mod), Text: k.Text, ShiftedCode: k.ShiftedCode, BaseCode: k.BaseCode}
}

func fourSessions() []contracts.SessionView {
	return []contracts.SessionView{
		sv("ses_a", "synth-mac", "/src/a"), sv("ses_b", "synth-mac", "/src/b"),
		sv("ses_c", "synth-mac", "/src/c"), sv("ses_d", "synth-mac", "/src/d"),
	}
}

// Every alt+arrow encoding terminals send resolves to the switch actions:
// xterm CSI 1;3, iTerm2-style CSI 1;9 (meta) and ESC-prefixed arrows.
func TestSwitchChordEncodings(t *testing.T) {
	m := switchModel(t, fourSessions())
	cases := map[string]string{
		"\x1b[1;3A": "sessions.prev", "\x1b[1;3B": "sessions.next", "\x1b[1;3D": "sessions.last", "\x1b[1;3C": "sessions.attention",
		"\x1b[1;9A": "sessions.prev", "\x1b[1;9B": "sessions.next", "\x1b[1;9D": "sessions.last", "\x1b[1;9C": "sessions.attention",
		"\x1b\x1b[A": "sessions.prev", "\x1b\x1b[B": "sessions.next", "\x1b\x1b[D": "sessions.last", "\x1b\x1b[C": "sessions.attention",
		"\x1b\x1bOA": "sessions.prev", "\x1b\x1bOB": "sessions.next",
		"\x1bs": "global.switcher",
	}
	for raw, want := range cases {
		k := decode(t, raw)
		got := m.chordAction(k.String())
		if got == "" {
			got = m.chordAction(metaAsAlt(k).String())
		}
		if got != want {
			t.Errorf("%q (%s) -> %q, want %q", raw, k.String(), got, want)
		}
	}
}

// alt+up/down walk the visible list and stop at the ends with a hint.
func TestSwitchStepStopsAtEnds(t *testing.T) {
	m := switchModel(t, fourSessions())
	m.sessList.selectID("ses_b")
	m.onKey(decode(t, "\x1b[1;3B"))
	if m.sessList.sel != "ses_c" {
		t.Fatalf("alt+down: sel %s", m.sessList.sel)
	}
	m.onKey(decode(t, "\x1b\x1b[B"))
	m.onKey(decode(t, "\x1b[1;9B"))
	if m.sessList.sel != "ses_d" || m.toast == nil || m.toast.text != "end of the list" {
		t.Fatalf("at the end: sel %s toast %+v", m.sessList.sel, m.toast)
	}
	for range 5 {
		m.onKey(decode(t, "\x1b[1;3A"))
	}
	if m.sessList.sel != "ses_a" || m.toast.text != "top of the list" {
		t.Fatalf("at the top: sel %s toast %q", m.sessList.sel, m.toast.text)
	}
}

// Folded groups' hidden rows and machine header rows are skipped; archived
// sessions are never visited.
func TestSwitchStepSkipsFoldedAndArchived(t *testing.T) {
	ss := []contracts.SessionView{
		sv("ses_a", "synth-alpha", "/src/a"), sv("ses_b", "synth-beta", "/src/b"),
		sv("ses_c", "synth-beta", "/src/c"), sv("ses_d", "synth-gamma", "/src/d"), sv("ses_e", "synth-gamma", "/src/e"),
	}
	ss[4].Session.Archived = true
	m := switchModel(t, ss)
	m.grouping.mode = groupMachine
	m.grouping.collapsed = map[string]bool{"machine:synth-beta": true}
	m.syncSessions()
	m.sessList.selectID("ses_a")
	m.onKey(KeyAlt(tea.KeyDown))
	if m.sessList.sel != "ses_d" {
		t.Fatalf("alt+down over a folded group: sel %s (rows %v)", m.sessList.sel, m.sessList.ids)
	}
	m.onKey(KeyAlt(tea.KeyDown))
	if m.sessList.sel != "ses_d" {
		t.Fatalf("archived session visited: %s", m.sessList.sel)
	}
}

// In the default project grouping alt+up/down follow the project groups
// (projects by name, "No project" last) and skip a
// folded project.
func TestSwitchStepFollowsProjectGroups(t *testing.T) {
	at := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	mk := func(id, node, prj string, min int) contracts.SessionView {
		s := sv(id, node, "/src/"+id)
		if prj != "" {
			s.Session.ProjectID = &prj
		}
		s.Run.StartedAt = at.Add(time.Duration(min) * time.Minute)
		return s
	}
	ss := []contracts.SessionView{
		mk("ses_old1", "synth-alpha", "prj_old", -60), mk("ses_new1", "synth-beta", "prj_new", -5),
		mk("ses_loose", "synth-alpha", "", 0), mk("ses_new2", "synth-alpha", "prj_new", -50), mk("ses_old2", "synth-beta", "prj_old", -40),
	}
	m := switchModel(t, ss)
	if m.GroupMode() != "project" {
		t.Fatalf("default grouping %s", m.GroupMode())
	}
	want := []string{"ses_new1", "ses_new2", "ses_old1", "ses_old2", "ses_loose"} // projects by name, creation order inside
	if !reflect.DeepEqual(m.sessList.ids, want) {
		t.Fatalf("rows %v, want %v", m.sessList.ids, want)
	}
	m.sessList.selectID("ses_new2")
	m.onKey(KeyAlt(tea.KeyDown))
	if m.sessList.sel != "ses_old1" {
		t.Fatalf("alt+down into the next project: %s", m.sessList.sel)
	}
	m.grouping.collapsed = map[string]bool{"project:prj_old": true}
	m.syncSessions()
	m.sessList.selectID("ses_new2")
	m.onKey(KeyAlt(tea.KeyDown))
	if m.sessList.sel != "ses_loose" {
		t.Fatalf("alt+down over a folded project: %s (rows %v)", m.sessList.sel, m.sessList.ids)
	}
}

// KeyAlt is alt+code as Bubble Tea delivers it.
func KeyAlt(code rune) tea.KeyPressMsg { return tea.KeyPressMsg{Code: code, Mod: tea.ModAlt} }

// alt+left toggles between the last two viewed sessions, like tmux.
func TestSwitchLastToggles(t *testing.T) {
	m := switchModel(t, fourSessions())
	m.sessList.selectID("ses_a")
	m.noteViewed("ses_a")
	m.onKey(KeyAlt(tea.KeyDown))
	m.onKey(KeyAlt(tea.KeyDown)) // ses_c
	m.onKey(KeyAlt(tea.KeyLeft))
	if m.sessList.sel != "ses_b" {
		t.Fatalf("alt+left: %s", m.sessList.sel)
	}
	m.onKey(KeyAlt(tea.KeyLeft))
	if m.sessList.sel != "ses_c" {
		t.Fatalf("alt+left again: %s", m.sessList.sel)
	}
	// j/k movement that dwelt (the preview fired) counts as viewing.
	m.sessList.selectID("ses_a")
	m.update(previewDueMsg{token: m.previewTok})
	m.onKey(KeyAlt(tea.KeyLeft))
	if m.sessList.sel != "ses_c" {
		t.Fatalf("alt+left after dwelling on ses_a: %s", m.sessList.sel)
	}
}

// alt+right cycles through the sessions that need you, opening a folded
// group when one is inside it.
func TestSwitchAttentionCycles(t *testing.T) {
	ss := []contracts.SessionView{
		sv("ses_a", "synth-alpha", "/src/a"), sv("ses_b", "synth-beta", "/src/b"),
		sv("ses_c", "synth-beta", "/src/c"), sv("ses_d", "synth-gamma", "/src/d"),
	}
	ss[1].Attention, ss[3].Attention = 1, 1
	m := switchModel(t, ss)
	m.grouping.collapsed = map[string]bool{"machine:synth-beta": true}
	m.syncSessions()
	m.sessList.selectID("ses_a")
	m.onKey(KeyAlt(tea.KeyRight))
	if m.sessList.sel != "ses_b" {
		t.Fatalf("first needs-you: %s (rows %v)", m.sessList.sel, m.sessList.ids)
	}
	m.onKey(KeyAlt(tea.KeyRight))
	if m.sessList.sel != "ses_d" {
		t.Fatalf("second needs-you: %s", m.sessList.sel)
	}
	m.onKey(KeyAlt(tea.KeyRight))
	if m.sessList.sel != "ses_b" {
		t.Fatalf("cycle: %s", m.sessList.sel)
	}
	ss[1].Attention, ss[3].Attention = 0, 0
	m.loads["sessions"].val = ss
	m.onKey(KeyAlt(tea.KeyRight))
	if m.toast == nil || m.toast.text != "nothing needs you" {
		t.Fatalf("no attention: %+v", m.toast)
	}
}

// ESC b / ESC f (Terminal.app's option+left/right) switch only where safe:
// not inside the embedded terminal by default, never with "off".
func TestOptionArrowsOnlyWhereSafe(t *testing.T) {
	m := switchModel(t, fourSessions())
	m.sessList.selectID("ses_a")
	m.noteViewed("ses_a")
	m.onKey(KeyAlt(tea.KeyDown))
	if _, ok := m.switchKey(decode(t, "\x1bb")); !ok || m.sessList.sel != "ses_a" {
		t.Fatalf("ESC b in the list should act as alt+left (sel %s)", m.sessList.sel)
	}
	m.km.OptionArrows = "off"
	if _, ok := m.switchKey(decode(t, "\x1bb")); ok {
		t.Fatal("ESC b switched with macos_option_arrows = off")
	}
	// A text input keeps alt+arrows (word motions).
	m.km.OptionArrows = "list"
	m.filtering = true
	if _, ok := m.switchKey(KeyAlt(tea.KeyDown)); ok {
		t.Fatal("alt+down taken from the filter input")
	}
}

// keys.toml rebinds the switch actions; a non-chord binding goes through
// the normal dispatcher, a chord works from anywhere.
func TestSwitchKeysRebindable(t *testing.T) {
	p := filepath.Join(t.TempDir(), "keys.toml")
	conf := "macos_option_arrows = \"always\"\n[keys]\n\"sessions.next\" = [\"ctrl+down\", \"J\"]\n\"global.switcher\" = \"alt+w\"\n"
	if err := os.WriteFile(p, []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	km, err := LoadKeymap(p)
	if err != nil || len(km.Warnings) > 0 {
		t.Fatalf("load: %v %v", err, km.Warnings)
	}
	if km.OptionArrows != "always" {
		t.Fatalf("macos_option_arrows = %q", km.OptionArrows)
	}
	m := switchModel(t, fourSessions())
	m.km = km
	m.sessList.selectID("ses_a")
	if m.chordAction("alt+down") != "" || m.chordAction("ctrl+down") != "sessions.next" || m.chordAction("J") != "" {
		t.Fatal("chord bindings not taken from keys.toml")
	}
	m.onKey(tea.KeyPressMsg{Code: 'J', Text: "J"})
	if m.sessList.sel != "ses_b" {
		t.Fatalf("J (dispatcher binding) did not switch: %s", m.sessList.sel)
	}
	m.onKey(tea.KeyPressMsg{Code: 'w', Mod: tea.ModAlt})
	if !m.isSwitcherOpen() {
		t.Fatal("alt+w did not open the switcher")
	}
	if _, err := LoadKeymap(writeKeys(t, "macos_option_arrows = \"sometimes\"\n")); err == nil {
		t.Fatal("bad macos_option_arrows accepted")
	}
}

func writeKeys(t *testing.T, s string) string {
	p := filepath.Join(t.TempDir(), "keys.toml")
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// The switcher lists recent sessions first with the cursor on the previous
// one, filters fuzzily and switches on enter.
func TestSwitcherRecentFirstAndFilter(t *testing.T) {
	ss := fourSessions()
	ss[2].Session.Title = "outfit api client"
	m := switchModel(t, ss)
	for _, id := range []string{"ses_d", "ses_b", "ses_a"} {
		m.sessList.selectID(id)
		m.noteViewed(id)
	}
	m.onKey(tea.KeyPressMsg{Code: 's', Mod: tea.ModAlt})
	o, ok := m.overlay.(*switcherOverlay)
	if !ok {
		t.Fatal("alt+s did not open the switcher")
	}
	var order []string
	for _, r := range o.res {
		order = append(order, r.it.s.Session.SessionID)
	}
	if strings.Join(order[:3], ",") != "ses_a,ses_b,ses_d" || o.sel != "ses_b" {
		t.Fatalf("order %v sel %s", order, o.sel)
	}
	for _, r := range "outapi" {
		m.onKey(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	if len(o.res) == 0 || o.res[0].it.s.Session.SessionID != "ses_c" {
		t.Fatalf("fuzzy filter: %+v", o.res)
	}
	m.onKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.overlay != nil || m.sessList.sel != "ses_c" || m.view != viewSessions {
		t.Fatalf("enter: overlay %v sel %s", m.overlay != nil, m.sessList.sel)
	}
	m.onKey(KeyAlt(tea.KeyLeft))
	if m.sessList.sel != "ses_a" {
		t.Fatalf("alt+left after the switcher: %s", m.sessList.sel)
	}
}

// The switcher opens (and draws) in well under 50 ms with hundreds of
// sessions.
func TestSwitcherOpensFastWithManySessions(t *testing.T) {
	var ss []contracts.SessionView
	for i := range 600 {
		s := sv(fmt.Sprintf("ses_synth-%03d", i), fmt.Sprintf("synth-node-%d", i%7), fmt.Sprintf("/src/repo-%d", i%40))
		s.Session.Title = fmt.Sprintf("synthetic session %d working on repo-%d", i, i%40)
		ss = append(ss, s)
	}
	m := switchModel(t, ss)
	for i := range 50 {
		m.noteViewed(ss[i*3].Session.SessionID)
	}
	best := time.Hour
	for range 5 {
		t0 := time.Now()
		m.openSwitcher(false)
		_ = m.render()
		m.overlay.update(m, tea.KeyPressMsg{Code: 'r', Text: "r"})
		_ = m.render()
		best = min(best, time.Since(t0))
		m.overlay = nil
	}
	t.Logf("open + draw + one key + draw with 600 sessions: %s", best)
	if best > 50*time.Millisecond {
		t.Fatalf("switcher took %s with 600 sessions", best)
	}
}
