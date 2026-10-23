package hub

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jhh3/pyre/internal/audit"
	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/persistence"
)

// Accounts: the hub stores subscription identities (owner-managed) and
// aggregates node-reported observations, device grants, assignments and
// fallback plans. Credentials never reach the hub.

func (h *Hub) getAccount(ctx context.Context, q queryer, id string) (contracts.Account, error) {
	var body string
	err := q.QueryRowContext(ctx, `SELECT account FROM accounts WHERE account_id=?`, id).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return contracts.Account{}, contracts.Errorf(contracts.ErrNotFound, "account not found").WithRef(contracts.KindAccount, id)
	}
	if err != nil {
		return contracts.Account{}, err
	}
	var a contracts.Account
	return a, json.Unmarshal([]byte(body), &a)
}

// putAccountTx upserts an account and emits account.changed (payload
// Account) in the same transaction.
func (h *Hub) putAccountTx(ctx context.Context, tx *sql.Tx, a contracts.Account) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO accounts(account_id,provider,account,created_at) VALUES(?,?,?,?)
		ON CONFLICT(account_id) DO UPDATE SET provider=excluded.provider, account=excluded.account`,
		a.AccountID, string(a.Provider), persistence.JSON(a), persistence.Time(a.CreatedAt)); err != nil {
		return err
	}
	return h.emitPayload(ctx, tx, contracts.EvAccountChanged, contracts.ObjectRef{Kind: contracts.KindAccount, ID: a.AccountID}, "", nil, "hub", 0, a)
}

// UpsertAccount creates or edits a subscription account (owner only).
func (h *Hub) UpsertAccount(ctx context.Context, req contracts.UpsertAccountRequest) (contracts.Account, error) {
	p, err := principal(ctx)
	if err != nil {
		return contracts.Account{}, err
	}
	if !isOwner(p) {
		return contracts.Account{}, contracts.Errorf(contracts.ErrForbidden, "only the owner manages accounts")
	}
	if err := requireOp(req.Mutation); err != nil {
		return contracts.Account{}, err
	}
	if contracts.HarnessForProvider(req.Provider) == "" {
		return contracts.Account{}, contracts.Errorf(contracts.ErrInvalid, "unknown provider %q", req.Provider)
	}
	if strings.TrimSpace(req.Label) == "" {
		return contracts.Account{}, contracts.Errorf(contracts.ErrInvalid, "label is required")
	}
	var out contracts.Account
	err = h.mutate(ctx, req.OperationID, contracts.OpUpsertAccount, p, req, &out, func(tx *sql.Tx) (any, error) {
		var a contracts.Account
		var before *contracts.Account
		if req.AccountID == "" {
			a = contracts.Account{AccountID: contracts.NewID(contracts.PrefixAccount), Kind: "subscription", CreatedAt: h.now()}
		} else {
			if !contracts.ValidID(req.AccountID, contracts.PrefixAccount) {
				return nil, contracts.Errorf(contracts.ErrInvalid, "account_id %q is malformed", req.AccountID)
			}
			existing, err := h.getAccount(ctx, tx, req.AccountID)
			switch {
			case err == nil:
				if existing.Provider != req.Provider {
					return nil, contracts.Errorf(contracts.ErrConflict, "account %s is a %s account; provider cannot change", req.AccountID, existing.Provider)
				}
				a = existing
				before = &existing
			case contracts.IsCode(err, contracts.ErrNotFound):
				a = contracts.Account{AccountID: req.AccountID, Kind: "subscription", CreatedAt: h.now()}
			default:
				return nil, err
			}
		}
		a.Provider, a.Label, a.Disabled, a.RotaRef = req.Provider, req.Label, req.Disabled, req.RotaRef
		a.PerMachineIndependent = req.PerMachineIndependent
		if req.ProviderRef != "" {
			if a.ProviderRef != "" && a.ProviderRef != req.ProviderRef {
				return nil, contracts.Errorf(contracts.ErrConflict, "account %s is bound to another provider identity; it cannot be rebound", a.AccountID).WithRef(contracts.KindAccount, a.AccountID)
			}
			a.ProviderRef = req.ProviderRef
		}
		h.auditAccountEditTx(ctx, tx, actorOf(p), before, a)
		return a, h.putAccountTx(ctx, tx, a) // emits account.changed
	})
	return out, err
}

// ensureAccountTx records an account first seen in a node report.
func (h *Hub) ensureAccountTx(ctx context.Context, tx *sql.Tx, id string, prov contracts.Provider, at contracts.Freshness) error {
	if !contracts.ValidID(id, contracts.PrefixAccount) {
		return contracts.Errorf(contracts.ErrInvalid, "account_id %q is malformed", id)
	}
	if h.isAccountAlias(ctx, tx, id) {
		return nil // folded into another account (accounts_merge.go); never re-created
	}
	if _, err := h.getAccount(ctx, tx, id); err == nil {
		return nil
	} else if !contracts.IsCode(err, contracts.ErrNotFound) {
		return err
	}
	if prov == "" {
		return contracts.Errorf(contracts.ErrInvalid, "unknown account %s and no provider reported", id)
	}
	created := at.ObservedAt
	if created.IsZero() {
		created = h.now()
	}
	h.auditTx(ctx, tx, auditRec{Action: contracts.AuditAccountAdded, Automatic: true, AccountID: id, Reason: string(prov) + ", first reported by a machine"})
	return h.putAccountTx(ctx, tx, contracts.Account{AccountID: id, Provider: prov, Label: id, Kind: "subscription", CreatedAt: created})
}

func (h *Hub) ingestObservationTx(ctx context.Context, tx *sql.Tx, nodeID string, o contracts.AccountObservation, seq uint64) error {
	if o.ReportingNode == "" {
		o.ReportingNode = nodeID
	}
	if o.ReportingNode != nodeID {
		return contracts.Errorf(contracts.ErrForbidden, "node %s cannot report observations for %s", nodeID, o.ReportingNode)
	}
	if o.ObservedAt.IsZero() {
		o.ObservedAt = h.now()
	}
	if o.Eligibility == "" {
		o.Eligibility = contracts.EligibleUnknown
	}
	o.AccountID = h.resolveAccountAlias(ctx, tx, o.AccountID) // a folded account reports as its survivor
	if err := h.ensureAccountTx(ctx, tx, o.AccountID, o.Provider, contracts.Freshness{ObservedAt: o.ObservedAt}); err != nil {
		return err
	}
	wasLimited := h.accountLimitedTx(ctx, tx, o.AccountID)
	// Keep the newest observation per (account, node).
	res, err := tx.ExecContext(ctx, `INSERT INTO account_observations(account_id,reporting_node,observed_at,observation) VALUES(?,?,?,?)
		ON CONFLICT(account_id,reporting_node) DO UPDATE SET observed_at=excluded.observed_at, observation=excluded.observation
		WHERE excluded.observed_at >= account_observations.observed_at`,
		o.AccountID, nodeID, persistence.Time(o.ObservedAt), persistence.JSON(o))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil // older than what we have
	}
	if limited := h.accountLimitedTx(ctx, tx, o.AccountID); limited != wasLimited {
		r := auditRec{Action: contracts.AuditAccountCooldownEnded, Automatic: true, NodeID: nodeID, AccountID: o.AccountID, ToNote: audit.UsageNote(&o)}
		if limited {
			r.Action = contracts.AuditAccountCooldown
			if o.ResetAt != nil {
				r.Reason = "resets " + o.ResetAt.UTC().Format("Mon Jan 2 15:04 UTC")
			} else if o.Detail != "" {
				r.Reason = o.Detail
			}
		}
		h.auditTx(ctx, tx, r)
	}
	ref := contracts.ObjectRef{Kind: contracts.KindAccount, ID: o.AccountID}
	return h.emitPayload(ctx, tx, contracts.EvAccountObserved, ref, "", nil, "node:"+nodeID, seq, o)
}

func (h *Hub) ingestGrantTx(ctx context.Context, tx *sql.Tx, nodeID string, g contracts.DeviceGrant, seq uint64) error {
	if !contracts.ValidID(g.GrantID, contracts.PrefixGrant) {
		return contracts.Errorf(contracts.ErrInvalid, "grant_id %q is malformed", g.GrantID)
	}
	if g.NodeID == "" {
		g.NodeID = nodeID
	}
	if g.NodeID != nodeID {
		return contracts.Errorf(contracts.ErrForbidden, "node %s cannot report grants of %s", nodeID, g.NodeID)
	}
	if h.isAccountAlias(ctx, tx, g.AccountID) {
		// The account was folded into another one: the node re-registers
		// the survivor and reports that grant; this one is dropped.
		return nil
	}
	if err := h.ensureAccountTx(ctx, tx, g.AccountID, contracts.ProviderFor(g.Harness), contracts.Freshness{ObservedAt: g.ObservedAt}); err != nil {
		return err
	}
	var gen int64
	var prevJSON string
	err := tx.QueryRowContext(ctx, `SELECT generation, grant_json FROM device_grants WHERE grant_id=?`, g.GrantID).Scan(&gen, &prevJSON)
	if err == nil && g.Generation < gen {
		return contracts.Errorf(contracts.ErrConflict, "stale grant generation %d < %d", g.Generation, gen).WithRef(contracts.KindGrant, g.GrantID)
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO device_grants(grant_id,account_id,node_id,generation,grant_json,observed_at) VALUES(?,?,?,?,?,?)
		ON CONFLICT(grant_id) DO UPDATE SET account_id=excluded.account_id, generation=excluded.generation, grant_json=excluded.grant_json, observed_at=excluded.observed_at`,
		g.GrantID, g.AccountID, nodeID, g.Generation, persistence.JSON(g), persistence.Time(g.ObservedAt)); err != nil {
		return err
	}
	h.auditGrantTx(ctx, tx, nodeID, prevJSON, g)
	// No event: account.* payload shapes do not include DeviceGrant; grants
	// are visible through ListAccounts.
	return nil
}

func (h *Hub) ingestAssignmentTx(ctx context.Context, tx *sql.Tx, nodeID string, a contracts.AccountAssignment, seq uint64) error {
	if !contracts.ValidID(a.AssignmentID, contracts.PrefixAssignment) {
		return contracts.Errorf(contracts.ErrInvalid, "assignment_id %q is malformed", a.AssignmentID)
	}
	if a.NodeID == "" {
		a.NodeID = nodeID
	}
	if a.NodeID != nodeID {
		return contracts.Errorf(contracts.ErrForbidden, "node %s cannot report assignments of %s", nodeID, a.NodeID)
	}
	if run, err := h.getRun(ctx, tx, a.RunID); err == nil {
		if run.NodeID != nodeID {
			return contracts.Errorf(contracts.ErrForbidden, "run %s is not on node %s", a.RunID, nodeID)
		}
		if a.SessionID == "" {
			a.SessionID = run.SessionID
		}
	}
	a.AccountID = h.resolveAccountAlias(ctx, tx, a.AccountID) // running work on a folded account
	var prov contracts.Provider
	if s, err := h.getSession(ctx, tx, a.SessionID); err == nil {
		prov = contracts.ProviderFor(s.Harness)
	}
	if err := h.ensureAccountTx(ctx, tx, a.AccountID, prov, a.Freshness); err != nil {
		return err
	}
	// An assignment binds a run to one account permanently.
	var existing string
	err := tx.QueryRowContext(ctx, `SELECT account_id FROM account_assignments WHERE assignment_id=?`, a.AssignmentID).Scan(&existing)
	isNew := errors.Is(err, sql.ErrNoRows)
	if err == nil && existing != a.AccountID {
		return contracts.Errorf(contracts.ErrConflict, "assignment %s is bound to account %s", a.AssignmentID, existing).WithRef(contracts.KindAssignment, a.AssignmentID)
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO account_assignments(assignment_id,account_id,run_id,session_id,node_id,released_at,assignment) VALUES(?,?,?,?,?,?,?)
		ON CONFLICT(assignment_id) DO UPDATE SET released_at=excluded.released_at, assignment=excluded.assignment`,
		a.AssignmentID, a.AccountID, a.RunID, a.SessionID, nodeID, persistence.TimePtr(a.ReleasedAt), persistence.JSON(a)); err != nil {
		return err
	}
	if isNew && a.ReleasedAt == nil {
		at := a.AssignedAt
		if at.IsZero() {
			at = h.now()
		}
		h.auditTx(ctx, tx, auditRec{Action: contracts.AuditAccountChosen, Automatic: true, At: at, NodeID: nodeID, SessionID: a.SessionID, RunID: a.RunID,
			AccountID: a.AccountID, ToNote: h.usageNoteTx(ctx, tx, a.AccountID, nodeID), Reason: a.Reason, Dedupe: "chosen:" + a.AssignmentID})
	}
	return h.emitPayload(ctx, tx, contracts.EvAccountAssigned, contracts.ObjectRef{Kind: contracts.KindAssignment, ID: a.AssignmentID}, a.SessionID, nil, "node:"+nodeID, seq, a)
}

func (h *Hub) ingestFallbackTx(ctx context.Context, tx *sql.Tx, nodeID string, f contracts.FallbackPlan, seq uint64) error {
	if f.SessionID == "" || f.FromRunID == "" {
		return contracts.Errorf(contracts.ErrInvalid, "fallback plan needs session_id and from_run_id")
	}
	if run, err := h.getRun(ctx, tx, f.FromRunID); err == nil && run.NodeID != nodeID {
		return contracts.Errorf(contracts.ErrForbidden, "run %s is not on node %s", f.FromRunID, nodeID)
	}
	f.FromAccountID = h.resolveAccountAlias(ctx, tx, f.FromAccountID)
	f.ToAccountID = h.resolveAccountAlias(ctx, tx, f.ToAccountID)
	if f.ToAccountID != "" && f.FromAccountID != "" {
		from, e1 := h.getAccount(ctx, tx, f.FromAccountID)
		to, e2 := h.getAccount(ctx, tx, f.ToAccountID)
		if e1 == nil && e2 == nil && from.Provider != to.Provider {
			return contracts.Errorf(contracts.ErrInvalid, "fallback crosses providers (%s -> %s)", from.Provider, to.Provider)
		}
	}
	if f.PlannedAt.IsZero() {
		f.PlannedAt = h.now()
	}
	sess, _ := h.getSession(ctx, tx, f.SessionID)
	if f.Provider == "" {
		f.Provider = contracts.ProviderFor(sess.Harness)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO fallback_plans(session_id,from_run_id,node_id,plan,planned_at) VALUES(?,?,?,?,?)`,
		f.SessionID, f.FromRunID, nodeID, persistence.JSON(f), persistence.Time(f.PlannedAt)); err != nil {
		return err
	}
	h.auditFallbackTx(ctx, tx, nodeID, f)
	ref := contracts.ObjectRef{Kind: contracts.KindSession, ID: f.SessionID}
	if err := h.emitPayload(ctx, tx, contracts.EvFallbackPlanned, ref, f.SessionID, sess.ProjectID, "node:"+nodeID, seq, f); err != nil {
		return err
	}
	// No eligible account: a durable exhaustion event (work is preserved).
	// One limit is one account.exhausted: the node's accounts service
	// reports its own (with earliest reset and reasons) just before the
	// plan, so the hub derives one only when the node did not.
	if f.Outcome == contracts.FallbackWaiting || f.Outcome == contracts.FallbackIntervention {
		dup, err := h.exhaustionRecordedTx(ctx, tx, f.SessionID, f.FromRunID)
		if err != nil || dup {
			return err
		}
		return h.emitPayload(ctx, tx, contracts.EvAccountExhausted, ref, f.SessionID, sess.ProjectID, "node:"+nodeID, seq, f)
	}
	return nil
}

// exhaustionRecordedTx reports whether the event log already holds an
// account.exhausted for this run's fallback (session + from_run_id), from
// the node or derived by the hub from the plan.
func (h *Hub) exhaustionRecordedTx(ctx context.Context, tx *sql.Tx, sessionID, fromRunID string) (bool, error) {
	if sessionID == "" || fromRunID == "" {
		return false, nil
	}
	var n int
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE session_id=? AND kind=? AND json_extract(payload, '$.from_run_id')=?`,
		sessionID, string(contracts.EvAccountExhausted), fromRunID).Scan(&n)
	return n > 0, err
}

// duplicateExhaustionTx reports whether a node-reported event is an
// account.exhausted for a run fallback the event log already records (the
// hub derived it from a plan that arrived first): it is dropped so one limit
// stays one event.
func (h *Hub) duplicateExhaustionTx(ctx context.Context, tx *sql.Tx, ev contracts.Event) (bool, error) {
	if ev.Kind != contracts.EvAccountExhausted {
		return false, nil
	}
	var p struct {
		SessionID string `json:"session_id"`
		FromRunID string `json:"from_run_id"`
	}
	if len(ev.Payload) == 0 || json.Unmarshal(ev.Payload, &p) != nil {
		return false, nil
	}
	sid := ev.SessionID
	if sid == "" {
		sid = p.SessionID
	}
	return h.exhaustionRecordedTx(ctx, tx, sid, p.FromRunID)
}

// presentObservation marks old observations stale; stale or unknown data is
// never presented as eligible.
func (h *Hub) presentObservation(o contracts.AccountObservation) contracts.AccountObservation {
	if h.now().Sub(o.ObservedAt) > h.opt.AccountStaleAfter {
		o.Stale = true
		if o.Detail == "" {
			o.Detail = fmt.Sprintf("observation is %s old", h.now().Sub(o.ObservedAt).Round(1e9))
		}
	}
	if o.Stale && o.Eligibility == contracts.EligibleYes {
		o.Eligibility = contracts.EligibleUnknown
	}
	return o
}

// ListAccounts aggregates accounts with their newest observation, grants and
// active assignments.
func (h *Hub) ListAccounts(ctx context.Context, req contracts.ListAccountsRequest) (contracts.Page[contracts.AccountView], error) {
	if _, err := principal(ctx); err != nil {
		return contracts.Page[contracts.AccountView]{}, err
	}
	pc, err := persistence.DecodeCursor(req.Cursor)
	if err != nil {
		return contracts.Page[contracts.AccountView]{}, err
	}
	limit := persistence.ClampLimit(req.Limit, persistence.DefaultLimit, persistence.MaxLimit)
	q := `SELECT account FROM accounts WHERE account_id > ?`
	args := []any{pc.I}
	if req.Provider != "" {
		q += ` AND provider = ?`
		args = append(args, string(req.Provider))
	}
	q += ` ORDER BY account_id LIMIT ?`
	args = append(args, limit+1)
	rows, err := h.db.QueryContext(ctx, q, args...)
	if err != nil {
		return contracts.Page[contracts.AccountView]{}, err
	}
	var accts []contracts.Account
	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil {
			rows.Close()
			return contracts.Page[contracts.AccountView]{}, err
		}
		var a contracts.Account
		if err := json.Unmarshal([]byte(body), &a); err != nil {
			rows.Close()
			return contracts.Page[contracts.AccountView]{}, err
		}
		accts = append(accts, a)
	}
	rows.Close()
	page := contracts.Page[contracts.AccountView]{Items: []contracts.AccountView{}, SnapshotAt: h.now()}
	for i, a := range accts {
		if i == limit {
			page.NextCursor = persistence.EncodeCursor(persistence.PageCursor{I: accts[i-1].AccountID})
			break
		}
		v, err := h.accountView(ctx, a)
		if err != nil {
			return page, err
		}
		page.Items = append(page.Items, v)
	}
	return page, nil
}

func (h *Hub) accountView(ctx context.Context, a contracts.Account) (contracts.AccountView, error) {
	v := contracts.AccountView{Account: a, Grants: []contracts.DeviceGrant{}, Assignments: []contracts.AccountAssignment{}}
	// One row per (account, reporting node): the newest per node.
	var all []contracts.AccountObservation
	if err := collectJSON(ctx, h.db, `SELECT observation FROM account_observations WHERE account_id=? ORDER BY reporting_node`, []any{a.AccountID}, &all); err != nil {
		return v, err
	}
	for i := range all {
		all[i] = h.presentObservation(all[i])
		if v.Latest == nil || all[i].ObservedAt.After(v.Latest.ObservedAt) {
			o := all[i]
			v.Latest = &o
		}
	}
	if len(all) > 0 {
		v.ByNode = all
	}
	if err := collectJSON(ctx, h.db, `SELECT grant_json FROM device_grants WHERE account_id=? ORDER BY grant_id`, []any{a.AccountID}, &v.Grants); err != nil {
		return v, err
	}
	if err := collectJSON(ctx, h.db, `SELECT assignment FROM account_assignments WHERE account_id=? AND released_at IS NULL ORDER BY assignment_id`, []any{a.AccountID}, &v.Assignments); err != nil {
		return v, err
	}
	return v, nil
}

func collectJSON[T any](ctx context.Context, q queryer, query string, args []any, out *[]T) error {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil {
			return err
		}
		var v T
		if err := json.Unmarshal([]byte(body), &v); err != nil {
			return err
		}
		*out = append(*out, v)
	}
	return rows.Err()
}

// ListFallbacks lists fallback plans, newest first.
func (h *Hub) ListFallbacks(ctx context.Context, req contracts.ListFallbacksRequest) (contracts.Page[contracts.FallbackPlan], error) {
	if _, err := principal(ctx); err != nil {
		return contracts.Page[contracts.FallbackPlan]{}, err
	}
	pc, err := persistence.DecodeCursor(req.Cursor)
	if err != nil {
		return contracts.Page[contracts.FallbackPlan]{}, err
	}
	limit := persistence.ClampLimit(req.Limit, persistence.DefaultLimit, persistence.MaxLimit)
	q := `SELECT id, plan FROM fallback_plans WHERE 1=1`
	args := []any{}
	if pc.N > 0 {
		q += ` AND id < ?`
		args = append(args, pc.N)
	}
	if req.SessionID != "" {
		q += ` AND session_id = ?`
		args = append(args, req.SessionID)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit+1)
	rows, err := h.db.QueryContext(ctx, q, args...)
	if err != nil {
		return contracts.Page[contracts.FallbackPlan]{}, err
	}
	defer rows.Close()
	page := contracts.Page[contracts.FallbackPlan]{Items: []contracts.FallbackPlan{}, SnapshotAt: h.now()}
	var last int64
	for rows.Next() {
		var id int64
		var body string
		if err := rows.Scan(&id, &body); err != nil {
			return page, err
		}
		if len(page.Items) == limit {
			page.NextCursor = persistence.EncodeCursor(persistence.PageCursor{N: last})
			break
		}
		var f contracts.FallbackPlan
		if err := json.Unmarshal([]byte(body), &f); err != nil {
			return page, err
		}
		page.Items = append(page.Items, f)
		last = id
	}
	return page, rows.Err()
}
