package hub

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/persistence"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

// Command lifecycle in the durable per-node queue.
//
//	queued  persisted; not handed to the node in its current connection
//	sent    NodeChannel.Send succeeded; awaiting CommandAck
//	acked   the node durably recorded it (CommandAck, or covered by
//	        NodeHello.LastCommandSeq)
//	done    a CommandResult was reported
//	expired its ExpiresAt passed while queued, so the node never received it
//	cancelled withdrawn by the hub before delivery (e.g. a cancelled message)
const (
	cmdQueued    = "queued"
	cmdSent      = "sent"
	cmdAcked     = "acked"
	cmdDone      = "done"
	cmdExpired   = "expired"
	cmdCancelled = "cancelled"
)

type commandRec struct {
	contracts.NodeCommand
	NodeID      string
	State       string
	RefKind     string
	RefID       string
	Outcome     contracts.CommandOutcome
	Result      contracts.RawJSON
	Error       *contracts.Error
	AckedAt     *time.Time
	CompletedAt *time.Time
	SentAt      *time.Time // last handed to the node (nil: never sent)
}

// expiredUnsent reports a queued command whose ExpiresAt passed and that
// was never handed to the node. A command sent before (its ack may have been
// lost) is resent even when expired: the node refuses to start an expired
// command it does not hold, and re-acknowledges one it does.
func (r commandRec) expiredUnsent(now time.Time) bool {
	return r.SentAt == nil && r.ExpiresAt != nil && !now.Before(*r.ExpiresAt)
}

func (r commandRec) finished() bool {
	return r.State == cmdDone || r.State == cmdExpired || r.State == cmdCancelled
}

const cmdCols = `command_id,node_id,seq,kind,actor,issued_at,expires_at,payload,state,ref_kind,ref_id,acked_at,completed_at,outcome,result,error,sent_at`

func scanCommand(sc interface{ Scan(...any) error }) (commandRec, error) {
	var r commandRec
	var kind, actor, issued, payload string
	var expires, refKind, refID, acked, completed, outcome, result, errb, sent sql.NullString
	if err := sc.Scan(&r.CommandID, &r.NodeID, &r.Seq, &kind, &actor, &issued, &expires, &payload, &r.State, &refKind, &refID, &acked, &completed, &outcome, &result, &errb, &sent); err != nil {
		return r, err
	}
	r.Kind = contracts.CommandKind(kind)
	_ = persistence.FromJSON(actor, &r.Actor)
	r.IssuedAt = persistence.ParseTime(issued)
	r.ExpiresAt = persistence.ParseTimePtr(expires)
	r.Payload = contracts.RawJSON(payload)
	r.RefKind, r.RefID = refKind.String, refID.String
	r.AckedAt = persistence.ParseTimePtr(acked)
	r.CompletedAt = persistence.ParseTimePtr(completed)
	r.SentAt = persistence.ParseTimePtr(sent)
	r.Outcome = contracts.CommandOutcome(outcome.String)
	if result.Valid && result.String != "" {
		r.Result = contracts.RawJSON(result.String)
	}
	if errb.Valid && errb.String != "" {
		var e contracts.Error
		if json.Unmarshal([]byte(errb.String), &e) == nil {
			r.Error = &e
		}
	}
	return r, nil
}

func (h *Hub) getCommand(ctx context.Context, q queryer, id string) (commandRec, error) {
	r, err := scanCommand(q.QueryRowContext(ctx, `SELECT `+cmdCols+` FROM commands WHERE command_id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return r, contracts.Errorf(contracts.ErrNotFound, "command not found").WithRef(contracts.KindCommand, id)
	}
	return r, err
}

// ensureNodeRow creates the node_state row if missing.
func (h *Hub) ensureNodeRow(ctx context.Context, tx *sql.Tx, nodeID string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO node_state(node_id,updated_at) VALUES(?,?) ON CONFLICT(node_id) DO NOTHING`, nodeID, persistence.Time(h.now()))
	return err
}

// enqueueTx durably queues a command for nodeID with the next per-node Seq.
// It is sent after commit (afterCommit -> flushNode).
func (h *Hub) enqueueTx(ctx context.Context, tx *sql.Tx, nodeID string, kind contracts.CommandKind, actor contracts.Principal, payload any, ttl time.Duration, ref contracts.ObjectRef) (commandRec, error) {
	var pending int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM commands WHERE node_id=? AND state IN (?,?,?)`, nodeID, cmdQueued, cmdSent, cmdAcked).Scan(&pending); err != nil {
		return commandRec{}, err
	}
	if pending >= h.opt.MaxPendingCommandsPerNode {
		return commandRec{}, contracts.Errorf(contracts.ErrCapacityExhausted, "node %s has %d outstanding commands (limit %d)", nodeID, pending, h.opt.MaxPendingCommandsPerNode).WithRef(contracts.KindNode, nodeID)
	}
	if err := h.ensureNodeRow(ctx, tx, nodeID); err != nil {
		return commandRec{}, err
	}
	var seq int64
	if err := tx.QueryRowContext(ctx, `UPDATE node_state SET last_command_seq=last_command_seq+1 WHERE node_id=? RETURNING last_command_seq`, nodeID).Scan(&seq); err != nil {
		return commandRec{}, err
	}
	now := h.now()
	rec := commandRec{NodeCommand: contracts.NodeCommand{
		CommandID: contracts.NewID(contracts.PrefixCommand), Seq: seq, Kind: kind, Actor: actor, IssuedAt: now,
		Payload: contracts.RawJSON(persistence.JSON(payload)),
	}, NodeID: nodeID, State: cmdQueued, RefKind: string(ref.Kind), RefID: ref.ID}
	if ttl > 0 {
		e := now.Add(ttl)
		rec.ExpiresAt = &e
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO commands(command_id,node_id,seq,kind,actor,issued_at,expires_at,payload,state,ref_kind,ref_id) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		rec.CommandID, nodeID, seq, string(kind), persistence.JSON(actor), persistence.Time(now), persistence.TimePtr(rec.ExpiresAt),
		string(rec.Payload), cmdQueued, persistence.NullStr(rec.RefKind), persistence.NullStr(rec.RefID)); err != nil {
		return rec, err
	}
	h.markDirty(nodeID)
	return rec, nil
}

func (h *Hub) nodeLock(nodeID string) func() {
	h.nodeMu.Lock()
	m, ok := h.nodeLocks[nodeID]
	if !ok {
		m = &sync.Mutex{}
		h.nodeLocks[nodeID] = m
	}
	h.nodeMu.Unlock()
	m.Lock()
	return m.Unlock
}

// flushNode hands queued commands to a connected node in Seq order,
// stopping at the first send failure (the rest stay queued). Commands whose
// ExpiresAt passed while queued are expired instead: the node never
// received them.
func (h *Hub) flushNode(ctx context.Context, nodeID string) {
	if h.cfg.Nodes == nil || !h.cfg.Nodes.Connected(nodeID) {
		return
	}
	unlock := h.nodeLock(nodeID)
	defer unlock()
	rows, err := h.db.QueryContext(ctx, `SELECT `+cmdCols+` FROM commands WHERE node_id=? AND state=? ORDER BY seq LIMIT 500`, nodeID, cmdQueued)
	if err != nil {
		return
	}
	var recs []commandRec
	for rows.Next() {
		r, err := scanCommand(rows)
		if err != nil {
			rows.Close()
			return
		}
		recs = append(recs, r)
	}
	rows.Close()
	expiredAny := false
	defer func() {
		if expiredAny {
			// Not afterCommit: it may re-enter flushNode for this node.
			h.events.Notify()
			h.cmdBC.Broadcast()
		}
	}()
	for _, r := range recs {
		if r.expiredUnsent(h.now()) {
			if err := sqlitex.Tx(ctx, h.db, func(tx *sql.Tx) error {
				return h.expireCommandTx(ctx, tx, r)
			}); err != nil {
				return
			}
			expiredAny = true
			continue
		}
		cmd, err := h.prepareForSend(ctx, r)
		if err != nil {
			return
		}
		sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err = h.cfg.Nodes.Send(sctx, nodeID, cmd)
		cancel()
		if err != nil {
			return
		}
		if _, err := h.db.ExecContext(ctx, `UPDATE commands SET state=?, sent_at=? WHERE command_id=? AND state=?`, cmdSent, persistence.Time(h.now()), r.CommandID, cmdQueued); err != nil {
			return
		}
	}
}

// ResumeNode sends commands queued for nodeID. The transport calls it after
// writing the Welcome frame so commands never precede the welcome.
func (h *Hub) ResumeNode(ctx context.Context, nodeID string) {
	h.flushNode(ctx, nodeID)
}

// prepareForSend injects secrets that are never persisted: a freshly minted
// session token for start/resume commands.
func (h *Hub) prepareForSend(ctx context.Context, r commandRec) (contracts.NodeCommand, error) {
	cmd := r.NodeCommand
	if (r.Kind != contracts.CmdStartRun && r.Kind != contracts.CmdResume) || h.cfg.Tokens == nil {
		return cmd, nil
	}
	var sr contracts.StartRunCommand
	if err := json.Unmarshal(r.Payload, &sr); err != nil {
		return cmd, err
	}
	tok, err := h.cfg.Tokens.IssueSessionToken(ctx, sr.Session.SessionID)
	if err != nil {
		return cmd, err
	}
	sr.SessionToken = tok
	var b []byte
	if r.Kind == contracts.CmdResume {
		b, err = json.Marshal(contracts.ResumeCommand{StartRunCommand: sr})
	} else {
		b, err = json.Marshal(sr)
	}
	if err != nil {
		return cmd, err
	}
	cmd.Payload = b
	return cmd, nil
}

// expireCommandTx marks a never-delivered command expired and records the
// consequence (a launch that never reached the node failed to start).
func (h *Hub) expireCommandTx(ctx context.Context, tx *sql.Tx, r commandRec) error {
	now := h.now()
	ce := contracts.Errorf(contracts.ErrExpired, "command expired before the node received it")
	res, err := tx.ExecContext(ctx, `UPDATE commands SET state=?, completed_at=?, outcome=?, error=? WHERE command_id=? AND state=?`,
		cmdExpired, persistence.Time(now), string(contracts.OutcomeFailed), persistence.JSON(ce), r.CommandID, cmdQueued)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil
	}
	if (r.Kind == contracts.CmdStartRun || r.Kind == contracts.CmdResume) && r.RefKind == string(contracts.KindRun) {
		if err := h.failStartingRunTx(ctx, tx, r.RefID, "launch command expired before the node received it"); err != nil {
			return err
		}
	}
	return h.emitCommandResult(ctx, tx, r, contracts.CommandResult{CommandID: r.CommandID, Kind: r.Kind, Outcome: contracts.OutcomeFailed, Error: ce, At: now}, "hub", 0)
}

// cancelDeliveriesTx withdraws outstanding deliver commands for a message.
// Commands the node may hold get a cancel_delivery command.
func (h *Hub) cancelDeliveriesTx(ctx context.Context, tx *sql.Tx, messageID string, actor contracts.Principal) error {
	return h.withdrawDeliveriesTx(ctx, tx, messageID, actor, map[string]string{"message_id": messageID})
}

// withdrawDeliveriesTx is cancelDeliveriesTx with an explicit cancel_delivery
// payload (a read withdrawal names the evidence and run, W-delivery).
//
// A node holds the message whenever its deliver command was sent, acked, or
// completed successfully: a deliver command completes as soon as the node
// commits the message to its inbox, long before any paste. Those nodes get
// one cancel_delivery each (W-delivery2, incident 2026-10-07: a read only
// looked at unfinished deliver commands, so a message the node had already
// stored and was holding behind the owner's input lease was pasted 8 hours
// after the recipient read it).
func (h *Hub) withdrawDeliveriesTx(ctx context.Context, tx *sql.Tx, messageID string, actor contracts.Principal, payload any) error {
	rows, err := tx.QueryContext(ctx, `SELECT `+cmdCols+` FROM commands WHERE ref_kind=? AND ref_id=? AND kind=? AND state IN (?,?,?,?)`,
		string(contracts.KindMessage), messageID, string(contracts.CmdDeliver), cmdQueued, cmdSent, cmdAcked, cmdDone)
	if err != nil {
		return err
	}
	var recs []commandRec
	for rows.Next() {
		r, err := scanCommand(rows)
		if err != nil {
			rows.Close()
			return err
		}
		recs = append(recs, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	notify := map[string]bool{}
	var nodes []string
	for _, r := range recs {
		if r.State == cmdQueued || r.State == cmdSent {
			if _, err := tx.ExecContext(ctx, `UPDATE commands SET state=?, completed_at=? WHERE command_id=?`, cmdCancelled, persistence.Time(h.now()), r.CommandID); err != nil {
				return err
			}
		}
		held := r.State == cmdSent || r.State == cmdAcked || (r.State == cmdDone && r.Outcome == contracts.OutcomeSucceeded)
		if held && !notify[r.NodeID] {
			notify[r.NodeID] = true
			nodes = append(nodes, r.NodeID)
		}
	}
	for _, n := range nodes {
		if _, err := h.enqueueTx(ctx, tx, n, contracts.CmdCancelDelivery, actor, payload, 0, contracts.ObjectRef{Kind: contracts.KindMessage, ID: messageID}); err != nil {
			return err
		}
	}
	return nil
}

// ackCommandTx records that the node durably recorded a command.
func (h *Hub) ackCommandTx(ctx context.Context, tx *sql.Tx, nodeID string, r commandRec) error {
	if r.NodeID != nodeID {
		return contracts.Errorf(contracts.ErrForbidden, "command %s belongs to another node", r.CommandID)
	}
	if r.State != cmdQueued && r.State != cmdSent {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE commands SET state=?, acked_at=? WHERE command_id=?`, cmdAcked, persistence.Time(h.now()), r.CommandID); err != nil {
		return err
	}
	if r.Kind == contracts.CmdDeliver && r.RefID != "" {
		return h.msgs.MarkNodeReceivedTx(ctx, tx, r.RefID, nodeID)
	}
	return nil
}

// HandleCommandAck processes a CommandAck frame.
func (h *Hub) HandleCommandAck(ctx context.Context, nodeID string, ack contracts.CommandAck) error {
	return h.tx(ctx, func(tx *sql.Tx) error {
		r, err := h.getCommand(ctx, tx, ack.CommandID)
		if err != nil {
			return err
		}
		if ack.Seq != 0 && ack.Seq != r.Seq {
			return contracts.Errorf(contracts.ErrInvalid, "command %s has seq %d, ack says %d", r.CommandID, r.Seq, ack.Seq)
		}
		return h.ackCommandTx(ctx, tx, nodeID, r)
	})
}

// completeCommandTx applies a reported CommandResult.
func (h *Hub) completeCommandTx(ctx context.Context, tx *sql.Tx, nodeID string, res contracts.CommandResult, originSeq uint64) error {
	r, err := h.getCommand(ctx, tx, res.CommandID)
	if err != nil {
		return err
	}
	if r.NodeID != nodeID {
		return contracts.Errorf(contracts.ErrForbidden, "command %s belongs to another node", r.CommandID)
	}
	if r.State == cmdDone || r.State == cmdExpired || r.State == cmdCancelled {
		return nil // duplicate result, or an interim ack after the final one
	}
	if res.Interim || legacyAsyncAck(r, res) {
		// The node accepted a long-running command; its final result
		// follows. Record delivery only: the command stays unfinished.
		now := persistence.Time(h.now())
		_, err := tx.ExecContext(ctx, `UPDATE commands SET state=CASE WHEN state IN (?,?) THEN ? ELSE state END, acked_at=COALESCE(acked_at, ?) WHERE command_id=?`,
			cmdQueued, cmdSent, cmdAcked, now, r.CommandID)
		return err
	}
	if res.Outcome == "" {
		res.Outcome = contracts.OutcomeSucceeded
		if res.Error != nil {
			res.Outcome = contracts.OutcomeFailed
		}
	}
	result := res.Result
	if r.Kind == contracts.CmdAttach && len(result) > 0 {
		// Never persist one-time attach tickets.
		var d contracts.AttachDescriptor
		if json.Unmarshal(result, &d) == nil && d.Ticket != "" {
			d.Ticket = ""
			result = contracts.RawJSON(persistence.JSON(d))
		}
	}
	now := h.now()
	var errj any
	if res.Error != nil {
		errj = persistence.JSON(res.Error)
	}
	var resj any
	if len(result) > 0 {
		resj = string(result)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE commands SET state=?, completed_at=?, acked_at=COALESCE(acked_at, ?), outcome=?, result=?, error=? WHERE command_id=?`,
		cmdDone, persistence.Time(now), persistence.Time(now), string(res.Outcome), resj, errj, r.CommandID); err != nil {
		return err
	}
	if r.Kind == contracts.CmdDeliver && r.RefID != "" {
		if err := h.msgs.MarkNodeReceivedTx(ctx, tx, r.RefID, nodeID); err != nil && !contracts.IsCode(err, contracts.ErrNotFound) {
			return err
		}
	}
	if r.Kind == contracts.CmdStartRun || r.Kind == contracts.CmdResume {
		if err := h.applyLaunchResultTx(ctx, tx, nodeID, r, res, originSeq); err != nil {
			return err
		}
	}
	res.Result = result
	res.Kind = r.Kind
	if res.At.IsZero() {
		res.At = now
	}
	return h.emitCommandResult(ctx, tx, r, res, "node:"+nodeID, originSeq)
}

// maxEventResult bounds the command result carried in a command.result
// event. Larger results (transcript reads for previews and tiles, action
// outputs) stay only in the command record, where commands.get, the
// synchronous call and the waiting caller read them; the event still says
// that and how the command finished. Before (W-dbhygiene) every transcript
// read was stored twice, the event copy averaging ~10KB, and pushed to every
// event-stream client.
const maxEventResult = 1024

// emitCommandResult appends command.result; the payload is the
// CommandResult (attach tickets already stripped; a result larger than
// maxEventResult omitted).
func (h *Hub) emitCommandResult(ctx context.Context, tx *sql.Tx, r commandRec, res contracts.CommandResult, origin string, seq uint64) error {
	if len(res.Result) > maxEventResult {
		res.Result = nil
	}
	sessionID := ""
	if r.RefKind == string(contracts.KindRun) {
		if run, err := h.getRun(ctx, tx, r.RefID); err == nil {
			sessionID = run.SessionID
		}
	}
	return h.emitPayload(ctx, tx, contracts.EvCommandResult, contracts.ObjectRef{Kind: contracts.KindCommand, ID: r.CommandID}, sessionID, nil, origin, seq, res)
}

// waitCommand waits up to d for a command to finish and returns its record.
func (h *Hub) waitCommand(ctx context.Context, id string, d time.Duration) (commandRec, error) {
	var rec commandRec
	err := persistence.LongPoll(ctx, &h.cmdBC, d, h.opt.PollInterval, func() (bool, error) {
		r, err := h.getCommand(ctx, h.db, id)
		if err != nil {
			return false, err
		}
		rec = r
		return r.finished(), nil
	})
	return rec, err
}

// pendingOutcome describes an unfinished command.
func pendingOutcome(r commandRec) contracts.CommandOutcome {
	switch r.State {
	case cmdAcked:
		return contracts.OutcomeDelivered
	case cmdDone:
		return r.Outcome
	case cmdExpired, cmdCancelled:
		return contracts.OutcomeFailed
	}
	return contracts.OutcomePending
}

// receiptFor builds a CommandReceipt from a command record.
func (h *Hub) receiptFor(r commandRec) contracts.CommandReceipt {
	rc := contracts.CommandReceipt{CommandID: r.CommandID, Accepted: true, Outcome: pendingOutcome(r), ObservedAt: h.now()}
	switch r.State {
	case cmdQueued, cmdSent:
		rc.Detail = "queued for node " + r.NodeID
	case cmdAcked:
		rc.Detail = "node " + r.NodeID + " accepted the command; outcome not yet reported"
	case cmdExpired:
		rc.Detail = "command expired before the node received it"
	case cmdCancelled:
		rc.Detail = "command cancelled before delivery"
	case cmdDone:
		var nr contracts.CommandReceipt
		if len(r.Result) > 0 && json.Unmarshal(r.Result, &nr) == nil && nr.CommandID != "" {
			rc.Detail = nr.Detail
			if !nr.ObservedAt.IsZero() {
				rc.ObservedAt = nr.ObservedAt
			}
		}
		if r.Error != nil && rc.Detail == "" {
			rc.Detail = r.Error.Message
		}
		if r.CompletedAt != nil && rc.ObservedAt.IsZero() {
			rc.ObservedAt = *r.CompletedAt
		}
	}
	return rc
}

// legacyAsyncAck recognises the acknowledgement of an async node action from
// a pyred that predates CommandResult.interim: outcome delivered with
// {"async": true}. Its final result follows like an interim one.
func legacyAsyncAck(r commandRec, res contracts.CommandResult) bool {
	if r.Kind != contracts.CmdAction || res.Outcome != contracts.OutcomeDelivered || len(res.Result) == 0 {
		return false
	}
	var m struct {
		Async bool `json:"async"`
	}
	return json.Unmarshal(res.Result, &m) == nil && m.Async
}
