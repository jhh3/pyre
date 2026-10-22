// Package tuitest drives the Pyre TUI model in-process for tests and demos:
// a small Bubble Tea runtime (commands run on goroutines, messages are fed
// back through Update on the test goroutine), plus API wrappers that add
// latency or record mutations.
package tuitest

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/mock"
	"github.com/jhh3/pyre/internal/tui"
)

// Owner is the synthetic owner principal used with the mock backend.
var Owner = contracts.Principal{Kind: contracts.PrincipalOwner, ID: "cli_synth-laptop", Name: "owner (synthetic)"}

// ContractFixtureDir returns tests/fixtures/contracts only: the frozen
// shared contract fixtures. The mock's default also loads the evolving demo
// fleet (tests/fixtures/mock), which would make TUI goldens drift.
func ContractFixtureDir() string {
	for _, d := range mock.DefaultFixtureDirs() {
		if filepath.Base(d) == "contracts" {
			return d
		}
	}
	return ""
}

// NewMock returns a mock backend seeded with ONLY the contract fixtures,
// on a fake clock fixed just after the fixture epoch, so views are
// deterministic.
func NewMock(t testing.TB) (*mock.Backend, *mock.FakeClock) {
	t.Helper()
	dir := ContractFixtureDir()
	if dir == "" {
		t.Fatal("tests/fixtures/contracts not found")
	}
	clk := mock.NewFakeClock(mock.FixtureEpoch.Add(30 * time.Second))
	owner := Owner
	b, err := mock.NewE(mock.Options{Clock: clk, DefaultPrincipal: &owner, FixtureDirs: []string{dir}})
	if err != nil {
		t.Fatalf("mock: %v", err)
	}
	return b, clk
}

// Harness runs a tui.Model without a terminal.
type Harness struct {
	T           testing.TB
	M           *tui.Model
	msgs        chan tea.Msg
	done        chan struct{}
	quit        bool
	once        sync.Once
	AttachCalls []contracts.AttachDescriptor
	mu          sync.Mutex
	terms       []*FakeTerm
	relays      []*RelayTerm // relay.go
	// RelayErr fails relay dials while set (SetRelayErr).
	RelayErr error
}

// Options returns test defaults: no periodic refresh, short debounce and
// preview delays, monochrome output, and synchronous exec.
func Options() tui.Options {
	return tui.Options{
		RefreshInterval: -1,
		Debounce:        30 * time.Millisecond,
		PreviewDelay:    5 * time.Millisecond,
		LoadTimeout:     2 * time.Second,
		NoColor:         true,
		Output:          io.Discard,
		KeysFile:        "none",          // never read the user's ~/.config/pyre/keys.toml
		StartTerm:       FakeTermStarter, // never run a real attach command
		// Never open a browser: I copies the web inbox link instead.
		OpenURL: func(string) error { return errors.New("tests open no browser") },
		// Never read the real clipboard: ctrl+v finds it empty.
		ReadClipboard: func(context.Context) (tui.ClipboardContent, error) { return tui.ClipboardContent{}, nil },
		// A selection never counts as viewed by wall-clock time passing
		// (lead signals, leads.go); tests that need it set a short dwell.
		SeenDwell: time.Hour,
	}
}

// FakeTermStarter starts a FakeTerm (a synthetic Claude-like screen) for
// any descriptor. Harnesses replace it with a recording starter.
func FakeTermStarter(d contracts.AttachDescriptor, cols, rows int) (tui.TermConn, error) {
	return NewFakeTerm(d, cols, rows, nil), nil
}

// New builds a harness around api. If opts.Attach is nil, attaches are
// recorded in AttachCalls and return immediately (as if the user detached).
// The model receives a WindowSizeMsg of w x h and its Init command runs.
func New(t testing.TB, api contracts.API, opts tui.Options, w, h int) *Harness {
	t.Helper()
	hs := &Harness{T: t, msgs: make(chan tea.Msg, 8192), done: make(chan struct{})}
	if opts.Attach == nil {
		opts.Attach = func(d contracts.AttachDescriptor) error {
			hs.mu.Lock()
			hs.AttachCalls = append(hs.AttachCalls, d)
			hs.mu.Unlock()
			return nil
		}
	}
	if opts.StartTerm == nil || reflect.ValueOf(opts.StartTerm).Pointer() == reflect.ValueOf(FakeTermStarter).Pointer() {
		// Embedded terminals never run a real command in tests unless the
		// test asks for one (tui.StartPTY against an isolated tmux).
		opts.StartTerm = hs.startTerm
	}
	if opts.DialRelay != nil && reflect.ValueOf(opts.DialRelay).Pointer() == reflect.ValueOf(FakeRelayDialer).Pointer() {
		opts.DialRelay = hs.dialRelay // relayed views get recording fakes
	}
	if opts.ExecFunc == nil {
		// Run the suspended command synchronously inside the command
		// goroutine, then deliver its callback, as tea.Exec would.
		opts.ExecFunc = func(c tea.ExecCommand, fn tea.ExecCallback) tea.Cmd {
			return func() tea.Msg { return fn(c.Run()) }
		}
	}
	hs.M = tui.New(context.Background(), api, opts)
	t.Cleanup(hs.Close)
	hs.Send(tea.WindowSizeMsg{Width: w, Height: h})
	hs.run(hs.M.Init())
	return hs
}

// Close stops background work.
func (h *Harness) Close() {
	h.once.Do(func() {
		close(h.done)
		h.M.Close()
	})
}

// Attaches returns a copy of recorded attach descriptors.
func (h *Harness) Attaches() []contracts.AttachDescriptor {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]contracts.AttachDescriptor(nil), h.AttachCalls...)
}

func (h *Harness) run(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	go func() {
		h.deliver(cmd())
	}()
}

func (h *Harness) deliver(msg tea.Msg) {
	switch msg := msg.(type) {
	case nil:
		return
	case tea.BatchMsg:
		for _, c := range msg {
			h.run(c)
		}
		return
	}
	select {
	case h.msgs <- msg:
	case <-h.done:
	}
}

// Send applies msg through Update on the calling goroutine.
func (h *Harness) Send(msg tea.Msg) {
	if _, ok := msg.(tea.QuitMsg); ok {
		h.quit = true
		return
	}
	_, cmd := h.M.Update(msg)
	h.M.View() // Bubble Tea renders after every Update
	h.runCmd(cmd)
}

func (h *Harness) runCmd(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	// tea.Quit is detected by running it inline (it is instantaneous).
	h.run(cmd)
}

// Key sends key presses. Names: enter esc tab shift+tab up down left right
// backspace space pgup pgdown home end ctrl+<x>; anything else is typed as
// runes (so "abc" types three characters).
func (h *Harness) Key(keys ...string) {
	for _, k := range keys {
		h.Send(KeyMsg(k))
		h.View() // a real program renders between keys
	}
}

// Type types text as individual rune key presses.
func (h *Harness) Type(s string) {
	for _, r := range s {
		h.Send(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
}

// Paste delivers text as a bracketed paste.
func (h *Harness) Paste(s string) { h.Send(tea.PasteMsg{Content: s}) }

var named = map[string]rune{
	"enter": tea.KeyEnter, "esc": tea.KeyEscape, "tab": tea.KeyTab, "up": tea.KeyUp, "down": tea.KeyDown,
	"left": tea.KeyLeft, "right": tea.KeyRight, "backspace": tea.KeyBackspace, "space": tea.KeySpace,
	"pgup": tea.KeyPgUp, "pgdown": tea.KeyPgDown, "home": tea.KeyHome, "end": tea.KeyEnd,
}

// KeyMsg converts a key name to a key press. Names: enter esc tab up down
// left right backspace space pgup pgdown home end, with optional ctrl+,
// alt+ and shift+ prefixes (ctrl+d, shift+enter, alt+enter); anything else
// is a single rune ("j", "G", ":").
func KeyMsg(k string) tea.KeyPressMsg {
	var mod tea.KeyMod
	rest := k
	for {
		switch {
		case strings.HasPrefix(rest, "ctrl+") && len(rest) > 5:
			mod |= tea.ModCtrl
			rest = rest[5:]
			continue
		case strings.HasPrefix(rest, "alt+") && len(rest) > 4:
			mod |= tea.ModAlt
			rest = rest[4:]
			continue
		case strings.HasPrefix(rest, "shift+") && len(rest) > 6:
			mod |= tea.ModShift
			rest = rest[6:]
			continue
		}
		break
	}
	if c, ok := named[rest]; ok {
		msg := tea.KeyPressMsg{Code: c, Mod: mod}
		if c == tea.KeySpace && mod == 0 {
			msg.Text = " "
		}
		return msg
	}
	r := []rune(rest)
	msg := tea.KeyPressMsg{Code: r[0], Mod: mod}
	if mod == 0 || mod == tea.ModShift {
		msg.Text = rest
	}
	return msg
}

// step processes one pending message, waiting up to d. Reports whether a
// message was processed.
func (h *Harness) step(d time.Duration) bool {
	select {
	case msg := <-h.msgs:
		h.Send(msg)
		return true
	case <-time.After(d):
		return false
	}
}

// Settle processes messages until none arrive for idle (default 80ms) or
// max (default 5s) elapses.
func (h *Harness) Settle() {
	h.SettleFor(80*time.Millisecond, 5*time.Second)
}

// SettleFor is Settle with explicit idle and max durations.
func (h *Harness) SettleFor(idle, max time.Duration) {
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) {
		if !h.step(idle) {
			return
		}
	}
}

// WaitFor processes messages until cond holds or timeout; it fails the
// test on timeout.
func (h *Harness) WaitFor(what string, timeout time.Duration, cond func() bool) {
	h.T.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			h.T.Fatalf("timed out waiting for %s; view:\n%s", what, h.View())
		}
		h.step(10 * time.Millisecond)
	}
}

// View renders the current screen with ANSI styling stripped.
func (h *Harness) View() string {
	return ansi.Strip(h.M.View().Content)
}

// Contains reports whether the rendered view contains s.
func (h *Harness) Contains(s string) bool { return strings.Contains(h.View(), s) }

// ---- API wrappers ----

// Slow adds latency to read calls (honoring ctx), simulating a slow hub or
// an unreachable node. Delay can be changed at any time.
type Slow struct {
	contracts.API
	mu    sync.Mutex
	delay time.Duration
	Only  map[string]bool // ops to slow ("ListSessions", ...); nil = all wrapped reads
	calls map[string]int
}

// NewSlow wraps api with delay d.
func NewSlow(api contracts.API, d time.Duration) *Slow {
	return &Slow{API: api, delay: d, calls: map[string]int{}}
}

// SetDelay changes the latency.
func (s *Slow) SetDelay(d time.Duration) {
	s.mu.Lock()
	s.delay = d
	s.mu.Unlock()
}

// Calls returns how many times op was called.
func (s *Slow) Calls(op string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[op]
}

func (s *Slow) wait(ctx context.Context, op string) error {
	s.mu.Lock()
	d := s.delay
	s.calls[op]++
	only := s.Only
	s.mu.Unlock()
	if only != nil && !only[op] {
		return nil
	}
	select {
	case <-time.After(d):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Slow) ListSessions(ctx context.Context, r contracts.ListSessionsRequest) (contracts.Page[contracts.SessionView], error) {
	if err := s.wait(ctx, "ListSessions"); err != nil {
		return contracts.Page[contracts.SessionView]{}, err
	}
	return s.API.ListSessions(ctx, r)
}

func (s *Slow) ReadSession(ctx context.Context, r contracts.ReadSessionRequest) (contracts.ReadSessionResponse, error) {
	if err := s.wait(ctx, "ReadSession"); err != nil {
		return contracts.ReadSessionResponse{}, err
	}
	return s.API.ReadSession(ctx, r)
}

func (s *Slow) ListNodes(ctx context.Context, r contracts.ListNodesRequest) (contracts.Page[contracts.Node], error) {
	if err := s.wait(ctx, "ListNodes"); err != nil {
		return contracts.Page[contracts.Node]{}, err
	}
	return s.API.ListNodes(ctx, r)
}

func (s *Slow) ListAttention(ctx context.Context, r contracts.ListAttentionRequest) (contracts.Page[contracts.AttentionItem], error) {
	if err := s.wait(ctx, "ListAttention"); err != nil {
		return contracts.Page[contracts.AttentionItem]{}, err
	}
	return s.API.ListAttention(ctx, r)
}

func (s *Slow) InvokeNodeAction(ctx context.Context, r contracts.NodeActionRequest) (contracts.NodeActionResult, error) {
	if err := s.wait(ctx, "InvokeNodeAction"); err != nil {
		return contracts.NodeActionResult{}, err
	}
	return s.API.InvokeNodeAction(ctx, r)
}

// Call is one recorded mutation.
type Call struct {
	Op          string
	OperationID string
	Request     any
}

// Recorder records selected mutations (with their operation ids) before
// forwarding them.
type Recorder struct {
	contracts.API
	mu    sync.Mutex
	calls []Call
}

// NewRecorder wraps api.
func NewRecorder(api contracts.API) *Recorder { return &Recorder{API: api} }

func (r *Recorder) rec(op, id string, req any) {
	r.mu.Lock()
	r.calls = append(r.calls, Call{Op: op, OperationID: id, Request: req})
	r.mu.Unlock()
}

// Calls returns recorded calls for op ("" = all).
func (r *Recorder) Calls(op string) []Call {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Call
	for _, c := range r.calls {
		if op == "" || c.Op == op {
			out = append(out, c)
		}
	}
	return out
}

func (r *Recorder) AnswerApproval(ctx context.Context, req contracts.AnswerApprovalRequest) (contracts.Approval, error) {
	r.rec("AnswerApproval", req.OperationID, req)
	return r.API.AnswerApproval(ctx, req)
}

func (r *Recorder) SendMessage(ctx context.Context, req contracts.SendMessageRequest) (contracts.MessageView, error) {
	r.rec("SendMessage", req.OperationID, req)
	return r.API.SendMessage(ctx, req)
}

func (r *Recorder) AttachSession(ctx context.Context, req contracts.AttachRequest) (contracts.AttachDescriptor, error) {
	r.rec("AttachSession", req.OperationID, req)
	return r.API.AttachSession(ctx, req)
}

func (r *Recorder) AcquireLease(ctx context.Context, req contracts.LeaseRequest) (contracts.InputLease, error) {
	r.rec("AcquireLease", req.OperationID, req)
	return r.API.AcquireLease(ctx, req)
}

func (r *Recorder) ReleaseLease(ctx context.Context, req contracts.LeaseReleaseRequest) (contracts.InputLease, error) {
	r.rec("ReleaseLease", req.OperationID, req)
	return r.API.ReleaseLease(ctx, req)
}

func (r *Recorder) StartSession(ctx context.Context, req contracts.StartSessionRequest) (contracts.StartSessionResponse, error) {
	r.rec("StartSession", req.OperationID, req)
	return r.API.StartSession(ctx, req)
}

func (r *Recorder) InterruptSession(ctx context.Context, req contracts.SessionCommandRequest) (contracts.CommandReceipt, error) {
	r.rec("InterruptSession", req.OperationID, req)
	return r.API.InterruptSession(ctx, req)
}

func (r *Recorder) ResolveDelivery(ctx context.Context, req contracts.ResolveDeliveryRequest) (contracts.MessageView, error) {
	r.rec("ResolveDelivery", req.OperationID, req)
	return r.API.ResolveDelivery(ctx, req)
}

func (r *Recorder) ReissueMessage(ctx context.Context, req contracts.ReissueMessageRequest) (contracts.MessageView, error) {
	r.rec("ReissueMessage", req.OperationID, req)
	return r.API.ReissueMessage(ctx, req)
}

func (r *Recorder) CreateComment(ctx context.Context, req contracts.CreateCommentRequest) (contracts.ReviewComment, error) {
	r.rec("CreateComment", req.OperationID, req)
	return r.API.CreateComment(ctx, req)
}

func (r *Recorder) StopSession(ctx context.Context, req contracts.SessionCommandRequest) (contracts.CommandReceipt, error) {
	r.rec("StopSession", req.OperationID, req)
	return r.API.StopSession(ctx, req)
}

func (r *Recorder) UpdateSession(ctx context.Context, req contracts.UpdateSessionRequest) (contracts.SessionView, error) {
	r.rec("UpdateSession", req.OperationID, req)
	return r.API.UpdateSession(ctx, req)
}
