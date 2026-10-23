package sentinel

import (
	"fmt"
	"strings"
	"time"
)

// classify turns one observation into this sentinel's vote.
//
// DOWN: every hub endpoint fails; or ssh connects but the user manager is
// not running; or ssh cannot connect and no hub endpoint answers.
// DEGRADED: a unit is not active, a port is closed, a public URL fails, or
// ssh fails while the hub answers. UNKNOWN: ssh was refused (a key or host
// key problem on this sentinel) and there is no hub to check instead.
func classify(o Observation) Vote {
	var down, degraded []string
	hubUp := false
	for _, c := range o.Hub {
		hubUp = hubUp || c.OK
	}
	if len(o.Hub) > 0 && !hubUp {
		down = append(down, "hub unreachable on "+joinAnd(checkLabels(o.Hub)))
	}
	switch o.SSH {
	case SSHOK:
		switch o.Manager {
		case ManagerAbsent:
			down = append(down, "user manager not running")
		case ManagerTimeout:
			degraded = append(degraded, "user manager not answering (systemctl timed out)")
		}
	case SSHHung:
		degraded = append(degraded, "ssh connects but the probe hung ("+o.SSHDetail+")")
	case SSHUnreachable:
		if len(o.Hub) == 0 || !hubUp {
			down = append(down, "ssh fails ("+o.SSHDetail+")")
		} else {
			degraded = append(degraded, "ssh fails ("+o.SSHDetail+")")
		}
	}
	if len(o.UnitsDown) > 0 {
		degraded = append(degraded, "not active: "+strings.Join(o.UnitsDown, ", "))
	}
	if len(o.TCPDown) > 0 {
		degraded = append(degraded, "port closed: "+strings.Join(o.TCPDown, ", "))
	}
	for _, c := range o.Public {
		if !c.OK {
			degraded = append(degraded, fmt.Sprintf("%s down (%s)", c.URL, c.Detail))
		}
	}
	v := Vote{Obs: o}
	switch {
	case len(down) > 0:
		v.Status, v.Reasons = StatusDown, append(down, degraded...)
	case (o.SSH == SSHAuth || o.SSH == SSHError) && len(o.Hub) == 0:
		v.Status, v.Reasons = StatusUnknown, append([]string{"ssh " + o.SSH + " (" + o.SSHDetail + ")"}, degraded...)
	case len(degraded) > 0:
		v.Status, v.Reasons = StatusDegraded, degraded
	default:
		v.Status = StatusUp
	}
	return v
}

// Cond is the evaluated condition of one target from the live sentinels'
// votes.
type Cond struct {
	Level      Level
	DownBy     []string
	DegradedBy []string
	UpBy       []string
	// Missing are sentinels (other than the target) with no fresh vote.
	Missing []string
	// SentinelMissing: the target runs a sentinel whose state is stale.
	SentinelMissing bool
	// Abstain: no live sentinel could tell (every vote unknown). Timers and
	// incidents stay as they were; this is not "OK".
	Abstain bool

	// Partition: CantReach could not get to the target at all while
	// CanReach did (or, with sites, the target's own sentinel reports while
	// only watchers at one other site fail). The level is judged from the
	// watchers that reach it; the failing path is a link incident.
	Partition bool
	CantReach []string
	CanReach  []string
	SelfOK    bool

	// PublicOnly: the only problem the watchers see is public URLs failing
	// (PublicFail). SelfPublicKnown: the target's own sentinel checked them
	// too, and SelfPublicFail are the ones failing there as well.
	PublicOnly      bool
	PublicFail      []string
	SelfPublicKnown bool
	SelfPublicFail  []string
}

// evaluate applies the quorum rule: DOWN needs at least two live sentinels
// (never the target itself) voting down; a lone live sentinel's down vote
// is DOWN (unconfirmed). DEGRADED needs as many degraded-or-down votes as
// min(2, live voters). A target whose own sentinel stopped reporting is
// DEGRADED.
func evaluate(target string, votes map[string]*Vote, missing []string, sentinelMissing bool) Cond {
	c := Cond{Missing: missing, SentinelMissing: sentinelMissing}
	for _, voter := range sortedKeys(votes) {
		v := votes[voter]
		if voter == target || v == nil {
			continue
		}
		switch v.Status {
		case StatusDown:
			c.DownBy = append(c.DownBy, voter)
		case StatusDegraded:
			c.DegradedBy = append(c.DegradedBy, voter)
		case StatusUp:
			c.UpBy = append(c.UpBy, voter)
		}
	}
	n := len(c.DownBy) + len(c.DegradedBy) + len(c.UpBy)
	bad := len(c.DownBy) + len(c.DegradedBy)
	switch {
	case len(c.DownBy) >= 2:
		c.Level = LevelDown
	case n == 1 && len(c.DownBy) == 1:
		c.Level = LevelUnconfirmed
	case n > 0 && bad >= min(2, n):
		c.Level = LevelDegraded
	case sentinelMissing:
		c.Level = LevelDegraded
	case n == 0:
		c.Abstain = true
	}
	return c
}

// voters lists who saw the condition, for messages.
func (c Cond) voters() []string {
	switch c.Level {
	case LevelDown, LevelUnconfirmed:
		return c.DownBy
	case LevelDegraded:
		return append(append([]string{}, c.DownBy...), c.DegradedBy...)
	}
	return nil
}

// unreached: the voter could not get to the target at all (ssh did not
// connect and no hub address answered).
func unreached(v *Vote) bool {
	if v == nil || v.Status == StatusUnknown || v.Obs.SSH != SSHUnreachable {
		return false
	}
	for _, c := range v.Obs.Hub {
		if c.OK {
			return false
		}
	}
	return true
}

// reached: the voter got to the target (ssh connected or the hub answered).
func reached(v *Vote) bool {
	if v == nil || v.Status == StatusUnknown || unreached(v) {
		return false
	}
	if v.Obs.SSH == SSHOK || v.Obs.SSH == SSHHung {
		return true
	}
	for _, c := range v.Obs.Hub {
		if c.OK {
			return true
		}
	}
	return false
}

// partition splits target's voters into those that cannot reach it at all
// and those that can. It is a partition (a link problem, not the host) when
// some cannot and either another watcher reached it, or the target's own
// sentinel is reporting (selfAt: its state's time on our clock) while every
// failing watcher is at one site other than the target's (sites known).
// Evidence must be at least as new as the latest failure started, so at the
// start of a real outage a watcher's last good probe from before it does
// not count: when every watcher fails, it is not a partition.
func partition(cfg *Config, target string, votes map[string]*Vote, selfOK bool, selfAt time.Time) (cant, can []string, ok bool) {
	var failedSince time.Time
	for _, voter := range sortedKeys(votes) {
		v := votes[voter]
		if voter == target || !unreached(v) {
			continue
		}
		cant = append(cant, voter)
		if v.Since.After(failedSince) {
			failedSince = v.Since
		}
	}
	if len(cant) == 0 {
		return nil, nil, false
	}
	for _, voter := range sortedKeys(votes) {
		v := votes[voter]
		if voter != target && reached(v) && !v.Obs.CheckedAt.Before(failedSince) {
			can = append(can, voter)
		}
	}
	if len(can) > 0 {
		return cant, can, true
	}
	if selfOK && !selfAt.Before(failedSince) && cfg.otherSite(target, cant) != "" {
		return cant, nil, true
	}
	return nil, nil, false
}

// otherSite is the one site every watcher in ws is at, when it is known and
// not target's site; otherwise "".
func (c *Config) otherSite(target string, ws []string) string {
	ts := c.site(target)
	if ts == "" || len(ws) == 0 {
		return ""
	}
	s := c.site(ws[0])
	for _, w := range ws {
		if c.site(w) != s {
			return ""
		}
	}
	if s == "" || s == ts {
		return ""
	}
	return s
}

// publicOnly: the observation's only problem is a public URL failing (the
// host itself, its hub, units and ports are fine).
func publicOnly(o Observation) bool {
	if o.SSH != SSHOK || o.Manager == ManagerAbsent || o.Manager == ManagerTimeout || len(o.UnitsDown) > 0 || len(o.TCPDown) > 0 {
		return false
	}
	for _, c := range o.Hub {
		if !c.OK {
			return false
		}
	}
	for _, c := range o.Public {
		if !c.OK {
			return true
		}
	}
	return false
}

// update advances the timer with this tick's level. A condition clears
// only after it has been absent for clearAfter; after a gap longer than gap
// since the last update the timer starts over (no stale grace or "since").
func (tm *Timer) update(l Level, now time.Time, clearAfter, gap time.Duration) {
	if !tm.LastEval.IsZero() && now.Sub(tm.LastEval) > gap {
		tm.Level, tm.DownLevel = LevelOK, LevelOK
		tm.DownSince, tm.DownClear, tm.DegradedSince, tm.OKSince = time.Time{}, time.Time{}, time.Time{}, time.Time{}
	}
	tm.LastEval, tm.Level = now, l
	if l >= LevelUnconfirmed {
		if tm.DownSince.IsZero() {
			tm.DownSince = now
		}
		tm.DownLevel, tm.DownClear = l, time.Time{}
	} else if !tm.DownSince.IsZero() {
		if tm.DownClear.IsZero() {
			tm.DownClear = now
		}
		if now.Sub(tm.DownClear) >= clearAfter {
			tm.DownSince, tm.DownClear, tm.DownLevel = time.Time{}, time.Time{}, LevelOK
		}
	}
	if l >= LevelDegraded {
		if tm.DegradedSince.IsZero() {
			tm.DegradedSince = now
		}
		tm.OKSince = time.Time{}
	} else {
		if tm.OKSince.IsZero() {
			tm.OKSince = now
		}
		if !tm.DegradedSince.IsZero() && now.Sub(tm.OKSince) >= clearAfter {
			tm.DegradedSince = time.Time{}
		}
	}
}

// confirmed is the level that has held for its grace period: DOWN (or DOWN
// unconfirmed) for DownGrace, DEGRADED or worse for DegradedGrace. "Held"
// tolerates short good spells (see update).
//
// Grace counts from the end of a maintenance window (QuietUntil) when the
// condition started inside it, and a public-URL-only problem needs
// PublicGrace.
func (tm *Timer) confirmed(now time.Time, t Timing) Level {
	from := func(since time.Time) time.Time {
		if tm.QuietUntil.After(since) {
			return tm.QuietUntil
		}
		return since
	}
	if !tm.DownSince.IsZero() && now.Sub(from(tm.DownSince)) >= t.DownGrace.D() && tm.DownLevel >= LevelUnconfirmed {
		return tm.DownLevel
	}
	grace := t.DegradedGrace.D()
	if tm.Public {
		grace = max(grace, t.PublicGrace.D())
	}
	if !tm.DegradedSince.IsZero() && now.Sub(from(tm.DegradedSince)) >= grace {
		return LevelDegraded
	}
	return LevelOK
}

func checkLabels(cs []Check) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Label)
	}
	return out
}

func joinAnd(xs []string) string {
	switch len(xs) {
	case 0:
		return ""
	case 1:
		return xs[0]
	case 2:
		return xs[0] + " and " + xs[1]
	}
	return strings.Join(xs[:len(xs)-1], ", ") + " and " + xs[len(xs)-1]
}
