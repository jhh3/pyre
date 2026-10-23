package hub

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jhh3/pyre/internal/autotitle"
	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/persistence"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

// NodeLink is the surface the node transport (WP04) drives for each node
// WebSocket. *Hub implements it.
//
//	on hello frame:       welcome, err := HandleHello(ctx, nodeID, hello)
//	                      write welcome (or an error frame), then ResumeNode
//	on report frame:      ack, err := HandleReport(ctx, nodeID, report)
//	                      write ack (no ack on error: the node resends)
//	on command_ack frame: HandleCommandAck(ctx, nodeID, ack)
//	on disconnect:        HandleDisconnect(ctx, nodeID)
type NodeLink interface {
	HandleHello(ctx context.Context, nodeID string, hello contracts.NodeHello) (contracts.NodeWelcome, error)
	HandleReport(ctx context.Context, nodeID string, rep contracts.NodeReport) (contracts.ReportAck, error)
	HandleCommandAck(ctx context.Context, nodeID string, ack contracts.CommandAck) error
	HandleDisconnect(ctx context.Context, nodeID string) error
	ResumeNode(ctx context.Context, nodeID string)
}

var _ NodeLink = (*Hub)(nil)

// HandleHello registers a (re)connected node: negotiates protocol, records
// its inventory, reconciles command delivery with LastCommandSeq, and
// reconciles its LiveRuns without inventing state.
func (h *Hub) HandleHello(ctx context.Context, nodeID string, hello contracts.NodeHello) (contracts.NodeWelcome, error) {
	if hello.NodeID != "" && hello.NodeID != nodeID {
		return contracts.NodeWelcome{}, contracts.Errorf(contracts.ErrForbidden, "hello names node %s but the credential is for %s", hello.NodeID, nodeID)
	}
	minP := hello.MinProtocol
	if minP == 0 {
		minP = hello.Protocol
	}
	if hello.Protocol < contracts.MinProtocolVersion || minP > contracts.ProtocolVersion {
		return contracts.NodeWelcome{}, contracts.Errorf(contracts.ErrProtocolMismatch, "node protocol %d..%d not supported; hub supports %d..%d",
			minP, hello.Protocol, contracts.MinProtocolVersion, contracts.ProtocolVersion).
			WithDetail("min_protocol", contracts.MinProtocolVersion).WithDetail("protocol", contracts.ProtocolVersion)
	}
	proto := hello.Protocol
	if proto > contracts.ProtocolVersion {
		proto = contracts.ProtocolVersion
	}
	if h.cfg.Fleet != nil {
		n, err := h.cfg.Fleet.GetNode(h.systemCtx(ctx), contracts.GetRequest{ID: nodeID})
		if err != nil {
			if contracts.IsCode(err, contracts.ErrNotFound) {
				return contracts.NodeWelcome{}, contracts.Errorf(contracts.ErrUnauthorized, "node is not enrolled").WithRef(contracts.KindNode, nodeID)
			}
			return contracts.NodeWelcome{}, err
		}
		if n.Connection.State == contracts.ConnRevoked {
			return contracts.NodeWelcome{}, contracts.Errorf(contracts.ErrForbidden, "node is revoked").WithRef(contracts.KindNode, nodeID)
		}
	}
	var welcome contracts.NodeWelcome
	err := sqlitex.Tx(ctx, h.db, func(tx *sql.Tx) error {
		now := h.now()
		if err := h.ensureNodeRow(ctx, tx, nodeID); err != nil {
			return err
		}
		hello.NodeID = nodeID
		if prev := h.helloVersion(ctx, tx, nodeID); prev != "" && hello.PyredVersion != "" && prev != hello.PyredVersion {
			h.auditTx(ctx, tx, auditRec{Action: contracts.AuditNodeUpgraded, Automatic: true, NodeID: nodeID, From: prev, To: hello.PyredVersion})
		}
		stored := hello
		stored.LiveRuns = nil
		// The hello is the node's newest full inventory: a status report
		// stored before it (e.g. from before a pyred restart) must not
		// override its harness availability. Later status reports update
		// the view again.
		if _, err := tx.ExecContext(ctx, `UPDATE node_state SET hello=?, status=NULL, connected=1, connected_since=?, last_seen_at=?, protocol=?, updated_at=? WHERE node_id=?`,
			persistence.JSON(stored), persistence.Time(now), persistence.Time(now), proto, persistence.Time(now), nodeID); err != nil {
			return err
		}
		// The fleet registry may have recorded this connection before the
		// hello arrived: give that entry the version now known.
		h.auditConnectedVersionTx(ctx, tx, nodeID, hello.PyredVersion)
		if err := h.reconcileCommandsTx(ctx, tx, nodeID, hello.LastCommandSeq); err != nil {
			return err
		}
		if err := h.reconcileLiveRunsTx(ctx, tx, nodeID, hello); err != nil {
			return err
		}
		var committed int64
		if err := tx.QueryRowContext(ctx, `SELECT committed_report_seq FROM node_state WHERE node_id=?`, nodeID).Scan(&committed); err != nil {
			return err
		}
		welcome = contracts.NodeWelcome{HubID: h.hubID, Protocol: proto, MinProtocol: contracts.MinProtocolVersion, ServerTime: now, CommittedReportSeq: uint64(committed), HeartbeatSeconds: h.opt.HeartbeatSeconds}
		return h.emitConnection(ctx, tx, nodeID, contracts.NodeConnection{State: contracts.ConnConnected, LastSeenAt: &now, ConnectedSince: &now, Protocol: proto})
	})
	if err == nil {
		h.noteInventory(nodeID, hello, h.now()) // unknownruns.go
	}
	// Not h.tx: queued commands go out with the Welcome (WelcomeCommands),
	// never ahead of it.
	h.events.Notify()
	h.msgs.Notify()
	h.cmdBC.Broadcast()
	return welcome, err
}

// reconcileCommandsTx runs on hello. Only a recorded CommandAck (or a
// result) proves the node holds a command: commands sent but never
// acknowledged are requeued and resent with the Welcome whatever their seq
// (commands can reach a node out of order, so LastCommandSeq may cover a
// lower command that was lost); the node dedupes by command_id, so a resend
// of something it already holds is re-acknowledged, never re-run.
// Acknowledged commands are never resent, even if the node's sequence
// regressed (a node that lost state must not re-run a launch or a
// delivery); that regression is recorded instead.
func (h *Hub) reconcileCommandsTx(ctx context.Context, tx *sql.Tx, nodeID string, lastSeq int64) error {
	if _, err := tx.ExecContext(ctx, `UPDATE commands SET state=? WHERE node_id=? AND state=?`, cmdQueued, nodeID, cmdSent); err != nil {
		return err
	}
	var regressed int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM commands WHERE node_id=? AND seq>? AND state=?`, nodeID, lastSeq, cmdAcked).Scan(&regressed); err != nil {
		return err
	}
	if regressed > 0 {
		// Diagnostic record (no pinned event shape fits).
		_, err := tx.ExecContext(ctx, `INSERT INTO report_rejects(node_id,origin_seq,kind,error,payload,at) VALUES(?,?,?,?,?,?)`,
			nodeID, lastSeq, "command_seq_regressed",
			fmt.Sprintf("node reports last_command_seq %d but acknowledged %d later commands; they are not resent", lastSeq, regressed), nil, persistence.Time(h.now()))
		return err
	}
	return nil
}

// reconcileLiveRunsTx ingests the node's live runs and marks runs the node
// no longer reports as unknown (never failed/exited without evidence).
func (h *Hub) reconcileLiveRunsTx(ctx context.Context, tx *sql.Tx, nodeID string, hello contracts.NodeHello) error {
	live := map[string]bool{}
	for _, r := range hello.LiveRuns {
		if err := h.observeRunTx(ctx, tx, nodeID, r, 0, "hello"); err != nil {
			if isReject(err) {
				continue // a bad entry must not block reconnection
			}
			return err
		}
		live[r.RunID] = true
	}
	rows, err := tx.QueryContext(ctx, `SELECT run_id FROM runs WHERE node_id=? AND state NOT IN (?,?,?)`, nodeID, string(contracts.RunExited), string(contracts.RunFailed), string(contracts.RunUnknown))
	if err != nil {
		return err
	}
	var missing []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		if !live[id] {
			missing = append(missing, id)
		}
	}
	rows.Close()
	for _, id := range missing {
		rr, err := h.getRunRow(ctx, tx, id)
		if err != nil {
			return err
		}
		if rr.State == contracts.RunStarting && rr.StartCommandID != "" {
			if c, err := h.getCommand(ctx, tx, rr.StartCommandID); err == nil && !c.finished() {
				continue // launch not yet executed by the node
			}
		}
		if err := h.setRunStateTx(ctx, tx, rr, contracts.RunUnknown, "node did not report this run on reconnect", "node:"+nodeID); err != nil {
			return err
		}
	}
	return nil
}

// HandleDisconnect records that a node's control connection ended. Runs are
// not changed; views report them stale.
func (h *Hub) HandleDisconnect(ctx context.Context, nodeID string) error {
	return h.tx(ctx, func(tx *sql.Tx) error {
		now := h.now()
		if err := h.ensureNodeRow(ctx, tx, nodeID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE node_state SET connected=0, last_seen_at=?, updated_at=? WHERE node_id=?`, persistence.Time(now), persistence.Time(now), nodeID); err != nil {
			return err
		}
		// Sent-but-unacknowledged commands are resent after reconnect.
		if _, err := tx.ExecContext(ctx, `UPDATE commands SET state=? WHERE node_id=? AND state=?`, cmdQueued, nodeID, cmdSent); err != nil {
			return err
		}
		return h.emitConnection(ctx, tx, nodeID, contracts.NodeConnection{State: contracts.ConnDisconnected, LastSeenAt: &now})
	})
}

// emitConnection emits node.connection (payload NodeConnection) only when
// no fleet domain is configured; otherwise the fleet registry owns
// connection observations and events.
func (h *Hub) emitConnection(ctx context.Context, tx *sql.Tx, nodeID string, c contracts.NodeConnection) error {
	if h.cfg.Fleet != nil {
		return nil
	}
	return h.emitPayload(ctx, tx, contracts.EvNodeConnection, contracts.ObjectRef{Kind: contracts.KindNode, ID: nodeID}, "", nil, "hub", 0, c)
}

// WelcomeCommands returns, in Seq order, the queued commands to send right
// after the Welcome frame (call after HandleHello). They are marked sent; a
// failed write ends the connection and HandleDisconnect re-queues them.
// Commands that expired while queued are expired instead (never sent).
func (h *Hub) WelcomeCommands(ctx context.Context, nodeID string) ([]contracts.NodeCommand, error) {
	unlock := h.nodeLock(nodeID)
	defer unlock()
	rows, err := h.db.QueryContext(ctx, `SELECT `+cmdCols+` FROM commands WHERE node_id=? AND state=? ORDER BY seq LIMIT 1000`, nodeID, cmdQueued)
	if err != nil {
		return nil, err
	}
	var recs []commandRec
	for rows.Next() {
		r, err := scanCommand(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		recs = append(recs, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Mint session tokens before the transaction: the auth store writes on
	// its own connection, which would wait on this transaction's write lock
	// until busy_timeout and fail every hello with a queued launch.
	now := h.now()
	prepared := make(map[string]contracts.NodeCommand, len(recs))
	for _, r := range recs {
		if r.expiredUnsent(now) {
			continue
		}
		cmd, err := h.prepareForSend(ctx, r)
		if err != nil {
			return nil, err
		}
		prepared[r.CommandID] = cmd
	}
	var out []contracts.NodeCommand
	err = sqlitex.Tx(ctx, h.db, func(tx *sql.Tx) error {
		out = out[:0]
		for _, r := range recs {
			cmd, ok := prepared[r.CommandID]
			if !ok {
				if err := h.expireCommandTx(ctx, tx, r); err != nil {
					return err
				}
				continue
			}
			if _, err := tx.ExecContext(ctx, `UPDATE commands SET state=?, sent_at=? WHERE command_id=? AND state=?`, cmdSent, persistence.Time(h.now()), r.CommandID, cmdQueued); err != nil {
				return err
			}
			out = append(out, cmd)
		}
		return nil
	})
	h.events.Notify()
	h.cmdBC.Broadcast()
	return out, err
}

// isReject reports whether err is a permanent problem with a report's
// content (recorded and acknowledged so the node stops resending it).
func isReject(err error) bool {
	var ce *contracts.Error
	if !errors.As(err, &ce) {
		return false
	}
	switch ce.Code {
	case contracts.ErrInvalid, contracts.ErrForbidden, contracts.ErrConflict, contracts.ErrNotFound, contracts.ErrUnsupported, contracts.ErrPayloadTooLarge:
		return true
	}
	return false
}

// HandleReport ingests one node report inside a transaction. Durable
// reports are deduplicated by (node_id, origin_seq). A report whose content
// is permanently invalid is recorded in report_rejects and acknowledged; an
// internal failure returns an error and is not acknowledged (the node
// resends).
func (h *Hub) HandleReport(ctx context.Context, nodeID string, rep contracts.NodeReport) (contracts.ReportAck, error) {
	var ack contracts.ReportAck
	// The connection catalog owns its own transactions. A connections
	// report is a replaceable per-node snapshot, so applying it before the
	// dedup commit is idempotent (a resend applies it again harmlessly).
	var preErr error
	if rep.Kind == contracts.ReportConnections && h.cfg.Connections != nil {
		cs, err := decode[[]contracts.Connection](rep.Payload)
		if err == nil {
			err = h.cfg.Connections.ApplyReport(ctx, nodeID, cs)
		}
		if err != nil && !isReject(err) {
			return ack, err
		}
		preErr = err
	}
	err := h.tx(ctx, func(tx *sql.Tx) error {
		now := h.now()
		if err := h.ensureNodeRow(ctx, tx, nodeID); err != nil {
			return err
		}
		dup := false
		if rep.OriginSeq > 0 {
			res, err := tx.ExecContext(ctx, `INSERT INTO node_reports(node_id,origin_seq,kind,processed_at) VALUES(?,?,?,?) ON CONFLICT DO NOTHING`,
				nodeID, int64(rep.OriginSeq), string(rep.Kind), persistence.Time(now))
			if err != nil {
				return err
			}
			n, _ := res.RowsAffected()
			dup = n == 0
		}
		if !dup {
			if _, err := tx.ExecContext(ctx, `SAVEPOINT report`); err != nil {
				return err
			}
			aerr := preErr
			if aerr == nil && !(rep.Kind == contracts.ReportConnections && h.cfg.Connections != nil) {
				aerr = h.applyReportTx(ctx, tx, nodeID, rep)
			}
			if aerr != nil {
				if _, err := tx.ExecContext(ctx, `ROLLBACK TO report`); err != nil {
					return err
				}
				if !isReject(aerr) {
					return aerr
				}
				if _, err := tx.ExecContext(ctx, `INSERT INTO report_rejects(node_id,origin_seq,kind,error,payload,at) VALUES(?,?,?,?,?,?)`,
					nodeID, int64(rep.OriginSeq), string(rep.Kind), aerr.Error(), string(rep.Payload), persistence.Time(now)); err != nil {
					return err
				}
			}
			if _, err := tx.ExecContext(ctx, `RELEASE report`); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE node_state SET committed_report_seq=MAX(committed_report_seq, ?), last_seen_at=?, updated_at=? WHERE node_id=?`,
			int64(rep.OriginSeq), persistence.Time(now), persistence.Time(now), nodeID); err != nil {
			return err
		}
		var c int64
		if err := tx.QueryRowContext(ctx, `SELECT committed_report_seq FROM node_state WHERE node_id=?`, nodeID).Scan(&c); err != nil {
			return err
		}
		ack.CommittedSeq = uint64(c)
		return nil
	})
	return ack, err
}

func decode[T any](raw contracts.RawJSON) (T, error) {
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		return v, contracts.Errorf(contracts.ErrInvalid, "malformed %T payload: %v", v, err)
	}
	return v, nil
}

func (h *Hub) applyReportTx(ctx context.Context, tx *sql.Tx, nodeID string, rep contracts.NodeReport) error {
	origin := "node:" + nodeID
	seq := rep.OriginSeq
	switch rep.Kind {
	case contracts.ReportStatus:
		st, err := decode[contracts.NodeStatusReport](rep.Payload)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE node_state SET status=? WHERE node_id=?`, persistence.JSON(st), nodeID)
		return err
	case contracts.ReportRun:
		r, err := decode[contracts.Run](rep.Payload)
		if err != nil {
			return err
		}
		return h.observeRunTx(ctx, tx, nodeID, r, seq, "report")
	case contracts.ReportReceipt:
		rc, err := decode[contracts.DeliveryReceipt](rep.Payload)
		if err != nil {
			return err
		}
		if rc.OriginSeq == 0 {
			rc.OriginSeq = seq
		}
		if rc.RunID != "" {
			if run, err := h.getRun(ctx, tx, rc.RunID); err == nil && run.NodeID != nodeID {
				return contracts.Errorf(contracts.ErrForbidden, "node %s cannot report receipts for run %s", nodeID, rc.RunID)
			}
		}
		_, err = h.msgs.ApplyReceiptTx(ctx, tx, nodeID, rc)
		return err
	case contracts.ReportApproval:
		a, err := decode[contracts.Approval](rep.Payload)
		if err != nil {
			return err
		}
		return h.ingestApprovalTx(ctx, tx, nodeID, a, seq)
	case contracts.ReportCommandResult:
		res, err := decode[contracts.CommandResult](rep.Payload)
		if err != nil {
			return err
		}
		return h.completeCommandTx(ctx, tx, nodeID, res, seq)
	case contracts.ReportAccountObs:
		o, err := decode[contracts.AccountObservation](rep.Payload)
		if err != nil {
			return err
		}
		return h.ingestObservationTx(ctx, tx, nodeID, o, seq)
	case contracts.ReportGrant:
		g, err := decode[contracts.DeviceGrant](rep.Payload)
		if err != nil {
			return err
		}
		return h.ingestGrantTx(ctx, tx, nodeID, g, seq)
	case contracts.ReportAssignment:
		a, err := decode[contracts.AccountAssignment](rep.Payload)
		if err != nil {
			return err
		}
		return h.ingestAssignmentTx(ctx, tx, nodeID, a, seq)
	case contracts.ReportFallback:
		f, err := decode[contracts.FallbackPlan](rep.Payload)
		if err != nil {
			return err
		}
		return h.ingestFallbackTx(ctx, tx, nodeID, f, seq)
	case contracts.ReportEnvironment:
		g, err := decode[contracts.EnvironmentGeneration](rep.Payload)
		if err != nil {
			return err
		}
		if g.GenerationID == "" {
			return contracts.Errorf(contracts.ErrInvalid, "environment generation needs generation_id")
		}
		if g.NodeID == "" {
			g.NodeID = nodeID
		}
		if g.NodeID != nodeID {
			return contracts.Errorf(contracts.ErrForbidden, "node %s cannot report environment for %s", nodeID, g.NodeID)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO environments(generation_id,node_id,generation,created_at) VALUES(?,?,?,?) ON CONFLICT(generation_id) DO NOTHING`,
			g.GenerationID, nodeID, persistence.JSON(g), persistence.Time(g.CreatedAt)); err != nil {
			return err
		}
		return h.emitPayload(ctx, tx, contracts.EvEnvironmentApplied, contracts.ObjectRef{Kind: contracts.KindEnvironment, ID: g.GenerationID}, "", g.ProjectID, origin, seq, g)
	case contracts.ReportConnections:
		cs, err := decode[[]contracts.Connection](rep.Payload)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO node_connections(node_id,connections,reported_at) VALUES(?,?,?) ON CONFLICT(node_id) DO UPDATE SET connections=excluded.connections, reported_at=excluded.reported_at`,
			nodeID, persistence.JSON(cs), persistence.Time(h.now())); err != nil {
			return err
		}
		return nil
	case contracts.ReportEvent:
		ev, err := decode[contracts.Event](rep.Payload)
		if err != nil {
			return err
		}
		if ev.Kind == "" {
			return contracts.Errorf(contracts.ErrInvalid, "event kind is required")
		}
		ev.EventID = "" // hub-assigned
		ev.Cursor = 0
		ev.Origin = origin
		ev.OriginSeq = seq
		if ev.Kind == autotitle.EventTitleSuggested { // W-names
			return h.applyTitleSuggestionTx(ctx, tx, nodeID, ev, origin, seq)
		}
		if dup, err := h.duplicateExhaustionTx(ctx, tx, ev); err != nil || dup {
			return err
		}
		return h.emit(ctx, tx, ev)
	case contracts.ReportRunSnapshot:
		snap, err := decode[contracts.RunSnapshot](rep.Payload)
		if err != nil {
			return err
		}
		return h.applyRunSnapshotTx(ctx, tx, nodeID, snap, seq)
	case contracts.ReportSessionActivity:
		a, err := decode[contracts.SessionActivity](rep.Payload)
		if err != nil {
			return err
		}
		if a.SessionID == "" {
			return contracts.Errorf(contracts.ErrInvalid, "activity needs session_id")
		}
		if a.RunID != "" {
			if run, err := h.getRun(ctx, tx, a.RunID); err == nil && run.NodeID != nodeID {
				return contracts.Errorf(contracts.ErrForbidden, "node %s cannot report activity for run %s", nodeID, a.RunID)
			}
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO session_activity(session_id,run_id,line,at) VALUES(?,?,?,?) ON CONFLICT(session_id) DO UPDATE SET run_id=excluded.run_id, line=excluded.line, at=excluded.at`,
			a.SessionID, persistence.NullStr(a.RunID), a.Line, persistence.Time(a.At))
		return err
	}
	if ok, err := h.applyRepoReportTx(ctx, tx, nodeID, rep); ok { // W-quicklaunch
		return err
	}
	return contracts.Errorf(contracts.ErrUnsupported, "unknown report kind %q", rep.Kind)
}

// NodeReportRejects returns recorded rejected reports for diagnostics.
func (h *Hub) NodeReportRejects(ctx context.Context, nodeID string) ([]string, error) {
	rows, err := h.db.QueryContext(ctx, `SELECT kind, origin_seq, error FROM report_rejects WHERE node_id=? ORDER BY id`, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var kind, msg string
		var seq int64
		if err := rows.Scan(&kind, &seq, &msg); err != nil {
			return nil, err
		}
		out = append(out, fmt.Sprintf("%s#%d: %s", kind, seq, msg))
	}
	return out, rows.Err()
}
