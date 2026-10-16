package hub

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jhh3/pyre/internal/attention/sessionflag"
	"github.com/jhh3/pyre/internal/audit"
	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/persistence"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

// ---- sessions ----

const sesCols = `session_id,harness,title,project_id,created_by,created_at,current_run_id,lineage,tags,archived,icon,owner_active_at`

// sessionIconMigrations add the session icon column (W-archive2). A
// separate namespace keeps the core hub migration registry untouched.
var sessionIconMigrations = []sqlitex.Migration{{Namespace: "hub_session_icon", Version: 1, Name: "session_icon", SQL: `
ALTER TABLE sessions ADD COLUMN icon TEXT NOT NULL DEFAULT '';
`}}

// sessionOwnerActiveMigrations add when the owner last messaged or typed
// into a session (owner_activity.go), in its own namespace like the icon.
var sessionOwnerActiveMigrations = []sqlitex.Migration{{Namespace: "hub_session_owner_active", Version: 1, Name: "session_owner_active_at", SQL: `
ALTER TABLE sessions ADD COLUMN owner_active_at TEXT;
`}}

func scanSession(sc interface{ Scan(...any) error }) (contracts.Session, error) {
	var s contracts.Session
	var harness, createdBy, createdAt, lineage, tags string
	var project, current, ownerActive sql.NullString
	var archived int
	if err := sc.Scan(&s.SessionID, &harness, &s.Title, &project, &createdBy, &createdAt, &current, &lineage, &tags, &archived, &s.Icon, &ownerActive); err != nil {
		return s, err
	}
	s.Harness = contracts.Harness(harness)
	s.ProjectID = persistence.ProjectPtr(project)
	_ = persistence.FromJSON(createdBy, &s.CreatedBy)
	s.CreatedAt = persistence.ParseTime(createdAt)
	s.CurrentRunID = current.String
	_ = persistence.FromJSON(lineage, &s.Lineage)
	_ = persistence.FromJSON(tags, &s.Tags)
	s.Archived = archived != 0
	s.OwnerActiveAt = persistence.ParseTimePtr(ownerActive)
	return s, nil
}

func (h *Hub) getSession(ctx context.Context, q queryer, id string) (contracts.Session, error) {
	s, err := scanSession(q.QueryRowContext(ctx, `SELECT `+sesCols+` FROM sessions WHERE session_id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return s, contracts.Errorf(contracts.ErrNotFound, "session not found").WithRef(contracts.KindSession, id)
	}
	return s, err
}

func (h *Hub) insertSessionTx(ctx context.Context, tx *sql.Tx, s contracts.Session) error {
	if s.Tags == nil {
		s.Tags = []string{}
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO sessions(session_id,harness,title,project_id,created_by,created_at,current_run_id,lineage,tags,archived,updated_at,icon) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		s.SessionID, string(s.Harness), s.Title, persistence.ProjectArg(s.ProjectID), persistence.JSON(s.CreatedBy), persistence.Time(s.CreatedAt),
		persistence.NullStr(s.CurrentRunID), persistence.JSON(s.Lineage), persistence.JSON(s.Tags), boolInt(s.Archived), persistence.Time(h.now()), s.Icon)
	return err
}

func (h *Hub) updateSessionTx(ctx context.Context, tx *sql.Tx, s contracts.Session) error {
	if s.Tags == nil {
		s.Tags = []string{}
	}
	_, err := tx.ExecContext(ctx, `UPDATE sessions SET title=?, project_id=?, current_run_id=?, lineage=?, tags=?, archived=?, updated_at=?, icon=? WHERE session_id=?`,
		s.Title, persistence.ProjectArg(s.ProjectID), persistence.NullStr(s.CurrentRunID), persistence.JSON(s.Lineage), persistence.JSON(s.Tags),
		boolInt(s.Archived), persistence.Time(h.now()), s.Icon, s.SessionID)
	if err == nil && s.Archived {
		err = h.withdrawSessionAsksTx(ctx, tx, s.SessionID) // W-asks: an archived session's asks are withdrawn
	}
	return err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ---- runs ----

type runRow struct {
	contracts.Run
	OriginSeq      uint64
	StartCommandID string
}

func (h *Hub) getRunRow(ctx context.Context, q queryer, id string) (runRow, error) {
	var rr runRow
	var body string
	var seq int64
	var startCmd sql.NullString
	err := q.QueryRowContext(ctx, `SELECT run, origin_seq, start_command_id FROM runs WHERE run_id=?`, id).Scan(&body, &seq, &startCmd)
	if errors.Is(err, sql.ErrNoRows) {
		return rr, contracts.Errorf(contracts.ErrNotFound, "run not found").WithRef(contracts.KindRun, id)
	}
	if err != nil {
		return rr, err
	}
	if err := json.Unmarshal([]byte(body), &rr.Run); err != nil {
		return rr, err
	}
	rr.OriginSeq = uint64(seq)
	rr.StartCommandID = startCmd.String
	return rr, nil
}

func (h *Hub) getRun(ctx context.Context, q queryer, id string) (contracts.Run, error) {
	rr, err := h.getRunRow(ctx, q, id)
	return rr.Run, err
}

func (h *Hub) putRunTx(ctx context.Context, tx *sql.Tx, r contracts.Run, originSeq uint64, startCmd string) error {
	if r.Capabilities == nil {
		r.Capabilities = []string{}
	}
	r.Lease, r.PendingApprovalIDs = nil, nil                  // computed on read
	r.AccountID = h.resolveAccountAlias(ctx, tx, r.AccountID) // a folded account (accounts_merge.go)
	_, err := tx.ExecContext(ctx, `INSERT INTO runs(run_id,session_id,node_id,state,run,origin_seq,start_command_id,started_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?)
		ON CONFLICT(run_id) DO UPDATE SET state=excluded.state, run=excluded.run, origin_seq=MAX(runs.origin_seq, excluded.origin_seq),
		start_command_id=COALESCE(runs.start_command_id, excluded.start_command_id), updated_at=excluded.updated_at`,
		r.RunID, r.SessionID, r.NodeID, string(r.State), persistence.JSON(r), int64(originSeq), persistence.NullStr(startCmd),
		persistence.Time(r.StartedAt), persistence.Time(h.now()))
	return err
}

// observeRunTx ingests a node's observation of a run (run report, hello
// LiveRuns, or a launch result). It never creates two current runs: a
// reported run becomes current only if the session has none, it already is
// current, or it is the declared successor (PreviousRunID) of the current
// run. Any other live run is recorded and surfaced as a visible conflict.
func (h *Hub) observeRunTx(ctx context.Context, tx *sql.Tx, nodeID string, r contracts.Run, originSeq uint64, source string) error {
	if r.RunID == "" || r.SessionID == "" {
		return contracts.Errorf(contracts.ErrInvalid, "run observation needs run_id and session_id")
	}
	if r.NodeID == "" {
		r.NodeID = nodeID
	}
	if r.NodeID != nodeID {
		return contracts.Errorf(contracts.ErrForbidden, "node %s cannot report run %s owned by node %s", nodeID, r.RunID, r.NodeID).WithRef(contracts.KindRun, r.RunID)
	}
	existing, err := h.getRunRow(ctx, tx, r.RunID)
	isNew := contracts.IsCode(err, contracts.ErrNotFound)
	if err != nil && !isNew {
		return err
	}
	if !isNew {
		if existing.NodeID != nodeID {
			return contracts.Errorf(contracts.ErrForbidden, "node %s cannot report run %s owned by node %s", nodeID, r.RunID, existing.NodeID).WithRef(contracts.KindRun, r.RunID)
		}
		if existing.SessionID != r.SessionID {
			return contracts.Errorf(contracts.ErrConflict, "run %s belongs to session %s", r.RunID, existing.SessionID).WithRef(contracts.KindRun, r.RunID)
		}
		if originSeq > 0 && originSeq < existing.OriginSeq {
			return nil // older observation; latest wins
		}
		mergeRun(&r, existing.Run)
	}
	now := h.now()
	if r.State == "" {
		r.State = contracts.RunUnknown
	}
	if r.State != contracts.RunRunning {
		r.Activity = nil // "doing now" is only meaningful while working
	}
	if r.StartedAt.IsZero() {
		r.StartedAt = now
	}
	r.Freshness = contracts.Freshness{ObservedAt: now, Source: "node:" + nodeID}

	sess, err := h.getSession(ctx, tx, r.SessionID)
	if contracts.IsCode(err, contracts.ErrNotFound) {
		// The node supervises a run for a session the hub has not seen (e.g.
		// launched locally while the hub was unreachable). Record it.
		sess = contracts.Session{
			SessionID: r.SessionID, Harness: r.Harness, CreatedBy: contracts.Principal{Kind: contracts.PrincipalNode, ID: nodeID},
			CreatedAt: r.StartedAt, Tags: []string{},
		}
		if err := h.insertSessionTx(ctx, tx, sess); err != nil {
			return err
		}
		if err := h.emitSession(ctx, tx, contracts.EvSessionCreated, sess, "node:"+nodeID, originSeq); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	origin := "node:" + nodeID
	becameCurrent := false
	cur := sess.CurrentRunID
	replaced := ""
	switch {
	case cur == r.RunID:
	case cur == "":
		becameCurrent = true
	case r.PreviousRunID != "" && r.PreviousRunID == cur:
		becameCurrent = true
		replaced = cur
	default:
		if !r.State.Terminal() {
			if cr, err := h.getRun(ctx, tx, cur); err == nil && !cr.State.Terminal() {
				// Visible conflict: a live run that is not current (e.g. a host
				// returning after a replacement run was created elsewhere).
				if r.AdapterData == nil {
					r.AdapterData = map[string]string{}
				}
				r.AdapterData[ConflictKey] = "concurrent_run: session's current run is " + cur + "; this run was not made current"
			}
		}
	}
	if r.State.Terminal() || becameCurrent || cur == r.RunID {
		delete(r.AdapterData, ConflictKey)
	}
	if err := h.putRunTx(ctx, tx, r, originSeq, ""); err != nil {
		return err
	}
	if replaced != "" {
		if err := h.withdrawApprovalsTx(ctx, tx, replaced, "its run was replaced"); err != nil {
			return err
		}
	}
	if becameCurrent {
		sess.CurrentRunID = r.RunID
		if err := h.updateSessionTx(ctx, tx, sess); err != nil {
			return err
		}
		if err := h.emitSession(ctx, tx, contracts.EvSessionUpdated, sess, origin, originSeq); err != nil {
			return err
		}
	}
	_, conflict := r.AdapterData[ConflictKey]
	_, hadConflict := existing.AdapterData[ConflictKey]
	switch {
	case isNew:
		if err := h.emitRun(ctx, tx, contracts.EvRunStarted, r, sess.ProjectID, origin, originSeq); err != nil {
			return err
		}
	case existing.State != r.State || existing.StateReason != r.StateReason || conflict != hadConflict:
		if err := h.emitRun(ctx, tx, contracts.EvRunObserved, r, sess.ProjectID, origin, originSeq); err != nil {
			return err
		}
	}
	if err := h.trackChildTx(ctx, tx, existing.State, r); err != nil { // W-childnotify
		return err
	}
	h.noteRunTransition(existing.State, r) // W-archive2
	if r.State.Terminal() && (isNew || !existing.State.Terminal()) {
		if err := h.emitRun(ctx, tx, contracts.EvRunEnded, r, sess.ProjectID, origin, originSeq); err != nil {
			return err
		}
		if err := h.withdrawApprovalsTx(ctx, tx, r.RunID, "its run ended"); err != nil {
			return err
		}
		h.auditLaunchFailedTx(ctx, tx, nodeID, r)
	}
	if becameCurrent && !r.State.Terminal() {
		if err := h.routeUnattemptedTx(ctx, tx, sess.SessionID, r); err != nil {
			return err
		}
	}
	return nil
}

// ConflictKey marks (in Run.AdapterData) a live run that the hub did not
// make current because the session already has a different live current run.
const ConflictKey = "pyre.hub/conflict"

// emitRun appends a run.* event; the payload is the Run (pinned shape).
func (h *Hub) emitRun(ctx context.Context, tx *sql.Tx, kind contracts.EventKind, r contracts.Run, projectID *string, origin string, seq uint64) error {
	r.Lease, r.PendingApprovalIDs = nil, nil
	return h.emitPayload(ctx, tx, kind, contracts.ObjectRef{Kind: contracts.KindRun, ID: r.RunID}, r.SessionID, projectID, origin, seq, r)
}

// emitSession appends a session.* event; the payload is the Session.
func (h *Hub) emitSession(ctx context.Context, tx *sql.Tx, kind contracts.EventKind, s contracts.Session, origin string, seq uint64) error {
	return h.emitPayload(ctx, tx, kind, contracts.ObjectRef{Kind: contracts.KindSession, ID: s.SessionID}, s.SessionID, s.ProjectID, origin, seq, s)
}

// mergeRun fills fields a node omitted from the stored run.
func mergeRun(r *contracts.Run, old contracts.Run) {
	if r.Adapter == "" {
		r.Adapter = old.Adapter
	}
	if r.Harness == "" {
		r.Harness = old.Harness
	}
	if r.Cwd == "" {
		r.Cwd = old.Cwd
	}
	if r.Checkout == nil {
		r.Checkout = old.Checkout
	}
	if r.EnvGenerationID == "" {
		r.EnvGenerationID = old.EnvGenerationID
	}
	if r.AccountID == "" {
		r.AccountID = old.AccountID
	}
	if r.AssignmentID == "" {
		r.AssignmentID = old.AssignmentID
	}
	if r.StartedAt.IsZero() {
		r.StartedAt = old.StartedAt
	}
	if r.NativeRef == nil {
		r.NativeRef = old.NativeRef
	}
	if r.Capabilities == nil {
		r.Capabilities = old.Capabilities
	}
	if r.PreviousRunID == "" {
		r.PreviousRunID = old.PreviousRunID
	}
	if r.ContinuationKind == "" {
		r.ContinuationKind = old.ContinuationKind
	}
	if r.Runtime.Harness == "" && r.Runtime.Version == "" {
		r.Runtime = old.Runtime
	}
}

// setRunStateTx records a hub-derived run state (from evidence such as a
// failed launch result or the node's reconnect inventory).
func (h *Hub) setRunStateTx(ctx context.Context, tx *sql.Tx, rr runRow, state contracts.RunState, reason, origin string) error {
	prev := rr.State
	if prev == state && rr.StateReason == reason {
		return nil
	}
	r := rr.Run
	r.State = state
	r.StateReason = reason
	if state != contracts.RunRunning {
		r.Activity = nil
	}
	r.Freshness = contracts.Freshness{ObservedAt: h.now(), Source: origin}
	if state.Terminal() && r.EndedAt == nil {
		t := h.now()
		r.EndedAt = &t
	}
	if err := h.putRunTx(ctx, tx, r, 0, ""); err != nil {
		return err
	}
	sess, _ := h.getSession(ctx, tx, r.SessionID)
	if err := h.emitRun(ctx, tx, contracts.EvRunObserved, r, sess.ProjectID, origin, 0); err != nil {
		return err
	}
	if err := h.trackChildTx(ctx, tx, prev, r); err != nil { // W-childnotify
		return err
	}
	h.noteRunTransition(prev, r) // W-archive2
	if state.Terminal() && !prev.Terminal() {
		if err := h.emitRun(ctx, tx, contracts.EvRunEnded, r, sess.ProjectID, origin, 0); err != nil {
			return err
		}
		return h.withdrawApprovalsTx(ctx, tx, r.RunID, "its run ended")
	}
	return nil
}

// failStartingRunTx marks a run that never started as failed.
func (h *Hub) failStartingRunTx(ctx context.Context, tx *sql.Tx, runID, reason string) error {
	rr, err := h.getRunRow(ctx, tx, runID)
	if err != nil {
		if contracts.IsCode(err, contracts.ErrNotFound) {
			return nil
		}
		return err
	}
	if rr.State != contracts.RunStarting {
		return nil
	}
	if err := h.setRunStateTx(ctx, tx, rr, contracts.RunFailed, reason, "hub"); err != nil {
		return err
	}
	r := rr.Run
	r.State, r.StateReason = contracts.RunFailed, reason
	h.auditLaunchFailedTx(ctx, tx, r.NodeID, r)
	return nil
}

// auditLaunchFailedTx records a run that never started (no account, no
// checkout, ...; reason "launch failed: <why>") in the audit log, so the
// owner sees why there and not only on the session. Once per run.
func (h *Hub) auditLaunchFailedTx(ctx context.Context, tx *sql.Tx, nodeID string, r contracts.Run) {
	if r.State != contracts.RunFailed || !strings.HasPrefix(r.StateReason, "launch ") {
		return
	}
	h.auditTx(ctx, tx, auditRec{Action: audit.ActionSessionLaunchFailed, Automatic: true, NodeID: nodeID, SessionID: r.SessionID,
		RunID: r.RunID, AccountID: r.AccountID, Reason: r.StateReason, Dedupe: audit.ActionSessionLaunchFailed + ":" + r.RunID})
}

// applyLaunchResultTx applies a start_run/resume command result.
func (h *Hub) applyLaunchResultTx(ctx context.Context, tx *sql.Tx, nodeID string, cmd commandRec, res contracts.CommandResult, originSeq uint64) error {
	runID := cmd.RefID
	switch res.Outcome {
	case contracts.OutcomeSucceeded, contracts.OutcomeDelivered:
		if len(res.Result) == 0 {
			return nil
		}
		var run contracts.Run
		if err := json.Unmarshal(res.Result, &run); err != nil {
			return contracts.Errorf(contracts.ErrInvalid, "launch result is not a run: %v", err)
		}
		if run.RunID == "" {
			run.RunID = runID
		}
		if run.RunID != runID {
			return contracts.Errorf(contracts.ErrConflict, "launch result names run %s, command was for %s", run.RunID, runID)
		}
		return h.observeRunTx(ctx, tx, nodeID, run, originSeq, "launch_result")
	case contracts.OutcomeFailed, contracts.OutcomeUnsupported:
		reason := "launch " + string(res.Outcome)
		if res.Error != nil {
			reason += ": " + res.Error.Message
		}
		return h.failStartingRunTx(ctx, tx, runID, reason)
	case contracts.OutcomeIndeterminate:
		rr, err := h.getRunRow(ctx, tx, runID)
		if err != nil || rr.State != contracts.RunStarting {
			return nil
		}
		return h.setRunStateTx(ctx, tx, rr, contracts.RunUnknown, "launch outcome indeterminate", "node:"+nodeID)
	}
	return nil
}

// ---- views ----

// nodeConnected reports whether the node's control channel is live, plus
// its last-seen time.
func (h *Hub) nodeConnected(ctx context.Context, q queryer, nodeID string) (bool, *contracts.Freshness) {
	m := nodeMemoFrom(ctx)
	if m == nil {
		return h.nodeConnectedUncached(ctx, q, nodeID)
	}
	m.mu.Lock()
	nc, ok := m.conn[nodeID]
	m.mu.Unlock()
	if !ok {
		nc.connected, nc.fresh = h.nodeConnectedUncached(ctx, q, nodeID)
		m.mu.Lock()
		m.conn[nodeID] = nc
		m.mu.Unlock()
	}
	f := *nc.fresh // callers may modify it
	return nc.connected, &f
}

func (h *Hub) nodeConnectedUncached(ctx context.Context, q queryer, nodeID string) (bool, *contracts.Freshness) {
	var connected int
	var lastSeen sql.NullString
	_ = q.QueryRowContext(ctx, `SELECT connected, last_seen_at FROM node_state WHERE node_id=?`, nodeID).Scan(&connected, &lastSeen)
	c := connected != 0
	if h.cfg.Nodes != nil {
		c = h.cfg.Nodes.Connected(nodeID)
	}
	f := &contracts.Freshness{Source: "node:" + nodeID}
	if t := persistence.ParseTimePtr(lastSeen); t != nil {
		f.ObservedAt = *t
	}
	return c, f
}

// decorateRun applies freshness and computed fields for presentation. A run
// whose node is disconnected is reported Stale with state unknown; the
// stored state is not changed.
func (h *Hub) decorateRun(ctx context.Context, q queryer, r *contracts.Run) {
	connected, nf := h.nodeConnected(ctx, q, r.NodeID)
	if !connected && !r.State.Terminal() {
		last := r.State
		r.State = contracts.RunUnknown
		r.StateReason = fmt.Sprintf("node disconnected; last observed %s", last)
		r.Activity = nil
		r.Freshness.Stale = true
		r.Freshness.Reason = "node disconnected"
		if !nf.ObservedAt.IsZero() && (r.Freshness.ObservedAt.IsZero() || nf.ObservedAt.Before(r.Freshness.ObservedAt)) {
			r.Freshness.ObservedAt = nf.ObservedAt
		}
	}
	if l, apr, ok := batchFrom(ctx).decorated(r.RunID); ok {
		r.Lease, r.PendingApprovalIDs = l, apr
		return
	}
	if l, err := h.activeLease(ctx, q, r.RunID); err == nil && l != nil {
		r.Lease = l
	}
	r.PendingApprovalIDs = h.pendingApprovalIDs(ctx, q, r.RunID)
}

// runUsage returns the run's latest process-tree sample from its node's
// last status report (nil if the node reported none for it).
func (h *Hub) runUsage(ctx context.Context, q queryer, nodeID, runID string) *contracts.RunUsage {
	st := h.nodeStatus(ctx, q, nodeID)
	if st == nil {
		return nil
	}
	for _, u := range st.Resources.Runs {
		if u.RunID == runID {
			return &u
		}
	}
	return nil
}

// nodeStatus parses the node's last status report, once per list request
// when the context carries a nodeMemo.
func (h *Hub) nodeStatus(ctx context.Context, q queryer, nodeID string) *contracts.NodeStatusReport {
	m := nodeMemoFrom(ctx)
	if m != nil {
		m.mu.Lock()
		st, ok := m.status[nodeID]
		m.mu.Unlock()
		if ok {
			return st
		}
	}
	var out *contracts.NodeStatusReport
	var status sql.NullString
	if err := q.QueryRowContext(ctx, `SELECT status FROM node_state WHERE node_id=?`, nodeID).Scan(&status); err == nil && status.Valid {
		var st contracts.NodeStatusReport
		if json.Unmarshal([]byte(status.String), &st) == nil {
			out = &st
		}
	}
	if m != nil {
		m.mu.Lock()
		m.status[nodeID] = out
		m.mu.Unlock()
	}
	return out
}

// nodeName resolves a display name. external permits asking the fleet
// domain; it must be false inside a write transaction.
func (h *Hub) nodeName(ctx context.Context, q queryer, nodeID string, external bool) string {
	if m := nodeMemoFrom(ctx); m != nil && external {
		m.mu.Lock()
		n, ok := m.names[nodeID]
		m.mu.Unlock()
		if ok {
			return n
		}
		n = h.nodeNameUncached(ctx, q, nodeID, external)
		m.mu.Lock()
		m.names[nodeID] = n
		m.mu.Unlock()
		return n
	}
	return h.nodeNameUncached(ctx, q, nodeID, external)
}

func (h *Hub) nodeNameUncached(ctx context.Context, q queryer, nodeID string, external bool) string {
	if external && h.cfg.Fleet != nil {
		if n, err := h.cfg.Fleet.GetNode(h.systemCtx(ctx), contracts.GetRequest{ID: nodeID}); err == nil && n.Name != "" {
			return n.Name
		}
	}
	var hello sql.NullString
	_ = q.QueryRowContext(ctx, `SELECT hello FROM node_state WHERE node_id=?`, nodeID).Scan(&hello)
	if hello.Valid {
		var nh contracts.NodeHello
		if json.Unmarshal([]byte(hello.String), &nh) == nil {
			return nh.Hostname
		}
	}
	return ""
}

// withNodeName sets a view built inside a write transaction (where only the
// hello hostname is known) to the node's configured name, so every
// SessionView path (start, resume, rename, associate) shows the same name as
// the lists ("server", not the hostname "alice-server"). Call outside the tx.
func (h *Hub) withNodeName(ctx context.Context, v *contracts.SessionView) {
	if v.Run == nil || v.Run.NodeID == "" {
		return
	}
	if h.cfg.Fleet == nil {
		return
	}
	if n, err := h.cfg.Fleet.GetNode(h.systemCtx(ctx), contracts.GetRequest{ID: v.Run.NodeID}); err == nil && n.Name != "" {
		v.NodeName = n.Name
	}
}

// sessionView builds a presentation view. external permits calls into other
// domains (fleet names, attention counts) and must be false inside a write
// transaction.
func (h *Hub) sessionView(ctx context.Context, q queryer, s contracts.Session, external bool) (contracts.SessionView, error) {
	v := contracts.SessionView{Session: s, Freshness: contracts.Freshness{ObservedAt: h.now(), Source: "hub"}}
	if s.CurrentRunID != "" {
		r, ok := batchFrom(ctx).run(s.CurrentRunID)
		var err error
		if !ok {
			r, err = h.getRun(ctx, q, s.CurrentRunID)
		}
		if err != nil && !contracts.IsCode(err, contracts.ErrNotFound) {
			return v, err
		}
		if err == nil {
			h.decorateRun(ctx, q, &r)
			if !r.State.Terminal() && !r.Freshness.Stale {
				r.Usage = h.runUsage(ctx, q, r.NodeID, r.RunID)
			}
			v.Run = &r
			v.Freshness = contracts.Freshness{ObservedAt: r.Freshness.ObservedAt, Source: "hub", Stale: r.Freshness.Stale, Reason: r.Freshness.Reason}
			if r.Freshness.Stale {
				v.Freshness.Source = "hub-cache"
			}
			v.NodeName = h.nodeName(ctx, q, r.NodeID, external)
		}
	}
	if external && h.cfg.Attention != nil {
		if f, ok := h.cfg.Attention.(sessionFlagger); ok {
			if n, flag, err := f.SessionAttentionRun(ctx, s.SessionID, v.Run); err == nil {
				v.Attention = n
				if flag != nil {
					flag.Text = sessionflag.ResolveTitle(flag.Text, s.SessionID, s.Title)
				}
				v.Flag = flag
			}
		} else if c, ok := h.cfg.Attention.(attentionCounter); ok {
			if n, err := c.CountOpen(ctx, s.SessionID); err == nil {
				v.Attention = n
			}
		} else if page, err := h.cfg.Attention.ListAttention(h.systemCtx(ctx), contracts.ListAttentionRequest{SessionID: s.SessionID, PageRequest: contracts.PageRequest{Limit: 100}}); err == nil {
			v.Attention = len(page.Items)
		}
	}
	if external {
		v.Flag = sessionflag.WithRun(v.Flag, v.Run)
	}
	return v, nil
}

// sessionFlagger is implemented by internal/attention.Store (v1.3). run is
// the session's current run (a launch failure does not flag "needs you").
type sessionFlagger interface {
	SessionAttentionRun(ctx context.Context, sessionID string, run *contracts.Run) (int, *contracts.SessionFlag, error)
}

// attentionCounter is implemented by internal/attention.Store.
type attentionCounter interface {
	CountOpen(ctx context.Context, sessionID string) (int, error)
}
