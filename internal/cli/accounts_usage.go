package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/usage"
)

// `pyre accounts`: one row per account login (an account on one machine;
// a token account is one row for all its machines) grouped by provider, with the 5-hour and weekly windows as bars and the
// state in plain words (internal/usage). --watch redraws it.

const cliBarW = 10

// cliBar renders "███░░░░░░░  30%"; last-known numbers are parenthesized.
func cliBar(uw *c.UsageWindow, lastKnown bool) string {
	if uw == nil {
		return "-"
	}
	pct := max(0, min(100, uw.UsedPercent))
	fill := int(pct/100*cliBarW + 0.5)
	n := fmt.Sprintf("%3.0f%%", uw.UsedPercent)
	if lastKnown {
		n = "(" + strings.TrimSpace(n) + ")"
	}
	return strings.Repeat("█", fill) + strings.Repeat("░", cliBarW-fill) + " " + n
}

func cliReset(now time.Time, uw *c.UsageWindow) string {
	if uw == nil || uw.ResetAt == nil {
		return "-"
	}
	if !uw.ResetAt.After(now) {
		return "passed"
	}
	return usage.Clock(now, *uw.ResetAt) + " (" + usage.In(now, *uw.ResetAt) + ")"
}

// cardNote is the NOTES cell: why, until when, who is using it.
func cardNote(now time.Time, cd usage.Card, sessionTitle, nodeName func(string) string) string {
	var notes []string
	switch cd.State {
	case usage.AtLimit:
		if cd.Until != nil && cd.Obs != nil && resetShown(cd.Obs.Windows, *cd.Until) {
			notes = append(notes, "limit reached")
		} else if cd.Until != nil {
			notes = append(notes, "until "+usage.Clock(now, *cd.Until)+" ("+usage.In(now, *cd.Until)+")")
		} else {
			notes = append(notes, cd.Why)
		}
	case usage.NeedsReauth, usage.Unknown, usage.Ambient:
		notes = append(notes, cd.Why)
	}
	if cd.OnUse == usage.CheckOnUse && cd.State != usage.AtLimit {
		notes = append(notes, string(usage.CheckOnUse))
	}
	for _, a := range cd.Assignments {
		n := "in use by " + sessionTitle(a.SessionID)
		if a.NodeID != "" {
			n += " on " + nodeName(a.NodeID) // machine affinity: which machine holds it
		}
		notes = append(notes, n)
	}
	return orDash(strings.Join(notes, "; "))
}

// resetShown: the 5h or weekly column already shows reset t.
func resetShown(ws []c.UsageWindow, t time.Time) bool {
	sp := usage.Split(ws)
	for _, w := range []*c.UsageWindow{sp.FiveHour, sp.Weekly} {
		if w != nil && w.ResetAt != nil && w.ResetAt.Equal(t) {
			return true
		}
	}
	return false
}

// printAccounts prints the accounts at now (the hub's snapshot time, in
// this machine's time zone). nodes and sessionTitle may be empty/nil.
func printAccounts(w io.Writer, as []c.AccountView, nodes []c.Node, now time.Time, sessionTitle func(string) string) {
	if sessionTitle == nil {
		sessionTitle = func(id string) string { return id }
	}
	names := map[string]string{}
	for _, n := range nodes {
		names[n.NodeID] = n.Name
	}
	nodeName := func(id string) string {
		if n := names[id]; n != "" {
			return n
		}
		return orDash(id)
	}
	cards := usage.Cards(as, nodes, now)
	if len(cards) == 0 {
		fmt.Fprintln(w, "no accounts configured (pyre accounts register)")
		return
	}
	mixed := usage.MixedKinds(cards)
	pools := map[c.Provider]usage.Pool{}
	for _, p := range usage.Pools(cards) {
		pools[p.Provider] = p
	}
	lastKnown := false
	for i := 0; i < len(cards); {
		prov := cards[i].Provider
		j := i
		for j < len(cards) && cards[j].Provider == prov {
			j++
		}
		head := strings.ToUpper(string(prov))
		if p, ok := pools[prov]; ok {
			head += " · " + p.Summary()
			if p.Tight() {
				head += " · nothing available"
			}
		}
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintln(w, head)
		var rows [][]string
		for _, cd := range cards[i:j] {
			var five, week *c.UsageWindow
			if cd.Obs != nil {
				sp := usage.Split(cd.Obs.Windows)
				five, week = sp.FiveHour, sp.Weekly
			}
			lastKnown = lastKnown || cd.LastKnown && (five != nil || week != nil)
			label := cd.Label()
			if k := cd.KindTag(mixed[cd.Provider]); k != "" {
				label += " · " + k
			}
			rows = append(rows, []string{"  " + label, cd.MachineCell(nodeName), cd.Assessment.Label(),
				cliBar(five, cd.LastKnown), cliReset(now, five), cliBar(week, cd.LastKnown), cliReset(now, week), cardNote(now, cd, sessionTitle, nodeName)})
		}
		table(w, "  ACCOUNT\tMACHINE\tSTATE\t5H\tRESETS\tWEEK\tRESETS\tNOTES", rows)
		i = j
	}
	if lastKnown {
		fmt.Fprintln(w, "\n(n%) is the last known value; current usage is unknown.")
	}
}

// accountsOnce fetches what printAccounts needs.
func (a *App) accountsOnce(ctx context.Context, ops *Ops, provider string) (c.Page[c.AccountView], []c.Node, func(string) string, error) {
	page, err := listAll(ctx, func(ctx context.Context, cur string) (c.Page[c.AccountView], error) {
		return ops.API.ListAccounts(ctx, c.ListAccountsRequest{PageRequest: c.PageRequest{Cursor: cur}, Provider: c.Provider(provider)})
	})
	if err != nil {
		return page, nil, nil, err
	}
	// Machine names and session titles are best effort (IDs otherwise).
	var nodes []c.Node
	if np, err := ops.ListNodes(ctx, false); err == nil {
		nodes = np.Items
	}
	titles := map[string]string{}
	for _, av := range page.Items {
		for _, as := range av.Assignments {
			if _, ok := titles[as.SessionID]; ok {
				continue
			}
			titles[as.SessionID] = as.SessionID
			if sv, err := ops.API.GetSession(ctx, c.GetRequest{ID: as.SessionID}); err == nil && sv.Session.Title != "" {
				titles[as.SessionID] = sv.Session.Title
			}
		}
	}
	return page, nodes, func(id string) string { return titles[id] }, nil
}

func snapshotNow(page c.Page[c.AccountView]) time.Time {
	if !page.SnapshotAt.IsZero() {
		return page.SnapshotAt.In(time.Local)
	}
	return time.Now()
}

// runAccounts prints once, or with watch redraws every interval until
// interrupted (JSON mode: one line per refresh).
func (a *App) runAccounts(ctx context.Context, provider string, watch bool, every time.Duration) error {
	ops, err := a.connect()
	if err != nil {
		return err
	}
	if !watch {
		page, nodes, titles, err := a.accountsOnce(ctx, ops, provider)
		if err != nil {
			return err
		}
		return a.emit(page, func(w io.Writer) { printAccounts(w, page.Items, nodes, snapshotNow(page), titles) })
	}
	if every < time.Second {
		every = time.Second
	}
	clear := a.IsTerminal != nil && a.IsTerminal()
	for {
		page, nodes, titles, err := a.accountsOnce(ctx, ops, provider)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			a.warnf("refresh failed: %v (retrying in %s)", err, every)
		} else {
			err = a.emitLine(page, func(w io.Writer) {
				if clear {
					fmt.Fprint(w, "\x1b[H\x1b[2J")
				}
				printAccounts(w, page.Items, nodes, snapshotNow(page), titles)
				fmt.Fprintf(w, "\nrefreshing every %s · updated %s · ctrl+c to stop\n", every, time.Now().Format("3:04:05 PM"))
			})
			if err != nil {
				return err
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(every):
		}
	}
}
