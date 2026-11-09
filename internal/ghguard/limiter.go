package ghguard

import (
	"sort"
	"sync"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// Limiter is a token bucket per session plus a one-hour call log. A call
// takes a token; with none left it may borrow up to MaxWait of future
// tokens (the caller sleeps that long), and beyond that it is refused. So
// a tight loop is slowed to PerMinute and parallel bursts are refused.
type Limiter struct {
	PerMinute float64
	Burst     float64
	MaxWait   time.Duration

	mu sync.Mutex
	s  map[string]*sessionUse
}

type sessionUse struct {
	tokens    float64
	at        time.Time
	runID     string
	calls     []time.Time // allowed calls in the last hour
	throttled []time.Time
	refused   []time.Time
	warnedAt  time.Time
}

// NewLimiter returns a limiter with the default settings.
func NewLimiter() *Limiter {
	return &Limiter{PerMinute: DefaultPerMinute, Burst: DefaultBurst, MaxWait: DefaultMaxWait}
}

// Decision is the outcome of Take.
type Decision struct {
	Wait       time.Duration // sleep this long, then call
	Refused    bool
	LastMinute int // the session's calls in the last minute, this one included
}

func (l *Limiter) use(sid string, now time.Time) *sessionUse {
	if l.s == nil {
		l.s = map[string]*sessionUse{}
	}
	u := l.s[sid]
	if u == nil {
		u = &sessionUse{tokens: l.Burst, at: now}
		l.s[sid] = u
	}
	rate := l.PerMinute / 60
	if el := now.Sub(u.at).Seconds(); el > 0 {
		u.tokens = min(l.Burst, u.tokens+el*rate)
		u.at = now
	}
	return u
}

// Take spends one call from session sid's budget.
func (l *Limiter) Take(sid, runID string, now time.Time) Decision {
	l.mu.Lock()
	defer l.mu.Unlock()
	u := l.use(sid, now)
	if runID != "" {
		u.runID = runID
	}
	rate := l.PerMinute / 60
	d := Decision{}
	if u.tokens-1 < -rate*l.MaxWait.Seconds() {
		u.refused = append(u.refused, now)
		d.Refused = true
	} else {
		u.tokens--
		if u.tokens < 0 {
			d.Wait = time.Duration(-u.tokens / rate * float64(time.Second))
			u.throttled = append(u.throttled, now)
		}
		u.calls = append(u.calls, now)
	}
	for _, t := range u.calls {
		if now.Sub(t) < time.Minute {
			d.LastMinute++
		}
	}
	if d.Refused {
		d.LastMinute++
	}
	l.prune(now)
	return d
}

// Warn reports whether session sid should be shown a low-budget warning
// now (at most once per every interval), and records it.
func (l *Limiter) Warn(sid string, now time.Time, every time.Duration) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	u := l.use(sid, now)
	if !u.warnedAt.IsZero() && now.Sub(u.warnedAt) < every {
		return false
	}
	u.warnedAt = now
	return true
}

func trim(ts []time.Time, now time.Time) []time.Time {
	i := 0
	for i < len(ts) && now.Sub(ts[i]) >= time.Hour {
		i++
	}
	return ts[i:]
}

// prune drops calls older than an hour and sessions with nothing left to
// remember (no recent calls and a full bucket).
func (l *Limiter) prune(now time.Time) {
	for sid, u := range l.s {
		u.calls, u.throttled, u.refused = trim(u.calls, now), trim(u.throttled, now), trim(u.refused, now)
		if len(u.calls)+len(u.refused) == 0 && u.tokens >= l.Burst && now.Sub(u.warnedAt) >= time.Hour {
			delete(l.s, sid)
		}
	}
}

// Top returns the n sessions with the most calls in the last hour (most
// first) and the total calls in that hour.
func (l *Limiter) Top(now time.Time, n int) ([]contracts.GitHubCaller, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.prune(now)
	var out []contracts.GitHubCaller
	total := 0
	for sid, u := range l.s {
		total += len(u.calls)
		if len(u.calls)+len(u.refused) == 0 {
			continue
		}
		out = append(out, contracts.GitHubCaller{SessionID: sid, RunID: u.runID, Calls: len(u.calls), Throttled: len(u.throttled), Refused: len(u.refused)})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Calls != out[j].Calls {
			return out[i].Calls > out[j].Calls
		}
		return out[i].SessionID < out[j].SessionID
	})
	if len(out) > n {
		out = out[:n]
	}
	return out, total
}
