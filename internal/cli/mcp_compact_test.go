package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/mock"
)

// sessAPI answers GetSession from a map (the only call compact rendering
// makes); every other API method is unused.
type sessAPI struct {
	c.API
	m     map[string]c.SessionView
	calls int
}

func (s *sessAPI) GetSession(_ context.Context, r c.GetRequest) (c.SessionView, error) {
	s.calls++
	if sv, ok := s.m[r.ID]; ok {
		return sv, nil
	}
	return c.SessionView{}, c.Errorf(c.ErrNotFound, "no session %s", r.ID)
}

var compactNow = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

func synthSession(id, title, node, nodeName string, state c.RunState) c.SessionView {
	return c.SessionView{
		Session:  c.Session{SessionID: id, Harness: c.HarnessClaude, Title: title, CreatedAt: compactNow.Add(-time.Hour)},
		Run:      &c.Run{RunID: "run_" + id[4:], SessionID: id, NodeID: node, Cwd: "/home/synth/work", State: state, Freshness: c.Freshness{ObservedAt: compactNow.Add(-2 * time.Minute)}, Capabilities: []string{"input.queue", "input.steer", "transcript.read"}},
		NodeName: nodeName,
	}
}

func synthMessage(id, from, to, text string, at time.Time) c.MessageView {
	stored := at.Add(time.Second)
	return c.MessageView{
		Envelope: c.MessageEnvelope{SchemaVersion: 1, MessageID: id, OperationID: "op_" + id[4:], Sender: c.Principal{Kind: c.PrincipalSession, ID: from},
			Recipient: c.Address{Kind: c.AddrSession, ID: to}, Mode: c.ModeQueue, Body: c.MessageBody{Text: text}, CreatedAt: at, ThreadID: "thr_" + id[4:]},
		Status: c.DeliveryStatus{Storage: c.StorageStored, StoredAt: &stored, Runtime: c.DeliveryAccepted, Ack: c.AckNone, UpdatedAt: stored, ConsumedAt: &stored,
			Receipts: []c.DeliveryReceipt{
				{MessageID: id, AttemptID: "att_" + id[4:] + "1", SessionID: to, RunID: "run_x", NodeID: "node_synth-mac", State: c.DeliveryAttempting, Evidence: c.EvidenceNone, ObservedAt: stored},
				{MessageID: id, AttemptID: "att_" + id[4:] + "1", SessionID: to, RunID: "run_x", NodeID: "node_synth-mac", State: c.DeliveryAccepted, Evidence: c.EvidencePyreRead, EvidenceRef: "mcp inbox", ObservedAt: stored, Consumed: true},
			}},
		Cursor: 42,
	}
}

func compactFixture() (*Ops, c.Page[c.MessageView]) {
	api := &sessAPI{m: map[string]c.SessionView{
		"ses_synth-child": synthSession("ses_synth-child", "synthetic research child", "node_synth-mac", "synth-mac", c.RunWaitingInput),
		"ses_synth-far":   synthSession("ses_synth-far", "synthetic remote builder", "node_synth-linux", "synth-linux", c.RunRunning),
		"ses_synth-me":    synthSession("ses_synth-me", "synthetic coordinator", "node_synth-mac", "synth-mac", c.RunRunning),
	}}
	o := &Ops{API: api, Self: Identity{SessionID: "ses_synth-me", NodeID: "node_synth-mac"}}
	long := "Finished the synthetic survey. " + strings.Repeat("Each finding is described at length with evidence and caveats. ", 12) +
		"Full report: /home/synth/work/reports/survey-2026-10-08.md and see ver_synth-report-2."
	pg := c.Page[c.MessageView]{SnapshotAt: compactNow, Items: []c.MessageView{
		synthMessage("msg_synth-1", "ses_synth-child", "ses_synth-me", long, compactNow.Add(-3*time.Minute)),
		synthMessage("msg_synth-2", "ses_synth-far", "ses_synth-me", "Build is green; artifacts in /srv/synth/out/build-17/ on my side.", compactNow.Add(-90*time.Second)),
	}}
	pg.Items[1].Envelope.RequestAck = true
	pg.Items[1].Envelope.InReplyTo = "msg_synth-0"
	return o, pg
}

func TestCompactInboxKeepsIDsAndPaths(t *testing.T) {
	o, pg := compactFixture()
	rc := newRenderCtx(context.Background(), o)
	got := renderInbox(rc, pg)
	full, _ := json.MarshalIndent(pg, "", "  ")
	t.Logf("inbox: JSON %d bytes -> compact %d bytes\n%s", len(full), len(got), got)
	lines := strings.Split(strings.TrimSpace(got), "\n")
	if len(lines) != 3 {
		t.Fatalf("want 2 message lines + footer:\n%s", got)
	}
	for _, want := range []string{"[pyre-msg:msg_synth-1] from synthetic research child · 3m ago", "/home/synth/work/reports/survey-2026-10-08.md", "ver_synth-report-2"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("line 1 lacks %q:\n%s", want, lines[0])
		}
	}
	if !strings.Contains(lines[1], "from synthetic remote builder (synth-linux)") || !strings.Contains(lines[1], "re msg_synth-0") || !strings.Contains(lines[1], "ack requested") {
		t.Errorf("line 2: %s", lines[1])
	}
	if strings.Contains(got, "receipt") || strings.Contains(got, "att_") || strings.Contains(got, "cursor") {
		t.Errorf("compact inbox leaks receipts/cursors:\n%s", got)
	}
	if lines[2] != "2 unacked messages · full text: get_message <id>" {
		t.Errorf("footer %q", lines[2])
	}
	if len(got)*4 > len(full) {
		t.Errorf("compact inbox is not much smaller: %d vs %d bytes", len(got), len(full))
	}
	if renderInbox(newRenderCtx(context.Background(), o), c.Page[c.MessageView]{}) != "no unacked messages\n" {
		t.Error("empty inbox")
	}
}

func TestCompactMessageShowsFullBodyAndRemotePathNote(t *testing.T) {
	o, pg := compactFixture()
	for i, m := range pg.Items {
		got := renderMessage(newRenderCtx(context.Background(), o), m)
		full, _ := json.MarshalIndent(m, "", "  ")
		t.Logf("get_message: JSON %d bytes -> compact %d bytes\n%s", len(full), len(got), got)
		if !strings.Contains(got, m.Envelope.Body.Text) || strings.Contains(got, "att_") {
			t.Errorf("body must be complete and receipts absent:\n%s", got)
		}
		remote := strings.Contains(got, "paths in this message are on synth-linux")
		if remote != (i == 1) {
			t.Errorf("remote path note on message %d: %v\n%s", i, remote, got)
		}
	}
}

func TestCompactSessionsMarkThisMachine(t *testing.T) {
	o, _ := compactFixture()
	api := o.API.(*sessAPI)
	pg := c.Page[c.SessionView]{SnapshotAt: compactNow}
	for _, id := range []string{"ses_synth-child", "ses_synth-far"} {
		pg.Items = append(pg.Items, api.m[id])
	}
	got := renderSessions(newRenderCtx(context.Background(), o), pg)
	full, _ := json.MarshalIndent(pg, "", "  ")
	t.Logf("list_sessions: JSON %d bytes -> compact %d bytes\n%s", len(full), len(got), got)
	if !strings.Contains(got, "ses_synth-child · synthetic research child · waiting_input · synth-mac (this machine) · claude · 2m ago") ||
		!strings.Contains(got, "· synth-linux · ") || strings.Contains(got, "synth-linux (this machine)") {
		t.Fatalf("sessions:\n%s", got)
	}
	if api.calls != 0 {
		t.Errorf("session lists need no lookups, made %d", api.calls)
	}
}

func TestSnippetKeepsPathsWhole(t *testing.T) {
	path := "/very/long/synthetic/path/to/a/report/file-with-a-long-name.md"
	s, cut := snippet(strings.Repeat("word ", 30)+path+" tail words here", 160)
	if !cut || !strings.Contains(s, path) {
		t.Fatalf("path crossing the cut must stay whole: %q", s)
	}
	s, cut = snippet(strings.Repeat("word ", 60)+"see /a/b/c.md and https://example.invalid/x", 40)
	if !cut || !strings.Contains(s, "[refs: /a/b/c.md https://example.invalid/x]") {
		t.Fatalf("refs beyond the cut must be appended: %q", s)
	}
	if s, cut := snippet("short\nmulti  line", 160); cut || s != "short multi line" {
		t.Fatalf("short text: %q %v", s, cut)
	}
	if refOf("and/or") != "" || refOf("(/tmp/x.md),") != "/tmp/x.md" {
		t.Fatal("refOf")
	}
}

func TestCompactToolFormatArgument(t *testing.T) {
	b := mock.New(mock.Options{Clock: mock.NewOffsetClock(mock.FixtureEpoch), DefaultPrincipal: &testOwner})
	cl := startMCP(t, &Ops{API: b, PollInterval: 10 * time.Millisecond})
	cl.call("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "test", "version": "0"}})
	text, isErr := cl.tool("list_sessions", map[string]any{})
	if isErr || strings.HasPrefix(string(text), "{") || !strings.Contains(string(text), "ses_synth-b") {
		t.Fatalf("compact list_sessions: %s", text)
	}
	out, isErr := cl.tool("list_sessions", map[string]any{"format": "json"})
	var pg c.Page[c.SessionView]
	if isErr || json.Unmarshal(out, &pg) != nil || len(pg.Items) == 0 {
		t.Fatalf("json list_sessions: %s", out)
	}
	t.Logf("list_sessions over MCP (mock fleet): JSON %d bytes -> compact %d bytes", len(out), len(text))
	if out, isErr := cl.tool("list_sessions", map[string]any{"format": "yaml"}); !isErr || !strings.Contains(string(out), "format") {
		t.Fatalf("bad format must be a usage error: %s", out)
	}
	if out, isErr := cl.tool("list_sessions", map[string]any{"bogus": 1}); !isErr || !strings.Contains(string(out), "invalid arguments") {
		t.Fatalf("unknown arguments still rejected: %s", out)
	}
	msg, isErr := cl.tool("get_message", map[string]any{"message_id": "msg_synth-indeterminate"})
	if isErr || !strings.HasPrefix(string(msg), "[pyre-msg:msg_synth-indeterminate]") || !strings.Contains(string(msg), "INDETERMINATE") {
		t.Fatalf("compact get_message: %s", msg)
	}
	in, isErr := cl.tool("inbox", map[string]any{"session": "ses_synth-b", "unacked": false})
	if isErr || strings.HasPrefix(string(in), "{") {
		t.Fatalf("inbox: %s", in)
	}
}

func TestMCPInboxDefaultsToUnacked(t *testing.T) {
	for _, tc := range []struct {
		p    mcpInboxParams
		want bool
	}{
		{mcpInboxParams{}, true},
		{mcpInboxParams{InboxParams: InboxParams{Thread: "thr_x"}}, false},
		{mcpInboxParams{InboxParams: InboxParams{Box: "outbox"}}, false},
		{mcpInboxParams{Unacked: new(bool)}, false},
	} {
		if got := tc.p.effective().Unacked; got != tc.want {
			t.Errorf("%+v: unacked %v, want %v", tc.p, got, tc.want)
		}
	}
	var p mcpInboxParams
	if err := json.Unmarshal([]byte(`{"unacked":false,"limit":3}`), &p); err != nil || p.Unacked == nil || *p.Unacked || p.Limit != 3 {
		t.Fatalf("decode: %+v %v", p, err)
	}
}
