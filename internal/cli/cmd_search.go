package cli

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	c "github.com/jhh3/pyre/internal/contracts"
)

// SearchParams is `pyre search` and the MCP search tool's input.
type SearchParams struct {
	Query    string   `json:"query"`
	Node     string   `json:"node,omitempty"`
	Since    string   `json:"since,omitempty"` // 7d, 36h, 90m, or a date (2026-10-01)
	Limit    int      `json:"limit,omitempty"`
	Archived bool     `json:"archived,omitempty"`
	Sources  []string `json:"sources,omitempty"`
}

// Search runs the hub's fan-out search over every node.
func (o *Ops) Search(ctx context.Context, p SearchParams) (c.SearchResponse, error) {
	if strings.TrimSpace(p.Query) == "" {
		return c.SearchResponse{}, usagef("a search query is required")
	}
	s, ok := o.API.(c.SearchAPI)
	if !ok {
		return c.SearchResponse{}, c.Errorf(c.ErrUnsupported, "this connection does not offer search")
	}
	req := c.SearchRequest{Query: p.Query, Limit: p.Limit, IncludeArchived: p.Archived, Sources: p.Sources}
	if p.Node != "" {
		id, err := o.NodeID(ctx, p.Node)
		if err != nil {
			return c.SearchResponse{}, err
		}
		req.NodeID = id
	}
	if p.Since != "" {
		t, err := parseSince(p.Since, time.Now())
		if err != nil {
			return c.SearchResponse{}, err
		}
		req.Since = &t
	}
	resp, err := s.Search(ctx, req)
	if resp.Hits == nil {
		resp.Hits = []c.SearchHit{}
	}
	if resp.Nodes == nil {
		resp.Nodes = []c.SearchNodeStatus{}
	}
	return resp, err
}

var sinceRe = regexp.MustCompile(`^(\d+)\s*(m|h|d|w)$`)

// parseSince accepts 90m, 36h, 7d, 2w or a date/RFC 3339 time.
func parseSince(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if m := sinceRe.FindStringSubmatch(s); m != nil {
		n, _ := strconv.Atoi(m[1])
		unit := map[string]time.Duration{"m": time.Minute, "h": time.Hour, "d": 24 * time.Hour, "w": 7 * 24 * time.Hour}[m[2]]
		return now.Add(-time.Duration(n) * unit).UTC(), nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, usagef("--since: use a duration like 7d, 36h, 90m or a date like 2026-10-01")
}

func (a *App) addSearchCommands(root *cobra.Command) {
	var sp SearchParams
	cmd := &cobra.Command{
		Use:   `search "query" [--node N] [--since 7d] [--archived]`,
		Short: "Search every session transcript on every machine, plus memory, notes and docs",
		Long: `Search every Pyre session transcript on every machine (live and ended;
archived ones with --archived), plus Hive memory where a machine has it and
the hub's notes, documents and artifacts. Machines that are offline are
listed as not searched. Each transcript hit names the session and the turn;
open it with the printed pyre read command, or in the TUI (space /).`,
		Args: cobra.MinimumNArgs(1),
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			p := sp
			p.Query = strings.Join(args, " ")
			resp, err := ops.Search(cmd.Context(), p)
			if err != nil {
				return err
			}
			color := a.IsTerminal() && a.Getenv("NO_COLOR") == ""
			return a.emit(resp, func(w io.Writer) { printSearch(w, resp, color) })
		}),
	}
	f := cmd.Flags()
	f.StringVar(&sp.Node, "node", "", "search one machine only")
	f.StringVar(&sp.Since, "since", "", "only matches newer than this (7d, 36h, 90m, 2026-10-01)")
	f.IntVar(&sp.Limit, "limit", 0, "max results (default 40)")
	f.BoolVar(&sp.Archived, "archived", false, "include archived sessions")
	f.StringSliceVar(&sp.Sources, "source", nil, "limit to sources: transcript, memory, note, document, artifact")
	root.AddCommand(cmd)
}

// printSearch groups transcript hits by session (in rank order of each
// session's best hit), then other sources, then the machines not searched.
func printSearch(w io.Writer, resp c.SearchResponse, color bool) {
	mark := func(s string) string {
		if color {
			return strings.NewReplacer(c.SearchMarkOpen, "\x1b[1;33m", c.SearchMarkClose, "\x1b[0m").Replace(s)
		}
		return s
	}
	dim := func(s string) string {
		if color {
			return "\x1b[2m" + s + "\x1b[0m"
		}
		return s
	}
	sessions := map[string]int{}
	type group struct {
		first c.SearchHit
		hits  []c.SearchHit
	}
	var groups []*group
	var others []c.SearchHit
	for _, h := range resp.Hits {
		if h.Source != c.SearchSourceTranscript {
			others = append(others, h)
			continue
		}
		i, ok := sessions[h.SessionID]
		if !ok {
			i = len(groups)
			sessions[h.SessionID] = i
			groups = append(groups, &group{first: h})
		}
		groups[i].hits = append(groups[i].hits, h)
	}
	n := len(resp.Hits)
	summary := fmt.Sprintf("%d %s", n, pluralWord(n, "match", "matches"))
	if len(groups) > 0 {
		summary += fmt.Sprintf(" in %d %s", len(groups), pluralWord(len(groups), "session", "sessions"))
	}
	fmt.Fprintf(w, "%s %s\n", resp.Query, dim("— "+summary+fmt.Sprintf(" · %d ms", resp.TookMS)))
	for _, g := range groups {
		h := g.first
		node := nodeName(resp, h.NodeID)
		head := h.Title
		if head == "" {
			head = h.SessionID
		}
		if h.Archived {
			head += " (archived)"
		}
		fmt.Fprintf(w, "\n%s  %s\n", head, dim(strings.TrimSuffix(h.Provenance, " transcript")+" on "+node+" · "+ago(h.At)+" · "+h.SessionID))
		for _, x := range g.hits {
			fmt.Fprintf(w, "  %-9s %s  %s\n", roleWord(x.Kind), mark(clip(x.Snippet, 160)), dim(fmt.Sprintf("#%d", x.Seq)))
		}
		x := g.hits[0]
		fmt.Fprintf(w, "  %s\n", dim(fmt.Sprintf("pyre read %s --run %s --cursor %d", x.SessionID, x.RunID, max(0, x.Seq-1))))
	}
	if len(others) > 0 {
		fmt.Fprintln(w)
		for _, h := range others {
			fmt.Fprintf(w, "%-9s %s  %s\n  %s\n", h.Source, h.Title, dim(orDash(h.Provenance)+" · "+ago(h.At)), mark(clip(h.Snippet, 160)))
		}
	}
	if n == 0 {
		fmt.Fprintln(w, "\nno matches")
	}
	var notes []string
	for _, ns := range resp.Nodes {
		switch ns.State {
		case c.SearchNodeSearched:
			if ns.Partial {
				notes = append(notes, ns.Name+" (partial: still indexing)")
			}
		case c.SearchNodeSkipped:
		default:
			notes = append(notes, ns.Name+" ("+orDash(ns.Detail)+")")
		}
	}
	if len(notes) > 0 {
		fmt.Fprintf(w, "\n%s %s\n", dim("not fully searched:"), strings.Join(notes, ", "))
	}
	if resp.Truncated {
		fmt.Fprintln(w, dim("more matches exist; narrow the query or raise --limit"))
	}
}

func nodeName(resp c.SearchResponse, id string) string {
	for _, n := range resp.Nodes {
		if n.NodeID == id && n.Name != "" {
			return n.Name
		}
	}
	return orDash(id)
}

func roleWord(kind string) string {
	switch kind {
	case "user":
		return "you"
	case "assistant":
		return "agent"
	case "message":
		return "message"
	case "tool":
		return "tool"
	}
	return kind
}

func pluralWord(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
