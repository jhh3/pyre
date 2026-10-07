package cli

import (
	"errors"
	"fmt"

	"github.com/jhh3/pyre/internal/contracts"
)

// Exit codes. They are part of the scripting contract (see `pyre help exit-codes`).
const (
	ExitOK            = 0
	ExitError         = 1 // any other failure (unauthorized, internal, ...)
	ExitUsage         = 2 // bad command line or invalid argument
	ExitConflict      = 3 // conflict / expired / stale (e.g. document CAS, lease held)
	ExitNotFound      = 4
	ExitUnavailable   = 5 // hub/node unreachable, capacity exhausted, or a wait timed out (retry later)
	ExitUnsupported   = 6 // capability or protocol not supported
	ExitIndeterminate = 7 // the outcome is unknown; inspect before retrying
)

const exitCodesHelp = `Exit codes:
  0  ok
  1  error (unauthorized, internal, ...)
  2  usage: bad command line or invalid argument
  3  conflict: stale revision, held lease, expired request, duplicate with different body
  4  not found
  5  unavailable: hub/node unreachable, capacity exhausted, or a wait timed out (retry later)
  6  unsupported capability or protocol mismatch
  7  indeterminate: the outcome is unknown; inspect before retrying (never blindly resend)
`

// ExitCodeFor maps an error to the documented exit code.
func ExitCodeFor(err error) int {
	if err == nil {
		return ExitOK
	}
	var st *statusError
	if errors.As(err, &st) {
		return st.code
	}
	var ue *usageError
	if errors.As(err, &ue) {
		return ExitUsage
	}
	var ce *contracts.Error
	if !errors.As(err, &ce) {
		return ExitError
	}
	return exitForCode(ce.Code)
}

func exitForCode(code contracts.ErrorCode) int {
	switch code {
	case contracts.ErrInvalid:
		return ExitUsage
	case contracts.ErrConflict, contracts.ErrExpired, contracts.ErrResetRequired:
		return ExitConflict
	case contracts.ErrNotFound:
		return ExitNotFound
	case contracts.ErrUnavailable, contracts.ErrCapacityExhausted:
		return ExitUnavailable
	case contracts.ErrUnsupported, contracts.ErrProtocolMismatch:
		return ExitUnsupported
	case contracts.ErrOutcomeIndeterminate:
		return ExitIndeterminate
	}
	return ExitError
}

// usageError is a command-line mistake (exit 2).
type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

func usagef(format string, args ...any) error {
	return &usageError{msg: fmt.Sprintf(format, args...)}
}

// statusError carries an explicit exit code after output was already
// written (e.g. a command receipt whose outcome is indeterminate).
type statusError struct {
	code int
	err  error
}

func (e *statusError) Error() string { return e.err.Error() }
func (e *statusError) Unwrap() error { return e.err }

func withStatus(code int, err error) error { return &statusError{code: code, err: err} }

// outcomeStatus converts a command outcome into an exit status: failures
// and unknown outcomes are not reported as success.
func outcomeStatus(o contracts.CommandOutcome, detail string) error {
	switch o {
	case contracts.OutcomeFailed:
		return withStatus(ExitError, fmt.Errorf("command failed: %s", detail))
	case contracts.OutcomeUnsupported:
		return withStatus(ExitUnsupported, fmt.Errorf("command unsupported: %s", detail))
	case contracts.OutcomeIndeterminate:
		return withStatus(ExitIndeterminate, fmt.Errorf("command outcome indeterminate: %s", detail))
	}
	return nil
}
