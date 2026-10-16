package claude

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// doorbellEnv starts the fake API on a short unix socket.
func doorbellEnv(t *testing.T) (*fakeAPI, string, []string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "pd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	api := newFakeAPI("ses_door")
	sock := filepath.Join(dir, "api.sock")
	if err := api.serve(sock, "synthetic-session-token"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(api.down)
	env := append(os.Environ(), "PYRE_SOCKET="+sock, "PYRE_SESSION_TOKEN=synthetic-session-token", "PYRE_SESSION_ID=ses_door",
		"PYRE_HUB=", "PYRE_TOKEN=")
	return api, dir, env
}

// TestDoorbellPyreSide: the real `pyre mcp` reads and acks through the fake
// API, and the watcher role rings once per batch.
func TestDoorbellPyreSide(t *testing.T) {
	api, dir, env := doorbellEnv(t)
	exe, _ := os.Executable()
	api.post("msg_d1", "ses_peer", "synthetic message one")

	// Watcher: one line on stdout, no body.
	runDir := filepath.Join(dir, "run")
	_ = os.MkdirAll(runDir, 0o700)
	w := exec.Command(exe, "--run-dir", runDir)
	w.Env = append(env, "PYRE_TEST_ROLE=inboxwatch", "PYRE_TEST_DOORBELL_INTERVAL_MS=300")
	out, _ := w.StdoutPipe()
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	defer w.Process.Kill()
	lines := make(chan string, 10)
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()
	select {
	case l := <-lines:
		if !strings.Contains(l, "msg_d1 from session:ses_peer, sent ") || strings.Contains(l, "synthetic message one") {
			t.Fatalf("line %q", l)
		}
	case <-time.After(10 * time.Second):
		b, _ := os.ReadFile(filepath.Join(runDir, "doorbell-watch.log"))
		t.Fatalf("no doorbell line; watch log:\n%s", b)
	}

	// pyre mcp: initialize, inbox, ack_message over stdio.
	m := exec.Command(exe)
	m.Env = append(env, "PYRE_TEST_ROLE=pyremcp")
	in, _ := m.StdinPipe()
	mout, _ := m.StdoutPipe()
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	defer m.Process.Kill()
	rd := bufio.NewReader(mout)
	call := func(id int, method string, params any) map[string]any {
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
		in.Write(append(b, '\n'))
		for {
			line, err := rd.ReadBytes('\n')
			if err != nil {
				t.Fatalf("mcp read: %v", err)
			}
			var r map[string]any
			if json.Unmarshal(line, &r) == nil && r["id"] != nil {
				return r
			}
		}
	}
	call(1, "initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "t", "version": "0"}})
	in.Write([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n"))
	r := call(2, "tools/call", map[string]any{"name": "inbox", "arguments": map[string]any{"unacked": true}})
	raw, _ := json.Marshal(r)
	if !strings.Contains(string(raw), "msg_d1") || !strings.Contains(string(raw), "synthetic message one") {
		t.Fatalf("inbox result %s", raw)
	}
	// Evidence extraction from a PostToolUse-shaped payload carrying it.
	payload, _ := json.Marshal(map[string]any{"tool_name": toolPyreInbox, "tool_input": map[string]any{"unacked": true}, "tool_response": r["result"]})
	if ids := pyreToolMessageIDs(toolPyreInbox, nil, payload); len(ids) != 1 || ids[0] != "msg_d1" {
		t.Fatalf("extracted %v from %s", ids, payload)
	}
	call(3, "tools/call", map[string]any{"name": "ack_message", "arguments": map[string]any{"message_id": "msg_d1"}})
	if _, ok := api.ackedAt("msg_d1"); !ok {
		t.Fatal("ack not recorded")
	}
}

func TestDoorbellBlockedByEnv(t *testing.T) {
	t.Setenv("DISABLE_TELEMETRY", "")
	t.Setenv("CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "")
	t.Setenv("CLAUDE_CODE_USE_BEDROCK", "")
	if k := doorbellBlocked(map[string]string{}, nil); k != "" {
		t.Fatalf("blocked by %s", k)
	}
	if k := doorbellBlocked(map[string]string{"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1"}, nil); k != "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC" {
		t.Fatal(k)
	}
	t.Setenv("DISABLE_TELEMETRY", "1")
	if k := doorbellBlocked(nil, nil); k != "DISABLE_TELEMETRY" {
		t.Fatal(k)
	}
	if k := doorbellBlocked(nil, []string{"DISABLE_TELEMETRY"}); k != "" {
		t.Fatalf("unset var still blocks: %s", k)
	}
}

func TestDoorbellFoldAndReceipts(t *testing.T) {
	dir := t.TempDir()
	a := &Adapter{o: Options{Now: time.Now, NodeID: "node_t", DoorbellCommand: []string{"pyred", "inbox-watch"}}}
	if !contracts.HasCap(a.Capabilities(), contracts.CapInputMonitor) {
		t.Fatal("cap")
	}
	resp := `{"content":[{"type":"text","text":"{\"items\":[{\"envelope\":{\"message_id\":\"msg_x\",\"in_reply_to\":\"msg_old\"}}]}"}]}`
	raw := []byte(`{"hook_event_name":"PostToolUse","tool_name":"mcp__pyre__inbox","tool_use_id":"tu1","tool_response":` + resp + `}`)
	var p map[string]any
	_ = json.Unmarshal(raw, &p)
	ids := pyreToolMessageIDs(toolPyreInbox, p, raw)
	if len(ids) != 1 || ids[0] != "msg_x" {
		t.Fatalf("ids %v (in_reply_to must not count)", ids)
	}
	// Compact text (W-compact): the [pyre-msg:<id>] marker per message; a
	// "re <id>" reference must not count.
	compactResp := `{"content":[{"type":"text","text":"[pyre-msg:msg_c1] from synthetic child · 3m ago · done\n[pyre-msg:msg_c2] from owner · 1m ago · re msg_old · ok\n2 unacked messages"}]}`
	craw := []byte(`{"hook_event_name":"PostToolUse","tool_name":"mcp__pyre__inbox","tool_response":` + compactResp + `}`)
	if got := pyreToolMessageIDs(toolPyreInbox, nil, craw); len(got) != 2 || got[0] != "msg_c1" || got[1] != "msg_c2" {
		t.Fatalf("compact ids %v", got)
	}
	rs := newRunState()
	t0 := time.Now()
	rs.applyHook(hookRecord{Event: "SessionStart", At: t0})
	rs.applyHook(hookRecord{ID: "h1", Event: "PostToolUse", ToolName: toolPyreInbox, Markers: ids, At: t0.Add(time.Second), offset: 10})
	rs.applyHook(hookRecord{ID: "h2", Event: "PostToolUse", ToolName: toolPyreAck, Markers: []string{"msg_x"}, At: t0.Add(2 * time.Second), offset: 20})
	rs.applyTranscript(transcriptRecord{Type: "user", Message: json.RawMessage(`{"role":"user","content":"<task-notification>\n<summary>Monitor event: \"New Pyre messages for this session\"</summary>"}`)}, 0)
	if rs.doorbells != 1 {
		t.Fatal("doorbell not counted")
	}
	// The real 2.1.289 failure notice for a killed monitor.
	rs.applyTranscript(transcriptRecord{Type: "user", Message: json.RawMessage(`{"role":"user","content":"<task-notification> <task-id>b</task-id> <status>failed</status> <summary>Monitor \"New Pyre messages for this session\" script failed (exit 137)</summary> </task-notification>"}`)}, 0)
	if rs.doorbells != 1 || rs.doorbellFailed != 1 {
		t.Fatalf("failure notice: doorbells=%d failed=%v", rs.doorbells, rs.doorbellFailed)
	}
	ev, ok := rs.bestEvidence("msg_x")
	if !ok || ev.Kind != contracts.EvidenceClaudeInboxRead {
		t.Fatalf("%+v", ev)
	}
	recs, _ := a.receipts(contracts.RunHandle{RunID: "run_1", SessionID: "ses_1", StateDir: dir}, rs, "")
	if len(recs) != 1 || recs[0].AttemptID != "" || !strings.Contains(recs[0].Detail, "acked_at=") || recs[0].Evidence != contracts.EvidenceClaudeInboxRead {
		t.Fatalf("receipts %+v", recs)
	}
}

// A run whose launch environment disables plugin monitors reports
// input.monitor as unavailable (the node drops it from the run), and gets
// no plugin; an enabled run keeps it.
func TestDoorbellPerRunCapability(t *testing.T) {
	f := newFakeEnv(t, func(o *Options) { o.DoorbellCommand = []string{"/bin/sleep", "60"} })
	if !contracts.HasCap(f.a.Capabilities(), contracts.CapInputMonitor) {
		t.Fatal("adapter with DoorbellCommand does not advertise input.monitor")
	}
	t.Setenv("DISABLE_TELEMETRY", "")
	t.Setenv("CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "")
	on := f.launch("run_dbon", nil, nil)
	if on.Data["doorbell"] != "on" || on.Data[contracts.HandleCapsUnavailable] != "" {
		t.Fatalf("enabled run data = %v", on.Data)
	}
	off := f.launch("run_dboff", map[string]string{"DISABLE_TELEMETRY": "1"}, nil)
	if !strings.HasPrefix(off.Data["doorbell"], "disabled") || off.Data[contracts.HandleCapsUnavailable] != contracts.CapInputMonitor {
		t.Fatalf("blocked run data = %v", off.Data)
	}
	if got := contracts.WithoutCaps(f.a.Capabilities(), off.Data[contracts.HandleCapsUnavailable]); contracts.HasCap(got, contracts.CapInputMonitor) || len(got) != len(f.a.Capabilities())-1 {
		t.Fatalf("WithoutCaps = %v", got)
	}
	if _, err := os.Stat(filepath.Join(off.StateDir, "pyre-plugin", "monitors")); err == nil {
		t.Fatal("blocked run got a doorbell monitor")
	}
}

// Mail the doorbell announced is held for the agent's inbox read (no paste
// racing the queued notification) until DoorbellPasteHold passes, and only
// while the watcher lives.
func TestDoorbellPasteHold(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1_900_000_000, 0)
	a := &Adapter{o: Options{DoorbellCommand: []string{"/x/pyred", "inbox-watch"}, DoorbellPasteHold: 3 * time.Minute, Now: func() time.Time { return now }}}
	h := contracts.RunHandle{RunID: "run_h", StateDir: dir}
	in := contracts.DeliveryInput{MessageID: "msg_held", AttemptID: "dlv_1", Mode: contracts.ModeQueue}
	if _, held := a.doorbellHold(h, in); held {
		t.Fatal("held without a notification")
	}
	b, _ := json.Marshal(map[string]any{"cursor": 3, "notified": map[string]int64{"msg_held": now.Add(-time.Minute).Unix()}})
	if err := os.WriteFile(filepath.Join(dir, "doorbell.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, held := a.doorbellHold(h, in); held {
		t.Fatal("held although no watcher is alive")
	}
	if err := os.WriteFile(filepath.Join(dir, "doorbell.pid"), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	r, held := a.doorbellHold(h, in)
	if !held || r.State != contracts.DeliveryQueued || !strings.Contains(r.Detail, "doorbell notified") {
		t.Fatalf("hold = %v %+v", held, r)
	}
	r2, _ := a.doorbellHold(h, in)
	if r2.Detail != r.Detail {
		t.Fatal("hold reason must be stable across retries")
	}
	if _, held := a.doorbellHold(h, contracts.DeliveryInput{MessageID: "msg_held", Mode: contracts.ModeSteer}); held {
		t.Fatal("steer held")
	}
	now = now.Add(3 * time.Minute)
	if _, held := a.doorbellHold(h, in); held {
		t.Fatal("still held after DoorbellPasteHold")
	}
}

// Claude lists the doorbell monitor in Stop's background_tasks; it must not
// make an idle run look like waiting_on_background (real 2.1.289 payload).
func TestBackgroundTasksIgnoreDoorbellMonitor(t *testing.T) {
	var p map[string]any
	raw := `{"background_tasks":[{"command":"/x/bin/pyred inbox-watch --run-dir /x/runs/run_1","description":"New Pyre messages for this session","id":"bgsk78b7j","status":"running","type":"shell"}]}`
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatal(err)
	}
	if n := backgroundTasks(p); n != 0 {
		t.Fatalf("monitor counted as background work: %d", n)
	}
	raw = `{"background_tasks":[{"command":"npm run dev","description":"dev server","id":"b1","status":"running","type":"shell"},{"command":"/x/pyred inbox-watch","description":"New Pyre messages for this session","id":"b2","status":"running","type":"shell"}]}`
	_ = json.Unmarshal([]byte(raw), &p)
	if n := backgroundTasks(p); n != 1 {
		t.Fatalf("background tasks = %d, want 1", n)
	}
}

// Every Claude run pre-approves the Pyre MCP messaging tools in its
// --settings, with or without the doorbell, so replying to a peer never
// opens a permission dialog. (This launch has no doorbell configured.)
func TestRunSettingsPreapproveMessagingTools(t *testing.T) {
	f := newFakeEnv(t, nil)
	h := f.launch("run_allow", nil, nil)
	b, err := os.ReadFile(filepath.Join(h.StateDir, fileHooks))
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Permissions struct {
			Allow []string `json:"allow"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	have := strings.Join(s.Permissions.Allow, ",")
	for _, want := range []string{"mcp__pyre__send_message", "mcp__pyre__inbox", "mcp__pyre__ack_message", "mcp__pyre__get_message", "mcp__pyre__wait_reply"} {
		if !strings.Contains(have, want) {
			t.Errorf("%s not pre-approved (allow=%s)", want, have)
		}
	}
	if strings.Contains(have, "start_session") || strings.Contains(have, "stop_session") {
		t.Errorf("session control pre-approved: %s", have)
	}
}
