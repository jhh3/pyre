package accounts

import (
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/usage"
)

// Local spread: a busy machine stops piling runs onto its home account.
func TestSpreadRanking(t *testing.T) {
	type acct struct {
		id     string
		five   float64
		active int
		home   bool
		pref   int
	}
	cases := []struct {
		name  string
		accts []acct
		want  string
	}{
		{"home under the run cap stays", []acct{{"a", 10, 3, true, 9}, {"b", 30, 0, false, 9}}, "a"},
		{"home at the run cap yields", []acct{{"a", 10, 4, true, 9}, {"b", 30, 0, false, 9}}, "b"},
		{"home at 5h 50% yields", []acct{{"a", 50, 0, true, 9}, {"b", 45, 2, false, 9}}, "b"},
		{"every account at the run cap: fewest runs", []acct{{"a", 10, 6, true, 9}, {"b", 20, 5, false, 9}, {"c", 5, 7, false, 9}}, "b"},
		{"run cap ranks before 5h >= 50%", []acct{{"a", 55, 0, true, 9}, {"b", 10, 5, false, 9}}, "b"},
		{"all at 5h >= 50%: home, then headroom", []acct{{"a", 70, 0, false, 9}, {"b", 60, 1, true, 9}, {"c", 55, 0, false, 9}}, "b"},
		{"all at 5h >= 50%, no home: most headroom", []acct{{"a", 70, 0, false, 9}, {"c", 55, 0, false, 9}}, "c"},
		{"29 runs on one account: the next goes elsewhere", []acct{{"code", 20, 29, true, 9}, {"john", 0, 0, false, 9}, {"dev", 5, 0, false, 9}}, "john"},
		{"explicit preference wins", []acct{{"a", 10, 9, true, 0}, {"b", 0, 0, false, 9}}, "a"},
	}
	e := newEnv(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var cs []*candidate
			for _, a := range tc.accts {
				c := &candidate{acct: contracts.Account{AccountID: a.id}, active: a.active, home: a.home, pref: a.pref,
					room: usage.Headroom{FiveHour: a.five, FiveOK: true, Weekly: 10, WeekOK: true}}
				c.near = nearLimit(c.room)
				cs = append(cs, c)
			}
			e.svc.markCrowd(cs)
			rank(cs)
			if cs[0].acct.AccountID != tc.want {
				var got []string
				for _, c := range cs {
					got = append(got, c.acct.AccountID)
				}
				t.Fatalf("order %v, want %s first", got, tc.want)
			}
		})
	}
	// Disabled spread: plain affinity (home stays until near its limit).
	e.svc.cfg.SpreadRuns, e.svc.cfg.SpreadFivePercent = -1, -1
	cs := []*candidate{
		{acct: contracts.Account{AccountID: "a"}, active: 20, home: true, room: usage.Headroom{FiveHour: 60, FiveOK: true}},
		{acct: contracts.Account{AccountID: "b"}, room: usage.Headroom{FiveHour: 0, FiveOK: true}},
	}
	e.svc.markCrowd(cs)
	rank(cs)
	if cs[0].acct.AccountID != "a" {
		t.Fatal("disabled spread still moved the run")
	}
}
