package tui_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/tui"
	"github.com/jhh3/pyre/internal/tui/tuitest"
)

// slowAttach delays sessions.attach (a busy hub: p90 ~0.7 s on server), so a
// pane that waited for a new attach could not show content on the first
// frame after a switch.
type slowAttach struct {
	contracts.API
	delay time.Duration
}

func (s slowAttach) AttachSession(ctx context.Context, req contracts.AttachRequest) (contracts.AttachDescriptor, error) {
	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
		return contracts.AttachDescriptor{}, ctx.Err()
	}
	return s.API.AttachSession(ctx, req)
}

// perSessionTerms starts fake terminals whose screen names their session
// ("synthetic screen of <sid>"), recording them by session.
type perSessionTerms struct {
	mu    sync.Mutex
	terms map[string][]*tuitest.FakeTerm
}

func (p *perSessionTerms) start(d contracts.AttachDescriptor, cols, rows int) (tui.TermConn, error) {
	sid := d.Command[1] // tuitest.Embeddable puts the session in the synthetic command
	f := tuitest.NewFakeTerm(d, cols, rows, func(int, int, string) string {
		return "\x1b[H\x1b[2J\x1b[3;1Hsynthetic screen of " + sid // the pane starts one row down when the last row is not a tmux status line
	})
	p.mu.Lock()
	p.terms[sid] = append(p.terms[sid], f)
	p.mu.Unlock()
	return f, nil
}

func (p *perSessionTerms) last(sid string) *tuitest.FakeTerm {
	p.mu.Lock()
	defer p.mu.Unlock()
	ts := p.terms[sid]
	if len(ts) == 0 {
		return nil
	}
	return ts[len(ts)-1]
}

func attachesOf(rec *tuitest.Recorder, sid string) int {
	n := 0
	for _, c := range rec.Calls("AttachSession") {
		if c.Request.(contracts.AttachRequest).SessionID == sid {
			n++
		}
	}
	return n
}

func startFastSwitch(t *testing.T) (*tuitest.Harness, *tuitest.Recorder, *perSessionTerms) {
	t.Helper()
	terms := &perSessionTerms{terms: map[string][]*tuitest.FakeTerm{}}
	var rec *tuitest.Recorder
	h, _ := start(t, 160, 40, func(a contracts.API) contracts.API {
		rec = tuitest.NewRecorder(slowAttach{API: tuitest.NewEmbeddable(a), delay: 400 * time.Millisecond})
		return rec
	}, func(o *tui.Options) { o.StartTerm = terms.start })
	return h, rec, terms
}

// Switching back to sessions viewed a moment ago shows each one's screen,
// live, on the first frame after the key: their read-only streams stayed
// warm, so no new attach is made (owner report 2026-10-13: "it feels like
// I was just there"). Four sessions are walked down, then back up.
func TestSwitchBackPaintsWarmScreenOnFirstFrame(t *testing.T) {
	h, rec, _ := startFastSwitch(t)
	selectSession(t, h, "ses_synth-a")
	h.WaitFor("a's screen", 5*sec, func() bool { return h.Contains("synthetic screen of ses_synth-a") })
	path := []string{"ses_synth-a"}
	for i := 0; i < 3; i++ {
		h.Key("down")
		sid := h.M.SelectedSession()
		h.WaitFor(sid+"'s screen", 5*sec, func() bool { return h.Contains("synthetic screen of " + sid) })
		path = append(path, sid)
	}
	h.Settle()
	before := map[string]int{}
	for _, sid := range path {
		before[sid] = attachesOf(rec, sid)
	}
	for i := len(path) - 2; i >= 0; i-- {
		h.Key("up") // one Update and one render: the first frame
		sid := path[i]
		if h.M.SelectedSession() != sid {
			t.Fatalf("up selected %s, want %s", h.M.SelectedSession(), sid)
		}
		if v := h.View(); !strings.Contains(v, "synthetic screen of "+sid) || strings.Contains(v, "last screen") || !h.M.TermLive(sid) {
			t.Fatalf("first frame after switching back to %s lacks its live screen (live %v):\n%s", sid, h.M.TermLive(sid), v)
		}
	}
	h.Settle()
	for _, sid := range path {
		if n := attachesOf(rec, sid) - before[sid]; n != 0 {
			t.Errorf("switching back re-attached %s %d times; its stream should have stayed warm", sid, n)
		}
	}
}

// A recently viewed session whose stream has closed still paints its last
// screen on the first frame (dimmed, "last screen" at the header's right),
// and the live one replaces it once the new attach lands.
func TestSwitchBackPaintsLastScreenWhileReconnecting(t *testing.T) {
	h, rec, terms := startFastSwitch(t)
	selectSession(t, h, "ses_synth-a")
	h.WaitFor("a's screen", 5*sec, func() bool { return h.Contains("synthetic screen of ses_synth-a") })
	h.Key("down")
	b := h.M.SelectedSession()
	h.WaitFor("b's screen", 5*sec, func() bool { return h.Contains("synthetic screen of " + b) })
	time.Sleep(500 * time.Millisecond) // past the prefetch dwell: neighbours are warm
	h.Settle()
	live := h.M.LiveTerms()
	terms.last("ses_synth-a").Close() // a's stream ends in the background
	h.WaitFor("a's stream ended", 3*sec, func() bool { return h.M.LiveTerms() < live })
	before := attachesOf(rec, "ses_synth-a")

	h.Key("up")
	v := h.View()
	if !strings.Contains(v, "synthetic screen of ses_synth-a") {
		t.Fatalf("first frame after switching back lacks a's last screen:\n%s", v)
	}
	if !strings.Contains(v, "last screen") || strings.Contains(v, "connecting to") {
		t.Fatalf("a's last screen is not marked as such:\n%s", v)
	}
	h.WaitFor("a live again", 5*sec, func() bool {
		return attachesOf(rec, "ses_synth-a") > before && h.Contains("live · enter to type")
	})
}
