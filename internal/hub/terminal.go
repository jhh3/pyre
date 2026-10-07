package hub

import (
	"context"
	"database/sql"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/persistence"
	"github.com/jhh3/pyre/internal/terminal"
)

// Web terminal support (WP19). The relay itself lives in internal/terminal;
// the hub issues its tickets from AttachSession and implements
// terminal.Backend below.

// TerminalRelay issues one-time web-terminal tickets (terminal.Relay).
type TerminalRelay interface {
	IssueTerminalTicket(ctx context.Context, p contracts.Principal, runID string, readOnly bool) (string, time.Time, error)
}

var _ terminal.Backend = (*Hub)(nil)

// prefersWebTerminal reports whether web_terminal is the client's first
// preference among terminal mechanisms.
func prefersWebTerminal(pref []contracts.AttachMechanism) bool {
	for _, m := range pref {
		switch m {
		case contracts.AttachWebTerminal:
			return true
		case contracts.AttachLocalTmux, contracts.AttachSSHTmux, contracts.AttachReadOnly:
			return false
		}
	}
	return false
}

// inputLeaseConflict returns ErrConflict when a principal other than p
// holds the run's active input lease.
func (h *Hub) inputLeaseConflict(ctx context.Context, q queryer, p contracts.Principal, runID string) error {
	l, err := h.activeLease(ctx, q, runID)
	if err != nil {
		return err
	}
	if l != nil && persistence.PrincipalKey(l.Holder) != persistence.PrincipalKey(p) {
		return contracts.Errorf(contracts.ErrConflict, "run input is leased by %s until %s; attach read_only or take over the lease", displayName(l.Holder), l.ExpiresAt.UTC().Format("15:04Z")).
			WithRef(contracts.KindLease, l.LeaseID).WithDetail("lease", *l)
	}
	return nil
}

// webDescriptor turns a node's local tmux descriptor into a web-terminal
// descriptor with a relay ticket. An interactive web terminal requires that
// no other principal holds the input lease.
func (h *Hub) webDescriptor(ctx context.Context, p contracts.Principal, d contracts.AttachDescriptor, readOnly bool) (contracts.AttachDescriptor, error) {
	if !readOnly {
		if err := h.inputLeaseConflict(ctx, h.db, p, d.RunID); err != nil {
			return contracts.AttachDescriptor{}, err
		}
	}
	tk, exp, err := h.cfg.Terminal.IssueTerminalTicket(ctx, p, d.RunID, readOnly)
	if err != nil {
		return contracts.AttachDescriptor{}, err
	}
	d.Mechanism, d.Command, d.URL, d.Ticket, d.ExpiresAt, d.ReadOnly = contracts.AttachWebTerminal, nil, contracts.PathTerminal, tk, exp, readOnly
	d.Notes = "web terminal: the ticket works once within a minute; closing the terminal detaches and the run keeps going"
	return d, nil
}

// PrepareTerminal implements terminal.Backend.
func (h *Hub) PrepareTerminal(ctx context.Context, p contracts.Principal, runID string, readOnly bool) (string, error) {
	run, err := h.getRun(ctx, h.db, runID)
	if err != nil {
		return "", err
	}
	if run.State.Terminal() {
		return "", contracts.Errorf(contracts.ErrConflict, "run has %s", run.State).WithRef(contracts.KindRun, runID)
	}
	if err := h.requireConnected(ctx, run.NodeID); err != nil {
		return "", err
	}
	if !readOnly {
		if err := h.inputLeaseConflict(ctx, h.db, p, runID); err != nil {
			return "", err
		}
	}
	return run.NodeID, nil
}

// QueueTerminalOpen implements terminal.Backend: terminal_open goes through
// the durable per-node command queue with a short expiry.
func (h *Hub) QueueTerminalOpen(ctx context.Context, p contracts.Principal, nodeID string, cmd contracts.TerminalOpenCommand, ttl time.Duration) (string, error) {
	var id string
	err := h.tx(ctx, func(tx *sql.Tx) error {
		run, err := h.getRun(ctx, tx, cmd.RunID)
		if err != nil {
			return err
		}
		if run.NodeID != nodeID {
			return contracts.Errorf(contracts.ErrConflict, "run %s moved to node %s", run.RunID, run.NodeID).WithRef(contracts.KindRun, run.RunID)
		}
		rec, err := h.enqueueTx(ctx, tx, nodeID, terminal.CmdTerminalOpen, p, cmd, ttl, contracts.ObjectRef{Kind: contracts.KindRun, ID: run.RunID})
		if err != nil {
			return err
		}
		id = rec.CommandID
		return nil
	})
	return id, err
}

// TerminalOpenResult implements terminal.Backend.
func (h *Hub) TerminalOpenResult(ctx context.Context, commandID string, wait time.Duration) (bool, error) {
	rec, err := h.waitCommand(ctx, commandID, wait)
	if err != nil {
		return false, err
	}
	if !rec.finished() {
		return false, nil
	}
	if rec.Outcome != contracts.OutcomeSucceeded {
		return true, commandError(rec)
	}
	return true, nil
}

// TerminalInputAllowed implements terminal.Backend.
// Owner input reaching the run is the owner engaging with the session.
func (h *Hub) TerminalInputAllowed(ctx context.Context, p contracts.Principal, runID string) error {
	if err := h.inputLeaseConflict(ctx, h.db, p, runID); err != nil {
		return err
	}
	if p.Kind == contracts.PrincipalOwner {
		h.ownerEngaged(p, h.runSession(ctx, runID), "You typed into the session.", engageKinds...)
	}
	return nil
}
