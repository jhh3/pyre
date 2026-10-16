package claude

// Real Claude Code check of plugin prompt delivery (PYRE_REAL_PLUGIN=1):
// the real `claude` binary in an isolated CLAUDE_CONFIG_DIR, Pyre's real
// generated plugin and hooks module, an isolated tmux server, and a local
// synthetic Messages API (ANTHROPIC_BASE_URL) so no account or login is
// used. Verifies the module loads, its $.prompt.submit text enters the
// session as a prompt (reaches the model request), in order, with a human
// client attached.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/tmux"
)

type synthAPI struct {
	mu    sync.Mutex
	users []string // last user text of each streamed /v1/messages request
}

func (s *synthAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || !strings.HasPrefix(r.URL.Path, "/v1/messages") {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, "{}")
		return
	}
	var req struct {
		Stream   bool `json:"stream"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	b, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(b, &req)
	if strings.HasSuffix(r.URL.Path, "/count_tokens") {
		_, _ = io.WriteString(w, `{"input_tokens":10}`)
		return
	}
	if n := len(req.Messages); n > 0 && req.Messages[n-1].Role == "user" {
		s.mu.Lock()
		s.users = append(s.users, string(req.Messages[n-1].Content))
		s.mu.Unlock()
	}
	const answer = "synthetic ok"
	if !req.Stream {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"msg_synth","type":"message","role":"assistant","model":"claude-synthetic","content":[{"type":"text","text":%q}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":3}}`, answer)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	ev := func(name, data string) { fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data) }
	ev("message_start", `{"type":"message_start","message":{"id":"msg_synth","type":"message","role":"assistant","model":"claude-synthetic","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":1}}}`)
	ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
	ev("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":%q}}`, answer))
	ev("content_block_stop", `{"type":"content_block_stop","index":0}`)
	ev("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":3}}`)
	ev("message_stop", `{"type":"message_stop"}`)
}

func (s *synthAPI) saw(marker string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, u := range s.users {
		if strings.Contains(u, marker) {
			return true
		}
	}
	return false
}

func TestRealClaudePluginDelivery(t *testing.T) {
	if os.Getenv("PYRE_REAL_PLUGIN") != "1" {
		t.Skip("set PYRE_REAL_PLUGIN=1 (real claude binary; synthetic API, isolated profile, no account)")
	}
	dir, _ := os.MkdirTemp("", "pp")
	defer os.RemoveAll(dir)
	ctx := context.Background()
	api := &synthAPI{}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: api}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	cfg := filepath.Join(dir, "cfg")
	work := filepath.Join(dir, "work")
	_ = os.MkdirAll(cfg, 0o700)
	_ = os.MkdirAll(work, 0o700)
	work, _ = filepath.EvalSymlinks(work)
	const key = "sk-ant-api03-synthetic-pyre-plugin-test-key-0000000000"
	gc, _ := json.Marshal(map[string]any{
		"hasCompletedOnboarding": true, "theme": "dark", "numStartups": 5,
		"customApiKeyResponses": map[string]any{"approved": []string{key[len(key)-20:]}, "rejected": []string{}},
		"projects":              map[string]any{work: map[string]any{"hasTrustDialogAccepted": true, "hasCompletedProjectOnboarding": true}},
	})
	_ = os.WriteFile(filepath.Join(cfg, ".claude.json"), gc, 0o600)

	o := realOptions(t, dir)
	o.TrustWorkspace = func(string) bool { return true }
	o.ProfileAsConfigDir = true
	a, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	defer a.srv.KillServer(ctx)
	l := contracts.ResolvedLaunch{RunID: "run_plugreal", SessionID: "ses_plugreal", Cwd: work,
		StateDir: filepath.Join(dir, "run"), ProfileDir: cfg, Args: []string{"--model", "claude-sonnet-4-5"},
		Env: map[string]string{"ANTHROPIC_BASE_URL": "http://" + ln.Addr().String(), "ANTHROPIC_API_KEY": key,
			"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1", "PYRE_SESSION_ID": "ses_plugreal"},
		Unset: []string{"CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_AUTH_TOKEN", "PYRE_HUB", "PYRE_TOKEN"}}
	h, err := a.Launch(ctx, l)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		screen, _ := a.srv.CapturePane(ctx, h.TmuxTarget, tmux.CaptureOptions{})
		t.Logf("screen:\n%s", screen)
		ev, _ := os.ReadFile(filepath.Join(h.StateDir, fileEvents))
		for _, l := range strings.Split(string(ev), "\n") {
			if strings.Contains(l, `"event":"PyrePlugin`) {
				t.Logf("event: %s", oneLine(l, 300))
			}
		}
	}()
	end := time.Now().Add(60 * time.Second)
	for time.Now().Before(end) && !a.pluginLive(h) {
		time.Sleep(250 * time.Millisecond)
	}
	if !a.pluginLive(h) {
		t.Fatal("plugin module never reported alive (hooks module not loaded?)")
	}
	alive, _ := os.ReadFile(filepath.Join(h.StateDir, "plugin-alive.json"))
	t.Logf("module heartbeat: %s", strings.TrimSpace(string(alive)))
	waitIdle := func() {
		for end := time.Now().Add(60 * time.Second); time.Now().Before(end); time.Sleep(200 * time.Millisecond) {
			if ok, _ := a.InputReady(ctx, h, contracts.ModeQueue); ok {
				return
			}
		}
		st, why, _ := a.derived(h)
		t.Fatalf("never idle: %s %s", st, why)
	}
	waitIdle()

	// A human client attached: plugin delivery does not need the input lease.
	d, err := a.Attach(ctx, h, contracts.AttachCommand{RunID: h.RunID})
	if err != nil {
		t.Fatal(err)
	}
	outer, _ := tmux.NewServer("", filepath.Join(dir, "o.sock"))
	defer outer.KillServer(ctx)
	if err := outer.NewSession(ctx, tmux.SessionSpec{Name: "human", Argv: d.Command, Width: 200, Height: 50}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	for i, id := range []string{"msg_synthreal1", "msg_synthreal2"} {
		text := fmt.Sprintf("[pyre-msg:%s · sent 9:00 AM PT (just now)] from session:synthetic-peer:\nSynthetic Pyre test message %d. Reply with one word.", id, i+1)
		r, err := a.Deliver(ctx, h, contracts.DeliveryInput{MessageID: id, AttemptID: "dlv_" + id, Mode: contracts.ModeQueue, Text: text})
		t.Logf("deliver %s: %+v err=%v", id, r, err)
		if err != nil || r.State != contracts.DeliveryAccepted || r.Evidence != contracts.EvidenceClaudePluginPrompt {
			t.Fatalf("deliver %s: %+v %v", id, r, err)
		}
		for end := time.Now().Add(30 * time.Second); time.Now().Before(end) && !api.saw("[pyre-msg:"+id); time.Sleep(200 * time.Millisecond) {
		}
		if !api.saw("[pyre-msg:" + id) {
			t.Fatalf("%s never reached the model request", id)
		}
		waitIdle()
	}
	api.mu.Lock()
	for _, u := range api.users {
		t.Logf("model saw: %s", oneLine(u, 240))
	}
	api.mu.Unlock()
	_, _ = a.Stop(ctx, h)
}
