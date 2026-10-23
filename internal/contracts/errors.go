package contracts

import (
	"errors"
	"fmt"
	"net/http"
)

// ErrorCode is a stable machine-readable error category.
type ErrorCode string

const (
	ErrInvalid              ErrorCode = "invalid_argument"
	ErrUnauthorized         ErrorCode = "unauthorized"
	ErrForbidden            ErrorCode = "forbidden"
	ErrNotFound             ErrorCode = "not_found"
	ErrConflict             ErrorCode = "conflict"
	ErrExpired              ErrorCode = "expired"
	ErrResetRequired        ErrorCode = "reset_required" // cursor older than retention
	ErrUnsupported          ErrorCode = "unsupported"
	ErrCapacityExhausted    ErrorCode = "capacity_exhausted"
	ErrUnavailable          ErrorCode = "unavailable"
	ErrOutcomeIndeterminate ErrorCode = "outcome_indeterminate"
	ErrProtocolMismatch     ErrorCode = "protocol_mismatch"
	ErrPayloadTooLarge      ErrorCode = "payload_too_large"
	ErrInternal             ErrorCode = "internal"
	// ErrApprovalRequested: a session asked for an owner-only operation;
	// the hub filed it as an approval in the owner's inbox (Ref: the ask;
	// Details: ask_id, status "filed" | "merged") and runs it as the owner
	// once approved, then messages the session the outcome. Not a refusal.
	ErrApprovalRequested ErrorCode = "approval_requested"
)

// Error is the single error shape on every Pyre wire boundary.
type Error struct {
	Code      ErrorCode      `json:"code"`
	Message   string         `json:"message"`
	Retryable bool           `json:"retryable"`
	Ref       *ObjectRef     `json:"ref,omitempty"`
	Details   map[string]any `json:"details,omitempty"`
}

func (e *Error) Error() string {
	if e.Ref != nil {
		return fmt.Sprintf("%s: %s (%s %s)", e.Code, e.Message, e.Ref.Kind, e.Ref.ID)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Errorf builds an *Error with retryability derived from the code.
func Errorf(code ErrorCode, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...), Retryable: code.DefaultRetryable()}
}

// WithRef attaches an object reference and returns the same error.
func (e *Error) WithRef(kind ObjectKind, id string) *Error {
	e.Ref = &ObjectRef{Kind: kind, ID: id}
	return e
}

// WithDetail attaches a detail value and returns the same error.
func (e *Error) WithDetail(key string, v any) *Error {
	if e.Details == nil {
		e.Details = map[string]any{}
	}
	e.Details[key] = v
	return e
}

// DefaultRetryable reports whether callers may retry the same operation.
func (c ErrorCode) DefaultRetryable() bool {
	switch c {
	case ErrUnavailable, ErrCapacityExhausted:
		return true
	}
	return false
}

// HTTPStatus maps an error code to the HTTP status used by the hub API.
func (c ErrorCode) HTTPStatus() int {
	switch c {
	case ErrInvalid:
		return http.StatusBadRequest
	case ErrUnauthorized:
		return http.StatusUnauthorized
	case ErrForbidden, ErrApprovalRequested: // not done now; the body says it was filed
		return http.StatusForbidden
	case ErrNotFound:
		return http.StatusNotFound
	case ErrConflict:
		return http.StatusConflict
	case ErrExpired, ErrResetRequired:
		return http.StatusGone
	case ErrPayloadTooLarge:
		return http.StatusRequestEntityTooLarge
	case ErrProtocolMismatch:
		return http.StatusUpgradeRequired
	case ErrCapacityExhausted:
		return http.StatusTooManyRequests
	case ErrUnsupported:
		return http.StatusNotImplemented
	case ErrUnavailable:
		return http.StatusServiceUnavailable
	case ErrOutcomeIndeterminate:
		return http.StatusGatewayTimeout
	}
	return http.StatusInternalServerError
}

// CodeOf extracts the ErrorCode from any error (ErrInternal if not an *Error).
func CodeOf(err error) ErrorCode {
	if err == nil {
		return ""
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ErrInternal
}

// IsCode reports whether err carries the given code.
func IsCode(err error, code ErrorCode) bool { return err != nil && CodeOf(err) == code }

// AsError converts any error to *Error (wrapping unknown errors as internal).
func AsError(err error) *Error {
	if err == nil {
		return nil
	}
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return &Error{Code: ErrInternal, Message: err.Error()}
}

// ErrorBody is the JSON envelope for error responses: {"error": {...}}.
type ErrorBody struct {
	Error *Error `json:"error"`
}
