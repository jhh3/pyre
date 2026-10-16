package attention

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"sync"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

// DefaultConsumer is the cursor name the deriver persists.
const DefaultConsumer = "attention.deriver"

// Snapshotter returns current state used to re-derive one-shot items after
// the event stream reports reset_required (cursor older than retention).
type Snapshotter interface {
	// PendingApprovals returns every approval currently pending.
	PendingApprovals(ctx context.Context) ([]contracts.Approval, error)
	// IndeterminateMessages returns messages whose runtime delivery is
	// currently indeterminate.
	IndeterminateMessages(ctx context.Context) ([]contracts.MessageView, error)
}

// DeriverOptions configures a Deriver.
type DeriverOptions struct {
	Consumer string      // default DefaultConsumer
	Lookup   Lookup      // optional; resolves account providers
	Snapshot Snapshotter // optional; without it reset gaps are only recorded
	// WaitSeconds is the long-poll wait passed to ListEvents (default 25, max 60).
	WaitSeconds int
	BatchLimit  int // default 200
	// MinPollInterval guards against EventAPI implementations that do not
	// block on long-poll (default 250ms).
	MinPollInterval time.Duration
	Logger          *slog.Logger
}

// Deriver consumes the hub event stream with a persisted cursor and turns
// events into attention items.
type Deriver struct {
	store  *Store
	events contracts.EventAPI
	opts   DeriverOptions
	log    *slog.Logger
}

// SystemPrincipal is the identity the deriver uses for hub reads.
var SystemPrincipal = contracts.Principal{Kind: contracts.PrincipalSystem, ID: "attention"}

// NewDeriver returns a Deriver reading events from events.
func NewDeriver(store *Store, events contracts.EventAPI, opts DeriverOptions) *Deriver {
	if opts.Consumer == "" {
		opts.Consumer = DefaultConsumer
	}
	if opts.WaitSeconds <= 0 {
		opts.WaitSeconds = 25
	}
	if opts.WaitSeconds > 60 {
		opts.WaitSeconds = 60
	}
	if opts.BatchLimit <= 0 {
		opts.BatchLimit = 200
	}
	if opts.MinPollInterval <= 0 {
		opts.MinPollInterval = 250 * time.Millisecond
	}
	log := opts.Logger
	if log == nil {
		log = store.log
	}
	return &Deriver{store: store, events: events, opts: opts, log: log}
}

// Run polls until ctx is cancelled. Transient errors back off exponentially
// (capped at 30s); the cursor only advances after events are applied.
func (d *Deriver) Run(ctx context.Context) error {
	backoff := time.Duration(0)
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		start := time.Now()
		n, err := d.Step(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if backoff == 0 {
				backoff = 500 * time.Millisecond
			} else {
				backoff = min(backoff*2, 30*time.Second)
			}
			d.log.Warn("attention: deriver step failed", "err", err, "retry_in", backoff)
			if !sleepCtx(ctx, backoff) {
				return nil
			}
			continue
		}
		backoff = 0
		if n == 0 {
			if el := time.Since(start); el < d.opts.MinPollInterval {
				if !sleepCtx(ctx, d.opts.MinPollInterval-el) {
					return nil
				}
			}
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// Step performs one poll: expire snoozes, fetch events after the persisted
// cursor, and apply them. It returns the number of events received.
func (d *Deriver) Step(ctx context.Context) (int, error) {
	return d.step(ctx, d.opts.WaitSeconds)
}

// StepNow is Step without the long-poll wait: it applies what is already
// committed and returns at once (read-your-writes catch-ups; a long poll
// there waited the full WaitSeconds whenever only events the deriver does
// not consume were pending).
func (d *Deriver) StepNow(ctx context.Context) (int, error) {
	return d.step(ctx, 0)
}

// WaitPending long-polls (read only, up to WaitSeconds) until an event the
// deriver consumes is committed after its persisted cursor. Callers that
// serialize steps with a lock wait here without holding it.
func (d *Deriver) WaitPending(ctx context.Context) error {
	ctx = contracts.WithPrincipal(ctx, SystemPrincipal)
	cur, err := d.store.Cursor(ctx, d.opts.Consumer)
	if err != nil {
		return err
	}
	_, err = d.events.ListEvents(ctx, contracts.ListEventsRequest{AfterCursor: cur, Limit: 1, Kinds: derivedKinds, WaitSeconds: d.opts.WaitSeconds})
	if contracts.IsCode(err, contracts.ErrResetRequired) {
		return nil // the next step resynchronizes
	}
	return err
}

func (d *Deriver) step(ctx context.Context, wait int) (int, error) {
	ctx = contracts.WithPrincipal(ctx, SystemPrincipal)
	if _, err := d.store.ExpireSnoozes(ctx); err != nil {
		return 0, err
	}
	cur, err := d.store.Cursor(ctx, d.opts.Consumer)
	if err != nil {
		return 0, err
	}
	resp, err := d.events.ListEvents(ctx, contracts.ListEventsRequest{
		AfterCursor: cur,
		Limit:       d.opts.BatchLimit,
		Kinds:       derivedKinds,
		WaitSeconds: wait,
	})
	if contracts.IsCode(err, contracts.ErrResetRequired) {
		return 0, d.resync(ctx, cur, contracts.AsError(err))
	}
	if err != nil {
		return 0, err
	}
	last := cur
	for _, ev := range resp.Events {
		if ev.Cursor > 0 && ev.Cursor <= cur {
			continue
		}
		changes := derive(ctx, ev, d.opts.Lookup)
		if _, err := d.store.applyEvent(ctx, d.opts.Consumer, ev, changes); err != nil {
			return 0, err
		}
		if ev.Cursor > last {
			last = ev.Cursor
		}
	}
	if resp.NextCursor > last {
		if err := d.store.advanceCursor(ctx, d.opts.Consumer, resp.NextCursor); err != nil {
			return 0, err
		}
	}
	return len(resp.Events), nil
}

func detailInt(details map[string]any, key string) int64 {
	switch v := details[key].(type) {
	case int64:
		return v
	case int:
		return int64(v)
	case float64:
		if v > 0 && v < math.MaxInt64 {
			return int64(v)
		}
	case string:
		n, _ := strconv.ParseInt(v, 10, 64)
		return n
	}
	return 0
}

// resync handles reset_required: re-derive one-shot items from current
// state when a Snapshotter is available, record the gap explicitly, and
// resume from the hub's snapshot cursor.
func (d *Deriver) resync(ctx context.Context, from int64, e *contracts.Error) error {
	resume := detailInt(e.Details, "snapshot_cursor")
	if resume <= 0 {
		if oldest := detailInt(e.Details, "oldest_cursor"); oldest > 0 {
			resume = oldest - 1
		}
	}
	if resume <= 0 {
		return fmt.Errorf("attention: reset_required without snapshot_cursor/oldest_cursor: %w", e)
	}
	var snap *snapshot
	if d.opts.Snapshot != nil {
		apr, err := d.opts.Snapshot.PendingApprovals(ctx)
		if err != nil {
			return fmt.Errorf("attention: snapshot approvals: %w", err)
		}
		msgs, err := d.opts.Snapshot.IndeterminateMessages(ctx)
		if err != nil {
			return fmt.Errorf("attention: snapshot deliveries: %w", err)
		}
		snap = &snapshot{approvals: apr, messages: msgs}
	}
	d.log.Warn("attention: event cursor expired; resynchronizing", "from", from, "resume", resume, "snapshot", snap != nil)
	return d.store.reconcile(ctx, d.opts.Consumer, from, resume, snap)
}

type snapshot struct {
	approvals []contracts.Approval
	messages  []contracts.MessageView
}

func (s *Store) reconcile(ctx context.Context, consumer string, from, resume int64, snap *snapshot) error {
	now := s.now()
	changed := false
	err := sqlitex.Tx(ctx, s.db, func(tx *sql.Tx) error {
		var detail string
		if snap != nil {
			n, err := s.reconcileSnapshotTx(ctx, tx, snap, resume, now)
			if err != nil {
				return err
			}
			changed = n > 0
			detail = fmt.Sprintf("event history between cursors %d and %d was unavailable (reset_required); re-derived %d pending approvals and %d indeterminate deliveries from current state (%d items changed); message, failure, account, review, comment, conflict and update attention from that range may be missing",
				from, resume, len(snap.approvals), len(snap.messages), n)
		} else {
			detail = fmt.Sprintf("event history between cursors %d and %d was unavailable (reset_required) and no snapshot source is configured; attention from that range may be missing", from, resume)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO attention_gaps(from_cursor,to_cursor,recorded_at,detail) VALUES(?,?,?,?)`,
			from, resume, sqlitex.FormatTime(now), detail); err != nil {
			return err
		}
		return setCursorTx(ctx, tx, consumer, resume, now)
	})
	if err == nil && changed {
		s.signal()
	}
	return err
}

func (s *Store) reconcileSnapshotTx(ctx context.Context, tx *sql.Tx, snap *snapshot, cursor int64, now time.Time) (int, error) {
	n := 0
	apply := func(ch change) error {
		c, err := s.applyChangeTx(ctx, tx, ch, cursor, now)
		if c {
			n++
		}
		return err
	}
	pending := map[string]bool{}
	for _, a := range snap.approvals {
		if a.ApprovalID == "" || (a.State != "" && a.State != contracts.ApprovalPending) {
			continue
		}
		pending[a.ApprovalID] = true
		if err := apply(approvalRaise(a, contracts.Event{SessionID: a.SessionID})); err != nil {
			return n, err
		}
	}
	indet := map[string]bool{}
	for _, m := range snap.messages {
		if m.Status.Runtime != contracts.DeliveryIndeterminate || m.Envelope.MessageID == "" {
			continue
		}
		indet[m.Envelope.MessageID] = true
		ev := contracts.Event{Kind: contracts.EvMessageDelivery, ProjectID: m.Envelope.ProjectID,
			Object: contracts.ObjectRef{Kind: contracts.KindMessage, ID: m.Envelope.MessageID}}
		ev.Payload, _ = json.Marshal(m)
		for _, ch := range deriveDelivery(ev) {
			if err := apply(ch); err != nil {
				return n, err
			}
		}
	}
	active, err := queryItems(ctx, tx, `WHERE kind IN (?,?,?) AND state IN (?,?,?)`,
		append([]any{string(contracts.AttnApproval), string(contracts.AttnQuestion), string(contracts.AttnIndeterminate)}, activeStates...)...)
	if err != nil {
		return n, err
	}
	for _, r := range active {
		stale := false
		switch r.item.Kind {
		case contracts.AttnApproval, contracts.AttnQuestion:
			stale = !pending[r.item.Object.ID]
		case contracts.AttnIndeterminate:
			stale = !indet[r.item.Object.ID]
		}
		if !stale {
			continue
		}
		if err := apply(change{op: opWithdraw, groupKey: r.item.GroupKey, summary: "Resolved while the inbox was catching up."}); err != nil {
			return n, err
		}
	}
	return n, nil
}

// ---- adapters over the public API ----

// APISnapshot implements Snapshotter over hub API interfaces. Sessions and
// Messages are optional; without them indeterminate deliveries are not
// re-derived (ListMessages requires an address, so the snapshot walks
// session inboxes).
type APISnapshot struct {
	Interaction contracts.InteractionAPI
	Sessions    contracts.SessionAPI
	Messages    contracts.MessageAPI
	MaxPages    int // bound per listing (default 50)
}

func (a APISnapshot) maxPages() int {
	if a.MaxPages <= 0 {
		return 50
	}
	return a.MaxPages
}

// PendingApprovals lists pending approvals.
func (a APISnapshot) PendingApprovals(ctx context.Context) ([]contracts.Approval, error) {
	if a.Interaction == nil {
		return nil, nil
	}
	ctx = contracts.WithPrincipal(ctx, SystemPrincipal)
	var out []contracts.Approval
	cursor := ""
	for i := 0; i < a.maxPages(); i++ {
		p, err := a.Interaction.ListApprovals(ctx, contracts.ListApprovalsRequest{
			PageRequest: contracts.PageRequest{Cursor: cursor, Limit: 200},
			States:      []contracts.ApprovalState{contracts.ApprovalPending},
		})
		if err != nil {
			return nil, err
		}
		out = append(out, p.Items...)
		if p.NextCursor == "" {
			break
		}
		cursor = p.NextCursor
	}
	return out, nil
}

// IndeterminateMessages walks non-archived session inboxes for
// indeterminate deliveries.
func (a APISnapshot) IndeterminateMessages(ctx context.Context) ([]contracts.MessageView, error) {
	if a.Sessions == nil || a.Messages == nil {
		return nil, nil
	}
	ctx = contracts.WithPrincipal(ctx, SystemPrincipal)
	var out []contracts.MessageView
	cursor := ""
	for i := 0; i < a.maxPages(); i++ {
		sp, err := a.Sessions.ListSessions(ctx, contracts.ListSessionsRequest{PageRequest: contracts.PageRequest{Cursor: cursor, Limit: 200}})
		if err != nil {
			return nil, err
		}
		for _, sv := range sp.Items {
			mc := ""
			for j := 0; j < a.maxPages(); j++ {
				mp, err := a.Messages.ListMessages(ctx, contracts.ListMessagesRequest{
					PageRequest:   contracts.PageRequest{Cursor: mc, Limit: 200},
					Box:           "inbox",
					Address:       &contracts.Address{Kind: contracts.AddrSession, ID: sv.Session.SessionID},
					RuntimeStates: []contracts.RuntimeDeliveryState{contracts.DeliveryIndeterminate},
				})
				if err != nil {
					return nil, err
				}
				out = append(out, mp.Items...)
				if mp.NextCursor == "" {
					break
				}
				mc = mp.NextCursor
			}
		}
		if sp.NextCursor == "" {
			break
		}
		cursor = sp.NextCursor
	}
	return out, nil
}

// APILookup implements Lookup and LabelLookup over contracts.AccountAPI
// (and optionally contracts.FleetAPI for node names) with a small cache.
type APILookup struct {
	Accounts contracts.AccountAPI
	Fleet    contracts.FleetAPI // optional; node names fall back to IDs

	mu    sync.Mutex
	cache map[string]contracts.Account
}

// account returns the (cached) account record. A miss reloads the listing,
// so labels changed since the last load are picked up for new accounts.
func (l *APILookup) account(ctx context.Context, accountID string) (contracts.Account, error) {
	l.mu.Lock()
	if a, ok := l.cache[accountID]; ok {
		l.mu.Unlock()
		return a, nil
	}
	l.mu.Unlock()
	if l.Accounts == nil {
		return contracts.Account{}, contracts.Errorf(contracts.ErrUnavailable, "no account API")
	}
	ctx = contracts.WithPrincipal(ctx, SystemPrincipal)
	cursor := ""
	for i := 0; i < 20; i++ {
		p, err := l.Accounts.ListAccounts(ctx, contracts.ListAccountsRequest{PageRequest: contracts.PageRequest{Cursor: cursor, Limit: 200}})
		if err != nil {
			return contracts.Account{}, err
		}
		l.mu.Lock()
		if l.cache == nil {
			l.cache = map[string]contracts.Account{}
		}
		for _, av := range p.Items {
			l.cache[av.Account.AccountID] = av.Account
		}
		got, ok := l.cache[accountID]
		l.mu.Unlock()
		if ok {
			return got, nil
		}
		if p.NextCursor == "" {
			break
		}
		cursor = p.NextCursor
	}
	return contracts.Account{}, contracts.Errorf(contracts.ErrNotFound, "account not found").WithRef(contracts.KindAccount, accountID)
}

// AccountProvider returns the provider of accountID.
func (l *APILookup) AccountProvider(ctx context.Context, accountID string) (contracts.Provider, error) {
	a, err := l.account(ctx, accountID)
	return a.Provider, err
}

// AccountLabel returns the label of accountID.
func (l *APILookup) AccountLabel(ctx context.Context, accountID string) (string, error) {
	a, err := l.account(ctx, accountID)
	return a.Label, err
}

// sessionRecorder is implemented by the hub: the stored session record,
// without the presentation view. GetSession builds a view that reads the
// attention store (flags), which waits for the deriver: called from a
// deriver step it would wait on itself.
type sessionRecorder interface {
	SessionRecord(ctx context.Context, sessionID string) (contracts.Session, error)
}

// session returns a session record when Fleet also serves sessions.
func (l *APILookup) session(ctx context.Context, sessionID string) (contracts.Session, error) {
	ctx = contracts.WithPrincipal(ctx, SystemPrincipal)
	if sr, ok := l.Fleet.(sessionRecorder); ok {
		return sr.SessionRecord(ctx, sessionID)
	}
	sg, ok := l.Fleet.(interface {
		GetSession(ctx context.Context, req contracts.GetRequest) (contracts.SessionView, error)
	})
	if !ok {
		return contracts.Session{}, contracts.Errorf(contracts.ErrUnavailable, "no session API")
	}
	v, err := sg.GetSession(ctx, contracts.GetRequest{ID: sessionID})
	return v.Session, err
}

// SessionTitle returns a session's title when Fleet also serves sessions.
func (l *APILookup) SessionTitle(ctx context.Context, sessionID string) (string, error) {
	s, err := l.session(ctx, sessionID)
	return s.Title, err
}

// SessionParent returns the session a session sits under ("" at the top
// level) when Fleet also serves sessions.
func (l *APILookup) SessionParent(ctx context.Context, sessionID string) (string, error) {
	s, err := l.session(ctx, sessionID)
	return s.Parent(), err
}

// NodeName returns the display name of nodeID.
func (l *APILookup) NodeName(ctx context.Context, nodeID string) (string, error) {
	if l.Fleet == nil {
		return "", contracts.Errorf(contracts.ErrUnavailable, "no fleet API")
	}
	n, err := l.Fleet.GetNode(contracts.WithPrincipal(ctx, SystemPrincipal), contracts.GetRequest{ID: nodeID})
	if err != nil {
		return "", err
	}
	return firstNonEmpty(n.Name, n.Hostname), nil
}
