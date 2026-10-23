package hub

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/persistence"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

// Standing grants (W-grants, contracts/standing_grants.go): a lasting
// go-ahead from the owner that answers matching asks without asking him
// again. Only the owner creates one: directly (grants.create), or by
// approving a session's proposal (proposals.go), in which case the hub
// writes it with his answer text and the ask ID. A grant holds for every
// session of its project (scope project) or for its root session and
// everything that session starts, transitively (scope session), until it
// expires or is revoked. CreateAsk answers an ask a held grant matches
// exactly (asks_items.go: grantAsk); StartSession lists held grants in the
// launch context (grantsHeldBy).

var grantMigrations = []sqlitex.Migration{{Namespace: "hub_grants", Version: 1, Name: "standing_grants", SQL: `
CREATE TABLE standing_grants (
	grant_id TEXT PRIMARY KEY,
	scope TEXT NOT NULL, -- project | session
	project_id TEXT,
	session_id TEXT NOT NULL DEFAULT '', -- scope session: the root of the tree
	action TEXT NOT NULL,
	condition TEXT NOT NULL DEFAULT '',
	quote TEXT NOT NULL,
	recorded_by TEXT NOT NULL, -- principal JSON
	created_at TEXT NOT NULL,
	revoked_at TEXT,
	revoke_reason TEXT NOT NULL DEFAULT ''
);
CREATE INDEX standing_grants_project ON standing_grants(project_id, revoked_at);
CREATE INDEX standing_grants_session ON standing_grants(session_id, revoked_at);
CREATE INDEX standing_grants_created ON standing_grants(created_at, grant_id);
`}, {Namespace: "hub_grants", Version: 2, Name: "grant_option_expiry_provenance", SQL: `
-- v1 rows were recorded by sessions quoting the owner and have no option
-- or expiry: expires_at '' never matches, so they hold for nobody.
ALTER TABLE standing_grants ADD COLUMN option TEXT NOT NULL DEFAULT '';
ALTER TABLE standing_grants ADD COLUMN expires_at TEXT NOT NULL DEFAULT '';
ALTER TABLE standing_grants ADD COLUMN ask_id TEXT NOT NULL DEFAULT '';
ALTER TABLE standing_grants ADD COLUMN proposed_by TEXT NOT NULL DEFAULT ''; -- principal JSON, '' = the owner directly
ALTER TABLE standing_grants ADD COLUMN used_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE standing_grants ADD COLUMN last_used_at TEXT;
`}}

var _ contracts.StandingGrantsAPI = (*Hub)(nil)

// grantSystem is the principal that answers asks covered by a grant.
var grantSystem = contracts.Principal{Kind: contracts.PrincipalSystem, ID: "grants"}

// grantMaxDepth bounds the ancestor walk (a parent chain this deep, or a
// cycle, stops there).
const grantMaxDepth = 64

const grantCols = `grant_id,scope,project_id,session_id,action,condition,quote,recorded_by,created_at,revoked_at,revoke_reason,option,expires_at,ask_id,proposed_by,used_count,last_used_at`

func scanGrant(sc interface{ Scan(...any) error }) (contracts.StandingGrant, error) {
	var g contracts.StandingGrant
	var project, revoked, lastUsed sql.NullString
	var scope, by, created, expires, proposedBy string
	if err := sc.Scan(&g.GrantID, &scope, &project, &g.SessionID, &g.Action, &g.Condition, &g.Quote, &by, &created, &revoked, &g.RevokeReason,
		&g.Option, &expires, &g.AskID, &proposedBy, &g.UsedCount, &lastUsed); err != nil {
		return g, err
	}
	g.Scope = contracts.GrantScope(scope)
	g.ProjectID = persistence.ProjectPtr(project)
	_ = persistence.FromJSON(by, &g.RecordedBy)
	if proposedBy != "" {
		var pb contracts.Principal
		if persistence.FromJSON(proposedBy, &pb) == nil {
			g.ProposedBy = &pb
		}
	}
	g.CreatedAt = persistence.ParseTime(created)
	g.ExpiresAt = persistence.ParseTime(expires)
	g.RevokedAt = persistence.ParseTimePtr(revoked)
	g.LastUsedAt = persistence.ParseTimePtr(lastUsed)
	return g, nil
}

func (h *Hub) getGrant(ctx context.Context, q queryer, id string) (contracts.StandingGrant, error) {
	g, err := scanGrant(q.QueryRowContext(ctx, `SELECT `+grantCols+` FROM standing_grants WHERE grant_id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return g, contracts.Errorf(contracts.ErrNotFound, "standing grant not found").WithRef(contracts.KindStandingGrant, id)
	}
	return g, err
}

func (h *Hub) queryGrants(ctx context.Context, q queryer, where string, args ...any) ([]contracts.StandingGrant, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+grantCols+` FROM standing_grants WHERE `+where+` ORDER BY created_at, grant_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []contracts.StandingGrant{}
	for rows.Next() {
		g, err := scanGrant(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func (h *Hub) emitGrant(ctx context.Context, tx *sql.Tx, g contracts.StandingGrant) error {
	return h.emitPayload(ctx, tx, contracts.EvStandingGrantChanged, contracts.ObjectRef{Kind: contracts.KindStandingGrant, ID: g.GrantID}, g.SessionID, g.ProjectID, "hub", 0, g)
}

// NormalizeGrantAction normalizes an action slug like a topic key ("Merge
// PRs" -> "merge-prs"), at most limit bytes.
func NormalizeGrantAction(s string, limit int) string {
	out := NormalizeTopicKey(s)
	for len(out) > limit {
		_, size := utf8.DecodeLastRuneInString(out)
		out = out[:len(out)-size]
	}
	return strings.TrimRight(out, "-")
}

// ---- the deny-list ----

// grantDenyWords are words that make an action (or an ask's topic, title or
// chosen option) never grant-answerable and never grantable, with the
// class shown in the refusal. Generous on purpose: a false positive only
// means the owner is asked.
var grantDenyWords = func() map[string]string {
	m := map[string]string{}
	add := func(class string, words ...string) {
		for _, w := range words {
			m[w] = class
		}
	}
	add("production deploys", "prod", "production", "prd")
	add("deleting or dropping data", "delete", "deletes", "deleting", "drop", "dropping", "destroy", "wipe", "truncate", "purge", "erase", "rm", "rmrf")
	add("spending money", "spend", "spending", "pay", "payment", "payments", "purchase", "buy", "charge", "billing", "invoice", "refund", "subscribe", "subscription")
	add("force-push or rewriting history", "force", "forcepush", "rewrite", "rewriting", "filter-branch", "squash-main")
	add("public releases or publishing", "release", "releases", "publish", "publishing", "public", "announce", "launch")
	add("account or credential changes", "account", "accounts", "credential", "credentials", "password", "passwords", "secret", "secrets",
		"token", "tokens", "login", "logout", "oauth", "apikey", "sso", "mfa", "2fa", "permission", "permissions", "iam")
	return m
}()

// grantDenied reports the deny-listed class any of texts falls in.
func grantDenied(texts ...string) (string, bool) {
	for _, t := range texts {
		slug := NormalizeTopicKey(t)
		if slug == "" {
			continue
		}
		for _, w := range strings.Split(slug, "-") {
			if class, ok := grantDenyWords[w]; ok {
				return class, true
			}
		}
		flat := strings.ReplaceAll(slug, "-", "")
		switch {
		case strings.Contains(flat, "prod") && containsAny(flat, "deploy", "release", "ship", "rollout", "promote", "push", "migrat"):
			return "production deploys", true
		case containsAny(flat, "forcepush", "rewritehistory", "resethard", "pushforce"):
			return "force-push or rewriting history", true
		case containsAny(flat, "deletedata", "dropdata", "droptable", "dropdatabase", "dropdb"):
			return "deleting or dropping data", true
		}
	}
	return "", false
}

func containsAny(s string, subs ...string) bool {
	for _, x := range subs {
		if strings.Contains(s, x) {
			return true
		}
	}
	return false
}

// ---- which grants hold ----

// sessionChain returns the session and its ancestors, nearest first (a
// missing ancestor, a cycle or grantMaxDepth ends the walk).
func (h *Hub) sessionChain(ctx context.Context, q queryer, sessionID string) ([]contracts.Session, error) {
	var out []contracts.Session
	seen := map[string]bool{}
	for id := sessionID; id != "" && !seen[id] && len(out) < grantMaxDepth; {
		seen[id] = true
		s, err := h.getSession(ctx, q, id)
		if contracts.IsCode(err, contracts.ErrNotFound) && len(out) > 0 {
			break
		}
		if err != nil {
			return nil, err
		}
		out = append(out, s)
		id = s.Parent()
	}
	return out, nil
}

// inChain reports whether id is the session or one of its ancestors.
func (h *Hub) inChain(ctx context.Context, q queryer, sessionID, id string) (bool, error) {
	chain, err := h.sessionChain(ctx, q, sessionID)
	if err != nil {
		return false, err
	}
	for _, s := range chain {
		if s.SessionID == id {
			return true, nil
		}
	}
	return false, nil
}

// grantsFor returns the grants of a session's tree (session grants rooted
// at it or an ancestor) and of projectID (nil: the session's project);
// sessionID may be "" (project grants only). activeOnly drops revoked and
// expired grants.
func (h *Hub) grantsFor(ctx context.Context, q queryer, sessionID string, projectID *string, activeOnly bool) ([]contracts.StandingGrant, error) {
	var where []string
	var args []any
	if sessionID != "" {
		chain, err := h.sessionChain(ctx, q, sessionID)
		if err != nil {
			return nil, err
		}
		if projectID == nil && len(chain) > 0 {
			projectID = chain[0].ProjectID
		}
		ids := make([]any, 0, len(chain))
		for _, s := range chain {
			ids = append(ids, s.SessionID)
		}
		if len(ids) > 0 {
			where = append(where, `(scope='session' AND session_id IN (?`+strings.Repeat(",?", len(ids)-1)+`))`)
			args = append(args, ids...)
		}
	}
	if projectID != nil && *projectID != "" {
		where = append(where, `(scope='project' AND project_id=?)`)
		args = append(args, *projectID)
	}
	if len(where) == 0 {
		return []contracts.StandingGrant{}, nil
	}
	cond := `(` + strings.Join(where, ` OR `) + `)`
	if activeOnly {
		cond = `revoked_at IS NULL AND expires_at>? AND ` + cond
		args = append([]any{persistence.Time(h.now())}, args...)
	}
	return h.queryGrants(ctx, q, cond, args...)
}

// grantsHeldBy returns the active (unrevoked, unexpired) grants holding
// for a session: session grants whose root is the session or one of its
// ancestors, plus the project grants of projectID (nil: the session's
// project). sessionID may be "" (project grants only).
func (h *Hub) grantsHeldBy(ctx context.Context, q queryer, sessionID string, projectID *string) ([]contracts.StandingGrant, error) {
	return h.grantsFor(ctx, q, sessionID, projectID, true)
}

// grantOption finds the ask option a grant chooses: by option id, else by
// label (case-insensitive, spacing ignored).
func grantOption(opts []contracts.AskOption, want string) (contracts.AskOption, bool) {
	want = strings.Join(strings.Fields(want), " ")
	if want == "" {
		return contracts.AskOption{}, false
	}
	for _, o := range opts {
		if strings.EqualFold(o.ID, want) {
			return o, true
		}
	}
	for _, o := range opts {
		if strings.EqualFold(o.Label, want) {
			return o, true
		}
	}
	return contracts.AskOption{}, false
}

// grantForAsk returns the held grant that answers a session's ask and the
// option it chooses (rules in contracts/standing_grants.go; the topic
// checks run separately, in the transaction). Only the asker's own tree
// and project are used: an ask for another project is never answered.
func (h *Hub) grantForAsk(ctx context.Context, q queryer, asker contracts.Session, in askInput) (contracts.StandingGrant, string, bool, error) {
	none := contracts.StandingGrant{}
	switch {
	case in.Action == "", in.Proposal != nil, in.Reopen, in.Irreversible, in.DefaultOption == "":
		return none, "", false, nil
	case in.ProjectID != nil && (asker.ProjectID == nil || *in.ProjectID != *asker.ProjectID):
		return none, "", false, nil
	}
	if _, denied := grantDenied(in.Action, in.TopicKey, in.Title); denied {
		return none, "", false, nil
	}
	held, err := h.grantsHeldBy(ctx, q, asker.SessionID, asker.ProjectID)
	if err != nil {
		return none, "", false, err
	}
	for _, g := range held {
		if g.Action != in.Action {
			continue
		}
		o, ok := grantOption(in.Options, g.Option)
		if !ok {
			continue
		}
		if _, denied := grantDenied(o.ID, o.Label); denied {
			continue
		}
		return g, o.ID, true, nil
	}
	return none, "", false, nil
}

// grantExpiry validates a requested expiry (nil: the default).
func grantExpiry(now time.Time, at *time.Time) (time.Time, error) {
	if at == nil || at.IsZero() {
		return now.Add(contracts.GrantDefaultTTL), nil
	}
	if !at.After(now) || at.Sub(now) > contracts.GrantMaxTTL+time.Minute {
		return time.Time{}, contracts.Errorf(contracts.ErrInvalid, "expires_at must be in the future and at most %d days away", int(contracts.GrantMaxTTL/(24*time.Hour)))
	}
	return at.UTC(), nil
}

// checkGrantFields validates the fields every grant needs (owner create and
// approved proposals).
func checkGrantFields(action, option, condition, quote string) error {
	switch {
	case action == "":
		return contracts.Errorf(contracts.ErrInvalid, "action is required: a short slug naming the go-ahead (\"merge\"); only asks naming exactly this action are answered by the grant")
	case option == "":
		return contracts.Errorf(contracts.ErrInvalid, "option is required: the ask option (id or label) the grant chooses; an ask without that option is not answered")
	case utf8.RuneCountInString(option) > contracts.AskMaxOptionText:
		return contracts.Errorf(contracts.ErrInvalid, "option is limited to %d characters", contracts.AskMaxOptionText)
	case utf8.RuneCountInString(condition) > contracts.GrantMaxCondition:
		return contracts.Errorf(contracts.ErrInvalid, "condition is limited to %d characters", contracts.GrantMaxCondition)
	case utf8.RuneCountInString(quote) > contracts.GrantMaxQuote:
		return contracts.Errorf(contracts.ErrInvalid, "quote is limited to %d characters", contracts.GrantMaxQuote)
	}
	if class, denied := grantDenied(action, option); denied {
		return contracts.Errorf(contracts.ErrForbidden, "%q is never grantable (%s): the owner decides those one at a time", action, class)
	}
	return nil
}

// insertGrantTx stores a new grant (ID and creation time set here) and
// emits standing_grant.changed.
func (h *Hub) insertGrantTx(ctx context.Context, tx *sql.Tx, g contracts.StandingGrant) (contracts.StandingGrant, error) {
	g.GrantID = contracts.NewID(contracts.PrefixStandingGrant)
	g.CreatedAt = h.now()
	proposedBy := ""
	if g.ProposedBy != nil {
		proposedBy = persistence.JSON(*g.ProposedBy)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO standing_grants(grant_id,scope,project_id,session_id,action,condition,quote,recorded_by,created_at,option,expires_at,ask_id,proposed_by) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		g.GrantID, string(g.Scope), persistence.ProjectArg(g.ProjectID), g.SessionID, g.Action, g.Condition, g.Quote, persistence.JSON(g.RecordedBy),
		persistence.Time(g.CreatedAt), g.Option, persistence.Time(g.ExpiresAt), g.AskID, proposedBy); err != nil {
		return g, err
	}
	return g, h.emitGrant(ctx, tx, g)
}

// ---- grants.create ----

// CreateGrant implements grants.create: the owner only (sessions propose;
// proposals.go).
func (h *Hub) CreateGrant(ctx context.Context, req contracts.CreateGrantRequest) (contracts.StandingGrant, error) {
	var out contracts.StandingGrant
	p, err := principal(ctx)
	if err != nil {
		return out, err
	}
	if !isOwner(p) {
		return out, contracts.Errorf(contracts.ErrForbidden, "only the owner creates standing grants; propose one with propose_grant (`pyre propose grant`): the owner approves it in his inbox and Pyre records it with his answer")
	}
	if err := requireOp(req.Mutation); err != nil {
		return out, err
	}
	g := contracts.StandingGrant{
		Scope: contracts.GrantScope(strings.TrimSpace(string(req.Scope))), SessionID: strings.TrimSpace(req.SessionID),
		Action: NormalizeGrantAction(req.Action, contracts.GrantMaxAction), Option: strings.Join(strings.Fields(req.Option), " "),
		Condition: strings.Join(strings.Fields(req.Condition), " "), Quote: strings.TrimSpace(req.Quote),
		RecordedBy: p,
	}
	if req.ProjectID != nil && strings.TrimSpace(*req.ProjectID) != "" {
		pid := strings.TrimSpace(*req.ProjectID)
		g.ProjectID = &pid
	}
	if err := checkGrantFields(g.Action, g.Option, g.Condition, g.Quote); err != nil {
		return out, err
	}
	if g.ExpiresAt, err = grantExpiry(h.now(), req.ExpiresAt); err != nil {
		return out, err
	}
	if g.Scope == "" {
		g.Scope = contracts.GrantScopeSession
		if g.ProjectID != nil && g.SessionID == "" {
			g.Scope = contracts.GrantScopeProject
		}
	}
	switch g.Scope {
	case contracts.GrantScopeProject:
		if g.ProjectID == nil {
			return out, contracts.Errorf(contracts.ErrInvalid, "a project grant needs project_id; use scope session with session_id for a session tree")
		}
		g.SessionID = ""
		if _, err := h.GetProject(h.systemCtx(ctx), contracts.GetRequest{ID: *g.ProjectID}); err != nil && !contracts.IsCode(err, contracts.ErrUnsupported) {
			return out, err
		}
	case contracts.GrantScopeSession:
		if g.SessionID == "" {
			return out, contracts.Errorf(contracts.ErrInvalid, "a session grant needs session_id (the root of the session tree it covers)")
		}
		s, err := h.getSession(ctx, h.db, g.SessionID)
		if err != nil {
			return out, err
		}
		g.ProjectID = s.ProjectID
	default:
		return out, contracts.Errorf(contracts.ErrInvalid, "scope must be project or session")
	}
	err = h.mutate(ctx, req.OperationID, contracts.OpGrantsCreate, p, req, &out, func(tx *sql.Tx) (any, error) {
		return h.insertGrantTx(ctx, tx, g)
	})
	return out, err
}

// ---- grants.list ----

// ListGrants implements grants.list: the owner lists anything; a session
// only its own project's and session tree's grants (default: those holding
// for itself).
func (h *Hub) ListGrants(ctx context.Context, req contracts.ListGrantsRequest) (contracts.ListGrantsResponse, error) {
	out := contracts.ListGrantsResponse{Grants: []contracts.StandingGrant{}}
	p, err := principal(ctx)
	if err != nil {
		return out, err
	}
	if req.ProjectID != nil && strings.TrimSpace(*req.ProjectID) == "" {
		req.ProjectID = nil
	}
	sid := strings.TrimSpace(req.SessionID)
	if !isOwner(p) {
		if p.Kind != contracts.PrincipalSession {
			return out, contracts.Errorf(contracts.ErrForbidden, "only the owner and sessions list standing grants")
		}
		caller, err := h.getSession(ctx, h.db, p.ID)
		if err != nil {
			return out, err
		}
		if req.ProjectID != nil && !sameProject(req.ProjectID, caller.ProjectID) {
			return out, contracts.Errorf(contracts.ErrForbidden, "a session lists only its own project's grants")
		}
		if sid != "" && sid != caller.SessionID {
			ok, err := h.relatedSession(ctx, caller, sid)
			if err != nil {
				return out, err
			}
			if !ok {
				return out, contracts.Errorf(contracts.ErrForbidden, "a session lists grants only for sessions in its own project or session tree").WithRef(contracts.KindSession, sid)
			}
		}
		if sid == "" && req.ProjectID == nil {
			sid = caller.SessionID
		}
	}
	action := NormalizeGrantAction(req.Action, contracts.GrantMaxAction)
	var grants []contracts.StandingGrant
	if sid != "" {
		grants, err = h.grantsFor(ctx, h.db, sid, req.ProjectID, !req.IncludeRevoked)
	} else {
		where, args := []string{"1=1"}, []any{}
		if !req.IncludeRevoked {
			where = append(where, `revoked_at IS NULL AND expires_at>?`)
			args = append(args, persistence.Time(h.now()))
		}
		if req.ProjectID != nil {
			where = append(where, `project_id=?`)
			args = append(args, *req.ProjectID)
		}
		grants, err = h.queryGrants(ctx, h.db, strings.Join(where, ` AND `), args...)
	}
	if err != nil {
		return out, err
	}
	for _, g := range grants {
		if action == "" || g.Action == action {
			out.Grants = append(out.Grants, g)
		}
	}
	return out, nil
}

func sameProject(a, b *string) bool {
	return a != nil && b != nil && *a != "" && *a == *b
}

// relatedSession reports whether id is in caller's project or session tree
// (an ancestor or a descendant of caller).
func (h *Hub) relatedSession(ctx context.Context, caller contracts.Session, id string) (bool, error) {
	target, err := h.getSession(ctx, h.db, id)
	if err != nil {
		return false, err
	}
	if sameProject(target.ProjectID, caller.ProjectID) {
		return true, nil
	}
	if ok, err := h.inChain(ctx, h.db, target.SessionID, caller.SessionID); ok || err != nil {
		return ok, err
	}
	return h.inChain(ctx, h.db, caller.SessionID, target.SessionID)
}

// ---- grants.revoke ----

// RevokeGrant implements grants.revoke: the owner, or a session the grant
// holds for (revoking only narrows what sessions may do).
func (h *Hub) RevokeGrant(ctx context.Context, req contracts.RevokeGrantRequest) (contracts.StandingGrant, error) {
	var out contracts.StandingGrant
	p, err := principal(ctx)
	if err != nil {
		return out, err
	}
	if err := requireOp(req.Mutation); err != nil {
		return out, err
	}
	err = h.mutate(ctx, req.OperationID, contracts.OpGrantsRevoke, p, req, &out, func(tx *sql.Tx) (any, error) {
		g, err := h.getGrant(ctx, tx, strings.TrimSpace(req.GrantID))
		if err != nil {
			return nil, err
		}
		if !isOwner(p) {
			ok := false
			if p.Kind == contracts.PrincipalSession {
				if ok, err = h.grantHoldsFor(ctx, tx, g, p.ID); err != nil {
					return nil, err
				}
			}
			if !ok {
				return nil, contracts.Errorf(contracts.ErrForbidden, "only the owner or a session the grant holds for revokes a standing grant").WithRef(contracts.KindStandingGrant, g.GrantID)
			}
		}
		if g.RevokedAt != nil {
			return nil, contracts.Errorf(contracts.ErrConflict, "standing grant %s was already revoked", g.GrantID).WithRef(contracts.KindStandingGrant, g.GrantID)
		}
		now := h.now()
		if _, err := tx.ExecContext(ctx, `UPDATE standing_grants SET revoked_at=?, revoke_reason=? WHERE grant_id=?`,
			persistence.Time(now), strings.TrimSpace(req.Reason), g.GrantID); err != nil {
			return nil, err
		}
		g, err = h.getGrant(ctx, tx, g.GrantID)
		if err != nil {
			return nil, err
		}
		return g, h.emitGrant(ctx, tx, g)
	})
	return out, err
}

// grantHoldsFor reports whether g's scope covers sessionID.
func (h *Hub) grantHoldsFor(ctx context.Context, q queryer, g contracts.StandingGrant, sessionID string) (bool, error) {
	if g.Scope == contracts.GrantScopeSession {
		return h.inChain(ctx, q, sessionID, g.SessionID)
	}
	s, err := h.getSession(ctx, q, sessionID)
	if err != nil {
		return false, err
	}
	return sameProject(s.ProjectID, g.ProjectID), nil
}

// ---- rendering ----

// grantProvenance says where a grant came from, quoting the owner only
// with his own words: `owner: "<his words>" (approved ask_…)`, `owner
// approved proposal ask_… (no words typed)`, `set by the owner: "<his
// words>"` or `set by the owner`.
func grantProvenance(g contracts.StandingGrant) string {
	quote := oneLine(g.Quote, 300)
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

// grantText is how an answer by a grant reads.
func grantText(g contracts.StandingGrant) string {
	return fmt.Sprintf("standing grant %s (%s → option %q; %s; expires %s)", g.GrantID, g.Action, g.Option, grantProvenance(g), askClock(g.ExpiresAt))
}
