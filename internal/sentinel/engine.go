package sentinel

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"time"
)

// Clock is the time source (fake in tests).
type Clock interface{ Now() time.Time }

type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now() }

// WallClock is the real clock.
var WallClock Clock = wallClock{}

// Prober runs the checks. Probe also delivers push (this sentinel's last
// state.json) to the target when it runs a sentinel, and returns that
// sentinel's own state in Observation.PeerState, in one ssh session.
type Prober interface {
	Network(ctx context.Context) (ok bool, detail string)
	Probe(ctx context.Context, t Target, push []byte) Observation
}

// PublicChecker is implemented by a Prober that can check public URLs on
// their own: a sentinel's check of its own host's public endpoints, which
// a public-URL alert about that host needs.
type PublicChecker interface {
	CheckPublic(ctx context.Context, eps []Endpoint) []Check
}

// Recoverer performs the ssh-login recovery of a target whose user manager
// is not running.
type Recoverer interface {
	Recover(ctx context.Context, t Target) (string, error)
}

// Notifier delivers an alert to the owner. Send returns nil only when the
// alert was delivered in a way that notifies him.
type Notifier interface {
	// key names the message (incident ID + kind, stable across retries);
	// a fallback that does not notify records each key at most once.
	Send(ctx context.Context, key, text string) error
}

// Checker is implemented by a Notifier that can verify delivery without
// sending (Slack auth.test).
type Checker interface {
	Check(ctx context.Context) error
}

// Engine is one sentinel: each Tick probes, votes, reads peers, evaluates
// quorum and grace, and (as leader) alerts and recovers.
type Engine struct {
	Cfg       *Config
	Clock     Clock
	Prober    Prober
	Recoverer Recoverer
	Notifier  Notifier
	// DryRun logs alerts and recoveries instead of performing them and
	// never writes state.
	DryRun bool
	Logf   func(format string, args ...any)

	st        *State
	cur       *State // the state being built this tick (send records delivery in it)
	started   time.Time
	lastCheck time.Time
}

// StatePath is state.json in the configured state directory.
func (e *Engine) StatePath() string { return filepath.Join(e.Cfg.StateDir, "state.json") }

func (e *Engine) peerPath(name string) string {
	return filepath.Join(e.Cfg.StateDir, "peers", name+".json")
}

func (e *Engine) logf(format string, args ...any) {
	if e.Logf != nil {
		e.Logf(format, args...)
		return
	}
	log.Printf(format, args...)
}

// Load reads the previous state.json so incidents and timers survive a
// restart.
func (e *Engine) Load() error {
	if e.Clock == nil {
		e.Clock = WallClock
	}
	st, err := ReadState(e.StatePath())
	if err != nil {
		e.logf("sentinel: ignoring unreadable %s: %v", e.StatePath(), err)
	}
	if st == nil || st.Sentinel != e.Cfg.Self {
		st = newState(e.Cfg.Self, e.Cfg.rank(e.Cfg.Self))
	}
	e.st = st
	return nil
}

// State is the state after the last Tick.
func (e *Engine) State() *State { return e.st }

func (e *Engine) save(st *State) {
	if e.DryRun {
		return
	}
	// Stamped at write time: peers judge liveness by it, and a slow tick
	// (a timing-out probe, a recovery login) must not make us look dead.
	st.UpdatedAt = e.Clock.Now()
	if err := WriteState(e.StatePath(), st); err != nil {
		e.logf("sentinel: write state: %v", err)
	}
}

func (e *Engine) isSentinel(name string) bool { return e.Cfg.rank(name) > 0 }

// Tick runs one round.
func (e *Engine) Tick(ctx context.Context) error {
	if e.st == nil {
		if err := e.Load(); err != nil {
			return err
		}
	}
	cfg, prev := e.Cfg, e.st
	now := e.Clock.Now()
	if e.started.IsZero() {
		e.started = now
	}
	stale := cfg.Timing.Stale.D()
	st := newState(cfg.Self, cfg.rank(cfg.Self))
	st.UpdatedAt = now
	st.Timers, st.Maintenance, st.Incidents, st.Closed = prev.Timers, prev.Maintenance, prev.Incidents, prev.Closed
	st.LinkTimers, st.Links = prev.LinkTimers, prev.Links
	st.NotifyErr, st.NotifyAt = prev.NotifyErr, prev.NotifyAt
	st.fill()
	e.cur = st

	// 1. Own network first: a sentinel that cannot reach its gateway or the
	// internet abstains instead of voting everything down.
	st.NetworkOK, st.Network = e.Prober.Network(ctx)

	// Heartbeat: the last state, re-stamped, so peers reading it (or the
	// copy pushed below) during a long tick still see us as live.
	var push []byte
	if !prev.UpdatedAt.IsZero() && !e.DryRun {
		hb := *prev
		e.save(&hb)
		push, _ = json.Marshal(&hb)
	}

	// 2. Probe every watched target in parallel (pushing our last state to
	// the sentinels among them).
	watched := cfg.watched()
	obs := make([]Observation, len(watched))
	if st.NetworkOK {
		var wg sync.WaitGroup
		for i, t := range watched {
			var p []byte
			if e.isSentinel(t.Name) {
				p = push
			}
			wg.Go(func() { obs[i] = e.Prober.Probe(ctx, t, p) })
		}
		if me, ok := cfg.target(cfg.Self); ok && len(me.Public) > 0 {
			if pc, ok := e.Prober.(PublicChecker); ok {
				wg.Go(func() { st.SelfPublic = pc.CheckPublic(ctx, me.Public) })
				st.SelfPublicAt = now
			}
		}
		wg.Wait()
		e.checkNotify(ctx, st, now)
	}
	obsBy := map[string]*Observation{}
	for i, t := range watched {
		if obs[i].CheckedAt.IsZero() {
			obs[i].CheckedAt = now
		}
		obsBy[t.Name] = &obs[i]
	}
	for _, t := range watched {
		o := *obsBy[t.Name]
		if !st.NetworkOK {
			st.Votes[t.Name] = &Vote{Status: StatusUnknown, Reasons: []string{"own network check failed: " + st.Network}, Obs: Observation{CheckedAt: now}}
			continue
		}
		v := classify(o)
		if t.Via != "" && o.SSH != SSHOK && e.viaFailing(t.Via, prev, obsBy) {
			v = Vote{Status: StatusUnknown, Reasons: []string{"the path through " + t.Via + " is failing"}, Obs: o}
		}
		st.Votes[t.Name] = &v
		// Maintenance: what we read this tick is authoritative (absent means
		// cleared); otherwise keep our cached copy until it expires.
		if o.SSH == SSHOK {
			if o.Maintenance != nil {
				st.Maintenance[t.Name] = o.Maintenance
			} else {
				delete(st.Maintenance, t.Name)
			}
		}
	}
	for name, w := range st.Maintenance {
		if !w.Active(now) {
			delete(st.Maintenance, name)
		}
	}

	// 3. Peers: the fresher of the state we pulled from the peer's host and
	// the copy the peer pushed to us. Ages are measured on one clock: the
	// peer's for a pulled state (its clock at read time minus its stamp),
	// ours for a pushed one (the file's mtime). Fresh and network-OK means
	// live.
	peers := map[string]*State{}
	peerAge := map[string]time.Duration{}
	liveSet, offline := map[string]bool{}, map[string]bool{}
	if st.NetworkOK {
		liveSet[cfg.Self] = true
	}
	for _, s := range cfg.Sentinels {
		if s.Name == cfg.Self {
			continue
		}
		var best *State
		var bestAge time.Duration
		try := func(b []byte, age func(*State) time.Duration) {
			if len(b) == 0 {
				return
			}
			ps, err := ParseState(b)
			if err != nil || ps.Sentinel != s.Name {
				return
			}
			a := age(ps)
			if a < -time.Minute { // stamped in the future: not trustworthy
				return
			}
			if best == nil || a < bestAge {
				best, bestAge = ps, max(a, 0)
			}
		}
		if o := obsBy[s.Name]; o != nil && len(o.PeerState) > 0 {
			ref := now
			if !o.RemoteNow.IsZero() {
				ref = o.RemoteNow
			}
			try(o.PeerState, func(ps *State) time.Duration { return ref.Sub(ps.UpdatedAt) })
		}
		if b, mtime, err := readPeerFile(e.peerPath(s.Name)); err == nil {
			try(b, func(*State) time.Duration { return now.Sub(mtime) })
		}
		switch {
		case best != nil && bestAge <= stale && best.NetworkOK:
			peers[s.Name], peerAge[s.Name] = best, bestAge
			liveSet[s.Name] = true
		case best != nil && bestAge <= stale:
			offline[s.Name] = true // it says its own network is down
		}
	}
	st.Seen = map[string]Seen{}
	for _, s := range cfg.Sentinels {
		if liveSet[s.Name] {
			st.Live = append(st.Live, s.Name)
			if ps := peers[s.Name]; ps != nil {
				st.Seen[s.Name] = Seen{AgeS: int64(peerAge[s.Name] / time.Second), NotifyErr: ps.NotifyErr}
			}
		}
	}
	// Gossip: a sentinel a live peer saw as live is live for leadership even
	// when this machine has no path to it (one hop; we publish only what we
	// saw ourselves). It expires with the total age (our age of the peer's
	// state plus the peer's age of its view), and carries the delivery
	// status the peer saw.
	known := map[string]bool{}
	for n := range liveSet {
		known[n] = true
	}
	gossipNotify := map[string]string{}
	gossipAge := map[string]time.Duration{}
	for _, pname := range sortedKeys(peers) {
		for n, sn := range peers[pname].Seen {
			if cfg.rank(n) == 0 || n == cfg.Self || liveSet[n] || offline[n] {
				continue
			}
			total := peerAge[pname] + time.Duration(sn.AgeS)*time.Second
			if total > stale {
				continue
			}
			if a, ok := gossipAge[n]; !ok || total < a {
				known[n], gossipAge[n], gossipNotify[n] = true, total, sn.NotifyErr
			}
		}
	}
	var liveRanked []string
	for _, s := range cfg.Sentinels {
		if known[s.Name] {
			liveRanked = append(liveRanked, s.Name)
		}
	}
	if len(liveRanked) > 0 {
		st.Leader = liveRanked[0]
	}

	// A target whose own sentinel is reporting is not DOWN because this
	// machine's ssh to it failed: that is a path problem (DEGRADED at most).
	for _, t := range watched {
		v := st.Votes[t.Name]
		if v.Status != StatusDown || !liveSet[t.Name] || v.Obs.SSH == SSHOK || v.Obs.SSH == SSHHung {
			continue
		}
		hubUp := len(v.Obs.Hub) == 0
		for _, c := range v.Obs.Hub {
			hubUp = hubUp || c.OK
		}
		if hubUp {
			v.Status = StatusDegraded
			v.Reasons = []string{fmt.Sprintf("no ssh path from %s (%s); its sentinel is reporting", cfg.Self, v.Obs.SSHDetail)}
		}
	}
	for name, v := range st.Votes {
		v.Since = now
		if pv := prev.Votes[name]; pv != nil && pv.Status == v.Status && !pv.Since.IsZero() {
			v.Since = pv.Since
		}
	}

	// 4. Evaluate every watched target from the live votes.
	conds := map[string]Cond{}
	votesBy := map[string]map[string]*Vote{}
	gap := max(stale, 3*cfg.Timing.Interval.D())
	for _, t := range watched {
		votes := map[string]*Vote{}
		if st.NetworkOK {
			votes[cfg.Self] = st.Votes[t.Name]
		}
		for name, ps := range peers {
			v := ps.Votes[t.Name]
			if v != nil && peerAge[name]+ps.UpdatedAt.Sub(v.Obs.CheckedAt) <= stale {
				votes[name] = v
			}
		}
		var missing []string
		for _, s := range cfg.Sentinels {
			if s.Name != t.Name && !known[s.Name] && !e.viaFailing(s.Via, prev, obsBy) {
				missing = append(missing, s.Name)
			}
		}
		// "Missing" needs a full stale period of listening first.
		sentinelMissing := false
		if r, ok := cfg.sentinel(t.Name); ok && !known[t.Name] && now.Sub(e.started) >= stale && !e.viaFailing(r.Via, prev, obsBy) {
			sentinelMissing = true
		}
		// Partition: watchers that cannot reach the target at all while
		// another can (or its own sentinel reports) do not vote on the host;
		// the path is a link incident below.
		ps := peers[t.Name]
		var selfAt time.Time
		if ps != nil {
			selfAt = now.Add(-peerAge[t.Name])
		}
		cant, can, part := partition(cfg, t.Name, votes, ps != nil, selfAt)
		ev := votes
		if part {
			ev = map[string]*Vote{}
			for name, v := range votes {
				if !slices.Contains(cant, name) {
					ev[name] = v
				}
			}
		}
		c := evaluate(t.Name, ev, missing, sentinelMissing)
		if part {
			c.Partition, c.CantReach, c.CanReach, c.SelfOK = true, cant, can, ps != nil
			if c.Abstain {
				c.Abstain, c.Level = false, LevelOK // only its own sentinel speaks for it: up
			}
		}
		e.publicCheck(&c, ev, ps, peerAge[t.Name])
		conds[t.Name], votesBy[t.Name] = c, ev
		tm := st.Timers[t.Name]
		if tm == nil {
			tm = &Timer{}
			st.Timers[t.Name] = tm
		}
		if !st.NetworkOK || c.Abstain {
			continue // nobody could tell: keep the grace timers as they were
		}
		tm.update(c.Level, now, cfg.Timing.RecoverGrace.D(), gap)
		tm.Public = c.PublicOnly && c.Level == LevelDegraded
		if e.inMaintenance(t.Name, st, peers, now) {
			tm.QuietUntil = now
		}
		if o := st.Votes[t.Name].Obs; o.ManagerStart != "" && o.ManagerStart != tm.ManagerStart {
			if tm.ManagerStart != "" {
				tm.PrevManagerStart, tm.StartChangedAt = tm.ManagerStart, now
			}
			tm.ManagerStart = o.ManagerStart
		}
	}
	for name := range st.Timers {
		if _, ok := cfg.target(name); !ok || name == cfg.Self {
			delete(st.Timers, name)
		}
	}

	// 4b. Links: partitions of the targets this sentinel watches, grouped
	// by the pair of sites (or one "network" link without sites).
	groups := e.linkGroups(watched, conds, st, peers, now)
	if st.NetworkOK {
		e.updateLinkTimers(st, groups, now)
	}

	// 5. Incidents. Each target has one actor: the first live sentinel in
	// rank order other than the target itself (so the leader's own host is
	// handled by the next one), skipping one whose last bot DM failed while
	// another can deliver. The actor alone alerts and recovers; every other
	// sentinel mirrors the actor's incident, so a successor knows what was
	// already sent and attempted.
	st.Closed = mergeClosed(st.Closed, peers)
	for _, t := range cfg.Targets {
		actor := e.actor(t.Name, liveRanked, st, peers, gossipNotify)
		switch {
		case actor == cfg.Self:
			e.adopt(st, t.Name, peers)
			if c := conds[t.Name]; !c.Abstain {
				e.handle(ctx, st, t, c, votesBy[t.Name], e.inMaintenance(t.Name, st, peers, now), peers, now)
			}
		case peers[actor] != nil:
			as := peers[actor]
			if inc := as.Incidents[t.Name]; inc != nil {
				c := *inc
				st.Incidents[t.Name] = &c
			} else {
				delete(st.Incidents, t.Name)
			}
			if lt, tm := as.Timers[t.Name], st.Timers[t.Name]; lt != nil && tm != nil {
				if lt.NotifiedStart != "" {
					tm.NotifiedStart = lt.NotifiedStart
				}
				if lt.LastRestartNotice.After(tm.LastRestartNotice) {
					tm.LastRestartNotice = lt.LastRestartNotice
				}
			}
		}
	}
	// Links have one actor too: the first live sentinel that can deliver.
	switch actor := e.actor("", liveRanked, st, peers, gossipNotify); {
	case actor == cfg.Self:
		e.adoptLinks(st, peers)
		if st.NetworkOK {
			e.handleLinks(ctx, st, groups, now)
		}
	case peers[actor] != nil:
		st.Links = map[string]*Incident{}
		for k, inc := range peers[actor].Links {
			c := *inc
			st.Links[k] = &c
		}
	}
	if st.NetworkOK {
		st.Seen[cfg.Self] = Seen{NotifyErr: st.NotifyErr}
	}
	e.st = st
	e.save(st)
	return nil
}

// actor picks who acts for target: the first live sentinel (rank order)
// other than the target whose bot delivery is not known to be failing; if
// none can deliver, the first one anyway.
func (e *Engine) actor(target string, liveRanked []string, st *State, peers map[string]*State, gossipNotify map[string]string) string {
	first := ""
	for _, name := range liveRanked {
		if name == target {
			continue
		}
		if first == "" {
			first = name
		}
		notifyErr := ""
		if name == e.Cfg.Self {
			notifyErr = st.NotifyErr
		} else if ps := peers[name]; ps != nil {
			notifyErr = ps.NotifyErr
		} else {
			notifyErr = gossipNotify[name] // known only through a peer
		}
		if notifyErr == "" {
			return name
		}
	}
	return first
}

// viaFailing: the only path to something runs through via, and via is DOWN
// (last tick) or this machine's ssh to via failed this tick.
func (e *Engine) viaFailing(via string, prev *State, obsBy map[string]*Observation) bool {
	if via == "" {
		return false
	}
	if o := obsBy[via]; o != nil && o.SSH != "" && o.SSH != SSHOK {
		return true
	}
	tm := prev.Timers[via]
	return tm != nil && tm.Level >= LevelUnconfirmed
}

// notifyCheckEvery is how often the bot token is verified (auth.test).
const notifyCheckEvery = 10 * time.Minute

// checkNotify verifies delivery when the Notifier can (auth.test), at most
// every notifyCheckEvery, and records the result in st.
func (e *Engine) checkNotify(ctx context.Context, st *State, now time.Time) {
	c, ok := e.Notifier.(Checker)
	if !ok || e.DryRun || (!e.lastCheck.IsZero() && now.Sub(e.lastCheck) < notifyCheckEvery) {
		return
	}
	e.lastCheck = now
	st.NotifyAt = now
	if err := c.Check(ctx); err != nil {
		st.NotifyErr = err.Error()
		e.logf("sentinel: Slack delivery check failed: %v", err)
		return
	}
	st.NotifyErr = ""
}

func (e *Engine) inMaintenance(target string, st *State, peers map[string]*State, now time.Time) bool {
	if st.Maintenance[target].Active(now) {
		return true
	}
	for _, ps := range peers {
		if ps.Maintenance[target].Active(now) {
			return true
		}
	}
	return false
}

// adopt merges what live peers recorded about target (a previous actor, or
// a sentinel that acted while this one was unreachable) into st, so taking
// over never repeats an alert or a recovery attempt. An incident any peer
// closed is closed here too, whatever the reason.
func (e *Engine) adopt(st *State, target string, peers map[string]*State) {
	if mine := st.Incidents[target]; mine != nil && st.closedAfter(mine) {
		delete(st.Incidents, target)
	}
	for _, name := range sortedKeys(peers) {
		theirs := peers[name].Incidents[target]
		if theirs == nil || st.closedAfter(theirs) {
			continue
		}
		mine := st.Incidents[target]
		switch {
		case mine == nil:
			c := *theirs
			st.Incidents[target] = &c
		case mine.ID == theirs.ID:
			merge(mine, theirs)
		case alerted(theirs) && (!alerted(mine) || theirs.Since.Before(mine.Since)):
			c := *theirs
			merge(&c, mine)
			c.ID = theirs.ID
			st.Incidents[target] = &c
		}
	}
}

// mergeClosed is the union of our and the live peers' closed incidents,
// newest first, bounded.
func mergeClosed(mine []*Incident, peers map[string]*State) []*Incident {
	seen := map[string]bool{}
	var out []*Incident
	add := func(list []*Incident) {
		for _, c := range list {
			if c != nil && !seen[c.ID] {
				seen[c.ID] = true
				out = append(out, c)
			}
		}
	}
	add(mine)
	for _, name := range sortedKeys(peers) {
		add(peers[name].Closed)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Resolved.After(out[j].Resolved) })
	if len(out) > maxClosed {
		out = out[:maxClosed]
	}
	return out
}

// readPeerFile reads a pushed peer state (bounded: another machine wrote
// it) and its mtime (our clock: the write happened on this machine).
func readPeerFile(path string) ([]byte, time.Time, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, time.Time{}, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, time.Time{}, err
	}
	b, err := io.ReadAll(io.LimitReader(f, maxStateBytes))
	return b, fi.ModTime(), err
}

// maxStateBytes bounds a state.json read from a peer.
const maxStateBytes = 4 << 20

func alerted(i *Incident) bool { return !i.FirstAlertAt.IsZero() }

// merge folds other's progress into inc (same problem, two records).
func merge(inc, other *Incident) {
	if other.AlertedLevel > inc.AlertedLevel {
		inc.AlertedLevel = other.AlertedLevel
	}
	if other.Level > inc.Level {
		inc.Level = other.Level
	}
	if !other.FirstAlertAt.IsZero() && (inc.FirstAlertAt.IsZero() || other.FirstAlertAt.Before(inc.FirstAlertAt)) {
		inc.FirstAlertAt = other.FirstAlertAt
	}
	if other.LastAlertAt.After(inc.LastAlertAt) {
		inc.LastAlertAt = other.LastAlertAt
	}
	if other.Since.Before(inc.Since) {
		inc.Since = other.Since
	}
	inc.RemindersSent = max(inc.RemindersSent, other.RemindersSent)
	inc.ManualHelpSent = inc.ManualHelpSent || other.ManualHelpSent
	seen := map[int64]bool{}
	for _, r := range inc.Recovery {
		seen[r.At.UnixNano()] = true
	}
	for _, r := range other.Recovery {
		if !seen[r.At.UnixNano()] {
			inc.Recovery = append(inc.Recovery, r)
		}
	}
	sort.Slice(inc.Recovery, func(i, j int) bool { return inc.Recovery[i].At.Before(inc.Recovery[j].At) })
}

// detailObs picks the observation a message describes: this sentinel's own
// when it agrees with the condition, else the first agreeing voter's.
func detailObs(self string, c Cond, votes map[string]*Vote) *Observation {
	by := c.voters()
	for _, v := range by {
		if v == self {
			return &votes[self].Obs
		}
	}
	if len(by) > 0 {
		return &votes[by[0]].Obs
	}
	if v := votes[self]; v != nil {
		return &v.Obs
	}
	return nil
}

func (e *Engine) send(ctx context.Context, key, text string) bool {
	if e.DryRun {
		e.logf("sentinel: [dry-run] would send: %s", text)
		return true
	}
	err := e.Notifier.Send(ctx, key, text)
	if e.cur != nil {
		e.cur.NotifyAt = e.Clock.Now()
		e.cur.NotifyErr = ""
		if err != nil {
			e.cur.NotifyErr = err.Error()
		}
	}
	if err != nil {
		e.logf("sentinel: alert not delivered (will retry): %v: %s", err, text)
		return false
	}
	e.logf("sentinel: sent: %s", text)
	return true
}

// earliestVote is the earliest Since among the named voters' votes: when
// the problem was first seen, before quorum and grace.
// It is at most two votes' lag before quorumSince, so one sentinel's long
// private view (a path broken for days) does not date the incident.
func earliestVote(voters []string, votes map[string]*Vote, quorumSince time.Time, interval time.Duration) time.Time {
	t := quorumSince
	floor := quorumSince.Add(-2 * interval)
	for _, name := range voters {
		if v := votes[name]; v != nil && !v.Since.IsZero() && v.Since.Before(t) {
			t = v.Since
		}
	}
	if t.Before(floor) {
		t = floor
	}
	return t
}

// downSince is when the DOWN condition was first seen.
func (e *Engine) downSince(c Cond, votes map[string]*Vote, tm *Timer) time.Time {
	return earliestVote(c.DownBy, votes, tm.DownSince, e.Cfg.Timing.Interval.D())
}

func canRecover(t Target, o *Observation) bool {
	return !t.NoRecovery && o != nil && o.SSH == SSHOK && o.Manager == ManagerAbsent
}

// FlapWindow: a target failing again this soon after "recovered" reopens
// the incident, which then needs FlapWindow of stability to resolve.
const FlapWindow = 30 * time.Minute

// restartNoticeEvery limits "user manager restarted" to one per target.
const restartNoticeEvery = time.Hour

// recoveredGiveUp bounds retries of an undeliverable "recovered" message.
const recoveredGiveUp = time.Hour

// handle runs one target's incident lifecycle on the leader.
func (e *Engine) handle(ctx context.Context, st *State, t Target, c Cond, votes map[string]*Vote, maint bool, peers map[string]*State, now time.Time) {
	cfg := e.Cfg
	tm := st.Timers[t.Name]
	conf := tm.confirmed(now, cfg.Timing)
	inc := st.Incidents[t.Name]
	var mine *Observation
	if v := votes[cfg.Self]; v != nil {
		mine = &v.Obs
	}
	obs := detailObs(cfg.Self, c, votes)

	defer func() { e.restartNotice(ctx, st, t, tm, st.Incidents[t.Name], maint, peers, now) }()

	if inc != nil && !inc.Resolved.IsZero() {
		if conf >= LevelDegraded || now.Sub(inc.Resolved) > recoveredGiveUp {
			st.close(inc)
			inc = nil
		} else {
			if e.send(ctx, fmt.Sprintf("%s/recovered/%d", inc.ID, inc.Reopened), recoveredText(inc, now)) {
				inc.RecoveredSent = true
				st.close(inc)
			}
			return
		}
	}
	reopened := false
	if inc == nil {
		if conf < LevelDegraded {
			return
		}
		if last := st.lastClosed(t.Name); last != nil && alerted(last) && now.Sub(last.Resolved) < FlapWindow {
			// Back within FlapWindow of "recovered": the same problem,
			// flapping. Reopen it rather than start a new alert cycle.
			st.removeClosed(last.ID)
			inc = last
			inc.Reopened++
			inc.Resolved, inc.RecoveredSent = time.Time{}, false
			reopened = true
			e.logf("sentinel: incident %s reopened (%d)", inc.ID, inc.Reopened)
		} else {
			since := earliestVote(c.voters(), votes, tm.DegradedSince, cfg.Timing.Interval.D())
			inc = &Incident{
				ID: t.Name + "-" + since.UTC().Format("20060102T1504Z"), Target: t.Name,
				Level: conf, Since: since, Confirmed: now,
			}
			e.logf("sentinel: incident %s opened: %s", inc.ID, conf)
		}
		st.Incidents[t.Name] = inc
	}
	inc.Owner = cfg.Self
	if conf > inc.Level {
		inc.Level = conf
	}
	if conf >= LevelDegraded {
		inc.Detail, inc.By = describe(obs, c), c.voters()
	}
	if conf == LevelOK {
		clear := cfg.Timing.RecoverGrace.D()
		if inc.Reopened > 0 {
			clear = FlapWindow // flapping: "recovered" only once it has been stable
		}
		if !tm.OKSince.IsZero() && now.Sub(tm.OKSince) >= clear {
			inc.Resolved = now
			e.logf("sentinel: incident %s resolved", inc.ID)
			if !alerted(inc) {
				st.close(inc)
			} else if e.send(ctx, fmt.Sprintf("%s/recovered/%d", inc.ID, inc.Reopened), recoveredText(inc, now)) {
				inc.RecoveredSent = true
				st.close(inc)
			}
		}
		return
	}
	if maint {
		return
	}

	maxN := cfg.Timing.MaxRecoveryAttempts
	var attempt *RecoveryAttempt
	if conf == LevelDown && canRecover(t, mine) && len(inc.Recovery) < maxN &&
		(len(inc.Recovery) == 0 || now.Sub(inc.Recovery[len(inc.Recovery)-1].At) >= cfg.Timing.RecoveryRetry.D()) {
		inc.Recovery = append(inc.Recovery, RecoveryAttempt{At: now, By: cfg.Self})
		e.save(st) // recorded before acting: never more than maxN, even across a crash
		res := "dry-run"
		if !e.DryRun {
			out, err := e.Recoverer.Recover(ctx, t)
			res = "ok"
			if err != nil {
				res = "failed: " + err.Error()
			} else if out != "" {
				res = out
			}
		}
		inc.Recovery[len(inc.Recovery)-1].Result = res
		attempt = &inc.Recovery[len(inc.Recovery)-1]
		e.logf("sentinel: %s recovery attempt %d: %s", t.Name, len(inc.Recovery), res)
	}

	since := inc.Since
	if conf >= LevelUnconfirmed {
		since = e.downSince(c, votes, tm)
	}
	last := len(inc.Recovery)
	// conf can stay DOWN for the clear window after the target came back:
	// nothing new is said unless this round itself is still bad.
	bad := tm.Level >= LevelDegraded
	switch {
	case !bad && !reopened && attempt == nil:
	case reopened:
		if e.send(ctx, fmt.Sprintf("%s/flap/%d", inc.ID, inc.Reopened), flapText(t.Name, conf, since, inc, c, obs, now)) {
			inc.AlertedLevel = max(inc.AlertedLevel, conf)
			inc.LastAlertAt = now
		} else {
			inc.AlertedLevel = min(inc.AlertedLevel, conf-1) // retried as an alert
		}
	case inc.AlertedLevel < conf:
		rec := ""
		if conf == LevelDown {
			rec = recoveryLine(t, mine, attempt, last, maxN, now)
		} else if c.PublicOnly {
			rec = publicLine(t.Name, c)
		}
		if e.send(ctx, fmt.Sprintf("%s/alert/%s/%d", inc.ID, conf, inc.Reopened), alertText(t.Name, conf, since, c, obs, rec, now)) {
			inc.AlertedLevel = conf
			if inc.FirstAlertAt.IsZero() {
				inc.FirstAlertAt = now
			}
			inc.LastAlertAt = now
		}
	case attempt != nil:
		msg := fmt.Sprintf("Pyre: %s is still DOWN.", t.Name)
		if e.send(ctx, fmt.Sprintf("%s/attempt/%d", inc.ID, last), sentence(msg, recoveryLine(t, mine, attempt, last, maxN, now))) {
			inc.LastAlertAt = now
		}
	case conf == LevelDown && tm.Level >= LevelUnconfirmed && last >= maxN && !inc.ManualHelpSent &&
		now.Sub(inc.Recovery[last-1].At) >= cfg.Timing.RecoveryRetry.D():
		if e.send(ctx, inc.ID+"/manual", manualHelpText(inc, c, obs, now)) {
			inc.ManualHelpSent, inc.LastAlertAt = true, now
		}
	case alerted(inc) && inc.RemindersSent < len(cfg.Timing.Reminders) &&
		now.Sub(inc.FirstAlertAt) >= cfg.Timing.Reminders[inc.RemindersSent].D():
		if inc.Since.Before(since) {
			since = inc.Since // the whole incident, across flaps
		}
		if e.send(ctx, fmt.Sprintf("%s/reminder/%d/%d", inc.ID, inc.Reopened, inc.RemindersSent), reminderText(t.Name, conf, since, c, obs, now)) {
			inc.RemindersSent++
			inc.LastAlertAt = now
		}
	}
}

// restartNotice reports a user-manager restart the leader saw (its own
// observation's start time changed), once. A restart inside an alerted
// incident or a maintenance window is covered or expected, so it is only
// recorded.
func (e *Engine) restartNotice(ctx context.Context, st *State, t Target, tm *Timer, inc *Incident, maint bool, peers map[string]*State, now time.Time) {
	if tm.PrevManagerStart == "" || tm.ManagerStart == "" || tm.NotifiedStart == tm.ManagerStart {
		return
	}
	for _, ps := range peers {
		if pt := ps.Timers[t.Name]; pt != nil && pt.NotifiedStart == tm.ManagerStart {
			tm.NotifiedStart = tm.ManagerStart
			return
		}
	}
	if maint || (inc != nil && alerted(inc)) || now.Sub(tm.StartChangedAt) > recoveredGiveUp {
		tm.NotifiedStart = tm.ManagerStart
		return
	}
	if !tm.LastRestartNotice.IsZero() && now.Sub(tm.LastRestartNotice) < restartNoticeEvery {
		// Rate limit (e.g. linger off: every probe login restarts it).
		tm.NotifiedStart = tm.ManagerStart
		tm.RestartsMuted++
		return
	}
	if e.send(ctx, t.Name+"/restart/"+tm.ManagerStart, restartText(t.Name, parseUnixStamp(tm.ManagerStart), parseUnixStamp(tm.PrevManagerStart), tm.RestartsMuted, now)) {
		tm.NotifiedStart, tm.LastRestartNotice, tm.RestartsMuted = tm.ManagerStart, now, 0
	}
}

// tickTimeout bounds one round (probes, at most one recovery login, alerts).
const tickTimeout = 3 * time.Minute

// Run ticks every Timing.Interval until ctx is done.
func (e *Engine) Run(ctx context.Context) error {
	if err := e.Load(); err != nil {
		return err
	}
	iv := e.Cfg.Timing.Interval.D()
	for {
		start := time.Now()
		tctx, cancel := context.WithTimeout(ctx, tickTimeout)
		if err := e.Tick(tctx); err != nil {
			e.logf("sentinel: tick: %v", err)
		}
		cancel()
		wait := iv - time.Since(start)
		if wait < time.Second {
			wait = time.Second
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
	}
}
