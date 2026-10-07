package daemon

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

// Migrations is the "daemon" namespace: node identity, run records with
// persisted RunHandles, approval identity mapping, session-token secrets
// (0600 database), and the local message outbox (pending_local).
var Migrations = []sqlitex.Migration{{Namespace: "daemon", Version: 1, Name: "runs_approvals_outbox", SQL: `
CREATE TABLE daemon_meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE daemon_runs (
	run_id      TEXT PRIMARY KEY,
	session_id  TEXT NOT NULL,
	session     BLOB NOT NULL,
	run         BLOB NOT NULL,
	handle      BLOB,
	phase       TEXT NOT NULL,           -- launching | live | orphaned | ended
	fingerprint TEXT NOT NULL DEFAULT '',
	created_at  TEXT NOT NULL,
	updated_at  TEXT NOT NULL
);
CREATE INDEX daemon_runs_session ON daemon_runs(session_id, created_at);
CREATE INDEX daemon_runs_phase ON daemon_runs(phase);
CREATE TABLE daemon_secrets (
	run_id     TEXT PRIMARY KEY,
	session_id TEXT NOT NULL,
	token_hash TEXT NOT NULL,
	token      TEXT NOT NULL,
	created_at TEXT NOT NULL
);
CREATE INDEX daemon_secrets_hash ON daemon_secrets(token_hash);
CREATE TABLE daemon_approvals (
	approval_id TEXT PRIMARY KEY,
	run_id      TEXT NOT NULL,
	request_id  TEXT NOT NULL,
	state       TEXT NOT NULL,
	approval    BLOB NOT NULL,
	updated_at  TEXT NOT NULL,
	UNIQUE(run_id, request_id)
);
CREATE TABLE daemon_outbox (
	seq          INTEGER PRIMARY KEY AUTOINCREMENT,
	message_id   TEXT NOT NULL UNIQUE,
	operation_id TEXT NOT NULL UNIQUE,
	principal    BLOB NOT NULL,
	request      BLOB NOT NULL,
	digest       TEXT NOT NULL,
	state        TEXT NOT NULL,          -- pending | stored | rejected
	attempts     INTEGER NOT NULL DEFAULT 0,
	last_error   TEXT NOT NULL DEFAULT '',
	next_at      TEXT NOT NULL DEFAULT '',
	result       BLOB,
	created_at   TEXT NOT NULL,
	updated_at   TEXT NOT NULL
);
CREATE INDEX daemon_outbox_state ON daemon_outbox(state, seq);
`}}

// Run phases (daemon-internal lifecycle of a run record).
const (
	phaseLaunching = "launching" // record committed, adapter.Launch not yet returned
	phaseLive      = "live"      // handle persisted; observed by the poll loop
	phaseOrphaned  = "orphaned"  // daemon stopped mid-launch; no handle; never relaunched
	phaseEnded     = "ended"     // terminal state observed
)

type runRecord struct {
	Session     contracts.Session
	Run         contracts.Run
	Handle      *contracts.RunHandle
	Phase       string
	Fingerprint string
	CreatedAt   time.Time
}

type store struct {
	db *sql.DB
}

type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func ftime(t time.Time) string { return sqlitex.FormatTime(t) }

func (s *store) meta(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM daemon_meta WHERE key=?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

func (s *store) setMeta(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO daemon_meta(key, value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

const runCols = `session, run, handle, phase, fingerprint, created_at`

func scanRun(sc interface{ Scan(...any) error }) (runRecord, error) {
	var r runRecord
	var sess, run []byte
	var handle []byte
	var created string
	if err := sc.Scan(&sess, &run, &handle, &r.Phase, &r.Fingerprint, &created); err != nil {
		return r, err
	}
	if err := json.Unmarshal(sess, &r.Session); err != nil {
		return r, err
	}
	if err := json.Unmarshal(run, &r.Run); err != nil {
		return r, err
	}
	if len(handle) > 0 {
		var h contracts.RunHandle
		if err := json.Unmarshal(handle, &h); err != nil {
			return r, err
		}
		r.Handle = &h
	}
	r.CreatedAt, _ = sqlitex.ParseTime(created)
	return r, nil
}

func getRun(ctx context.Context, q querier, runID string) (runRecord, bool, error) {
	r, err := scanRun(q.QueryRowContext(ctx, `SELECT `+runCols+` FROM daemon_runs WHERE run_id=?`, runID))
	if errors.Is(err, sql.ErrNoRows) {
		return r, false, nil
	}
	return r, err == nil, err
}

func (s *store) run(ctx context.Context, runID string) (runRecord, bool, error) {
	return getRun(ctx, s.db, runID)
}

func (s *store) runs(ctx context.Context, where string, args ...any) ([]runRecord, error) {
	q := `SELECT ` + runCols + ` FROM daemon_runs`
	if where != "" {
		q += ` WHERE ` + where
	}
	q += ` ORDER BY created_at`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []runRecord
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// latestRunForSession returns the most recently created run of a session.
func (s *store) latestRunForSession(ctx context.Context, sessionID string) (runRecord, bool, error) {
	r, err := scanRun(s.db.QueryRowContext(ctx, `SELECT `+runCols+` FROM daemon_runs WHERE session_id=? ORDER BY created_at DESC, rowid DESC LIMIT 1`, sessionID))
	if errors.Is(err, sql.ErrNoRows) {
		return r, false, nil
	}
	return r, err == nil, err
}

// runForNative returns the most recent run whose harness-native session
// (Claude session UUID, Codex thread ID) is nativeID.
func (s *store) runForNative(ctx context.Context, nativeID string) (runRecord, bool, error) {
	r, err := scanRun(s.db.QueryRowContext(ctx, `SELECT `+runCols+` FROM daemon_runs WHERE json_extract(run, '$.native_ref.id')=? ORDER BY created_at DESC, rowid DESC LIMIT 1`, nativeID))
	if errors.Is(err, sql.ErrNoRows) {
		return r, false, nil
	}
	return r, err == nil, err
}

func putRunTx(ctx context.Context, tx *sql.Tx, r runRecord, now time.Time) error {
	sess, err := json.Marshal(r.Session)
	if err != nil {
		return err
	}
	run, err := json.Marshal(r.Run)
	if err != nil {
		return err
	}
	var handle []byte
	if r.Handle != nil {
		if handle, err = json.Marshal(r.Handle); err != nil {
			return err
		}
	}
	created := r.CreatedAt
	if created.IsZero() {
		created = now
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO daemon_runs(run_id, session_id, session, run, handle, phase, fingerprint, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?,?,?)
		ON CONFLICT(run_id) DO UPDATE SET session=excluded.session, run=excluded.run, handle=excluded.handle,
			phase=excluded.phase, fingerprint=excluded.fingerprint, updated_at=excluded.updated_at`,
		r.Run.RunID, r.Run.SessionID, sess, run, handle, r.Phase, r.Fingerprint, ftime(created), ftime(now))
	return err
}

func hashToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

func putSecretTx(ctx context.Context, tx *sql.Tx, runID, sessionID, token string, now time.Time) error {
	if token == "" {
		return nil
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO daemon_secrets(run_id, session_id, token_hash, token, created_at) VALUES(?,?,?,?,?)
		ON CONFLICT(run_id) DO UPDATE SET token_hash=excluded.token_hash, token=excluded.token`, runID, sessionID, hashToken(token), token, ftime(now))
	return err
}

// sessionForToken resolves a session token presented on the local socket.
func (s *store) sessionForToken(ctx context.Context, token string) (sessionID string, ok bool, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT session_id FROM daemon_secrets WHERE token_hash=? LIMIT 1`, hashToken(token)).Scan(&sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return sessionID, err == nil, err
}

// tokenForSession returns a stored session token (to flush that session's
// outbox entries with its own identity).
func (s *store) tokenForSession(ctx context.Context, sessionID string) (string, bool, error) {
	var tok string
	err := s.db.QueryRowContext(ctx, `SELECT token FROM daemon_secrets WHERE session_id=? ORDER BY created_at DESC LIMIT 1`, sessionID).Scan(&tok)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return tok, err == nil, err
}

// pruneSecrets deletes tokens of ended runs whose session has no pending
// outbox entries.
func (s *store) pruneSecrets(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM daemon_secrets WHERE run_id IN (SELECT run_id FROM daemon_runs WHERE phase='ended')
		AND session_id NOT IN (SELECT json_extract(principal, '$.id') FROM daemon_outbox WHERE state='pending')`)
	return err
}

// ---- approvals ----

func getApprovalByRequest(ctx context.Context, q querier, runID, requestID string) (contracts.Approval, bool, error) {
	var b []byte
	err := q.QueryRowContext(ctx, `SELECT approval FROM daemon_approvals WHERE run_id=? AND request_id=?`, runID, requestID).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return contracts.Approval{}, false, nil
	}
	if err != nil {
		return contracts.Approval{}, false, err
	}
	var a contracts.Approval
	return a, true, json.Unmarshal(b, &a)
}

func getApproval(ctx context.Context, q querier, approvalID string) (contracts.Approval, bool, error) {
	var b []byte
	err := q.QueryRowContext(ctx, `SELECT approval FROM daemon_approvals WHERE approval_id=?`, approvalID).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return contracts.Approval{}, false, nil
	}
	if err != nil {
		return contracts.Approval{}, false, err
	}
	var a contracts.Approval
	return a, true, json.Unmarshal(b, &a)
}

func putApprovalTx(ctx context.Context, tx *sql.Tx, a contracts.Approval, now time.Time) error {
	b, err := json.Marshal(a)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO daemon_approvals(approval_id, run_id, request_id, state, approval, updated_at) VALUES(?,?,?,?,?,?)
		ON CONFLICT(approval_id) DO UPDATE SET state=excluded.state, approval=excluded.approval, updated_at=excluded.updated_at`,
		a.ApprovalID, a.RunID, a.RequestID, string(a.State), b, ftime(now))
	return err
}

func pendingApprovals(ctx context.Context, q querier, runID string) ([]contracts.Approval, error) {
	rows, err := q.QueryContext(ctx, `SELECT approval FROM daemon_approvals WHERE run_id=? AND state='pending' ORDER BY approval_id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []contracts.Approval
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		var a contracts.Approval
		if err := json.Unmarshal(b, &a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
