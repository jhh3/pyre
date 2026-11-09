package ghwait

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/jhh3/pyre/internal/osutil"
)

// Poll intervals: fast while checks or a run are going (30 s for the
// first 10 minutes, then 60 s), slower for an idle open PR, slowest once
// it is merged, closed or completed. They double when the account has
// less than a fifth of its hourly limit left.
const (
	IntervalActive     = 30 * time.Second
	IntervalActiveLate = 60 * time.Second
	IntervalIdle       = 2 * time.Minute
	IntervalTerminal   = 5 * time.Minute
	IntervalError      = time.Minute
	activeFastFor      = 10 * time.Minute
)

// Poller is the node's shared poller: one per machine (a directory), used
// by every wait on it. For each target, the first waiter that finds the
// snapshot due fetches it while holding the target's lock file; waiters
// that come while it fetches block on the lock and then read what it
// wrote. So N waits on one PR cost one fetch per interval, and a waiter
// that dies hands the job to the next one (no daemon to keep alive).
type Poller struct {
	Dir    string // <state dir>/ghwait
	Client *Client
	Now    func() time.Time
	// Fetch overrides the GitHub fetch (tests); nil uses FetchPR/FetchRun.
	Fetch func(ctx context.Context, t Target) (Snapshot, error)
}

// PollError is a failed fetch, shared with every waiter until the next try.
type PollError struct {
	Msg       string
	Permanent bool // needs a person (auth, access, not found)
}

func (e *PollError) Error() string { return e.Msg }

// snapFile is a target's shared state on disk.
type snapFile struct {
	Snap         *Snapshot `json:"snap,omitempty"`
	Next         time.Time `json:"next"` // when the next fetch is due
	ActiveSince  time.Time `json:"active_since,omitempty"`
	Err          string    `json:"err,omitempty"`
	ErrPermanent bool      `json:"err_permanent,omitempty"`
	Fetches      int       `json:"fetches"`
}

// PollInfo says how a Poll was served.
type PollInfo struct {
	Fetched bool      // this call fetched from GitHub (else it read the shared snapshot)
	Next    time.Time // when the shared snapshot is due again
	Fetches int       // fetches of this target on this machine so far
}

func (p *Poller) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *Poller) snapPath(t Target) string { return filepath.Join(p.Dir, "snaps", t.Key()+".json") }

// CacheDir is where the client keeps ETag'd answers.
func CacheDir(dir string) string { return filepath.Join(dir, "cache") }

// Poll returns t's current snapshot: the shared one while it is fresh,
// else a new fetch (under the target's lock).
func (p *Poller) Poll(ctx context.Context, t Target) (Snapshot, PollInfo, error) {
	return p.PollFresh(ctx, t, time.Time{})
}

// PollFresh is Poll, but fetches anew unless the shared snapshot was
// fetched at or after notBefore (a settle confirmation needs a second,
// later look at GitHub, not the same snapshot again).
func (p *Poller) PollFresh(ctx context.Context, t Target, notBefore time.Time) (Snapshot, PollInfo, error) {
	lockDir := filepath.Join(p.Dir, "locks")
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		return Snapshot{}, PollInfo{}, err
	}
	lf, err := os.OpenFile(filepath.Join(lockDir, t.Key()+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return Snapshot{}, PollInfo{}, err
	}
	defer lf.Close()
	if err := osutil.Lock(lf); err != nil {
		return Snapshot{}, PollInfo{}, err
	}
	defer osutil.Unlock(lf)

	var sf snapFile
	if b, err := os.ReadFile(p.snapPath(t)); err == nil {
		_ = json.Unmarshal(b, &sf)
	}
	now := p.now()
	fresh := notBefore.IsZero() || (sf.Snap != nil && !sf.Snap.Fetched.Before(notBefore))
	// A recent error keeps its backoff even when a fresh look is wanted.
	if now.Before(sf.Next) && (fresh || sf.Err != "") {
		info := PollInfo{Next: sf.Next, Fetches: sf.Fetches}
		if sf.Err != "" {
			return Snapshot{}, info, &PollError{Msg: sf.Err, Permanent: sf.ErrPermanent}
		}
		if sf.Snap != nil {
			return *sf.Snap, info, nil
		}
	}
	snap, ferr := p.fetch(ctx, t)
	if ctx.Err() != nil {
		return Snapshot{}, PollInfo{}, ctx.Err()
	}
	sf.Fetches++
	if ferr != nil {
		sf.Err, sf.ErrPermanent = ferr.Error(), Permanent(ferr)
		sf.Next = now.Add(IntervalError)
		var pe *PollError
		if errors.As(ferr, &pe) {
			sf.ErrPermanent = pe.Permanent
		}
	} else {
		snap.Fetched = now
		sf.Snap, sf.Err, sf.ErrPermanent = &snap, "", false
		sf.Next = now.Add(p.interval(&sf, snap, now))
	}
	if b, err := json.Marshal(sf); err == nil {
		_ = writeAtomic(p.snapPath(t), b)
	}
	info := PollInfo{Fetched: true, Next: sf.Next, Fetches: sf.Fetches}
	if ferr != nil {
		return Snapshot{}, info, &PollError{Msg: sf.Err, Permanent: sf.ErrPermanent}
	}
	return snap, info, nil
}

func (p *Poller) fetch(ctx context.Context, t Target) (Snapshot, error) {
	if p.Fetch != nil {
		return p.Fetch(ctx, t)
	}
	s := Snapshot{Target: t}
	var err error
	if t.Kind == KindRun {
		s.Run, err = FetchRun(ctx, p.Client, t)
	} else {
		s.PR, err = FetchPR(ctx, p.Client, t)
	}
	return s, err
}

// interval picks when the snapshot is due again (adaptive).
func (p *Poller) interval(sf *snapFile, s Snapshot, now time.Time) time.Duration {
	var d time.Duration
	switch {
	case s.Terminal():
		d, sf.ActiveSince = IntervalTerminal, time.Time{}
	case s.Active():
		if sf.ActiveSince.IsZero() {
			sf.ActiveSince = now
		}
		d = IntervalActive
		if now.Sub(sf.ActiveSince) >= activeFastFor {
			d = IntervalActiveLate
		}
	default:
		d, sf.ActiveSince = IntervalIdle, time.Time{}
	}
	if p.Client != nil {
		if left, limit := p.Client.RateLeft(); limit > 0 && left*5 < limit {
			d *= 2
		}
	}
	return d
}

// Terminal reports a target that will not change much any more: a merged
// or closed PR, a completed run.
func (s Snapshot) Terminal() bool {
	switch {
	case s.PR != nil:
		return s.PR.Pull.State == "closed"
	case s.Run != nil:
		return s.Run.Run.Status == "completed"
	}
	return false
}

// Active reports checks or a run in progress.
func (s Snapshot) Active() bool {
	switch {
	case s.PR != nil:
		c := Checks(s.PR)
		return c.Pending > 0
	case s.Run != nil:
		return s.Run.Run.Status != "completed"
	}
	return false
}
