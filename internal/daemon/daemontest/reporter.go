package daemontest

import (
	"context"
	"database/sql"
	"encoding/json"
	"sync"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

// Reporter is a fake durable report outbox with NodeLink's EnqueueTx
// semantics: reports are written inside the caller's transaction, so a
// rolled-back state change never leaks a report.
type Reporter struct {
	db *sql.DB
	// FailOn, if set, makes EnqueueTx fail for matching reports (crash
	// injection between a runtime effect and its durable record).
	FailOn func(kind contracts.ReportKind, payload any) error

	mu        sync.Mutex
	connected bool
	statuses  []contracts.NodeStatusReport
}

// NewReporter creates the fake outbox table in db.
func NewReporter(db *sql.DB) (*Reporter, error) {
	if err := sqlitex.Migrate(context.Background(), db, sqlitex.Migration{Namespace: "fakeoutbox", Version: 1, Name: "outbox", SQL: `
CREATE TABLE fake_outbox (seq INTEGER PRIMARY KEY AUTOINCREMENT, kind TEXT NOT NULL, at TEXT NOT NULL, payload BLOB NOT NULL);`}); err != nil {
		return nil, err
	}
	return &Reporter{db: db, connected: true}, nil
}

func (r *Reporter) EnqueueTx(ctx context.Context, tx *sql.Tx, kind contracts.ReportKind, at time.Time, payload any) (uint64, error) {
	if r.FailOn != nil {
		if err := r.FailOn(kind, payload); err != nil {
			return 0, err
		}
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO fake_outbox(kind, at, payload) VALUES(?,?,?)`, string(kind), sqlitex.FormatTime(at), b)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	return uint64(id), nil
}

func (r *Reporter) Kick() {}

// Enqueue appends a durable report in its own transaction.
func (r *Reporter) Enqueue(ctx context.Context, kind contracts.ReportKind, payload any) (uint64, error) {
	var seq uint64
	err := sqlitex.Tx(ctx, r.db, func(tx *sql.Tx) error {
		var err error
		seq, err = r.EnqueueTx(ctx, tx, kind, time.Now(), payload)
		return err
	})
	return seq, err
}

// SetConnected simulates hub connectivity for SendStatus.
func (r *Reporter) SetConnected(v bool) {
	r.mu.Lock()
	r.connected = v
	r.mu.Unlock()
}

func (r *Reporter) Connected() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.connected
}

func (r *Reporter) SendStatus(ctx context.Context, st contracts.NodeStatusReport) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.connected {
		return contracts.Errorf(contracts.ErrUnavailable, "fake: not connected")
	}
	r.statuses = append(r.statuses, st)
	return nil
}

// Statuses returns status reports sent so far.
func (r *Reporter) Statuses() []contracts.NodeStatusReport {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]contracts.NodeStatusReport(nil), r.statuses...)
}

// Report is a stored durable report.
type Report struct {
	Seq     uint64
	Kind    contracts.ReportKind
	Payload json.RawMessage
}

// Reports returns stored reports of the given kind (all if empty).
func (r *Reporter) Reports(kind contracts.ReportKind) []Report {
	q := `SELECT seq, kind, payload FROM fake_outbox`
	var args []any
	if kind != "" {
		q += ` WHERE kind=?`
		args = append(args, string(kind))
	}
	q += ` ORDER BY seq`
	rows, err := r.db.Query(q, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []Report
	for rows.Next() {
		var rep Report
		var b []byte
		if rows.Scan(&rep.Seq, &rep.Kind, &b) == nil {
			rep.Payload = b
			out = append(out, rep)
		}
	}
	return out
}

// Receipts decodes receipt reports, optionally for one message.
func (r *Reporter) Receipts(messageID string) []contracts.DeliveryReceipt {
	var out []contracts.DeliveryReceipt
	for _, rep := range r.Reports(contracts.ReportReceipt) {
		var rc contracts.DeliveryReceipt
		if json.Unmarshal(rep.Payload, &rc) == nil && (messageID == "" || rc.MessageID == messageID) {
			out = append(out, rc)
		}
	}
	return out
}
