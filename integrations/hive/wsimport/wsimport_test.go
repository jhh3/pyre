package wsimport

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

const fixture = "../testdata/ws"

var t0 = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

func loadFixture(t *testing.T) *Tree {
	t.Helper()
	tr, err := Load(fixture, nil, false, t0)
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

func fullAliases() AliasMap {
	return AliasMap{Version: 1, Mailboxes: map[string]Alias{
		"alpha-stream": {Kind: contracts.AddrSession, ID: "ses_synth-alpha"},
		"beta-stream":  {Kind: contracts.AddrSession, ID: "ses_synth-beta"},
	}}
}

func TestLoadInventory(t *testing.T) {
	tr := loadFixture(t)
	p := Build(tr, Options{Aliases: fullAliases()})
	r := p.Report
	want := Report{Streams: 2, Mailboxes: 3, Messages: 7, Unread: 2, Read: 5, PendingObligations: 1, ExpectReply: 3,
		WithReplyTo: 2, MissingReplyParent: 1, MissingID: 1, Channels: 1, ChannelPosts: 2, Subscriptions: 2,
		ChannelCursors: 2, UnreadChannelDeliveries: 1, Requests: 9, QueueRequests: 1, InfoRequests: 8, RequestAck: 2}
	got := Report{Streams: r.Streams, Mailboxes: r.Mailboxes, Messages: r.Messages, Unread: r.Unread, Read: r.Read,
		PendingObligations: r.PendingObligations, ExpectReply: r.ExpectReply, WithReplyTo: r.WithReplyTo,
		MissingReplyParent: r.MissingReplyParent, MissingID: r.MissingID, Channels: r.Channels, ChannelPosts: r.ChannelPosts,
		Subscriptions: r.Subscriptions, ChannelCursors: r.ChannelCursors, UnreadChannelDeliveries: r.UnreadChannelDeliveries,
		Requests: r.Requests, QueueRequests: r.QueueRequests, InfoRequests: r.InfoRequests, RequestAck: r.RequestAck}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("inventory\n got %+v\nwant %+v", got, want)
	}
	if !strings.HasPrefix(r.Digest, "sha256:") || r.Skipped != 0 || len(r.Unresolved) != 0 {
		t.Fatalf("digest/skips: %+v", r)
	}
	posts := tr.Channels["synth-chan"]
	if posts[0].Offset != 15 || posts[1].Offset != 116 {
		t.Fatalf("channel offsets must be code points: %d %d", posts[0].Offset, posts[1].Offset)
	}
	if tr.Streams["alpha-stream"].Title != "Synthetic: alpha stream" || tr.Streams["alpha-stream"].Cursors["synth-chan"] != 115 {
		t.Fatalf("stream frontmatter: %+v", tr.Streams["alpha-stream"])
	}
}

func itemByLegacy(p *Plan, id string) *Item {
	for i := range p.Items {
		if p.Items[i].LegacyID == id {
			return &p.Items[i]
		}
	}
	return nil
}

func TestPlanMapping(t *testing.T) {
	p := Build(loadFixture(t), Options{Aliases: fullAliases()})
	for _, it := range p.Items {
		r := it.Request
		if !contracts.ValidID(r.MessageID, contracts.PrefixMessage) || !strings.HasPrefix(r.MessageID, "msg_legacy-ws-") ||
			!contracts.ValidID(r.OperationID, contracts.PrefixOperation) {
			t.Fatalf("ids: %q %q", r.MessageID, r.OperationID)
		}
		if r.ThreadID != "" && !contracts.ValidID(r.ThreadID, contracts.PrefixThread) {
			t.Fatalf("thread id %q", r.ThreadID)
		}
	}
	parent, reply := itemByLegacy(p, "20260929-080000-103-0003"), itemByLegacy(p, "20260930-110000-104-0004")
	if reply.Request.InReplyTo != parent.ImportID || reply.Request.ThreadID == "" || reply.Request.ThreadID != parent.Request.ThreadID {
		t.Fatalf("reply relation: parent=%+v reply=%+v", parent.Request, reply.Request)
	}
	if reply.Request.Mode != contracts.ModeQueue || reply.Request.Recipient != (contracts.Address{Kind: contracts.AddrSession, ID: "ses_synth-beta"}) {
		t.Fatalf("unread to session must queue: %+v", reply.Request)
	}
	pi, ri := -1, -1
	for i, it := range p.Items {
		switch it.ImportID {
		case parent.ImportID:
			pi = i
		case reply.ImportID:
			ri = i
		}
	}
	if pi > ri {
		t.Fatal("reply ordered before its parent")
	}
	if parent.Obligation != "settled" || parent.Request.RequestAck || parent.Request.Mode != contracts.ModeInfo {
		t.Fatalf("settled obligation: %+v", parent)
	}
	open := itemByLegacy(p, "20260929-091500-102-0002")
	if open.Obligation != "open" || !open.Request.RequestAck || !strings.Contains(open.Request.Body.Text, "reply-obligation: open") {
		t.Fatalf("open obligation: %+v", open)
	}
	hq := itemByLegacy(p, "20260930-100000-101-aaaa")
	if hq.Request.Recipient.Kind != contracts.AddrOwner || hq.Obligation != "open" || hq.Request.Mode != contracts.ModeInfo {
		t.Fatalf("hq unread question: %+v", hq.Request)
	}
	uni := itemByLegacy(p, "20260928-120000-105-0005")
	body := uni.Request.Body.Text
	if !strings.HasSuffix(body, "---\nSynthetic Grüße — 你好 ✓\nSecond line: keep the original body verbatim.") {
		t.Fatalf("body not preserved: %q", body)
	}
	for _, want := range []string{"from: hq", "sent: 2026-09-28T12:00:00-07:00", "severity: high",
		"parent not found in the legacy archive", "not evidence that a model acted on it"} {
		if !strings.Contains(body, want) {
			t.Fatalf("header missing %q:\n%s", want, body)
		}
	}
	if uni.Request.InReplyTo != "" {
		t.Fatal("missing parent must not produce InReplyTo")
	}
	if noid := itemByLegacy(p, "file:legacy-noid"); noid == nil || !strings.Contains(noid.Request.Body.Text, "legacy-id: file:legacy-noid") {
		t.Fatalf("message without id: %+v", noid)
	}
	ch := itemByLegacy(p, "20260930-120000-107-0007")
	if ch.Request.Recipient != (contracts.Address{Kind: contracts.AddrChannel, ID: "synth-chan"}) || ch.Request.Mode != contracts.ModeInfo ||
		!strings.Contains(ch.Request.Body.Text, "legacy-pending-for: alpha-stream") {
		t.Fatalf("channel post: %+v", ch.Request)
	}
	if first := itemByLegacy(p, "20260929-100000-106-0006"); !strings.Contains(first.Request.Body.Text, "legacy-delivery: returned to beta-stream") {
		t.Fatalf("channel cursor observation: %s", first.Request.Body.Text)
	}
}

func TestPlanDeterministicAndUnresolved(t *testing.T) {
	a := Build(loadFixture(t), Options{Aliases: fullAliases()})
	b := Build(loadFixture(t), Options{Aliases: fullAliases()})
	ja, _ := json.Marshal(reqs(a))
	jb, _ := json.Marshal(reqs(b))
	if !bytes.Equal(ja, jb) {
		t.Fatal("plan is not deterministic")
	}
	// beta unmapped: skipped by default, archived to the owner on request.
	am := fullAliases()
	delete(am.Mailboxes, "beta-stream")
	skip := Build(loadFixture(t), Options{Aliases: am})
	if skip.Report.Skipped != 3 || skip.Report.Unresolved["beta-stream"] != 3 || skip.Report.Requests != 6 {
		t.Fatalf("skip: %+v", skip.Report)
	}
	own := Build(loadFixture(t), Options{Aliases: am, DefaultRecipient: "owner"})
	r := itemByLegacy(own, "20260930-110000-104-0004")
	if own.Report.Skipped != 0 || r.Request.Recipient.Kind != contracts.AddrOwner || r.Request.Mode != contracts.ModeInfo ||
		!strings.Contains(r.Request.Body.Text, "(no alias; archived to owner:owner)") {
		t.Fatalf("owner default: %+v", r.Request)
	}
	// Import ids do not depend on the alias map.
	if r.ImportID != itemByLegacy(a, "20260930-110000-104-0004").ImportID {
		t.Fatal("import id changed with alias map")
	}
}

func reqs(p *Plan) []contracts.SendMessageRequest {
	out := make([]contracts.SendMessageRequest, len(p.Items))
	for i, it := range p.Items {
		out[i] = it.Request
	}
	return out
}

func TestLiveRootRefused(t *testing.T) {
	abs, _ := filepath.Abs(fixture)
	live := LiveRoots(func(k string) string {
		if k == "WS_ROOT" {
			return abs
		}
		return ""
	})
	if _, err := Load(fixture, live, false, t0); err != ErrLiveRoot {
		t.Fatalf("want ErrLiveRoot, got %v", err)
	}
	if _, err := Load(fixture, live, true, t0); err != nil {
		t.Fatal(err)
	}
}

func TestCopyTreeAndWatermark(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "copy")
	n, err := CopyTree(fixture, dst)
	if err != nil || n != 12 { // 8 messages, 1 ws marker dotfile, 1 channel, 2 STREAM.md
		t.Fatalf("copy: n=%d err=%v", n, err)
	}
	if _, err := CopyTree(fixture, dst); err == nil {
		t.Fatal("copy into non-empty destination allowed")
	}
	orig, cp := loadFixture(t), mustLoad(t, dst)
	if orig.Digest != cp.Digest {
		t.Fatal("copy digest differs from source")
	}
	before := Build(cp, Options{Aliases: fullAliases()})
	// A message arrives after the snapshot: the watermark changes and a
	// re-plan adds exactly one new request; existing ids are unchanged.
	msg := "---\nid: 20261001-090000-200-beef\nfrom: hq\nto: alpha-stream\ncreated: 2026-10-01T09:00:00-07:00\n---\n\nSynthetic late message.\n"
	if err := os.WriteFile(filepath.Join(dst, "inbox", "alpha-stream", "20261001-090000-200-beef.md"), []byte(msg), 0o600); err != nil {
		t.Fatal(err)
	}
	after := Build(mustLoad(t, dst), Options{Aliases: fullAliases()})
	if after.Report.Digest == before.Report.Digest {
		t.Fatal("watermark did not change")
	}
	old := map[string]bool{}
	for _, it := range before.Items {
		old[it.ImportID] = true
	}
	var added []string
	for _, it := range after.Items {
		if !old[it.ImportID] {
			added = append(added, it.LegacyID)
		}
	}
	if !reflect.DeepEqual(added, []string{"20261001-090000-200-beef"}) {
		t.Fatalf("delta after watermark: %v", added)
	}
}

func mustLoad(t *testing.T, root string) *Tree {
	t.Helper()
	tr, err := Load(root, nil, false, t0)
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

// fakeHub is an in-memory MessageAPI with the hub's idempotency rule:
// same MessageID + same content = replay; different content = conflict.
type fakeHub struct {
	mu     sync.Mutex
	byID   map[string]contracts.SendMessageRequest
	order  []string
	acks   int
	failAt int // fail the Nth send (1-based) once
	sends  int
}

func newFakeHub() *fakeHub { return &fakeHub{byID: map[string]contracts.SendMessageRequest{}} }

func (h *fakeHub) SendMessage(_ context.Context, req contracts.SendMessageRequest) (contracts.MessageView, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sends++
	if h.failAt == h.sends {
		return contracts.MessageView{}, contracts.Errorf(contracts.ErrUnavailable, "synthetic hub outage")
	}
	if prev, ok := h.byID[req.MessageID]; ok {
		a, _ := json.Marshal(prev)
		b, _ := json.Marshal(req)
		if !bytes.Equal(a, b) {
			return contracts.MessageView{}, contracts.Errorf(contracts.ErrConflict, "message id reused with different content")
		}
	} else {
		if req.InReplyTo != "" {
			if _, ok := h.byID[req.InReplyTo]; !ok {
				return contracts.MessageView{}, contracts.Errorf(contracts.ErrInvalid, "in_reply_to unknown")
			}
		}
		h.byID[req.MessageID] = req
		h.order = append(h.order, req.MessageID)
	}
	return contracts.MessageView{Envelope: contracts.MessageEnvelope{MessageID: req.MessageID}, Cursor: int64(len(h.order))}, nil
}
func (h *fakeHub) GetMessage(context.Context, contracts.GetRequest) (contracts.MessageView, error) {
	return contracts.MessageView{}, nil
}
func (h *fakeHub) ListMessages(context.Context, contracts.ListMessagesRequest) (contracts.Page[contracts.MessageView], error) {
	return contracts.Page[contracts.MessageView]{}, nil
}
func (h *fakeHub) AckMessage(context.Context, contracts.AckMessageRequest) (contracts.MessageView, error) {
	h.mu.Lock()
	h.acks++
	h.mu.Unlock()
	return contracts.MessageView{}, nil
}
func (h *fakeHub) ReissueMessage(context.Context, contracts.ReissueMessageRequest) (contracts.MessageView, error) {
	return contracts.MessageView{}, nil
}
func (h *fakeHub) ResolveDelivery(context.Context, contracts.ResolveDeliveryRequest) (contracts.MessageView, error) {
	return contracts.MessageView{}, nil
}
func (h *fakeHub) CancelMessage(context.Context, contracts.CancelMessageRequest) (contracts.MessageView, error) {
	return contracts.MessageView{}, nil
}

func TestApplyIdempotent(t *testing.T) {
	p := Build(loadFixture(t), Options{Aliases: fullAliases()})
	hub := newFakeHub()
	res, err := Apply(context.Background(), hub, p, ApplyOptions{})
	if err != nil || res.Stored != 9 || res.Conflicts != 0 || len(hub.order) != 9 {
		t.Fatalf("first apply: %+v err=%v stored=%d", res, err, len(hub.order))
	}
	res, err = Apply(context.Background(), hub, p, ApplyOptions{})
	if err != nil || res.Stored != 9 || len(hub.order) != 9 {
		t.Fatalf("re-apply duplicated: %+v stored=%d", res, len(hub.order))
	}
	if hub.acks != 0 {
		t.Fatal("import must never acknowledge legacy messages")
	}
	// Changed content under the same id is reported, not overwritten.
	p.Items[0].Request.Body.Text += " (edited)"
	res, _ = Apply(context.Background(), hub, p, ApplyOptions{})
	if res.Conflicts != 1 {
		t.Fatalf("conflict not reported: %+v", res)
	}
}

func TestApplyResumesAfterOutage(t *testing.T) {
	p := Build(loadFixture(t), Options{Aliases: fullAliases()})
	hub := newFakeHub()
	hub.failAt = 3
	res, err := Apply(context.Background(), hub, p, ApplyOptions{MaxConsecutiveErrors: 1})
	if err == nil || !res.Stopped || res.Stored != 2 {
		t.Fatalf("outage: %+v err=%v", res, err)
	}
	res, err = Apply(context.Background(), hub, p, ApplyOptions{})
	if err != nil || len(hub.order) != 9 {
		t.Fatalf("resume: %+v err=%v stored=%d", res, err, len(hub.order))
	}
}

func TestMainCLI(t *testing.T) {
	dir := t.TempDir()
	copyRoot := filepath.Join(dir, "copy")
	env := func(k string) string {
		if k == "HOME" {
			return dir
		}
		return ""
	}
	var out, errb bytes.Buffer
	code := Main(context.Background(), []string{"--root", copyRoot, "--snapshot-from", fixture}, &out, &errb, env, nil)
	if code != 0 || !strings.Contains(out.String(), "DRY RUN") || !strings.Contains(out.String(), "messages      7") {
		t.Fatalf("dry run: code=%d out=%s err=%s", code, out.String(), errb.String())
	}
	tmpl := filepath.Join(dir, "aliases.json")
	out.Reset()
	if code := Main(context.Background(), []string{"--root", copyRoot, "--write-alias-template", tmpl}, &out, &errb, env, nil); code != 0 {
		t.Fatalf("template: %d %s", code, errb.String())
	}
	am, err := LoadAliasMap(tmpl)
	if err != nil || am.Mailboxes["hq"].Kind != contracts.AddrOwner || !strings.Contains(am.Mailboxes["beta-stream"].Note, "harness=codex") {
		t.Fatalf("template content: %+v %v", am, err)
	}
	b, _ := json.Marshal(fullAliases())
	aliasPath := filepath.Join(dir, "filled.json")
	if err := os.WriteFile(aliasPath, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if code := Main(context.Background(), []string{"--root", copyRoot, "--alias-map", aliasPath, "--apply"}, &out, &errb, env, nil); code != 2 {
		t.Fatalf("--apply without hub: %d", code)
	}
	hub := newFakeHub()
	mapOut := filepath.Join(dir, "map.json")
	out.Reset()
	code = Main(context.Background(), []string{"--root", copyRoot, "--alias-map", aliasPath, "--map-out", mapOut, "--apply"}, &out, &errb, env,
		func(context.Context) (contracts.MessageAPI, error) { return hub, nil })
	if code != 0 || len(hub.order) != 9 {
		t.Fatalf("apply: code=%d stored=%d err=%s", code, len(hub.order), errb.String())
	}
	mb, _ := os.ReadFile(mapOut)
	if bytes.Contains(mb, []byte("Grüße")) || !bytes.Contains(mb, []byte("msg_legacy-ws-")) {
		t.Fatal("map-out must carry ids, not bodies")
	}
	// The live root (HOME/streams) is refused.
	live := filepath.Join(dir, "streams")
	if _, err := CopyTree(fixture, live); err != nil {
		t.Fatal(err)
	}
	errb.Reset()
	if code := Main(context.Background(), []string{"--root", live}, &out, &errb, env, nil); code != 1 || !strings.Contains(errb.String(), "live ws root") {
		t.Fatalf("live root: code=%d err=%s", code, errb.String())
	}
}

func TestCopyTreeKeepsEmptyMailboxes(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src")
	if _, err := CopyTree(fixture, src); err != nil {
		t.Fatal(err)
	}
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(src, "inbox", "gamma-stream", "read"), 0o700))
	must(os.MkdirAll(filepath.Join(src, "streams", "gamma-stream"), 0o700))
	must(os.WriteFile(filepath.Join(src, "streams", "gamma-stream", "STREAM.md"), []byte("---\ntitle: Synthetic gamma\n---\n"), 0o600))
	dst := filepath.Join(t.TempDir(), "dst")
	if _, err := CopyTree(src, dst); err != nil {
		t.Fatal(err)
	}
	r := Build(mustLoad(t, dst), Options{}).Report
	if r.Mailboxes != 4 || r.Streams != 3 || r.MailboxesWithoutStream != 0 {
		t.Fatalf("empty mailbox lost in copy: %+v", r)
	}
}

func TestRollbackWriteBackNotReimported(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "copy")
	if _, err := CopyTree(fixture, dst); err != nil {
		t.Fatal(err)
	}
	wb := "---\nid: 20261002-090000-300-cafe\nfrom: hq\nto: alpha-stream\ncreated: 2026-10-02T09:00:00-07:00\npyre_message_id: msg_01SYNTHROLLBACK\n---\n\nSynthetic message returned to ws by a rollback.\n"
	if err := os.WriteFile(filepath.Join(dst, "inbox", "alpha-stream", "20261002-090000-300-cafe.md"), []byte(wb), 0o600); err != nil {
		t.Fatal(err)
	}
	p := Build(mustLoad(t, dst), Options{Aliases: fullAliases()})
	if p.Report.AlreadyInPyre != 1 || p.Report.Requests != 9 || itemByLegacy(p, "20261002-090000-300-cafe") != nil {
		t.Fatalf("rollback write-back re-imported: %+v", p.Report)
	}
}
