package web

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// cached holds one hub list between page loads. Hub events mark it stale
// (invalidate); the next reader refetches it once, and concurrent readers
// share that fetch. maxAge bounds staleness when the event stream is down.
type cached[T any] struct {
	mu     sync.Mutex
	val    T
	have   bool
	gen    uint64 // bumped by invalidate
	loaded uint64 // gen the value was fetched at
	at     time.Time
	maxAge time.Duration
	// liveMaxAge replaces maxAge while live reports the hub event stream
	// delivering (events then keep the value exact; the age is only a
	// safety net). Zero: maxAge always.
	liveMaxAge time.Duration
	live       *atomic.Bool
	loading    chan struct{}
	err        error
}

// ageLimit is the staleness bound in force (caller holds c.mu).
func (c *cached[T]) ageLimit() time.Duration {
	if c.liveMaxAge > 0 && c.live != nil && c.live.Load() {
		return c.liveMaxAge
	}
	return c.maxAge
}

// freshLocked reports whether the value may be served as is.
func (c *cached[T]) freshLocked(now time.Time) bool {
	age := c.ageLimit()
	return c.have && c.loaded == c.gen && (age <= 0 || now.Sub(c.at) < age)
}

// peekFresh returns the value and its generation when it may be served as
// is (not invalidated, not too old).
func (c *cached[T]) peekFresh(now time.Time) (T, uint64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.freshLocked(now) {
		var zero T
		return zero, 0, false
	}
	return c.val, c.gen, true
}

// patch replaces a fresh value read at generation gen with v, keeping its
// load time (the safety-net age still counts from the last full fetch).
// It reports false, changing nothing, when the value was invalidated or
// refetched since.
func (c *cached[T]) patch(gen uint64, v T) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.have || c.gen != gen || c.loaded != gen || c.loading != nil {
		return false
	}
	c.val = v
	return true
}

func (c *cached[T]) invalidate() {
	c.mu.Lock()
	c.gen++
	c.mu.Unlock()
}

// set stores a value known to be current (e.g. an event payload).
func (c *cached[T]) set(v T, now time.Time) {
	c.mu.Lock()
	c.val, c.have, c.loaded, c.at = v, true, c.gen, now
	c.mu.Unlock()
}

// peek returns the last value without fetching.
func (c *cached[T]) peek() (T, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.val, c.have
}

// get returns the cached value, fetching it when missing, invalidated or
// older than maxAge.
func (c *cached[T]) get(ctx context.Context, now time.Time, fetch func(context.Context) (T, error)) (T, error) {
	c.mu.Lock()
	if c.freshLocked(now) {
		v := c.val
		c.mu.Unlock()
		return v, nil
	}
	if ch := c.loading; ch != nil {
		c.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			var zero T
			return zero, ctx.Err()
		}
		c.mu.Lock()
		// The fetch that just finished answers this reader too, even
		// when an invalidation raced it (the next reader refetches).
		if c.err == nil && c.have {
			v := c.val
			c.mu.Unlock()
			return v, nil
		}
		err := c.err
		c.mu.Unlock()
		var zero T
		return zero, err
	}
	ch := make(chan struct{})
	c.loading = ch
	gen := c.gen
	c.mu.Unlock()

	v, err := fetch(ctx)

	c.mu.Lock()
	c.loading = nil
	c.err = err
	if err == nil {
		c.val, c.have, c.loaded, c.at = v, true, gen, now
	}
	close(ch)
	c.mu.Unlock()
	if err != nil {
		var zero T
		return zero, err
	}
	return v, nil
}
