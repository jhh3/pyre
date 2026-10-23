package hub

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/audit"
	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/persistence"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

// Audit log (W-audit): an owner-facing record of what Pyre did on its own
// and of fleet-state changes (contracts/audit.go).
//
// Where the facts come from:
//   - the hub's own mutations and node-report ingestion add an entry in the
//     same transaction as the change (they know the actor, the previous
//     value and the reason: session start/stop/rename, account choice and
//     switch, tokens, folds, logins, cooldowns);
//   - events appended by other packages (fleet enrollment/connection,
//     update results, node automation) are mapped by an event-log append
//     hook, also inside the appending transaction.
//
// The event log itself keeps only a week and full object payloads; the
// audit table keeps compact rows (IDs, short reasons; no secrets, message
// bodies or transcripts) for AuditRetention (90 days / 100k rows). Display
// names are resolved when listed.

var auditMigrations = []sqlitex.Migration{{Namespace: "hub_audit", Version: 1, Name: "audit_log", SQL: `
CREATE TABLE audit_log (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	at TEXT NOT NULL,
	kind TEXT NOT NULL,
	action TEXT NOT NULL,
	automatic INTEGER NOT NULL DEFAULT 0,
	actor_kind TEXT NOT NULL,
	actor_id TEXT,
	actor_name TEXT,
	node_id TEXT,
	session_id TEXT,
	run_id TEXT,
	account_id TEXT,
	from_val TEXT,
	to_val TEXT,
	from_account TEXT,
	to_account TEXT,
	from_note TEXT,
	to_note TEXT,
	reason TEXT,
	dedupe TEXT UNIQUE
);
CREATE INDEX audit_at ON audit_log(at);
CREATE INDEX audit_kind ON audit_log(kind, id);
CREATE INDEX audit_session ON audit_log(session_id, id);
CREATE INDEX audit_node ON audit_log(node_id, id);
CREATE INDEX audit_account ON audit_log(account_id, id);
`}}

// AuditRetention bounds the audit log. Zero values disable that bound.
type AuditRetention struct {
	MaxRows int
	MaxAge  time.Duration
}

// DefaultAuditRetention keeps 90 days or 100k entries, whichever is smaller.
var DefaultAuditRetention = AuditRetention{MaxRows: 100_000, MaxAge: 90 * 24 * time.Hour}

// auditRec is one entry to record (IDs only; names are resolved on read).
type auditRec struct {
	Action    string
	Automatic bool
	Actor     contracts.AuditActor
	At        time.Time
	NodeID    string
	SessionID string
	RunID     string
	AccountID string
	From, To  string
	FromAcct  string
	ToAcct    string
	FromNote  string
	ToNote    string
	Reason    string
	Dedupe    string // optional: a second entry with the same key is dropped
}

var pyreActor = contracts.AuditActor{Kind: "pyre"}

// actorOf maps an authenticated principal to an audit actor.
func actorOf(p contracts.Principal) contracts.AuditActor {
	switch {
	case isOwner(p):
		name := p.Name
		if p.OnBehalfOf != nil && p.OnBehalfOf.Name != "" {
			name = p.OnBehalfOf.Name
		}
		return contracts.AuditActor{Kind: "owner", ID: p.ID, Name: name}
	case p.Kind == contracts.PrincipalSession:
		return contracts.AuditActor{Kind: "session", ID: p.ID}
	case p.Kind == contracts.PrincipalNode:
		return contracts.AuditActor{Kind: "node", ID: p.ID}
	}
	return pyreActor
}

// ctxActor is the request's actor (Pyre itself when unauthenticated or
// hub-internal).
func ctxActor(ctx context.Context) contracts.AuditActor {
	if p, ok := contracts.PrincipalFrom(ctx); ok && p.Kind != "" {
		return actorOf(p)
	}
	return pyreActor
}

// maxAuditReason bounds stored reasons (they are explanations, not logs).
const maxAuditReason = 300

func clipReason(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > maxAuditReason {
		s = string(r[:maxAuditReason-1]) + "…"
	}
	return s
}

// auditTx records r inside tx. Best effort: an audit failure never fails
// the change it describes (SQLite rolls back only the failed statement).
func (h *Hub) auditTx(ctx context.Context, q interface {
	ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error)
}, r auditRec) {
	if r.At.IsZero() {
		r.At = h.now()
	}
	if r.Actor.Kind == "" {
		r.Actor = pyreActor
	}
	if why := ownerActionReason(ctx); why != "" { // owneractions.go: done on the owner's approval
		r.Reason = strings.TrimPrefix(r.Reason+"; "+why, "; ")
	}
	_, _ = q.ExecContext(ctx, `INSERT INTO audit_log(at,kind,action,automatic,actor_kind,actor_id,actor_name,node_id,session_id,run_id,account_id,
		from_val,to_val,from_account,to_account,from_note,to_note,reason,dedupe) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(dedupe) DO NOTHING`,
		persistence.Time(r.At), string(audit.KindOf(r.Action)), r.Action, boolInt(r.Automatic), r.Actor.Kind,
		persistence.NullStr(r.Actor.ID), persistence.NullStr(r.Actor.Name), persistence.NullStr(r.NodeID), persistence.NullStr(r.SessionID),
		persistence.NullStr(r.RunID), persistence.NullStr(r.AccountID), persistence.NullStr(r.From), persistence.NullStr(r.To),
		persistence.NullStr(r.FromAcct), persistence.NullStr(r.ToAcct), persistence.NullStr(r.FromNote), persistence.NullStr(r.ToNote),
		persistence.NullStr(clipReason(r.Reason)), persistence.NullStr(r.Dedupe))
}

// nodeOfOrigin extracts the node of an event origin ("node:<id>").
func nodeOfOrigin(origin string) string {
	return strings.TrimPrefix(origin, "node:")
}

// auditEvent is the event-log append hook: it maps events appended by
// other packages (and node-reported events) to audit entries.
func (h *Hub) auditEvent(ctx context.Context, tx *sql.Tx, ev contracts.Event) {
	switch ev.Kind {
	case contracts.EvNodeEnrolled, contracts.EvNodeRevoked:
		a := contracts.AuditNodeEnrolled
		if ev.Kind == contracts.EvNodeRevoked {
			a = contracts.AuditNodeRevoked
		}
		act := ctxActor(ctx)
		if act.Kind != "owner" {
			act = pyreActor
		}
		h.auditTx(ctx, tx, auditRec{Action: a, Actor: act, NodeID: ev.Object.ID, At: ev.OccurredAt})
	case contracts.EvNodeConnection:
		var c contracts.NodeConnection
		if json.Unmarshal(ev.Payload, &c) != nil {
			return
		}
		h.auditConnectionTx(ctx, tx, ev.Object.ID, c.State)
	case contracts.EvUpdateResult:
		var u struct {
			NodeID     string `json:"node_id"`
			Tool       string `json:"tool"`
			Version    string `json:"version"`
			Outcome    string `json:"outcome"`
			RevertedTo string `json:"reverted_to"`
			Detail     string `json:"detail"`
		}
		if json.Unmarshal(ev.Payload, &u) != nil {
			return
		}
		switch u.Outcome {
		case "failed", "failed_reverted", "rolled_back", "refused", "stage_failed":
		default:
			return // success shows up as a version change on the next hello
		}
		node := u.NodeID
		if node == "" {
			node = nodeOfOrigin(ev.Origin)
		}
		to := u.Version
		if u.Tool != "" {
			to = u.Tool + " " + u.Version
		}
		reason := strings.ReplaceAll(u.Outcome, "_", " ")
		if u.RevertedTo != "" {
			reason += "; back on " + u.RevertedTo
		}
		if u.Detail != "" {
			reason += ": " + u.Detail
		}
		h.auditTx(ctx, tx, auditRec{Action: contracts.AuditNodeUpdateFailed, Automatic: true, NodeID: node, To: to, Reason: reason, At: ev.OccurredAt})
	case contracts.EvNodeAutomation:
		var a contracts.NodeAutomation
		if json.Unmarshal(ev.Payload, &a) != nil {
			return
		}
		action := ""
		switch a.Action {
		case contracts.AutomationTrustAccepted:
			action = contracts.AuditNodeTrustAccepted
		case contracts.AutomationExitConfirmed:
			action = contracts.AuditNodeExitConfirmed
		case contracts.AutomationCapacityRetry:
			action = contracts.AuditNodeCapacityRetry
		case contracts.AutomationDevChannelsAccepted:
			action = contracts.AuditNodeDevChannelsAccepted
		case contracts.AutomationBackgroundWake:
			action = contracts.AuditNodeBackgroundWake
		case contracts.AutomationOnboardingAnswered:
			action = contracts.AuditNodeOnboardingAnswered
		default:
			return
		}
		node := nodeOfOrigin(ev.Origin)
		sid := a.SessionID
		if a.RunID != "" {
			if run, err := h.getRun(ctx, tx, a.RunID); err == nil {
				if run.NodeID != node && strings.HasPrefix(ev.Origin, "node:") {
					return // another node's run
				}
				sid = run.SessionID
			}
		}
		if sid == "" {
			sid = ev.SessionID
		}
		h.auditTx(ctx, tx, auditRec{Action: action, Automatic: true, NodeID: node, SessionID: sid, RunID: a.RunID, Reason: a.Detail,
			At: ev.OccurredAt, Dedupe: action + ":" + a.RunID})
	}
}

// auditConnectionTx records connected/disconnected transitions only (a
// repeated state, e.g. from both the fleet registry and the hub, is one
// entry).
func (h *Hub) auditConnectionTx(ctx context.Context, tx *sql.Tx, nodeID string, st contracts.ConnectionState) {
	action := ""
	switch st {
	case contracts.ConnConnected:
		action = contracts.AuditNodeConnected
	case contracts.ConnDisconnected:
		action = contracts.AuditNodeDisconnected
	default:
		return
	}
	var last string
	_ = tx.QueryRowContext(ctx, `SELECT action FROM audit_log WHERE node_id=? AND action IN (?,?) ORDER BY id DESC LIMIT 1`,
		nodeID, contracts.AuditNodeConnected, contracts.AuditNodeDisconnected).Scan(&last)
	if last == action {
		return
	}
	r := auditRec{Action: action, Actor: contracts.AuditActor{Kind: "node", ID: nodeID}, NodeID: nodeID}
	if action == contracts.AuditNodeConnected {
		r.To = h.helloVersion(ctx, tx, nodeID)
	}
	h.auditTx(ctx, tx, r)
}

// auditConnectedVersionTx sets the pyred version on the node's newest
// connection entry when it is a "connected" one.
func (h *Hub) auditConnectedVersionTx(ctx context.Context, tx *sql.Tx, nodeID, version string) {
	if version == "" {
		return
	}
	_, _ = tx.ExecContext(ctx, `UPDATE audit_log SET to_val=? WHERE action=? AND id=(SELECT MAX(id) FROM audit_log WHERE node_id=? AND action IN (?,?))`,
		version, contracts.AuditNodeConnected, nodeID, contracts.AuditNodeConnected, contracts.AuditNodeDisconnected)
}

// helloVersion is the pyred version from a node's stored hello.
func (h *Hub) helloVersion(ctx context.Context, q queryer, nodeID string) string {
	var hello sql.NullString
	_ = q.QueryRowContext(ctx, `SELECT hello FROM node_state WHERE node_id=?`, nodeID).Scan(&hello)
	if !hello.Valid {
		return ""
	}
	var nh contracts.NodeHello
	if json.Unmarshal([]byte(hello.String), &nh) != nil {
		return ""
	}
	return nh.PyredVersion
}

// auditHubVersion records a pyre-hub version change at start-up.
func (h *Hub) auditHubVersion(ctx context.Context) error {
	return sqlitex.Tx(ctx, h.db, func(tx *sql.Tx) error {
		var prev string
		err := tx.QueryRowContext(ctx, `SELECT value FROM hub_meta WHERE key='hub_version'`).Scan(&prev)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		cur := h.opt.HubVersion
		if prev == cur {
			return nil
		}
		if prev != "" {
			h.auditTx(ctx, tx, auditRec{Action: contracts.AuditHubUpgraded, From: prev, To: cur})
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO hub_meta(key,value) VALUES('hub_version',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, cur)
		return err
	})
}

// usageNoteTx is the dominant usage window of an account's newest
// observation from nodeID (any node when nodeID is "" or has none).
func (h *Hub) usageNoteTx(ctx context.Context, q queryer, accountID, nodeID string) string {
	if accountID == "" {
		return ""
	}
	var body string
	err := q.QueryRowContext(ctx, `SELECT observation FROM account_observations WHERE account_id=? AND reporting_node=?`, accountID, nodeID).Scan(&body)
	if err != nil {
		if err := q.QueryRowContext(ctx, `SELECT observation FROM account_observations WHERE account_id=? ORDER BY observed_at DESC LIMIT 1`, accountID).Scan(&body); err != nil {
			return ""
		}
	}
	var o contracts.AccountObservation
	if json.Unmarshal([]byte(body), &o) != nil {
		return ""
	}
	return audit.UsageNote(&o)
}

// accountLimitedTx reports whether any machine's newest observation of the
// account says it is limited (cooldown or exhausted).
func (h *Hub) accountLimitedTx(ctx context.Context, q queryer, accountID string) bool {
	var n int
	_ = q.QueryRowContext(ctx, `SELECT COUNT(*) FROM account_observations WHERE account_id=? AND json_extract(observation,'$.eligibility') IN (?,?)`,
		accountID, string(contracts.EligibleCooldown), string(contracts.EligibleExhausted)).Scan(&n)
	return n > 0
}

// auditAccountEditTx records owner edits of an account (rename,
// disable/enable, tier) by comparing before and after.
func (h *Hub) auditAccountEditTx(ctx context.Context, tx *sql.Tx, act contracts.AuditActor, before *contracts.Account, after contracts.Account) {
	if before == nil {
		h.auditTx(ctx, tx, auditRec{Action: contracts.AuditAccountAdded, Actor: act, AccountID: after.AccountID, Reason: string(after.Provider)})
		return
	}
	if before.Label != after.Label {
		h.auditTx(ctx, tx, auditRec{Action: contracts.AuditAccountRenamed, Actor: act, AccountID: after.AccountID, From: before.Label, To: after.Label})
	}
	if before.Disabled != after.Disabled {
		a := contracts.AuditAccountEnabled
		if after.Disabled {
			a = contracts.AuditAccountDisabled
		}
		h.auditTx(ctx, tx, auditRec{Action: a, Actor: act, AccountID: after.AccountID})
	}
	if before.TierName() != after.TierName() {
		h.auditTx(ctx, tx, auditRec{Action: contracts.AuditAccountTierChanged, Actor: act, AccountID: after.AccountID,
			From: string(before.TierName()), To: string(after.TierName())})
	}
}

// ---- reading ----

var _ contracts.AuditAPI = (*Hub)(nil)

// ListAudit lists audit entries newest first with display names resolved.
func (h *Hub) ListAudit(ctx context.Context, req contracts.ListAuditRequest) (contracts.Page[contracts.AuditEntry], error) {
	page := contracts.Page[contracts.AuditEntry]{Items: []contracts.AuditEntry{}, SnapshotAt: h.now()}
	if _, err := principal(ctx); err != nil {
		return page, err
	}
	pc, err := persistence.DecodeCursor(req.Cursor)
	if err != nil {
		return page, err
	}
	limit := persistence.ClampLimit(req.Limit, persistence.DefaultLimit, 500)
	where := []string{"1=1"}
	var args []any
	if pc.N > 0 {
		where = append(where, "id < ?")
		args = append(args, pc.N)
	}
	if req.Since != nil {
		where = append(where, "at >= ?")
		args = append(args, persistence.Time(*req.Since))
	}
	if req.Until != nil {
		where = append(where, "at <= ?")
		args = append(args, persistence.Time(*req.Until))
	}
	if len(req.Kinds) > 0 {
		ph := make([]string, len(req.Kinds))
		for i, k := range req.Kinds {
			if k != contracts.AuditAccount && k != contracts.AuditSession && k != contracts.AuditFleet {
				return page, contracts.Errorf(contracts.ErrInvalid, "unknown audit kind %q", k)
			}
			ph[i] = "?"
			args = append(args, string(k))
		}
		where = append(where, "kind IN ("+strings.Join(ph, ",")+")")
	}
	if req.SessionID != "" {
		where = append(where, "session_id = ?")
		args = append(args, req.SessionID)
	}
	if req.NodeID != "" {
		where = append(where, "node_id = ?")
		args = append(args, req.NodeID)
	}
	if req.AccountID != "" {
		id := h.resolveAccountAlias(ctx, h.db, req.AccountID)
		where = append(where, "(account_id = ? OR from_account = ? OR to_account = ? OR account_id = ?)")
		args = append(args, id, id, id, req.AccountID)
	}
	args = append(args, limit+1)
	rows, err := h.db.QueryContext(ctx, `SELECT id,at,kind,action,automatic,actor_kind,actor_id,actor_name,node_id,session_id,run_id,account_id,
		from_val,to_val,from_account,to_account,from_note,to_note,reason FROM audit_log WHERE `+strings.Join(where, " AND ")+` ORDER BY id DESC LIMIT ?`, args...)
	if err != nil {
		return page, err
	}
	var items []contracts.AuditEntry
	for rows.Next() {
		var e contracts.AuditEntry
		var at, kind string
		var auto int
		var aid, aname, node, sess, run, acct, from, to, fromA, toA, fromN, toN, reason sql.NullString
		if err := rows.Scan(&e.AuditID, &at, &kind, &e.Action, &auto, &e.Actor.Kind, &aid, &aname, &node, &sess, &run, &acct,
			&from, &to, &fromA, &toA, &fromN, &toN, &reason); err != nil {
			rows.Close()
			return page, err
		}
		e.At, e.Kind, e.Automatic = persistence.ParseTime(at), contracts.AuditKind(kind), auto != 0
		e.Actor.ID, e.Actor.Name = aid.String, aname.String
		e.NodeID, e.SessionID, e.RunID, e.AccountID = node.String, sess.String, run.String, acct.String
		e.From, e.To, e.FromAccountID, e.ToAccountID = from.String, to.String, fromA.String, toA.String
		e.FromNote, e.ToNote, e.Reason = fromN.String, toN.String, reason.String
		items = append(items, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return page, err
	}
	if len(items) > limit {
		items = items[:limit]
		page.NextCursor = persistence.EncodeCursor(persistence.PageCursor{N: items[limit-1].AuditID})
	}
	h.resolveAudit(ctx, items)
	page.Items = append(page.Items, items...)
	return page, nil
}

// resolveAudit fills display names (current node names, session titles,
// account labels) and the Summary.
func (h *Hub) resolveAudit(ctx context.Context, items []contracts.AuditEntry) {
	nodes := map[string]string{}
	titles := map[string]string{}
	labels := map[string]string{}
	nodeName := func(id string) string {
		if id == "" {
			return ""
		}
		if n, ok := nodes[id]; ok {
			return n
		}
		n := h.nodeName(ctx, h.db, id, true)
		nodes[id] = n
		return n
	}
	title := func(id string) string {
		if id == "" {
			return ""
		}
		if t, ok := titles[id]; ok {
			return t
		}
		t := ""
		if s, err := h.getSession(ctx, h.db, id); err == nil {
			t = s.Title
		}
		titles[id] = t
		return t
	}
	label := func(id string) string {
		if id == "" {
			return ""
		}
		if l, ok := labels[id]; ok {
			return l
		}
		l := ""
		if a, err := h.getAccount(ctx, h.db, h.resolveAccountAlias(ctx, h.db, id)); err == nil {
			l = a.Label
		}
		labels[id] = l
		return l
	}
	for i := range items {
		e := &items[i]
		e.NodeName = nodeName(e.NodeID)
		e.SessionTitle = title(e.SessionID)
		if e.AccountID != "" {
			e.AccountLabel = label(e.AccountID)
		}
		if e.From == "" && e.FromAccountID != "" {
			e.From = label(e.FromAccountID)
		}
		if e.To == "" && e.ToAccountID != "" {
			e.To = label(e.ToAccountID)
		}
		switch e.Actor.Kind {
		case "session":
			if e.Actor.Name == "" {
				e.Actor.Name = title(e.Actor.ID)
			}
		}
		e.Summary = audit.Summarize(*e)
	}
}

// pruneAudit applies AuditRetention (called from pruneHistory).
func (h *Hub) pruneAudit(ctx context.Context) error {
	_, err := h.pruneAuditN(ctx)
	return err
}

// pruneAuditN applies the audit retention in short write transactions
// (sqlitex.Batched, W-dbhygiene) and returns how many entries it deleted.
func (h *Hub) pruneAuditN(ctx context.Context) (int64, error) {
	ret := DefaultAuditRetention
	if h.opt.AuditRetention != nil {
		ret = *h.opt.AuditRetention
	}
	var total int64
	if ret.MaxAge > 0 {
		cut := persistence.Time(h.clock.Now().Add(-ret.MaxAge))
		n, err := sqlitex.Batched(ctx, h.db, sqlitex.PruneBatch, 0, func(tx *sql.Tx, limit int) (int64, error) {
			res, err := tx.ExecContext(ctx, `DELETE FROM audit_log WHERE id IN (SELECT id FROM audit_log WHERE at < ? ORDER BY id LIMIT ?)`, cut, limit)
			if err != nil {
				return 0, err
			}
			return res.RowsAffected()
		})
		total += n
		if err != nil {
			return total, err
		}
	}
	if ret.MaxRows > 0 {
		var maxID sql.NullInt64
		if err := h.db.QueryRowContext(ctx, `SELECT MAX(id) FROM audit_log`).Scan(&maxID); err != nil {
			return total, err
		}
		if maxID.Valid && maxID.Int64 > int64(ret.MaxRows) {
			through := maxID.Int64 - int64(ret.MaxRows)
			n, err := sqlitex.Batched(ctx, h.db, sqlitex.PruneBatch, 0, func(tx *sql.Tx, limit int) (int64, error) {
				res, err := tx.ExecContext(ctx, `DELETE FROM audit_log WHERE id IN (SELECT id FROM audit_log WHERE id <= ? ORDER BY id LIMIT ?)`, through, limit)
				if err != nil {
					return 0, err
				}
				return res.RowsAffected()
			})
			total += n
			if err != nil {
				return total, err
			}
		}
	}
	return total, nil
}

// auditGrantTx records a machine's new login for an account and login
// state changes that need (or no longer need) the owner.
func (h *Hub) auditGrantTx(ctx context.Context, tx *sql.Tx, nodeID, prevJSON string, g contracts.DeviceGrant) {
	if prevJSON == "" {
		how := g.Mechanism
		if how == "" {
			how = string(g.Kind)
		}
		h.auditTx(ctx, tx, auditRec{Action: contracts.AuditAccountLoginAdded, Automatic: true, NodeID: nodeID, AccountID: g.AccountID, Reason: how,
			Dedupe: "login:" + g.GrantID})
		return
	}
	var prev contracts.DeviceGrant
	if json.Unmarshal([]byte(prevJSON), &prev) != nil || prev.State == g.State {
		return
	}
	bad := func(s contracts.GrantState) bool { return s == contracts.GrantExpired || s == contracts.GrantDead }
	switch {
	case bad(g.State) && !bad(prev.State):
		reason := g.Detail
		if reason == "" {
			reason = "login " + string(g.State)
		}
		h.auditTx(ctx, tx, auditRec{Action: contracts.AuditAccountReauthNeeded, Automatic: true, NodeID: nodeID, AccountID: g.AccountID, Reason: reason})
	case bad(prev.State) && (g.State == contracts.GrantValid || g.State == contracts.GrantExpiring):
		h.auditTx(ctx, tx, auditRec{Action: contracts.AuditAccountReauthed, Automatic: true, NodeID: nodeID, AccountID: g.AccountID})
	}
}

// auditFallbackTx records an automatic account switch (or that no account
// was eligible) from a node's fallback plan.
func (h *Hub) auditFallbackTx(ctx context.Context, tx *sql.Tx, nodeID string, f contracts.FallbackPlan) {
	r := auditRec{Automatic: true, At: f.PlannedAt, NodeID: nodeID, SessionID: f.SessionID, RunID: f.FromRunID, AccountID: f.FromAccountID,
		FromAcct: f.FromAccountID, FromNote: h.usageNoteTx(ctx, tx, f.FromAccountID, nodeID), Reason: f.Reason,
		Dedupe: "fallback:" + f.SessionID + ":" + f.FromRunID}
	switch f.Outcome {
	case contracts.FallbackNativeResume, contracts.FallbackHandoff:
		r.Action, r.ToAcct = contracts.AuditAccountSwitched, f.ToAccountID
		r.ToNote = h.usageNoteTx(ctx, tx, f.ToAccountID, nodeID)
	default:
		r.Action = contracts.AuditAccountWaiting
		if strings.HasPrefix(strings.ToLower(f.Reason), "account switching paused") { // accounts.SwitchPausedPrefix
			r.To = "switching paused"
		} else if f.Outcome == contracts.FallbackIntervention {
			r.To = "needs you"
		} else {
			r.To = "waiting for a reset"
		}
	}
	h.auditTx(ctx, tx, r)
}
