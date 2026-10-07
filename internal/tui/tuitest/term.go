package tuitest

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/tui"
)

// FakeTerm is an in-memory embedded terminal: what the TUI types is
// recorded, and the screen is redrawn by Screen (a synthetic Claude-like
// session by default) on start, on every resize and after every input.
type FakeTerm struct {
	D      contracts.AttachDescriptor
	Screen func(cols, rows int, typed string) string

	mu         sync.Mutex
	cols, rows int
	input      strings.Builder
	closed     bool
	pr         *io.PipeReader
	pw         *io.PipeWriter
	writes     chan string
}

// NewFakeTerm starts a fake terminal for d at cols x rows.
func NewFakeTerm(d contracts.AttachDescriptor, cols, rows int, screen func(cols, rows int, typed string) string) *FakeTerm {
	if screen == nil {
		screen = ClaudeScreen
	}
	f := &FakeTerm{D: d, Screen: screen, cols: cols, rows: rows, writes: make(chan string, 64)}
	f.pr, f.pw = io.Pipe()
	go func() {
		for s := range f.writes {
			if _, err := io.WriteString(f.pw, s); err != nil {
				return
			}
		}
	}()
	f.redraw()
	return f
}

func (f *FakeTerm) redraw() {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return
	}
	s := f.Screen(f.cols, f.rows, f.input.String())
	select {
	case f.writes <- s:
	default:
	}
	f.mu.Unlock()
}

func (f *FakeTerm) Read(b []byte) (int, error) { return f.pr.Read(b) }

func (f *FakeTerm) Write(b []byte) (int, error) {
	f.mu.Lock()
	f.input.Write(b)
	f.mu.Unlock()
	f.redraw()
	return len(b), nil
}

// Resize records the new size and redraws.
func (f *FakeTerm) Resize(cols, rows int) error {
	f.mu.Lock()
	f.cols, f.rows = cols, rows
	f.mu.Unlock()
	f.redraw()
	return nil
}

// Close ends the terminal (reads return EOF).
func (f *FakeTerm) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed {
		f.closed = true
		close(f.writes)
		_ = f.pw.Close()
	}
	return nil
}

// Input is everything typed into the terminal so far.
func (f *FakeTerm) Input() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.input.String()
}

// Closed reports whether the TUI closed the terminal.
func (f *FakeTerm) Closed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

// Size is the terminal's current size.
func (f *FakeTerm) Size() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cols, f.rows
}

// Terms returns the fake terminals started so far, oldest first.
func (h *Harness) Terms() []*FakeTerm {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]*FakeTerm(nil), h.terms...)
}

func (h *Harness) startTerm(d contracts.AttachDescriptor, cols, rows int) (tui.TermConn, error) {
	f := NewFakeTerm(d, cols, rows, nil)
	h.mu.Lock()
	h.terms = append(h.terms, f)
	h.mu.Unlock()
	return f, nil
}

// Embeddable wraps an API so attach requests return a local_tmux
// descriptor (the mock offers only read-only snapshots), letting the
// embedded terminal run against a fake or real terminal starter.
type Embeddable struct {
	contracts.API
	mu    sync.Mutex
	calls []contracts.AttachRequest
}

// NewEmbeddable wraps api.
func NewEmbeddable(api contracts.API) *Embeddable { return &Embeddable{API: api} }

// AttachSession returns a synthetic local_tmux descriptor for the
// session's current run (read-only when asked).
func (e *Embeddable) AttachSession(ctx context.Context, req contracts.AttachRequest) (contracts.AttachDescriptor, error) {
	e.mu.Lock()
	e.calls = append(e.calls, req)
	e.mu.Unlock()
	d, err := e.API.AttachSession(ctx, contracts.AttachRequest{Mutation: req.Mutation, SessionID: req.SessionID, ReadOnly: true})
	if err != nil {
		return d, err
	}
	d.Mechanism = contracts.AttachLocalTmux
	d.ReadOnly = req.ReadOnly
	d.Command = []string{"pyre-synthetic-term", req.SessionID, d.RunID}
	if req.ReadOnly {
		d.Command = append(d.Command, "-r")
	}
	d.Notes = "synthetic embedded terminal"
	return d, nil
}

// Attaches returns the attach requests seen so far.
func (e *Embeddable) Attaches() []contracts.AttachRequest {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]contracts.AttachRequest(nil), e.calls...)
}

// ClaudeScreen draws a synthetic Claude-Code-like session (obviously
// synthetic content) at cols x rows, with typed text in the input box and
// a tmux status line on the last row, the way an attached tmux client
// shows it.
func ClaudeScreen(cols, rows int, typed string) string {
	if cols < 20 || rows < 8 {
		return "\x1b[2J\x1b[H(synthetic terminal)"
	}
	var b strings.Builder
	b.WriteString("\x1b[?1049h\x1b[H\x1b[2J\x1b[?25h")
	line := func(y int, s string) {
		fmt.Fprintf(&b, "\x1b[%d;1H%s\x1b[m", y, s)
	}
	orange, dim, green, red, bold, reset := "\x1b[38;2;215;119;87m", "\x1b[2m", "\x1b[32m", "\x1b[31m", "\x1b[1m", "\x1b[m"
	inner := cols - 2
	bw := min(inner, 58)
	y := 1
	if rows >= 16 {
		y = 5
		line(1, orange+"╭"+strings.Repeat("─", bw-2)+"╮")
		line(2, orange+"│"+reset+" "+orange+"✻"+reset+bold+" Welcome to Claude Code (synthetic)"+reset+strings.Repeat(" ", max(0, bw-39))+orange+"│")
		line(3, orange+"│"+reset+dim+"   cwd: ~/src/synthetic-demo"+reset+strings.Repeat(" ", max(0, bw-30))+orange+"│")
		line(4, orange+"╰"+strings.Repeat("─", bw-2)+"╯")
	}
	body := []string{
		"",
		dim + "> " + reset + "fix the flaky retry test in internal/queue (synthetic)",
		"",
		"⏺ I'll look at the retry test first.",
		"",
		"⏺ " + bold + "Read" + reset + "(internal/queue/retry_test.go)",
		dim + "  ⎿  Read 84 lines" + reset,
		"",
		"⏺ The test sleeps 10ms and races the ticker. I'll inject the clock",
		"  instead of sleeping.",
		"",
		"⏺ " + bold + "Update" + reset + "(internal/queue/retry_test.go)",
		dim + "  ⎿  Updated internal/queue/retry_test.go with 3 additions and 2 removals" + reset,
		"       " + red + "-   time.Sleep(10 * time.Millisecond)" + reset,
		"       " + green + "+   clk.Advance(retryDelay)" + reset,
		"       " + green + "+   <-q.Retried()" + reset,
		"",
		"⏺ " + bold + "Bash" + reset + "(go test -count=50 ./internal/queue)",
		dim + "  ⎿  ok  	example.com/synthetic/internal/queue	1.284s" + reset,
		"",
		"⏺ Fixed: 50 runs pass. The test now drives a fake clock.",
	}
	boxTop := rows - 5 // box: 3 rows, hint row, status row
	start := 0
	if over := len(body) - (boxTop - y); over > 0 {
		start = min(over, len(body))
	}
	for _, l := range body[start:] {
		if y >= boxTop {
			break
		}
		line(y, l)
		y++
	}
	line(boxTop, dim+"╭"+strings.Repeat("─", inner-2)+"╮")
	in := strings.ReplaceAll(strings.ReplaceAll(typed, "\r", "⏎"), "\x1b", "^[")
	if len([]rune(in)) > inner-6 {
		r := []rune(in)
		in = string(r[len(r)-(inner-6):])
	}
	line(boxTop+1, dim+"│"+reset+" > "+in+strings.Repeat(" ", max(0, inner-5-len([]rune(in))))+dim+"│")
	line(boxTop+2, dim+"╰"+strings.Repeat("─", inner-2)+"╯")
	line(boxTop+3, dim+"  ? for shortcuts"+reset)
	status := " [synthetic] 0:claude*"
	line(rows, "\x1b[42;30m"+status+strings.Repeat(" ", max(0, cols-len(status))))
	fmt.Fprintf(&b, "\x1b[%d;%dH", boxTop+1, 5+len([]rune(in)))
	return b.String()
}
