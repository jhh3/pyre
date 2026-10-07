package attention

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/notifications"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

// Provider names accepted by RegisterPush and recorded in NotificationStatus.
const (
	ProviderFCM       = "fcm"
	ProviderHubStream = "hub-stream"
)

// DefaultFCMUnavailableDetail is reported by RegisterPush when the hub has no
// FCM service account configured.
const DefaultFCMUnavailableDetail = "FCM not configured on hub (no service account); items remain in the Inbox and on the hub event stream"

// Options configures a Store.
type Options struct {
	// Sink appends attention.changed events inside the store's write
	// transactions. Required.
	Sink contracts.EventSink
	// Clock defaults to contracts.SystemClock.
	Clock contracts.Clock
	// FCMConfigured reports whether the hub has an FCM service account and
	// a working notifications.FCM provider. When false, FCM registrations
	// are still recorded but RegisterPush reports Enabled=false.
	FCMConfigured bool
	// FCMDetail overrides DefaultFCMUnavailableDetail.
	FCMDetail string
	// SeenRetention bounds the dedup table of applied event IDs (default 20000).
	SeenRetention int
	Logger        *slog.Logger
}

// Store is the hub-side attention inbox. It implements contracts.AttentionAPI
// and notifications.Source. All tables live in the shared hub database under
// the "attention" migration namespace.
type Store struct {
	db        *sql.DB
	sink      contracts.EventSink
	clock     contracts.Clock
	fcm       bool
	fcmDetail string
	seenKeep  int
	log       *slog.Logger
	wake      chan struct{}
	applied   atomic.Int64
}

var (
	_ contracts.AttentionAPI = (*Store)(nil)
	_ notifications.Source   = (*Store)(nil)
	_ notifications.Waker    = (*Store)(nil)
)

// NewStore migrates the attention namespace (and the shared operations
// table) in db and returns a Store.
func NewStore(ctx context.Context, db *sql.DB, opts Options) (*Store, error) {
	if db == nil {
		return nil, errors.New("attention: nil db")
	}
	if opts.Sink == nil {
		return nil, errors.New("attention: EventSink is required")
	}
	if err := sqlitex.Migrate(ctx, db, Migrations()...); err != nil {
		return nil, err
	}
	s := &Store{
		db:        db,
		sink:      opts.Sink,
		clock:     opts.Clock,
		fcm:       opts.FCMConfigured,
		fcmDetail: opts.FCMDetail,
		seenKeep:  opts.SeenRetention,
		log:       opts.Logger,
		wake:      make(chan struct{}, 1),
	}
	if s.clock == nil {
		s.clock = contracts.SystemClock{}
	}
	if s.fcmDetail == "" {
		s.fcmDetail = DefaultFCMUnavailableDetail
	}
	if s.seenKeep <= 0 {
		s.seenKeep = 20000
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	return s, nil
}

// Wake signals that notification jobs may be due (notifications.Waker).
func (s *Store) Wake() <-chan struct{} { return s.wake }

func (s *Store) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Store) now() time.Time { return s.clock.Now().UTC() }

// ---- authorization ----

func requireOwner(ctx context.Context) (contracts.Principal, error) {
	p, ok := contracts.PrincipalFrom(ctx)
	if !ok || p.Kind == "" {
		return contracts.Principal{}, contracts.Errorf(contracts.ErrUnauthorized, "no authenticated principal")
	}
	switch p.Kind {
	case contracts.PrincipalOwner, contracts.PrincipalSystem:
		return p, nil
	}
	return contracts.Principal{}, contracts.Errorf(contracts.ErrForbidden, "the attention inbox belongs to the owner")
}

// ---- AttentionAPI ----

const (
	defaultLimit = 50
	maxLimit     = 200
)

// ListAttention lists inbox items, newest update first. Default states are
// open and read. Due snoozes are reopened before listing.
func (s *Store) ListAttention(ctx context.Context, req contracts.ListAttentionRequest) (contracts.Page[contracts.AttentionItem], error) {
	var page contracts.Page[contracts.AttentionItem]
	if _, err := requireOwner(ctx); err != nil {
		return page, err
	}
	if _, err := s.ExpireSnoozes(ctx); err != nil {
		return page, err
	}
	states := req.States
	if len(states) == 0 {
		states = []contracts.AttentionState{contracts.AttnOpen, contracts.AttnRead}
	}
	limit := req.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	if limit > maxLimit {
		limit = maxLimit
	}
	var where []string
	var args []any
	where = append(where, "state IN ("+placeholders(len(states))+")")
	for _, st := range states {
		args = append(args, string(st))
	}
	if len(req.Kinds) > 0 {
		where = append(where, "kind IN ("+placeholders(len(req.Kinds))+")")
		for _, k := range req.Kinds {
			args = append(args, string(k))
		}
	}
	if req.ProjectID != nil {
		if *req.ProjectID == "" {
			where = append(where, "project_id IS NULL")
		} else {
			where = append(where, "project_id = ?")
			args = append(args, *req.ProjectID)
		}
	}
	if req.SessionID != "" {
		where = append(where, "session_id = ?")
		args = append(args, req.SessionID)
	}
	if req.Cursor != "" {
		ts, id, err := decodeCursor(req.Cursor)
		if err != nil {
			return page, err
		}
		where = append(where, "(updated_at < ? OR (updated_at = ? AND attention_id < ?))")
		args = append(args, ts, ts, id)
	}
	q := "SELECT " + itemColumns + " FROM attention_items WHERE " + strings.Join(where, " AND ") +
		" ORDER BY updated_at DESC, attention_id DESC LIMIT ?"
	args = append(args, limit+1)
	page.SnapshotAt = s.now()
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	var raw []itemRow
	for rows.Next() {
		r, err := scanItem(rows)
		if err != nil {
			return page, err
		}
		raw = append(raw, r)
	}
	if err := rows.Err(); err != nil {
		return page, err
	}
	if len(raw) > limit {
		last := raw[limit-1]
		page.NextCursor = encodeCursor(sqlitex.FormatTime(last.item.UpdatedAt), last.item.AttentionID)
		raw = raw[:limit]
	}
	page.Items = make([]contracts.AttentionItem, 0, len(raw))
	for _, r := range raw {
		page.Items = append(page.Items, r.item)
	}
	return page, nil
}

// GetAttention returns one item in any state (owner only). Expired snoozes
// are reopened first so the returned state matches ListAttention.
func (s *Store) GetAttention(ctx context.Context, req contracts.GetRequest) (contracts.AttentionItem, error) {
	if _, err := requireOwner(ctx); err != nil {
		return contracts.AttentionItem{}, err
	}
	if req.ID == "" {
		return contracts.AttentionItem{}, contracts.Errorf(contracts.ErrInvalid, "id is required")
	}
	if _, err := s.ExpireSnoozes(ctx); err != nil {
		return contracts.AttentionItem{}, err
	}
	r, err := s.getQ(ctx, s.db, req.ID)
	return r.item, err
}

// UpdateAttention records the owner acting on an item: read, handled,
// snoozed (with a future snoozed_until), or open (mark unread; never pushes).
// Withdrawn items are returned unchanged: the underlying request no longer
// exists and the client should drop its controls.
func (s *Store) UpdateAttention(ctx context.Context, req contracts.UpdateAttentionRequest) (contracts.AttentionItem, error) {
	var out contracts.AttentionItem
	p, err := requireOwner(ctx)
	if err != nil {
		return out, err
	}
	if req.AttentionID == "" {
		return out, contracts.Errorf(contracts.ErrInvalid, "attention_id is required")
	}
	switch req.State {
	case contracts.AttnRead, contracts.AttnHandled, contracts.AttnOpen:
		if req.SnoozedUntil != nil {
			return out, contracts.Errorf(contracts.ErrInvalid, "snoozed_until is only valid with state snoozed")
		}
	case contracts.AttnSnoozed:
		if req.SnoozedUntil == nil || !req.SnoozedUntil.After(s.now()) {
			return out, contracts.Errorf(contracts.ErrInvalid, "state snoozed requires a future snoozed_until")
		}
	case contracts.AttnWithdrawn:
		return out, contracts.Errorf(contracts.ErrInvalid, "withdrawn is set by the hub when the underlying request is resolved")
	default:
		return out, contracts.Errorf(contracts.ErrInvalid, "unknown attention state %q", req.State)
	}
	changed := false
	err = sqlitex.Tx(ctx, s.db, func(tx *sql.Tx) error {
		_, err := sqlitex.Idempotent(ctx, tx, req.OperationID, contracts.OpUpdateAttention, p, req, &out, func() (any, error) {
			r, err := s.getTx(ctx, tx, req.AttentionID)
			if err != nil {
				return nil, err
			}
			if r.item.State == contracts.AttnWithdrawn {
				return r.item, nil
			}
			now := s.now()
			var until *time.Time
			if req.State == contracts.AttnSnoozed {
				u := req.SnoozedUntil.UTC()
				until = &u
			}
			if r.item.State == req.State && timePtrEqual(r.item.SnoozedUntil, until) {
				return r.item, nil
			}
			if err := s.setStateTx(ctx, tx, &r, req.State, until, now, false); err != nil {
				return nil, err
			}
			r.item.UpdatedAt = now
			if err := s.saveTx(ctx, tx, r); err != nil {
				return nil, err
			}
			if err := s.emitTx(ctx, tx, r.item, now); err != nil {
				return nil, err
			}
			changed = true
			return r.item, nil
		})
		return err
	})
	if err != nil {
		return contracts.AttentionItem{}, err
	}
	if changed {
		s.signal()
	}
	return out, nil
}

// GetNotificationPrefs returns stored preferences (zero value if unset).
func (s *Store) GetNotificationPrefs(ctx context.Context) (contracts.NotificationPrefs, error) {
	if _, err := requireOwner(ctx); err != nil {
		return contracts.NotificationPrefs{}, err
	}
	return s.Prefs(ctx)
}

// Prefs returns stored preferences without an authorization check
// (notifications.Source; hub-internal).
func (s *Store) Prefs(ctx context.Context) (contracts.NotificationPrefs, error) {
	var p contracts.NotificationPrefs
	var b string
	err := s.db.QueryRowContext(ctx, `SELECT prefs FROM attention_prefs WHERE id=1`).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return p, nil
	}
	if err != nil {
		return p, err
	}
	if err := json.Unmarshal([]byte(b), &p); err != nil {
		return p, fmt.Errorf("attention: decode prefs: %w", err)
	}
	return p, nil
}

// SetNotificationPrefs validates and replaces preferences. Preferences
// affect presentation only; durable inbox items are never dropped.
func (s *Store) SetNotificationPrefs(ctx context.Context, req contracts.SetNotificationPrefsRequest) (contracts.NotificationPrefs, error) {
	var out contracts.NotificationPrefs
	p, err := requireOwner(ctx)
	if err != nil {
		return out, err
	}
	prefs := normalizePrefs(req.Prefs)
	if err := notifications.ValidatePrefs(prefs); err != nil {
		return out, err
	}
	err = sqlitex.Tx(ctx, s.db, func(tx *sql.Tx) error {
		_, err := sqlitex.Idempotent(ctx, tx, req.OperationID, contracts.OpSetNotificationPrefs, p, req, &out, func() (any, error) {
			b, err := json.Marshal(prefs)
			if err != nil {
				return nil, err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO attention_prefs(id,prefs,updated_at) VALUES(1,?,?)
				ON CONFLICT(id) DO UPDATE SET prefs=excluded.prefs, updated_at=excluded.updated_at`,
				string(b), sqlitex.FormatTime(s.now())); err != nil {
				return nil, err
			}
			return prefs, nil
		})
		return err
	})
	if err != nil {
		return contracts.NotificationPrefs{}, err
	}
	return out, nil
}

func normalizePrefs(p contracts.NotificationPrefs) contracts.NotificationPrefs {
	p.QuietHoursStart = strings.TrimSpace(p.QuietHoursStart)
	p.QuietHoursEnd = strings.TrimSpace(p.QuietHoursEnd)
	p.TZ = strings.TrimSpace(p.TZ)
	p.MinSeverity = strings.TrimSpace(p.MinSeverity)
	seen := map[string]bool{}
	var muted []string
	for _, m := range p.MutedProjects {
		m = strings.TrimSpace(m)
		if m == "" || seen[m] {
			continue
		}
		seen[m] = true
		muted = append(muted, m)
	}
	sort.Strings(muted)
	p.MutedProjects = muted
	return p
}

// RegisterPush records a device push registration for the calling client.
// "hub-stream" is always enabled. "fcm" registrations are recorded even when
// the hub lacks an FCM service account, but are then reported honestly as
// Enabled=false with a detail.
func (s *Store) RegisterPush(ctx context.Context, req contracts.PushRegistration) (contracts.PushRegistrationResult, error) {
	var out contracts.PushRegistrationResult
	p, err := requireOwner(ctx)
	if err != nil {
		return out, err
	}
	clientID := req.ClientID
	switch p.Kind {
	case contracts.PrincipalOwner:
		if clientID != "" && clientID != p.ID {
			return out, contracts.Errorf(contracts.ErrForbidden, "a client may only register push for itself")
		}
		clientID = p.ID
	case contracts.PrincipalSystem:
		if clientID == "" {
			return out, contracts.Errorf(contracts.ErrInvalid, "client_id is required")
		}
	}
	token := strings.TrimSpace(req.DeviceToken)
	switch req.Provider {
	case ProviderFCM:
		if token == "" {
			return out, contracts.Errorf(contracts.ErrInvalid, "fcm registration requires device_token")
		}
		if len(token) > 4096 {
			return out, contracts.Errorf(contracts.ErrInvalid, "device_token too long")
		}
	case ProviderHubStream:
		token = ""
	default:
		return out, contracts.Errorf(contracts.ErrInvalid, "unknown push provider %q (want fcm or hub-stream)", req.Provider)
	}
	err = sqlitex.Tx(ctx, s.db, func(tx *sql.Tx) error {
		_, err := sqlitex.Idempotent(ctx, tx, req.OperationID, contracts.OpRegisterPush, p, req, &out, func() (any, error) {
			now := sqlitex.FormatTime(s.now())
			if token != "" {
				// A device token belongs to one app install; drop stale copies.
				if _, err := tx.ExecContext(ctx, `DELETE FROM attention_push WHERE provider=? AND device_token=? AND client_id<>?`, req.Provider, token, clientID); err != nil {
					return nil, err
				}
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO attention_push(provider,client_id,device_token,created_at,updated_at) VALUES(?,?,?,?,?)
				ON CONFLICT(provider,client_id) DO UPDATE SET device_token=excluded.device_token, updated_at=excluded.updated_at`,
				req.Provider, clientID, token, now, now); err != nil {
				return nil, err
			}
			res := contracts.PushRegistrationResult{Provider: req.Provider, Enabled: true}
			switch req.Provider {
			case ProviderFCM:
				if !s.fcm {
					res.Enabled = false
					res.Detail = s.fcmDetail
				}
			case ProviderHubStream:
				res.Detail = "alerts are published on the hub event stream; delivered only while a client holds the stream"
			}
			return res, nil
		})
		return err
	})
	if err != nil {
		return contracts.PushRegistrationResult{}, err
	}
	return out, nil
}

// RemoveClientPush deletes every push registration of a client. The hub
// calls it when a client is revoked.
func (s *Store) RemoveClientPush(ctx context.Context, clientID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM attention_push WHERE client_id=?`, clientID)
	return err
}

// ---- hub helpers ----

// Get returns one item (no authorization check; hub-internal).
func (s *Store) Get(ctx context.Context, attentionID string) (contracts.AttentionItem, error) {
	r, err := s.getQ(ctx, s.db, attentionID)
	return r.item, err
}

// CountOpen returns the number of open items for a session (for
// SessionView.Attention).
func (s *Store) CountOpen(ctx context.Context, sessionID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM attention_items WHERE session_id=? AND state=?`, sessionID, string(contracts.AttnOpen)).Scan(&n)
	return n, err
}

// Gap is an explicitly recorded range of events that could not be replayed.
type Gap struct {
	FromCursor int64     `json:"from_cursor"`
	ToCursor   int64     `json:"to_cursor"`
	RecordedAt time.Time `json:"recorded_at"`
	Detail     string    `json:"detail"`
}

// Gaps lists recorded event gaps, oldest first.
func (s *Store) Gaps(ctx context.Context) ([]Gap, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT from_cursor,to_cursor,recorded_at,detail FROM attention_gaps ORDER BY gap_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Gap
	for rows.Next() {
		var g Gap
		var at string
		if err := rows.Scan(&g.FromCursor, &g.ToCursor, &at, &g.Detail); err != nil {
			return nil, err
		}
		g.RecordedAt, _ = sqlitex.ParseTime(at)
		out = append(out, g)
	}
	return out, rows.Err()
}

// Cursor returns the persisted event cursor for a consumer (0 if none).
func (s *Store) Cursor(ctx context.Context, consumer string) (int64, error) {
	return cursorQ(ctx, s.db, consumer)
}

// ExpireSnoozes reopens snoozed items whose snoozed_until has passed. A
// reopened item is a new open transition and is eligible for one push.
func (s *Store) ExpireSnoozes(ctx context.Context) (int, error) {
	now := s.now()
	var due int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM attention_items WHERE state=? AND snoozed_until IS NOT NULL AND snoozed_until<=?`,
		string(contracts.AttnSnoozed), sqlitex.FormatTime(now)).Scan(&due); err != nil {
		return 0, err
	}
	if due == 0 {
		return 0, nil
	}
	n := 0
	err := sqlitex.Tx(ctx, s.db, func(tx *sql.Tx) error {
		rows, err := queryItems(ctx, tx, `WHERE state=? AND snoozed_until IS NOT NULL AND snoozed_until<=?`,
			string(contracts.AttnSnoozed), sqlitex.FormatTime(now))
		if err != nil {
			return err
		}
		for _, r := range rows {
			if err := s.setStateTx(ctx, tx, &r, contracts.AttnOpen, nil, now, true); err != nil {
				return err
			}
			r.item.UpdatedAt = now
			if err := s.saveTx(ctx, tx, r); err != nil {
				return err
			}
			if err := s.emitTx(ctx, tx, r.item, now); err != nil {
				return err
			}
			n++
		}
		return nil
	})
	if err == nil && n > 0 {
		s.signal()
	}
	return n, err
}

// ---- small helpers ----

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func encodeCursor(ts, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(ts + "|" + id))
}

func decodeCursor(c string) (string, string, error) {
	b, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return "", "", contracts.Errorf(contracts.ErrInvalid, "malformed cursor")
	}
	ts, id, ok := strings.Cut(string(b), "|")
	if !ok || ts == "" || id == "" {
		return "", "", contracts.Errorf(contracts.ErrInvalid, "malformed cursor")
	}
	return ts, id, nil
}

func timePtrEqual(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}
