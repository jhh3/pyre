package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
)

// A request the server stopped serving (its context ended: the hub is
// shutting down, or the client left) is answered as a retryable 503, not
// a generic 500 "internal error"; a definite answer keeps its code, and a
// real internal error on a live request stays a 500.
func TestWriteOpErrorInterrupted(t *testing.T) {
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	for _, tc := range []struct {
		name   string
		ctx    context.Context
		err    error
		status int
		code   contracts.ErrorCode
	}{
		{"store query cancelled by shutdown", ended, fmt.Errorf("update upload: %w", context.Canceled), http.StatusServiceUnavailable, contracts.ErrUnavailable},
		{"plain error after the context ended", ended, errors.New("sql: database is closed"), http.StatusServiceUnavailable, contracts.ErrUnavailable},
		{"canceled on a live request", context.Background(), context.Canceled, http.StatusServiceUnavailable, contracts.ErrUnavailable},
		{"definite answer stays", ended, contracts.Errorf(contracts.ErrNotFound, "no such upload"), http.StatusNotFound, contracts.ErrNotFound},
		{"too large stays", ended, contracts.Errorf(contracts.ErrPayloadTooLarge, "too big"), http.StatusRequestEntityTooLarge, contracts.ErrPayloadTooLarge},
		{"internal on a live request stays", context.Background(), errors.New("disk on fire"), http.StatusInternalServerError, contracts.ErrInternal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequestWithContext(tc.ctx, http.MethodPut, "/v1/uploads/upl_synth?offset=0", nil)
			w := httptest.NewRecorder()
			writeOpError(w, r, tc.err)
			var eb contracts.ErrorBody
			if err := json.Unmarshal(w.Body.Bytes(), &eb); err != nil || eb.Error == nil {
				t.Fatalf("body %q: %v", w.Body.String(), err)
			}
			if w.Code != tc.status || eb.Error.Code != tc.code {
				t.Fatalf("got %d %s, want %d %s", w.Code, eb.Error.Code, tc.status, tc.code)
			}
			if tc.code == contracts.ErrUnavailable && (!eb.Error.Retryable || w.Header().Get("Retry-After") == "") {
				t.Fatalf("interrupted answer not retryable: %s %v", w.Body.String(), w.Header())
			}
		})
	}
}
