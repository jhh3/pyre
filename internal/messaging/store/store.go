// Package store is the hub's durable message store. It implements
// contracts.MessageAPI with the delivery rules of docs/contracts.md §5:
//
//   - The sender is the authenticated principal from ctx; bodies never claim
//     identity, so a session cannot spoof another sender.
//   - Storage is deduplicated by message_id: the same ID with the same content
//     returns the existing message; different content is a conflict.
//   - Every mutation is idempotent by operation_id.
//   - Storage, runtime delivery, acknowledgement and work outcome are kept as
//     separate facts.
//   - The first delivery attempt binds a message to one run permanently.
//     accepted is final; indeterminate resolves only through a later evidence
//     receipt or an explicit ResolveDelivery; uncertain work is re-sent only
//     by ReissueMessage, which creates a new message referencing the original.
//   - Queues are bounded: a full recipient queue returns capacity_exhausted
//     instead of dropping work. Expiry produces a terminal "expired" record.
//
// Routing to nodes is not done here: the hub supplies hooks that run inside
// the storing transaction (OnStored) and after commit (AfterCommit).
package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/messaging/ownermail"
	"github.com/jhh3/pyre/internal/persistence"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

// Limits bound message storage.
type Limits struct {
	MaxQueuedPerRecipient int           // undelivered messages per recipient session (default 1000)
	MaxQueuedTotal        int           // undelivered messages hub-wide (default 100000)
	MaxBodyBytes          int           // default 64 KiB
	MaxAttachments        int           // default 32
	DefaultTTL            time.Duration // default 7 days
	MaxTTL                time.Duration // default 30 days
}

func (l *Limits) defaults() {
	if l.MaxQueuedPerRecipient <= 0 {
		l.MaxQueuedPerRecipient = 1000
	}
	if l.MaxQueuedTotal <= 0 {
		l.MaxQueuedTotal = 100_000
	}
	if l.MaxBodyBytes <= 0 {
		l.MaxBodyBytes = 64 << 10
	}
	if l.MaxAttachments <= 0 {
		l.MaxAttachments = 32
	}
	if l.DefaultTTL <= 0 {
		l.DefaultTTL = 7 * 24 * time.Hour
	}
	if l.MaxTTL <= 0 {
		l.MaxTTL = 30 * 24 * time.Hour
	}
	if l.DefaultTTL > l.MaxTTL {
		l.DefaultTTL = l.MaxTTL
	}
}

// Config wires a Store.
type Config struct {
	DB     *sql.DB
	Clock  contracts.Clock
	Events contracts.EventSink
	Limits Limits
	// PollInterval bounds long-poll latency for commits made without
	// Notify. Default 5s (waiters wake on Notify).
	PollInterval time.Duration

	// ValidateRecipient checks a recipient inside the storing tx (e.g. the
	// session exists). nil accepts any well-formed address.
	ValidateRecipient func(ctx context.Context, tx *sql.Tx, addr contracts.Address) error
	// OnStored runs inside the storing tx for a newly stored message whose
	// runtime state is queued. The hub routes it (queues a node command, or
	// marks it rejected via SetRuntimeTx).
	OnStored func(ctx context.Context, tx *sql.Tx, v contracts.MessageView) error
	// OnWithdrawn runs inside the tx that cancels or expires a message so the
	// hub can withdraw outstanding deliver commands.
	OnWithdrawn func(ctx context.Context, tx *sql.Tx, v contracts.MessageView) error
	// AfterCommit runs after any committed change (wake event readers, flush
	// node commands).
	AfterCommit func()
}

// Store is the durable message store.
type Store struct {
	cfg Config
	bc  persistence.Broadcaster
}

var _ contracts.MessageAPI = (*Store)(nil)

// Open migrates the "messages" namespace (and shared operations table).
func Open(ctx context.Context, cfg Config) (*Store, error) {
	if cfg.DB == nil {
		return nil, errors.New("store: nil DB")
	}
	if cfg.Events == nil {
		return nil, errors.New("store: nil EventSink")
	}
	if cfg.Clock == nil {
		cfg.Clock = contracts.SystemClock{}
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 5 * time.Second
	}
	cfg.Limits.defaults()
	if err := sqlitex.Migrate(ctx, cfg.DB, sqlitex.OperationsMigration); err != nil {
		return nil, err
	}
	if err := sqlitex.Migrate(ctx, cfg.DB, Migrations...); err != nil {
		return nil, err
	}
	return &Store{cfg: cfg}, nil
}

// SetHooks replaces the routing hooks (used by the hub, which is constructed
// after the store).
func (s *Store) SetHooks(validate func(context.Context, *sql.Tx, contracts.Address) error,
	onStored func(context.Context, *sql.Tx, contracts.MessageView) error,
	onWithdrawn func(context.Context, *sql.Tx, contracts.MessageView) error,
	afterCommit func()) {
	s.cfg.ValidateRecipient = validate
	s.cfg.OnStored = onStored
	s.cfg.OnWithdrawn = onWithdrawn
	s.cfg.AfterCommit = afterCommit
}

// Notify wakes ListMessages long polls.
func (s *Store) Notify() { s.bc.Broadcast() }

func (s *Store) committed() {
	s.bc.Broadcast()
	if s.cfg.AfterCommit != nil {
		s.cfg.AfterCommit()
	}
}

func (s *Store) now() time.Time { return s.cfg.Clock.Now().UTC() }

func principal(ctx context.Context) (contracts.Principal, error) {
	p, ok := contracts.PrincipalFrom(ctx)
	if !ok || p.Kind == "" || p.ID == "" {
		return p, contracts.Errorf(contracts.ErrUnauthorized, "no authenticated principal")
	}
	return p, nil
}

// effective returns the principal a delegated actor acts for.
func effective(p contracts.Principal) contracts.Principal {
	if p.OnBehalfOf != nil {
		return *p.OnBehalfOf
	}
	return p
}

// SenderAddress maps an authenticated principal to the address used for
// outbox queries and reply linking. Owners share the "owner" address.
func SenderAddress(p contracts.Principal) (kind, id string) {
	e := effective(p)
	switch e.Kind {
	case contracts.PrincipalOwner:
		return string(contracts.AddrOwner), "owner"
	case contracts.PrincipalSession:
		return string(contracts.AddrSession), e.ID
	}
	return string(e.Kind), e.ID
}

func isOwner(p contracts.Principal) bool { return effective(p).Kind == contracts.PrincipalOwner }

// ---- row access ----

const msgCols = `message_id,recipient_kind,recipient_id,inbox_cursor,sender_kind,sender_id,envelope,storage,stored_at,runtime,runtime_reason,bound_run_id,ack,ack_ref,outcome_ref,updated_at,content_digest,consumed_at`

type row struct {
	view         contracts.MessageView
	senderKind   string
	senderID     string
	digest       string
	recipientKey string
}

type queryer interface {
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
	QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error)
}

func scanRow(sc interface{ Scan(...any) error }, extra ...any) (row, error) {
	var r row
	var id, rk, rid, env, storage, storedAt, runtime, ack, updated string
	var reason, bound, ackRef, outcome, consumed sql.NullString
	var cursor int64
	dst := append([]any{&id, &rk, &rid, &cursor, &r.senderKind, &r.senderID, &env, &storage, &storedAt, &runtime, &reason, &bound, &ack, &ackRef, &outcome, &updated, &r.digest, &consumed}, extra...)
	if err := sc.Scan(dst...); err != nil {
		return r, err
	}
	if err := json.Unmarshal([]byte(env), &r.view.Envelope); err != nil {
		return r, fmt.Errorf("store: decode envelope %s: %w", id, err)
	}
	st := persistence.ParseTime(storedAt)
	r.view.Status = contracts.DeliveryStatus{
		Storage:       contracts.StorageState(storage),
		StoredAt:      &st,
		Runtime:       contracts.RuntimeDeliveryState(runtime),
		RuntimeReason: reason.String,
		BoundRunID:    bound.String,
		Ack:           contracts.AckState(ack),
		AckRef:        ackRef.String,
		UpdatedAt:     persistence.ParseTime(updated),
		ConsumedAt:    persistence.ParseTimePtr(consumed),
	}
	if outcome.Valid && outcome.String != "" {
		var ref contracts.ObjectRef
		if err := json.Unmarshal([]byte(outcome.String), &ref); err == nil {
			r.view.Status.OutcomeRef = &ref
		}
	}
	r.view.Cursor = cursor
	r.recipientKey = rk + ":" + rid
	return r, nil
}

func (s *Store) loadReceipts(ctx context.Context, q queryer, v *contracts.MessageView) error {
	rows, err := q.QueryContext(ctx, `SELECT receipt FROM message_receipts WHERE message_id=? ORDER BY id`, v.Envelope.MessageID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var b string
		if err := rows.Scan(&b); err != nil {
			return err
		}
		var r contracts.DeliveryReceipt
		if err := json.Unmarshal([]byte(b), &r); err != nil {
			return err
		}
		v.Status.Receipts = append(v.Status.Receipts, r)
	}
	return rows.Err()
}

func (s *Store) get(ctx context.Context, q queryer, id string) (row, error) {
	r, err := scanRow(q.QueryRowContext(ctx, `SELECT `+msgCols+` FROM messages WHERE message_id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return r, contracts.Errorf(contracts.ErrNotFound, "message not found").WithRef(contracts.KindMessage, id)
	}
	if err != nil {
		return r, err
	}
	if err := s.loadReceipts(ctx, q, &r.view); err != nil {
		return r, err
	}
	return r, nil
}

// GetTx reads a message inside tx.
func (s *Store) GetTx(ctx context.Context, tx *sql.Tx, id string) (contracts.MessageView, error) {
	r, err := s.get(ctx, tx, id)
	return r.view, err
}

// emit appends a message.* event. The payload is the MessageView (pinned
// payload shape, contracts/events.go).
func (s *Store) emit(ctx context.Context, tx *sql.Tx, kind contracts.EventKind, v contracts.MessageView) error {
	ev := contracts.Event{
		Kind:      kind,
		Object:    contracts.ObjectRef{Kind: contracts.KindMessage, ID: v.Envelope.MessageID},
		ProjectID: v.Envelope.ProjectID,
	}
	if v.Envelope.Recipient.Kind == contracts.AddrSession {
		ev.SessionID = v.Envelope.Recipient.ID
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	ev.Payload = b
	_, err = s.cfg.Events.AppendTx(ctx, tx, ev)
	return err
}

func (s *Store) emitDelivery(ctx context.Context, tx *sql.Tx, v contracts.MessageView) error {
	return s.emit(ctx, tx, contracts.EvMessageDelivery, v)
}

// ---- send ----

type contentKey struct {
	Sender      string                 `json:"sender"`
	OnBehalfOf  string                 `json:"on_behalf_of,omitempty"`
	Recipient   contracts.Address      `json:"recipient"`
	ProjectID   *string                `json:"project_id"`
	ThreadID    string                 `json:"thread_id"`
	InReplyTo   string                 `json:"in_reply_to"`
	Reissues    string                 `json:"reissues"`
	Mode        contracts.MessageMode  `json:"mode"`
	Body        contracts.MessageBody  `json:"body"`
	Attachments []contracts.Attachment `json:"attachments"`
	TTLSeconds  int                    `json:"ttl_seconds"`
	RequestAck  bool                   `json:"request_ack"`
}

func contentDigest(k contentKey) string {
	if k.ProjectID != nil && *k.ProjectID == "" {
		k.ProjectID = nil
	}
	if len(k.Attachments) == 0 {
		k.Attachments = nil
	}
	b, _ := json.Marshal(k)
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func validMode(m contracts.MessageMode) bool {
	return m == contracts.ModeQueue || m == contracts.ModeSteer || m == contracts.ModeInfo
}

func validAddress(a contracts.Address) error {
	switch a.Kind {
	case contracts.AddrSession:
		if !contracts.ValidID(a.ID, contracts.PrefixSession) {
			return contracts.Errorf(contracts.ErrInvalid, "recipient session id %q is malformed", a.ID)
		}
	case contracts.AddrOwner:
		if a.ID != "owner" && a.ID != "" {
			return contracts.Errorf(contracts.ErrInvalid, "owner recipient id must be \"owner\"")
		}
	case contracts.AddrChannel, contracts.AddrProject:
		if a.ID == "" {
			return contracts.Errorf(contracts.ErrInvalid, "recipient id is required")
		}
	default:
		return contracts.Errorf(contracts.ErrInvalid, "unknown recipient kind %q", a.Kind)
	}
	return nil
}

func (s *Store) validateSend(req *contracts.SendMessageRequest) error {
	if req.OperationID == "" {
		return contracts.Errorf(contracts.ErrInvalid, "operation_id is required")
	}
	if req.Recipient.Kind == contracts.AddrOwner && req.Recipient.ID == "" {
		req.Recipient.ID = "owner"
	}
	if err := validAddress(req.Recipient); err != nil {
		return err
	}
	if req.Mode == "" {
		req.Mode = contracts.ModeQueue
	}
	if !validMode(req.Mode) {
		return contracts.Errorf(contracts.ErrInvalid, "unknown mode %q", req.Mode)
	}
	if req.MessageID != "" && !contracts.ValidID(req.MessageID, contracts.PrefixMessage) {
		return contracts.Errorf(contracts.ErrInvalid, "message_id %q is malformed", req.MessageID)
	}
	if strings.TrimSpace(req.Body.Text) == "" && len(req.Attachments) == 0 {
		return contracts.Errorf(contracts.ErrInvalid, "message body or attachments required")
	}
	if len(req.Body.Text) > s.cfg.Limits.MaxBodyBytes {
		return contracts.Errorf(contracts.ErrPayloadTooLarge, "message body is %d bytes; limit %d", len(req.Body.Text), s.cfg.Limits.MaxBodyBytes)
	}
	if len(req.Attachments) > s.cfg.Limits.MaxAttachments {
		return contracts.Errorf(contracts.ErrPayloadTooLarge, "%d attachments; limit %d", len(req.Attachments), s.cfg.Limits.MaxAttachments)
	}
	for _, a := range req.Attachments {
		n := 0
		if a.ArtifactVersionID != "" {
			n++
		}
		if a.DocumentRevisionID != "" {
			n++
		}
		if a.NoteID != "" {
			n++
		}
		if n != 1 {
			return contracts.Errorf(contracts.ErrInvalid, "each attachment must reference exactly one published object")
		}
	}
	if req.TTLSeconds < 0 {
		return contracts.Errorf(contracts.ErrInvalid, "ttl_seconds must be >= 0")
	}
	if time.Duration(req.TTLSeconds)*time.Second > s.cfg.Limits.MaxTTL {
		return contracts.Errorf(contracts.ErrInvalid, "ttl_seconds exceeds the hub maximum of %d", int(s.cfg.Limits.MaxTTL/time.Second))
	}
	if req.InReplyTo != "" && !contracts.ValidID(req.InReplyTo, contracts.PrefixMessage) {
		return contracts.Errorf(contracts.ErrInvalid, "in_reply_to %q is malformed", req.InReplyTo)
	}
	return nil
}

// SendMessage stores an addressed message and returns its delivery facts.
func (s *Store) SendMessage(ctx context.Context, req contracts.SendMessageRequest) (contracts.MessageView, error) {
	p, err := principal(ctx)
	if err != nil {
		return contracts.MessageView{}, err
	}
	if err := s.validateSend(&req); err != nil {
		return contracts.MessageView{}, err
	}
	var out contracts.MessageView
	_, err = persistence.Mutate(ctx, s.cfg.DB, req.OperationID, contracts.OpSendMessage, p, req, &out, func(tx *sql.Tx) (any, error) {
		id := req.MessageID
		if id == "" {
			id = contracts.NewID(contracts.PrefixMessage)
		}
		return s.insertTx(ctx, tx, p, id, req, "", false)
	})
	if err == nil {
		s.committed()
	}
	return out, err
}

// insertTx stores a new message (or returns the existing one with identical
// content). reissues/riskAccepted record an explicit reissue.
func (s *Store) insertTx(ctx context.Context, tx *sql.Tx, p contracts.Principal, id string, req contracts.SendMessageRequest, reissues string, riskAccepted bool) (contracts.MessageView, error) {
	sk, sid := SenderAddress(p)
	key := contentKey{
		Sender: persistence.PrincipalKey(p), Recipient: req.Recipient, ProjectID: req.ProjectID, ThreadID: req.ThreadID,
		InReplyTo: req.InReplyTo, Reissues: reissues, Mode: req.Mode, Body: req.Body, Attachments: req.Attachments,
		TTLSeconds: req.TTLSeconds, RequestAck: req.RequestAck,
	}
	if p.OnBehalfOf != nil {
		key.OnBehalfOf = persistence.PrincipalKey(*p.OnBehalfOf)
	}
	digest := contentDigest(key)

	existing, err := s.get(ctx, tx, id)
	switch {
	case err == nil:
		if existing.digest != digest {
			return contracts.MessageView{}, contracts.Errorf(contracts.ErrConflict, "message_id already stored with different content").WithRef(contracts.KindMessage, id)
		}
		return existing.view, nil
	case !contracts.IsCode(err, contracts.ErrNotFound):
		return contracts.MessageView{}, err
	}

	if s.cfg.ValidateRecipient != nil {
		if err := s.cfg.ValidateRecipient(ctx, tx, req.Recipient); err != nil {
			return contracts.MessageView{}, err
		}
	}

	runtime := contracts.DeliveryNotApplicable
	if req.Recipient.Kind == contracts.AddrSession && req.Mode != contracts.ModeInfo {
		runtime = contracts.DeliveryQueued
		if err := s.checkCapacity(ctx, tx, req.Recipient); err != nil {
			return contracts.MessageView{}, err
		}
	}

	now := s.now()
	ttl := s.cfg.Limits.DefaultTTL
	if req.TTLSeconds > 0 {
		ttl = time.Duration(req.TTLSeconds) * time.Second
	}
	var expires *time.Time
	if runtime == contracts.DeliveryQueued {
		e := now.Add(ttl)
		expires = &e
	}

	// Thread inheritance: a reply joins its parent's thread by default.
	var parent *row
	if req.InReplyTo != "" {
		pr, err := s.get(ctx, tx, req.InReplyTo)
		if err == nil {
			parent = &pr
			if req.ThreadID == "" {
				req.ThreadID = pr.view.Envelope.ThreadID
			}
		} else if !contracts.IsCode(err, contracts.ErrNotFound) {
			return contracts.MessageView{}, err
		}
	}
	projectID := req.ProjectID
	if projectID != nil && *projectID == "" {
		projectID = nil
	}

	env := contracts.MessageEnvelope{
		SchemaVersion: contracts.SchemaVersion,
		MessageID:     id,
		OperationID:   req.OperationID,
		Sender:        p,
		Recipient:     req.Recipient,
		ProjectID:     projectID,
		ThreadID:      req.ThreadID,
		InReplyTo:     req.InReplyTo,
		Reissues:      reissues,
		Mode:          req.Mode,
		Body:          req.Body,
		Attachments:   req.Attachments,
		CreatedAt:     now,
		ExpiresAt:     expires,
		RequestAck:    req.RequestAck,
	}

	var cursor int64
	if err := tx.QueryRowContext(ctx, `INSERT INTO message_inbox_heads(recipient_kind,recipient_id,last_cursor) VALUES(?,?,1)
		ON CONFLICT(recipient_kind,recipient_id) DO UPDATE SET last_cursor=last_cursor+1 RETURNING last_cursor`,
		string(req.Recipient.Kind), req.Recipient.ID).Scan(&cursor); err != nil {
		return contracts.MessageView{}, err
	}
	var riskBy any
	if riskAccepted {
		riskBy = persistence.JSON(p)
	}
	// Mail to the owner that asks nothing of him is stored acknowledged: an
	// update, never unacked inbox mail (internal/messaging/ownermail).
	ack, ackRef := contracts.AckNone, any(nil)
	if req.Recipient.Kind == contracts.AddrOwner && ownermail.Update(env, func() bool {
		return parent != nil && parent.view.Envelope.Sender.Kind == contracts.PrincipalOwner || s.ownerWaitsOnTx(ctx, tx, sk, sid, now)
	}) {
		ack, ackRef = contracts.AckAcknowledged, contracts.AckRefUpdate
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO messages(message_id,recipient_kind,recipient_id,inbox_cursor,sender_kind,sender_id,thread_id,project_id,in_reply_to,reissues,risk_accepted_by,mode,content_digest,envelope,storage,stored_at,runtime,runtime_reason,bound_run_id,ack,ack_ref,outcome_ref,expires_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,NULL,?,?,NULL,?,?)`,
		id, string(req.Recipient.Kind), req.Recipient.ID, cursor, sk, sid, persistence.NullStr(req.ThreadID), persistence.ProjectArg(projectID),
		persistence.NullStr(req.InReplyTo), persistence.NullStr(reissues), riskBy, string(req.Mode), digest, persistence.JSON(env),
		string(contracts.StorageStored), persistence.Time(now), string(runtime), nil, string(ack), ackRef,
		persistence.TimePtr(expires), persistence.Time(now)); err != nil {
		return contracts.MessageView{}, err
	}
	v, err := s.GetTx(ctx, tx, id)
	if err != nil {
		return v, err
	}
	if err := s.emit(ctx, tx, contracts.EvMessageStored, v); err != nil {
		return v, err
	}
	// Reply linking: a reply from the parent's recipient marks the parent
	// replied.
	if parent != nil {
		pk, pid := string(parent.view.Envelope.Recipient.Kind), parent.view.Envelope.Recipient.ID
		if pk == sk && pid == sid && parent.view.Status.Ack != contracts.AckReplied {
			if _, err := tx.ExecContext(ctx, `UPDATE messages SET ack=?, ack_ref=?, updated_at=? WHERE message_id=?`,
				string(contracts.AckReplied), id, persistence.Time(now), parent.view.Envelope.MessageID); err != nil {
				return v, err
			}
			pv, err := s.GetTx(ctx, tx, parent.view.Envelope.MessageID)
			if err != nil {
				return v, err
			}
			if err := s.emit(ctx, tx, contracts.EvMessageAck, pv); err != nil {
				return v, err
			}
		}
	}
	if runtime == contracts.DeliveryQueued && s.cfg.OnStored != nil {
		if err := s.cfg.OnStored(ctx, tx, v); err != nil {
			return v, err
		}
		return s.GetTx(ctx, tx, id)
	}
	return v, nil
}

// ownerAnswerWindow bounds how long an owner message to a session makes
// that session's next message to him an answer.
const ownerAnswerWindow = 24 * time.Hour

// ownerWaitsOnTx reports whether the owner wrote to session (sender kind
// sk, id sid) within ownerAnswerWindow and after its last message to him:
// what it sends him now answers him, even without reply_to.
func (s *Store) ownerWaitsOnTx(ctx context.Context, tx *sql.Tx, sk, sid string, now time.Time) bool {
	if sk != string(contracts.AddrSession) && sk != string(contracts.PrincipalSession) {
		return false
	}
	var n int
	// Both sides through messages_sender(sender_kind, sender_id, seq).
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM messages o WHERE o.sender_kind=? AND o.sender_id=? AND o.recipient_kind=? AND o.recipient_id=? AND o.stored_at>=?
		AND o.seq > COALESCE((SELECT MAX(seq) FROM messages m WHERE m.sender_kind=? AND m.sender_id=? AND m.recipient_kind=?), 0))`,
		string(contracts.AddrOwner), "owner", string(contracts.AddrSession), sid, persistence.Time(now.Add(-ownerAnswerWindow)),
		sk, sid, string(contracts.AddrOwner)).Scan(&n)
	return err == nil && n > 0
}

var pendingStates = []any{string(contracts.DeliveryQueued), string(contracts.DeliveryNodeReceived), string(contracts.DeliveryAttempting)}

func (s *Store) checkCapacity(ctx context.Context, tx *sql.Tx, to contracts.Address) error {
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE recipient_kind=? AND recipient_id=? AND runtime IN (?,?,?)`,
		append([]any{string(to.Kind), to.ID}, pendingStates...)...).Scan(&n); err != nil {
		return err
	}
	if n >= s.cfg.Limits.MaxQueuedPerRecipient {
		return contracts.Errorf(contracts.ErrCapacityExhausted, "recipient has %d undelivered messages (limit %d); retry later", n, s.cfg.Limits.MaxQueuedPerRecipient).
			WithRef(contracts.KindSession, to.ID)
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE runtime IN (?,?,?)`, pendingStates...).Scan(&n); err != nil {
		return err
	}
	if n >= s.cfg.Limits.MaxQueuedTotal {
		return contracts.Errorf(contracts.ErrCapacityExhausted, "hub has %d undelivered messages (limit %d); retry later", n, s.cfg.Limits.MaxQueuedTotal)
	}
	return nil
}

// ---- reads ----

// GetMessage returns one message with its delivery facts.
func (s *Store) GetMessage(ctx context.Context, req contracts.GetRequest) (contracts.MessageView, error) {
	if _, err := principal(ctx); err != nil {
		return contracts.MessageView{}, err
	}
	r, err := s.get(ctx, s.cfg.DB, req.ID)
	return r.view, err
}

// ListMessages lists an inbox, outbox, or thread in ascending hub order.
// AfterCursor (inbox only) and WaitSeconds support agents awaiting replies.
func (s *Store) ListMessages(ctx context.Context, req contracts.ListMessagesRequest) (contracts.Page[contracts.MessageView], error) {
	p, err := principal(ctx)
	if err != nil {
		return contracts.Page[contracts.MessageView]{}, err
	}
	if req.Address != nil && req.ThreadID != "" {
		return contracts.Page[contracts.MessageView]{}, contracts.Errorf(contracts.ErrInvalid, "specify exactly one of address or thread_id")
	}
	box := req.Box
	if box == "" {
		box = "inbox"
	}
	if box != "inbox" && box != "outbox" && box != "all" {
		return contracts.Page[contracts.MessageView]{}, contracts.Errorf(contracts.ErrInvalid, "box must be inbox, outbox, or all")
	}
	addr := req.Address
	if addr == nil && req.ThreadID == "" {
		k, id := SenderAddress(p)
		if k != string(contracts.AddrSession) && k != string(contracts.AddrOwner) {
			return contracts.Page[contracts.MessageView]{}, contracts.Errorf(contracts.ErrInvalid, "address is required for %s principals", p.Kind)
		}
		addr = &contracts.Address{Kind: contracts.AddressKind(k), ID: id}
	}
	if addr != nil && addr.Kind == contracts.AddrOwner && addr.ID == "" {
		addr = &contracts.Address{Kind: contracts.AddrOwner, ID: "owner"}
	}
	if req.AfterCursor < 0 {
		return contracts.Page[contracts.MessageView]{}, contracts.Errorf(contracts.ErrInvalid, "after_cursor must be >= 0")
	}
	if req.AfterCursor > 0 && (addr == nil || box != "inbox") {
		return contracts.Page[contracts.MessageView]{}, contracts.Errorf(contracts.ErrInvalid, "after_cursor applies to an address's inbox (box=inbox)")
	}
	pc, err := persistence.DecodeCursor(req.Cursor)
	if err != nil {
		return contracts.Page[contracts.MessageView]{}, err
	}
	limit := persistence.ClampLimit(req.Limit, persistence.DefaultLimit, persistence.MaxLimit)

	inboxOrder := addr != nil && box == "inbox"
	var where []string
	var args []any
	switch {
	case req.ThreadID != "":
		where = append(where, "thread_id = ?")
		args = append(args, req.ThreadID)
	case box == "inbox":
		where = append(where, "recipient_kind = ? AND recipient_id = ?")
		args = append(args, string(addr.Kind), addr.ID)
	case box == "outbox":
		where = append(where, "sender_kind = ? AND sender_id = ?")
		args = append(args, string(addr.Kind), addr.ID)
	default:
		where = append(where, "((recipient_kind = ? AND recipient_id = ?) OR (sender_kind = ? AND sender_id = ?))")
		args = append(args, string(addr.Kind), addr.ID, string(addr.Kind), addr.ID)
	}
	if req.ProjectID != nil {
		if *req.ProjectID == "" {
			where = append(where, "project_id IS NULL")
		} else {
			where = append(where, "project_id = ?")
			args = append(args, *req.ProjectID)
		}
	}
	if req.Unacked {
		where = append(where, "ack = ?")
		args = append(args, string(contracts.AckNone))
	}
	if len(req.RuntimeStates) > 0 {
		ph := make([]string, len(req.RuntimeStates))
		for i, st := range req.RuntimeStates {
			ph[i] = "?"
			args = append(args, string(st))
		}
		where = append(where, "runtime IN ("+strings.Join(ph, ",")+")")
	}
	orderCol := "seq"
	if inboxOrder {
		orderCol = "inbox_cursor"
	}
	after := req.AfterCursor
	if pc.N > after {
		after = pc.N
	}
	where = append(where, orderCol+" > ?")
	args = append(args, after, limit)
	q := `SELECT ` + msgCols + `, ` + orderCol + ` FROM messages WHERE ` + strings.Join(where, " AND ") + ` ORDER BY ` + orderCol + ` LIMIT ?`

	var page contracts.Page[contracts.MessageView]
	var last int64
	err = persistence.LongPoll(ctx, &s.bc, persistence.WaitSeconds(req.WaitSeconds), s.cfg.PollInterval, func() (bool, error) {
		page = contracts.Page[contracts.MessageView]{Items: []contracts.MessageView{}, SnapshotAt: s.now()}
		rows, err := s.cfg.DB.QueryContext(ctx, q, args...)
		if err != nil {
			return false, err
		}
		var rs []row
		for rows.Next() {
			var ord int64
			rr, err := scanRow(rows, &ord)
			if err != nil {
				rows.Close()
				return false, err
			}
			rs = append(rs, rr)
			last = ord
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return false, err
		}
		for i := range rs {
			if err := s.loadReceipts(ctx, s.cfg.DB, &rs[i].view); err != nil {
				return false, err
			}
			page.Items = append(page.Items, rs[i].view)
		}
		return len(page.Items) > 0, nil
	})
	if err != nil {
		return page, err
	}
	if len(page.Items) == limit {
		page.NextCursor = persistence.EncodeCursor(persistence.PageCursor{N: last})
	}
	return page, nil
}

// ---- ack / reissue / resolve / cancel ----

func isRecipient(p contracts.Principal, v contracts.MessageView) bool {
	k, id := SenderAddress(p)
	return k == string(v.Envelope.Recipient.Kind) && id == v.Envelope.Recipient.ID
}

func isSender(p contracts.Principal, r row) bool {
	k, id := SenderAddress(p)
	return k == r.senderKind && id == r.senderID
}

// AckMessage records an explicit acknowledgement by the recipient (or the
// owner). A replied message stays replied.
func (s *Store) AckMessage(ctx context.Context, req contracts.AckMessageRequest) (contracts.MessageView, error) {
	p, err := principal(ctx)
	if err != nil {
		return contracts.MessageView{}, err
	}
	var out contracts.MessageView
	_, err = persistence.Mutate(ctx, s.cfg.DB, req.OperationID, contracts.OpAckMessage, p, req, &out, func(tx *sql.Tx) (any, error) {
		r, err := s.get(ctx, tx, req.MessageID)
		if err != nil {
			return nil, err
		}
		if !isRecipient(p, r.view) && !isOwner(p) {
			return nil, contracts.Errorf(contracts.ErrForbidden, "only the recipient or owner can acknowledge a message").WithRef(contracts.KindMessage, req.MessageID)
		}
		if r.view.Status.Ack != contracts.AckNone {
			return r.view, nil
		}
		now := s.now()
		if _, err := tx.ExecContext(ctx, `UPDATE messages SET ack=?, ack_ref=?, updated_at=? WHERE message_id=?`,
			string(contracts.AckAcknowledged), req.OperationID, persistence.Time(now), req.MessageID); err != nil {
			return nil, err
		}
		v, err := s.GetTx(ctx, tx, req.MessageID)
		if err != nil {
			return nil, err
		}
		return v, s.emit(ctx, tx, contracts.EvMessageAck, v)
	})
	if err == nil {
		s.committed()
	}
	return out, err
}

// OwnerMail selects unacknowledged mail to the owner for AckOwnerMailTx.
// At least one of MessageID, ThreadID and FromSession is required.
type OwnerMail struct {
	MessageID       string    // this message
	ThreadID        string    // in this thread
	FromSession     string    // sent by this session
	SkipAsks        bool      // leave ask threads alone (they close with their ask)
	NoThread        bool      // only mail sent without a thread
	SkipAckRequests bool      // leave mail whose sender asked for an acknowledgement
	Through         time.Time // stored at or before this time (zero: any)
}

// AckOwnerMailTx acknowledges, on Pyre's behalf, the owner's unacknowledged
// mail that sel selects (ref: a contracts.AckRef* value saying why) and
// emits message.ack for each. It returns how many it acknowledged.
func (s *Store) AckOwnerMailTx(ctx context.Context, tx *sql.Tx, sel OwnerMail, ref string) (int, error) {
	where, args, err := sel.where()
	if err != nil {
		return 0, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT message_id FROM messages WHERE `+where+` ORDER BY seq LIMIT 500`, args...)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	now := persistence.Time(s.now())
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `UPDATE messages SET ack=?, ack_ref=?, updated_at=? WHERE message_id=? AND ack=?`,
			string(contracts.AckAcknowledged), ref, now, id, string(contracts.AckNone)); err != nil {
			return 0, err
		}
		v, err := s.GetTx(ctx, tx, id)
		if err != nil {
			return 0, err
		}
		if err := s.emit(ctx, tx, contracts.EvMessageAck, v); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}

// where is sel as an SQL condition on messages.
func (sel OwnerMail) where() (string, []any, error) {
	if sel.MessageID == "" && sel.ThreadID == "" && sel.FromSession == "" {
		return "", nil, contracts.Errorf(contracts.ErrInvalid, "select owner mail by message, thread or sending session")
	}
	where := []string{"recipient_kind=?", "ack=?"}
	args := []any{string(contracts.AddrOwner), string(contracts.AckNone)}
	if sel.MessageID != "" {
		where = append(where, "message_id=?")
		args = append(args, sel.MessageID)
	}
	if sel.ThreadID != "" {
		where = append(where, "thread_id=?")
		args = append(args, sel.ThreadID)
	}
	if sel.FromSession != "" {
		where = append(where, "sender_kind=?", "sender_id=?")
		args = append(args, string(contracts.PrincipalSession), sel.FromSession)
	}
	if sel.SkipAsks {
		where = append(where, "(thread_id IS NULL OR substr(thread_id,1,?)<>?)")
		args = append(args, len(contracts.PrefixAsk)+1, contracts.PrefixAsk+"_")
	}
	if sel.NoThread {
		where = append(where, "(thread_id IS NULL OR thread_id='')")
	}
	if sel.SkipAckRequests {
		where = append(where, "COALESCE(json_extract(envelope,'$.request_ack'),0)=0")
	}
	if !sel.Through.IsZero() {
		where = append(where, "stored_at<=?")
		args = append(args, persistence.Time(sel.Through))
	}
	return strings.Join(where, " AND "), args, nil
}

// AckOwnerMail is AckOwnerMailTx in a transaction of its own, opened only
// when something matches (the owner typing calls it every few seconds).
func (s *Store) AckOwnerMail(ctx context.Context, sel OwnerMail, ref string) (int, error) {
	where, args, err := sel.where()
	if err != nil {
		return 0, err
	}
	var found int
	if err := s.cfg.DB.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM messages WHERE `+where+`)`, args...).Scan(&found); err != nil || found == 0 {
		return 0, err
	}
	n := 0
	err = sqlitex.Tx(ctx, s.cfg.DB, func(tx *sql.Tx) error {
		var err error
		n, err = s.AckOwnerMailTx(ctx, tx, sel, ref)
		return err
	})
	if err == nil && n > 0 {
		s.committed()
	}
	return n, err
}

// ReissueMessage explicitly re-sends a message as a NEW message referencing
// the original. Indeterminate originals require accept_duplicate_risk;
// pending originals must be cancelled first; accepted originals are a
// conflict (the runtime has the work; send a new message).
func (s *Store) ReissueMessage(ctx context.Context, req contracts.ReissueMessageRequest) (contracts.MessageView, error) {
	p, err := principal(ctx)
	if err != nil {
		return contracts.MessageView{}, err
	}
	var out contracts.MessageView
	_, err = persistence.Mutate(ctx, s.cfg.DB, req.OperationID, contracts.OpReissueMessage, p, req, &out, func(tx *sql.Tx) (any, error) {
		r, err := s.get(ctx, tx, req.MessageID)
		if err != nil {
			return nil, err
		}
		if !isSender(p, r) && !isOwner(p) {
			return nil, contracts.Errorf(contracts.ErrForbidden, "only the original sender or owner can reissue a message").WithRef(contracts.KindMessage, req.MessageID)
		}
		orig := r.view
		if orig.Envelope.Recipient.Kind != contracts.AddrSession || orig.Status.Runtime == contracts.DeliveryNotApplicable {
			return nil, contracts.Errorf(contracts.ErrInvalid, "only runtime-delivered session messages can be reissued").WithRef(contracts.KindMessage, req.MessageID)
		}
		switch orig.Status.Runtime {
		case contracts.DeliveryQueued, contracts.DeliveryNodeReceived, contracts.DeliveryAttempting:
			return nil, contracts.Errorf(contracts.ErrConflict, "original delivery is still %s; cancel it or wait for an outcome", orig.Status.Runtime).WithRef(contracts.KindMessage, req.MessageID)
		case contracts.DeliveryAccepted:
			// Accepted work is reconciled: the runtime has it. A follow-up is
			// a new message, not a reissue.
			return nil, contracts.Errorf(contracts.ErrConflict, "original was accepted by run %s; send a new message instead of reissuing", orig.Status.BoundRunID).
				WithRef(contracts.KindMessage, req.MessageID).WithDetail("runtime", orig.Status.Runtime)
		case contracts.DeliveryIndeterminate:
			if !req.AcceptDuplicateRisk {
				return nil, contracts.Errorf(contracts.ErrConflict, "original delivery is indeterminate; reissuing may duplicate work and requires accept_duplicate_risk").
					WithRef(contracts.KindMessage, req.MessageID).WithDetail("runtime", orig.Status.Runtime)
			}
		}
		body := orig.Envelope.Body
		if req.Body != nil {
			body = *req.Body
		}
		sreq := contracts.SendMessageRequest{
			Mutation:    contracts.Mutation{OperationID: req.OperationID, CorrelationID: req.MessageID},
			Recipient:   orig.Envelope.Recipient,
			ProjectID:   orig.Envelope.ProjectID,
			ThreadID:    orig.Envelope.ThreadID,
			InReplyTo:   orig.Envelope.InReplyTo,
			Mode:        orig.Envelope.Mode,
			Body:        body,
			Attachments: orig.Envelope.Attachments,
			RequestAck:  orig.Envelope.RequestAck,
		}
		if len(sreq.Body.Text) > s.cfg.Limits.MaxBodyBytes {
			return nil, contracts.Errorf(contracts.ErrPayloadTooLarge, "message body too large")
		}
		return s.insertTx(ctx, tx, p, contracts.NewID(contracts.PrefixMessage), sreq, orig.Envelope.MessageID, req.AcceptDuplicateRisk)
	})
	if err == nil {
		s.committed()
	}
	return out, err
}

// ResolveDelivery records a human/coordinator decision about an
// indeterminate delivery.
func (s *Store) ResolveDelivery(ctx context.Context, req contracts.ResolveDeliveryRequest) (contracts.MessageView, error) {
	p, err := principal(ctx)
	if err != nil {
		return contracts.MessageView{}, err
	}
	if req.Resolution != contracts.DeliveryAccepted && req.Resolution != contracts.DeliveryRejected {
		return contracts.MessageView{}, contracts.Errorf(contracts.ErrInvalid, "resolution must be accepted or rejected")
	}
	var out contracts.MessageView
	_, err = persistence.Mutate(ctx, s.cfg.DB, req.OperationID, contracts.OpResolveDelivery, p, req, &out, func(tx *sql.Tx) (any, error) {
		r, err := s.get(ctx, tx, req.MessageID)
		if err != nil {
			return nil, err
		}
		if !isSender(p, r) && !isOwner(p) {
			return nil, contracts.Errorf(contracts.ErrForbidden, "only the original sender or owner can resolve a delivery").WithRef(contracts.KindMessage, req.MessageID)
		}
		if r.view.Status.Runtime != contracts.DeliveryIndeterminate {
			return nil, contracts.Errorf(contracts.ErrConflict, "delivery is %s, not indeterminate", r.view.Status.Runtime).
				WithRef(contracts.KindMessage, req.MessageID).WithDetail("runtime", r.view.Status.Runtime)
		}
		now := s.now()
		reason := fmt.Sprintf("resolved %s by %s:%s", req.Resolution, p.Kind, p.ID)
		if req.Note != "" {
			reason += ": " + req.Note
		}
		rc := contracts.DeliveryReceipt{
			MessageID: req.MessageID, SessionID: r.view.Envelope.Recipient.ID, RunID: r.view.Status.BoundRunID,
			State: req.Resolution, Evidence: contracts.EvidenceNone, Detail: reason, ObservedAt: now,
		}
		if err := s.recordReceipt(ctx, tx, "", rc); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE messages SET runtime=?, runtime_reason=?, updated_at=? WHERE message_id=?`,
			string(req.Resolution), reason, persistence.Time(now), req.MessageID); err != nil {
			return nil, err
		}
		v, err := s.GetTx(ctx, tx, req.MessageID)
		if err != nil {
			return nil, err
		}
		return v, s.emitDelivery(ctx, tx, v)
	})
	if err == nil {
		s.committed()
	}
	return out, err
}

// CancelMessage cancels a message before any delivery attempt.
func (s *Store) CancelMessage(ctx context.Context, req contracts.CancelMessageRequest) (contracts.MessageView, error) {
	p, err := principal(ctx)
	if err != nil {
		return contracts.MessageView{}, err
	}
	var out contracts.MessageView
	_, err = persistence.Mutate(ctx, s.cfg.DB, req.OperationID, contracts.OpCancelMessage, p, req, &out, func(tx *sql.Tx) (any, error) {
		r, err := s.get(ctx, tx, req.MessageID)
		if err != nil {
			return nil, err
		}
		if !isSender(p, r) && !isOwner(p) {
			return nil, contracts.Errorf(contracts.ErrForbidden, "only the original sender or owner can cancel a message").WithRef(contracts.KindMessage, req.MessageID)
		}
		st := r.view.Status
		if st.Runtime == contracts.DeliveryCancelled {
			return r.view, nil
		}
		if (st.Runtime != contracts.DeliveryQueued && st.Runtime != contracts.DeliveryNodeReceived) || st.BoundRunID != "" {
			return nil, contracts.Errorf(contracts.ErrConflict, "cannot cancel: delivery is %s", st.Runtime).
				WithRef(contracts.KindMessage, req.MessageID).WithDetail("runtime", st.Runtime)
		}
		return s.withdrawTx(ctx, tx, r.view, contracts.DeliveryCancelled, fmt.Sprintf("cancelled by %s:%s before any delivery attempt", p.Kind, p.ID))
	})
	if err == nil {
		s.committed()
	}
	return out, err
}

func (s *Store) withdrawTx(ctx context.Context, tx *sql.Tx, v contracts.MessageView, state contracts.RuntimeDeliveryState, reason string) (contracts.MessageView, error) {
	now := s.now()
	if _, err := tx.ExecContext(ctx, `UPDATE messages SET runtime=?, runtime_reason=?, updated_at=? WHERE message_id=?`,
		string(state), reason, persistence.Time(now), v.Envelope.MessageID); err != nil {
		return v, err
	}
	nv, err := s.GetTx(ctx, tx, v.Envelope.MessageID)
	if err != nil {
		return nv, err
	}
	if s.cfg.OnWithdrawn != nil {
		if err := s.cfg.OnWithdrawn(ctx, tx, v); err != nil {
			return nv, err
		}
	}
	return nv, s.emitDelivery(ctx, tx, nv)
}

// ExpireDue marks undelivered messages past their expiry as expired (a
// terminal, explained state; the record is kept). Returns how many expired.
func (s *Store) ExpireDue(ctx context.Context) (int, error) {
	now := s.now()
	n := 0
	err := sqlitex.Tx(ctx, s.cfg.DB, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT message_id FROM messages WHERE runtime IN (?,?) AND bound_run_id IS NULL AND expires_at IS NOT NULL AND expires_at < ? ORDER BY seq LIMIT 1000`,
			string(contracts.DeliveryQueued), string(contracts.DeliveryNodeReceived), persistence.Time(now))
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, id := range ids {
			v, err := s.GetTx(ctx, tx, id)
			if err != nil {
				return err
			}
			exp := ""
			if v.Envelope.ExpiresAt != nil {
				exp = v.Envelope.ExpiresAt.Format(time.RFC3339)
			}
			if _, err := s.withdrawTx(ctx, tx, v, contracts.DeliveryExpired, "expired at "+exp+" before any delivery attempt (was "+string(v.Status.Runtime)+")"); err != nil {
				return err
			}
			n++
		}
		return nil
	})
	if err == nil && n > 0 {
		s.committed()
	}
	return n, err
}

// ---- tx-level operations used by the hub ----

// SetRuntimeTx changes the runtime state of an unattempted message (e.g.
// rejected because the recipient adapter lacks input.steer) or updates the
// reason of a queued one.
func (s *Store) SetRuntimeTx(ctx context.Context, tx *sql.Tx, messageID string, state contracts.RuntimeDeliveryState, reason string) (contracts.MessageView, error) {
	v, err := s.GetTx(ctx, tx, messageID)
	if err != nil {
		return v, err
	}
	prev := v.Status.Runtime
	if prev != contracts.DeliveryQueued && prev != contracts.DeliveryNodeReceived {
		return v, contracts.Errorf(contracts.ErrConflict, "message delivery is %s", prev).WithRef(contracts.KindMessage, messageID)
	}
	if prev == state && v.Status.RuntimeReason == reason {
		return v, nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE messages SET runtime=?, runtime_reason=?, updated_at=? WHERE message_id=?`,
		string(state), persistence.NullStr(reason), persistence.Time(s.now()), messageID); err != nil {
		return v, err
	}
	nv, err := s.GetTx(ctx, tx, messageID)
	if err != nil {
		return nv, err
	}
	if prev != state {
		return nv, s.emitDelivery(ctx, tx, nv)
	}
	return nv, nil
}

// MarkNodeReceivedTx records that the recipient node durably committed the
// message to its inbox (CommandAck of a deliver command).
func (s *Store) MarkNodeReceivedTx(ctx context.Context, tx *sql.Tx, messageID, nodeID string) error {
	v, err := s.GetTx(ctx, tx, messageID)
	if err != nil {
		return err
	}
	if v.Status.Runtime != contracts.DeliveryQueued {
		return nil
	}
	reason := "recipient node " + nodeID + " committed the message to its inbox"
	if _, err := tx.ExecContext(ctx, `UPDATE messages SET runtime=?, runtime_reason=?, updated_at=? WHERE message_id=?`,
		string(contracts.DeliveryNodeReceived), reason, persistence.Time(s.now()), messageID); err != nil {
		return err
	}
	nv, err := s.GetTx(ctx, tx, messageID)
	if err != nil {
		return err
	}
	return s.emitDelivery(ctx, tx, nv)
}

// RecordReadTx records that the recipient session itself read (or acked or
// replied to) a message whose runtime delivery is still pending (queued at
// the hub or held by the recipient node, no attempt in flight): acceptance
// and consumption with evidence ev, bound to runID (the session's current
// run; "" leaves it unbound). ok=false means the delivery was not pending
// (attempting, accepted, indeterminate, terminal, informational) and nothing
// changed. Decision I16 extended to hub-observed reads (W-delivery).
func (s *Store) RecordReadTx(ctx context.Context, tx *sql.Tx, messageID, runID string, ev contracts.EvidenceCategory, detail string) (contracts.MessageView, bool, error) {
	v, err := s.GetTx(ctx, tx, messageID)
	if err != nil {
		return v, false, err
	}
	switch v.Status.Runtime {
	case contracts.DeliveryQueued, contracts.DeliveryNodeReceived:
	default:
		return v, false, nil
	}
	if v.Envelope.Mode == contracts.ModeInfo || v.Envelope.Recipient.Kind != contracts.AddrSession {
		return v, false, nil
	}
	bound := v.Status.BoundRunID
	if bound == "" {
		bound = runID
	}
	now := s.now()
	rc := contracts.DeliveryReceipt{
		MessageID: messageID, SessionID: v.Envelope.Recipient.ID, RunID: bound,
		State: contracts.DeliveryAccepted, Evidence: ev, Detail: detail, ObservedAt: now, Consumed: true,
	}
	if err := s.recordReceipt(ctx, tx, "", rc); err != nil {
		return v, false, err
	}
	reason := fmt.Sprintf("accepted (evidence %s): %s", ev, detail)
	if _, err := tx.ExecContext(ctx, `UPDATE messages SET runtime=?, runtime_reason=?, bound_run_id=?, consumed_at=COALESCE(consumed_at, ?), updated_at=? WHERE message_id=?`,
		string(contracts.DeliveryAccepted), reason, persistence.NullStr(bound), persistence.Time(now), persistence.Time(now), messageID); err != nil {
		return v, false, err
	}
	nv, err := s.GetTx(ctx, tx, messageID)
	if err != nil {
		return nv, false, err
	}
	return nv, true, s.emitDelivery(ctx, tx, nv)
}

// UnattemptedForSessionTx lists messages for a session that are queued at
// the hub and not bound to any run, in inbox order. These may be delivered
// to a replacement run.
func (s *Store) UnattemptedForSessionTx(ctx context.Context, tx *sql.Tx, sessionID string) ([]contracts.MessageView, error) {
	rows, err := tx.QueryContext(ctx, `SELECT message_id FROM messages WHERE recipient_kind=? AND recipient_id=? AND runtime=? AND bound_run_id IS NULL ORDER BY inbox_cursor`,
		string(contracts.AddrSession), sessionID, string(contracts.DeliveryQueued))
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]contracts.MessageView, 0, len(ids))
	for _, id := range ids {
		v, err := s.GetTx(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

func (s *Store) recordReceipt(ctx context.Context, tx *sql.Tx, nodeID string, r contracts.DeliveryReceipt) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO message_receipts(message_id,node_id,origin_seq,receipt,observed_at) VALUES(?,?,?,?,?)`,
		r.MessageID, nodeID, int64(r.OriginSeq), persistence.JSON(r), persistence.Time(r.ObservedAt))
	return err
}

// receiptDedupQuery finds an already-applied receipt by (node, origin_seq).
// message_receipts_origin is a partial index (WHERE origin_seq > 0); SQLite
// uses it only when the query repeats that condition. Without it every
// receipt scanned the whole table: in the A20 soak this was 40% of the hub's
// CPU and made the per-message cost grow with history (78 -> 115 ms per
// message over 3 hours).
const receiptDedupQuery = `SELECT 1 FROM message_receipts WHERE node_id=? AND origin_seq=? AND origin_seq > 0`

// ReceiptResult reports what ApplyReceiptTx did.
type ReceiptResult struct {
	Duplicate bool
	Changed   bool
	View      contracts.MessageView
}

// ApplyReceiptTx ingests a durable delivery receipt reported by nodeID. It
// is deduplicated by (node, origin_seq). The caller has verified that the
// node owns the receipt's run. Transition rules:
//
//   - The first attempt binds the message to receipt.RunID; a receipt for a
//     different run is refused (never rebound).
//   - accepted is final; it requires acceptance evidence (terminal_write or
//     none are recorded as indeterminate).
//   - indeterminate changes only through a later accepted receipt, an
//     evidence-backed rejected receipt, or ResolveDelivery.
//   - Evidence of an attempt overrides an expired/cancelled record: facts
//     beat intent, and the receipt is never discarded.
func (s *Store) ApplyReceiptTx(ctx context.Context, tx *sql.Tx, nodeID string, rc contracts.DeliveryReceipt) (ReceiptResult, error) {
	var res ReceiptResult
	if rc.MessageID == "" {
		return res, contracts.Errorf(contracts.ErrInvalid, "receipt message_id is required")
	}
	if rc.NodeID == "" {
		rc.NodeID = nodeID
	}
	if rc.NodeID != nodeID {
		return res, contracts.Errorf(contracts.ErrForbidden, "receipt names node %s but was reported by %s", rc.NodeID, nodeID)
	}
	if rc.OriginSeq > 0 {
		var one int
		err := tx.QueryRowContext(ctx, receiptDedupQuery, nodeID, int64(rc.OriginSeq)).Scan(&one)
		if err == nil {
			res.Duplicate = true
			res.View, err = s.GetTx(ctx, tx, rc.MessageID)
			return res, err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return res, err
		}
	}
	r, err := s.get(ctx, tx, rc.MessageID)
	if err != nil {
		return res, err
	}
	v := r.view
	if v.Envelope.Recipient.Kind != contracts.AddrSession {
		return res, contracts.Errorf(contracts.ErrInvalid, "message is not addressed to a session").WithRef(contracts.KindMessage, rc.MessageID)
	}
	if rc.SessionID == "" {
		rc.SessionID = v.Envelope.Recipient.ID
	}
	if rc.SessionID != v.Envelope.Recipient.ID {
		return res, contracts.Errorf(contracts.ErrConflict, "receipt session %s does not match recipient %s", rc.SessionID, v.Envelope.Recipient.ID).WithRef(contracts.KindMessage, rc.MessageID)
	}
	// The hub records receipt times on its own clock (decision I17): a node
	// with a skewed clock must not put receipts ahead of (or behind) the
	// hub's own timeline. The node's observation time stays in the stored
	// receipt's detail; the contract has no separate field for it.
	detail := rc.Detail
	if !rc.ObservedAt.IsZero() {
		note := "node_observed_at=" + rc.ObservedAt.UTC().Format(time.RFC3339Nano)
		if rc.Detail != "" {
			rc.Detail += "; " + note
		} else {
			rc.Detail = note
		}
	}
	rc.ObservedAt = s.now()
	binds := rc.State == contracts.DeliveryAttempting || rc.State == contracts.DeliveryAccepted ||
		rc.State == contracts.DeliveryRejected || rc.State == contracts.DeliveryIndeterminate
	bound := v.Status.BoundRunID
	if binds {
		if rc.RunID == "" {
			return res, contracts.Errorf(contracts.ErrInvalid, "an attempt receipt must name its run").WithRef(contracts.KindMessage, rc.MessageID)
		}
		if bound != "" && bound != rc.RunID {
			return res, contracts.Errorf(contracts.ErrConflict, "message is bound to run %s; receipt for run %s refused", bound, rc.RunID).WithRef(contracts.KindMessage, rc.MessageID)
		}
		bound = rc.RunID
	}
	next, changed, note := Transition(v.Status.Runtime, rc)
	if err := s.recordReceipt(ctx, tx, nodeID, rc); err != nil {
		return res, err
	}
	reason := v.Status.RuntimeReason
	if changed {
		reason = detail
		if note != "" {
			if reason != "" {
				reason = note + "; " + reason
			} else {
				reason = note
			}
		}
		if reason == "" {
			reason = fmt.Sprintf("%s (evidence %s)", next, rc.Evidence)
		}
	}
	// A deferred attempt (the node reports "queued" with the attempt's ID:
	// the runtime was busy and consumed nothing) releases the binding that
	// its "attempting" receipt made, once no attempt of this message has an
	// unknown or final outcome. The message was never submitted, so it may
	// reach a replacement run of the session (the node releases its own
	// binding the same way, inbox.FinishTx).
	if rc.State == contracts.DeliveryQueued && rc.AttemptID != "" && bound != "" && bound == rc.RunID &&
		v.Status.Runtime == contracts.DeliveryAttempting && next == contracts.DeliveryNodeReceived {
		release, err := s.onlyDeferredAttemptsTx(ctx, tx, rc.MessageID)
		if err != nil {
			return res, err
		}
		if release {
			bound = ""
		}
	}
	// Consumption is an extra fact recorded alongside acceptance; it never
	// moves the runtime state.
	consumed := persistence.TimePtr(v.Status.ConsumedAt)
	consumedNow := false
	if next == contracts.DeliveryAccepted && v.Status.ConsumedAt == nil && rc.State == contracts.DeliveryAccepted && ConsumptionEvidence(rc.Evidence) {
		consumed = persistence.Time(rc.ObservedAt)
		consumedNow = true
	}
	if _, err := tx.ExecContext(ctx, `UPDATE messages SET runtime=?, runtime_reason=?, bound_run_id=?, consumed_at=?, updated_at=? WHERE message_id=?`,
		string(next), persistence.NullStr(reason), persistence.NullStr(bound), consumed, persistence.Time(s.now()), rc.MessageID); err != nil {
		return res, err
	}
	nv, err := s.GetTx(ctx, tx, rc.MessageID)
	if err != nil {
		return res, err
	}
	res.Changed = changed || consumedNow
	res.View = nv
	return res, s.emitDelivery(ctx, tx, nv)
}

// onlyDeferredAttemptsTx reports whether every attempt of the message that
// reported "attempting" was later reported deferred ("queued" with the same
// attempt ID) and no receipt carries an outcome (accepted, rejected,
// indeterminate).
func (s *Store) onlyDeferredAttemptsTx(ctx context.Context, tx *sql.Tx, messageID string) (bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT receipt FROM message_receipts WHERE message_id=?`, messageID)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	attempts, deferred := map[string]bool{}, map[string]bool{}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return false, err
		}
		var r contracts.DeliveryReceipt
		if err := json.Unmarshal(raw, &r); err != nil {
			return false, err
		}
		switch r.State {
		case contracts.DeliveryAttempting:
			if r.AttemptID == "" {
				return false, nil
			}
			attempts[r.AttemptID] = true
		case contracts.DeliveryQueued:
			if r.AttemptID != "" {
				deferred[r.AttemptID] = true
			}
		case contracts.DeliveryAccepted, contracts.DeliveryRejected, contracts.DeliveryIndeterminate:
			return false, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	for id := range attempts {
		if !deferred[id] {
			return false, nil
		}
	}
	return len(attempts) > 0, nil
}

// strongEvidence reports whether an evidence category can establish
// runtime acceptance (contracts.AcceptanceEvidence). Terminal writes,
// "none" and informational kinds never can.
func strongEvidence(e contracts.EvidenceCategory) bool { return contracts.AcceptanceEvidence(e) }

// ConsumptionEvidence reports whether evidence shows the runtime took the
// input into a turn (not merely queued it); see contracts.ConsumptionEvidence.
// Such a receipt sets DeliveryStatus.ConsumedAt.
func ConsumptionEvidence(e contracts.EvidenceCategory) bool { return contracts.ConsumptionEvidence(e) }

// Transition computes the runtime state after a receipt. It is pure so the
// rules can be tested exhaustively.
func Transition(cur contracts.RuntimeDeliveryState, rc contracts.DeliveryReceipt) (next contracts.RuntimeDeliveryState, changed bool, note string) {
	target := rc.State
	if target == contracts.DeliveryAccepted && !strongEvidence(rc.Evidence) {
		target = contracts.DeliveryIndeterminate
		note = fmt.Sprintf("acceptance reported with %q evidence, which cannot establish acceptance; treated as indeterminate", rc.Evidence)
	}
	switch cur {
	case contracts.DeliveryAccepted, contracts.DeliveryNotApplicable:
		return cur, false, ""
	}
	set := func(s contracts.RuntimeDeliveryState) (contracts.RuntimeDeliveryState, bool, string) {
		if s == cur {
			return cur, false, ""
		}
		return s, true, note
	}
	switch target {
	case contracts.DeliveryAccepted:
		return set(contracts.DeliveryAccepted)
	case contracts.DeliveryIndeterminate:
		return set(contracts.DeliveryIndeterminate)
	case contracts.DeliveryRejected:
		switch cur {
		case contracts.DeliveryIndeterminate:
			if strongEvidence(rc.Evidence) {
				return set(contracts.DeliveryRejected)
			}
		case contracts.DeliveryQueued, contracts.DeliveryNodeReceived, contracts.DeliveryAttempting:
			return set(contracts.DeliveryRejected)
		}
	case contracts.DeliveryAttempting:
		switch cur {
		case contracts.DeliveryQueued, contracts.DeliveryNodeReceived, contracts.DeliveryRejected, contracts.DeliveryExpired, contracts.DeliveryCancelled:
			return set(contracts.DeliveryAttempting)
		}
	case contracts.DeliveryNodeReceived:
		if cur == contracts.DeliveryQueued {
			return set(contracts.DeliveryNodeReceived)
		}
	case contracts.DeliveryQueued:
		// Runtime busy; no attempt consumed. The node still holds it.
		if cur == contracts.DeliveryAttempting {
			note = "runtime busy; attempt not consumed"
			return set(contracts.DeliveryNodeReceived)
		}
	case contracts.DeliveryExpired, contracts.DeliveryCancelled:
		if cur == contracts.DeliveryQueued || cur == contracts.DeliveryNodeReceived {
			return set(target)
		}
	}
	return cur, false, ""
}
