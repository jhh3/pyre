package client

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/jhh3/pyre/internal/contracts"
)

// FreshnessHeader marks a response that was not read from the hub: a node
// daemon answering from its local read cache while the hub is unreachable
// sets it to a JSON contracts.Freshness (source "node-cache:<node>", stale,
// observed_at = when the copy was last confirmed with the hub). Responses
// read live from the hub never carry it. It is a binding-level annotation:
// the response body is the same object the hub returned when it was cached.
const FreshnessHeader = "Pyre-Freshness"

// FreshnessSink receives the FreshnessHeader of a response (op is the RPC
// operation, e.g. "documents.get").
type FreshnessSink func(op string, f contracts.Freshness)

type freshnessSinkKey struct{}

// WithFreshnessSink returns ctx whose calls report FreshnessHeader values to
// sink. Calls without a sink ignore the header.
func WithFreshnessSink(ctx context.Context, sink FreshnessSink) context.Context {
	return context.WithValue(ctx, freshnessSinkKey{}, sink)
}

func reportFreshness(ctx context.Context, op string, h http.Header) {
	v := h.Get(FreshnessHeader)
	if v == "" {
		return
	}
	sink, _ := ctx.Value(freshnessSinkKey{}).(FreshnessSink)
	if sink == nil {
		return
	}
	var f contracts.Freshness
	if json.Unmarshal([]byte(v), &f) != nil {
		return
	}
	sink(op, f)
}

// NoRetryHeader marks an error response as definitive for this call: the
// server has already applied its own retry policy, so the client returns
// the error at once instead of retrying with backoff. A node daemon sets it
// on "unavailable" answers while its hub link is down (it already made its
// single attempt, or answered from local state). The error body is
// unchanged, including its retryable flag (retrying later may succeed).
// Transport failures reaching the daemon itself are still retried.
const NoRetryHeader = "Pyre-No-Retry"

// noRetryError carries an error response that had NoRetryHeader.
type noRetryError struct{ err error }

func (e noRetryError) Error() string { return e.err.Error() }
func (e noRetryError) Unwrap() error { return e.err }

type maxAttemptsKey struct{}

// WithMaxAttempts returns ctx whose calls make at most n attempts (lowering,
// never raising, Options.MaxAttempts). A node daemon uses 1 while its hub
// link is down so local fallbacks answer at once instead of after the retry
// backoff.
func WithMaxAttempts(ctx context.Context, n int) context.Context {
	return context.WithValue(ctx, maxAttemptsKey{}, n)
}
