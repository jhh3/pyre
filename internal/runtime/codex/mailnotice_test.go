package codex

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/messaging/delivery"
	"github.com/jhh3/pyre/internal/messaging/inbox"
	"github.com/jhh3/pyre/internal/search"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

var _ delivery.MailNotifier = (*Adapter)(nil)

// mailNotices are the run's in-turn mail notices in transcript order.
func mailNotices(t *testing.T, a *Adapter, h contracts.RunHandle) []contracts.TranscriptEntry {
	t.Helper()
	resp, err := a.ReadTranscript(context.Background(), h, contracts.ReadSessionRequest{Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	var out []contracts.TranscriptEntry
	for _, en := range resp.Entries {
		if strings.HasPrefix(en.Text, "[pyre] ") && strings.Contains(en.Text, " for this session (oldest ") {
			if en.Kind != "system" {
				t.Fatalf("mail notice read as %s, not system: %+v", en.Kind, en)
			}
			out = append(out, en)
		}
	}
	return out
}

// sends counts the turn/start and turn/steer requests whose client id has
// prefix (the fake logs every one, refused or not).
func sends(t *testing.T, home, prefix string) int {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(home, "fake-turnstart.log"))
	n := 0
	for _, l := range strings.Split(string(b), "\n") {
		if l != "" && strings.HasPrefix(l, prefix) {
			n++
		}
	}
	return n
}

func running(o contracts.Observation) bool { return o.State == contracts.RunRunning }

// engineRuns serves the codex run to a real delivery engine; the run state
// is the adapter's live observation.
type engineRuns struct {
	a *Adapter
	h contracts.RunHandle
}

func (r engineRuns) target() delivery.Target {
	obs, _ := r.a.Observe(context.Background(), r.h, "")
	caps := contracts.WithoutCaps(r.a.Capabilities(), r.h.Data[contracts.HandleCapsUnavailable])
	return delivery.Target{Run: contracts.Run{RunID: r.h.RunID, SessionID: r.h.SessionID, Adapter: AdapterName, State: obs.State, Capabilities: caps},
		Handle: r.h, Adapter: r.a}
}

func (r engineRuns) Current(ctx context.Context, sessionID string) (delivery.Target, bool) {
	return r.target(), sessionID == r.h.SessionID
}

func (r engineRuns) Get(ctx context.Context, runID string) (delivery.Target, bool) {
	return r.target(), runID == r.h.RunID
}

func putMail(t *testing.T, ib *inbox.Store, id, session string, received time.Time) {
	t.Helper()
	d := contracts.DeliverCommand{SessionID: session, Message: contracts.MessageEnvelope{
		SchemaVersion: 1, MessageID: id, OperationID: "op_" + id,
		Sender:    contracts.Principal{Kind: contracts.PrincipalSession, ID: "ses_synthetic_peer", Name: "integrator"},
		Recipient: contracts.Address{Kind: contracts.AddrSession, ID: session},
		Mode:      contracts.ModeQueue, Body: contracts.MessageBody{Text: "synthetic body of " + id},
	}}
	if _, _, err := ib.Put(context.Background(), d, received); err != nil {
		t.Fatal(err)
	}
}

// The node's delivery engine and the Codex adapter end to end, on the fake
// app-server: queued mail older than the threshold gets exactly one notice
// steered into the long turn (count and age, no body); young mail gets
// none; mail the agent read through the inbox before the turn ended is not
// pasted; the rest is delivered at idle as before.
func TestCodexMailNoticeEndToEnd(t *testing.T) {
	t.Parallel()
	e := newEnv(t, func(o *Options) { o.MailNotice = true })
	h := e.launch(e.a, "run_mn1", e.home, nil, runEnv("run_mn1"))
	e.wait(e.a, h, 5*time.Second, idle)
	if !contracts.HasCap(e.a.Capabilities(), contracts.CapInputDoorbell) || h.Data[contracts.HandleCapsUnavailable] != "" {
		t.Fatalf("input.doorbell not available: caps %v data %v", e.a.Capabilities(), h.Data)
	}
	db, err := sqlitex.Open(filepath.Join(t.TempDir(), "node.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ib, err := inbox.Open(context.Background(), db, inbox.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	eng := delivery.New(delivery.Config{NodeID: "node_x", Inbox: ib, Runs: engineRuns{a: e.a, h: h}, MinBackoff: 10 * time.Millisecond, MaxBackoff: 50 * time.Millisecond})
	ctx := context.Background()
	pass := func(n int) {
		for range n {
			eng.ProcessSession(ctx, "ses_x", true)
		}
	}
	state := func(id string) inbox.Message {
		m, err := ib.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}

	if r := dlv(t, e.a, h, "msg_long", "work a while FAKE_SLEEP_MS=5000", contracts.ModeQueue); r.State != contracts.DeliveryAccepted {
		t.Fatalf("%+v", r)
	}
	e.wait(e.a, h, 5*time.Second, running)

	// Young mail: queued, no notice.
	putMail(t, ib, "msg_young", "ses_x", time.Now())
	pass(3)
	if n := mailNotices(t, e.a, h); len(n) != 0 {
		t.Fatalf("notice for young mail: %+v", n)
	}
	if m := state("msg_young"); m.State != contracts.DeliveryQueued {
		t.Fatalf("young mail: %+v", m)
	}

	// Mail that waited two minutes: exactly one notice, counting it alone.
	putMail(t, ib, "msg_old", "ses_x", time.Now().Add(-2*time.Minute))
	pass(5)
	got := mailNotices(t, e.a, h)
	if len(got) != 1 || sends(t, e.home, mailNoticeIDPrefix) != 1 {
		t.Fatalf("want one notice, got %d (sent %d): %+v", len(got), sends(t, e.home, mailNoticeIDPrefix), got)
	}
	txt := got[0].Text
	if !strings.HasPrefix(txt, "[pyre] 1 new message for this session (oldest 2m") || !strings.Contains(txt, "mcp__pyre__inbox (unacked: true)") {
		t.Fatalf("notice text: %q", txt)
	}
	if strings.Contains(txt, "synthetic body") || strings.Contains(txt, "integrator") || strings.Contains(txt, "msg_old") {
		t.Fatalf("notice leaks the mail: %q", txt)
	}
	if rec := loadMailNotice(h.StateDir); rec.Outcome != "steered" || len(rec.IDs) != 1 || rec.IDs[0] != "msg_old" {
		t.Fatalf("notice record: %+v", rec)
	}
	for _, id := range []string{"msg_old", "msg_young"} {
		if m := state(id); m.State != contracts.DeliveryQueued || m.Attempted {
			t.Fatalf("%s after the notice: %+v", id, m)
		}
	}
	if obs, _ := e.a.Observe(ctx, h, ""); obs.State != contracts.RunRunning || obs.Doing == "" {
		t.Fatalf("the notice changed the turn: %+v", obs)
	}

	// The agent reads msg_old through the inbox before its turn ends.
	if m, err := eng.ApplyRead(ctx, "msg_old", "", contracts.EvidencePyreRead, ""); err != nil || m.State != contracts.DeliveryAccepted {
		t.Fatalf("inbox read: %+v %v", m, err)
	}

	// Idle: msg_young is delivered as before; msg_old is never pasted.
	e.wait(e.a, h, 10*time.Second, idle)
	pass(2)
	if m := state("msg_young"); m.State != contracts.DeliveryAccepted || m.BoundRunID != "run_mn1" {
		t.Fatalf("idle delivery: %+v", m)
	}
	if n := sends(t, e.home, "msg_old"); n != 0 {
		t.Fatalf("read mail pasted %d time(s)", n)
	}
	if n := sends(t, e.home, "msg_young"); n != 1 {
		t.Fatalf("msg_young sent %d times", n)
	}
	if n := sends(t, e.home, mailNoticeIDPrefix); n != 1 {
		t.Fatalf("notices sent: %d", n)
	}
}

// The adapter's bounds: one notice per turn; a new batch in the same turn
// only after mailNoticeEvery; a refused steer (turn mismatch) is dropped
// and never retried; none with an approval pending; none in a run without
// the pyre MCP server.
func TestCodexMailNoticeBounds(t *testing.T) {
	t.Parallel()
	e := newEnv(t, func(o *Options) { o.MailNotice = true })
	h := e.launch(e.a, "run_mn2", e.home, nil, runEnv("run_mn2"))
	e.wait(e.a, h, 5*time.Second, idle)
	ctx := context.Background()
	old := time.Now().Add(-90 * time.Second)
	mail := func(ids ...string) []contracts.QueuedMail {
		var out []contracts.QueuedMail
		for _, id := range ids {
			out = append(out, contracts.QueuedMail{MessageID: id, ReceivedAt: old})
		}
		return out
	}
	notify := func(ids ...string) { e.a.NotifyQueuedMail(ctx, h, mail(ids...)) }
	e.a.mu.Lock()
	ts := e.a.runs["run_mn2"].ts
	e.a.mu.Unlock()
	age := func(d time.Duration) {
		ts.mu.Lock()
		ts.notice.At = ts.notice.At.Add(-d)
		ts.mu.Unlock()
	}

	// Idle thread: nothing (the mail is pasted at idle).
	notify("msg_1")
	if n := sends(t, e.home, mailNoticeIDPrefix); n != 0 {
		t.Fatalf("notice to an idle thread: %d", n)
	}

	dlv(t, e.a, h, "msg_long", "FAKE_SLEEP_MS=30000", contracts.ModeQueue)
	e.wait(e.a, h, 5*time.Second, running)
	notify("msg_1")
	notify("msg_1")
	notify("msg_1", "msg_2") // a new batch, too soon
	if n := sends(t, e.home, mailNoticeIDPrefix); n != 1 {
		t.Fatalf("notices in one turn: %d", n)
	}
	age(mailNoticeEvery)
	notify("msg_1", "msg_2")
	got := mailNotices(t, e.a, h)
	if len(got) != 2 || !strings.HasPrefix(got[1].Text, "[pyre] 2 new messages for this session (oldest 1m3") {
		t.Fatalf("second batch: %+v", got)
	}
	age(mailNoticeEvery)
	notify("msg_1", "msg_2") // nothing new
	if n := sends(t, e.home, mailNoticeIDPrefix); n != 2 {
		t.Fatalf("notice without new mail: %d", n)
	}

	// Turn mismatch: Codex refuses the steer; it is dropped, not retried.
	ts.mu.Lock()
	real := ts.activeTurn
	ts.activeTurn = "turn-synthetic-ended"
	ts.mu.Unlock()
	notify("msg_3")
	if n := sends(t, e.home, mailNoticeIDPrefix); n != 3 {
		t.Fatalf("mismatch attempt: %d", n)
	}
	if rec := loadMailNotice(h.StateDir); !strings.HasPrefix(rec.Outcome, "refused: ") {
		t.Fatalf("refused steer recorded as %+v", rec)
	}
	ts.mu.Lock()
	ts.activeTurn = real
	ts.mu.Unlock()
	age(mailNoticeEvery)
	notify("msg_3")
	if n := sends(t, e.home, mailNoticeIDPrefix); n != 3 {
		t.Fatalf("refused notice retried: %d", n)
	}
	if n := len(mailNotices(t, e.a, h)); n != 2 {
		t.Fatalf("notices in the transcript: %d", n)
	}
	// A pyred restart keeps the record: the same mail is not announced again.
	a2 := e.restart()
	if _, err := a2.Recover(ctx, h); err != nil {
		t.Fatal(err)
	}
	a2.o.MailNotice = true
	a2.NotifyQueuedMail(ctx, h, mail("msg_1", "msg_2", "msg_3"))
	if n := sends(t, e.home, mailNoticeIDPrefix); n != 3 {
		t.Fatalf("notice repeated after a restart: %d", n)
	}
	if rc, err := e.a.Interrupt(ctx, h); err != nil || rc.Outcome != contracts.OutcomeSucceeded {
		t.Fatalf("interrupt %+v %v", rc, err)
	}
	e.wait(e.a, h, 5*time.Second, idle)

	// Approval pending: no notice.
	dlv(t, e.a, h, "msg_appr", "FAKE_APPROVAL", contracts.ModeQueue)
	e.wait(e.a, h, 5*time.Second, func(o contracts.Observation) bool { return o.State == contracts.RunWaitingApproval })
	notify("msg_4")
	if n := sends(t, e.home, mailNoticeIDPrefix); n != 3 {
		t.Fatalf("notice with an approval pending: %d", n)
	}
	_, _ = e.a.Interrupt(ctx, h)

	// A run without the pyre MCP server has no inbox tool: no doorbell.
	h3 := e.launch(e.a, "run_mn3", e.home, nil, nil)
	if !strings.Contains(h3.Data[contracts.HandleCapsUnavailable], contracts.CapInputDoorbell) {
		t.Fatalf("doorbell not withdrawn without MCP: %v", h3.Data)
	}
}

func TestMailNoticeText(t *testing.T) {
	now := time.Unix(1791100000, 0)
	one := mailNoticeText([]contracts.QueuedMail{{MessageID: "m", ReceivedAt: now.Add(-75 * time.Second)}}, now)
	if one != "[pyre] 1 new message for this session (oldest 1m15s). Read it with mcp__pyre__inbox (unacked: true) when you reach a stopping point; unread mail is delivered when this turn ends." {
		t.Fatalf("%q", one)
	}
	if !isMailNotice(one) || isMailNotice("[pyre] background command finished: `x` (exit 0). No output.") || isMailNotice("hello") {
		t.Fatal("isMailNotice")
	}
	if e := rolloutEntry(search.Entry{Kind: "user", Text: one}); e.Kind != "system" {
		t.Fatalf("rollout kind %s", e.Kind)
	}
}
