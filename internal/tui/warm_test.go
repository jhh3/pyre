package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/jhh3/pyre/internal/contracts"
)

// warmModel is a model over synthetic embeddable sessions on another
// machine (relayed), each with a run; no API is called.
func warmModel(t *testing.T, ids ...string) *Model {
	t.Helper()
	var ss []contracts.SessionView
	for _, id := range ids {
		s := sv(id, "synth-mac", "/src/"+id)
		s.Run.NodeID = "node_synth-mac"
		s.Run.Capabilities = []string{contracts.CapAttachNative, contracts.CapAttachReadOnly}
		ss = append(ss, s)
	}
	m := New(t.Context(), nil, Options{NoColor: true, KeysFile: "none", StateFile: "none",
		DialRelay: func(context.Context, contracts.AttachDescriptor, int, int, bool) (TermConn, error) {
			return newNopConn(), nil
		}})
	m.width, m.height = 200, 55
	m.loads["sessions"] = &loadState{val: ss, have: true, seq: 1}
	m.syncSessions()
	t.Cleanup(m.closeTerms)
	return m
}

// warmTerm adds a live terminal for sid, last viewed at used, with a
// screen drawn.
func warmTerm(m *Model, sid string, used time.Time) (*termView, nopConn) {
	tv := newTermView(sid, "run_"+sid, 40, 10, m.termPoke(sid))
	c := newNopConn()
	tv.setConn(c, termReadOnly)
	tv.feed([]byte("synthetic screen of " + sid))
	tv.lastUse = used
	m.terms().views[sid] = tv
	return tv, c
}

func connClosed(c nopConn) bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

// The warm cache keeps maxLiveTerms streams running, closing the least
// recently viewed; closed terminals keep their screen; the selected
// session's stream is never the one closed.
func TestPruneTermsKeepsWarmStreamsLeastRecentFirst(t *testing.T) {
	ids := []string{"ses_a", "ses_b", "ses_c", "ses_d", "ses_e", "ses_f", "ses_g", "ses_h"}
	m := warmModel(t, ids...)
	m.sessList.selectID("ses_a") // selected, yet viewed longest ago
	base := time.Now().Add(-time.Hour)
	conns := map[string]nopConn{}
	for i, id := range ids {
		_, conns[id] = warmTerm(m, id, base.Add(time.Duration(i)*time.Minute))
	}
	m.pruneTerms("")
	if n := m.LiveTerms(); n != maxLiveTerms {
		t.Fatalf("%d live after prune, want %d", n, maxLiveTerms)
	}
	// a stays (selected); b, c, d are the least recently viewed others.
	for id, want := range map[string]bool{"ses_a": false, "ses_b": true, "ses_c": true, "ses_d": true, "ses_e": false, "ses_h": false} {
		if got := connClosed(conns[id]); got != want {
			t.Errorf("%s closed = %v, want %v", id, got, want)
		}
	}
	if len(m.terms().views) != len(ids) {
		t.Fatalf("closed terminals left the cache: %d of %d kept", len(m.terms().views), len(ids))
	}
	if got := m.TermText("ses_b"); !strings.Contains(got, "synthetic screen of ses_b") {
		t.Fatalf("a closed terminal lost its screen: %q", got)
	}
}

// At most maxCachedTerms screens are kept; the least recently viewed go.
func TestPruneTermsCapsCachedScreens(t *testing.T) {
	var ids []string
	for i := 0; i < maxCachedTerms+4; i++ {
		ids = append(ids, "ses_"+string(rune('a'+i)))
	}
	m := warmModel(t, ids...)
	m.sessList.selectID(ids[len(ids)-1])
	base := time.Now().Add(-time.Hour)
	for i, id := range ids {
		warmTerm(m, id, base.Add(time.Duration(i)*time.Minute))
	}
	m.pruneTerms("")
	if n := len(m.terms().views); n != maxCachedTerms {
		t.Fatalf("%d terminals cached, want %d", n, maxCachedTerms)
	}
	for _, id := range ids[:4] {
		if m.terms().views[id] != nil {
			t.Errorf("%s (least recently viewed) still cached", id)
		}
	}
}

// sweepTerms drops terminals of archived or vanished sessions, closes
// streams of ended runs and warm streams idle past termWarmIdle (keeping
// their screens), and leaves the selected and recent ones running.
func TestSweepTermsDropsArchivedClosesIdle(t *testing.T) {
	m := warmModel(t, "ses_arch", "ses_ended", "ses_idle", "ses_sel", "ses_recent")
	ss := m.sessions()
	ss[0].Session.Archived = true
	ss[1].Run.State = contracts.RunExited
	m.sessList.selectID("ses_sel")
	old := time.Now().Add(-2 * termWarmIdle)
	_, arch := warmTerm(m, "ses_arch", time.Now())
	_, ended := warmTerm(m, "ses_ended", time.Now())
	_, idle := warmTerm(m, "ses_idle", old)
	_, sel := warmTerm(m, "ses_sel", old)
	_, recent := warmTerm(m, "ses_recent", time.Now())
	_, gone := warmTerm(m, "ses_gone", time.Now()) // not in the session list at all
	m.sweepTerms()
	views := m.terms().views
	if views["ses_arch"] != nil || views["ses_gone"] != nil || !connClosed(arch) || !connClosed(gone) {
		t.Fatalf("archived/vanished terminals kept: arch=%v gone=%v", views["ses_arch"] != nil, views["ses_gone"] != nil)
	}
	if !connClosed(ended) || !connClosed(idle) {
		t.Fatalf("ended run or idle warm stream still open: ended=%v idle=%v", connClosed(ended), connClosed(idle))
	}
	if views["ses_idle"] == nil || !strings.Contains(m.TermText("ses_idle"), "synthetic screen of ses_idle") {
		t.Fatal("an idle-closed terminal lost its cached screen")
	}
	if connClosed(sel) || connClosed(recent) {
		t.Fatalf("selected or recently viewed stream closed: sel=%v recent=%v", connClosed(sel), connClosed(recent))
	}
}

// Quitting closes every warm stream.
func TestCloseTermsClosesWarmStreams(t *testing.T) {
	m := warmModel(t, "ses_a", "ses_b", "ses_c")
	var conns []nopConn
	for _, id := range []string{"ses_a", "ses_b", "ses_c"} {
		_, c := warmTerm(m, id, time.Now())
		conns = append(conns, c)
	}
	m.Close()
	for i, c := range conns {
		if !connClosed(c) {
			t.Errorf("stream %d still open after Close", i)
		}
	}
	if len(m.terms().views) != 0 {
		t.Fatalf("%d terminals left after Close", len(m.terms().views))
	}
}

// Output of a warm terminal out of sight schedules no frame; the shown
// one does.
func TestTermPokeOnlyForShownTerminal(t *testing.T) {
	m := warmModel(t, "ses_a", "ses_b")
	tm := m.terms()
	tm.shown.Store("ses_a")
	m.termPoke("ses_b")()
	select {
	case <-tm.notify:
		t.Fatal("a hidden terminal scheduled a frame")
	default:
	}
	m.termPoke("ses_a")()
	select {
	case <-tm.notify:
	default:
		t.Fatal("the shown terminal scheduled no frame")
	}
}

// A single move loads the new selection at once; a move right after
// another (a held key) is debounced.
func TestSchedulePreviewImmediateUnlessBurst(t *testing.T) {
	m := warmModel(t, "ses_a")
	m.opts.PreviewDelay = time.Hour
	first := m.schedulePreview()
	done := make(chan tea.Msg, 1)
	go func() { done <- first() }()
	select {
	case msg := <-done:
		if p, ok := msg.(previewDueMsg); !ok || p.token != m.previewTok {
			t.Fatalf("first move: %#v", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a single move waited for the debounce")
	}
	burst := m.schedulePreview()
	go func() { done <- burst() }()
	select {
	case msg := <-done:
		t.Fatalf("a move within PreviewDelay of the last was not debounced: %#v", msg)
	case <-time.After(50 * time.Millisecond):
	}
}

// Once the cursor rests, the rows above and below are warmed (read-only,
// relayed), ranked older than the sessions actually viewed.
func TestPrefetchTermsWarmsNeighbours(t *testing.T) {
	m := warmModel(t, "ses_a", "ses_b", "ses_c", "ses_d")
	m.view = viewSessions
	m.sessList.selectID("ses_b")
	if cmd := m.prefetchTerms(); cmd == nil {
		t.Fatal("no prefetch issued")
	}
	views := m.terms().views
	for _, id := range []string{"ses_a", "ses_c"} {
		tv := views[id]
		if tv == nil {
			t.Fatalf("%s not prefetched", id)
		}
		if st, _, _ := tv.status(); st != termConnecting || tv.reqMode != termReadOnly {
			t.Errorf("%s: state %v mode %v, want a read-only connect", id, st, tv.reqMode)
		}
		if age := time.Since(tv.lastUse); age < termPrefetchAge-time.Second {
			t.Errorf("%s ranks as viewed %v ago; a prefetch must rank older than viewed sessions", id, age)
		}
	}
	if views["ses_d"] != nil || views["ses_b"] != nil {
		t.Fatal("prefetched beyond the neighbours (or the selection, which openTermView owns)")
	}
	// A second rest prefetches nothing new (both are connecting).
	if cmd := m.prefetchTerms(); cmd != nil {
		if msg := cmd(); msg != nil {
			if b, ok := msg.(tea.BatchMsg); !ok || len(b) != 0 {
				t.Fatalf("re-prefetched connecting terminals: %#v", msg)
			}
		}
	}
	// Sessions on another machine without a relay would need ssh: never prefetched.
	m2 := warmModel(t, "ses_a", "ses_b")
	m2.opts.DialRelay = nil
	m2.view = viewSessions
	m2.sessList.selectID("ses_a")
	_ = m2.prefetchTerms()
	if m2.terms().views["ses_b"] != nil {
		t.Fatal("prefetched an ssh attach")
	}
}

// The embedded emulator keeps no history of its own (tmux does): a busy
// terminal's memory stays bounded however much it prints. (vt treats a
// scrollback size of 0 as "unchanged", which kept 10,000 lines, ~130 MB
// for a busy 120-column pane.)
func TestTermViewKeepsNoScrollback(t *testing.T) {
	tv := newTermView("ses_synth-sb", "run_synth-sb", 120, 40, nil)
	defer tv.close()
	for i := 0; i < 500; i++ {
		tv.feed([]byte(strings.Repeat("x", 118) + "\r\n"))
	}
	tv.feed([]byte("\x1b[2J")) // tmux clears on every attach
	tv.mu.Lock()
	n := tv.em.ScrollbackLen()
	tv.mu.Unlock()
	if n > 1 {
		t.Fatalf("emulator scrollback holds %d lines; want at most 1", n)
	}
}

// A prefetch answered with an ssh descriptor (a hub without the relay)
// starts no ssh client for a session that is not on screen.
func TestPrefetchNeverStartsSSH(t *testing.T) {
	m := warmModel(t, "ses_a", "ses_b")
	started := 0
	m.opts.StartTerm = func(contracts.AttachDescriptor, int, int) (TermConn, error) { started++; return newNopConn(), nil }
	m.view = viewSessions
	m.sessList.selectID("ses_a")
	s, _ := m.session("ses_b")
	tv := m.termViewFor(s)
	tv.reqTok++
	d := contracts.AttachDescriptor{Mechanism: contracts.AttachSSHTmux, ReadOnly: true, Command: []string{"ssh", "-t", "synth-mac", "tmux", "attach", "-r"}}
	_ = m.onTermAttach(termAttachMsg{sid: "ses_b", tok: tv.reqTok, mode: termReadOnly, d: d})
	if started != 0 || m.TermLive("ses_b") {
		t.Fatalf("an ssh attach started for a session off screen (%d starts)", started)
	}
	// The selected session's own answer still starts.
	sa, _ := m.session("ses_a")
	ta := m.termViewFor(sa)
	ta.reqTok++
	_ = m.onTermAttach(termAttachMsg{sid: "ses_a", tok: ta.reqTok, mode: termReadOnly, d: d})
	if started != 1 {
		t.Fatalf("the selected session's ssh attach did not start (%d starts)", started)
	}
}
