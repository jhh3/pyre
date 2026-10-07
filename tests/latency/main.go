// Command latency measures keystroke-to-echo latency through an outer pty,
// the way a person sees it: a key is written to the outer terminal and the
// time until the echoed glyph appears in that terminal's output is recorded.
//
//	native: the outer pty runs the attach command itself (tmux attach, or
//	        ssh -t host tmux attach): what a native terminal window shows.
//	pyre:   the outer pty runs the Pyre TUI (in process, real Bubble Tea
//	        program on the pty) with the same command embedded as the
//	        focused session's terminal (writable attach, input lease held).
//
// The target is a session running tests/latency/latapp.py (echo or ink) on
// an isolated tmux server; see tests/latency/run_local.sh. Synthetic data
// only: the TUI runs against the in-process mock backend.
//
//	go run ./tests/latency -mode native -cmd "tmux -L pyre-wlat attach -t lat"
//	go run ./tests/latency -mode pyre   -cmd "tmux -L pyre-wlat attach -t lat"
package main

import (
	"context"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/vt"
	"github.com/creack/pty"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/mock"
	"github.com/jhh3/pyre/internal/tui"
)

func main() {
	mode := flag.String("mode", "native", "native | pyre | both (both outer terminals at once, keys alternating: network jitter hits both alike)")
	cmdline := flag.String("cmd", "", "attach command (space separated); pyre appends -r for the read-only view")
	n := flag.Int("n", 200, "samples (per side)")
	cols := flag.Int("cols", 160, "outer terminal columns")
	rows := flag.Int("rows", 45, "outer terminal rows")
	gapMin := flag.Duration("gap", 40*time.Millisecond, "minimum pause between keys (a random 0-40ms is added)")
	warm := flag.Duration("warm", 2*time.Second, "settle time before measuring")
	dump := flag.Bool("dump", false, "print the outer screen at the end")
	label := flag.String("label", "", "label for the result line")
	flag.Parse()
	argv := strings.Fields(*cmdline)
	if len(argv) == 0 {
		fail("-cmd is required")
	}
	modes := []string{*mode}
	if *mode == "both" {
		modes = []string{"native", "pyre"}
	}
	var sides []*side
	stopAll := func() {
		for _, sd := range sides {
			sd.stop()
		}
	}
	for _, md := range modes {
		sd := startSide(md, argv, *cols, *rows)
		sides = append(sides, sd)
		if !sd.mon.waitText("lat-ready", 15*time.Second) {
			sd.mon.dump()
			stopAll()
			fail("%s: target never drew lat-ready", md)
		}
		if md == "pyre" {
			_, _ = sd.master.Write([]byte("\r")) // enter: focus the pane (writable attach, input lease)
			time.Sleep(1500 * time.Millisecond)
		}
	}
	time.Sleep(*warm)

	letters := "abcdefgijkopquvwxyz" // no letters the TUI chrome or status flips draw by chance
	for i := 0; i < *n*len(sides); i++ {
		sd := sides[i%len(sides)]
		ch := letters[(i/len(sides))%len(letters)]
		if len(sides) > 1 {
			ch = letters[i%len(letters)]
		}
		mark := sd.mon.mark()
		t0 := time.Now()
		_, _ = sd.master.Write([]byte{ch})
		if at, ok := sd.mon.waitByte(mark, ch, 2*time.Second); ok {
			sd.lat = append(sd.lat, at.Sub(t0))
		} else {
			sd.missed++
		}
		time.Sleep(*gapMin + time.Duration(rand.Int63n(int64(40*time.Millisecond))))
	}
	if *dump {
		for _, sd := range sides {
			sd.mon.dump()
		}
	}
	stopAll()
	for _, sd := range sides {
		l := *label
		if len(sides) > 1 || l == "" {
			l = strings.TrimSpace(l + " " + sd.mode)
		}
		report(l, sd.mode, sd.lat, sd.missed)
	}
}

// side is one outer terminal under measurement.
type side struct {
	mode   string
	master *os.File
	mon    *monitor
	stop   func()
	lat    []time.Duration
	missed int
}

func startSide(mode string, argv []string, cols, rows int) *side {
	sd := &side{mode: mode, stop: func() {}}
	switch mode {
	case "native":
		c := exec.Command(argv[0], argv[1:]...)
		c.Env = append(os.Environ(), "TERM=xterm-256color")
		f, err := pty.StartWithSize(c, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
		if err != nil {
			fail("start: %v", err)
		}
		sd.master = f
		sd.stop = func() { _ = c.Process.Kill(); _ = c.Wait() }
	case "pyre":
		m, s, err := pty.Open()
		if err != nil {
			fail("pty: %v", err)
		}
		_ = pty.Setsize(m, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
		sd.master = m
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			if err := runPyre(ctx, s, argv); err != nil && ctx.Err() == nil {
				fmt.Fprintln(os.Stderr, "pyre:", err)
			}
		}()
		sd.stop = func() {
			cancel()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
			}
			_ = s.Close()
		}
	default:
		fail("unknown -mode %q", mode)
	}
	sd.mon = newMonitor(cols, rows)
	go sd.mon.read(sd.master)
	return sd
}

func report(label, mode string, lat []time.Duration, missed int) {
	if label == "" {
		label = mode
	}
	if len(lat) == 0 {
		fmt.Printf("%-28s no samples (missed %d)\n", label, missed)
		return
	}
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	pct := func(p float64) float64 {
		i := int(p * float64(len(lat)-1))
		return float64(lat[i].Microseconds()) / 1000
	}
	var sum time.Duration
	for _, d := range lat {
		sum += d
	}
	fmt.Printf("%-28s n=%d missed=%d  p50=%.1fms  p95=%.1fms  p99=%.1fms  max=%.1fms  mean=%.1fms\n",
		label, len(lat), missed, pct(0.5), pct(0.95), pct(0.99), pct(1), float64((sum/time.Duration(len(lat))).Microseconds())/1000)
}

// monitor reads the outer pty, strips escape sequences into a printable
// text stream (with arrival times per byte), and keeps a screen emulator
// for -dump.
type monitor struct {
	mu    sync.Mutex
	cond  *sync.Cond
	text  []byte
	times []time.Time
	esc   int // escape parser state
	em    *vt.Emulator
}

func newMonitor(cols, rows int) *monitor {
	m := &monitor{em: vt.NewEmulator(cols, rows)}
	m.cond = sync.NewCond(&m.mu)
	go func() { // drain emulator replies
		buf := make([]byte, 256)
		for {
			if _, err := m.em.Read(buf); err != nil {
				return
			}
		}
	}()
	return m
}

const (
	stText = iota
	stEsc
	stCSI
	stStr    // OSC/DCS/APC/PM/SOS: until BEL or ST
	stStrEsc // ESC inside a string
	stCharset
)

func (m *monitor) read(f *os.File) {
	buf := make([]byte, 64*1024)
	for {
		n, err := f.Read(buf)
		if n > 0 {
			now := time.Now()
			m.mu.Lock()
			_, _ = m.em.Write(buf[:n])
			for _, b := range buf[:n] {
				if c, ok := m.step(b); ok {
					m.text = append(m.text, c)
					m.times = append(m.times, now)
				}
			}
			m.cond.Broadcast()
			m.mu.Unlock()
		}
		if err != nil {
			m.mu.Lock()
			m.cond.Broadcast()
			m.mu.Unlock()
			return
		}
	}
}

// step advances the escape-sequence stripper; ok when b is printable text.
func (m *monitor) step(b byte) (byte, bool) {
	switch m.esc {
	case stText:
		switch {
		case b == 0x1b:
			m.esc = stEsc
		case b == 0x9b:
			m.esc = stCSI
		case b >= 0x20 && b != 0x7f:
			return b, true
		}
	case stEsc:
		switch {
		case b == '[':
			m.esc = stCSI
		case b == ']' || b == 'P' || b == '_' || b == '^' || b == 'X':
			m.esc = stStr
		case b == '(' || b == ')' || b == '*' || b == '+' || b == '#' || b == ' ' || b == '%':
			m.esc = stCharset
		default:
			m.esc = stText
		}
	case stCSI:
		if b >= 0x40 && b <= 0x7e {
			m.esc = stText
		}
	case stStr:
		if b == 0x07 {
			m.esc = stText
		} else if b == 0x1b {
			m.esc = stStrEsc
		}
	case stStrEsc:
		if b == '\\' {
			m.esc = stText
		} else {
			m.esc = stStr
		}
	case stCharset:
		m.esc = stText
	}
	return 0, false
}

func (m *monitor) mark() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.text)
}

// waitByte waits for printable byte c at or after offset from.
func (m *monitor) waitByte(from int, c byte, timeout time.Duration) (time.Time, bool) {
	deadline := time.Now().Add(timeout)
	t := time.AfterFunc(timeout, func() { m.mu.Lock(); m.cond.Broadcast(); m.mu.Unlock() })
	defer t.Stop()
	m.mu.Lock()
	defer m.mu.Unlock()
	for {
		for i := from; i < len(m.text); i++ {
			if m.text[i] == c {
				return m.times[i], true
			}
		}
		from = len(m.text)
		if time.Now().After(deadline) {
			return time.Time{}, false
		}
		m.cond.Wait()
	}
}

func (m *monitor) waitText(s string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		ok := strings.Contains(string(m.text), s) || strings.Contains(m.em.String(), s)
		m.mu.Unlock()
		if ok {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

func (m *monitor) dump() {
	m.mu.Lock()
	defer m.mu.Unlock()
	fmt.Fprintln(os.Stderr, "----- outer screen -----")
	fmt.Fprintln(os.Stderr, m.em.String())
	fmt.Fprintln(os.Stderr, "------------------------")
}

func fail(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "latency: "+f+"\n", a...)
	os.Exit(1)
}

// runPyre runs the Pyre TUI on the pty slave against the synthetic mock,
// showing one embeddable session whose attach command is argv.
func runPyre(ctx context.Context, tty *os.File, argv []string) error {
	owner := contracts.Principal{Kind: contracts.PrincipalOwner, ID: "cli_synth-latency", Name: "owner (synthetic)"}
	clk := mock.NewFakeClock(mock.FixtureEpoch.Add(30 * time.Second))
	b, err := mock.NewE(mock.Options{Clock: clk, DefaultPrincipal: &owner})
	if err != nil {
		return err
	}
	api := &oneSession{API: b, argv: argv}
	opts := tui.Options{
		Clock:           clk,
		RefreshInterval: -1,
		KeysFile:        "none",
		StateFile:       "none",
		Output:          tty,
		Attach:          func(contracts.AttachDescriptor) error { return nil },
		Edit:            func(string) error { return nil },
		StartTerm:       tui.StartPTY,
		ProgramOptions: []tea.ProgramOption{
			tea.WithInput(tty),
			tea.WithEnvironment([]string{"TERM=xterm-256color", "COLORTERM=truecolor"}),
		},
	}
	return tui.Run(ctx, api, opts)
}

// oneSession narrows the mock to one live, attach-capable session on a
// connected node, and answers its attach with argv.
type oneSession struct {
	contracts.API
	argv []string
	once sync.Once
	sid  string
}

func (o *oneSession) pick(ctx context.Context) {
	o.once.Do(func() {
		nodes, _ := o.API.ListNodes(ctx, contracts.ListNodesRequest{})
		up := map[string]bool{}
		for _, n := range nodes.Items {
			up[n.NodeID] = n.Connection.State == contracts.ConnConnected
		}
		page, _ := o.API.ListSessions(ctx, contracts.ListSessionsRequest{})
		for _, s := range page.Items {
			if s.Run != nil && !s.Run.State.Terminal() && s.Run.State != contracts.RunStarting && contracts.HasCap(s.Run.Capabilities, contracts.CapAttachNative) && up[s.Run.NodeID] {
				o.sid = s.Session.SessionID
				return
			}
		}
	})
}

func (o *oneSession) ListSessions(ctx context.Context, r contracts.ListSessionsRequest) (contracts.Page[contracts.SessionView], error) {
	o.pick(ctx)
	p, err := o.API.ListSessions(ctx, r)
	if err != nil {
		return p, err
	}
	var keep []contracts.SessionView
	for _, s := range p.Items {
		if s.Session.SessionID == o.sid {
			keep = append(keep, s)
		}
	}
	p.Items = keep
	return p, nil
}

func (o *oneSession) AttachSession(ctx context.Context, req contracts.AttachRequest) (contracts.AttachDescriptor, error) {
	d, err := o.API.AttachSession(ctx, contracts.AttachRequest{Mutation: req.Mutation, SessionID: req.SessionID, ReadOnly: true})
	if err != nil {
		return d, err
	}
	d.Mechanism, d.ReadOnly = contracts.AttachLocalTmux, req.ReadOnly
	d.Command = append([]string(nil), o.argv...)
	if req.ReadOnly {
		d.Command = append(d.Command, "-r")
	}
	return d, nil
}
