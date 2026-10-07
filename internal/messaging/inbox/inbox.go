// Package inbox is the recipient node's durable per-session inbox of
// messages handed over by the hub (DeliverCommand), plus the durable
// delivery-attempt journal (contracts.md §5).
//
// Invariants enforced here:
//   - A message is stored once per message ID; the same ID with different
//     content is a conflict.
//   - An attempt is claimed durably (attempt ID + bound run) before the
//     runtime is invoked; once a possibly-consuming attempt exists the
//     message is bound to that run forever.
//   - Only messages that were never (possibly) attempted can be cancelled or
//     consumed by a replacement run.
//
// The delivery engine (internal/messaging/delivery) owns sequencing and
// policy; this package owns storage and state transitions.
package inbox

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

// Migrations is the "inbox" namespace.
var Migrations = []sqlitex.Migration{{Namespace: "inbox", Version: 1, Name: "messages_and_attempts", SQL: `
CREATE TABLE inbox_messages (
	seq             INTEGER PRIMARY KEY AUTOINCREMENT,
	message_id      TEXT NOT NULL UNIQUE,
	session_id      TEXT NOT NULL,
	envelope        BLOB NOT NULL,
	digest          TEXT NOT NULL,
	mode            TEXT NOT NULL,
	state           TEXT NOT NULL,
	reason          TEXT NOT NULL DEFAULT '',
	reported_reason TEXT NOT NULL DEFAULT '',
	bound_run_id    TEXT NOT NULL DEFAULT '',
	hub_bound       INTEGER NOT NULL DEFAULT 0,
	attempted       INTEGER NOT NULL DEFAULT 0,
	current_attempt TEXT NOT NULL DEFAULT '',
	next_at         TEXT NOT NULL DEFAULT '',
	backoff_ms      INTEGER NOT NULL DEFAULT 0,
	expires_at      TEXT NOT NULL DEFAULT '',
	consumed_at     TEXT NOT NULL DEFAULT '',
	received_at     TEXT NOT NULL,
	updated_at      TEXT NOT NULL
);
CREATE INDEX inbox_messages_session ON inbox_messages(session_id, state, seq);
CREATE INDEX inbox_messages_state ON inbox_messages(state, updated_at);
CREATE TABLE inbox_attempts (
	attempt_id   TEXT PRIMARY KEY,
	message_id   TEXT NOT NULL,
	run_id       TEXT NOT NULL,
	state        TEXT NOT NULL,
	evidence     TEXT NOT NULL DEFAULT '',
	evidence_ref TEXT NOT NULL DEFAULT '',
	detail       TEXT NOT NULL DEFAULT '',
	started_at   TEXT NOT NULL,
	finished_at  TEXT NOT NULL DEFAULT ''
);
CREATE INDEX inbox_attempts_state ON inbox_attempts(state);
CREATE INDEX inbox_attempts_msg ON inbox_attempts(message_id);
`}}

// AttemptDeferred marks an attempt the adapter declined without consuming
// (runtime busy, State=queued). It is not a contracts state.
const AttemptDeferred contracts.RuntimeDeliveryState = "deferred"

// Message is one inbox entry.
type Message struct {
	Seq            int64
	Envelope       contracts.MessageEnvelope
	SessionID      string
	State          contracts.RuntimeDeliveryState
	Reason         string
	ReportedReason string
	BoundRunID     string
	HubBound       bool // binding came from the hub (an earlier attempt elsewhere/earlier)
	Attempted      bool // a possibly-consuming attempt exists
	CurrentAttempt string
	NextAt         time.Time
	Backoff        time.Duration
	ExpiresAt      *time.Time
	ConsumedAt     *time.Time
	ReceivedAt     time.Time
	UpdatedAt      time.Time
}

// Terminal reports whether no further automatic delivery happens.
func (m Message) Terminal() bool {
	switch m.State {
	case contracts.DeliveryQueued, contracts.DeliveryAttempting:
		return false
	}
	return true
}

// Attempt is one durable delivery attempt.
type Attempt struct {
	AttemptID   string
	MessageID   string
	RunID       string
	State       contracts.RuntimeDeliveryState
	Evidence    contracts.EvidenceCategory
	EvidenceRef string
	Detail      string
	StartedAt   time.Time
	FinishedAt  *time.Time
}

// Limits bound inbox size.
type Limits struct {
	MaxPerSession int // non-terminal messages per session (default 1000)
	MaxTotal      int // non-terminal messages (default 10000)
}

// Store is the inbox over a node-local SQLite database.
type Store struct {
	db     *sql.DB
	limits Limits
}

// Open migrates the "inbox" namespace and returns a store.
func Open(ctx context.Context, db *sql.DB, limits Limits) (*Store, error) {
	if err := sqlitex.Migrate(ctx, db, Migrations...); err != nil {
		return nil, err
	}
	if limits.MaxPerSession <= 0 {
		limits.MaxPerSession = 1000
	}
	if limits.MaxTotal <= 0 {
		limits.MaxTotal = 10000
	}
	return &Store{db: db, limits: limits}, nil
}

// DB returns the underlying database (shared with the delivery engine so
// state changes and outgoing reports commit in one transaction).
func (s *Store) DB() *sql.DB { return s.db }

type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// ContentDigest hashes the immutable content of an envelope. Informational
// fields (created_at) and transport fields do not participate.
func ContentDigest(e contracts.MessageEnvelope) string {
	b, _ := json.Marshal(struct {
		ID          string
		Sender      contracts.Principal
		Recipient   contracts.Address
		ProjectID   *string
		ThreadID    string
		InReplyTo   string
		Reissues    string
		Mode        contracts.MessageMode
		Body        contracts.MessageBody
		Attachments []contracts.Attachment
	}{e.MessageID, e.Sender, e.Recipient, e.ProjectID, e.ThreadID, e.InReplyTo, e.Reissues, e.Mode, e.Body, e.Attachments})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func ft(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return sqlitex.FormatTime(t)
}

func pt(s string) time.Time { t, _ := sqlitex.ParseTime(s); return t }

func ptp(s string) *time.Time {
	if s == "" {
		return nil
	}
	t := pt(s)
	return &t
}

const msgCols = `seq, message_id, session_id, envelope, state, reason, reported_reason, bound_run_id, hub_bound, attempted, current_attempt, next_at, backoff_ms, expires_at, consumed_at, received_at, updated_at`

func scanMsg(sc interface{ Scan(...any) error }) (Message, error) {
	var m Message
	var id, env, next, exp, cons, recv, upd string
	var hub, att int
	var backoff int64
	err := sc.Scan(&m.Seq, &id, &m.SessionID, &env, &m.State, &m.Reason, &m.ReportedReason, &m.BoundRunID, &hub, &att, &m.CurrentAttempt, &next, &backoff, &exp, &cons, &recv, &upd)
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal([]byte(env), &m.Envelope); err != nil {
		return m, err
	}
	m.HubBound, m.Attempted = hub != 0, att != 0
	m.NextAt, m.Backoff = pt(next), time.Duration(backoff)*time.Millisecond
	m.ExpiresAt, m.ConsumedAt = ptp(exp), ptp(cons)
	m.ReceivedAt, m.UpdatedAt = pt(recv), pt(upd)
	return m, nil
}

func getMsg(ctx context.Context, q querier, id string) (Message, error) {
	m, err := scanMsg(q.QueryRowContext(ctx, `SELECT `+msgCols+` FROM inbox_messages WHERE message_id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return m, contracts.Errorf(contracts.ErrNotFound, "message not in this node's inbox").WithRef(contracts.KindMessage, id)
	}
	return m, err
}

// Get returns a message by ID.
func (s *Store) Get(ctx context.Context, id string) (Message, error) { return getMsg(ctx, s.db, id) }

// GetTx returns a message by ID inside tx.
func (s *Store) GetTx(ctx context.Context, tx *sql.Tx, id string) (Message, error) {
	return getMsg(ctx, tx, id)
}

// Put durably records a DeliverCommand. It returns created=false for a
// duplicate with identical content (the stored message is returned), and
// ErrConflict for the same ID with different content or a conflicting run
// binding. It commits before returning, so the caller may then acknowledge.
func (s *Store) Put(ctx context.Context, dc contracts.DeliverCommand, now time.Time) (Message, bool, error) {
	env := dc.Message
	if !contracts.ValidID(env.MessageID, contracts.PrefixMessage) {
		return Message{}, false, contracts.Errorf(contracts.ErrInvalid, "invalid message_id %q", env.MessageID)
	}
	if dc.SessionID == "" {
		return Message{}, false, contracts.Errorf(contracts.ErrInvalid, "deliver command without session_id").WithRef(contracts.KindMessage, env.MessageID)
	}
	if env.Mode == "" {
		env.Mode = contracts.ModeQueue
	}
	digest := ContentDigest(env)
	var out Message
	created := false
	err := sqlitex.Tx(ctx, s.db, func(tx *sql.Tx) error {
		var gotDigest, gotSession string
		err := tx.QueryRowContext(ctx, `SELECT digest, session_id FROM inbox_messages WHERE message_id=?`, env.MessageID).Scan(&gotDigest, &gotSession)
		if err == nil {
			if gotDigest != digest || gotSession != dc.SessionID {
				return contracts.Errorf(contracts.ErrConflict, "message_id reused with different content").WithRef(contracts.KindMessage, env.MessageID)
			}
			m, err := getMsg(ctx, tx, env.MessageID)
			if err != nil {
				return err
			}
			if dc.BoundRunID != "" && m.BoundRunID != dc.BoundRunID {
				if m.BoundRunID != "" {
					return contracts.Errorf(contracts.ErrConflict, "message is bound to run %s on this node, hub says %s", m.BoundRunID, dc.BoundRunID).WithRef(contracts.KindMessage, env.MessageID)
				}
				echo, err := deferredOnlyOn(ctx, tx, env.MessageID, dc.BoundRunID)
				if err != nil {
					return err
				}
				if echo && !m.Attempted {
					// The hub's binding echoes this node's own attempt on
					// that run, which the runtime declined without
					// consuming anything (deferred). The message was never
					// submitted, so it stays unbound and may reach a
					// replacement run; the hub releases its binding when it
					// receives the deferred attempt's "queued" receipt.
					out = m
					return nil
				}
				if _, err := tx.ExecContext(ctx, `UPDATE inbox_messages SET bound_run_id=?, hub_bound=1, updated_at=? WHERE message_id=?`, dc.BoundRunID, ft(now), env.MessageID); err != nil {
					return err
				}
				m.BoundRunID, m.HubBound = dc.BoundRunID, true
			}
			out = m
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var nSession, nTotal int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM inbox_messages WHERE state IN ('queued','attempting')`).Scan(&nTotal); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM inbox_messages WHERE session_id=? AND state IN ('queued','attempting')`, dc.SessionID).Scan(&nSession); err != nil {
			return err
		}
		if nSession >= s.limits.MaxPerSession || nTotal >= s.limits.MaxTotal {
			return contracts.Errorf(contracts.ErrCapacityExhausted, "node inbox full (%d queued for session, %d total)", nSession, nTotal).WithRef(contracts.KindSession, dc.SessionID)
		}
		raw, err := json.Marshal(env)
		if err != nil {
			return err
		}
		exp := ""
		if env.ExpiresAt != nil {
			exp = ft(*env.ExpiresAt)
		}
		hub := 0
		if dc.BoundRunID != "" {
			hub = 1
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO inbox_messages(message_id, session_id, envelope, digest, mode, state, bound_run_id, hub_bound, expires_at, received_at, updated_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?)`, env.MessageID, dc.SessionID, raw, digest, string(env.Mode), string(contracts.DeliveryQueued), dc.BoundRunID, hub, exp, ft(now), ft(now)); err != nil {
			return err
		}
		created = true
		out, err = getMsg(ctx, tx, env.MessageID)
		return err
	})
	return out, created, err
}

// Queued returns the session's queued messages in hub-accepted (arrival)
// order.
func (s *Store) Queued(ctx context.Context, sessionID string) ([]Message, error) {
	return s.list(ctx, `SELECT `+msgCols+` FROM inbox_messages WHERE session_id=? AND state='queued' ORDER BY seq`, sessionID)
}

// List returns up to limit messages of a session (any state), newest last.
func (s *Store) List(ctx context.Context, sessionID string, limit int) ([]Message, error) {
	if limit <= 0 {
		limit = 100
	}
	return s.list(ctx, `SELECT `+msgCols+` FROM (SELECT * FROM inbox_messages WHERE session_id=? ORDER BY seq DESC LIMIT ?) ORDER BY seq`, sessionID, limit)
}

func (s *Store) list(ctx context.Context, q string, args ...any) ([]Message, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		m, err := scanMsg(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// SessionsWithQueued returns sessions that have queued messages.
func (s *Store) SessionsWithQueued(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT session_id FROM inbox_messages WHERE state='queued'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ClaimTx durably claims a delivery attempt for a queued message and binds
// it to runID. It fails with ErrConflict if the message is bound to another
// run or is not queued.
func (s *Store) ClaimTx(ctx context.Context, tx *sql.Tx, messageID, runID, attemptID string, now time.Time) (Message, error) {
	m, err := getMsg(ctx, tx, messageID)
	if err != nil {
		return m, err
	}
	if m.State != contracts.DeliveryQueued {
		return m, contracts.Errorf(contracts.ErrConflict, "message is %s, not queued", m.State).WithRef(contracts.KindMessage, messageID)
	}
	if m.BoundRunID != "" && m.BoundRunID != runID {
		return m, contracts.Errorf(contracts.ErrConflict, "message is bound to run %s; it never moves to run %s", m.BoundRunID, runID).WithRef(contracts.KindMessage, messageID)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO inbox_attempts(attempt_id, message_id, run_id, state, started_at) VALUES(?,?,?,?,?)`,
		attemptID, messageID, runID, string(contracts.DeliveryAttempting), ft(now)); err != nil {
		return m, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE inbox_messages SET state='attempting', bound_run_id=?, current_attempt=?, reason='', updated_at=? WHERE message_id=?`,
		runID, attemptID, ft(now), messageID); err != nil {
		return m, err
	}
	return getMsg(ctx, tx, messageID)
}

// deferredOnlyOn reports whether every recorded attempt of the message was
// deferred (declined by a busy runtime, nothing submitted) and at least one
// of them was on runID.
func deferredOnlyOn(ctx context.Context, q querier, messageID, runID string) (bool, error) {
	var total, other, onRun int
	err := q.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(state != ?),0), COALESCE(SUM(run_id = ?),0) FROM inbox_attempts WHERE message_id=?`,
		string(AttemptDeferred), runID, messageID).Scan(&total, &other, &onRun)
	if err != nil {
		return false, err
	}
	return total > 0 && other == 0 && onRun > 0, nil
}

func getAttempt(ctx context.Context, q querier, id string) (Attempt, error) {
	var a Attempt
	var started, finished string
	err := q.QueryRowContext(ctx, `SELECT attempt_id, message_id, run_id, state, evidence, evidence_ref, detail, started_at, finished_at FROM inbox_attempts WHERE attempt_id=?`, id).
		Scan(&a.AttemptID, &a.MessageID, &a.RunID, &a.State, &a.Evidence, &a.EvidenceRef, &a.Detail, &started, &finished)
	if errors.Is(err, sql.ErrNoRows) {
		return a, contracts.Errorf(contracts.ErrNotFound, "delivery attempt not found").WithRef(contracts.KindMessage, id)
	}
	a.StartedAt, a.FinishedAt = pt(started), ptp(finished)
	return a, err
}

// FinishTx records the adapter's result for an attempt.
//
//   - accepted / rejected / indeterminate: terminal for the message; the
//     message stays bound to the attempt's run.
//   - queued (runtime busy, nothing consumed): the attempt is closed as
//     deferred, the message returns to queued with nextAt, and the run
//     binding is released unless the hub or an earlier consuming attempt
//     set it.
func (s *Store) FinishTx(ctx context.Context, tx *sql.Tx, attemptID string, r contracts.DeliveryResult, now, nextAt time.Time, backoff time.Duration) (Message, Attempt, error) {
	a, err := getAttempt(ctx, tx, attemptID)
	if err != nil {
		return Message{}, a, err
	}
	if a.State != contracts.DeliveryAttempting {
		m, _ := getMsg(ctx, tx, a.MessageID)
		return m, a, contracts.Errorf(contracts.ErrConflict, "attempt already finished as %s", a.State).WithRef(contracts.KindMessage, a.MessageID)
	}
	state := r.State
	switch state {
	case contracts.DeliveryAccepted, contracts.DeliveryRejected, contracts.DeliveryIndeterminate:
	case contracts.DeliveryQueued:
		state = AttemptDeferred
	default:
		state = contracts.DeliveryIndeterminate
	}
	if _, err := tx.ExecContext(ctx, `UPDATE inbox_attempts SET state=?, evidence=?, evidence_ref=?, detail=?, finished_at=? WHERE attempt_id=?`,
		string(state), string(r.Evidence), r.EvidenceRef, r.Detail, ft(now), attemptID); err != nil {
		return Message{}, a, err
	}
	if state == AttemptDeferred {
		m, err := getMsg(ctx, tx, a.MessageID)
		if err != nil {
			return m, a, err
		}
		bound := m.BoundRunID
		if !m.HubBound && !m.Attempted {
			bound = ""
		}
		if _, err := tx.ExecContext(ctx, `UPDATE inbox_messages SET state='queued', current_attempt='', bound_run_id=?, reason=?, next_at=?, backoff_ms=?, updated_at=? WHERE message_id=?`,
			bound, r.Detail, ft(nextAt), backoff.Milliseconds(), ft(now), a.MessageID); err != nil {
			return m, a, err
		}
	} else {
		if _, err := tx.ExecContext(ctx, `UPDATE inbox_messages SET state=?, attempted=1, current_attempt='', reason=?, updated_at=? WHERE message_id=?`,
			string(state), r.Detail, ft(now), a.MessageID); err != nil {
			return Message{}, a, err
		}
	}
	a, err = getAttempt(ctx, tx, attemptID)
	if err != nil {
		return Message{}, a, err
	}
	m, err := getMsg(ctx, tx, a.MessageID)
	return m, a, err
}

// DeferTx keeps a queued message queued with a reason and the next time
// to try. It returns whether the reason differs from the last reported one.
func (s *Store) DeferTx(ctx context.Context, tx *sql.Tx, messageID, reason string, nextAt time.Time, backoff time.Duration, now time.Time) (changed bool, err error) {
	m, err := getMsg(ctx, tx, messageID)
	if err != nil {
		return false, err
	}
	if m.State != contracts.DeliveryQueued {
		return false, contracts.Errorf(contracts.ErrConflict, "message is %s, not queued", m.State).WithRef(contracts.KindMessage, messageID)
	}
	_, err = tx.ExecContext(ctx, `UPDATE inbox_messages SET reason=?, next_at=?, backoff_ms=?, updated_at=? WHERE message_id=?`,
		reason, ft(nextAt), backoff.Milliseconds(), ft(now), messageID)
	return reason != m.ReportedReason, err
}

// MarkReportedTx records the reason last reported to the hub.
func (s *Store) MarkReportedTx(ctx context.Context, tx *sql.Tx, messageID, reason string) error {
	_, err := tx.ExecContext(ctx, `UPDATE inbox_messages SET reported_reason=? WHERE message_id=?`, reason, messageID)
	return err
}

// CloseQueuedTx moves a queued message that was never possibly attempted
// to a terminal state without an attempt (expired, cancelled, rejected as
// unsupported, not_applicable).
func (s *Store) CloseQueuedTx(ctx context.Context, tx *sql.Tx, messageID string, state contracts.RuntimeDeliveryState, reason string, now time.Time) (Message, error) {
	m, err := getMsg(ctx, tx, messageID)
	if err != nil {
		return m, err
	}
	if m.State != contracts.DeliveryQueued {
		return m, contracts.Errorf(contracts.ErrConflict, "message is %s; only queued messages can become %s", m.State, state).WithRef(contracts.KindMessage, messageID)
	}
	if state == contracts.DeliveryCancelled && m.Attempted {
		return m, contracts.Errorf(contracts.ErrConflict, "message was already attempted; cancel is only possible before any attempt").WithRef(contracts.KindMessage, messageID)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE inbox_messages SET state=?, reason=?, updated_at=? WHERE message_id=?`, string(state), reason, ft(now), messageID); err != nil {
		return m, err
	}
	return getMsg(ctx, tx, messageID)
}

// ResolveByEvidenceTx applies asynchronously discovered evidence to a
// message that is attempting or indeterminate (never to a queued message:
// evidence for an unattempted message is ignored). Accepted evidence
// resolves indeterminate; rejected evidence resolves it only when the
// adapter proves non-submission.
func (s *Store) ResolveByEvidenceTx(ctx context.Context, tx *sql.Tx, messageID, attemptID string, r contracts.DeliveryResult, now time.Time) (Message, bool, error) {
	m, err := getMsg(ctx, tx, messageID)
	if err != nil {
		return m, false, err
	}
	switch m.State {
	case contracts.DeliveryAttempting, contracts.DeliveryIndeterminate:
	default:
		return m, false, nil
	}
	if r.State != contracts.DeliveryAccepted && r.State != contracts.DeliveryRejected {
		return m, false, nil
	}
	if attemptID == "" {
		attemptID = m.CurrentAttempt
	}
	if attemptID == "" {
		_ = tx.QueryRowContext(ctx, `SELECT attempt_id FROM inbox_attempts WHERE message_id=? AND state IN ('attempting','indeterminate') ORDER BY started_at DESC LIMIT 1`, messageID).Scan(&attemptID)
	}
	if attemptID != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE inbox_attempts SET state=?, evidence=?, evidence_ref=?, detail=?, finished_at=? WHERE attempt_id=?`,
			string(r.State), string(r.Evidence), r.EvidenceRef, r.Detail, ft(now), attemptID); err != nil {
			return m, false, err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE inbox_messages SET state=?, attempted=1, current_attempt='', reason=?, updated_at=? WHERE message_id=?`,
		string(r.State), r.Detail, ft(now), messageID); err != nil {
		return m, false, err
	}
	m, err = getMsg(ctx, tx, messageID)
	return m, true, err
}

// RecordPullTx records acceptance proven by pull evidence (the recipient
// run fetched the message itself, e.g. the Claude doorbell's inbox-tool
// read) for a message that has no open attempt: queued, or closed without
// a consuming attempt (rejected, expired, cancelled — facts beat intent).
// It mints the attempt (attemptID, already finished as accepted), binds the
// message to runID and marks it consumed. ok=false means the message was in
// a state this does not apply to (accepted, attempting, indeterminate,
// not_applicable) and nothing changed; a binding to another run is a
// conflict. Decision I16.
func (s *Store) RecordPullTx(ctx context.Context, tx *sql.Tx, messageID, runID, attemptID string, r contracts.DeliveryResult, now time.Time) (Message, bool, error) {
	m, err := getMsg(ctx, tx, messageID)
	if err != nil {
		return m, false, err
	}
	switch m.State {
	case contracts.DeliveryQueued, contracts.DeliveryRejected, contracts.DeliveryExpired, contracts.DeliveryCancelled:
	default:
		return m, false, nil
	}
	if m.Envelope.Mode == contracts.ModeInfo {
		return m, false, nil // informational: never runtime input, reading it is not delivery
	}
	if runID == "" || attemptID == "" {
		return m, false, contracts.Errorf(contracts.ErrInvalid, "pull evidence needs a run and an attempt id").WithRef(contracts.KindMessage, messageID)
	}
	if m.BoundRunID != "" && m.BoundRunID != runID {
		return m, false, contracts.Errorf(contracts.ErrConflict, "message is bound to run %s; evidence from run %s never rebinds it", m.BoundRunID, runID).WithRef(contracts.KindMessage, messageID)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO inbox_attempts(attempt_id, message_id, run_id, state, evidence, evidence_ref, detail, started_at, finished_at) VALUES(?,?,?,?,?,?,?,?,?)`,
		attemptID, messageID, runID, string(contracts.DeliveryAccepted), string(r.Evidence), r.EvidenceRef, r.Detail, ft(now), ft(now)); err != nil {
		return m, false, err
	}
	consumed := ""
	if contracts.ConsumptionEvidence(r.Evidence) {
		consumed = ft(now)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE inbox_messages SET state='accepted', attempted=1, current_attempt='', bound_run_id=?, reason=?, consumed_at=CASE WHEN consumed_at='' THEN ? ELSE consumed_at END, updated_at=? WHERE message_id=?`,
		runID, r.Detail, consumed, ft(now), messageID); err != nil {
		return m, false, err
	}
	m, err = getMsg(ctx, tx, messageID)
	return m, true, err
}

// MarkConsumedTx records evidence that accepted input entered a turn.
func (s *Store) MarkConsumedTx(ctx context.Context, tx *sql.Tx, messageID string, at time.Time) (bool, error) {
	r, err := tx.ExecContext(ctx, `UPDATE inbox_messages SET consumed_at=?, updated_at=? WHERE message_id=? AND consumed_at='' AND state='accepted'`, ft(at), ft(at), messageID)
	if err != nil {
		return false, err
	}
	n, _ := r.RowsAffected()
	return n == 1, nil
}

// Attempting returns attempts left in the attempting state (crash window).
func (s *Store) Attempting(ctx context.Context) ([]Attempt, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT attempt_id FROM inbox_attempts WHERE state='attempting' ORDER BY started_at`)
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
	out := make([]Attempt, 0, len(ids))
	for _, id := range ids {
		a, err := getAttempt(ctx, s.db, id)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

// Attempts returns all attempts of a message in order.
func (s *Store) Attempts(ctx context.Context, messageID string) ([]Attempt, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT attempt_id FROM inbox_attempts WHERE message_id=? ORDER BY started_at, attempt_id`, messageID)
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
	var out []Attempt
	for _, id := range ids {
		a, err := getAttempt(ctx, s.db, id)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

// Counts returns message counts by state (diagnostics).
func (s *Store) Counts(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT state, COUNT(*) FROM inbox_messages GROUP BY state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			return nil, err
		}
		out[st] = n
	}
	return out, rows.Err()
}

// Prune deletes terminal messages (and their attempts) last updated before
// cutoff. Indeterminate messages are retained until resolved: they are the
// evidence trail for a possible duplicate.
func (s *Store) Prune(ctx context.Context, cutoff time.Time) (int, error) {
	var n int64
	err := sqlitex.Tx(ctx, s.db, func(tx *sql.Tx) error {
		const where = `state IN ('accepted','rejected','expired','cancelled','not_applicable') AND updated_at < ?`
		if _, err := tx.ExecContext(ctx, `DELETE FROM inbox_attempts WHERE message_id IN (SELECT message_id FROM inbox_messages WHERE `+where+`)`, ft(cutoff)); err != nil {
			return err
		}
		r, err := tx.ExecContext(ctx, `DELETE FROM inbox_messages WHERE `+where, ft(cutoff))
		if err != nil {
			return err
		}
		n, _ = r.RowsAffected()
		return nil
	})
	return int(n), err
}
