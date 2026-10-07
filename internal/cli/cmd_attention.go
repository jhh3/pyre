package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	c "github.com/jhh3/pyre/internal/contracts"
)

func (a *App) addApprovalCommands(root *cobra.Command) {
	var af ApprovalFilter
	ls := func(cmd *cobra.Command, args []string) error {
		ops, err := a.connect()
		if err != nil {
			return err
		}
		page, err := ops.ListApprovals(cmd.Context(), af)
		if err != nil {
			return err
		}
		return a.emit(page, func(w io.Writer) { printApprovals(w, page.Items) })
	}
	approvals := &cobra.Command{
		Use:     "approvals [ls|show <id>]",
		Aliases: []string{"approval"},
		Short:   "List pending approvals and questions from runtimes",
		Args:    cobra.NoArgs,
		RunE:    a.run(ls),
	}
	approvals.PersistentFlags().StringVar(&af.Session, "session", "", "only this session's approvals")
	approvals.PersistentFlags().BoolVar(&af.All, "all", false, "include resolved, expired and withdrawn")
	approvals.AddCommand(&cobra.Command{Use: "ls", Short: "List approvals", Args: cobra.NoArgs, RunE: a.run(ls)})
	approvals.AddCommand(&cobra.Command{
		Use:   "show <approval_id>",
		Short: "Show an approval and its options",
		Args:  cobra.ExactArgs(1),
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			ap, err := ops.API.GetApproval(cmd.Context(), c.GetRequest{ID: args[0]})
			if err != nil {
				return err
			}
			return a.emit(ap, func(w io.Writer) { printApproval(w, ap) })
		}),
	})
	root.AddCommand(approvals)

	var pp ApproveParams
	approve := &cobra.Command{
		Use:   "approve <approval_id> [--option ID] [--text TEXT]",
		Short: "Answer an approval or question (bound to its own run; stale requests are refused)",
		Args:  cobra.ExactArgs(1),
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			p := pp
			p.ApprovalID, p.OperationID = args[0], a.opID
			ap, err := ops.Approve(cmd.Context(), p)
			if err != nil {
				return err
			}
			return a.emit(ap, func(w io.Writer) { printApproval(w, ap) })
		}),
	}
	approve.Flags().StringVar(&pp.Option, "option", "", "`ID` of the option to choose (listed by pyre approvals show)")
	approve.Flags().StringVar(&pp.Text, "text", "", "free-text answer, where allowed")
	root.AddCommand(approve)
}

func printApprovals(w io.Writer, as []c.Approval) {
	var rows [][]string
	for _, ap := range as {
		opts := make([]string, len(ap.Options))
		for i, o := range ap.Options {
			opts[i] = o.ID
		}
		opt := strings.Join(opts, "/")
		if ap.State == c.ApprovalExpired {
			opt = "-"
		}
		rows = append(rows, []string{ap.ApprovalID, string(ap.Kind), string(ap.State), ap.SessionID, ago(ap.CreatedAt), clip(ap.Title, 40), opt})
	}
	table(w, "ID\tKIND\tSTATE\tSESSION\tAGE\tTITLE\tOPTIONS", rows)
	for _, ap := range as {
		if ap.State == c.ApprovalExpired {
			fmt.Fprintf(w, "%s: the remote answer window closed; the prompt is waiting in its terminal. Answer it there (pyre attach %s) or cancel it (pyre interrupt %s).\n",
				ap.ApprovalID, ap.SessionID, ap.SessionID)
		}
	}
}

func printApproval(w io.Writer, ap c.Approval) {
	fmt.Fprintf(w, "%s  %s  %s\n", ap.ApprovalID, ap.Kind, ap.State)
	fmt.Fprintf(w, "  session %s run %s, created %s", ap.SessionID, ap.RunID, ago(ap.CreatedAt))
	if ap.ExpiresAt != nil {
		fmt.Fprintf(w, ", expires %s", agoPtr(ap.ExpiresAt))
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  %s\n", ap.Title)
	if ap.Body != "" {
		for _, l := range strings.Split(strings.TrimRight(ap.Body, "\n"), "\n") {
			fmt.Fprintf(w, "    %s\n", l)
		}
	}
	for _, o := range ap.Options {
		fmt.Fprintf(w, "  option %-14s %s %s\n", o.ID, o.Label, refSuffix(o.Kind))
	}
	if ap.AllowText {
		fmt.Fprintln(w, "  free text allowed (--text)")
	}
	if r := ap.Resolution; r != nil {
		fmt.Fprintf(w, "  resolved by %s via %s %s: option %s text %q, effect %s\n", principalText(r.Actor), r.Source, ago(r.ResolvedAt), orDash(r.OptionID), r.Text, orDash(r.Effect))
		if r.Effect == "submitted" {
			fmt.Fprintln(w, "  (answer submitted; the runtime has not yet confirmed it took effect)")
		}
	}
}

func (a *App) addAttentionCommands(root *cobra.Command) {
	var all bool
	var kinds []string
	var session, project string
	ls := func(cmd *cobra.Command, args []string) error {
		ops, err := a.connect()
		if err != nil {
			return err
		}
		req := c.ListAttentionRequest{}
		if all {
			req.States = []c.AttentionState{c.AttnOpen, c.AttnRead, c.AttnHandled, c.AttnWithdrawn, c.AttnSnoozed}
		}
		for _, k := range kinds {
			req.Kinds = append(req.Kinds, c.AttentionKind(k))
		}
		if session != "" {
			if req.SessionID, err = ops.sessionID(cmd.Context(), session); err != nil {
				return err
			}
		}
		if req.ProjectID, err = ops.projectPtr(cmd.Context(), project); err != nil {
			return err
		}
		page, err := listAll(cmd.Context(), func(ctx context.Context, cur string) (c.Page[c.AttentionItem], error) {
			r := req
			r.Cursor = cur
			return ops.API.ListAttention(ctx, r)
		})
		if err != nil {
			return err
		}
		return a.emit(page, func(w io.Writer) { printAttention(w, page.Items) })
	}
	attn := &cobra.Command{
		Use:     "attention [ls|show <id>|read <id>|handle <id>|snooze <id>]",
		Aliases: []string{"attn"},
		Short:   "The durable inbox of things that need you (approvals, failures, reviews, ...)",
		Args:    cobra.NoArgs,
		RunE:    a.run(ls),
	}
	pf := attn.PersistentFlags()
	pf.BoolVar(&all, "all", false, "include handled, withdrawn and snoozed items")
	pf.StringSliceVar(&kinds, "kind", nil, "approval, question, failure, account_exhausted, review_ready, message, indeterminate_delivery, ...")
	pf.StringVar(&session, "session", "", "only this session")
	pf.StringVar(&project, "project", "", "only this project")
	attn.AddCommand(&cobra.Command{Use: "ls", Short: "List attention items", Args: cobra.NoArgs, RunE: a.run(ls)})
	attn.AddCommand(&cobra.Command{
		Use:   "show <attention_id>",
		Short: "Show one attention item in any state",
		Args:  cobra.ExactArgs(1),
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			it, err := ops.API.GetAttention(cmd.Context(), c.GetRequest{ID: args[0]})
			if err != nil {
				return err
			}
			return a.emit(it, func(w io.Writer) {
				fmt.Fprintf(w, "%s  %s  %s  %s\n", it.AttentionID, it.Kind, it.Severity, it.State)
				fmt.Fprintf(w, "  %s\n", it.Title)
				if it.Summary != "" {
					fmt.Fprintf(w, "  %s\n", it.Summary)
				}
				fmt.Fprintf(w, "  object %s %s  %s\n", it.Object.Kind, it.Object.ID, it.DeepLink)
				if it.SessionID != "" {
					fmt.Fprintf(w, "  session %s\n", it.SessionID)
				}
				fmt.Fprintf(w, "  count %d, created %s, updated %s\n", it.Count, stamp(it.CreatedAt), ago(it.UpdatedAt))
			})
		}),
	})
	var snoozeFor time.Duration
	for _, st := range []struct {
		name  string
		state c.AttentionState
		short string
	}{{"read", c.AttnRead, "Mark an item read"}, {"handle", c.AttnHandled, "Mark an item handled"}, {"snooze", c.AttnSnoozed, "Snooze an item (--for)"}} {
		st := st
		sub := &cobra.Command{
			Use:   st.name + " <attention_id>",
			Short: st.short,
			Args:  cobra.ExactArgs(1),
			RunE: a.run(func(cmd *cobra.Command, args []string) error {
				ops, err := a.connect()
				if err != nil {
					return err
				}
				req := c.UpdateAttentionRequest{Mutation: mut(a.opID), AttentionID: args[0], State: st.state}
				if st.state == c.AttnSnoozed {
					until := time.Now().Add(snoozeFor).UTC()
					req.SnoozedUntil = &until
				}
				it, err := ops.API.UpdateAttention(cmd.Context(), req)
				if err != nil {
					return err
				}
				return a.emit(it, func(w io.Writer) { fmt.Fprintf(w, "%s: %s\n", it.AttentionID, it.State) })
			}),
		}
		if st.state == c.AttnSnoozed {
			sub.Flags().DurationVar(&snoozeFor, "for", time.Hour, "snooze duration")
		}
		attn.AddCommand(sub)
	}
	attn.AddCommand(a.flagCommand())
	root.AddCommand(attn)

	var wAfter int64
	var wKinds []string
	var wSession string
	var noNotify bool
	watch := &cobra.Command{
		Use:   "watch [--after CURSOR] [--kind K] [--no-notify]",
		Short: "Stream fleet events; desktop-notify new attention items",
		Long: `Stream durable events as they happen (one line per event, or one JSON
object per line with --json). New attention items raise a desktop
notification unless --no-notify. If the cursor falls out of retention the
inbox is re-listed and the stream resumes from the hub's snapshot cursor.`,
		Args: cobra.NoArgs,
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			req := c.ListEventsRequest{AfterCursor: wAfter}
			for _, k := range wKinds {
				req.Kinds = append(req.Kinds, c.EventKind(k))
			}
			if wSession != "" {
				if req.SessionID, err = ops.sessionID(cmd.Context(), wSession); err != nil {
					return err
				}
			}
			return a.watch(cmd.Context(), ops, req, wAfter > 0, !noNotify)
		}),
	}
	watch.Flags().Int64Var(&wAfter, "after", 0, "resume after this event cursor (default: now)")
	watch.Flags().StringSliceVar(&wKinds, "kind", nil, "only these event kinds (attention.changed is always followed)")
	watch.Flags().StringVar(&wSession, "session", "", "only events for this session")
	watch.Flags().BoolVar(&noNotify, "no-notify", false, "do not show desktop notifications")
	root.AddCommand(watch)
}

func printAttention(w io.Writer, items []c.AttentionItem) {
	var rows [][]string
	for _, it := range items {
		count := ""
		if it.Count > 1 {
			count = fmt.Sprintf("x%d", it.Count)
		}
		rows = append(rows, []string{it.AttentionID, it.Severity, string(it.Kind), string(it.State), ago(it.UpdatedAt), count, clip(it.Title, 50), it.DeepLink})
	}
	table(w, "ID\tSEVERITY\tKIND\tSTATE\tUPDATED\tCOUNT\tTITLE\tLINK", rows)
}

type eventWatcher interface {
	WatchEventsFiltered(ctx context.Context, req c.ListEventsRequest, fn func(c.Event) error) error
}

// watch streams events, notifying for newly opened attention items.
func (a *App) watch(ctx context.Context, ops *Ops, req c.ListEventsRequest, explicitCursor bool, notify bool) error {
	seen := map[string]int{}
	snapshot := func(announce bool) error {
		page, err := listAll(ctx, func(ctx context.Context, cur string) (c.Page[c.AttentionItem], error) {
			return ops.API.ListAttention(ctx, c.ListAttentionRequest{PageRequest: c.PageRequest{Cursor: cur}, States: []c.AttentionState{c.AttnOpen}})
		})
		if err != nil {
			return err
		}
		for _, it := range page.Items {
			if announce && notify && it.Count > seen[it.AttentionID] {
				a.notify(it)
			}
			seen[it.AttentionID] = it.Count
		}
		return nil
	}
	if err := snapshot(false); err != nil {
		return err
	}
	if !explicitCursor {
		head, err := a.headCursor(ctx, ops)
		if err != nil {
			return err
		}
		req.AfterCursor = head
	}
	if len(req.Kinds) > 0 {
		req.Kinds = append(req.Kinds, c.EvAttentionChanged)
	}
	handle := func(ev c.Event) error {
		req.AfterCursor = ev.Cursor
		if ev.Kind == c.EvAttentionChanged {
			var it c.AttentionItem
			if json.Unmarshal(ev.Payload, &it) == nil && it.AttentionID != "" {
				if it.State == c.AttnOpen && it.Count > seen[it.AttentionID] && notify {
					a.notify(it)
				}
				if it.State == c.AttnOpen {
					seen[it.AttentionID] = it.Count
				} else {
					delete(seen, it.AttentionID)
				}
			}
		}
		return a.emitLine(ev, func(w io.Writer) {
			fmt.Fprintf(w, "%s #%d %-20s %s %s", ev.OccurredAt.Local().Format("15:04:05"), ev.Cursor, ev.Kind, ev.Object.Kind, ev.Object.ID)
			if ev.SessionID != "" && ev.SessionID != ev.Object.ID {
				fmt.Fprintf(w, " session %s", ev.SessionID)
			}
			fmt.Fprintln(w)
		})
	}
	for {
		var err error
		if wt, ok := ops.API.(eventWatcher); ok {
			err = wt.WatchEventsFiltered(ctx, req, handle)
		} else {
			err = a.pollEvents(ctx, ops, &req, handle)
		}
		if ctx.Err() != nil {
			return nil
		}
		if !c.IsCode(err, c.ErrResetRequired) {
			return err
		}
		a.warnf("pyre: event cursor %d is older than retention; re-listing the inbox", req.AfterCursor)
		next := snapshotCursor(err)
		if err := snapshot(true); err != nil {
			return err
		}
		if next <= 0 {
			if next, err = a.headCursor(ctx, ops); err != nil {
				return err
			}
		}
		req.AfterCursor = next
	}
}

func (a *App) notify(it c.AttentionItem) {
	title := "Pyre: " + strings.ReplaceAll(string(it.Kind), "_", " ")
	if err := a.Notify(title, it.Title); err != nil {
		a.warnf("pyre: desktop notification unavailable: %v", err)
	}
}

// headCursor returns the newest event cursor.
func (a *App) headCursor(ctx context.Context, ops *Ops) (int64, error) {
	resp, err := ops.API.ListEvents(ctx, c.ListEventsRequest{AfterCursor: 1<<62 - 1, Limit: 1})
	if err == nil {
		return resp.HeadCursor, nil
	}
	if c.IsCode(err, c.ErrResetRequired) {
		if n := snapshotCursor(err); n > 0 {
			return n, nil
		}
	}
	resp, err2 := ops.API.ListEvents(ctx, c.ListEventsRequest{AfterCursor: 0, Limit: 1})
	if err2 != nil {
		if n := snapshotCursor(err2); n > 0 {
			return n, nil
		}
		return 0, err
	}
	return resp.HeadCursor, nil
}

func snapshotCursor(err error) int64 {
	var ce *c.Error
	if !errors.As(err, &ce) || ce.Details == nil {
		return 0
	}
	switch v := ce.Details["snapshot_cursor"].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	case json.Number:
		n, _ := v.Int64()
		return n
	}
	return 0
}

// pollEvents is the long-poll fallback for APIs without a stream.
func (a *App) pollEvents(ctx context.Context, ops *Ops, req *c.ListEventsRequest, fn func(c.Event) error) error {
	for {
		r := *req
		r.WaitSeconds = 25
		resp, err := ops.API.ListEvents(ctx, r)
		if err != nil {
			if c.IsCode(err, c.ErrUnavailable) && ctx.Err() == nil {
				time.Sleep(a.PollInterval)
				continue
			}
			return err
		}
		for _, ev := range resp.Events {
			if ev.Cursor <= req.AfterCursor {
				continue
			}
			if err := fn(ev); err != nil {
				return err
			}
		}
		if resp.NextCursor > req.AfterCursor {
			req.AfterCursor = resp.NextCursor
		}
	}
}
