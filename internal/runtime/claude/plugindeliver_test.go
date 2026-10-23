package claude

// Plugin prompt delivery against the fake Claude. With FAKE_PLUGIN_HOST=1
// the fake plays Claude Code's plugin host for the generated hooks module:
// it reads the module's run dir and hook command from hooks/register.ts and
// does what the module does (heartbeat, claim through the hook command,
// submit as a turn of its own when idle, report). The module's own logic
// runs under Claude Code's plugin test kit in
// claudeplugin.TestModuleUnderClaudePluginTest.

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/claudeplugin"
	"github.com/jhh3/pyre/internal/runtime/tmux"
)

var (
	modRunDirRe = regexp.MustCompile(`const RUN_DIR: string = (".*")`)
	modHookRe   = regexp.MustCompile(`const HOOK: readonly string\[\] = (\[.*\])`)
)

func (f *fakeClaude) pluginHost(dir string) {
	src, err := os.ReadFile(filepath.Join(dir, "hooks", "register.ts"))
	if err != nil {
		return
	}
	var runDir string
	var hook []string
	if m := modRunDirRe.FindSubmatch(src); m != nil {
		_ = json.Unmarshal(m[1], &runDir)
	}
	if m := modHookRe.FindSubmatch(src); m != nil {
		_ = json.Unmarshal(m[1], &hook)
	}
	if runDir == "" || len(hook) == 0 {
		return
	}
	emit := func(ev string, payload map[string]string) string {
		argv := append(append([]string{}, hook...), ev, runDir)
		c := exec.Command(argv[0], argv[1:]...)
		b, _ := json.Marshal(payload)
		c.Stdin = strings.NewReader(string(b))
		out, _ := c.Output()
		return string(out)
	}
	alive := func() {
		_ = os.WriteFile(filepath.Join(runDir, claudeplugin.AliveFile), []byte(`{"version":"fake"}`+"\n"), 0o600)
	}
	alive()
	emit(claudeplugin.EvPluginReady, map[string]string{"version": "fake"})
	tick := time.NewTicker(150 * time.Millisecond)
	last := time.Now()
	for range tick.C {
		if time.Since(last) > time.Second {
			alive()
			last = time.Now()
		}
		f.mu.Lock()
		busy := f.busy || f.dialog != ""
		f.mu.Unlock()
		if busy {
			continue
		}
		ents, _ := os.ReadDir(filepath.Join(runDir, claudeplugin.SpoolDir))
		var names []string
		for _, e := range ents {
			if strings.HasSuffix(e.Name(), ".msg") {
				names = append(names, e.Name())
			}
		}
		sort.Strings(names)
		for _, n := range names {
			out := emit(claudeplugin.EvPluginClaim, map[string]string{"name": n})
			var m struct {
				MessageID string `json:"message_id"`
				AttemptID string `json:"attempt_id"`
				Text      string `json:"text"`
			}
			if json.Unmarshal([]byte(out), &m) != nil || m.Text == "" {
				continue
			}
			ids := map[string]string{"name": n, "message_id": m.MessageID, "attempt_id": m.AttemptID}
			if strings.Contains(m.Text, "FAKE_PLUGIN_DROP") {
				ids["reason"] = "synthetic prompt hook refusal"
				emit(claudeplugin.EvPluginDropped, ids)
				continue
			}
			f.submit(m.Text) // a turn of its own; the input box is untouched
			emit(claudeplugin.EvPluginSubmitted, ids)
			break
		}
	}
}

func pluginEnv() map[string]string { return map[string]string{"FAKE_PLUGIN_HOST": "1"} }

func waitPluginAlive(t *testing.T, f *fakeEnv, h contracts.RunHandle) {
	t.Helper()
	end := time.Now().Add(10 * time.Second)
	for time.Now().Before(end) {
		if f.a.pluginLive(h) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("plugin module never reported alive")
}

func transcriptOrder(t *testing.T, a *Adapter, h contracts.RunHandle) []string {
	resp, err := a.ReadTranscript(context.Background(), h, contracts.ReadSessionRequest{Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, e := range resp.Entries {
		if e.MessageID != "" {
			ids = append(ids, e.MessageID)
		}
	}
	return ids
}

func TestFakePluginDeliveryOrderedAndIdempotent(t *testing.T) {
	t.Parallel()
	f := newFakeEnv(t, nil)
	h := f.launch("run_plug", pluginEnv(), nil)
	if h.Data["plugin_delivery"] != "on" {
		t.Fatalf("handle data %v", h.Data)
	}
	f.waitState(f.a, h, contracts.RunWaitingInput, 15*time.Second)
	waitPluginAlive(t, f, h)
	for _, id := range []string{"msg_p1", "msg_p2", "msg_p3"} {
		if ok, why := f.a.InputReady(context.Background(), h, contracts.ModeQueue); !ok {
			t.Fatalf("%s not ready: %s", id, why)
		}
		r := deliver(t, f.a, h, id, "[pyre-msg:"+id+" · sent 3:05 PM PT (8h ago)] from session:synthetic:\nbody "+id, contracts.ModeQueue)
		if r.State != contracts.DeliveryAccepted || r.Evidence != contracts.EvidenceClaudePluginPrompt {
			t.Fatalf("%s: %+v", id, r)
		}
		f.waitState(f.a, h, contracts.RunWaitingInput, 15*time.Second)
	}
	if got := strings.Join(transcriptOrder(t, f.a, h), ","); got != "msg_p1,msg_p2,msg_p3" {
		t.Fatalf("order %s", got)
	}
	if last := lastAttempts(h.StateDir)["msg_p2"]; last.Mode != "plugin" || last.State != jAccepted {
		t.Fatalf("journal %+v", last)
	}
	// Already accepted: never re-spooled.
	r := deliver(t, f.a, h, "msg_p1", "again", contracts.ModeQueue)
	if r.State != contracts.DeliveryAccepted || !strings.Contains(r.Detail, "already accepted") {
		t.Fatalf("redeliver %+v", r)
	}
	if n := countUserRecords(t, f.a, h, "msg_p1"); n != 1 {
		t.Fatalf("user records for msg_p1: %d", n)
	}
}

func TestFakePluginDeliveryWhileHumanAttachedAndTyping(t *testing.T) {
	t.Parallel()
	f := newFakeEnv(t, nil)
	h := f.launch("run_plugatt", pluginEnv(), nil)
	f.waitState(f.a, h, contracts.RunWaitingInput, 15*time.Second)
	waitPluginAlive(t, f, h)
	ctx := context.Background()
	d, err := f.a.Attach(ctx, h, contracts.AttachCommand{RunID: h.RunID})
	if err != nil {
		t.Fatal(err)
	}
	outer, _ := tmux.NewServer("", filepath.Join(f.dir, "o.sock"))
	defer outer.KillServer(ctx)
	if err := outer.NewSession(ctx, tmux.SessionSpec{Name: "human", Argv: d.Command}); err != nil {
		t.Fatal(err)
	}
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		if cs, _ := f.a.srv.ListClients(ctx, h.TmuxTarget); tmux.WritableClients(cs) == 1 {
			break
		}
	}
	// The owner has a half-typed draft in the box.
	if _, err := f.a.srv.(*tmux.Server).Run(ctx, nil, "send-keys", "-t", h.TmuxTarget, "-l", "owner half typed"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	r := deliver(t, f.a, h, "msg_att", "[pyre-msg:msg_att] from owner:synthetic:\nwhile attached", contracts.ModeQueue)
	if r.State != contracts.DeliveryAccepted || r.Evidence != contracts.EvidenceClaudePluginPrompt {
		t.Fatalf("attached: %+v", r)
	}
	f.waitState(f.a, h, contracts.RunWaitingInput, 15*time.Second)
	si, _ := f.a.screen(ctx, h.TmuxTarget)
	if !strings.Contains(strings.Join(si.InputLines, "\n"), "owner half typed") {
		t.Fatalf("owner's draft lost: %q", si.InputLines)
	}
}

func TestFakePluginBusyWithdrawsAndDrop(t *testing.T) {
	t.Parallel()
	// The default DeliverTimeout: deliveries to the idle session wait for
	// the module's next poll and its report, which a loaded runner slows.
	// Only the attempt during the long turn is cut short (its context).
	f := newFakeEnv(t, nil)
	h := f.launch("run_plugbusy", pluginEnv(), nil)
	f.waitState(f.a, h, contracts.RunWaitingInput, 15*time.Second)
	waitPluginAlive(t, f, h)
	ctx := context.Background()
	// A long turn: the engine is told to wait, and a direct attempt is
	// withdrawn unsent (never claimed while the turn runs).
	if r := deliver(t, f.a, h, "msg_long", "FAKE_SLEEP_MS=8000 work", contracts.ModeQueue); r.State != contracts.DeliveryAccepted {
		t.Fatalf("long: %+v", r)
	}
	f.waitState(f.a, h, contracts.RunRunning, 3*time.Second)
	if ok, why := f.a.InputReady(ctx, h, contracts.ModeQueue); ok || !strings.Contains(why, "plugin delivery waits") {
		t.Fatalf("ready while busy: %v %s", ok, why)
	}
	bctx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond) // well inside the 8 s turn, even when its acceptance was slow
	r, err := f.a.Deliver(bctx, h, contracts.DeliveryInput{MessageID: "msg_wait", AttemptID: "dlv_msg_wait", Mode: contracts.ModeQueue, Text: "after the turn"})
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	if r.State != contracts.DeliveryQueued || !strings.Contains(r.Detail, "withdrawn") {
		t.Fatalf("busy: %+v", r)
	}
	if last := lastAttempts(h.StateDir)["msg_wait"]; last.State != jRejected {
		t.Fatalf("journal %+v", last)
	}
	f.waitState(f.a, h, contracts.RunWaitingInput, 15*time.Second)
	if r := deliver(t, f.a, h, "msg_wait", "after the turn", contracts.ModeQueue); r.State != contracts.DeliveryAccepted {
		t.Fatalf("after turn: %+v", r)
	}
	if n := countUserRecords(t, f.a, h, "msg_wait"); n != 1 {
		t.Fatalf("user records %d", n)
	}
	f.waitState(f.a, h, contracts.RunWaitingInput, 10*time.Second)
	// A prompt hook refusing the plugin's prompt: rejected, not entered.
	r = deliver(t, f.a, h, "msg_drop", "FAKE_PLUGIN_DROP", contracts.ModeQueue)
	if r.State != contracts.DeliveryRejected || !strings.Contains(r.Detail, "dropped") {
		t.Fatalf("drop: %+v", r)
	}
	if rr, _ := f.a.Reconcile(ctx, h, "msg_drop"); rr.State != contracts.DeliveryRejected {
		t.Fatalf("reconcile drop: %+v", rr)
	}
}

func TestFakePluginFallsBackToPaste(t *testing.T) {
	t.Parallel()
	// Claude without the module API (or the plugin not loaded): no
	// heartbeat, so delivery pastes as before.
	f := newFakeEnv(t, nil)
	h := f.launch("run_plugnone", nil, nil)
	f.waitState(f.a, h, contracts.RunWaitingInput, 15*time.Second)
	time.Sleep(500 * time.Millisecond)
	if f.a.pluginLive(h) {
		t.Fatal("plugin reported alive without a host")
	}
	r := deliver(t, f.a, h, "msg_paste", "pasted", contracts.ModeQueue)
	if r.State != contracts.DeliveryAccepted || r.Evidence != contracts.EvidenceClaudePromptHook {
		t.Fatalf("fallback: %+v", r)
	}
	if last := lastAttempts(h.StateDir)["msg_paste"]; last.Mode != "paste" {
		t.Fatalf("journal %+v", last)
	}
	// Disabled: no module in the plugin, no capability.
	g := newFakeEnv(t, func(o *Options) { o.DisablePluginDelivery = true })
	h2 := g.launch("run_plugoff", pluginEnv(), nil)
	if _, err := os.Stat(filepath.Join(h2.StateDir, "pyre-plugin", "hooks", "register.ts")); err == nil {
		t.Fatal("module generated while disabled")
	}
	for _, c := range g.a.Capabilities() {
		if c == contracts.CapInputPlugin {
			t.Fatal("capability advertised while disabled")
		}
	}
}

func TestMarkerRegexAcceptsDatedHeader(t *testing.T) {
	got := findMarkers("[pyre-msg:msg_x · sent Oct 6, 3:05 PM PT (8h ago)] from session:planner:\nhi [pyre-msg:msg_y]")
	if strings.Join(got, ",") != "msg_x,msg_y" {
		t.Fatalf("markers %v", got)
	}
	if renderInput(contracts.DeliveryInput{MessageID: "msg_x", Text: "[pyre-msg:msg_x · sent 3:05 PM PT (just now)] from owner:o:\nhi"}) !=
		"[pyre-msg:msg_x · sent 3:05 PM PT (just now)] from owner:o:\nhi" {
		t.Fatal("dated header got a second marker")
	}
}
