package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/jhh3/pyre/internal/audit"
	c "github.com/jhh3/pyre/internal/contracts"
)

// AuditParams are audit_log's arguments (and the CLI flags).
type AuditParams struct {
	// Since: a duration back from now (24h, 90m, 7d) or an RFC 3339 time.
	// Default 24h; "all" means no bound.
	Since   string `json:"since,omitempty"`
	Kind    string `json:"kind,omitempty"`    // account | session | fleet
	Session string `json:"session,omitempty"` // ID or exact title
	Node    string `json:"node,omitempty"`    // ID or name
	Account string `json:"account,omitempty"` // ID or label
	Limit   int    `json:"limit,omitempty"`   // default 200, max 2000
}

// Audit lists the hub's audit log (newest first), resolving names to IDs.
func (o *Ops) Audit(ctx context.Context, p AuditParams, now time.Time) (c.Page[c.AuditEntry], error) {
	out := c.Page[c.AuditEntry]{Items: []c.AuditEntry{}}
	au, ok := o.API.(c.AuditAPI)
	if !ok {
		return out, c.Errorf(c.ErrUnsupported, "this connection does not offer %s", c.OpListAudit)
	}
	var req c.ListAuditRequest
	since := strings.TrimSpace(p.Since)
	if since == "" {
		since = "24h"
	}
	if !strings.EqualFold(since, "all") {
		t, err := audit.ParseSince(since, now)
		if err != nil {
			return out, usagef("%v", err)
		}
		req.Since = t
	}
	if p.Kind != "" {
		k, err := audit.ParseKind(p.Kind)
		if err != nil {
			return out, usagef("%v", err)
		}
		req.Kinds = []c.AuditKind{k}
	}
	if p.Session != "" {
		s, err := o.ResolveSession(ctx, p.Session)
		if err != nil {
			return out, err
		}
		req.SessionID = s.Session.SessionID
	}
	if p.Node != "" {
		n, err := o.ResolveNode(ctx, p.Node)
		if err != nil {
			return out, err
		}
		req.NodeID = n.NodeID
	}
	if p.Account != "" {
		id, err := o.resolveAccountRef(ctx, p.Account)
		if err != nil {
			return out, err
		}
		req.AccountID = id
	}
	limit := p.Limit
	if limit <= 0 {
		limit = 200
	}
	limit = min(limit, 2000)
	for len(out.Items) < limit {
		req.Limit = min(500, limit-len(out.Items))
		page, err := au.ListAudit(ctx, req)
		if err != nil {
			return out, err
		}
		out.Items = append(out.Items, page.Items...)
		out.SnapshotAt, out.NextCursor = page.SnapshotAt, page.NextCursor
		if page.NextCursor == "" || page.NextCursor == req.Cursor {
			break
		}
		req.Cursor = page.NextCursor
	}
	return out, nil
}

// resolveAccountRef accepts an account ID or an exact (then
// case-insensitive) label.
func (o *Ops) resolveAccountRef(ctx context.Context, ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if c.ValidID(ref, c.PrefixAccount) {
		return ref, nil
	}
	page, err := listAll(ctx, func(ctx context.Context, cur string) (c.Page[c.AccountView], error) {
		return o.API.ListAccounts(ctx, c.ListAccountsRequest{PageRequest: c.PageRequest{Cursor: cur}})
	})
	if err != nil {
		return "", err
	}
	for _, pass := range []func(a, b string) bool{func(a, b string) bool { return a == b }, strings.EqualFold} {
		var hits []string
		for _, a := range page.Items {
			if pass(a.Account.Label, ref) {
				hits = append(hits, a.Account.AccountID)
			}
		}
		switch len(hits) {
		case 1:
			return hits[0], nil
		case 0:
			continue
		default:
			return "", usagef("account %q is ambiguous (%s); use the ID", ref, strings.Join(hits, ", "))
		}
	}
	return "", usagef("no account named %q (see `pyre accounts`)", ref)
}

// writeAuditText prints entries oldest first (newest at the bottom, like a
// log), one plain-English line each.
func writeAuditText(w io.Writer, p c.Page[c.AuditEntry], since string, now time.Time) {
	if len(p.Items) == 0 {
		if strings.EqualFold(since, "all") {
			fmt.Fprintln(w, "no activity recorded")
		} else {
			if since == "" {
				since = "24h"
			}
			fmt.Fprintf(w, "no activity in the last %s (try --since 7d)\n", since)
		}
		return
	}
	for i := len(p.Items) - 1; i >= 0; i-- {
		e := p.Items[i]
		mark := "  "
		if e.Automatic {
			mark = "⚙ "
		}
		fmt.Fprintln(w, mark+audit.Line(e, now))
	}
	if p.NextCursor != "" {
		fmt.Fprintf(w, "(older entries not shown; raise --limit or narrow --since)\n")
	}
}

func (a *App) addAuditCommands(root *cobra.Command) {
	var p AuditParams
	cmd := &cobra.Command{
		Use:   "audit [--since 24h] [--kind account|session|fleet] [--session S] [--node N] [--account A]",
		Short: "Audit log: what Pyre did on its own and what changed in the fleet (account switches, starts/stops, upgrades)",
		Long: `Show the hub's audit log: who did what, why, where and when.

Account events: which account a run got and why (affinity, headroom),
automatic switches after a limit (from -> to, with usage), re-auth needed,
cooldowns, tokens added/replaced/removed/synced, folds, renames, disable.
Session events: started (by you or by another session), stopped,
interrupted, resumed/handoff, renamed, archived, auto-titled, input taken over.
Fleet events: machines enrolled, connected/disconnected, pyred and hub
upgrades, failed updates, and dialogs a machine answered by itself
(folder trust for owner-trusted dirs, Claude's exit dialog on stop).

Lines are oldest first; ⚙ marks what Pyre did on its own. Entries hold IDs,
names and short reasons only (never secrets or message text) and are kept
90 days by default (pyre-hub --audit-max-age / --audit-max-rows).`,
		Example: `  pyre audit
  pyre audit --kind account --since 7d
  pyre audit --session "Credit perf" --json`,
		Args: cobra.NoArgs,
		RunE: a.run(func(cmd *cobra.Command, _ []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			now := time.Now()
			page, err := ops.Audit(cmd.Context(), p, now)
			if err != nil {
				return err
			}
			return a.emit(page, func(w io.Writer) { writeAuditText(w, page, p.Since, now) })
		}),
	}
	f := cmd.Flags()
	f.StringVar(&p.Since, "since", "24h", "how far back: a duration (24h, 90m, 7d), an RFC 3339 time, or all")
	f.StringVar(&p.Kind, "kind", "", "only account, session or fleet events")
	f.StringVar(&p.Session, "session", "", "only this session (ID or exact title)")
	f.StringVar(&p.Node, "node", "", "only this machine (ID or name)")
	f.StringVar(&p.Account, "account", "", "only this account (ID or label)")
	f.IntVar(&p.Limit, "limit", 200, "at most this many entries (max 2000)")
	root.AddCommand(cmd)
}
