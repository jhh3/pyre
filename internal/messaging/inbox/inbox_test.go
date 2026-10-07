package inbox

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

func openStore(t *testing.T, lim Limits) *Store {
	t.Helper()
	db, err := sqlitex.Open(filepath.Join(t.TempDir(), "node.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s, err := Open(context.Background(), db, lim)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func dc(id, session, text string) contracts.DeliverCommand {
	return contracts.DeliverCommand{SessionID: session, Message: contracts.MessageEnvelope{
		SchemaVersion: 1, MessageID: id, OperationID: "op_" + id,
		Sender:    contracts.Principal{Kind: contracts.PrincipalSession, ID: "ses_sender"},
		Recipient: contracts.Address{Kind: contracts.AddrSession, ID: session},
		Mode:      contracts.ModeQueue, Body: contracts.MessageBody{Text: text},
		CreatedAt: time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC),
	}}
}

func tx(t *testing.T, s *Store, fn func(*sql.Tx) error) {
	t.Helper()
	if err := sqlitex.Tx(context.Background(), s.DB(), fn); err != nil {
		t.Fatal(err)
	}
}

func TestPutDedupeAndConflict(t *testing.T) {
	s := openStore(t, Limits{})
	ctx := context.Background()
	now := time.Now()
	m, created, err := s.Put(ctx, dc("msg_1", "ses_a", "hello"), now)
	if err != nil || !created || m.State != contracts.DeliveryQueued {
		t.Fatalf("first put = %+v %v %v", m, created, err)
	}
	// Same content (different informational created_at) is a duplicate.
	d := dc("msg_1", "ses_a", "hello")
	d.Message.CreatedAt = now
	m2, created, err := s.Put(ctx, d, now)
	if err != nil || created || m2.Seq != m.Seq {
		t.Fatalf("dup put = %+v %v %v", m2, created, err)
	}
	_, _, err = s.Put(ctx, dc("msg_1", "ses_a", "different"), now)
	if !contracts.IsCode(err, contracts.ErrConflict) {
		t.Fatalf("conflicting put err = %v", err)
	}
	_, _, err = s.Put(ctx, dc("bad", "ses_a", "x"), now)
	if !contracts.IsCode(err, contracts.ErrInvalid) {
		t.Fatalf("invalid id err = %v", err)
	}
}

func TestCapacity(t *testing.T) {
	s := openStore(t, Limits{MaxPerSession: 2, MaxTotal: 3})
	ctx := context.Background()
	now := time.Now()
	for _, id := range []string{"msg_1", "msg_2"} {
		if _, _, err := s.Put(ctx, dc(id, "ses_a", id), now); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := s.Put(ctx, dc("msg_3", "ses_a", "x"), now); !contracts.IsCode(err, contracts.ErrCapacityExhausted) {
		t.Fatalf("per-session cap err = %v", err)
	}
	if _, _, err := s.Put(ctx, dc("msg_4", "ses_b", "x"), now); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Put(ctx, dc("msg_5", "ses_c", "x"), now); !contracts.IsCode(err, contracts.ErrCapacityExhausted) {
		t.Fatalf("total cap err = %v", err)
	}
}

func TestClaimBindsAndNeverMoves(t *testing.T) {
	s := openStore(t, Limits{})
	ctx := context.Background()
	now := time.Now()
	s.Put(ctx, dc("msg_1", "ses_a", "x"), now)
	tx(t, s, func(tx *sql.Tx) error {
		m, err := s.ClaimTx(ctx, tx, "msg_1", "run_1", "dlv_1", now)
		if err != nil {
			return err
		}
		if m.State != contracts.DeliveryAttempting || m.BoundRunID != "run_1" {
			t.Fatalf("claimed = %+v", m)
		}
		return nil
	})
	tx(t, s, func(tx *sql.Tx) error {
		m, _, err := s.FinishTx(ctx, tx, "dlv_1", contracts.DeliveryResult{State: contracts.DeliveryIndeterminate, Detail: "crash"}, now, time.Time{}, 0)
		if err != nil {
			return err
		}
		if m.State != contracts.DeliveryIndeterminate || !m.Attempted || m.BoundRunID != "run_1" {
			t.Fatalf("finished = %+v", m)
		}
		return nil
	})
	err := sqlitex.Tx(ctx, s.DB(), func(tx *sql.Tx) error {
		_, err := s.ClaimTx(ctx, tx, "msg_1", "run_2", "dlv_2", now)
		return err
	})
	if !contracts.IsCode(err, contracts.ErrConflict) {
		t.Fatalf("claim of indeterminate message = %v", err)
	}
	err = sqlitex.Tx(ctx, s.DB(), func(tx *sql.Tx) error {
		_, err := s.CloseQueuedTx(ctx, tx, "msg_1", contracts.DeliveryCancelled, "x", now)
		return err
	})
	if !contracts.IsCode(err, contracts.ErrConflict) {
		t.Fatalf("cancel after attempt = %v", err)
	}
	// Evidence resolves indeterminate.
	tx(t, s, func(tx *sql.Tx) error {
		m, ok, err := s.ResolveByEvidenceTx(ctx, tx, "msg_1", "", contracts.DeliveryResult{State: contracts.DeliveryAccepted, Evidence: contracts.EvidenceTranscript}, now)
		if err != nil || !ok || m.State != contracts.DeliveryAccepted {
			t.Fatalf("resolve = %+v %v %v", m, ok, err)
		}
		return nil
	})
	atts, _ := s.Attempts(ctx, "msg_1")
	if len(atts) != 1 || atts[0].State != contracts.DeliveryAccepted || atts[0].Evidence != contracts.EvidenceTranscript {
		t.Fatalf("attempts = %+v", atts)
	}
}

func TestDeferredAttemptReleasesBinding(t *testing.T) {
	s := openStore(t, Limits{})
	ctx := context.Background()
	now := time.Now()
	s.Put(ctx, dc("msg_1", "ses_a", "x"), now)
	tx(t, s, func(tx *sql.Tx) error {
		if _, err := s.ClaimTx(ctx, tx, "msg_1", "run_1", "dlv_1", now); err != nil {
			return err
		}
		m, a, err := s.FinishTx(ctx, tx, "dlv_1", contracts.DeliveryResult{State: contracts.DeliveryQueued, Detail: "busy"}, now, now.Add(time.Second), time.Second)
		if err != nil {
			return err
		}
		if m.State != contracts.DeliveryQueued || m.BoundRunID != "" || m.Attempted || a.State != AttemptDeferred || m.Reason != "busy" {
			t.Fatalf("deferred = %+v / %+v", m, a)
		}
		return nil
	})
	// A replacement run may now take it.
	tx(t, s, func(tx *sql.Tx) error {
		_, err := s.ClaimTx(ctx, tx, "msg_1", "run_2", "dlv_2", now)
		return err
	})
}

func TestHubBindingPersists(t *testing.T) {
	s := openStore(t, Limits{})
	ctx := context.Background()
	now := time.Now()
	d := dc("msg_1", "ses_a", "x")
	d.BoundRunID = "run_old"
	m, _, err := s.Put(ctx, d, now)
	if err != nil || m.BoundRunID != "run_old" || !m.HubBound {
		t.Fatalf("put = %+v %v", m, err)
	}
	err = sqlitex.Tx(ctx, s.DB(), func(tx *sql.Tx) error {
		_, err := s.ClaimTx(ctx, tx, "msg_1", "run_new", "dlv_1", now)
		return err
	})
	if !contracts.IsCode(err, contracts.ErrConflict) {
		t.Fatalf("claim on other run = %v", err)
	}
}

func TestAttemptingSurvivesReopenAndPrune(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "node.db")
	db, _ := sqlitex.Open(path)
	s, _ := Open(context.Background(), db, Limits{})
	ctx := context.Background()
	now := time.Now()
	s.Put(ctx, dc("msg_1", "ses_a", "x"), now)
	s.Put(ctx, dc("msg_2", "ses_a", "y"), now)
	sqlitex.Tx(ctx, db, func(tx *sql.Tx) error {
		_, err := s.ClaimTx(ctx, tx, "msg_1", "run_1", "dlv_1", now)
		return err
	})
	db.Close()

	db, _ = sqlitex.Open(path)
	defer db.Close()
	s, _ = Open(ctx, db, Limits{})
	atts, err := s.Attempting(ctx)
	if err != nil || len(atts) != 1 || atts[0].MessageID != "msg_1" || atts[0].RunID != "run_1" {
		t.Fatalf("attempting after reopen = %+v %v", atts, err)
	}
	sqlitex.Tx(ctx, db, func(tx *sql.Tx) error {
		_, err := s.CloseQueuedTx(ctx, tx, "msg_2", contracts.DeliveryExpired, "ttl", now.Add(-48*time.Hour))
		return err
	})
	n, err := s.Prune(ctx, now.Add(-24*time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("prune = %d %v", n, err)
	}
	c, _ := s.Counts(ctx)
	if c["attempting"] != 1 || c["expired"] != 0 {
		t.Fatalf("counts = %v", c)
	}
}
