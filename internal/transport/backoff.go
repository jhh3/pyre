package transport

import (
	"math"
	"math/rand/v2"
	"time"
)

// BackoffConfig bounds reconnect retries. Zero fields take defaults.
type BackoffConfig struct {
	Initial time.Duration // default 500ms
	Max     time.Duration // cap for transient failures; default 10s
	// RejectMax is the delay after a rejection that retrying soon will not
	// fix (revoked credential, protocol mismatch); default 60s, or Max when
	// Max is set explicitly.
	RejectMax  time.Duration
	Multiplier float64 // default 2
	// Jitter is the fraction of each delay that is randomised (0..1,
	// default 0.5): a delay d becomes uniform in [d*(1-Jitter), d].
	Jitter float64
}

func (c BackoffConfig) withDefaults() BackoffConfig {
	if c.Initial <= 0 {
		c.Initial = 500 * time.Millisecond
	}
	maxSet := c.Max > 0
	if !maxSet {
		c.Max = 10 * time.Second
	}
	if c.Max < c.Initial {
		c.Max = c.Initial
	}
	if c.RejectMax <= 0 {
		c.RejectMax = c.Max // an explicit Max caps rejections too
		if !maxSet {
			c.RejectMax = 60 * time.Second
		}
	}
	if c.Multiplier < 1 {
		c.Multiplier = 2
	}
	if c.Jitter <= 0 || c.Jitter > 1 {
		c.Jitter = 0.5
	}
	return c
}

// Backoff produces exponentially growing, jittered, capped delays. It is not
// safe for concurrent use.
type Backoff struct {
	cfg     BackoffConfig
	attempt int
}

// NewBackoff returns a Backoff using cfg (with defaults applied).
func NewBackoff(cfg BackoffConfig) *Backoff { return &Backoff{cfg: cfg.withDefaults()} }

// Next returns the delay before the next attempt and advances the attempt
// counter. The result never exceeds Max.
func (b *Backoff) Next() time.Duration {
	d := b.ceiling(b.attempt)
	if b.attempt < 64 {
		b.attempt++
	}
	return b.jitter(d)
}

// MaxDelay returns a jittered delay at the cap, used after a rejection that
// retrying soon will not fix (revoked credential, protocol mismatch).
func (b *Backoff) MaxDelay() time.Duration { return b.jitter(b.cfg.RejectMax) }

// Reset returns to the initial delay (after a stable connection).
func (b *Backoff) Reset() { b.attempt = 0 }

// Attempt reports how many delays have been issued since the last Reset.
func (b *Backoff) Attempt() int { return b.attempt }

func (b *Backoff) ceiling(attempt int) time.Duration {
	f := float64(b.cfg.Initial) * math.Pow(b.cfg.Multiplier, float64(attempt))
	if f > float64(b.cfg.Max) || math.IsInf(f, 0) || math.IsNaN(f) {
		return b.cfg.Max
	}
	return time.Duration(f)
}

func (b *Backoff) jitter(d time.Duration) time.Duration {
	lo := float64(d) * (1 - b.cfg.Jitter)
	return time.Duration(lo + rand.Float64()*(float64(d)-lo))
}
