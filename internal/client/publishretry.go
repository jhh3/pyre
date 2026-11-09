package client

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
)

// DefaultPublishRetryWindow is how long Publish keeps retrying through an
// outage (a hub restarting for an upgrade takes ~10 s; a node's pyred a
// few seconds) before giving up.
const DefaultPublishRetryWindow = 2 * time.Minute

// publishMaxInternal bounds consecutive "internal error" answers Publish
// retries. A hub older than the interrupted-request answer (httpapi
// writeOpError) reports an upload cut off by its shutdown that way, so a
// few are retried; a persistent internal error is a hub fault, reported
// after these instead of after the whole window.
const publishMaxInternal = 3

// Test hooks.
var (
	publishSleep   = sleepCtx
	publishBackoff = func(n int) time.Duration { return min(time.Second<<uint(min(n, 4)), 10*time.Second) }
)

// publishRetry retries the steps of one Publish through an outage: each
// step is idempotent (BeginUpload and FinalizeUpload by operation id,
// UploadChunk by byte range, GetUpload is a read), so a step whose outcome
// was lost is simply sent again and never yields a second version.
type publishRetry struct {
	ctx      context.Context
	window   time.Duration
	opID     string
	uploadID string
	since    time.Time // start of the current outage; zero while healthy
	tries    int       // retries in the current outage
	internal int       // consecutive internal errors
}

// do runs step until it succeeds, fails definitively, or the outage
// outlasts the window. what names the step for notices and errors.
func (r *publishRetry) do(what string, step func() error) error {
	for {
		err := step()
		if err == nil {
			if !r.since.IsZero() {
				notify(r.ctx, fmt.Sprintf("publish: %s succeeded after %s; continuing", what, time.Since(r.since).Round(time.Second)))
			}
			r.since, r.tries, r.internal = time.Time{}, 0, 0
			return nil
		}
		if r.ctx.Err() != nil {
			return r.ctx.Err()
		}
		var ce *c.Error
		if !errors.As(err, &ce) || r.window <= 0 {
			return err
		}
		// Only an outage is waited out: "unavailable" (transport failures,
		// 502/503/504, an interrupted request) and a few internal errors.
		// Anything else is a definite answer (too large, conflict, invalid,
		// capacity_exhausted: a full quota does not clear by waiting) the
		// client has already retried as far as it should.
		internal := ce.Code == c.ErrInternal
		if !(ce.Code == c.ErrUnavailable && ce.Retryable) && !internal {
			return err
		}
		if r.since.IsZero() {
			r.since = time.Now()
		}
		if internal {
			r.internal++
		} else {
			r.internal = 0
		}
		elapsed := time.Since(r.since)
		if internal && r.internal > publishMaxInternal {
			return c.Errorf(c.ErrInternal, "publish: the hub failed %s %d times in a row with an internal error (the hub log has the cause)%s",
				what, r.internal, r.resumeHint()).WithRef(c.KindUpload, r.uploadID)
		}
		if elapsed >= r.window {
			return r.giveUp(what, ce, elapsed)
		}
		wait := min(publishBackoff(r.tries), r.window-elapsed)
		r.tries++
		notify(r.ctx, fmt.Sprintf("publish: %s failed (%s: %s); %s restarting? retrying in %s (%s of %s retry window used)",
			what, ce.Code, oneLine(ce.Message), endpoint(ce), wait.Round(100*time.Millisecond), elapsed.Round(time.Second), r.window))
		if err := publishSleep(r.ctx, wait); err != nil {
			return err
		}
	}
}

func (r *publishRetry) giveUp(what string, last *c.Error, elapsed time.Duration) error {
	e := c.Errorf(c.ErrUnavailable, "publish: %s unavailable (restarting?) after %s of retries; last error during %s: %s%s",
		endpoint(last), elapsed.Round(time.Second), what, oneLine(last.Message), r.resumeHint())
	for k, v := range last.Details {
		e.WithDetail(k, v)
	}
	if r.uploadID != "" {
		e.WithRef(c.KindUpload, r.uploadID)
	}
	return e
}

func (r *publishRetry) resumeHint() string {
	if r.uploadID == "" {
		return ""
	}
	return fmt.Sprintf("; the bytes sent so far stay in upload %s: publish again with operation id %s (pyre publish --operation-id %s) to resume", r.uploadID, r.opID, r.opID)
}

// endpoint names what a failed call could not reach.
func endpoint(ce *c.Error) string {
	if ce.Details[DetailUnreachable] == "local_daemon" {
		return "pyred"
	}
	return "hub"
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	return s
}

// mib formats a byte count for progress notices.
func mib(n int64) string { return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20)) }
