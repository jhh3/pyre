package hub

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/persistence"
)

// ---- input leases ----

const leaseCols = `lease_id,run_id,holder,acquired_at,renewed_at,expires_at,takeover_of,released_at`

func scanLease(sc interface{ Scan(...any) error }) (contracts.InputLease, string, error) {
	var l contracts.InputLease
	var holder, acq, ren, exp string
	var takeover, released sql.NullString
	if err := sc.Scan(&l.LeaseID, &l.RunID, &holder, &acq, &ren, &exp, &takeover, &released); err != nil {
		return l, "", err
	}
	_ = persistence.FromJSON(holder, &l.Holder)
	l.AcquiredAt = persistence.ParseTime(acq)
	l.RenewedAt = persistence.ParseTime(ren)
	l.ExpiresAt = persistence.ParseTime(exp)
	l.TakeoverOf = takeover.String
	l.ReleasedAt = persistence.ParseTimePtr(released)
	return l, persistence.PrincipalKey(l.Holder), nil
}

func (h *Hub) activeLease(ctx context.Context, q queryer, runID string) (*contracts.InputLease, error) {
	l, _, err := scanLease(q.QueryRowContext(ctx, `SELECT `+leaseCols+` FROM leases WHERE run_id=? AND released_at IS NULL AND expires_at > ? ORDER BY acquired_at DESC LIMIT 1`,
		runID, persistence.Time(h.now())))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &l, nil
}

func (h *Hub) getLease(ctx context.Context, q queryer, id string) (contracts.InputLease, error) {
	l, _, err := scanLease(q.QueryRowContext(ctx, `SELECT `+leaseCols+` FROM leases WHERE lease_id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return l, contracts.Errorf(contracts.ErrNotFound, "lease not found").WithRef(contracts.KindLease, id)
	}
	return l, err
}

func displayName(p contracts.Principal) string {
	if p.Name != "" {
		return p.Name
	}
	return string(p.Kind) + ":" + p.ID
}

// AcquireLease takes (or renews) the raw-input lease for a run. A lease
// held by someone else is a conflict naming the holder unless Takeover is
// set, in which case the takeover is recorded. Expired leases are free.
func (h *Hub) AcquireLease(ctx context.Context, req contracts.LeaseRequest) (contracts.InputLease, error) {
	p, err := principal(ctx)
	if err != nil {
		return contracts.InputLease{}, err
	}
	if err := requireOp(req.Mutation); err != nil {
		return contracts.InputLease{}, err
	}
	ttl := h.opt.DefaultLeaseTTL
	if req.TTLSeconds > 0 {
		ttl = time.Duration(req.TTLSeconds) * time.Second
	}
	if ttl > h.opt.MaxLeaseTTL {
		return contracts.InputLease{}, contracts.Errorf(contracts.ErrInvalid, "ttl_seconds exceeds the maximum of %d", int(h.opt.MaxLeaseTTL/time.Second))
	}
	var out contracts.InputLease
	err = h.mutate(ctx, req.OperationID, contracts.OpAcquireLease, p, req, &out, func(tx *sql.Tx) (any, error) {
		run, err := h.getRun(ctx, tx, req.RunID)
		if err != nil {
			return nil, err
		}
		if run.State.Terminal() {
			return nil, contracts.Errorf(contracts.ErrConflict, "run has %s", run.State).WithRef(contracts.KindRun, run.RunID)
		}
		now := h.now()
		me := persistence.PrincipalKey(p)
		cur, err := h.activeLease(ctx, tx, req.RunID)
		if err != nil {
			return nil, err
		}
		var takeoverOf string
		if cur != nil {
			if persistence.PrincipalKey(cur.Holder) == me {
				cur.RenewedAt = now
				cur.ExpiresAt = now.Add(ttl)
				if _, err := tx.ExecContext(ctx, `UPDATE leases SET renewed_at=?, expires_at=? WHERE lease_id=?`, persistence.Time(now), persistence.Time(cur.ExpiresAt), cur.LeaseID); err != nil {
					return nil, err
				}
				return *cur, h.leaseChangedTx(ctx, tx, run, *cur, p, "renewed")
			}
			if !req.Takeover {
				return nil, contracts.Errorf(contracts.ErrConflict, "run input is leased by %s until %s", displayName(cur.Holder), cur.ExpiresAt.UTC().Format("15:04Z")).
					WithRef(contracts.KindLease, cur.LeaseID).WithDetail("lease", *cur)
			}
			if _, err := tx.ExecContext(ctx, `UPDATE leases SET released_at=? WHERE lease_id=?`, persistence.Time(now), cur.LeaseID); err != nil {
				return nil, err
			}
			released := *cur
			released.ReleasedAt = &now
			if err := h.leaseChangedTx(ctx, tx, run, released, p, "taken_over"); err != nil {
				return nil, err
			}
			h.auditTx(ctx, tx, auditRec{Action: contracts.AuditSessionLeaseTakenOver, Actor: actorOf(p), NodeID: run.NodeID, SessionID: run.SessionID, RunID: run.RunID,
				From: displayName(cur.Holder)})
			takeoverOf = cur.LeaseID
		}
		l := contracts.InputLease{LeaseID: contracts.NewID(contracts.PrefixLease), RunID: req.RunID, Holder: p, AcquiredAt: now, RenewedAt: now, ExpiresAt: now.Add(ttl), TakeoverOf: takeoverOf}
		if _, err := tx.ExecContext(ctx, `INSERT INTO leases(lease_id,run_id,holder,holder_key,acquired_at,renewed_at,expires_at,takeover_of) VALUES(?,?,?,?,?,?,?,?)`,
			l.LeaseID, l.RunID, persistence.JSON(p), me, persistence.Time(now), persistence.Time(now), persistence.Time(l.ExpiresAt), persistence.NullStr(takeoverOf)); err != nil {
			return nil, err
		}
		action := "acquired"
		if takeoverOf != "" {
			action = "takeover"
		}
		return l, h.leaseChangedTx(ctx, tx, run, l, p, action)
	})
	if err == nil {
		// The owner taking (or keeping) input is the owner typing into the
		// session: its messages and own flag are answered.
		if p.Kind == contracts.PrincipalOwner {
			sid := h.runSession(ctx, out.RunID)
			h.ownerEngaged(p, sid, "You typed into the session.", engageKinds...)
			h.ownerActive(p, sid)
		}
	}
	return out, err
}

func (h *Hub) leaseChangedTx(ctx context.Context, tx *sql.Tx, run contracts.Run, l contracts.InputLease, actor contracts.Principal, action string) error {
	sess, _ := h.getSession(ctx, tx, run.SessionID)
	if err := h.emitPayload(ctx, tx, contracts.EvLeaseChanged, contracts.ObjectRef{Kind: contracts.KindLease, ID: l.LeaseID}, run.SessionID, sess.ProjectID, "hub", 0, l); err != nil {
		return err
	}
	// Informational for the node; useless after the lease ends.
	ttl := l.ExpiresAt.Sub(h.now())
	if l.ReleasedAt != nil || ttl <= 0 {
		ttl = time.Minute
	}
	_, err := h.enqueueTx(ctx, tx, run.NodeID, contracts.CmdLeaseChanged, actor, l, ttl, contracts.ObjectRef{Kind: contracts.KindLease, ID: l.LeaseID})
	return err
}

// ReleaseLease ends a lease held by the caller (or any lease, for the owner).
func (h *Hub) ReleaseLease(ctx context.Context, req contracts.LeaseReleaseRequest) (contracts.InputLease, error) {
	p, err := principal(ctx)
	if err != nil {
		return contracts.InputLease{}, err
	}
	if err := requireOp(req.Mutation); err != nil {
		return contracts.InputLease{}, err
	}
	var out contracts.InputLease
	err = h.mutate(ctx, req.OperationID, contracts.OpReleaseLease, p, req, &out, func(tx *sql.Tx) (any, error) {
		l, err := h.getLease(ctx, tx, req.LeaseID)
		if err != nil {
			return nil, err
		}
		if persistence.PrincipalKey(l.Holder) != persistence.PrincipalKey(p) && !isOwner(p) {
			return nil, contracts.Errorf(contracts.ErrForbidden, "lease is held by %s", displayName(l.Holder)).WithRef(contracts.KindLease, l.LeaseID)
		}
		if l.ReleasedAt != nil {
			return l, nil
		}
		now := h.now()
		if _, err := tx.ExecContext(ctx, `UPDATE leases SET released_at=? WHERE lease_id=?`, persistence.Time(now), l.LeaseID); err != nil {
			return nil, err
		}
		l.ReleasedAt = &now
		run, err := h.getRun(ctx, tx, l.RunID)
		if err != nil {
			return nil, err
		}
		return l, h.leaseChangedTx(ctx, tx, run, l, p, "released")
	})
	return out, err
}

// ---- approvals ----

func (h *Hub) getApproval(ctx context.Context, q queryer, id string) (contracts.Approval, error) {
	var body string
	err := q.QueryRowContext(ctx, `SELECT approval FROM approvals WHERE approval_id=?`, id).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return contracts.Approval{}, contracts.Errorf(contracts.ErrNotFound, "approval not found").WithRef(contracts.KindApproval, id)
	}
	if err != nil {
		return contracts.Approval{}, err
	}
	var a contracts.Approval
	return a, json.Unmarshal([]byte(body), &a)
}

func (h *Hub) putApprovalTx(ctx context.Context, tx *sql.Tx, a contracts.Approval, nodeID string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO approvals(approval_id,session_id,run_id,node_id,state,approval,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)
		ON CONFLICT(approval_id) DO UPDATE SET state=excluded.state, approval=excluded.approval, updated_at=excluded.updated_at`,
		a.ApprovalID, a.SessionID, a.RunID, persistence.NullStr(nodeID), string(a.State), persistence.JSON(a), persistence.Time(a.CreatedAt), persistence.Time(h.now()))
	return err
}

func (h *Hub) pendingApprovalIDs(ctx context.Context, q queryer, runID string) []string {
	return h.approvalIDsInStates(ctx, q, runID, contracts.ApprovalPending)
}

// approvalIDsInStates lists a run's approvals in the given states, oldest
// first.
func (h *Hub) approvalIDsInStates(ctx context.Context, q queryer, runID string, states ...contracts.ApprovalState) []string {
	ph := make([]string, len(states))
	args := []any{runID}
	for i, st := range states {
		ph[i] = "?"
		args = append(args, string(st))
	}
	rows, err := q.QueryContext(ctx, `SELECT approval_id FROM approvals WHERE run_id=? AND state IN (`+strings.Join(ph, ",")+`) ORDER BY created_at`, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			out = append(out, id)
		}
	}
	return out
}

// emitApproval appends an approval.* event; the payload is the Approval.
func (h *Hub) emitApproval(ctx context.Context, tx *sql.Tx, kind contracts.EventKind, a contracts.Approval, origin string, seq uint64) error {
	sess, _ := h.getSession(ctx, tx, a.SessionID)
	return h.emitPayload(ctx, tx, kind, contracts.ObjectRef{Kind: contracts.KindApproval, ID: a.ApprovalID}, a.SessionID, sess.ProjectID, origin, seq, a)
}

// withdrawApprovalsTx withdraws pending approvals of a run that ended or
// was replaced, and expired ones (their native prompt may have been on
// screen) so no surface keeps saying the run waits in its terminal.
func (h *Hub) withdrawApprovalsTx(ctx context.Context, tx *sql.Tx, runID, reason string) error {
	if runID == "" {
		return nil
	}
	ids := h.approvalIDsInStates(ctx, tx, runID, contracts.ApprovalPending, contracts.ApprovalExpired)
	for _, id := range ids {
		a, err := h.getApproval(ctx, tx, id)
		if err != nil {
			return err
		}
		a.State = contracts.ApprovalWithdrawn
		if err := h.putApprovalTx(ctx, tx, a, ""); err != nil {
			return err
		}
		if err := h.emitApproval(ctx, tx, contracts.EvApprovalWithdrawn, a, "hub", 0); err != nil {
			return err
		}
	}
	return nil
}

// ingestApprovalTx applies an approval reported by the node owning its run.
// The first resolution wins; later conflicting reports are ignored.
func (h *Hub) ingestApprovalTx(ctx context.Context, tx *sql.Tx, nodeID string, a contracts.Approval, seq uint64) error {
	if !contracts.ValidID(a.ApprovalID, contracts.PrefixApproval) {
		return contracts.Errorf(contracts.ErrInvalid, "approval_id %q is malformed", a.ApprovalID)
	}
	if a.RunID == "" {
		return contracts.Errorf(contracts.ErrInvalid, "approval needs run_id")
	}
	run, err := h.getRun(ctx, tx, a.RunID)
	if err == nil {
		if run.NodeID != nodeID {
			return contracts.Errorf(contracts.ErrForbidden, "node %s cannot report approvals for run %s", nodeID, a.RunID)
		}
		if a.SessionID == "" {
			a.SessionID = run.SessionID
		}
		if a.SessionID != run.SessionID {
			return contracts.Errorf(contracts.ErrConflict, "approval session %s does not match run session %s", a.SessionID, run.SessionID)
		}
	} else if !contracts.IsCode(err, contracts.ErrNotFound) {
		return err
	}
	if a.State == "" {
		a.State = contracts.ApprovalPending
	}
	if a.CreatedAt.IsZero() {
		a.CreatedAt = h.now()
	}
	origin := "node:" + nodeID
	existing, err := h.getApproval(ctx, tx, a.ApprovalID)
	if contracts.IsCode(err, contracts.ErrNotFound) {
		if err := h.putApprovalTx(ctx, tx, a, nodeID); err != nil {
			return err
		}
		kind := contracts.EvApprovalRequested
		switch a.State {
		case contracts.ApprovalResolved:
			kind = contracts.EvApprovalResolved
		case contracts.ApprovalWithdrawn, contracts.ApprovalExpired:
			kind = contracts.EvApprovalWithdrawn
		}
		return h.emitApproval(ctx, tx, kind, a, origin, seq)
	}
	if err != nil {
		return err
	}
	if existing.State == contracts.ApprovalExpired {
		return h.settleExpiredApprovalTx(ctx, tx, nodeID, a, seq)
	}
	if existing.State != contracts.ApprovalPending {
		return h.reconcileSettledApprovalTx(ctx, tx, nodeID, existing, a, seq)
	}
	switch a.State {
	case contracts.ApprovalPending:
		a.Resolution = nil
		if err := h.putApprovalTx(ctx, tx, a, nodeID); err != nil {
			return err
		}
		return nil
	case contracts.ApprovalResolved:
		if a.Resolution == nil {
			a.Resolution = &contracts.ApprovalResolution{Source: "native", Actor: contracts.Principal{Kind: contracts.PrincipalSystem, ID: "native-terminal"}, ResolvedAt: h.now()}
		}
		if err := h.putApprovalTx(ctx, tx, a, nodeID); err != nil {
			return err
		}
		return h.emitApproval(ctx, tx, contracts.EvApprovalResolved, a, origin, seq)
	case contracts.ApprovalWithdrawn, contracts.ApprovalExpired:
		if err := h.putApprovalTx(ctx, tx, a, nodeID); err != nil {
			return err
		}
		return h.emitApproval(ctx, tx, contracts.EvApprovalWithdrawn, a, origin, seq)
	}
	return contracts.Errorf(contracts.ErrInvalid, "unknown approval state %q", a.State)
}

// settleExpiredApprovalTx applies the node's report for an approval whose
// remote answer window expired. Expiry is not a resolution: the harness's
// own prompt may still be on screen (Claude falls back to its dialog). The
// node later reports how that prompt ended (resolved in the terminal, or
// withdrawn when the turn moved on); the hub records it and emits the
// event so surfaces drop their "waiting in its terminal" item. A pending
// report never reopens it (it cannot be answered through Pyre any more).
func (h *Hub) settleExpiredApprovalTx(ctx context.Context, tx *sql.Tx, nodeID string, rep contracts.Approval, seq uint64) error {
	switch rep.State {
	case contracts.ApprovalResolved:
		if rep.Resolution == nil {
			rep.Resolution = &contracts.ApprovalResolution{Source: "native", Actor: contracts.Principal{Kind: contracts.PrincipalSystem, ID: "native-terminal"}, ResolvedAt: h.now()}
		}
		if err := h.putApprovalTx(ctx, tx, rep, nodeID); err != nil {
			return err
		}
		return h.emitApproval(ctx, tx, contracts.EvApprovalResolved, rep, "node:"+nodeID, seq)
	case contracts.ApprovalWithdrawn:
		if err := h.putApprovalTx(ctx, tx, rep, nodeID); err != nil {
			return err
		}
		return h.emitApproval(ctx, tx, contracts.EvApprovalWithdrawn, rep, "node:"+nodeID, seq)
	}
	return nil
}

// reconcileSettledApprovalTx applies runtime truth about an approval the hub
// already settled. A Pyre answer is "submitted" until the node reports its
// effect: "effective" confirms it; a native resolution with effect
// "superseded" (the TUI answered first) replaces it, because runtime
// evidence is the truth. Any other later report is ignored (first
// resolution wins).
func (h *Hub) reconcileSettledApprovalTx(ctx context.Context, tx *sql.Tx, nodeID string, existing, rep contracts.Approval, seq uint64) error {
	if existing.State != contracts.ApprovalResolved || existing.Resolution == nil || existing.Resolution.Source != "pyre" ||
		rep.State != contracts.ApprovalResolved || rep.Resolution == nil {
		return nil
	}
	switch {
	case rep.Resolution.Effect == "superseded" || rep.Resolution.Source == "native":
		r := *rep.Resolution
		if r.Effect == "" {
			r.Effect = "superseded"
		}
		if r.Source == "" {
			r.Source = "native"
		}
		existing.Resolution = &r
	case rep.Resolution.Effect == "effective" && existing.Resolution.Effect != "effective":
		existing.Resolution.Effect = "effective"
	default:
		return nil
	}
	if err := h.putApprovalTx(ctx, tx, existing, nodeID); err != nil {
		return err
	}
	return h.emitApproval(ctx, tx, contracts.EvApprovalResolved, existing, "node:"+nodeID, seq)
}

// effectiveApproval reports expiry by time without mutating storage.
func (h *Hub) effectiveApproval(a contracts.Approval) contracts.Approval {
	if a.State == contracts.ApprovalPending && a.ExpiresAt != nil && !h.now().Before(*a.ExpiresAt) {
		a.State = contracts.ApprovalExpired
	}
	return a
}

// GetApproval returns one approval.
func (h *Hub) GetApproval(ctx context.Context, req contracts.GetRequest) (contracts.Approval, error) {
	if _, err := principal(ctx); err != nil {
		return contracts.Approval{}, err
	}
	a, err := h.getApproval(ctx, h.db, req.ID)
	return h.effectiveApproval(a), err
}

// ListApprovals lists approvals, by default pending ones, newest first.
func (h *Hub) ListApprovals(ctx context.Context, req contracts.ListApprovalsRequest) (contracts.Page[contracts.Approval], error) {
	if _, err := principal(ctx); err != nil {
		return contracts.Page[contracts.Approval]{}, err
	}
	states := req.States
	if len(states) == 0 {
		states = []contracts.ApprovalState{contracts.ApprovalPending}
	}
	pc, err := persistence.DecodeCursor(req.Cursor)
	if err != nil {
		return contracts.Page[contracts.Approval]{}, err
	}
	limit := persistence.ClampLimit(req.Limit, persistence.DefaultLimit, persistence.MaxLimit)
	where := []string{}
	args := []any{}
	ph := make([]string, len(states))
	for i, s := range states {
		ph[i] = "?"
		args = append(args, string(s))
	}
	// Time-expired pending approvals are stored as pending until swept.
	where = append(where, "state IN ("+strings.Join(ph, ",")+")")
	if req.SessionID != "" {
		where = append(where, "session_id = ?")
		args = append(args, req.SessionID)
	}
	if pc.K != "" {
		where = append(where, "(created_at < ? OR (created_at = ? AND approval_id < ?))")
		args = append(args, pc.K, pc.K, pc.I)
	}
	q := `SELECT approval, created_at, approval_id FROM approvals WHERE ` + strings.Join(where, " AND ") + ` ORDER BY created_at DESC, approval_id DESC LIMIT ?`
	args = append(args, limit+1)
	rows, err := h.db.QueryContext(ctx, q, args...)
	if err != nil {
		return contracts.Page[contracts.Approval]{}, err
	}
	defer rows.Close()
	page := contracts.Page[contracts.Approval]{Items: []contracts.Approval{}, SnapshotAt: h.now()}
	want := map[contracts.ApprovalState]bool{}
	for _, s := range states {
		want[s] = true
	}
	n := 0
	var lastK, lastI string
	for rows.Next() {
		var body, k, id string
		if err := rows.Scan(&body, &k, &id); err != nil {
			return page, err
		}
		n++
		if n > limit {
			page.NextCursor = persistence.EncodeCursor(persistence.PageCursor{K: lastK, I: lastI})
			break
		}
		lastK, lastI = k, id
		var a contracts.Approval
		if err := json.Unmarshal([]byte(body), &a); err != nil {
			return page, err
		}
		a = h.effectiveApproval(a)
		if want[a.State] {
			page.Items = append(page.Items, a)
		}
	}
	return page, rows.Err()
}

func resolutionLabel(r *contracts.ApprovalResolution) string {
	if r == nil {
		return "unknown"
	}
	choice := r.OptionID
	if choice == "" {
		choice = "text answer"
	}
	who := displayName(r.Actor)
	if r.Source == "native" {
		who = "native terminal"
	}
	return choice + ", " + who
}

// AnswerApproval resolves a pending approval for its originating run. Only
// one resolution commits; a replay of the same operation returns the stored
// result; an already-resolved approval is a conflict carrying the existing
// resolution; a withdrawn/expired approval or one whose run is no longer
// current is expired.
func (h *Hub) AnswerApproval(ctx context.Context, req contracts.AnswerApprovalRequest) (contracts.Approval, error) {
	p, err := principal(ctx)
	if err != nil {
		return contracts.Approval{}, err
	}
	if err := requireOp(req.Mutation); err != nil {
		return contracts.Approval{}, err
	}
	var out contracts.Approval
	err = h.mutate(ctx, req.OperationID, contracts.OpAnswerApproval, p, req, &out, func(tx *sql.Tx) (any, error) {
		a, err := h.getApproval(ctx, tx, req.ApprovalID)
		if err != nil {
			return nil, err
		}
		ref := func(e *contracts.Error) *contracts.Error { return e.WithRef(contracts.KindApproval, a.ApprovalID) }
		if p.Kind == contracts.PrincipalSession && p.ID == a.SessionID {
			return nil, ref(contracts.Errorf(contracts.ErrForbidden, "a session cannot answer its own approval requests"))
		}
		if req.RunID != a.RunID {
			return nil, ref(contracts.Errorf(contracts.ErrConflict, "approval belongs to run %s, not %s", a.RunID, req.RunID))
		}
		switch a.State {
		case contracts.ApprovalResolved:
			return nil, ref(contracts.Errorf(contracts.ErrConflict, "approval already resolved (%s)", resolutionLabel(a.Resolution))).WithDetail("resolution", a.Resolution)
		case contracts.ApprovalWithdrawn:
			return nil, ref(contracts.Errorf(contracts.ErrExpired, "approval withdrawn: %s", h.withdrawReason(ctx, tx, a)))
		case contracts.ApprovalExpired:
			return nil, ref(contracts.Errorf(contracts.ErrExpired, "approval expired"))
		}
		if a.ExpiresAt != nil && !h.now().Before(*a.ExpiresAt) {
			return nil, ref(contracts.Errorf(contracts.ErrExpired, "approval expired at %s", a.ExpiresAt.UTC().Format(time.RFC3339)))
		}
		run, err := h.getRun(ctx, tx, a.RunID)
		if err != nil {
			return nil, err
		}
		sess, err := h.getSession(ctx, tx, a.SessionID)
		if err != nil {
			return nil, err
		}
		if sess.CurrentRunID != run.RunID {
			return nil, ref(contracts.Errorf(contracts.ErrExpired, "approval withdrawn: its run was replaced"))
		}
		if run.State.Terminal() {
			return nil, ref(contracts.Errorf(contracts.ErrExpired, "approval withdrawn: its run ended"))
		}
		capName := contracts.CapApprovals
		if a.Kind == contracts.ApprovalQuestion {
			capName = contracts.CapQuestions
		}
		if !contracts.HasCap(run.Capabilities, capName) {
			return nil, ref(contracts.Errorf(contracts.ErrUnsupported, "run adapter does not support %s; answer it in the terminal", capName))
		}
		if err := validateAnswer(a, req); err != nil {
			return nil, ref(contracts.AsError(err))
		}
		now := h.now()
		a.State = contracts.ApprovalResolved
		a.Resolution = &contracts.ApprovalResolution{OptionID: req.OptionID, Text: req.Text, Actor: p, ResolvedAt: now, Source: "pyre", Effect: "submitted"}
		res, err := tx.ExecContext(ctx, `UPDATE approvals SET state=?, approval=?, updated_at=? WHERE approval_id=? AND state=?`,
			string(a.State), persistence.JSON(a), persistence.Time(now), a.ApprovalID, string(contracts.ApprovalPending))
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			cur, _ := h.getApproval(ctx, tx, a.ApprovalID)
			return nil, ref(contracts.Errorf(contracts.ErrConflict, "approval already resolved (%s)", resolutionLabel(cur.Resolution)))
		}
		if _, err := h.enqueueTx(ctx, tx, run.NodeID, contracts.CmdAnswer, p, contracts.AnswerCommand{Approval: a, OptionID: req.OptionID, Text: req.Text}, h.opt.StartCommandTTL,
			contracts.ObjectRef{Kind: contracts.KindApproval, ID: a.ApprovalID}); err != nil {
			return nil, err
		}
		return a, h.emitApproval(ctx, tx, contracts.EvApprovalResolved, a, "hub", 0)
	})
	return out, err
}

func (h *Hub) withdrawReason(ctx context.Context, q queryer, a contracts.Approval) string {
	sess, err := h.getSession(ctx, q, a.SessionID)
	if err == nil && sess.CurrentRunID != "" && sess.CurrentRunID != a.RunID {
		return "its run was replaced"
	}
	if run, err := h.getRun(ctx, q, a.RunID); err == nil && run.State.Terminal() {
		return "its run ended"
	}
	return "the runtime no longer waits for it"
}

func validateAnswer(a contracts.Approval, req contracts.AnswerApprovalRequest) error {
	if req.OptionID == "" && req.Text == "" {
		return contracts.Errorf(contracts.ErrInvalid, "option_id or text is required")
	}
	if req.OptionID != "" {
		ok := false
		for _, o := range a.Options {
			if o.ID == req.OptionID {
				ok = true
			}
		}
		if !ok {
			return contracts.Errorf(contracts.ErrInvalid, "option %q is not offered by this approval", req.OptionID)
		}
	}
	if req.Text != "" && !a.AllowText {
		return contracts.Errorf(contracts.ErrInvalid, "this approval does not accept free text")
	}
	return nil
}

// expireApprovalsTx marks time-expired pending approvals.
func (h *Hub) expireApprovalsTx(ctx context.Context, tx *sql.Tx) (int, error) {
	rows, err := tx.QueryContext(ctx, `SELECT approval FROM approvals WHERE state=?`, string(contracts.ApprovalPending))
	if err != nil {
		return 0, err
	}
	var due []contracts.Approval
	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil {
			rows.Close()
			return 0, err
		}
		var a contracts.Approval
		if json.Unmarshal([]byte(body), &a) == nil && a.ExpiresAt != nil && !h.now().Before(*a.ExpiresAt) {
			due = append(due, a)
		}
	}
	rows.Close()
	for _, a := range due {
		a.State = contracts.ApprovalExpired
		if err := h.putApprovalTx(ctx, tx, a, ""); err != nil {
			return 0, err
		}
		if err := h.emitApproval(ctx, tx, contracts.EvApprovalWithdrawn, a, "hub", 0); err != nil {
			return 0, err
		}
	}
	return len(due), nil
}
