package usage

import (
	"sort"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// The machines' own login. A node reports the identity of its own harness
// login (HarnessInfo.Login, read-only, never a credential). When it is an
// account the owner registered (rota_ref = the email, or the provider ref)
// that has no Pyre login anywhere, the account shows as one "own login"
// card covering those machines and counts as ready: Codex runs fine on each
// machine's own login, so "openai 0 ready" was wrong. Usage comes from the
// rate limits Pyre's own runs on that login reported (no probes).

// KindOwn is the kind tag of an own-login card.
const KindOwn = "own login"

// OwnUse: usage of an own login is known only from runs on it.
const OwnUse OnUse = "from runs on it"

// MatchesLogin reports whether account a is the login l of the same
// provider (rota_ref equal to its email, or the same provider ref).
func MatchesLogin(a contracts.Account, l *contracts.HarnessLogin) bool {
	if l == nil {
		return false
	}
	if l.Email != "" && strings.EqualFold(a.RotaRef, l.Email) {
		return true
	}
	return l.ProviderRef != "" && a.ProviderRef == l.ProviderRef
}

// ownLogins maps enabled accounts to the nodes whose own login they are,
// and marks those nodes per provider.
func ownLogins(views []contracts.AccountView, nodes []contracts.Node) (map[string][]string, map[contracts.Provider]map[string]bool) {
	own := map[string][]string{}
	at := map[contracts.Provider]map[string]bool{}
	for _, n := range nodes {
		for _, h := range n.Harnesses {
			if !h.Available || h.Login == nil {
				continue
			}
			p := contracts.ProviderFor(h.Harness)
			for _, v := range views {
				if v.Account.Disabled || v.Account.Provider != p || len(v.Grants) > 0 || !MatchesLogin(v.Account, h.Login) {
					continue
				}
				own[v.Account.AccountID] = append(own[v.Account.AccountID], n.NodeID)
				if at[p] == nil {
					at[p] = map[string]bool{}
				}
				at[p][n.NodeID] = true
				break
			}
		}
	}
	for id := range own {
		sort.Strings(own[id])
	}
	return own, at
}

// ownCard is the single card of an account that is the machines' own login.
// Without a reading from a run it is ready (the login works: runs use it);
// an aged reading still says how it stands (Judge), never "unknown".
func ownCard(v *contracts.AccountView, ids []string, nodes []contracts.Node, now time.Time) Card {
	in := map[string]bool{}
	for _, id := range ids {
		in[id] = true
	}
	var obs *contracts.AccountObservation
	for i := range v.ByNode {
		o := &v.ByNode[i]
		if in[o.ReportingNode] && len(o.Windows) > 0 && (obs == nil || o.ObservedAt.After(obs.ObservedAt)) {
			obs = o
		}
	}
	c := Card{Provider: v.Account.Provider, Account: &v.Account, Obs: obs, TokenNodes: ids, Own: true,
		Machines: machinesText(ids, nodes, v.Account.Provider)}
	switch {
	case obs == nil:
		c.Assessment = Assessment{State: Healthy, OnUse: OwnUse, Why: "your own login; usage shows after a Pyre run on it"}
	case !obs.Stale && now.Sub(obs.ObservedAt) <= FreshFor:
		c.Assessment = Assess(v.Account, obs, nil, now)
	default:
		c.Assessment = assessOnUse(OwnUse, obs, now)
	}
	c.Assignments = append(c.Assignments, v.Assignments...)
	return c
}
