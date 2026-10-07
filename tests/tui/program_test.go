package tui_test

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/tui"
	"github.com/jhh3/pyre/internal/tui/tuitest"
)

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) contains(s string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Contains(l.b.String(), s)
}

func waitUntil(t *testing.T, what string, d time.Duration, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestAttachSuspendsRealProgram runs the real Bubble Tea program (no
// terminal) and checks that A (full-screen attach) suspends the UI via tea.Exec, calls the
// CLI-supplied Attach, and resumes on the same filter and selection.
func TestAttachSuspendsRealProgram(t *testing.T) {
	b, clk := tuitest.NewMock(t)
	attached := make(chan contracts.AttachDescriptor, 4)
	opts := tuitest.Options()
	opts.Clock = clk
	opts.ExecFunc = nil // real tea.Exec
	opts.Attach = func(d contracts.AttachDescriptor) error {
		attached <- d
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	m := tui.New(ctx, b, opts)
	defer m.Close()
	inR, inW := io.Pipe()
	defer inW.Close()
	out := &lockedBuf{}
	p := tea.NewProgram(m, tea.WithInput(inR), tea.WithOutput(out), tea.WithContext(ctx), tea.WithoutSignalHandler(), tea.WithWindowSize(100, 30))
	type result struct {
		m   tea.Model
		err error
	}
	done := make(chan result, 1)
	go func() {
		fm, err := p.Run()
		done <- result{fm, err}
	}()
	p.Send(tea.WindowSizeMsg{Width: 100, Height: 30})
	waitUntil(t, "first render", 5*time.Second, func() bool { return out.contains("coordinator") })

	for _, r := range "/coordinator\r" {
		io.WriteString(inW, string(r)) // one keystroke per read, like a terminal
		time.Sleep(15 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	io.WriteString(inW, "F") // full-screen attach (enter types into the embedded terminal)
	var d contracts.AttachDescriptor
	select {
	case d = <-attached:
	case <-time.After(5 * time.Second):
		t.Fatal("Attach was not called")
	}
	if d.Mechanism != contracts.AttachSSHTmux {
		t.Fatalf("mechanism = %s", d.Mechanism)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !out.contains("detached from coordinator") {
		if time.Now().After(deadline) {
			out.mu.Lock()
			s := out.b.String()
			out.mu.Unlock()
			if len(s) > 4000 {
				s = s[len(s)-4000:]
			}
			t.Fatalf("no resume after detach; output tail:\n%q", s)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Quit. The input reader may lose a byte across the exec boundary, so
	// repeat q until the program exits.
	var res result
	for i := 0; ; i++ {
		io.WriteString(inW, "q")
		select {
		case res = <-done:
		case <-time.After(300 * time.Millisecond):
			if i > 20 {
				t.Fatal("program did not quit")
			}
			continue
		}
		break
	}
	if res.err != nil {
		t.Fatalf("run: %v", res.err)
	}
	fm := res.m.(*tui.Model)
	if fm.Filter() != "coordinator" || fm.SelectedSession() != "ses_synth-a" || fm.ActiveView() != "Sessions" {
		t.Fatalf("after attach: filter=%q sel=%s view=%s", fm.Filter(), fm.SelectedSession(), fm.ActiveView())
	}
}
