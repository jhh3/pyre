package contracts

import (
	"context"
	"time"
)

// Waiting on own job (W-childnotify, additive, optional): a session that
// ends its turn while a CI run, review or watcher it started is still
// pending says so first. Its turn end is then reported to its parent as
// "waiting" (not idle: do not give it new work or treat it as done), a
// top-level session's turn end does not count as waiting on the owner,
// and if the session has not started a new turn by Until its parent is
// told the job looks stalled.
//
// Quiet (additive, optional): the turn's end is not reported to the parent
// at all (a lead waiting on its own workers, CI or a review, woken later by
// a watcher, a child's report or a message). The waiting_overdue report at
// Until still goes out, with the last message, and a run that exits is
// reported as usual.
//
// The marker belongs to the caller's current turn: a later turn that ends
// without a new marker reports normally.
const OpSessionsWaitingOn = "sessions.waiting_on"

// WaitingOnDefault and WaitingOnMax bound ExpectMinutes.
const (
	WaitingOnDefault = 60 * time.Minute
	WaitingOnMax     = 24 * time.Hour
	WaitingOnMaxWhat = 300
)

type SetWaitingOnRequest struct {
	Mutation
	// What the session waits on ("CI on PR #12 https://…").
	What string `json:"what"`
	// ExpectMinutes: when it should be done (default 60).
	ExpectMinutes int `json:"expect_minutes,omitempty"`
	// Clear drops the marker (the job finished within the turn).
	Clear bool `json:"clear,omitempty"`
	// Quiet: this turn's turn-end report is not sent to the parent.
	Quiet bool `json:"quiet,omitempty"`
}

type SetWaitingOnResponse struct {
	SessionID string     `json:"session_id"`
	What      string     `json:"what,omitempty"`
	Until     *time.Time `json:"until,omitempty"`
	Quiet     bool       `json:"quiet,omitempty"`
	Detail    string     `json:"detail,omitempty"`
}

// WaitingOnAPI is implemented by hubs (and the client and the node-local
// proxy) that offer sessions.waiting_on. Only a session principal calls it.
type WaitingOnAPI interface {
	SetWaitingOn(ctx context.Context, req SetWaitingOnRequest) (SetWaitingOnResponse, error)
}
