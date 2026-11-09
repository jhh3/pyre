package cli

// `pyre node avoid` and MCP avoid_node: keep new work off a machine for a
// while (presenting, a timed benchmark, heavy local work). The flag lives
// on the hub (contracts/node_avoid.go) and lifts by itself at its expiry.
// Soft (default): automatic placement skips the node unless nothing else
// fits, an explicit --node starts with a warning. Hard: the hub refuses a
// start there unless --ignore-avoid (the owner's flag yields only to the
// owner). Sessions already on the node keep running.

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	c "github.com/jhh3/pyre/internal/contracts"
)

func init() { mcpToolProviders = append(mcpToolProviders, avoidMCPTools) }

func (o *Ops) nodeAvoid() (c.NodeAvoidAPI, error) {
	if a, ok := o.API.(c.NodeAvoidAPI); ok {
		return a, nil
	}
	return nil, c.Errorf(c.ErrUnsupported, "this connection does not offer node avoid flags (%s); upgrade pyre-hub", c.OpNodesAvoid)
}

// AvoidParams are `pyre node avoid`'s arguments (MCP avoid_node input).
type AvoidParams struct {
	Node        string `json:"node"`
	For         string `json:"for,omitempty"`   // a duration: 2h, 90m, 1d (default 2h)
	Until       string `json:"until,omitempty"` // a local clock time (18:00, 6pm; earlier than now = tomorrow) or a date and time
	Reason      string `json:"reason,omitempty"`
	Hard        bool   `json:"hard,omitempty"`
	Clear       bool   `json:"clear,omitempty"`
	Project     string `json:"project,omitempty"`
	OperationID string `json:"operation_id,omitempty"`
}

// AvoidNode sets or clears a node's avoid flag.
func (o *Ops) AvoidNode(ctx context.Context, p AvoidParams) (c.AvoidNodeResponse, error) {
	api, err := o.nodeAvoid()
	if err != nil {
		return c.AvoidNodeResponse{}, err
	}
	if strings.TrimSpace(p.Node) == "" {
		return c.AvoidNodeResponse{}, usagef("say which node to avoid (see `pyre nodes`)")
	}
	req := c.AvoidNodeRequest{Mutation: mut(p.OperationID), Hard: p.Hard, Reason: strings.TrimSpace(p.Reason), Clear: p.Clear}
	if p.Clear {
		if p.For != "" || p.Until != "" || p.Hard || req.Reason != "" {
			return c.AvoidNodeResponse{}, usagef("--clear takes only the node (and --project)")
		}
	} else {
		at, err := avoidExpiry(o.now(), p.For, p.Until)
		if err != nil {
			return c.AvoidNodeResponse{}, err
		}
		at = at.UTC()
		req.ExpiresAt = &at
		if len(req.Reason) > c.NodeAvoidMaxReason {
			return c.AvoidNodeResponse{}, usagef("--reason is at most %d characters", c.NodeAvoidMaxReason)
		}
	}
	if req.NodeID, err = o.NodeID(ctx, p.Node); err != nil {
		return c.AvoidNodeResponse{}, err
	}
	if req.ProjectID, err = o.projectPtr(ctx, p.Project); err != nil {
		return c.AvoidNodeResponse{}, err
	}
	return api.AvoidNode(ctx, req)
}

// ---- --for / --until ----

var avoidDaysRe = regexp.MustCompile(`^(\d+)d(.*)$`)

// parseAvoidFor reads a duration: Go syntax (2h, 90m, 1h30m) plus whole
// days (1d, 2d12h).
func parseAvoidFor(s string) (time.Duration, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	var d time.Duration
	if m := avoidDaysRe.FindStringSubmatch(s); m != nil {
		n, err := strconv.Atoi(m[1])
		if err != nil || n > 366 {
			return 0, usagef("--for %q is not a duration (e.g. 2h, 90m, 1d)", s)
		}
		d, s = time.Duration(n)*24*time.Hour, m[2]
	}
	if s != "" {
		rest, err := time.ParseDuration(s)
		if err != nil {
			return 0, usagef("--for %q is not a duration (e.g. 2h, 90m, 1d)", s)
		}
		d += rest
	}
	if d <= 0 {
		return 0, usagef("--for must be positive (e.g. 2h)")
	}
	return d, nil
}

// avoidClockFormats are the clock-time spellings --until accepts.
var avoidClockFormats = []string{"15:04", "3:04pm", "3pm", "3:04 pm", "3 pm"}

// avoidDateFormats are the date-and-time spellings --until accepts (local
// time unless the value names a zone).
var avoidDateFormats = []string{"2006-01-02 15:04", "2006-01-02T15:04", "2006-01-02T15:04:05"}

// parseAvoidUntil reads --until in now's location: a clock time (18:00,
// 6pm, 6:30pm; one not after now means tomorrow), a local date and time
// (2026-10-24 09:00), or RFC 3339.
func parseAvoidUntil(now time.Time, s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	loc := now.Location()
	for _, f := range avoidDateFormats {
		if t, err := time.ParseInLocation(f, s, loc); err == nil {
			return t, nil
		}
	}
	low := strings.ToLower(s)
	for _, f := range avoidClockFormats {
		t, err := time.ParseInLocation(f, low, loc)
		if err != nil {
			continue
		}
		at := time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), 0, 0, loc)
		if !at.After(now) {
			at = time.Date(now.Year(), now.Month(), now.Day()+1, t.Hour(), t.Minute(), 0, 0, loc) // tomorrow
		}
		return at, nil
	}
	return time.Time{}, usagef("--until %q is not a time (e.g. 18:00, 6pm, \"2026-10-24 09:00\")", s)
}

// avoidExpiry is when a flag set at now lifts: --until, else --for, else
// NodeAvoidDefaultTTL; never unbounded, at most NodeAvoidMaxTTL.
func avoidExpiry(now time.Time, forS, untilS string) (time.Time, error) {
	forS, untilS = strings.TrimSpace(forS), strings.TrimSpace(untilS)
	var at time.Time
	switch {
	case forS != "" && untilS != "":
		return time.Time{}, usagef("give --for or --until, not both")
	case untilS != "":
		t, err := parseAvoidUntil(now, untilS)
		if err != nil {
			return time.Time{}, err
		}
		at = t
	default:
		d := c.NodeAvoidDefaultTTL
		if forS != "" {
			var err error
			if d, err = parseAvoidFor(forS); err != nil {
				return time.Time{}, err
			}
		}
		at = now.Add(d)
	}
	if !at.After(now) {
		return time.Time{}, usagef("the avoid flag must end in the future (got %s)", at.Format(time.RFC3339))
	}
	if at.Sub(now) > c.NodeAvoidMaxTTL {
		return time.Time{}, usagef("an avoid flag lasts at most 7 days (it always lifts by itself); got %s", short(at.Sub(now)))
	}
	return at, nil
}

// ---- text ----

// avoidWhen is a flag's expiry in now's time zone: the clock alone today,
// the weekday within a week, else the date; plus how long from now.
func avoidWhen(t, now time.Time) string {
	lt := t.In(now.Location())
	y1, m1, d1 := lt.Date()
	y2, m2, d2 := now.Date()
	var s string
	switch {
	case y1 == y2 && m1 == m2 && d1 == d2:
		s = lt.Format("15:04 MST")
	case lt.Sub(now) < 6*24*time.Hour:
		s = lt.Format("Mon 15:04 MST")
	default:
		s = lt.Format("Mon Jan 2 15:04 MST")
	}
	if d := t.Sub(now); d > 0 {
		s += " (in " + short(d) + ")"
	}
	return s
}

// avoidSetBy says who set a flag.
func avoidSetBy(a c.NodeAvoid) string {
	s := "set by session " + a.SetBy.ID
	if a.OwnerSet {
		s = "set by the owner"
	}
	if a.UpdatedBy != nil {
		s += ", extended by session " + a.UpdatedBy.ID
	}
	return s
}

func avoidKind(a c.NodeAvoid) string {
	if a.Hard {
		return "hard"
	}
	return "soft"
}

// avoidBrief is a one-phrase description for placement notes: "soft until
// 18:00 PDT (in 2h): presenting".
func avoidBrief(a c.NodeAvoid, now time.Time) string {
	s := avoidKind(a) + " until " + avoidWhen(a.ExpiresAt, now)
	if a.Reason != "" {
		s += ": " + oneLineClip(a.Reason, 80)
	}
	return s
}

// avoidLine describes one flag in full for `pyre nodes` and avoid output.
func avoidLine(a c.NodeAvoid, now time.Time) string {
	s := avoidKind(a) + " until " + avoidWhen(a.ExpiresAt, now)
	if a.ProjectID != nil && *a.ProjectID != "" {
		s += ", project " + *a.ProjectID + " only"
	}
	s += ", " + avoidSetBy(a)
	if a.Reason != "" {
		s += " — " + oneLineClip(a.Reason, 120)
	}
	return s
}

// printAvoids lists the nodes' flags in force at now (after a node table).
func printAvoids(w io.Writer, ns []c.Node, now time.Time) {
	for _, n := range ns {
		for _, a := range n.Avoid {
			if a.Active(now) {
				fmt.Fprintf(w, "avoid %s: %s\n", placeName(n), avoidLine(a, now))
			}
		}
	}
}

// avoidRef is "now" for display: the hub's snapshot time when known.
func avoidRef(ref time.Time) time.Time {
	if ref.IsZero() {
		return time.Now()
	}
	return ref.In(time.Local)
}

// ---- MCP ----

func avoidMCPTools() []MCPTool {
	return []MCPTool{
		tool("avoid_node", "Avoid a machine",
			"Keep new work off a machine for a while (presenting, a benchmark): placement skips it until the flag lifts (default 2h, max 7 days); "+
				"hard refuses even a named node. Existing sessions keep running; a session can't weaken the owner's flag.",
			obj(props{"node": str("node ID (node_...) or name"), "for": str("how long: 2h, 90m, 1d (default 2h)"),
				"until":  str("until a local clock time on this machine (18:00, 6pm; earlier than now means tomorrow) or \"2026-10-24 09:00\" / RFC 3339; not with for"),
				"reason": str("why, shown to the owner and agents (e.g. presenting)"), "hard": boo("refuse starts there, even with an explicit node, unless ignore_avoid"),
				"clear": boo("remove the flag instead (with project: only that project's)"), "project": str("project ID (prj_...) or name: only placements for that project"),
				"operation_id": opIDProp}, "node"),
			hints("Avoid a machine", idempotent, true),
			func(ctx context.Context, o *Ops, p AvoidParams) (c.AvoidNodeResponse, error) {
				return o.AvoidNode(ctx, p)
			}),
	}
}

// ---- command ----

// addAvoidCommands adds `pyre node avoid`.
func (a *App) addAvoidCommands(nodes *cobra.Command) {
	var p AvoidParams
	cmd := &cobra.Command{
		Use:   "avoid [<node>] [--for 2h | --until 18:00] [--reason TEXT] [--hard] [--project P] | --clear <node>",
		Short: "Keep new work off a machine for a while (lifts by itself); no node: list the flags",
		Long: `Flag a machine so new work stays off it for a while: presenting, a timed
benchmark, heavy local work. The flag lifts by itself at its expiry:
--for (2h, 90m, 1d; default 2h) or --until (a local clock time such as 18:00
or 6pm, which means tomorrow when it has passed today, or "2026-10-24 09:00");
at most 7 days.

Soft (default): pyre start / pyre new / MCP start_session without --node skip
the machine unless nothing else fits, and an explicit --node starts there with
a warning. --hard: starts there are refused, even with --node, unless
--ignore-avoid (a flag the owner set yields only to the owner's credentials).
Sessions already on the machine keep running; agents see the flag in
pyre nodes, list_nodes and get_context.

The owner and sessions set flags. A session cannot clear or weaken a flag the
owner set; asking for a softer or shorter one keeps the owner's (a stronger or
longer one tightens it). --project limits the flag to that project's launches.
--clear removes the flag (with --project: only that project's). With no node,
lists the flags in force.`,
		Args: cobra.MaximumNArgs(1),
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			if len(args) == 0 {
				if p.Clear || p.Hard || p.For != "" || p.Until != "" || p.Reason != "" {
					return usagef("say which node (see `pyre nodes`)")
				}
				page, err := ops.ListNodes(cmd.Context(), false)
				if err != nil {
					return err
				}
				flagged := []c.Node{}
				for _, n := range page.Items {
					if len(n.Avoid) > 0 {
						flagged = append(flagged, n)
					}
				}
				sort.SliceStable(flagged, func(i, j int) bool { return flagged[i].Name < flagged[j].Name })
				return a.emit(flagged, func(w io.Writer) {
					if len(flagged) == 0 {
						fmt.Fprintln(w, "no machine is avoided")
						return
					}
					printAvoids(w, flagged, avoidRef(page.SnapshotAt))
				})
			}
			p.Node, p.OperationID = args[0], a.opID
			res, err := ops.AvoidNode(cmd.Context(), p)
			if err != nil {
				return err
			}
			if res.Note != "" {
				a.warnf("note: %s", res.Note)
			}
			return a.emit(res, func(w io.Writer) {
				name := res.NodeName
				if name == "" {
					name = res.NodeID
				}
				now := time.Now()
				switch {
				case p.Clear && res.Cleared == 0:
					fmt.Fprintf(w, "%s had no avoid flag to clear\n", name)
				case p.Clear:
					fmt.Fprintf(w, "cleared %d avoid flag(s) on %s\n", res.Cleared, name)
				}
				if len(res.Avoid) == 0 && p.Clear {
					return
				}
				for _, f := range res.Avoid {
					fmt.Fprintf(w, "avoid %s: %s\n", name, avoidLine(f, now))
				}
			})
		}),
	}
	f := cmd.Flags()
	f.StringVar(&p.For, "for", "", "how long: 2h, 90m, 1d (default 2h; at most 7 days)")
	f.StringVar(&p.Until, "until", "", "until a local time: 18:00, 6pm (tomorrow if passed), or \"2026-10-24 09:00\"")
	f.StringVar(&p.Reason, "reason", "", "why (shown to agents and in pyre nodes)")
	f.BoolVar(&p.Hard, "hard", false, "refuse starts there, even with --node, unless --ignore-avoid")
	f.BoolVar(&p.Clear, "clear", false, "remove the flag instead")
	f.StringVar(&p.Project, "project", "", "only launches for this project (ID or name)")
	nodes.AddCommand(cmd)
}
