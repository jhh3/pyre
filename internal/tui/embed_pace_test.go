package tui

import (
	"context"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// Frame pacing: a watched pane repaints at ~30fps, the pane you type into
// at the renderer's rate, and the echo of a key at once.
func TestFrameGap(t *testing.T) {
	now := time.Unix(1000, 0)
	key := now.Add(-20 * time.Millisecond).UnixNano()
	old := now.Add(-2 * termEchoFor).UnixNano()
	cases := []struct {
		name string
		gap  time.Duration
		key  int64
		want time.Duration
	}{
		{"watched, never typed", termFrameGap, 0, termFrameGap},
		{"focused, idle", termFocusGap, old, termFocusGap},
		{"echo of a key", termFocusGap, key, termEchoGap},
		{"echo while the wheel holds", termFrameGap, key, termEchoGap},
	}
	for _, c := range cases {
		if got := frameGap(c.gap, c.key, now); got != c.want {
			t.Errorf("%s: frameGap = %v, want %v", c.name, got, c.want)
		}
	}
	if termFocusGap > time.Second/renderFPS+time.Millisecond {
		t.Errorf("focused gap %v is slower than the renderer (%d fps)", termFocusGap, renderFPS)
	}
}

// termSend stamps the key time the frame waiter reads.
func TestTermSendStampsKeyTime(t *testing.T) {
	m := New(context.Background(), nil, Options{Keymap: DefaultKeymap(), StateFile: "none", NoColor: true})
	tv := newTermView("ses_synth-pace", "run_synth-pace", 20, 6, nil)
	defer tv.close()
	tv.setConn(newNopConn(), termInteractive)
	m.terms().views[tv.sid] = tv
	before := time.Now().UnixNano()
	_ = m.termSend(tv, []byte("x"))
	if k := m.terms().lastKey.Load(); k < before {
		t.Fatalf("lastKey not stamped by termSend (%d < %d)", k, before)
	}
}

// The width memo answers like ansi.StringWidth, stays bounded, and pad
// still fits lines to exactly w cells.
func TestWidthMemoAndPad(t *testing.T) {
	lines := []string{
		"plain ascii line that is long enough to be memoised",
		"\x1b[1;31mred\x1b[m and 漢字 wide runes, long enough to be memoised",
		"short",
	}
	for i := 0; i < 3; i++ {
		for _, l := range lines {
			if got, want := width(l), ansi.StringWidth(l); got != want {
				t.Fatalf("width(%q) = %d, want %d", l, got, want)
			}
			for _, w := range []int{0, 3, 10, 40, 80} {
				if got := ansi.StringWidth(pad(l, w)); got != w {
					t.Fatalf("pad(%q, %d) is %d cells", l, w, got)
				}
			}
		}
	}
	for i := 0; i < 3*widthMemoCap; i++ {
		_ = width(fmt.Sprintf("a line long enough to be memoised number %d", i))
	}
	widthMemo.Lock()
	n := len(widthMemo.m)
	widthMemo.Unlock()
	if n > widthMemoCap {
		t.Fatalf("width memo holds %d lines, cap %d", n, widthMemoCap)
	}
}

// nopConn is a writable attach that accepts input and draws nothing.
type nopConn struct {
	done chan struct{}
	once *sync.Once
}

func newNopConn() nopConn { return nopConn{done: make(chan struct{}), once: &sync.Once{}} }

func (c nopConn) Read([]byte) (int, error)    { <-c.done; return 0, io.EOF }
func (c nopConn) Write(b []byte) (int, error) { return len(b), nil }
func (c nopConn) Close() error                { c.once.Do(func() { close(c.done) }); return nil }
func (c nopConn) Resize(int, int) error       { return nil }
