package usage

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// Token accounts: usage is read by a 1-token probe only while an account is
// in use (and at selection), so an idle token account's reading ages. Its
// last-known windows still say how it stands (Judge), and the selector
// re-probes before choosing it: the display counts it as available, never
// as unknown.

// ProbeOnUse: a token account whose usage is re-read when it is chosen.
const ProbeOnUse OnUse = "checked on use"

// assessToken returns the state of a token login whose reading aged out
// (ok=false: assess as usual).
func assessToken(obs *contracts.AccountObservation, now time.Time) (Assessment, bool) {
	if obs == nil || obs.Eligibility != contracts.EligibleYes {
		return Assessment{}, false
	}
	if !obs.Stale && now.Sub(obs.ObservedAt) <= FreshFor {
		return Assessment{}, false
	}
	return assessOnUse(ProbeOnUse, obs, now), true
}

// Credential kinds as the owner reads them.
const (
	KindToken = "token" // hub-held access token, every machine at once
	KindRota  = "rota"  // rota login checked out to one machine at a time
	KindLogin = "login" // a device login inside the profile
)

// Kind names how a card's login is held ("" for the ambient login or an
// unknown grant).
func (c Card) Kind() string {
	if c.Own {
		return KindOwn
	}
	if c.Grant == nil {
		return ""
	}
	switch c.Grant.Kind {
	case contracts.CredOAuthToken:
		return KindToken
	case contracts.CredRotaCheckout:
		return KindRota
	case contracts.CredClaudeProfileStore, contracts.CredCodexHomeFile, contracts.CredClaudeTokenFD:
		return KindLogin
	}
	return ""
}

// KindTag is the card's kind as shown next to its label: always for a
// token account, and for every kind when the provider mixes token
// accounts with others (MixedKinds), so each card says which it is.
func (c Card) KindTag(mixed bool) string {
	k := c.Kind()
	if k == KindToken || k == KindOwn || mixed && k != "" {
		return k
	}
	return ""
}

// MixedKinds reports, per provider, whether token accounts sit next to
// accounts of another kind.
func MixedKinds(cards []Card) map[contracts.Provider]bool {
	tok, other := map[contracts.Provider]bool{}, map[contracts.Provider]bool{}
	for _, c := range cards {
		switch k := c.Kind(); {
		case k == KindToken:
			tok[c.Provider] = true
		case k != "":
			other[c.Provider] = true
		}
	}
	out := map[contracts.Provider]bool{}
	for p := range tok {
		out[p] = other[p]
	}
	return out
}

// One card per token account: a token runs on every machine at once, so
// the account is one identity with one usage reading, not one login per
// machine. Its other grants (a rota login kept as a fallback on some
// machine) fold into the same card.

// AllMachines is the machine cell of a token account that every machine
// with the provider's harness can use.
const AllMachines = "all machines"

// hasTokenGrant reports whether any of the account's grants is a token.
func hasTokenGrant(v *contracts.AccountView) bool {
	for _, g := range v.Grants {
		if g.Kind == contracts.CredOAuthToken {
			return true
		}
	}
	return false
}

// tokenCard builds the single card of a token account.
func tokenCard(v *contracts.AccountView, nodes []contracts.Node, now time.Time) Card {
	p := v.Account.Provider
	var rep *contracts.DeviceGrant
	seen := map[string]bool{}
	var tokNodes []string
	usable := func(g *contracts.DeviceGrant) bool {
		return g.State == contracts.GrantValid || g.State == contracts.GrantExpiring
	}
	for i := range v.Grants {
		g := &v.Grants[i]
		if g.Kind != contracts.CredOAuthToken {
			continue
		}
		if !seen[g.NodeID] {
			seen[g.NodeID] = true
			tokNodes = append(tokNodes, g.NodeID)
		}
		switch {
		case rep == nil:
			rep = g
		case usable(g) != usable(rep):
			if usable(g) {
				rep = g
			}
		case g.ObservedAt.After(rep.ObservedAt):
			rep = g
		}
	}
	sort.Strings(tokNodes)
	// The newest reading from a machine holding the token, else any.
	var obs *contracts.AccountObservation
	for i := range v.ByNode {
		o := &v.ByNode[i]
		if seen[o.ReportingNode] && (obs == nil || o.ObservedAt.After(obs.ObservedAt)) {
			obs = o
		}
	}
	if obs == nil {
		for i := range v.ByNode {
			o := &v.ByNode[i]
			if obs == nil || o.ObservedAt.After(obs.ObservedAt) {
				obs = o
			}
		}
	}
	if obs == nil {
		obs = v.Latest
	}
	if obs != nil && len(obs.Windows) == 0 && obs.Eligibility == contracts.EligibleUnknown {
		if ws := newestWindows(v); ws != nil {
			cp := *obs
			cp.Windows = ws
			obs = &cp
		}
	}
	c := Card{Provider: p, Account: &v.Account, Obs: obs, Grant: rep, TokenNodes: tokNodes,
		Machines: machinesText(tokNodes, nodes, p)}
	c.Assessment = Assess(v.Account, obs, rep, now)
	c.Assignments = append(c.Assignments, v.Assignments...)
	return c
}

// machinesText is "all machines" when the token covers every listed
// machine with the provider's harness, else the machines' names ("2
// machines" when no machine list is known).
func machinesText(covered []string, nodes []contracts.Node, p contracts.Provider) string {
	if len(nodes) == 0 {
		if len(covered) == 1 {
			return "1 machine"
		}
		return fmt.Sprintf("%d machines", len(covered))
	}
	in := map[string]bool{}
	for _, id := range covered {
		in[id] = true
	}
	names := map[string]string{}
	all, harness := true, 0
	for _, n := range nodes {
		names[n.NodeID] = n.Name
		for _, h := range n.Harnesses {
			if h.Harness == harnessFor(p) && h.Available {
				harness++
				all = all && in[n.NodeID]
				break
			}
		}
	}
	if harness > 0 && all {
		return AllMachines
	}
	var out []string
	for _, id := range covered {
		if n := names[id]; n != "" {
			out = append(out, n)
		} else {
			out = append(out, id)
		}
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i]) < strings.ToLower(out[j]) })
	return strings.Join(out, ", ")
}

// Collapsed reports whether the card is a token account's single card
// (Machines names the machines it covers, NodeID is empty).
func (c Card) Collapsed() bool { return len(c.TokenNodes) > 0 }

// MachineCell is the card's MACHINE cell: the covered machines of a token
// card, else nodeName(NodeID).
func (c Card) MachineCell(nodeName func(string) string) string {
	if c.Collapsed() {
		return c.Machines
	}
	return nodeName(c.NodeID)
}
