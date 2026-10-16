package tuitest

import (
	"context"
	"sync"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/tui"
)

// Relayed wraps an API like a hub with a terminal relay (contracts.
// TerminalFit): attach requests that prefer web_terminal get a
// web_terminal descriptor with a synthetic ticket, Hello advertises
// terminal.fit unless NoHubFit, and every node advertises it unless
// NoNodeFit. Other requests pass through (wrap an Embeddable for native
// descriptors).
type Relayed struct {
	contracts.API
	NoHubFit  bool
	NoNodeFit bool

	mu    sync.Mutex
	calls []contracts.AttachRequest
}

// NewRelayed wraps api.
func NewRelayed(api contracts.API) *Relayed { return &Relayed{API: api} }

func (r *Relayed) AttachSession(ctx context.Context, req contracts.AttachRequest) (contracts.AttachDescriptor, error) {
	r.mu.Lock()
	r.calls = append(r.calls, req)
	r.mu.Unlock()
	if len(req.Preferred) == 0 || req.Preferred[0] != contracts.AttachWebTerminal {
		return r.API.AttachSession(ctx, req)
	}
	d, err := r.API.AttachSession(ctx, contracts.AttachRequest{Mutation: req.Mutation, SessionID: req.SessionID, ReadOnly: true})
	if err != nil {
		return d, err
	}
	d.Mechanism, d.Command, d.URL, d.Ticket, d.ReadOnly = contracts.AttachWebTerminal, nil, contracts.PathTerminal, "pyt_synthetic-relay-ticket", req.ReadOnly
	d.Notes = "synthetic web terminal"
	return d, nil
}

// Attaches returns the attach requests seen so far.
func (r *Relayed) Attaches() []contracts.AttachRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]contracts.AttachRequest(nil), r.calls...)
}

func (r *Relayed) Hello(ctx context.Context, req contracts.HelloRequest) (contracts.HelloResponse, error) {
	h, err := r.API.Hello(ctx, req)
	if err == nil && !r.NoHubFit {
		h.Features = append(append([]string(nil), h.Features...), contracts.TerminalFit)
	}
	return h, err
}

func (r *Relayed) ListNodes(ctx context.Context, req contracts.ListNodesRequest) (contracts.Page[contracts.Node], error) {
	p, err := r.API.ListNodes(ctx, req)
	if err == nil && !r.NoNodeFit {
		items := make([]contracts.Node, len(p.Items))
		for i, n := range p.Items {
			n.Capabilities = append(append([]string(nil), n.Capabilities...), contracts.TerminalFit)
			items[i] = n
		}
		p.Items = items
	}
	return p, err
}

// RelayTerm is a fake relayed stream: a FakeTerm drawing the synthetic
// screen. A fitted one is sized to a synthetic window (FitCols x FitRows)
// that SetWindow changes, like a node following a resized window.
type RelayTerm struct {
	*FakeTerm
	Fit bool

	mu      sync.Mutex
	wcols   int
	wrows   int
	changed bool
}

// Synthetic window size of fitted relay streams.
const (
	FitCols = 132
	FitRows = 41
)

// FakeRelayDialer marks Options.DialRelay for the harness to replace with
// its recording fake (Harness.Relays).
func FakeRelayDialer(ctx context.Context, d contracts.AttachDescriptor, cols, rows int, fit bool) (tui.TermConn, error) {
	return nil, contracts.Errorf(contracts.ErrInternal, "tuitest: FakeRelayDialer is replaced by the harness")
}

func (h *Harness) dialRelay(ctx context.Context, d contracts.AttachDescriptor, cols, rows int, fit bool) (tui.TermConn, error) {
	if d.Mechanism != contracts.AttachWebTerminal || d.Ticket == "" {
		return nil, contracts.Errorf(contracts.ErrInvalid, "tuitest: not a web terminal descriptor")
	}
	h.mu.Lock()
	fail := h.RelayErr
	h.mu.Unlock()
	if fail != nil {
		return nil, fail
	}
	if fit {
		cols, rows = FitCols, FitRows
	}
	r := &RelayTerm{FakeTerm: NewFakeTerm(d, cols, rows, nil), Fit: fit, wcols: cols, wrows: rows}
	h.mu.Lock()
	h.relays = append(h.relays, r)
	h.mu.Unlock()
	return r, nil
}

// Relays returns the fake relayed streams opened so far, oldest first.
func (h *Harness) Relays() []*RelayTerm {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]*RelayTerm(nil), h.relays...)
}

// SetRelayErr makes later relay dials fail with err (nil: succeed).
func (h *Harness) SetRelayErr(err error) {
	h.mu.Lock()
	h.RelayErr = err
	h.mu.Unlock()
}

// Resize is ignored by a fitted stream (the node keeps the window's size).
func (r *RelayTerm) Resize(cols, rows int) error {
	if r.Fit {
		return nil
	}
	return r.FakeTerm.Resize(cols, rows)
}

func (r *RelayTerm) WindowSize() (int, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.wcols, r.wrows
}

func (r *RelayTerm) TakeWindowSize() (int, int, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ok := r.changed
	r.changed = false
	return r.wcols, r.wrows, ok
}

// SetWindow simulates the node following a resized window.
func (r *RelayTerm) SetWindow(cols, rows int) {
	r.mu.Lock()
	r.wcols, r.wrows, r.changed = cols, rows, true
	r.mu.Unlock()
	_ = r.FakeTerm.Resize(cols, rows) // redraws: the next read applies the size first
}
