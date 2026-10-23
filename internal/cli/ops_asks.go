package cli

// Asks (W-asks, I30): a top-level session's structured question to the
// owner — topic key, context, 2–4 options, a default and a deadline. The
// hub keeps one open ask per topic (a repeat merges), answers a decided
// topic with its decision, and redirects a child session's ask to its
// parent. Agents call find_asks before asking; ask_owner/find_asks/
// withdraw_ask are pre-approved. The owner lists and answers asks with
// `pyre ask`. The ops are optional (contracts.AsksAPI): a connection
// without them answers ErrUnsupported.

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	c "github.com/jhh3/pyre/internal/contracts"
)

func init() { mcpToolProviders = append(mcpToolProviders, askMCPTools) }

// askDecidedLookback is how far back find_asks (and `pyre ask find`)
// include decided asks by default.
const askDecidedLookback = 7 * 24 * time.Hour

func (o *Ops) asks() (c.AsksAPI, error) {
	if a, ok := o.API.(c.AsksAPI); ok {
		return a, nil
	}
	return nil, c.Errorf(c.ErrUnsupported, "this connection does not offer asks (%s); upgrade pyre-hub, or message the owner with send_message", c.OpAsksCreate)
}

// AskParams are ask_owner's arguments (and `pyre ask new`'s flags).
type AskParams struct {
	TopicKey        string        `json:"topic_key"`
	Title           string        `json:"title"`
	Context         string        `json:"context"`
	Options         []c.AskOption `json:"options"`
	Default         string        `json:"default,omitempty"`
	DeadlineMinutes int           `json:"deadline_minutes,omitempty"`
	Urgent          bool          `json:"urgent,omitempty"`
	Reopen          bool          `json:"reopen,omitempty"`
	ConfirmNew      bool          `json:"confirm_new,omitempty"`
	Project         string        `json:"project,omitempty"`
	// Action (W-grants) names the go-ahead asked for; a standing grant for
	// exactly it may answer the ask at once (it must offer the grant's
	// option, have a default and not be irreversible).
	Action string `json:"action,omitempty"`
	// Irreversible: never answered by a standing grant.
	Irreversible bool `json:"irreversible,omitempty"`
	// Items (W-grants) makes a batched ask: the options apply to each.
	Items       []c.AskItem `json:"items,omitempty"`
	OperationID string      `json:"operation_id,omitempty"`
}

// Ask creates (or merges into) an ask. Options without an id get a, b, c,
// d; the default may name an option by id or label.
func (o *Ops) Ask(ctx context.Context, p AskParams) (c.CreateAskResponse, error) {
	api, err := o.asks()
	if err != nil {
		return c.CreateAskResponse{}, err
	}
	req := c.CreateAskRequest{Mutation: mut(p.OperationID), TopicKey: strings.TrimSpace(p.TopicKey), Title: strings.TrimSpace(p.Title),
		Context: strings.TrimSpace(p.Context), Urgent: p.Urgent, Reopen: p.Reopen, ConfirmNew: p.ConfirmNew, Action: strings.TrimSpace(p.Action),
		Irreversible: p.Irreversible}
	switch {
	case req.TopicKey == "":
		return c.CreateAskResponse{}, usagef("topic_key is required: a short stable slug for the decision (e.g. \"db-migration-strategy\"); a later ask on the same topic merges into this one")
	case req.Title == "":
		return c.CreateAskResponse{}, usagef("title is required: the question in one line")
	case req.Context == "":
		return c.CreateAskResponse{}, usagef("context is required: what this is about, why it matters, what you tried, and what happens with each option, for a reader who is not in the weeds")
	case len(p.Options) < c.AskMinOptions || len(p.Options) > c.AskMaxOptions:
		return c.CreateAskResponse{}, usagef("give %d–%d options (got %d)", c.AskMinOptions, c.AskMaxOptions, len(p.Options))
	case p.DeadlineMinutes < 0:
		return c.CreateAskResponse{}, usagef("deadline_minutes must be positive")
	case len(p.Items) > c.AskMaxItems:
		return c.CreateAskResponse{}, usagef("a batched ask has at most %d items (got %d); split it", c.AskMaxItems, len(p.Items))
	}
	seenItem := map[string]bool{}
	for _, it := range p.Items {
		it.ID, it.Label, it.Detail, it.URL = strings.TrimSpace(it.ID), strings.TrimSpace(it.Label), strings.TrimSpace(it.Detail), strings.TrimSpace(it.URL)
		switch {
		case it.ID == "":
			return c.CreateAskResponse{}, usagef("every item needs a short id (e.g. \"pr-12\")")
		case it.Label == "":
			return c.CreateAskResponse{}, usagef("item %s needs a label", it.ID)
		case seenItem[strings.ToLower(it.ID)]:
			return c.CreateAskResponse{}, usagef("item id %q is repeated", it.ID)
		}
		seenItem[strings.ToLower(it.ID)] = true
		req.Items = append(req.Items, it)
	}
	seen := map[string]bool{}
	for i, op := range p.Options {
		op.ID, op.Label, op.Detail = strings.TrimSpace(op.ID), strings.TrimSpace(op.Label), strings.TrimSpace(op.Detail)
		if op.ID == "" {
			op.ID = string(rune('a' + i))
		}
		if op.Label == "" {
			return c.CreateAskResponse{}, usagef("option %s needs a label", op.ID)
		}
		if seen[op.ID] {
			return c.CreateAskResponse{}, usagef("option id %q is repeated", op.ID)
		}
		seen[op.ID] = true
		req.Options = append(req.Options, op)
	}
	if d := strings.TrimSpace(p.Default); d != "" {
		for _, op := range req.Options {
			if op.ID == d || (req.DefaultOption == "" && strings.EqualFold(op.Label, d)) {
				req.DefaultOption = op.ID
			}
		}
		if req.DefaultOption == "" {
			return c.CreateAskResponse{}, usagef("default %q is not one of the options", d)
		}
	}
	req.DeadlineSeconds = p.DeadlineMinutes * 60
	if req.ProjectID, err = o.projectPtr(ctx, p.Project); err != nil {
		return c.CreateAskResponse{}, err
	}
	return api.CreateAsk(ctx, req)
}

// FindAsksParams are find_asks' arguments.
type FindAsksParams struct {
	TopicKey string `json:"topic_key,omitempty"`
	Query    string `json:"query,omitempty"`
	// IncludeDecided (default true) adds asks decided in the last 7 days.
	IncludeDecided *bool  `json:"include_decided,omitempty"`
	Project        string `json:"project,omitempty"`
	Limit          int    `json:"limit,omitempty"`
}

// FindAsks lists open asks and, by default, those decided in the last
// seven days, matching a topic key or text.
func (o *Ops) FindAsks(ctx context.Context, p FindAsksParams, now time.Time) (c.Page[c.Ask], error) {
	req := c.ListAsksRequest{TopicKey: strings.TrimSpace(p.TopicKey), Query: strings.TrimSpace(p.Query)}
	if p.IncludeDecided != nil && !*p.IncludeDecided {
		req.States = []c.AskState{c.AskOpen}
	} else {
		since := now.Add(-askDecidedLookback).UTC()
		req.Since = &since
	}
	var err error
	if req.ProjectID, err = o.projectPtr(ctx, p.Project); err != nil {
		return c.Page[c.Ask]{}, err
	}
	return o.listAsks(ctx, req, p.Limit)
}

func (o *Ops) listAsks(ctx context.Context, req c.ListAsksRequest, limit int) (c.Page[c.Ask], error) {
	api, err := o.asks()
	if err != nil {
		return c.Page[c.Ask]{}, err
	}
	if limit <= 0 {
		limit = 50
	}
	out := c.Page[c.Ask]{Items: []c.Ask{}}
	for len(out.Items) < limit {
		req.Limit = limit - len(out.Items)
		page, err := api.ListAsks(ctx, req)
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

// WithdrawAskParams are withdraw_ask's arguments.
type WithdrawAskParams struct {
	AskID        string `json:"ask_id"`
	Reason       string `json:"reason,omitempty"`
	SupersededBy string `json:"superseded_by,omitempty"`
	OperationID  string `json:"operation_id,omitempty"`
}

// WithdrawAsk takes an open ask back (or marks it superseded).
func (o *Ops) WithdrawAsk(ctx context.Context, p WithdrawAskParams) (c.Ask, error) {
	api, err := o.asks()
	if err != nil {
		return c.Ask{}, err
	}
	if !c.ValidID(strings.TrimSpace(p.AskID), c.PrefixAsk) {
		return c.Ask{}, usagef("ask_id must be an ask ID (ask_...); find_asks lists them")
	}
	return api.WithdrawAsk(ctx, c.WithdrawAskRequest{Mutation: mut(p.OperationID), AskID: strings.TrimSpace(p.AskID),
		Reason: strings.TrimSpace(p.Reason), SupersededBy: strings.TrimSpace(p.SupersededBy)})
}

// DismissAsk is the owner archiving an open ask: withdrawn with
// contracts.AskDismissReason (no option, the default never applies), and
// the asking session is told.
func (o *Ops) DismissAsk(ctx context.Context, askID, opID string) (c.Ask, error) {
	return o.WithdrawAsk(ctx, WithdrawAskParams{AskID: askID, Reason: c.AskDismissReason, OperationID: opID})
}

// AnswerAsks answers one or more asks in one call (owner only).
func (o *Ops) AnswerAsks(ctx context.Context, items []c.AnswerAskItem, opID string) (c.AnswerAsksResponse, error) {
	api, err := o.asks()
	if err != nil {
		return c.AnswerAsksResponse{}, err
	}
	if len(items) == 0 {
		return c.AnswerAsksResponse{}, usagef("name at least one ask and its answer")
	}
	return api.AnswerAsks(ctx, c.AnswerAsksRequest{Mutation: mut(opID), Answers: items})
}

// ---- MCP ----

func askMCPTools() []MCPTool {
	option := map[string]any{"type": "object", "additionalProperties": false, "required": []string{"label", "detail"},
		"properties": map[string]any{
			"id":     str("short stable id (default a, b, c, d)"),
			"label":  str("the choice, a few words"),
			"detail": str("required, one line: what happens if chosen"),
		}}
	item := map[string]any{"type": "object", "additionalProperties": false, "required": []string{"id", "label"},
		"properties": map[string]any{
			"id":     str("short stable id, e.g. \"pr-12\""),
			"label":  str("what the item is, a few words"),
			"detail": str("one line: what and why, its state"),
			"url":    str("link to it (PR, doc)"),
		}}
	topic := str("short stable slug for the decision, e.g. \"auth-token-storage\"; the same key merges into the open ask instead of new mail, and a decided topic returns its answer")
	return []MCPTool{
		compactly(tool("ask_owner", "Ask the owner",
			"Ask the owner to decide (top-level sessions only). find_asks first. Plenty of context (the owner decides from it alone), 2–4 options each with a detail, a default you will take (none if irreversible); keep working. Same topic_key merges.",
			obj(props{"topic_key": topic, "title": str("the question in one line"),
				"context": str(fmt.Sprintf("markdown, at least %d characters: what is being decided and why now; what is known or was tried (links, paths); what each option leads to; what happens by default if nobody answers",
					c.AskMinContext)),
				"options":          map[string]any{"type": "array", "minItems": c.AskMinOptions, "maxItems": c.AskMaxOptions, "items": option, "description": "2–4 options"},
				"default":          str("option id (or label) applied at the deadline if the owner does not answer; omit for irreversible actions"),
				"deadline_minutes": num("default 60; a deadline at night moves to the owner's morning unless urgent"),
				"urgent":           boo("needs an answer even at night (keeps the deadline)"),
				"reopen":           boo("ask again on a decided topic because something changed (say what in context)"),
				"confirm_new":      boo("create it even though a similar open ask exists under another topic"),
				"project":          str("project ID or name (default: this session's project)"),
				"action":           str("the go-ahead asked for in one word (merge, deploy); a standing grant for exactly this action answers at once when the ask offers its option and has a default"),
				"irreversible":     boo("cannot be undone: never answered by a standing grant (give no default)"),
				"items": map[string]any{"type": "array", "maxItems": c.AskMaxItems, "items": item,
					"description": "batch several like decisions into one ask (e.g. the PRs to merge): the options apply to each item; the owner answers per item or all at once"},
				"operation_id": opIDProp}, "topic_key", "title", "context", "options"),
			hints("Ask the owner", additive, false),
			func(ctx context.Context, o *Ops, p AskParams) (c.CreateAskResponse, error) { return o.Ask(ctx, p) }),
			func(rc *renderCtx, r c.CreateAskResponse) string { return askCreateText(r, rc.now) }),
		compactly(tool("find_asks", "Find asks",
			"Find open asks and those decided in the last 7 days by topic key or text, before asking or messaging the owner. A decided topic is final: act on its answer instead of asking again.",
			obj(props{"topic_key": str("exact topic key"), "query": str("text over title, context and options"),
				"include_decided": boo("include asks decided in the last 7 days (default true)"),
				"project":         str("project ID or name"), "limit": num("max asks (default 50)")}),
			hints("Find asks", readOnly, true),
			func(ctx context.Context, o *Ops, p FindAsksParams) (c.Page[c.Ask], error) {
				return o.FindAsks(ctx, p, time.Now())
			}),
			func(rc *renderCtx, pg c.Page[c.Ask]) string { return asksText(pg.Items, rc.at(pg.SnapshotAt).now) }),
		compactly(tool("withdraw_ask", "Withdraw ask",
			"Take back your open ask (no longer needed), or mark it superseded by another ask. To correct or add to an ask, call ask_owner again with the same topic_key instead.",
			obj(props{"ask_id": str("ask ID (ask_...)"), "reason": str("one line the owner sees"),
				"superseded_by": str("the ask that replaces it (ask_...)"), "operation_id": opIDProp}, "ask_id"),
			hints("Withdraw ask", idempotent, true),
			func(ctx context.Context, o *Ops, p WithdrawAskParams) (c.Ask, error) { return o.WithdrawAsk(ctx, p) }),
			func(rc *renderCtx, a c.Ask) string { return askWithdrawText(a) }),
	}
}

// ---- rendering (MCP compact text and CLI human output) ----

// askZone is where ask deadlines are shown (the hub's default quiet-hours
// zone); the local zone when its tzdata is missing.
var askZone = func() *time.Location {
	if l, err := time.LoadLocation("America/Los_Angeles"); err == nil {
		return l
	}
	return time.Local
}()

// askWhen renders a deadline as "3:05 PM PDT (in 58m)", with the weekday
// when it is not today.
func askWhen(t *time.Time, now time.Time) string {
	if t == nil || t.IsZero() {
		return "no deadline"
	}
	lt, ln := t.In(askZone), now.In(askZone)
	f := "3:04 PM MST"
	if lt.YearDay() != ln.YearDay() || lt.Year() != ln.Year() {
		f = "Mon 3:04 PM MST"
	}
	rel := "passed"
	if d := t.Sub(now); d > 0 {
		rel = "in " + short(d)
	}
	return lt.Format(f) + " (" + rel + ")"
}

func askOptionText(a c.Ask, id string) string {
	if o, ok := a.Option(id); ok {
		return fmt.Sprintf("%s %q", o.ID, o.Label)
	}
	return id
}

// askOutcome is the decision in a few words ("answered b \"Rebase\" by
// owner: text").
func askOutcome(a c.Ask) string {
	switch a.State {
	case c.AskOpen:
		if a.DefaultOption != "" {
			return "open; defaults to " + askOptionText(a, a.DefaultOption)
		}
		return "open; no default (expires unanswered)"
	case c.AskSuperseded:
		return "superseded by " + orDash(a.SupersededBy)
	case c.AskWithdrawn:
		if an := a.Answer; an != nil && an.By.Kind == c.PrincipalOwner {
			if an.Text == c.AskDismissReason {
				return "dismissed by you (no option, no default)"
			}
			return "withdrawn by you"
		}
		return string(a.State)
	case c.AskExpired:
		return string(a.State)
	}
	s := string(a.State)
	if a.GrantID != "" {
		s += " by standing grant " + a.GrantID
	}
	if an := a.Answer; an != nil && an.Source == c.AskSourceChat {
		s += " in chat: " + clip(an.Text, 200)
		if len(an.With) > 0 {
			s += " (also answered " + strings.Join(an.With, ", ") + ")"
		}
		return s
	}
	if an := a.Answer; an != nil {
		if an.OptionID != "" {
			s += " " + askOptionText(a, an.OptionID)
		}
		if an.Text != "" {
			s += ": " + clip(an.Text, 200)
		}
	}
	return s
}

func askCreateText(r c.CreateAskResponse, now time.Time) string {
	var b strings.Builder
	a := r.Ask
	switch r.Status {
	case c.AskCreated, c.AskMerged:
		if a == nil {
			break
		}
		if r.Status == c.AskCreated {
			fmt.Fprintf(&b, "asked %s (topic %s); ", a.AskID, a.TopicKey)
		} else {
			fmt.Fprintf(&b, "merged into the open ask %s (topic %s, asked %d times); the owner was not notified again; ", a.AskID, a.TopicKey, max(a.Count, 1))
		}
		if a.DefaultOption != "" {
			fmt.Fprintf(&b, "defaults to %s at %s unless the owner answers", askOptionText(*a, a.DefaultOption), askWhen(a.DeadlineAt, now))
		} else {
			fmt.Fprintf(&b, "no default: it expires at %s unless the owner answers", askWhen(a.DeadlineAt, now))
		}
		if a.DeadlineRule == "morning" {
			b.WriteString(" (moved past the owner's quiet hours)")
		}
		b.WriteString(".\n")
		if a.MessageID != "" {
			fmt.Fprintf(&b, "The answer arrives as a reply to %s; use wait_reply %s or keep working", a.MessageID, a.MessageID)
		} else {
			b.WriteString("The answer arrives as a Pyre message; keep working")
		}
		if a.DefaultOption != "" {
			b.WriteString(" on the default path where safe")
		}
		b.WriteString(". Corrections: ask_owner again with the same topic_key (merges), or withdraw_ask.\n")
	case c.AskAlreadyDecided:
		if a == nil {
			break
		}
		fmt.Fprintf(&b, "already decided, nothing sent: %s (topic %s) was %s", a.AskID, a.TopicKey, askOutcome(*a))
		if an := a.Answer; an != nil && !an.At.IsZero() {
			fmt.Fprintf(&b, " %s", agoFrom(now, an.At))
		}
		b.WriteString(".\nAct on it; do not ask again. If something material changed, call ask_owner with reopen: true and say what changed in context.\n")
	case c.AskPossibleDuplicate:
		b.WriteString("not sent: similar open asks exist:\n")
		for _, x := range r.Candidates {
			b.WriteString("  " + askLine(x, now) + "\n")
		}
		b.WriteString("If one covers your question, ask_owner with its topic_key (your text merges into it); otherwise call again with confirm_new: true.\n")
	case c.AskGranted:
		if a == nil {
			break
		}
		opt := ""
		if a.Answer != nil {
			opt = askOptionText(*a, a.Answer.OptionID)
		}
		fmt.Fprintf(&b, "granted, the owner was not asked: %s (topic %s) is answered %s by standing grant %s", a.AskID, a.TopicKey, opt, a.GrantID)
		if a.Answer != nil && a.Answer.Text != "" {
			fmt.Fprintf(&b, " — %s", clip(a.Answer.Text, 400))
		}
		b.WriteString(".\nGo ahead; the owner gets an FYI. The decision also arrives as a Pyre message.\n")
	case c.AskRedirected:
		fmt.Fprintf(&b, "not sent to the owner: only top-level sessions ask the owner. Your question went to your parent session as %s", orDash(r.MessageID))
		if r.MessageID != "" {
			fmt.Fprintf(&b, "; use wait_reply %s for its answer", r.MessageID)
		}
		b.WriteString(".\n")
	}
	// The hub's Detail restates the known statuses above; show it only
	// for a status this client does not render.
	if b.Len() == 0 {
		fmt.Fprintf(&b, "status %s\n", r.Status)
		if r.Detail != "" {
			b.WriteString(r.Detail + "\n")
		}
	}
	return b.String()
}

// askLine: "ask_… · topic k · open; defaults to b "Label" at … · title · asker".
func askLine(a c.Ask, now time.Time) string {
	parts := []string{a.AskID, "topic " + a.TopicKey, askOutcome(a)}
	if a.State == c.AskOpen {
		parts[2] += " at " + askWhen(a.DeadlineAt, now)
	} else if an := a.Answer; an != nil && !an.At.IsZero() {
		parts[2] += " " + agoFrom(now, an.At)
	}
	parts = append(parts, clip(a.Title, 100))
	if a.SessionID != "" {
		parts = append(parts, "by "+a.SessionID)
	}
	if a.Count > 1 {
		parts = append(parts, fmt.Sprintf("asked %d times", a.Count))
	}
	return strings.Join(parts, " · ")
}

func asksText(as []c.Ask, now time.Time) string {
	if len(as) == 0 {
		return "no matching asks (open, or decided in the last 7 days)\n"
	}
	var b strings.Builder
	decided := false
	for _, a := range as {
		b.WriteString(askLine(a, now) + "\n")
		decided = decided || a.State.Decided()
	}
	fmt.Fprintf(&b, "%d ask", len(as))
	if len(as) != 1 {
		b.WriteString("s")
	}
	b.WriteString(" · the same topic_key merges into an open ask")
	if decided {
		b.WriteString(" · decided ones are final: act on them")
	}
	b.WriteString("\n")
	return b.String()
}

func askWithdrawText(a c.Ask) string {
	s := fmt.Sprintf("%s (topic %s) is %s", a.AskID, a.TopicKey, askOutcome(a))
	if a.State == c.AskOpen {
		return s + "\n"
	}
	return s + "; the owner's item is closed\n"
}

// askDetail is `pyre ask show`.
func askDetail(w io.Writer, a c.Ask, now time.Time) {
	fmt.Fprintf(w, "%s  %s  topic %s\n", a.AskID, a.State, a.TopicKey)
	fmt.Fprintf(w, "  %s\n", a.Title)
	asker := "owner"
	if a.SessionID != "" {
		asker = a.SessionID
	}
	fmt.Fprintf(w, "  asked by %s %s", asker, agoFrom(now, a.CreatedAt))
	if a.ProjectID != nil && *a.ProjectID != "" {
		fmt.Fprintf(w, " in %s", *a.ProjectID)
	}
	if a.Count > 1 {
		fmt.Fprintf(w, ", %d times", a.Count)
	}
	if a.Urgent {
		fmt.Fprint(w, ", urgent")
	}
	fmt.Fprintln(w)
	if a.State == c.AskOpen {
		fmt.Fprintf(w, "  deadline %s", askWhen(a.DeadlineAt, now))
		if a.DeadlineRule == "morning" {
			fmt.Fprint(w, " (moved past quiet hours)")
		}
		fmt.Fprintln(w)
	}
	fmt.Fprintln(w)
	for _, l := range strings.Split(strings.TrimRight(a.Context, "\n"), "\n") {
		fmt.Fprintf(w, "  %s\n", l)
	}
	fmt.Fprintln(w)
	for _, o := range a.Options {
		mark := " "
		if o.ID == a.DefaultOption {
			mark = "*"
		}
		fmt.Fprintf(w, " %s %-8s %s", mark, o.ID, o.Label)
		if o.Detail != "" {
			fmt.Fprintf(w, " — %s", o.Detail)
		}
		fmt.Fprintln(w)
	}
	if a.DefaultOption != "" {
		fmt.Fprintln(w, "  (* default)")
	}
	if len(a.Items) > 0 {
		fmt.Fprintf(w, "  items (%d; the options apply to each):\n", len(a.Items))
		for _, it := range a.Items {
			fmt.Fprintf(w, "    %-10s %s", it.ID, it.Label)
			if it.Detail != "" {
				fmt.Fprintf(w, " — %s", it.Detail)
			}
			if it.URL != "" {
				fmt.Fprintf(w, " <%s>", it.URL)
			}
			if a.State.Decided() && a.Answer != nil {
				if o := askAnswerItem(a, it.ID); o != "" {
					fmt.Fprintf(w, "  → %s", o)
				}
			}
			fmt.Fprintln(w)
		}
	}
	if a.GrantID != "" {
		fmt.Fprintf(w, "  answered by standing grant %s (the owner was not asked)\n", a.GrantID)
	}
	for _, u := range a.Updates {
		fmt.Fprintf(w, "  update %d %s: %s\n", u.Seq, agoFrom(now, u.At), clip(u.Text, 300))
	}
	if a.State.Decided() {
		fmt.Fprintf(w, "  outcome: %s", askOutcome(a))
		if an := a.Answer; an != nil {
			fmt.Fprintf(w, " by %s %s", principalText(an.By), agoFrom(now, an.At))
		}
		fmt.Fprintln(w)
	} else {
		fmt.Fprintf(w, "  answer: pyre ask answer %s <option> [--text ...]\n", a.AskID)
		if len(a.Items) > 0 {
			fmt.Fprintf(w, "  per item: pyre ask answer %s [<option for the rest>] --item %s=<option> ...\n", a.AskID, a.Items[0].ID)
		}
	}
	if a.MessageID != "" {
		fmt.Fprintf(w, "  message %s", a.MessageID)
		if a.DecisionMessageID != "" {
			fmt.Fprintf(w, ", decision sent as %s", a.DecisionMessageID)
		}
		fmt.Fprintln(w)
	}
}

// askAnswerItem is the option chosen for one item of a decided ask.
func askAnswerItem(a c.Ask, itemID string) string {
	if a.Answer == nil {
		return ""
	}
	if o, ok := a.Answer.Items[itemID]; ok {
		return o
	}
	return a.Answer.OptionID
}

func printAsks(w io.Writer, as []c.Ask, now time.Time) {
	var rows [][]string
	for _, a := range as {
		when := "-"
		if a.State == c.AskOpen {
			when = askWhen(a.DeadlineAt, now)
		} else if an := a.Answer; an != nil {
			when = agoFrom(now, an.At)
		}
		rows = append(rows, []string{a.AskID, string(a.State), clip(a.TopicKey, 30), clip(a.Title, 50), clip(askOutcome(a), 40), when, orDash(a.SessionID)})
	}
	table(w, "ID\tSTATE\tTOPIC\tTITLE\tOUTCOME\tDEADLINE/DECIDED\tASKER", rows)
}

// ---- CLI: pyre ask ----

func (a *App) addAskCommands(root *cobra.Command) {
	var open bool
	var project, topic, session, since string
	var limit int
	ls := func(cmd *cobra.Command, args []string) error {
		ops, err := a.connect()
		if err != nil {
			return err
		}
		req := c.ListAsksRequest{TopicKey: topic, Query: strings.Join(args, " ")}
		if open {
			req.States = []c.AskState{c.AskOpen}
		}
		if since != "" {
			t, err := parseSince(since, time.Now())
			if err != nil {
				return usagef("--since: %v", err)
			}
			req.Since = &t
		}
		if req.ProjectID, err = ops.projectPtr(cmd.Context(), project); err != nil {
			return err
		}
		if session != "" {
			if req.SessionID, err = ops.sessionID(cmd.Context(), session); err != nil {
				return err
			}
		}
		page, err := ops.listAsks(cmd.Context(), req, limit)
		if err != nil {
			return err
		}
		return a.emit(page, func(w io.Writer) {
			if len(page.Items) == 0 {
				fmt.Fprintln(w, "no asks")
				return
			}
			printAsks(w, page.Items, time.Now())
		})
	}
	ask := &cobra.Command{
		Use:     "ask [ls|show|answer|dismiss|withdraw|new|find]",
		Aliases: []string{"asks"},
		Short:   "Structured questions from sessions to the owner, with options, a default and a deadline",
		Long: `Asks are decisions sessions need from the owner: a topic, context, 2–4
options, a default applied at the deadline (1 h; a deadline at night moves to
the morning) and one open ask per topic. Answer several at once with
` + "`pyre ask answer <id> <option> <id> <option> ...`" + `.
Writing to the session itself (typing into it, or ` + "`pyre send`" + ` to it, not a
thread reply) answers its open asks with your words; ` + "`pyre ask dismiss`" + `
archives one without an answer.`,
		Args: cobra.NoArgs,
		RunE: a.run(ls),
	}
	lsCmd := &cobra.Command{Use: "ls [query...]", Short: "List asks (newest first)", RunE: a.run(ls)}
	lf := lsCmd.Flags()
	lf.BoolVar(&open, "open", false, "only open asks")
	lf.StringVar(&project, "project", "", "only this project")
	lf.StringVar(&topic, "topic", "", "only this topic key")
	lf.StringVar(&session, "session", "", "only asks from this session")
	lf.StringVar(&since, "since", "", "decided asks only since (7d, 24h, RFC 3339); open ones always match")
	lf.IntVar(&limit, "limit", 100, "max asks")
	ask.AddCommand(lsCmd)

	ask.AddCommand(&cobra.Command{
		Use:   "show <ask_id>",
		Short: "Show an ask with its context, options and outcome",
		Args:  cobra.ExactArgs(1),
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			api, err := ops.asks()
			if err != nil {
				return err
			}
			as, err := api.GetAsk(cmd.Context(), c.GetRequest{ID: args[0]})
			if err != nil {
				return err
			}
			return a.emit(as, func(w io.Writer) { askDetail(w, as, time.Now()) })
		}),
	})

	var optFlag, text string
	var itemFlags []string
	answer := &cobra.Command{
		Use:   "answer <ask_id> [<option>] [<ask_id> [<option>] ...] [--option ID] [--text TEXT] [--item ITEM=OPTION ...]",
		Short: "Answer one or more asks in one call (each commits independently)",
		Long: `Answer asks by option ID: ` + "`pyre ask answer ask_A b ask_B a`" + ` or ` + "`ask_A=b ask_B=a`" + `.
--option applies to every ask named without one; --text adds free text to
each answer (or is the answer when no option is given).
A batched ask (one with items) takes per-item answers: --item pr-12=approve
--item pr-13=hold; items left out take the ask's option (without one, answer
every item).`,
		Args: cobra.MinimumNArgs(1),
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			items, err := parseAskAnswers(args, optFlag, text, itemFlags...)
			if err != nil {
				return err
			}
			ops, err := a.connect()
			if err != nil {
				return err
			}
			resp, err := ops.AnswerAsks(cmd.Context(), items, a.opID)
			if err != nil {
				return err
			}
			var firstErr *c.Error
			if err := a.emit(resp, func(w io.Writer) {
				for _, r := range resp.Results {
					switch {
					case r.Error != nil:
						fmt.Fprintf(w, "%s: not answered: %s\n", r.AskID, r.Error.Message)
					case r.Ask != nil:
						fmt.Fprintf(w, "%s: %s\n", r.AskID, askOutcome(*r.Ask))
					default:
						fmt.Fprintf(w, "%s: done\n", r.AskID)
					}
				}
			}); err != nil {
				return err
			}
			for _, r := range resp.Results {
				if r.Error != nil && firstErr == nil {
					firstErr = r.Error
				}
			}
			if firstErr != nil {
				return withStatus(exitForCode(firstErr.Code), fmt.Errorf("some asks were not answered: %s", firstErr.Message))
			}
			return nil
		}),
	}
	answer.Flags().StringVar(&optFlag, "option", "", "option `ID` for every ask named without one")
	answer.Flags().StringVar(&text, "text", "", "free-text answer (with or instead of an option)")
	answer.Flags().StringArrayVar(&itemFlags, "item", nil, "batched ask: an item's answer as `ITEM=OPTION` (repeatable; one ask only)")
	ask.AddCommand(answer)

	var wp WithdrawAskParams
	withdraw := &cobra.Command{
		Use:   "withdraw <ask_id> [--reason TEXT] [--superseded-by ask_id]",
		Short: "Withdraw an open ask (or mark it superseded)",
		Args:  cobra.ExactArgs(1),
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			p := wp
			p.AskID, p.OperationID = args[0], a.opID
			as, err := ops.WithdrawAsk(cmd.Context(), p)
			if err != nil {
				return err
			}
			return a.emit(as, func(w io.Writer) { fmt.Fprint(w, askWithdrawText(as)) })
		}),
	}
	withdraw.Flags().StringVar(&wp.Reason, "reason", "", "one line the owner sees")
	withdraw.Flags().StringVar(&wp.SupersededBy, "superseded-by", "", "the ask that replaces it")
	ask.AddCommand(withdraw)

	ask.AddCommand(&cobra.Command{
		Use:     "dismiss <ask_id>",
		Aliases: []string{"archive"},
		Short:   "Dismiss an open ask (owner): no option is chosen, its default never applies, and the session is told",
		Args:    cobra.ExactArgs(1),
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			as, err := ops.DismissAsk(cmd.Context(), args[0], a.opID)
			if err != nil {
				return err
			}
			return a.emit(as, func(w io.Writer) { fmt.Fprint(w, askWithdrawText(as)) })
		}),
	})

	var np AskParams
	var opts, itemArgs []string
	var deadline time.Duration
	newCmd := &cobra.Command{
		Use:   "new --topic KEY --title TEXT --context TEXT --option id=label[|detail]... [--default ID]",
		Short: "Ask the owner to decide (top-level sessions; a child's ask goes to its parent)",
		Long: `Ask the owner a structured question. Run ` + "`pyre ask find --topic KEY`" + ` first: a
decided topic is final, and an ask on an open topic merges into it.
Context is for a reader who is not in the weeds: what, why it matters, what
you tried, what each option does. Give a default you will take (none for
irreversible actions) and keep working on the default path where safe.
--context - reads the context from stdin.`,
		Args: cobra.NoArgs,
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			p := np
			var err error
			if p.Context, err = a.readArg(p.Context); err != nil {
				return err
			}
			for _, s := range opts {
				id, rest, ok := strings.Cut(s, "=")
				if !ok {
					id, rest = "", s
				}
				label, detail, _ := strings.Cut(rest, "|")
				p.Options = append(p.Options, c.AskOption{ID: id, Label: label, Detail: detail})
			}
			for _, s := range itemArgs {
				id, rest, ok := strings.Cut(s, "=")
				if !ok {
					return usagef("--item %q: give it as id=label[|detail[|url]]", s)
				}
				parts := strings.SplitN(rest, "|", 3)
				it := c.AskItem{ID: id, Label: parts[0]}
				if len(parts) > 1 {
					it.Detail = parts[1]
				}
				if len(parts) > 2 {
					it.URL = parts[2]
				}
				p.Items = append(p.Items, it)
			}
			if deadline > 0 {
				p.DeadlineMinutes = int((deadline + time.Minute - 1) / time.Minute)
			}
			p.OperationID = a.opID
			ops, err := a.connect()
			if err != nil {
				return err
			}
			resp, err := ops.Ask(cmd.Context(), p)
			if err != nil {
				return err
			}
			return a.emit(resp, func(w io.Writer) { fmt.Fprint(w, askCreateText(resp, time.Now())) })
		}),
	}
	nf := newCmd.Flags()
	nf.StringVar(&np.TopicKey, "topic", "", "short stable slug for the decision (same topic = same ask)")
	nf.StringVar(&np.Title, "title", "", "the question in one line")
	nf.StringVar(&np.Context, "context", "", "what, why it matters, what you tried, what each option does (- reads stdin)")
	nf.StringArrayVar(&opts, "option", nil, "an option as id=label[|detail] (2–4)")
	nf.StringVar(&np.Default, "default", "", "option applied at the deadline if unanswered")
	nf.DurationVar(&deadline, "deadline", 0, "time to answer (default 1h; a night deadline moves to the morning unless --urgent)")
	nf.BoolVar(&np.Urgent, "urgent", false, "needs an answer even at night")
	nf.BoolVar(&np.Reopen, "reopen", false, "ask again on a decided topic because something changed")
	nf.BoolVar(&np.ConfirmNew, "confirm-new", false, "create it even though a similar open ask exists")
	nf.StringVar(&np.Project, "project", "", "project (default: this session's)")
	nf.StringVar(&np.Action, "action", "", "the go-ahead asked for (merge); a standing grant for exactly it may answer at once")
	nf.BoolVar(&np.Irreversible, "irreversible", false, "cannot be undone: never answered by a standing grant")
	nf.StringArrayVar(&itemArgs, "item", nil, "batched ask: an item as id=label[|detail[|url]] (repeatable; the options apply to each)")
	ask.AddCommand(newCmd)

	var fp FindAsksParams
	var openOnly bool
	find := &cobra.Command{
		Use:   "find [--topic KEY] [query...]",
		Short: "Find open asks and those decided in the last 7 days (before asking)",
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			p := fp
			p.Query = strings.Join(args, " ")
			if openOnly {
				f := false
				p.IncludeDecided = &f
			}
			now := time.Now()
			page, err := ops.FindAsks(cmd.Context(), p, now)
			if err != nil {
				return err
			}
			return a.emit(page, func(w io.Writer) { fmt.Fprint(w, asksText(page.Items, now)) })
		}),
	}
	find.Flags().StringVar(&fp.TopicKey, "topic", "", "exact topic key")
	find.Flags().StringVar(&fp.Project, "project", "", "only this project")
	find.Flags().BoolVar(&openOnly, "open", false, "only open asks")
	ask.AddCommand(find)
	a.addAskHoldCommands(ask) // ops_askhold.go (W-web-inbox)

	root.AddCommand(ask)
}

// parseAskAnswers reads "ask_A b ask_B a", "ask_A=b ask_B=a" or
// "ask_A ask_B --option b" into answer items; perItem ("pr-12=approve")
// answers the items of a batched ask (one ask only).
func parseAskAnswers(args []string, option, text string, perItem ...string) ([]c.AnswerAskItem, error) {
	var items []c.AnswerAskItem
	open := -1 // index of an item still waiting for its option
	for _, arg := range args {
		arg = strings.TrimSpace(arg)
		if id, opt, ok := strings.Cut(arg, "="); ok && c.ValidID(id, c.PrefixAsk) {
			items = append(items, c.AnswerAskItem{AskID: id, OptionID: opt, Text: text})
			open = -1
			continue
		}
		if c.ValidID(arg, c.PrefixAsk) {
			items = append(items, c.AnswerAskItem{AskID: arg, OptionID: option, Text: text})
			open = len(items) - 1
			continue
		}
		if open < 0 {
			return nil, usagef("%q: name the ask first (pyre ask answer <ask_id> <option>)", arg)
		}
		items[open].OptionID = arg
		open = -1
	}
	if len(perItem) > 0 {
		if len(items) != 1 {
			return nil, usagef("--item answers the items of one batched ask; name exactly one ask")
		}
		items[0].Items = map[string]string{}
		for _, s := range perItem {
			id, opt, ok := strings.Cut(strings.TrimSpace(s), "=")
			if !ok || strings.TrimSpace(id) == "" || strings.TrimSpace(opt) == "" {
				return nil, usagef("--item %q: give it as ITEM=OPTION (e.g. pr-12=approve)", s)
			}
			items[0].Items[strings.TrimSpace(id)] = strings.TrimSpace(opt)
		}
	}
	for _, it := range items {
		if it.OptionID == "" && strings.TrimSpace(it.Text) == "" && len(it.Items) == 0 {
			return nil, usagef("%s: give an option (pyre ask show %s lists them) or --text", it.AskID, it.AskID)
		}
	}
	return items, nil
}

// DismissAttention is the owner archiving an inbox item: an open ask's
// item dismisses the ask (DismissAsk; the deriver then closes the item),
// any other item is marked handled.
func (o *Ops) DismissAttention(ctx context.Context, id, opID string) (c.AttentionItem, error) {
	it, err := o.API.GetAttention(ctx, c.GetRequest{ID: strings.TrimSpace(id)})
	if err != nil {
		return it, err
	}
	if it.Kind == c.AttnAsk && it.Object.Kind == c.KindAsk && it.Object.ID != "" {
		if api, err := o.asks(); err == nil {
			if a, err := api.GetAsk(ctx, c.GetRequest{ID: it.Object.ID}); err == nil && a.State == c.AskOpen {
				if _, err := o.DismissAsk(ctx, a.AskID, opID); err != nil && !c.IsCode(err, c.ErrConflict) {
					return it, err
				}
				if after, err := o.API.GetAttention(ctx, c.GetRequest{ID: it.AttentionID}); err == nil && after.State != c.AttnOpen && after.State != c.AttnRead {
					return after, nil
				}
			}
		}
	}
	op := opID
	if op != "" {
		op += "-attn"
	}
	return o.API.UpdateAttention(ctx, c.UpdateAttentionRequest{Mutation: mut(op), AttentionID: it.AttentionID, State: c.AttnHandled})
}
