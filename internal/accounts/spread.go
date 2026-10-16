package accounts

import "fmt"

// Local spread (W-xacct). Machine affinity keeps a machine on its home
// account until a window reaches usage.NearPercent (80 %), so every run a
// busy machine starts lands on the same account: 29 Claude sessions on server
// all ran on one token account and hit its 5-hour limit together. Inside an
// availability class, rank therefore orders by a spread level first
// (after an explicit Prefer list, before affinity):
//
//	crowdNone  room: fewer than SpreadRuns active runs here and the 5-hour
//	           window below SpreadFivePercent (or not reported);
//	crowdRuns  already SpreadRuns or more active runs on this machine
//	           (5-hour still below SpreadFivePercent); among these, fewest
//	           local runs first;
//	crowdFive  5-hour window at/above SpreadFivePercent.
//
// It never makes a run fail and never picks an at-limit account: when every
// eligible account is crowded the usual order (home, headroom) decides.
const (
	crowdNone = iota
	crowdRuns
	crowdFive
)

const (
	defaultSpreadRuns        = 4
	defaultSpreadFivePercent = 50
)

// markCrowd sets the spread level of every candidate (after c.active and
// c.room are known).
func (s *Service) markCrowd(cs []*candidate) {
	for _, c := range cs {
		c.crowd = crowdNone
		switch {
		case s.cfg.SpreadFivePercent > 0 && c.room.FiveOK && c.room.FiveHour >= s.cfg.SpreadFivePercent:
			c.crowd = crowdFive
		case s.cfg.SpreadRuns > 0 && c.active >= s.cfg.SpreadRuns:
			c.crowd = crowdRuns
		}
	}
}

// crowdNote says why an account was passed over to spread runs.
func (s *Service) crowdNote(c *candidate) string {
	switch c.crowd {
	case crowdRuns:
		return fmt.Sprintf("already %d runs on %s", c.active, s.nodeName())
	case crowdFive:
		return fmt.Sprintf("5h at %.0f%% (new runs go elsewhere from %.0f%%)", c.room.FiveHour, s.cfg.SpreadFivePercent)
	}
	return ""
}
