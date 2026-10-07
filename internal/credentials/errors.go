package credentials

import (
	"errors"
	"fmt"

	"github.com/jhh3/pyre/internal/contracts"
)

// ErrorKind classifies store failures.
type ErrorKind string

const (
	KindNotFound    ErrorKind = "not_found"
	KindLocked      ErrorKind = "locked"      // store exists but is locked / needs user unlock
	KindUnavailable ErrorKind = "unavailable" // no usable store (tool missing, no Secret Service, bad perms)
	KindInvalid     ErrorKind = "invalid"
	KindRefused     ErrorKind = "refused" // policy refusal (e.g. file store not opted in)
)

// StoreError is the only error type stores return. Its text names the backend,
// operation and reference — never a value — and carries an actionable hint.
type StoreError struct {
	Kind    ErrorKind
	Backend string
	Op      string // "get" | "put" | "delete" | "open"
	Ref     string // Ref.String(); safe metadata
	Hint    string
	Detail  string // tool diagnostics, already redacted and bounded
}

func (e *StoreError) Error() string {
	msg := fmt.Sprintf("credentials: %s %s", e.Backend, e.Op)
	if e.Ref != "" {
		msg += " " + e.Ref
	}
	msg += ": " + kindText(e.Kind)
	if e.Detail != "" {
		msg += " (" + e.Detail + ")"
	}
	if e.Hint != "" {
		msg += "; " + e.Hint
	}
	return msg
}

func kindText(k ErrorKind) string {
	switch k {
	case KindNotFound:
		return "no credential stored at this reference"
	case KindLocked:
		return "credential store is locked"
	case KindUnavailable:
		return "credential store is unavailable"
	case KindInvalid:
		return "invalid request"
	case KindRefused:
		return "refused by policy"
	}
	return string(k)
}

// Contract maps the error to the wire error shape.
func (e *StoreError) Contract() *contracts.Error {
	var ce *contracts.Error
	switch e.Kind {
	case KindNotFound:
		ce = contracts.Errorf(contracts.ErrNotFound, "%s", e.Error())
	case KindLocked:
		ce = contracts.Errorf(contracts.ErrUnavailable, "%s", e.Error()).WithDetail("availability", "locked_store")
	case KindUnavailable:
		ce = contracts.Errorf(contracts.ErrUnavailable, "%s", e.Error()).WithDetail("availability", "unknown")
	case KindRefused:
		ce = contracts.Errorf(contracts.ErrForbidden, "%s", e.Error())
	default:
		ce = contracts.Errorf(contracts.ErrInvalid, "%s", e.Error())
	}
	return ce.WithDetail("backend", e.Backend)
}

func storeErr(kind ErrorKind, backend, op string, ref Ref, hint, detail string) *StoreError {
	e := &StoreError{Kind: kind, Backend: backend, Op: op, Hint: hint, Detail: detail}
	if ref != (Ref{}) {
		e.Ref = ref.String()
	}
	return e
}

func kindOf(err error) ErrorKind {
	var se *StoreError
	if errors.As(err, &se) {
		return se.Kind
	}
	return ""
}

// IsNotFound / IsLocked / IsUnavailable classify any error.
func IsNotFound(err error) bool    { return kindOf(err) == KindNotFound }
func IsLocked(err error) bool      { return kindOf(err) == KindLocked }
func IsUnavailable(err error) bool { return kindOf(err) == KindUnavailable }

// AvailabilityState maps a store probe result to a ConnectionAvailability
// state ("verified" | "missing" | "locked_store" | "unknown").
func AvailabilityState(err error) string {
	switch {
	case err == nil:
		return "verified"
	case IsNotFound(err):
		return "missing"
	case IsLocked(err):
		return "locked_store"
	}
	return "unknown"
}
