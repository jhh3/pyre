package sentinel

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
)

// A link incident is a partition: watchers at one place cannot reach hosts
// that other watchers (or the hosts' own sentinels) say are fine, e.g. the
// home uplink congested so the office NUCs time out on server while the home
// machines see it healthy. It is one message when confirmed and one when
// the link has been stable for FlapWindow, with no reminders and no
// flapping notices in between; the hosts themselves are judged only by the
// watchers that reach them.

// linkClearAfter is the hysteresis of a link timer: a partition that comes
// and goes (a congested uplink) still accumulates toward its grace.
const linkClearAfter = 5 * time.Minute

// linkGroup is one link's partitions this tick.
type linkGroup struct {
	key, label string
	cant       map[string][]string // target -> watchers that cannot reach it
	fine       []string            // hosts seen up: the targets, then the watchers that reach them
}

func (g *linkGroup) addFine(names ...string) {
	for _, n := range names {
		if !slices.Contains(g.fine, n) {
			g.fine = append(g.fine, n)
		}
	}
}

// watchers are the sentinels that cannot reach some target of the link.
func (g *linkGroup) watchers() []string {
	var out []string
	for _, ws := range g.cant {
		for _, w := range ws {
			if !slices.Contains(out, w) {
				out = append(out, w)
			}
		}
	}
	slices.Sort(out)
	return out
}

// describe: "office-box1, office-box2 can't reach server; server, devbox
// and laptop are fine."
func (g *linkGroup) describe() string {
	bySet := map[string][]string{}
	for _, t := range sortedKeys(g.cant) {
		k := strings.Join(g.cant[t], ", ")
		bySet[k] = append(bySet[k], t)
	}
	var clauses []string
	for _, k := range sortedKeys(bySet) {
		clauses = append(clauses, k+" can't reach "+joinAnd(bySet[k]))
	}
	s := strings.Join(clauses, "; ")
	if len(g.fine) > 0 {
		verb := "are"
		if len(g.fine) == 1 {
			verb = "is"
		}
		s += "; " + joinAnd(g.fine) + " " + verb + " fine"
	}
	return s + "."
}

// linkKey names the link a partition of target runs over: the sorted pair
// of sites when every failing watcher is at one other site, else "network".
func (c *Config) linkKey(target string, cant []string) (key, label string) {
	other := c.otherSite(target, cant)
	if other == "" {
		return "network", ""
	}
	ts := c.site(target)
	pair := []string{other, ts}
	slices.Sort(pair)
	return pair[0] + "~" + pair[1], other + " ↔ " + ts
}

func linkName(key, label string) string {
	if label == "" {
		if a, b, ok := strings.Cut(key, "~"); ok {
			label = a + " ↔ " + b
		}
	}
	if label == "" {
		return "a network link"
	}
	return "the " + label + " link"
}

// linkGroups collects this tick's partitions by link. A target in a
// maintenance window is left out, and a link needs at least two failing
// watchers (one sentinel's broken path is not a link).
func (e *Engine) linkGroups(watched []Target, conds map[string]Cond, st *State, peers map[string]*State, now time.Time) map[string]*linkGroup {
	groups := map[string]*linkGroup{}
	names := make([]string, 0, len(watched))
	for _, t := range watched {
		names = append(names, t.Name)
	}
	slices.Sort(names)
	for _, name := range names {
		c := conds[name]
		if !c.Partition || e.inMaintenance(name, st, peers, now) {
			continue
		}
		key, label := e.Cfg.linkKey(name, c.CantReach)
		g := groups[key]
		if g == nil {
			g = &linkGroup{key: key, label: label, cant: map[string][]string{}}
			groups[key] = g
		}
		g.cant[name] = c.CantReach
		if c.Level == LevelOK {
			g.addFine(name)
		}
	}
	for key, g := range groups {
		if len(g.watchers()) < 2 {
			delete(groups, key)
			continue
		}
		for t := range g.cant {
			g.addFine(conds[t].CanReach...)
		}
	}
	return groups
}

// updateLinkTimers advances this sentinel's own evaluation of every link.
func (e *Engine) updateLinkTimers(st *State, groups map[string]*linkGroup, now time.Time) {
	gap := max(e.Cfg.Timing.Stale.D(), 3*e.Cfg.Timing.Interval.D())
	for key := range groups {
		if st.LinkTimers[key] == nil {
			st.LinkTimers[key] = &Timer{}
		}
	}
	for key, tm := range st.LinkTimers {
		l := LevelOK
		if groups[key] != nil {
			l = LevelDegraded
		}
		tm.update(l, now, linkClearAfter, gap)
		if l == LevelOK && tm.DegradedSince.IsZero() && st.Links[key] == nil && now.Sub(tm.OKSince) > FlapWindow {
			delete(st.LinkTimers, key)
		}
	}
}

// adoptLinks merges the link incidents live peers recorded (a previous
// actor's), like adopt does for targets.
func (e *Engine) adoptLinks(st *State, peers map[string]*State) {
	for key, mine := range st.Links {
		if st.closedAfter(mine) {
			delete(st.Links, key)
		}
	}
	for _, name := range sortedKeys(peers) {
		for _, key := range sortedKeys(peers[name].Links) {
			theirs := peers[name].Links[key]
			if theirs == nil || st.closedAfter(theirs) {
				continue
			}
			mine := st.Links[key]
			switch {
			case mine == nil:
				c := *theirs
				st.Links[key] = &c
			case mine.ID == theirs.ID:
				merge(mine, theirs)
			case alerted(theirs) && !alerted(mine):
				c := *theirs
				merge(&c, mine)
				c.ID = theirs.ID
				st.Links[key] = &c
			}
		}
	}
}

func closeLink(st *State, key string, inc *Incident) {
	delete(st.Links, key)
	st.Closed = append([]*Incident{inc}, st.Closed...)
	if len(st.Closed) > maxClosed {
		st.Closed = st.Closed[:maxClosed]
	}
}

// handleLinks runs the link incidents on their actor: one message once a
// partition has held DegradedGrace, and one "recovered" once it has been
// gone for FlapWindow. Nothing in between.
func (e *Engine) handleLinks(ctx context.Context, st *State, groups map[string]*linkGroup, now time.Time) {
	keys := sortedKeys(st.Links)
	for _, k := range sortedKeys(groups) {
		if !slices.Contains(keys, k) {
			keys = append(keys, k)
		}
	}
	for _, key := range keys {
		tm := st.LinkTimers[key]
		if tm == nil { // adopted without our own history: stability counts from now
			tm = &Timer{LastEval: now, OKSince: now}
			st.LinkTimers[key] = tm
		}
		inc, g := st.Links[key], groups[key]
		label := ""
		if g != nil {
			label = g.label
		}
		if inc != nil && !inc.Resolved.IsZero() {
			if g == nil && now.Sub(inc.Resolved) <= recoveredGiveUp {
				if e.send(ctx, inc.ID+"/recovered", linkRecoveredText(key, inc, now)) {
					inc.RecoveredSent = true
					closeLink(st, key, inc)
				}
				continue
			}
			closeLink(st, key, inc)
			inc = nil
		}
		conf := tm.confirmed(now, e.Cfg.Timing)
		if inc == nil {
			if g == nil || conf < LevelDegraded {
				continue
			}
			since := tm.DegradedSince
			inc = &Incident{
				ID: "link-" + key + "-" + since.UTC().Format("20060102T1504Z"), Target: "link:" + key,
				Level: LevelDegraded, Since: since, Confirmed: now,
			}
			st.Links[key] = inc
			e.logf("sentinel: link incident %s opened", inc.ID)
		}
		inc.Owner = e.Cfg.Self
		if g == nil {
			if !tm.OKSince.IsZero() && now.Sub(tm.OKSince) >= FlapWindow {
				inc.Resolved = now
				e.logf("sentinel: link incident %s resolved", inc.ID)
				if !alerted(inc) {
					closeLink(st, key, inc)
				} else if e.send(ctx, inc.ID+"/recovered", linkRecoveredText(key, inc, now)) {
					inc.RecoveredSent = true
					closeLink(st, key, inc)
				}
			}
			continue
		}
		inc.Detail, inc.By = g.describe(), g.watchers()
		if !alerted(inc) && conf >= LevelDegraded {
			if e.send(ctx, inc.ID+"/alert", linkText(key, label, inc, now)) {
				inc.AlertedLevel = LevelDegraded
				inc.FirstAlertAt, inc.LastAlertAt = now, now
			}
		}
	}
}

func linkText(key, label string, inc *Incident, now time.Time) string {
	name := linkName(key, label)
	head := fmt.Sprintf("Pyre: %s%s looks down since %s: %s", strings.ToUpper(name[:1]), name[1:], clockPT(inc.Since, now), inc.Detail)
	return sentence(head, fmt.Sprintf("Not a host outage; next message when it has been stable for %s.", span(FlapWindow)))
}

func linkRecoveredText(key string, inc *Incident, now time.Time) string {
	name := linkName(key, "")
	head := fmt.Sprintf("Pyre: %s%s recovered at %s (looked down since %s, %s; stable for %s).", strings.ToUpper(name[:1]), name[1:], clockPT(now, now), clockPT(inc.Since, now), span(now.Sub(inc.Since)), span(FlapWindow))
	return sentence(head, "Incident "+inc.ID+".")
}

// publicCheck marks a DEGRADED condition whose only problem is public URLs
// failing, and applies the target's own check of them (from its sentinel's
// fresh state, age on our clock): when the target reaches its URLs, only
// the watchers' path to the edge is failing and it is not an alert.
func (e *Engine) publicCheck(c *Cond, votes map[string]*Vote, ps *State, age time.Duration) {
	if c.Level != LevelDegraded || c.SentinelMissing || len(c.DownBy) > 0 || len(c.DegradedBy) == 0 {
		return
	}
	fail := map[string]bool{}
	for _, name := range c.DegradedBy {
		o := votes[name].Obs
		if !publicOnly(o) {
			return
		}
		for _, p := range o.Public {
			if !p.OK {
				fail[p.URL] = true
			}
		}
	}
	c.PublicOnly, c.PublicFail = true, sortedKeys(fail)
	if ps == nil || ps.SelfPublicAt.IsZero() || age+ps.UpdatedAt.Sub(ps.SelfPublicAt) > e.Cfg.Timing.Stale.D() {
		return
	}
	c.SelfPublicKnown = true
	for _, p := range ps.SelfPublic {
		if !p.OK && fail[p.URL] {
			c.SelfPublicFail = append(c.SelfPublicFail, p.URL+" ("+p.Detail+")")
		}
	}
	if len(c.SelfPublicFail) == 0 {
		c.Level = LevelOK
	}
}

// publicLine is the self-check sentence of a public-URL-only alert.
func publicLine(target string, c Cond) string {
	if c.SelfPublicKnown {
		return fmt.Sprintf("%s's own check fails too: %s.", target, strings.Join(c.SelfPublicFail, ", "))
	}
	return fmt.Sprintf("%s reports no check of its own public URLs.", target)
}
