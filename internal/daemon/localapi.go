package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/buildinfo"
	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

// LocalAPI is contracts.API served on the node's Unix socket. Every call is
// proxied to the hub with the caller's bearer. When the hub is unavailable,
// this node's sessions remain listable/readable/attachable from local state
// (marked stale, source "node-local"), SendMessage commits to the durable
// local outbox (storage pending_local) and is flushed later with the same
// operation and message IDs, and hub-only operations return ErrUnavailable.
type LocalAPI struct {
	proxy
}

var _ contracts.API = (*LocalAPI)(nil)

// LocalAPI returns the local API implementation.
func (d *Daemon) LocalAPI() *LocalAPI { return &LocalAPI{proxy{d: d}} }

func hubDown(err error) bool { return contracts.IsCode(err, contracts.ErrUnavailable) }

func (l *LocalAPI) localFreshness() contracts.Freshness {
	return contracts.Freshness{ObservedAt: l.d.now(), Source: "node-local:" + l.d.nodeID, Stale: true,
		Reason: "hub unavailable; showing this node's local state only"}
}

func (l *LocalAPI) Hello(ctx context.Context, r contracts.HelloRequest) (contracts.HelloResponse, error) {
	resp, err := l.proxy.Hello(ctx, r)
	if err == nil || !hubDown(err) {
		return resp, err
	}
	p, _ := contracts.PrincipalFrom(ctx)
	return contracts.HelloResponse{Protocol: contracts.ProtocolVersion, MinProtocol: contracts.MinProtocolVersion,
		HubVersion: "pyred-local/" + buildinfo.Version, ServerTime: l.d.now(), Principal: p,
		Features: []string{"node-local", "hub-unavailable"}}, nil
}

func (l *LocalAPI) WhoAmI(ctx context.Context) (contracts.Principal, error) {
	p, err := l.proxy.WhoAmI(ctx)
	if err == nil || !hubDown(err) {
		return p, err
	}
	if lp, ok := contracts.PrincipalFrom(ctx); ok {
		return lp, nil
	}
	return contracts.Principal{}, err
}

// localViews returns the latest run of each session on this node.
func (l *LocalAPI) localViews(ctx context.Context) ([]contracts.SessionView, error) {
	recs, err := l.d.st.runs(ctx, "")
	if err != nil {
		return nil, err
	}
	latest := map[string]runRecord{}
	for _, r := range recs { // ordered by created_at
		latest[r.Run.SessionID] = r
	}
	fr := l.localFreshness()
	var out []contracts.SessionView
	for _, r := range latest {
		if lr, ok := l.d.getLive(r.Run.RunID); ok {
			r, _ = lr.snapshot()
		}
		run := r.Run
		out = append(out, contracts.SessionView{Session: r.Session, Run: &run, NodeName: l.d.cfg.NodeName, Freshness: fr})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Session.CreatedAt.After(out[j].Session.CreatedAt) })
	return out, nil
}

func (l *LocalAPI) ListSessions(ctx context.Context, r contracts.ListSessionsRequest) (contracts.Page[contracts.SessionView], error) {
	page, err := l.proxy.ListSessions(ctx, r)
	if err == nil || !hubDown(err) {
		return page, err
	}
	views, lerr := l.localViews(ctx)
	if lerr != nil {
		return page, lerr
	}
	out := contracts.Page[contracts.SessionView]{Items: []contracts.SessionView{}, SnapshotAt: l.d.now()}
	if r.NodeID != "" && r.NodeID != l.d.nodeID {
		return out, nil
	}
	q := strings.ToLower(r.Query)
	limit := r.Limit
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	for _, v := range views {
		if v.Session.Archived && !r.IncludeArchived {
			continue
		}
		if r.Harness != "" && v.Session.Harness != r.Harness {
			continue
		}
		if r.ProjectID != nil {
			pid := ""
			if v.Session.ProjectID != nil {
				pid = *v.Session.ProjectID
			}
			if pid != *r.ProjectID {
				continue
			}
		}
		if len(r.States) > 0 {
			match := false
			for _, s := range r.States {
				if v.Run != nil && v.Run.State == s {
					match = true
				}
			}
			if !match {
				continue
			}
		}
		if q != "" && !strings.Contains(strings.ToLower(v.Session.Title+" "+v.Session.SessionID), q) {
			continue
		}
		out.Items = append(out.Items, v)
		if len(out.Items) >= limit {
			break
		}
	}
	return out, nil
}

func (l *LocalAPI) GetSession(ctx context.Context, r contracts.GetRequest) (contracts.SessionView, error) {
	v, err := l.proxy.GetSession(ctx, r)
	if err == nil || !hubDown(err) {
		return v, err
	}
	views, lerr := l.localViews(ctx)
	if lerr != nil {
		return v, lerr
	}
	for _, sv := range views {
		if sv.Session.SessionID == r.ID {
			return sv, nil
		}
	}
	return v, contracts.Errorf(contracts.ErrUnavailable, "hub unavailable and session is not on this node").WithRef(contracts.KindSession, r.ID)
}

func (l *LocalAPI) ReadSession(ctx context.Context, r contracts.ReadSessionRequest) (contracts.ReadSessionResponse, error) {
	resp, err := l.proxy.ReadSession(ctx, r)
	if err == nil || !hubDown(err) {
		return resp, err
	}
	lresp, lerr := l.d.readTranscript(ctx, r)
	if lerr != nil {
		if contracts.IsCode(lerr, contracts.ErrNotFound) {
			return resp, err
		}
		return resp, lerr
	}
	lresp.Freshness.Source = "node-local:" + l.d.nodeID
	lresp.Freshness.Reason = "hub unavailable; read from this node"
	return lresp, nil
}

func (l *LocalAPI) AttachSession(ctx context.Context, r contracts.AttachRequest) (contracts.AttachDescriptor, error) {
	desc, err := l.proxy.AttachSession(ctx, r)
	if err == nil || !hubDown(err) {
		return desc, err
	}
	ldesc, lerr := l.d.attach(ctx, contracts.AttachCommand{SessionID: r.SessionID, ClientNodeID: l.d.nodeID, Preferred: r.Preferred, ReadOnly: r.ReadOnly})
	if lerr != nil {
		if contracts.IsCode(lerr, contracts.ErrNotFound) {
			return desc, err
		}
		return desc, lerr
	}
	ldesc.Notes = strings.TrimSpace(ldesc.Notes + " (hub unavailable: local attach; input lease not coordinated by the hub)")
	return ldesc, nil
}

func (l *LocalAPI) ListRuns(ctx context.Context, r contracts.ListRunsRequest) (contracts.Page[contracts.Run], error) {
	page, err := l.proxy.ListRuns(ctx, r)
	if err == nil || !hubDown(err) {
		return page, err
	}
	recs, lerr := l.d.st.runs(ctx, `session_id=?`, r.SessionID)
	if lerr != nil {
		return page, lerr
	}
	out := contracts.Page[contracts.Run]{Items: []contracts.Run{}, SnapshotAt: l.d.now()}
	for _, rec := range recs {
		if lr, ok := l.d.getLive(rec.Run.RunID); ok {
			rec, _ = lr.snapshot()
		}
		run := rec.Run
		run.Freshness = l.localFreshness()
		out.Items = append(out.Items, run)
	}
	return out, nil
}

// ---- messages via the local outbox ----

type outboxRow struct {
	Seq       int64
	MessageID string
	Principal contracts.Principal
	Request   contracts.SendMessageRequest
	Digest    string
	State     string
	Attempts  int
	LastError string
	CreatedAt time.Time
	Result    *contracts.MessageView
}

func scanOutbox(sc interface{ Scan(...any) error }) (outboxRow, error) {
	var o outboxRow
	var p, req, res []byte
	var created string
	if err := sc.Scan(&o.Seq, &o.MessageID, &p, &req, &o.Digest, &o.State, &o.Attempts, &o.LastError, &created, &res); err != nil {
		return o, err
	}
	if err := json.Unmarshal(p, &o.Principal); err != nil {
		return o, err
	}
	if err := json.Unmarshal(req, &o.Request); err != nil {
		return o, err
	}
	if len(res) > 0 {
		var v contracts.MessageView
		if json.Unmarshal(res, &v) == nil {
			o.Result = &v
		}
	}
	o.CreatedAt, _ = sqlitex.ParseTime(created)
	return o, nil
}

const outboxCols = `seq, message_id, principal, request, digest, state, attempts, last_error, created_at, result`

func (l *LocalAPI) outboxView(o outboxRow) contracts.MessageView {
	if o.Result != nil {
		return *o.Result
	}
	r := o.Request
	mode := r.Mode
	if mode == "" {
		mode = contracts.ModeQueue
	}
	env := contracts.MessageEnvelope{SchemaVersion: 1, MessageID: o.MessageID, OperationID: r.OperationID, Sender: o.Principal,
		Recipient: r.Recipient, ProjectID: r.ProjectID, ThreadID: r.ThreadID, InReplyTo: r.InReplyTo, Mode: mode, Body: r.Body,
		Attachments: r.Attachments, CreatedAt: o.CreatedAt, RequestAck: r.RequestAck}
	if r.TTLSeconds > 0 {
		exp := o.CreatedAt.Add(time.Duration(r.TTLSeconds) * time.Second)
		env.ExpiresAt = &exp
	}
	st := contracts.DeliveryStatus{Storage: contracts.StoragePendingLocal, Runtime: contracts.DeliveryQueued,
		RuntimeReason: "hub unreachable; held in node outbox " + l.d.nodeID, Ack: contracts.AckNone, UpdatedAt: l.d.now()}
	if o.State == "rejected" {
		st.Storage = contracts.StorageRejected
		st.Runtime = contracts.DeliveryNotApplicable
		st.RuntimeReason = "hub rejected the outbox message: " + o.LastError
	}
	return contracts.MessageView{Envelope: env, Status: st}
}

func (l *LocalAPI) SendMessage(ctx context.Context, r contracts.SendMessageRequest) (contracts.MessageView, error) {
	p, ok := contracts.PrincipalFrom(ctx)
	if !ok {
		return contracts.MessageView{}, contracts.Errorf(contracts.ErrUnauthorized, "no principal")
	}
	if r.OperationID == "" {
		return contracts.MessageView{}, contracts.Errorf(contracts.ErrInvalid, "operation_id is required")
	}
	digest, err := sqlitex.RequestDigest(r)
	if err != nil {
		return contracts.MessageView{}, err
	}
	d := l.d
	// Same operation already in the outbox: return it (or conflict).
	if o, found, err := l.outboxByOp(ctx, r.OperationID); err != nil {
		return contracts.MessageView{}, err
	} else if found {
		if o.Digest != digest || o.Principal.Kind != p.Kind || o.Principal.ID != p.ID {
			return contracts.MessageView{}, contracts.Errorf(contracts.ErrConflict, "operation_id reused with a different request").WithRef(contracts.KindOperation, r.OperationID)
		}
		return l.outboxView(o), nil
	}
	// A stable message ID is fixed before the first attempt so a retry
	// through the outbox can never create a second logical message.
	if r.MessageID == "" {
		r.MessageID = contracts.NewID(contracts.PrefixMessage)
	}
	var pending int
	if err := d.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM daemon_outbox WHERE state='pending'`).Scan(&pending); err != nil {
		return contracts.MessageView{}, err
	}
	if pending == 0 { // keep per-origin order: never overtake queued outbox mail
		v, err := l.proxy.SendMessage(ctx, r)
		if err == nil || !hubDown(err) {
			return v, err
		}
	}
	if pending >= d.cfg.MaxOutbox {
		return contracts.MessageView{}, contracts.Errorf(contracts.ErrCapacityExhausted, "node outbox full (%d messages waiting for the hub)", pending)
	}
	pb, _ := json.Marshal(p)
	rb, _ := json.Marshal(r)
	now := d.now()
	_, err = d.db.ExecContext(ctx, `INSERT INTO daemon_outbox(message_id, operation_id, principal, request, digest, state, created_at, updated_at) VALUES(?,?,?,?,?,?,?,?)`,
		r.MessageID, r.OperationID, pb, rb, digest, "pending", ftime(now), ftime(now))
	if err != nil {
		return contracts.MessageView{}, err
	}
	o, _, err := l.outboxByOp(ctx, r.OperationID)
	if err != nil {
		return contracts.MessageView{}, err
	}
	d.kickOutbox()
	return l.outboxView(o), nil
}

func (l *LocalAPI) outboxByOp(ctx context.Context, opID string) (outboxRow, bool, error) {
	o, err := scanOutbox(l.d.db.QueryRowContext(ctx, `SELECT `+outboxCols+` FROM daemon_outbox WHERE operation_id=?`, opID))
	if errors.Is(err, sql.ErrNoRows) {
		return o, false, nil
	}
	return o, err == nil, err
}

func (l *LocalAPI) GetMessage(ctx context.Context, r contracts.GetRequest) (contracts.MessageView, error) {
	v, err := l.proxy.GetMessage(ctx, r)
	if err == nil || !hubDown(err) {
		return v, err
	}
	o, lerr := scanOutbox(l.d.db.QueryRowContext(ctx, `SELECT `+outboxCols+` FROM daemon_outbox WHERE message_id=?`, r.ID))
	if lerr != nil {
		return v, err
	}
	return l.outboxView(o), nil
}

func (d *Daemon) kickOutbox() {
	select {
	case d.outboxKick <- struct{}{}:
	default:
	}
}

// KickOutbox triggers an immediate flush attempt (e.g. on hub reconnect).
func (d *Daemon) KickOutbox() { d.kickOutbox() }

func (d *Daemon) outboxLoop(ctx context.Context) {
	t := time.NewTicker(d.cfg.OutboxFlush)
	defer t.Stop()
	for {
		d.FlushOutbox(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-d.outboxKick:
		}
	}
}

// FlushOutbox sends pending local messages to the hub in order with their
// original operation and message IDs (the hub deduplicates). It stops at
// the first unavailable error to preserve order.
func (d *Daemon) FlushOutbox(ctx context.Context) int {
	if d.cfg.Upstream == nil {
		return 0
	}
	rows, err := d.db.QueryContext(ctx, `SELECT `+outboxCols+` FROM daemon_outbox WHERE state='pending' AND (next_at='' OR next_at<=?) ORDER BY seq LIMIT 100`, ftime(d.now()))
	if err != nil {
		return 0
	}
	var batch []outboxRow
	for rows.Next() {
		if o, err := scanOutbox(rows); err == nil {
			batch = append(batch, o)
		}
	}
	rows.Close()
	sent := 0
	for _, o := range batch {
		bearer, ok := d.bearerFor(ctx, o.Principal)
		if !ok {
			d.finishOutbox(ctx, o, "rejected", nil, "no local credential to send as "+string(o.Principal.Kind)+":"+o.Principal.ID)
			continue
		}
		up, err := d.upstream(bearer)
		if err != nil {
			return sent
		}
		v, err := up.SendMessage(contracts.WithPrincipal(ctx, o.Principal), o.Request)
		switch {
		case err == nil:
			d.finishOutbox(ctx, o, "stored", &v, "")
			sent++
		case hubDown(err) || contracts.IsCode(err, contracts.ErrCapacityExhausted):
			backoff := time.Duration(1<<min(o.Attempts, 6)) * time.Second
			_, _ = d.db.ExecContext(ctx, `UPDATE daemon_outbox SET attempts=attempts+1, last_error=?, next_at=?, updated_at=? WHERE message_id=?`,
				redact(err.Error()), ftime(d.now().Add(backoff)), ftime(d.now()), o.MessageID)
			return sent
		default:
			d.finishOutbox(ctx, o, "rejected", nil, redact(err.Error()))
		}
	}
	return sent
}

func (d *Daemon) finishOutbox(ctx context.Context, o outboxRow, state string, v *contracts.MessageView, errMsg string) {
	var res []byte
	if v != nil {
		res, _ = json.Marshal(v)
	}
	if _, err := d.db.ExecContext(ctx, `UPDATE daemon_outbox SET state=?, result=?, last_error=?, attempts=attempts+1, updated_at=? WHERE message_id=?`,
		state, res, errMsg, ftime(d.now()), o.MessageID); err != nil {
		d.noteErr("outbox update %s: %v", o.MessageID, err)
	}
	if state == "rejected" {
		d.noteErr("hub rejected outbox message %s: %s", o.MessageID, errMsg)
	}
}

// bearerFor returns the credential to use for an outbox entry's sender.
func (d *Daemon) bearerFor(ctx context.Context, p contracts.Principal) (string, bool) {
	switch p.Kind {
	case contracts.PrincipalSession:
		tok, ok, err := d.st.tokenForSession(ctx, p.ID)
		return tok, ok && err == nil
	case contracts.PrincipalOwner:
		return d.cfg.OwnerToken, d.cfg.OwnerToken != ""
	}
	return "", false
}
