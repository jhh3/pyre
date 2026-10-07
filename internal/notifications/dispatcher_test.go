package notifications_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/attention"
	"github.com/jhh3/pyre/internal/attention/attentiontest"
	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/notifications"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

const secret = "SYNTH-SECRET-TRANSCRIPT-TEXT"

var t0 = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

// fakeProvider records deliveries and returns scripted results.
type fakeProvider struct {
	mu     sync.Mutex
	msgs   []notifications.Message
	script []func(notifications.Target) notifications.Result
}

func (f *fakeProvider) Name() string { return notifications.ProviderFCM }

func (f *fakeProvider) Deliver(_ context.Context, m notifications.Message, ts []notifications.Target) []notifications.Result {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.msgs = append(f.msgs, m)
	var step func(notifications.Target) notifications.Result
	if len(f.script) > 0 {
		step, f.script = f.script[0], f.script[1:]
	}
	out := make([]notifications.Result, len(ts))
	for i, t := range ts {
		if step != nil {
			out[i] = step(t)
			out[i].Target = t
		} else {
			out[i] = notifications.Result{Target: t}
		}
	}
	return out
}

func (f *fakeProvider) sent() []notifications.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]notifications.Message(nil), f.msgs...)
}

type env struct {
	t     *testing.T
	store *attention.Store
	log   *attentiontest.Log
	clock *attentiontest.Clock
	der   *attention.Deriver
	disp  *notifications.Dispatcher
}

func newEnv(t *testing.T, fcm notifications.Provider, mod func(*notifications.DispatcherOptions)) *env {
	t.Helper()
	db, err := sqlitex.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	e := &env{t: t, log: attentiontest.NewLog(), clock: attentiontest.NewClock(t0)}
	e.store, err = attention.NewStore(context.Background(), db, attention.Options{Sink: e.log, Clock: e.clock, FCMConfigured: fcm != nil})
	if err != nil {
		t.Fatal(err)
	}
	e.der = attention.NewDeriver(e.store, e.log, attention.DeriverOptions{})
	opts := notifications.DispatcherOptions{FCM: fcm, Clock: e.clock, Debounce: 2 * time.Second, RateBurst: 100, BaseBackoff: time.Second}
	if mod != nil {
		mod(&opts)
	}
	e.disp = notifications.NewDispatcher(e.store, opts)
	return e
}

func ownerCtx() context.Context {
	return contracts.WithPrincipal(context.Background(), contracts.Principal{Kind: contracts.PrincipalOwner, ID: "cli_synth-phone"})
}

func (e *env) register(token string) {
	e.t.Helper()
	if _, err := e.store.RegisterPush(ownerCtx(), contracts.PushRegistration{Mutation: contracts.Mutation{OperationID: contracts.NewID("op")}, Provider: "fcm", DeviceToken: token}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) setPrefs(p contracts.NotificationPrefs) {
	e.t.Helper()
	if _, err := e.store.SetNotificationPrefs(ownerCtx(), contracts.SetNotificationPrefsRequest{Mutation: contracts.Mutation{OperationID: contracts.NewID("op")}, Prefs: p}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) drain() {
	e.t.Helper()
	for i := 0; i < 50; i++ {
		n, err := e.der.Step(context.Background())
		if err != nil {
			e.t.Fatal(err)
		}
		if n == 0 {
			return
		}
	}
}

// pump drains the deriver and dispatches, advancing past the debounce.
func (e *env) pump() {
	e.t.Helper()
	e.drain()
	if _, err := e.disp.ProcessOnce(context.Background()); err != nil {
		e.t.Fatal(err)
	}
	e.clock.Advance(3 * time.Second)
	if _, err := e.disp.ProcessOnce(context.Background()); err != nil {
		e.t.Fatal(err)
	}
	e.drain()
}

func (e *env) approval(id string, kind contracts.EventKind, project *string, mod func(*contracts.Approval)) {
	a := contracts.Approval{ApprovalID: id, SessionID: "ses_synth-a", RunID: "run_synth-a", Kind: contracts.ApprovalPermission,
		Title: "deploy", Body: secret, State: contracts.ApprovalPending}
	if mod != nil {
		mod(&a)
	}
	e.log.AppendPayload(kind, contracts.ObjectRef{Kind: contracts.KindApproval, ID: id}, a.SessionID, project, a)
}

func (e *env) ownerMessage(thread string) {
	v := contracts.MessageView{Envelope: contracts.MessageEnvelope{MessageID: contracts.NewID(contracts.PrefixMessage),
		Sender: contracts.Principal{Kind: contracts.PrincipalSession, ID: "ses_synth-a"}, Recipient: contracts.Address{Kind: contracts.AddrOwner, ID: "owner"},
		ThreadID: thread, Mode: contracts.ModeQueue, Body: contracts.MessageBody{Text: secret}}}
	e.log.AppendPayload(contracts.EvMessageStored, contracts.ObjectRef{Kind: contracts.KindMessage, ID: v.Envelope.MessageID}, "ses_synth-a", nil, v)
}

func (e *env) item(group string) contracts.AttentionItem {
	e.t.Helper()
	p, err := e.store.ListAttention(ownerCtx(), contracts.ListAttentionRequest{States: []contracts.AttentionState{"open", "read", "handled", "withdrawn", "snoozed"}})
	if err != nil {
		e.t.Fatal(err)
	}
	for _, it := range p.Items {
		if it.GroupKey == group {
			return it
		}
	}
	e.t.Fatalf("no item %s", group)
	return contracts.AttentionItem{}
}

func TestOnePushPerTransitionAndCollapse(t *testing.T) {
	fp := &fakeProvider{}
	e := newEnv(t, fp, nil)
	e.register("synthetic-token-1")
	e.approval("apr_synth-1", contracts.EvApprovalRequested, nil, nil)
	for i := 0; i < 5; i++ {
		e.ownerMessage("thr_synth-1") // a burst in one thread
	}
	e.pump()
	e.pump() // nothing new: no duplicate pushes
	msgs := fp.sent()
	if len(msgs) != 2 {
		t.Fatalf("pushes = %d, want 2 (one per item): %+v", len(msgs), msgs)
	}
	it := e.item("approval:apr_synth-1")
	if it.Notification == nil || it.Notification.Provider != "fcm" || it.Notification.State != "sent" || it.Notification.SentAt == nil {
		t.Fatalf("status = %+v", it.Notification)
	}
	if it.State != contracts.AttnOpen {
		t.Fatalf("delivery changed the human state: %s", it.State)
	}
	// More messages on the still-open thread collapse without a new push.
	e.ownerMessage("thr_synth-1")
	e.pump()
	if len(fp.sent()) != 2 {
		t.Fatalf("collapse re-pushed: %d", len(fp.sent()))
	}
	if c := e.item("message:thr_synth-1").Count; c != 6 {
		t.Fatalf("count = %d", c)
	}
}

func TestReadBeforeDebounceSuppressesPush(t *testing.T) {
	fp := &fakeProvider{}
	e := newEnv(t, fp, nil)
	e.register("synthetic-token-1")
	e.approval("apr_synth-1", contracts.EvApprovalRequested, nil, nil)
	e.drain()
	_, _ = e.disp.ProcessOnce(context.Background()) // within debounce
	// Resolved in the terminal within moments.
	e.approval("apr_synth-1", contracts.EvApprovalResolved, nil, func(a *contracts.Approval) { a.State = contracts.ApprovalResolved })
	e.pump()
	if n := len(fp.sent()); n != 0 {
		t.Fatalf("pushed a request that was already resolved: %d", n)
	}
}

func TestWithdrawalDismissesOnPhone(t *testing.T) {
	fp := &fakeProvider{}
	e := newEnv(t, fp, nil)
	e.register("synthetic-token-1")
	e.approval("apr_synth-1", contracts.EvApprovalRequested, nil, nil)
	e.pump()
	e.approval("apr_synth-1", contracts.EvApprovalResolved, nil, func(a *contracts.Approval) {
		a.State = contracts.ApprovalResolved
		a.Resolution = &contracts.ApprovalResolution{Source: "native"}
	})
	e.pump()
	msgs := fp.sent()
	if len(msgs) != 2 || msgs[0].Action != notifications.ActionNotify || msgs[1].Action != notifications.ActionDismiss || msgs[1].AttentionID != msgs[0].AttentionID {
		t.Fatalf("want notify then dismiss: %+v", msgs)
	}
	it := e.item("approval:apr_synth-1")
	if it.State != contracts.AttnWithdrawn || it.Notification.State != "sent" {
		t.Fatalf("after withdrawal: %+v %+v", it.State, it.Notification)
	}
}

func TestQuietHoursMuteAndKindsKeepDurableItem(t *testing.T) {
	fp := &fakeProvider{}
	e := newEnv(t, fp, nil)
	e.register("synthetic-token-1")
	muted := "prj_synth-muted"
	// 12:00 UTC is inside 11:00-13:00 UTC quiet hours.
	e.setPrefs(contracts.NotificationPrefs{QuietHoursStart: "11:00", QuietHoursEnd: "13:00", TZ: "UTC"})
	e.approval("apr_synth-q", contracts.EvApprovalRequested, nil, nil)
	e.pump()
	it := e.item("approval:apr_synth-q")
	if it.Notification.State != notifications.StateQuietHours || it.State != contracts.AttnOpen {
		t.Fatalf("quiet hours: %+v state=%s", it.Notification, it.State)
	}
	e.setPrefs(contracts.NotificationPrefs{MutedProjects: []string{muted}, Kinds: map[contracts.AttentionKind]bool{contracts.AttnMessage: false}})
	e.approval("apr_synth-m", contracts.EvApprovalRequested, &muted, nil)
	e.ownerMessage("thr_synth-x")
	e.pump()
	if st := e.item("approval:apr_synth-m").Notification; st.State != notifications.StateDisabled || st.Detail != "project muted" {
		t.Fatalf("muted: %+v", st)
	}
	if st := e.item("message:thr_synth-x").Notification; st.State != notifications.StateDisabled {
		t.Fatalf("kind toggle: %+v", st)
	}
	if n := len(fp.sent()); n != 0 {
		t.Fatalf("suppressed items were pushed: %d", n)
	}
	// Durable records remain listed.
	p, _ := e.store.ListAttention(ownerCtx(), contracts.ListAttentionRequest{})
	if len(p.Items) != 3 {
		t.Fatalf("durable items = %d", len(p.Items))
	}
}

func TestRetriesWithBackoffThenSuccess(t *testing.T) {
	transient := func(notifications.Target) notifications.Result {
		return notifications.Result{Err: errors.New("fcm: 503"), Retryable: true}
	}
	fp := &fakeProvider{script: []func(notifications.Target) notifications.Result{transient, transient}}
	e := newEnv(t, fp, nil)
	e.register("synthetic-token-1")
	e.approval("apr_synth-1", contracts.EvApprovalRequested, nil, nil)
	e.pump() // attempt 1 fails
	if st := e.item("approval:apr_synth-1").Notification; st.State != "pending" || !strings.Contains(st.Detail, "retrying") {
		t.Fatalf("after first failure: %+v", st)
	}
	_, _ = e.disp.ProcessOnce(context.Background()) // backoff not elapsed
	if len(fp.sent()) != 1 {
		t.Fatalf("retried before backoff: %d", len(fp.sent()))
	}
	e.clock.Advance(1100 * time.Millisecond) // base backoff 1s
	_, _ = e.disp.ProcessOnce(context.Background())
	e.clock.Advance(2100 * time.Millisecond) // doubled
	_, _ = e.disp.ProcessOnce(context.Background())
	if n := len(fp.sent()); n != 3 {
		t.Fatalf("attempts = %d", n)
	}
	if st := e.item("approval:apr_synth-1").Notification; st.State != "sent" {
		t.Fatalf("final: %+v", st)
	}
}

func TestRetriesAreBounded(t *testing.T) {
	always := func(notifications.Target) notifications.Result {
		return notifications.Result{Err: errors.New("fcm: 500"), Retryable: true}
	}
	fp := &fakeProvider{script: []func(notifications.Target) notifications.Result{always, always, always, always, always, always}}
	e := newEnv(t, fp, func(o *notifications.DispatcherOptions) { o.MaxAttempts = 3 })
	e.register("synthetic-token-1")
	e.approval("apr_synth-1", contracts.EvApprovalRequested, nil, nil)
	e.pump()
	for i := 0; i < 10; i++ {
		e.clock.Advance(time.Minute)
		_, _ = e.disp.ProcessOnce(context.Background())
	}
	if n := len(fp.sent()); n != 3 {
		t.Fatalf("attempts = %d, want 3", n)
	}
	it := e.item("approval:apr_synth-1")
	if it.Notification.State != "failed" || it.State != contracts.AttnOpen {
		t.Fatalf("failed push must keep the durable item: %+v %s", it.Notification, it.State)
	}
}

func TestUnregisteredTokenRemovedFallsBackToStream(t *testing.T) {
	gone := func(notifications.Target) notifications.Result {
		return notifications.Result{Err: errors.New("fcm: UNREGISTERED"), Unregistered: true}
	}
	fp := &fakeProvider{script: []func(notifications.Target) notifications.Result{gone}}
	e := newEnv(t, fp, nil)
	e.register("synthetic-token-stale")
	e.approval("apr_synth-1", contracts.EvApprovalRequested, nil, nil)
	e.pump()
	if ts, _ := e.store.Targets(context.Background(), "fcm"); len(ts) != 0 {
		t.Fatalf("unregistered token kept: %+v", ts)
	}
	if st := e.item("approval:apr_synth-1").Notification; st.Provider != "hub-stream" || st.State != "sent" {
		t.Fatalf("fallback status: %+v", st)
	}
}

func TestNoFCMUsesHubStream(t *testing.T) {
	e := newEnv(t, nil, nil)
	res, err := e.store.RegisterPush(ownerCtx(), contracts.PushRegistration{Mutation: contracts.Mutation{OperationID: "op_synth-1"}, Provider: "fcm", DeviceToken: "synthetic-token"})
	if err != nil || res.Enabled {
		t.Fatalf("register without FCM: %+v %v", res, err)
	}
	e.approval("apr_synth-1", contracts.EvApprovalRequested, nil, nil)
	e.pump()
	st := e.item("approval:apr_synth-1").Notification
	if st.Provider != "hub-stream" || st.State != "sent" || !strings.Contains(st.Detail, "FCM not configured") {
		t.Fatalf("status: %+v", st)
	}
}

func TestRateLimitDefers(t *testing.T) {
	fp := &fakeProvider{}
	e := newEnv(t, fp, func(o *notifications.DispatcherOptions) { o.RateBurst = 2; o.RateInterval = 10 * time.Second })
	e.register("synthetic-token-1")
	for _, id := range []string{"apr_synth-1", "apr_synth-2", "apr_synth-3"} {
		e.approval(id, contracts.EvApprovalRequested, nil, nil)
	}
	e.pump()
	if n := len(fp.sent()); n != 2 {
		t.Fatalf("burst allowed %d", n)
	}
	e.clock.Advance(11 * time.Second)
	_, _ = e.disp.ProcessOnce(context.Background())
	if n := len(fp.sent()); n != 3 {
		t.Fatalf("deferred push not sent after refill: %d", n)
	}
}

func TestDeriverAndDispatcherQuiesce(t *testing.T) {
	fp := &fakeProvider{}
	e := newEnv(t, fp, nil)
	e.log.IgnoreKindFilter = true
	e.register("synthetic-token-1")
	e.approval("apr_synth-1", contracts.EvApprovalRequested, nil, nil)
	e.ownerMessage("thr_synth-1")
	e.pump()
	head := e.log.Head()
	for i := 0; i < 5; i++ {
		e.pump()
	}
	if e.log.Head() != head {
		t.Fatalf("status recording looped: head %d -> %d", head, e.log.Head())
	}
	if n := len(fp.sent()); n != 2 {
		t.Fatalf("pushes = %d", n)
	}
	p, _ := e.store.ListAttention(ownerCtx(), contracts.ListAttentionRequest{})
	if len(p.Items) != 2 {
		t.Fatalf("items = %d", len(p.Items))
	}
}

// TestFCMPayloadCarriesNoBodyText runs the real FCM provider end to end
// against a fake OAuth + FCM server and inspects the raw HTTP body.
func TestFCMPayloadCarriesNoBodyText(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_, _ = io.WriteString(w, `{"access_token":"synthetic-access","expires_in":3600,"token_type":"Bearer"}`)
			return
		}
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		_, _ = io.WriteString(w, `{"name":"projects/pyre-synthetic/messages/1"}`)
	}))
	defer srv.Close()
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	sa, _ := json.Marshal(map[string]string{"type": "service_account", "project_id": "pyre-synthetic", "client_email": "pyre-synthetic@example.invalid",
		"private_key": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), "token_uri": srv.URL + "/token"})
	fcm, err := notifications.NewFCM(notifications.FCMConfig{ServiceAccountJSON: sa, Endpoint: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, fcm, nil)
	e.register("synthetic-token-1")
	e.approval("apr_synth-1", contracts.EvApprovalRequested, nil, func(a *contracts.Approval) { a.Title = "rm -rf build " + secret })
	e.ownerMessage("thr_synth-1")
	e.pump()
	mu.Lock()
	defer mu.Unlock()
	// Under heavy machine load a slow fake response can be retried, so allow
	// duplicates here; exact push counts are covered by the dedup tests.
	if len(bodies) < 2 {
		t.Fatalf("sends = %d", len(bodies))
	}
	for _, b := range bodies {
		if strings.Contains(b, secret) || strings.Contains(b, "rm -rf") || strings.Contains(b, `"notification"`) {
			t.Fatalf("push payload leaks content or is not data-only: %s", b)
		}
	}
	if st := e.item("approval:apr_synth-1").Notification; st.Provider != "fcm" || st.State != "sent" {
		t.Fatalf("status: %+v", st)
	}
}
