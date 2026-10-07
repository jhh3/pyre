package claudeplugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeSource struct {
	mu      sync.Mutex
	msgs    []Message
	pending map[string]bool
	fail    int // fail the next N polls
	polls   int
}

func (f *fakeSource) add(id, sender string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.msgs = append(f.msgs, Message{ID: id, Sender: sender, Cursor: int64(len(f.msgs) + 1)})
	f.pending[id] = true
}

func (f *fakeSource) Poll(ctx context.Context, after int64, wait time.Duration) ([]Message, int64, error) {
	f.mu.Lock()
	f.polls++
	if f.fail > 0 {
		f.fail--
		f.mu.Unlock()
		return nil, after, errors.New("connection refused (pyred down)")
	}
	var out []Message
	next := after
	for _, m := range f.msgs {
		if m.Cursor > after {
			out = append(out, m)
			next = m.Cursor
		}
	}
	f.mu.Unlock()
	if len(out) == 0 {
		select {
		case <-time.After(20 * time.Millisecond):
		case <-ctx.Done():
		}
	}
	return out, next, nil
}

func (f *fakeSource) Pending(ctx context.Context, ids []string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, id := range ids {
		if f.pending[id] {
			out = append(out, id)
		}
	}
	return out, nil
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) lines() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := strings.TrimSpace(s.b.String())
	if t == "" {
		return nil
	}
	return strings.Split(t, "\n")
}

func waitLines(t *testing.T, b *syncBuf, n int, d time.Duration) []string {
	t.Helper()
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if l := b.lines(); len(l) >= n {
			return l
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("want %d lines, got %q", n, b.lines())
	return nil
}

func TestWatchCoalescesAndDedupes(t *testing.T) {
	dir := t.TempDir()
	src := &fakeSource{pending: map[string]bool{}}
	out := &syncBuf{}
	opts := WatchOptions{Out: out, StateFile: filepath.Join(dir, "state.json"), MinInterval: 400 * time.Millisecond, Grace: 50 * time.Millisecond, PollWait: time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Watch(ctx, src, opts) }()
	src.add("msg_1", "ses_peer")
	l := waitLines(t, out, 1, 3*time.Second)
	if !strings.Contains(l[0], "1 new message for this session (msg_1 from ses_peer)") || !strings.Contains(l[0], "mcp__pyre__inbox") || !strings.Contains(l[0], "mcp__pyre__ack_message") {
		t.Fatalf("line %q", l[0])
	}
	// Storm: 12 messages within the interval -> one coalesced line.
	for i := 0; i < 12; i++ {
		src.add("msg_s"+string(rune('a'+i)), "owner")
	}
	waitLines(t, out, 2, 3*time.Second)
	time.Sleep(600 * time.Millisecond)
	if got := out.lines(); len(got) != 2 || !strings.Contains(got[1], "12 new messages") || !strings.Contains(got[1], "and 7 more") {
		t.Fatalf("storm lines %q", got)
	}
	cancel()
	<-done
	// Restart (pyred or monitor restart): no duplicate notifications.
	out2 := &syncBuf{}
	opts.Out = out2
	ctx2, cancel2 := context.WithCancel(context.Background())
	go func() { done <- Watch(ctx2, src, opts) }()
	time.Sleep(700 * time.Millisecond)
	if got := out2.lines(); len(got) != 0 {
		t.Fatalf("duplicate after restart: %q", got)
	}
	src.add("msg_new", "ses_peer")
	l = waitLines(t, out2, 1, 3*time.Second)
	if !strings.Contains(l[0], "msg_new") || strings.Contains(l[0], "msg_1") {
		t.Fatalf("after restart %q", l)
	}
	cancel2()
	<-done
	var st watchState
	b, _ := os.ReadFile(opts.StateFile)
	if json.Unmarshal(b, &st) != nil || st.Cursor != 14 || len(st.Notified) != 14 {
		t.Fatalf("state %+v", st)
	}
}

func TestWatchSkipsHandledAndRetries(t *testing.T) {
	dir := t.TempDir()
	// The paste path claimed msg_p.
	_ = os.WriteFile(filepath.Join(dir, "deliveries.jsonl"), []byte(`{"message_id":"msg_p","state":"attempting"}`+"\n"+`{"message_id":"msg_r","state":"attempting"}`+"\n"+`{"message_id":"msg_r","state":"rejected"}`+"\n"), 0o600)
	src := &fakeSource{pending: map[string]bool{}, fail: 2}
	out := &syncBuf{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Watch(ctx, src, WatchOptions{Out: out, RunDir: dir, MinInterval: 100 * time.Millisecond, Grace: 300 * time.Millisecond, PollWait: time.Second})
	src.add("msg_p", "a")
	src.add("msg_r", "b") // paste rejected -> doorbell may ring
	src.add("msg_acked", "c")
	src.mu.Lock()
	src.pending["msg_acked"] = false // acked before the grace period ended
	src.mu.Unlock()
	l := waitLines(t, out, 1, 6*time.Second)
	if len(l) != 1 || !strings.Contains(l[0], "1 new message") || !strings.Contains(l[0], "msg_r") {
		t.Fatalf("lines %q", l)
	}
	src.mu.Lock()
	polls := src.polls
	src.mu.Unlock()
	if polls < 3 {
		t.Fatalf("polls %d: errors must be retried", polls)
	}
}

func TestFormatLineSanitizes(t *testing.T) {
	l := FormatLine([]Message{{ID: "msg_1", Sender: "evil\nname\x1b[2J"}})
	if strings.ContainsAny(l, "\n\x1b") {
		t.Fatalf("%q", l)
	}
}

func TestGenerate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "plugin")
	if _, err := Generate(dir, `'/opt/pyred' inbox-watch --run-dir '/x y'`); err != nil {
		t.Fatal(err)
	}
	var mons []map[string]string
	b, _ := os.ReadFile(filepath.Join(dir, "monitors", "monitors.json"))
	if json.Unmarshal(b, &mons) != nil || len(mons) != 1 || mons[0]["name"] != MonitorName || !strings.Contains(mons[0]["command"], "inbox-watch") {
		t.Fatalf("%s", b)
	}
	if _, err := os.Stat(filepath.Join(dir, ".claude-plugin", "plugin.json")); err != nil {
		t.Fatal(err)
	}
	if !IsNotification("<task-notification>\n<summary>Monitor event: \"New Pyre messages for this session\"</summary>") {
		t.Fatal("IsNotification")
	}
}

// An idle watcher whose Claude session is gone exits on its own (no orphan
// long-polling forever), and removes its pid file.
func TestWatchExitsWhenSessionGone(t *testing.T) {
	dir := t.TempDir()
	WritePID(dir)
	src := &fakeSource{pending: map[string]bool{}}
	var alive sync.Mutex
	gone := false
	done := make(chan error, 1)
	go func() {
		done <- Watch(context.Background(), src, WatchOptions{Out: &bytes.Buffer{}, AliveEvery: 500 * time.Millisecond, Alive: func() bool {
			alive.Lock()
			defer alive.Unlock()
			return !gone
		}})
	}()
	time.Sleep(300 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("watcher exited while the session lives: %v", err)
	default:
	}
	alive.Lock()
	gone = true
	alive.Unlock()
	select {
	case err := <-done:
		if !errors.Is(err, ErrSessionGone) {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("watcher did not exit after the session was gone")
	}
	RemovePID(dir)
	if _, err := os.Stat(filepath.Join(dir, PIDFile)); !os.IsNotExist(err) {
		t.Fatalf("pid file left behind: %v", err)
	}
}

// The notification names the pre-approved MCP reply tool too, so the agent
// does not reply through `pyre send` in Bash (a permission dialog).
func TestFormatLineNamesTheMCPReplyTool(t *testing.T) {
	l := FormatLine([]Message{{ID: "msg_1", Sender: "ses_peer"}})
	for _, want := range []string{"mcp__pyre__inbox", "mcp__pyre__send_message", "reply_to", "mcp__pyre__ack_message"} {
		if !strings.Contains(l, want) {
			t.Errorf("notification lacks %q: %s", want, l)
		}
	}
}
