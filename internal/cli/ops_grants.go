package cli

// Standing grants and proposals (W-grants): a standing grant is the
// owner's lasting go-ahead ("merge PRs once reviewed and green") that
// answers matching asks without asking him again. Only the owner creates
// one (`pyre grant add`, or approving a proposal in his inbox); a session
// proposes one with propose_grant, and a project fact with propose_fact.
// A proposal is an owner ask: when he approves, the hub records it with his
// own answer text and the ask ID — nothing a session writes is stored as
// his words. propose_grant/propose_fact/list_grants/revoke_grant are
// pre-approved (they only file an ask, read, or narrow). The ops are
// optional (contracts.StandingGrantsAPI, AsksAPI).

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	c "github.com/jhh3/pyre/internal/contracts"
)

func init() { mcpToolProviders = append(mcpToolProviders, grantMCPTools) }

func (o *Ops) grants() (c.StandingGrantsAPI, error) {
	if a, ok := o.API.(c.StandingGrantsAPI); ok {
		return a, nil
	}
	return nil, c.Errorf(c.ErrUnsupported, "this connection does not offer standing grants (%s); upgrade pyre-hub", c.OpGrantsCreate)
}

// GrantParams are `pyre grant add`'s flags (owner only; the hub refuses
// anyone else).
type GrantParams struct {
	Action      string `json:"action"`
	Option      string `json:"option"`
	Note        string `json:"note,omitempty"`
	Quote       string `json:"quote,omitempty"`
	Scope       string `json:"scope,omitempty"`
	Project     string `json:"project,omitempty"`
	Session     string `json:"session,omitempty"`
	ExpiresDays int    `json:"expires_days,omitempty"`
	OperationID string `json:"operation_id,omitempty"`
}

func checkExpiresDays(n int) error {
	if maxDays := int(c.GrantMaxTTL / (24 * time.Hour)); n < 0 || n > maxDays {
		return usagef("expires is 1 to %d days (default 7)", maxDays)
	}
	return nil
}

// CreateGrant records a standing grant (the owner's own command).
func (o *Ops) CreateGrant(ctx context.Context, p GrantParams) (c.StandingGrant, error) {
	api, err := o.grants()
	if err != nil {
		return c.StandingGrant{}, err
	}
	req := c.CreateGrantRequest{Mutation: mut(p.OperationID), Action: strings.TrimSpace(p.Action), Option: strings.TrimSpace(p.Option),
		Condition: strings.TrimSpace(p.Note), Quote: strings.TrimSpace(p.Quote), Scope: c.GrantScope(strings.ToLower(strings.TrimSpace(p.Scope)))}
	switch {
	case req.Action == "":
		return c.StandingGrant{}, usagef("action is required: the go-ahead in one word (merge); only asks naming exactly it are answered")
	case req.Option == "":
		return c.StandingGrant{}, usagef("option is required: the ask option (id or label) the grant picks, e.g. merge")
	case req.Scope != "" && req.Scope != c.GrantScopeProject && req.Scope != c.GrantScopeSession:
		return c.StandingGrant{}, usagef("scope is project or session")
	}
	if err := checkExpiresDays(p.ExpiresDays); err != nil {
		return c.StandingGrant{}, err
	}
	if p.ExpiresDays > 0 {
		at := time.Now().Add(time.Duration(p.ExpiresDays) * 24 * time.Hour).UTC()
		req.ExpiresAt = &at
	}
	if req.ProjectID, err = o.projectPtr(ctx, p.Project); err != nil {
		return c.StandingGrant{}, err
	}
	if s := strings.TrimSpace(p.Session); s != "" {
		if req.SessionID, err = o.sessionID(ctx, s); err != nil {
			return c.StandingGrant{}, err
		}
	}
	return api.CreateGrant(ctx, req)
}

// ProposeGrantParams are propose_grant's arguments.
type ProposeGrantParams struct {
	Action          string `json:"action"`
	Option          string `json:"option"`
	Note            string `json:"note,omitempty"`
	Why             string `json:"why,omitempty"`
	Scope           string `json:"scope,omitempty"`
	Session         string `json:"session,omitempty"`
	ExpiresDays     int    `json:"expires_days,omitempty"`
	DeadlineMinutes int    `json:"deadline_minutes,omitempty"`
	OperationID     string `json:"operation_id,omitempty"`
}

// ProposeGrant files an owner ask proposing a standing grant.
func (o *Ops) ProposeGrant(ctx context.Context, p ProposeGrantParams) (c.CreateAskResponse, error) {
	api, err := o.asks()
	if err != nil {
		return c.CreateAskResponse{}, err
	}
	g := c.GrantProposal{Action: strings.TrimSpace(p.Action), Option: strings.TrimSpace(p.Option), Condition: strings.TrimSpace(p.Note),
		Scope: c.GrantScope(strings.ToLower(strings.TrimSpace(p.Scope))), ExpiresInDays: p.ExpiresDays}
	switch {
	case g.Action == "":
		return c.CreateAskResponse{}, usagef("action is required: the go-ahead in one word (merge)")
	case g.Option == "":
		return c.CreateAskResponse{}, usagef("option is required: the ask option the grant would pick (id or label your asks use, e.g. merge)")
	case g.Scope != "" && g.Scope != c.GrantScopeProject && g.Scope != c.GrantScopeSession:
		return c.CreateAskResponse{}, usagef("scope is project or session")
	case p.DeadlineMinutes < 0:
		return c.CreateAskResponse{}, usagef("deadline_minutes must be positive")
	}
	if err := checkExpiresDays(p.ExpiresDays); err != nil {
		return c.CreateAskResponse{}, err
	}
	if s := strings.TrimSpace(p.Session); s != "" && s != "self" {
		if g.SessionID, err = o.sessionID(ctx, s); err != nil {
			return c.CreateAskResponse{}, err
		}
	}
	return api.CreateAsk(ctx, c.CreateAskRequest{Mutation: mut(p.OperationID), Context: strings.TrimSpace(p.Why),
		DeadlineSeconds: p.DeadlineMinutes * 60, Proposal: &c.AskProposal{Kind: c.ProposalGrant, Grant: &g}})
}

// ProposeFactParams are propose_fact's arguments.
type ProposeFactParams struct {
	Fact            string `json:"fact"`
	Why             string `json:"why,omitempty"`
	DeadlineMinutes int    `json:"deadline_minutes,omitempty"`
	OperationID     string `json:"operation_id,omitempty"`
}

// ProposeFact files an owner ask proposing a line for the project's fact
// sheet.
func (o *Ops) ProposeFact(ctx context.Context, p ProposeFactParams) (c.CreateAskResponse, error) {
	api, err := o.asks()
	if err != nil {
		return c.CreateAskResponse{}, err
	}
	if strings.TrimSpace(p.Fact) == "" {
		return c.CreateAskResponse{}, usagef("fact is required: the settled fact in one line")
	}
	if p.DeadlineMinutes < 0 {
		return c.CreateAskResponse{}, usagef("deadline_minutes must be positive")
	}
	return api.CreateAsk(ctx, c.CreateAskRequest{Mutation: mut(p.OperationID), Context: strings.TrimSpace(p.Why),
		DeadlineSeconds: p.DeadlineMinutes * 60, Proposal: &c.AskProposal{Kind: c.ProposalFact, Fact: strings.TrimSpace(p.Fact)}})
}

// ListGrantsParams are list_grants' arguments.
type ListGrantsParams struct {
	// Session lists the grants holding for that session (default: this
	// session, unless All or Project).
	Session        string `json:"session,omitempty"`
	Project        string `json:"project,omitempty"`
	Action         string `json:"action,omitempty"`
	All            bool   `json:"all,omitempty"`
	IncludeRevoked bool   `json:"include_revoked,omitempty"`
}

// ListGrants lists standing grants.
func (o *Ops) ListGrants(ctx context.Context, p ListGrantsParams) (c.ListGrantsResponse, error) {
	api, err := o.grants()
	if err != nil {
		return c.ListGrantsResponse{}, err
	}
	req := c.ListGrantsRequest{Action: strings.TrimSpace(p.Action), IncludeRevoked: p.IncludeRevoked}
	if req.ProjectID, err = o.projectPtr(ctx, p.Project); err != nil {
		return c.ListGrantsResponse{}, err
	}
	switch s := strings.TrimSpace(p.Session); {
	case s != "" && s != "self":
		if req.SessionID, err = o.sessionID(ctx, s); err != nil {
			return c.ListGrantsResponse{}, err
		}
	case !p.All && req.ProjectID == nil:
		req.SessionID = o.Self.SessionID
	}
	return api.ListGrants(ctx, req)
}

// RevokeGrantParams are revoke_grant's arguments.
type RevokeGrantParams struct {
	GrantID     string `json:"grant_id"`
	Reason      string `json:"reason,omitempty"`
	OperationID string `json:"operation_id,omitempty"`
}

// RevokeGrant revokes a standing grant (the owner, or a session it holds
// for).
func (o *Ops) RevokeGrant(ctx context.Context, p RevokeGrantParams) (c.StandingGrant, error) {
	api, err := o.grants()
	if err != nil {
		return c.StandingGrant{}, err
	}
	id := strings.TrimSpace(p.GrantID)
	if !c.ValidID(id, c.PrefixStandingGrant) {
		return c.StandingGrant{}, usagef("grant_id must be a standing grant ID (sgr_...); list_grants lists them")
	}
	return api.RevokeGrant(ctx, c.RevokeGrantRequest{Mutation: mut(p.OperationID), GrantID: id, Reason: strings.TrimSpace(p.Reason)})
}

// ---- MCP ----

func grantMCPTools() []MCPTool {
	why := str("why the owner should approve it, with the evidence (his earlier message or ask, links); shown as your words, never as his")
	deadline := num("minutes until the proposal expires unanswered (default 1440); nothing is recorded then")
	return []MCPTool{
		compactly(tool("propose_grant", "Propose a standing grant",
			"Ask the owner to approve a lasting go-ahead (e.g. merge PRs once reviewed and green). Files an owner ask; only if he approves does Pyre record it, quoting his own answer. Never claim he already said it.",
			obj(props{"action": str("the go-ahead in one word: merge, ... (asks must name exactly this action to be answered)"),
				"option":           str("the ask option (id or label) the grant picks, e.g. merge"),
				"note":             str("informational note shown with it, e.g. \"once reviewed and CI green\" (Pyre never checks it)"),
				"why":              why,
				"scope":            str("project (default when this session has one) or session (this session and the sessions it starts)"),
				"session":          str("scope session: the root of the tree (default: this session; or a session you started)"),
				"expires_days":     num("days after approval until it lapses (default 7, max 30)"),
				"deadline_minutes": deadline, "operation_id": opIDProp}, "action", "option"),
			hints("Propose a standing grant", additive, false),
			func(ctx context.Context, o *Ops, p ProposeGrantParams) (c.CreateAskResponse, error) {
				return o.ProposeGrant(ctx, p)
			}),
			func(rc *renderCtx, r c.CreateAskResponse) string { return proposalCreateText(r, rc.now) }),
		compactly(tool("propose_fact", "Propose a project fact",
			"Ask the owner to approve a settled fact for this project's fact sheet (every new session reads it). Files an owner ask; only if he approves does Pyre add it, quoting his own answer.",
			obj(props{"fact": str("the settled fact, one line"), "why": why, "deadline_minutes": deadline, "operation_id": opIDProp}, "fact"),
			hints("Propose a project fact", additive, false),
			func(ctx context.Context, o *Ops, p ProposeFactParams) (c.CreateAskResponse, error) {
				return o.ProposeFact(ctx, p)
			}),
			func(rc *renderCtx, r c.CreateAskResponse) string { return proposalCreateText(r, rc.now) }),
		compactly(tool("list_grants", "List standing grants",
			"List the standing grants (the owner's lasting go-aheads) that hold for this session: its project's and its session tree's.",
			obj(props{"session": str("list the grants holding for this session (default: this session)"), "project": str("project ID or name (your own)"),
				"action": str("only this action"), "all": boo("every grant you may see, not only those holding for this session"),
				"include_revoked": boo("include revoked and expired grants")}),
			hints("List standing grants", readOnly, true),
			func(ctx context.Context, o *Ops, p ListGrantsParams) (c.ListGrantsResponse, error) {
				return o.ListGrants(ctx, p)
			}),
			func(rc *renderCtx, r c.ListGrantsResponse) string { return grantsText(r.Grants, rc.now) }),
		compactly(tool("revoke_grant", "Revoke a standing grant",
			"Revoke a standing grant that holds for you, e.g. when the owner takes the go-ahead back. Asks for its action then go to the owner again.",
			obj(props{"grant_id": str("standing grant ID (sgr_...)"), "reason": str("one line: why"), "operation_id": opIDProp}, "grant_id"),
			hints("Revoke a standing grant", idempotent, true),
			func(ctx context.Context, o *Ops, p RevokeGrantParams) (c.StandingGrant, error) {
				return o.RevokeGrant(ctx, p)
			}),
			func(rc *renderCtx, g c.StandingGrant) string { return grantLine(g, time.Now()) + "\n" }),
	}
}

// ---- rendering ----

func proposalCreateText(r c.CreateAskResponse, now time.Time) string {
	s := askCreateText(r, now)
	if r.Status == c.AskCreated || r.Status == c.AskMerged {
		s += "Nothing is recorded unless the owner approves; if he does, Pyre records it with his answer and tells you. Do not record or claim it yourself.\n"
	}
	return s
}

func grantScopeText(g c.StandingGrant) string {
	if g.Scope == c.GrantScopeProject {
		return "project " + orDash(ptrStr(g.ProjectID))
	}
	return "session tree " + g.SessionID
}

func ptrStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// grantProvenanceText quotes the owner only with his own words.
func grantProvenanceText(g c.StandingGrant) string {
	quote := clip(strings.Join(strings.Fields(g.Quote), " "), 200)
	switch {
	case g.AskID != "" && quote != "":
		return fmt.Sprintf("owner: %q (approved %s)", quote, g.AskID)
	case g.AskID != "":
		return "owner approved proposal " + g.AskID + " (no words typed)"
	case quote != "":
		return fmt.Sprintf("set by the owner: %q", quote)
	}
	return "set by the owner"
}

// grantLine: `sgr_… · merge → "merge" · project prj_… · owner: "…" (approved ask_…) · expires … · used 2×`.
func grantLine(g c.StandingGrant, now time.Time) string {
	parts := []string{g.GrantID, fmt.Sprintf("%s → %q", g.Action, g.Option), grantScopeText(g), grantProvenanceText(g)}
	if g.Condition != "" {
		parts = append(parts, "note: "+clip(g.Condition, 120))
	}
	switch {
	case g.RevokedAt != nil:
		r := "revoked " + agoFrom(now, *g.RevokedAt)
		if g.RevokeReason != "" {
			r += ": " + clip(g.RevokeReason, 120)
		}
		parts = append(parts, r)
	case !g.ExpiresAt.After(now):
		parts = append(parts, "expired")
	default:
		parts = append(parts, "expires "+askWhen(&g.ExpiresAt, now))
	}
	if g.UsedCount > 0 {
		parts = append(parts, fmt.Sprintf("used %d×", g.UsedCount))
	}
	return strings.Join(parts, " · ")
}

func grantCreatedText(g c.StandingGrant) string {
	holders := "every session in " + orDash(ptrStr(g.ProjectID))
	if g.Scope == c.GrantScopeSession {
		holders = g.SessionID + " and every session it starts"
	}
	return fmt.Sprintf("recorded standing grant %s: %s → %q (%s), expires %s. It holds for %s: asks naming action %q that offer option %q, have a default and are not irreversible are answered at once (you get an FYI each time). Revoke with `pyre grant revoke %s`.\n",
		g.GrantID, g.Action, g.Option, grantScopeText(g), askWhen(&g.ExpiresAt, time.Now()), holders, g.Action, g.Option, g.GrantID)
}

func grantsText(gs []c.StandingGrant, now time.Time) string {
	if len(gs) == 0 {
		return "no standing grants\n"
	}
	var b strings.Builder
	for _, g := range gs {
		b.WriteString(grantLine(g, now) + "\n")
	}
	fmt.Fprintf(&b, "%d grant", len(gs))
	if len(gs) != 1 {
		b.WriteString("s")
	}
	b.WriteString(" · an ask is answered by one only when it names the action, offers the option, has a default and is not irreversible\n")
	return b.String()
}

// ---- CLI: pyre grant, pyre propose ----

func (a *App) addGrantCommands(root *cobra.Command) {
	grant := &cobra.Command{
		Use:     "grant [add|ls|revoke]",
		Aliases: []string{"grants"},
		Short:   "Standing grants: the owner's lasting go-aheads (\"merge PRs once reviewed and green\")",
		Long: `A standing grant is a lasting go-ahead from the owner. It holds for a project
or for a session and every session it starts, until it expires (default 7 days,
at most 30) or is revoked. An ask is answered by it at once only when the ask
names its action, offers its option, has a default and is not irreversible;
production deploys, deleting data, spending, force-pushes, public releases and
account changes are never covered. Only the owner adds one; a session proposes
one with ` + "`pyre propose grant`" + ` and the owner approves it in his inbox.`,
	}

	var gp GrantParams
	add := &cobra.Command{
		Use:   "add --action ACTION --option OPTION [--note TEXT] [--quote TEXT] [--scope project|session] [--project P] [--session S] [--expires DAYS]",
		Short: "Record a standing grant (owner only)",
		Args:  cobra.NoArgs,
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			p := gp
			p.OperationID = a.opID
			g, err := ops.CreateGrant(cmd.Context(), p)
			if err != nil {
				return err
			}
			return a.emit(g, func(w io.Writer) { fmt.Fprint(w, grantCreatedText(g)) })
		}),
	}
	af := add.Flags()
	af.StringVar(&gp.Action, "action", "", "the go-ahead in one word (merge)")
	af.StringVar(&gp.Option, "option", "", "the ask option (id or label) it picks")
	af.StringVar(&gp.Note, "note", "", "informational note (\"once reviewed and CI green\"); never checked")
	af.StringVar(&gp.Quote, "quote", "", "your words, saved with it")
	af.StringVar(&gp.Scope, "scope", "", "project (default with --project) or session")
	af.StringVar(&gp.Project, "project", "", "the project it holds for")
	af.StringVar(&gp.Session, "session", "", "scope session: the root session")
	af.IntVar(&gp.ExpiresDays, "expires", 0, "days until it lapses (default 7, max 30)")
	grant.AddCommand(add)

	var lp ListGrantsParams
	ls := &cobra.Command{
		Use:   "ls [--session S] [--project P] [--action A] [--all]",
		Short: "List standing grants (those holding for this session; --all for every grant, revoked and expired too)",
		Args:  cobra.NoArgs,
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			p := lp
			p.IncludeRevoked = p.All
			resp, err := ops.ListGrants(cmd.Context(), p)
			if err != nil {
				return err
			}
			return a.emit(resp, func(w io.Writer) { fmt.Fprint(w, grantsText(resp.Grants, time.Now())) })
		}),
	}
	lf := ls.Flags()
	lf.StringVar(&lp.Session, "session", "", "the grants holding for this session")
	lf.StringVar(&lp.Project, "project", "", "only this project's")
	lf.StringVar(&lp.Action, "action", "", "only this action")
	lf.BoolVar(&lp.All, "all", false, "every grant, revoked and expired ones included")
	grant.AddCommand(ls)

	var rp RevokeGrantParams
	revoke := &cobra.Command{
		Use:   "revoke <grant_id> [--reason TEXT]",
		Short: "Revoke a standing grant",
		Args:  cobra.ExactArgs(1),
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			p := rp
			p.GrantID, p.OperationID = args[0], a.opID
			g, err := ops.RevokeGrant(cmd.Context(), p)
			if err != nil {
				return err
			}
			return a.emit(g, func(w io.Writer) { fmt.Fprintln(w, grantLine(g, time.Now())) })
		}),
	}
	revoke.Flags().StringVar(&rp.Reason, "reason", "", "one line: why")
	grant.AddCommand(revoke)
	root.AddCommand(grant)

	propose := &cobra.Command{
		Use:   "propose [grant|fact]",
		Short: "Propose a standing grant or a project fact to the owner (he approves it in his inbox)",
	}
	var pg ProposeGrantParams
	pgc := &cobra.Command{
		Use:   "grant --action ACTION --option OPTION [--note TEXT] [--why TEXT] [--scope project|session] [--session S] [--expires DAYS]",
		Short: "Propose a standing grant (an owner ask; recorded only if he approves)",
		Args:  cobra.NoArgs,
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			p := pg
			p.OperationID = a.opID
			r, err := ops.ProposeGrant(cmd.Context(), p)
			if err != nil {
				return err
			}
			return a.emit(r, func(w io.Writer) { fmt.Fprint(w, proposalCreateText(r, time.Now())) })
		}),
	}
	pf := pgc.Flags()
	pf.StringVar(&pg.Action, "action", "", "the go-ahead in one word (merge)")
	pf.StringVar(&pg.Option, "option", "", "the ask option (id or label) it would pick")
	pf.StringVar(&pg.Note, "note", "", "informational note; never checked")
	pf.StringVar(&pg.Why, "why", "", "why the owner should approve it (shown as your words)")
	pf.StringVar(&pg.Scope, "scope", "", "project (default in a project) or session")
	pf.StringVar(&pg.Session, "session", "", "scope session: the root (default: this session)")
	pf.IntVar(&pg.ExpiresDays, "expires", 0, "days after approval until it lapses (default 7, max 30)")
	pf.IntVar(&pg.DeadlineMinutes, "deadline", 0, "minutes until the proposal expires unanswered (default 1440)")
	propose.AddCommand(pgc)

	var pfp ProposeFactParams
	pfc := &cobra.Command{
		Use:   "fact \"<fact>\" [--why TEXT]",
		Short: "Propose a fact for this project's fact sheet (an owner ask; added only if he approves)",
		Args:  cobra.ExactArgs(1),
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			p := pfp
			p.Fact, p.OperationID = args[0], a.opID
			r, err := ops.ProposeFact(cmd.Context(), p)
			if err != nil {
				return err
			}
			return a.emit(r, func(w io.Writer) { fmt.Fprint(w, proposalCreateText(r, time.Now())) })
		}),
	}
	pfc.Flags().StringVar(&pfp.Why, "why", "", "why the owner should approve it (shown as your words)")
	pfc.Flags().IntVar(&pfp.DeadlineMinutes, "deadline", 0, "minutes until the proposal expires unanswered (default 1440)")
	propose.AddCommand(pfc)
	root.AddCommand(propose)
}
