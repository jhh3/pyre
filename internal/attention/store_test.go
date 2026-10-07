package attention

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/attention/attentiontest"
	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

var t0 = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

type env struct {
	t     *testing.T
	path  string
	db    *sql.DB
	store *Store
	log   *attentiontest.Log
	clock *attentiontest.Clock
	der   *Deriver
	snap  Snapshotter
}

func newEnv(t *testing.T, opts ...func(*Options)) *env {
	t.Helper()
	e := &env{t: t, path: filepath.Join(t.TempDir(), "hub.db"), log: attentiontest.NewLog(), clock: attentiontest.NewClock(t0)}
	e.open(opts...)
	return e
}

func (e *env) open(opts ...func(*Options)) {
	e.t.Helper()
	db, err := sqlitex.Open(e.path)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { db.Close() })
	o := Options{Sink: e.log, Clock: e.clock}
	for _, f := range opts {
		f(&o)
	}
	s, err := NewStore(context.Background(), db, o)
	if err != nil {
		e.t.Fatal(err)
	}
	e.db, e.store = db, s
	e.der = NewDeriver(s, e.log, DeriverOptions{Lookup: fakeLookup{}, Snapshot: e.snap})
}

// drain steps the deriver until it has consumed the whole log.
func (e *env) drain() {
	e.t.Helper()
	for i := 0; i < 100; i++ {
		n, err := e.der.Step(context.Background())
		if err != nil {
			e.t.Fatal(err)
		}
		if n == 0 {
			return
		}
	}
	e.t.Fatal("deriver did not quiesce")
}

func ownerCtx() context.Context {
	return contracts.WithPrincipal(context.Background(), contracts.Principal{Kind: contracts.PrincipalOwner, ID: "cli_synth-phone"})
}

func (e *env) list(states ...contracts.AttentionState) []contracts.AttentionItem {
	e.t.Helper()
	p, err := e.store.ListAttention(ownerCtx(), contracts.ListAttentionRequest{States: states, PageRequest: contracts.PageRequest{Limit: 200}})
	if err != nil {
		e.t.Fatal(err)
	}
	return p.Items
}

func (e *env) byGroup(group string) []contracts.AttentionItem {
	var out []contracts.AttentionItem
	for _, it := range e.list(contracts.AttnOpen, contracts.AttnRead, contracts.AttnHandled, contracts.AttnWithdrawn, contracts.AttnSnoozed) {
		if it.GroupKey == group {
			out = append(out, it)
		}
	}
	return out
}

func (e *env) jobs() int {
	e.t.Helper()
	var n int
	if err := e.db.QueryRow(`SELECT COUNT(*) FROM attention_notify WHERE done=0`).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *env) approval(id string, kind contracts.EventKind, mod func(*contracts.Approval)) contracts.Event {
	a := contracts.Approval{ApprovalID: id, SessionID: "ses_synth-a", RunID: "run_synth-a", Kind: contracts.ApprovalPermission, Title: "run tests", State: contracts.ApprovalPending}
	if mod != nil {
		mod(&a)
	}
	return e.log.AppendPayload(kind, contracts.ObjectRef{Kind: contracts.KindApproval, ID: id}, a.SessionID, nil, a)
}

func (e *env) ownerMessage(id, thread string, sender string) contracts.Event {
	v := contracts.MessageView{Envelope: contracts.MessageEnvelope{MessageID: id, Sender: contracts.Principal{Kind: contracts.PrincipalSession, ID: sender},
		Recipient: contracts.Address{Kind: contracts.AddrOwner, ID: "owner"}, ThreadID: thread, Mode: contracts.ModeQueue, Body: contracts.MessageBody{Text: secret}}}
	return e.log.AppendPayload(contracts.EvMessageStored, contracts.ObjectRef{Kind: contracts.KindMessage, ID: id}, sender, nil, v)
}

func (e *env) peerMessage(id string) contracts.Event {
	v := contracts.MessageView{Envelope: contracts.MessageEnvelope{MessageID: id, Sender: contracts.Principal{Kind: contracts.PrincipalSession, ID: "ses_synth-a"},
		Recipient: contracts.Address{Kind: contracts.AddrSession, ID: "ses_synth-b"}, Mode: contracts.ModeInfo, Body: contracts.MessageBody{Text: "progress 10%"}}}
	return e.log.AppendPayload(contracts.EvMessageStored, contracts.ObjectRef{Kind: contracts.KindMessage, ID: id}, "ses_synth-a", nil, v)
}

func TestUpdateAttentionStates(t *testing.T) {
	e := newEnv(t)
	e.approval("apr_synth-1", contracts.EvApprovalRequested, nil)
	e.drain()
	items := e.list()
	if len(items) != 1 {
		t.Fatalf("items = %d", len(items))
	}
	id := items[0].AttentionID
	ctx := ownerCtx()

	upd := func(op string, st contracts.AttentionState, until *time.Time) (contracts.AttentionItem, error) {
		return e.store.UpdateAttention(ctx, contracts.UpdateAttentionRequest{Mutation: contracts.Mutation{OperationID: op}, AttentionID: id, State: st, SnoozedUntil: until})
	}
	it, err := upd("op_synth-1", contracts.AttnRead, nil)
	if err != nil || it.State != contracts.AttnRead {
		t.Fatalf("read: %v %v", it.State, err)
	}
	// Idempotent replay returns the stored result.
	it, err = upd("op_synth-1", contracts.AttnRead, nil)
	if err != nil || it.State != contracts.AttnRead {
		t.Fatalf("replay: %v %v", it.State, err)
	}
	// Same op ID, different body: conflict.
	if _, err := upd("op_synth-1", contracts.AttnHandled, nil); !contracts.IsCode(err, contracts.ErrConflict) {
		t.Fatalf("want conflict, got %v", err)
	}
	past := t0.Add(-time.Hour)
	if _, err := upd("op_synth-2", contracts.AttnSnoozed, &past); !contracts.IsCode(err, contracts.ErrInvalid) {
		t.Fatalf("past snooze: %v", err)
	}
	if _, err := upd("op_synth-3", contracts.AttnWithdrawn, nil); !contracts.IsCode(err, contracts.ErrInvalid) {
		t.Fatalf("client withdraw: %v", err)
	}
	if _, err := e.store.UpdateAttention(ctx, contracts.UpdateAttentionRequest{Mutation: contracts.Mutation{OperationID: "op_synth-4"}, AttentionID: "att_synth-missing", State: contracts.AttnRead}); !contracts.IsCode(err, contracts.ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	it, err = upd("op_synth-5", contracts.AttnHandled, nil)
	if err != nil || it.State != contracts.AttnHandled {
		t.Fatalf("handled: %v %v", it.State, err)
	}
	if got := e.list(); len(got) != 0 {
		t.Fatalf("handled item still in default list: %+v", got)
	}

	// Authorization.
	sessCtx := contracts.WithPrincipal(context.Background(), contracts.Principal{Kind: contracts.PrincipalSession, ID: "ses_synth-a"})
	if _, err := e.store.ListAttention(sessCtx, contracts.ListAttentionRequest{}); !contracts.IsCode(err, contracts.ErrForbidden) {
		t.Fatalf("session list: %v", err)
	}
	if _, err := e.store.ListAttention(context.Background(), contracts.ListAttentionRequest{}); !contracts.IsCode(err, contracts.ErrUnauthorized) {
		t.Fatalf("anonymous list: %v", err)
	}
	// Operation IDs are required.
	if _, err := upd("", contracts.AttnRead, nil); !contracts.IsCode(err, contracts.ErrInvalid) {
		t.Fatalf("missing op id: %v", err)
	}
}

func TestWithdrawnItemIgnoresStaleTaps(t *testing.T) {
	e := newEnv(t)
	e.approval("apr_synth-1", contracts.EvApprovalRequested, nil)
	e.drain()
	id := e.list()[0].AttentionID
	e.approval("apr_synth-1", contracts.EvApprovalResolved, func(a *contracts.Approval) {
		a.State = contracts.ApprovalResolved
		a.Resolution = &contracts.ApprovalResolution{Source: "native"}
	})
	e.drain()
	it, err := e.store.UpdateAttention(ownerCtx(), contracts.UpdateAttentionRequest{Mutation: contracts.Mutation{OperationID: "op_synth-stale"}, AttentionID: id, State: contracts.AttnHandled})
	if err != nil {
		t.Fatal(err)
	}
	if it.State != contracts.AttnWithdrawn || it.Summary != "Resolved in the terminal." {
		t.Fatalf("stale tap changed a withdrawn item: %+v", it)
	}
}

func TestSnoozeExpiryReopensAndQueuesNotify(t *testing.T) {
	e := newEnv(t)
	e.ownerMessage("msg_synth-1", "thr_synth-1", "ses_synth-a")
	e.drain()
	id := e.list()[0].AttentionID
	until := t0.Add(time.Hour)
	if _, err := e.store.UpdateAttention(ownerCtx(), contracts.UpdateAttentionRequest{Mutation: contracts.Mutation{OperationID: "op_synth-s"}, AttentionID: id, State: contracts.AttnSnoozed, SnoozedUntil: &until}); err != nil {
		t.Fatal(err)
	}
	if got := e.list(); len(got) != 0 {
		t.Fatalf("snoozed item listed by default")
	}
	// New activity while snoozed collapses but stays snoozed.
	e.ownerMessage("msg_synth-2", "thr_synth-1", "ses_synth-a")
	e.drain()
	sn := e.list(contracts.AttnSnoozed)
	if len(sn) != 1 || sn[0].Count != 2 {
		t.Fatalf("snoozed collapse: %+v", sn)
	}
	e.clock.Advance(2 * time.Hour)
	got := e.list()
	if len(got) != 1 || got[0].State != contracts.AttnOpen || got[0].SnoozedUntil != nil {
		t.Fatalf("snooze did not expire: %+v", got)
	}
	_, seq, _ := e.store.ItemForJob(context.Background(), id)
	if seq != 2 {
		t.Fatalf("reopen should be transition 2, got %d", seq)
	}
	if got[0].Notification == nil || got[0].Notification.State != "pending" {
		t.Fatalf("reopen should queue a notification: %+v", got[0].Notification)
	}
}

func TestReadItemReopensOnNewActivity(t *testing.T) {
	e := newEnv(t)
	e.ownerMessage("msg_synth-1", "thr_synth-1", "ses_synth-a")
	e.drain()
	id := e.list()[0].AttentionID
	if _, err := e.store.UpdateAttention(ownerCtx(), contracts.UpdateAttentionRequest{Mutation: contracts.Mutation{OperationID: "op_synth-r"}, AttentionID: id, State: contracts.AttnRead}); err != nil {
		t.Fatal(err)
	}
	e.ownerMessage("msg_synth-2", "thr_synth-1", "ses_synth-a")
	e.drain()
	got := e.byGroup("message:thr_synth-1")
	if len(got) != 1 || got[0].State != contracts.AttnOpen || got[0].Count != 2 || got[0].AttentionID != id {
		t.Fatalf("reopen: %+v", got)
	}
	// Owner replies in the thread: handled, and a new message afterwards
	// starts a new incident.
	v := contracts.MessageView{Envelope: contracts.MessageEnvelope{MessageID: "msg_synth-3", Sender: contracts.Principal{Kind: contracts.PrincipalOwner, ID: "cli_synth-phone"},
		Recipient: contracts.Address{Kind: contracts.AddrSession, ID: "ses_synth-a"}, ThreadID: "thr_synth-1"}}
	e.log.AppendPayload(contracts.EvMessageStored, contracts.ObjectRef{Kind: contracts.KindMessage, ID: "msg_synth-3"}, "", nil, v)
	e.drain()
	got = e.byGroup("message:thr_synth-1")
	if len(got) != 1 || got[0].State != contracts.AttnHandled {
		t.Fatalf("owner reply should handle: %+v", got)
	}
	e.ownerMessage("msg_synth-4", "thr_synth-1", "ses_synth-a")
	e.drain()
	if got = e.byGroup("message:thr_synth-1"); len(got) != 2 {
		t.Fatalf("new activity after handled should open a new item: %+v", got)
	}
}

func TestPrefs(t *testing.T) {
	e := newEnv(t)
	ctx := ownerCtx()
	p, err := e.store.GetNotificationPrefs(ctx)
	if err != nil || p.QuietHoursStart != "" {
		t.Fatalf("default prefs: %+v %v", p, err)
	}
	want := contracts.NotificationPrefs{QuietHoursStart: "22:00", QuietHoursEnd: "07:00", TZ: "America/New_York", MinSeverity: "normal",
		MutedProjects: []string{"prj_synth-b", "prj_synth-a", "prj_synth-a"}, Kinds: map[contracts.AttentionKind]bool{contracts.AttnMessage: false}}
	got, err := e.store.SetNotificationPrefs(ctx, contracts.SetNotificationPrefsRequest{Mutation: contracts.Mutation{OperationID: "op_synth-p"}, Prefs: want})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.MutedProjects) != 2 || got.MutedProjects[0] != "prj_synth-a" {
		t.Fatalf("muted not normalized: %+v", got.MutedProjects)
	}
	back, _ := e.store.GetNotificationPrefs(ctx)
	if back.TZ != "America/New_York" || back.Kinds[contracts.AttnMessage] != false || len(back.Kinds) != 1 {
		t.Fatalf("roundtrip: %+v", back)
	}
	bad := []contracts.NotificationPrefs{
		{QuietHoursStart: "22:00"},
		{QuietHoursStart: "25:00", QuietHoursEnd: "07:00"},
		{TZ: "Mars/Olympus"},
		{MinSeverity: "critical"},
		{Kinds: map[contracts.AttentionKind]bool{"bogus": true}},
	}
	for i, b := range bad {
		if _, err := e.store.SetNotificationPrefs(ctx, contracts.SetNotificationPrefsRequest{Mutation: contracts.Mutation{OperationID: "op_synth-bad-" + string(rune('a'+i))}, Prefs: b}); !contracts.IsCode(err, contracts.ErrInvalid) {
			t.Errorf("bad prefs %d accepted: %v", i, err)
		}
	}
}

func TestRegisterPush(t *testing.T) {
	ctx := ownerCtx()
	e := newEnv(t)
	res, err := e.store.RegisterPush(ctx, contracts.PushRegistration{Mutation: contracts.Mutation{OperationID: "op_synth-r1"}, Provider: ProviderFCM, DeviceToken: "synthetic-fcm-token-1"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Enabled || res.Detail == "" {
		t.Fatalf("FCM must be reported disabled when unconfigured: %+v", res)
	}
	res, err = e.store.RegisterPush(ctx, contracts.PushRegistration{Mutation: contracts.Mutation{OperationID: "op_synth-r2"}, Provider: ProviderHubStream})
	if err != nil || !res.Enabled {
		t.Fatalf("hub-stream: %+v %v", res, err)
	}
	if _, err := e.store.RegisterPush(ctx, contracts.PushRegistration{Mutation: contracts.Mutation{OperationID: "op_synth-r3"}, Provider: ProviderFCM}); !contracts.IsCode(err, contracts.ErrInvalid) {
		t.Fatalf("missing token: %v", err)
	}
	if _, err := e.store.RegisterPush(ctx, contracts.PushRegistration{Mutation: contracts.Mutation{OperationID: "op_synth-r4"}, Provider: "apns", DeviceToken: "x"}); !contracts.IsCode(err, contracts.ErrInvalid) {
		t.Fatalf("unknown provider: %v", err)
	}
	if _, err := e.store.RegisterPush(ctx, contracts.PushRegistration{Mutation: contracts.Mutation{OperationID: "op_synth-r5"}, Provider: ProviderFCM, DeviceToken: "t", ClientID: "cli_synth-other"}); !contracts.IsCode(err, contracts.ErrForbidden) {
		t.Fatalf("foreign client: %v", err)
	}
	// Token refresh replaces the registration for the same client.
	if _, err := e.store.RegisterPush(ctx, contracts.PushRegistration{Mutation: contracts.Mutation{OperationID: "op_synth-r6"}, Provider: ProviderFCM, DeviceToken: "synthetic-fcm-token-2"}); err != nil {
		t.Fatal(err)
	}
	ts, err := e.store.Targets(context.Background(), ProviderFCM)
	if err != nil || len(ts) != 1 || ts[0].DeviceToken != "synthetic-fcm-token-2" || ts[0].ClientID != "cli_synth-phone" {
		t.Fatalf("targets: %+v %v", ts, err)
	}

	e2 := newEnv(t, func(o *Options) { o.FCMConfigured = true })
	res, err = e2.store.RegisterPush(ctx, contracts.PushRegistration{Mutation: contracts.Mutation{OperationID: "op_synth-r1"}, Provider: ProviderFCM, DeviceToken: "synthetic-fcm-token-1"})
	if err != nil || !res.Enabled {
		t.Fatalf("configured FCM: %+v %v", res, err)
	}
	if err := e2.store.RemoveClientPush(context.Background(), "cli_synth-phone"); err != nil {
		t.Fatal(err)
	}
	if ts, _ := e2.store.Targets(context.Background(), ProviderFCM); len(ts) != 0 {
		t.Fatalf("revoked client still registered: %+v", ts)
	}
}

func TestListPaginationAndFilters(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < 7; i++ {
		e.clock.Advance(time.Second)
		e.ownerMessage(contracts.NewID(contracts.PrefixMessage), "thr_synth-"+string(rune('a'+i)), "ses_synth-a")
		e.drain()
	}
	e.approval("apr_synth-1", contracts.EvApprovalRequested, nil)
	e.drain()
	seen := map[string]bool{}
	cursor := ""
	pages := 0
	for {
		p, err := e.store.ListAttention(ownerCtx(), contracts.ListAttentionRequest{PageRequest: contracts.PageRequest{Limit: 3, Cursor: cursor}})
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, it := range p.Items {
			if seen[it.AttentionID] {
				t.Fatalf("duplicate across pages: %s", it.AttentionID)
			}
			seen[it.AttentionID] = true
		}
		if p.NextCursor == "" {
			break
		}
		cursor = p.NextCursor
	}
	if len(seen) != 8 || pages != 3 {
		t.Fatalf("paged %d items in %d pages", len(seen), pages)
	}
	p, _ := e.store.ListAttention(ownerCtx(), contracts.ListAttentionRequest{Kinds: []contracts.AttentionKind{contracts.AttnApproval}})
	if len(p.Items) != 1 || p.Items[0].Kind != contracts.AttnApproval {
		t.Fatalf("kind filter: %+v", p.Items)
	}
	p, _ = e.store.ListAttention(ownerCtx(), contracts.ListAttentionRequest{SessionID: "ses_synth-zzz"})
	if len(p.Items) != 0 {
		t.Fatalf("session filter: %+v", p.Items)
	}
	empty := ""
	p, _ = e.store.ListAttention(ownerCtx(), contracts.ListAttentionRequest{ProjectID: &empty})
	if len(p.Items) != 8 {
		t.Fatalf("standalone filter: %d", len(p.Items))
	}
	if _, err := e.store.ListAttention(ownerCtx(), contracts.ListAttentionRequest{PageRequest: contracts.PageRequest{Cursor: "!!"}}); !contracts.IsCode(err, contracts.ErrInvalid) {
		t.Fatalf("bad cursor: %v", err)
	}
	n, _ := e.store.CountOpen(context.Background(), "ses_synth-a")
	if n != 8 {
		t.Fatalf("CountOpen = %d", n)
	}
}

func TestAttentionChangedEmittedInTx(t *testing.T) {
	e := newEnv(t)
	e.approval("apr_synth-1", contracts.EvApprovalRequested, nil)
	e.drain()
	evs := e.log.Events(contracts.EvAttentionChanged)
	if len(evs) != 1 || evs[0].Object.Kind != contracts.KindAttention || evs[0].SessionID != "ses_synth-a" {
		t.Fatalf("attention.changed: %+v", evs)
	}
}

// The deriver consumes message.ack and account.grant from the stream.
func TestDeriverConsumesAckAndGrant(t *testing.T) {
	e := newEnv(t)
	ev := e.ownerMessage("msg_synth-ack", "", "ses_synth-a")
	e.drain()
	group := "message:from:session:ses_synth-a"
	if got := e.byGroup(group); len(got) != 1 || got[0].State != contracts.AttnOpen {
		t.Fatalf("message item: %+v", got)
	}
	var v contracts.MessageView
	_ = json.Unmarshal(ev.Payload, &v)
	v.Status.Ack = contracts.AckAcknowledged
	e.log.AppendPayload(contracts.EvMessageAck, contracts.ObjectRef{Kind: contracts.KindMessage, ID: "msg_synth-ack"}, "", nil, v)
	e.drain()
	if got := e.byGroup(group); len(got) != 1 || got[0].State != contracts.AttnHandled {
		t.Fatalf("after owner ack: %+v", got)
	}

	g := contracts.DeviceGrant{GrantID: "grt_synth-1", AccountID: "acct_synth-1", NodeID: "node_synth-mac", State: contracts.GrantDead}
	ref := contracts.ObjectRef{Kind: contracts.KindGrant, ID: g.GrantID}
	e.log.AppendPayload(contracts.EvGrantChanged, ref, "", nil, g)
	e.drain()
	if got := e.byGroup("grant:grt_synth-1"); len(got) != 1 || got[0].State != contracts.AttnOpen || got[0].Kind != contracts.AttnNode {
		t.Fatalf("dead grant item: %+v", got)
	}
	g.State = contracts.GrantValid
	e.log.AppendPayload(contracts.EvGrantChanged, ref, "", nil, g)
	e.drain()
	if got := e.byGroup("grant:grt_synth-1"); len(got) != 1 || got[0].State != contracts.AttnWithdrawn {
		t.Fatalf("after valid grant: %+v", got)
	}
}

func TestGetAttention(t *testing.T) {
	e := newEnv(t)
	e.approval("apr_synth-get", contracts.EvApprovalRequested, nil)
	e.drain()
	id := e.list()[0].AttentionID
	it, err := e.store.GetAttention(ownerCtx(), contracts.GetRequest{ID: id})
	if err != nil || it.AttentionID != id || it.State != contracts.AttnOpen {
		t.Fatalf("get: %+v %v", it, err)
	}
	// Withdrawn items stay readable (deep links from stale notifications).
	e.approval("apr_synth-get", contracts.EvApprovalWithdrawn, nil)
	e.drain()
	if it, err = e.store.GetAttention(ownerCtx(), contracts.GetRequest{ID: id}); err != nil || it.State != contracts.AttnWithdrawn {
		t.Fatalf("withdrawn: %+v %v", it, err)
	}
	if _, err := e.store.GetAttention(ownerCtx(), contracts.GetRequest{ID: "att_synth-missing"}); !contracts.IsCode(err, contracts.ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	sess := contracts.WithPrincipal(context.Background(), contracts.Principal{Kind: contracts.PrincipalSession, ID: "ses_synth-a"})
	if _, err := e.store.GetAttention(sess, contracts.GetRequest{ID: id}); !contracts.IsCode(err, contracts.ErrForbidden) {
		t.Fatalf("session: %v", err)
	}
}
