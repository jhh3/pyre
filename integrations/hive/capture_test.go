package hive

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// capRunner records `hive mem capture` invocations and delegates the rest
// (detection) to the fake Hive.
type capRunner struct {
	inner     Runner
	mu        sync.Mutex
	calls     []Cmd
	exit      int
	onCapture func(Cmd)
}

func (r *capRunner) Run(ctx context.Context, c Cmd) (Result, error) {
	if len(c.Args) > 1 && c.Args[0] == "mem" && c.Args[1] == "capture" {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.calls = append(r.calls, c)
		if r.onCapture != nil {
			r.onCapture(c)
		}
		return Result{ExitCode: r.exit, Stderr: []byte("synthetic capture failure\n")}, nil
	}
	return r.inner.Run(ctx, c)
}

func (r *capRunner) n() int { r.mu.Lock(); defer r.mu.Unlock(); return len(r.calls) }

func argAfter(args []string, flag string) string {
	if i := slices.Index(args, flag); i >= 0 && i+1 < len(args) {
		return args[i+1]
	}
	return ""
}

const synthThread = "019a0000-aaaa-7bbb-8ccc-000000000001"

func writeRollout(t *testing.T, home, day, thread, body string) string {
	t.Helper()
	p := filepath.Join(home, "sessions", day, "rollout-2026-10-05T08-00-00-"+thread+".jsonl")
	must(t, os.MkdirAll(filepath.Dir(p), 0o700))
	must(t, os.WriteFile(p, []byte(body), 0o600))
	return p
}

func TestCodexRollout(t *testing.T) {
	home := t.TempDir()
	if _, err := CodexRollout(home, synthThread); err == nil {
		t.Fatal("missing rollout must be an error")
	}
	writeRollout(t, home, "2026/10/04", synthThread, "{}\n")
	want := writeRollout(t, home, "2026/10/05", synthThread, "{}\n")
	writeRollout(t, home, "2026/10/05", "019a0000-aaaa-7bbb-8ccc-000000000002", "{}\n")
	got, err := CodexRollout(home, synthThread)
	if err != nil || got != want {
		t.Fatalf("rollout = %q %v (want the newest, %q)", got, err, want)
	}
	for _, bad := range []string{"", "../x", "a*b", "x/y"} {
		if _, err := CodexRollout(home, bad); err == nil {
			t.Fatalf("thread id %q accepted", bad)
		}
	}
}

// TestCapturerHandsRolloutToHive: the capturer runs Hive's capture verb with
// the Codex thread as the session (the run's native reference, so hits are
// attributed to the Pyre session), merges duplicates, skips an unchanged
// transcript, reports failures, and always releases holds (Done).
func TestCapturerHandsRolloutToHive(t *testing.T) {
	f := newFakeHome(t, true, true)
	rr := &capRunner{inner: ExecRunner{}}
	env := f.env()
	env.Runner = rr
	c := New(env, Options{StatusTTL: time.Hour})
	codexHome := t.TempDir()
	path := writeRollout(t, codexHome, "2026/10/05", synthThread, `{"type":"session_meta","payload":{"id":"`+synthThread+`"}}`+"\n"+`{"type":"event_msg","payload":{"type":"user_message","message":"synthetic"}}`+"\n")
	cwd := t.TempDir()
	k := NewCapturer(c, 2, t.Logf)
	done := 0
	inc := func() { done++ }
	if !k.Enqueue(CaptureRequest{Transcript: path, SessionID: synthThread, Cwd: cwd, Reason: "pyre-idle", Env: []string{"CODEX_HOME=" + codexHome}, Done: inc}) {
		t.Fatal("enqueue failed")
	}
	// A duplicate merges (one subprocess, both holds released) and upgrades the reason.
	k.Enqueue(CaptureRequest{Transcript: path, SessionID: synthThread, Cwd: cwd, Reason: "pyre-run-end", Done: inc})
	k.Drain(context.Background())
	if rr.n() != 1 || done != 2 {
		t.Fatalf("captures=%d done=%d", rr.n(), done)
	}
	a := rr.calls[0]
	if argAfter(a.Args, "--transcript") != path || argAfter(a.Args, "--session") != synthThread || argAfter(a.Args, "--cwd") != cwd ||
		argAfter(a.Args, "--reason") != "pyre-run-end" || a.Dir != cwd || a.Timeout != env.WriteTimeout {
		t.Fatalf("capture call = %+v", a)
	}
	if !slices.Contains(a.Env, "CODEX_HOME="+codexHome) || !slices.Contains(a.Env, "HIVE_NO_CAPTURE=1") {
		t.Fatalf("capture env = %v", a.Env)
	}
	// Unchanged transcript: skipped without a subprocess.
	k.Enqueue(CaptureRequest{Transcript: path, SessionID: synthThread, Cwd: cwd, Reason: "pyre-idle", Done: inc})
	k.Drain(context.Background())
	if rr.n() != 1 || done != 3 || k.Stats().Skipped != 1 {
		t.Fatalf("unchanged transcript re-captured: calls=%d done=%d %+v", rr.n(), done, k.Stats())
	}
	// Grown transcript, Hive failing: reported, hold still released.
	must(t, os.WriteFile(path, []byte(`{"type":"session_meta"}`+"\n"+`{"type":"event_msg"}`+"\n"), 0o600))
	rr.exit = 1
	k.Enqueue(CaptureRequest{Transcript: path, SessionID: synthThread, Cwd: cwd, Reason: "pyre-idle", Done: inc})
	k.Drain(context.Background())
	if st := k.Stats(); st.Failed != 1 || !strings.Contains(st.LastError, "synthetic capture failure") || done != 4 {
		t.Fatalf("failure not reported: %+v done=%d", st, done)
	}
	// Full queue: dropped, hold released at once.
	p2 := writeRollout(t, codexHome, "2026/10/05", "019a0000-aaaa-7bbb-8ccc-000000000002", "{}\n")
	p3 := writeRollout(t, codexHome, "2026/10/05", "019a0000-aaaa-7bbb-8ccc-000000000003", "{}\n")
	p4 := writeRollout(t, codexHome, "2026/10/05", "019a0000-aaaa-7bbb-8ccc-000000000004", "{}\n")
	k.Enqueue(CaptureRequest{Transcript: p2, SessionID: "019a0000-aaaa-7bbb-8ccc-000000000002"})
	k.Enqueue(CaptureRequest{Transcript: p3, SessionID: "019a0000-aaaa-7bbb-8ccc-000000000003"})
	if k.Enqueue(CaptureRequest{Transcript: p4, SessionID: "019a0000-aaaa-7bbb-8ccc-000000000004", Done: inc}) || done != 5 || k.Stats().Dropped != 1 {
		t.Fatalf("full queue: done=%d %+v", done, k.Stats())
	}
	// Run exits with ctx and releases what is still queued.
	ctx, cancel := context.WithCancel(context.Background())
	k.Enqueue(CaptureRequest{Transcript: p2, SessionID: "019a0000-aaaa-7bbb-8ccc-000000000002", Done: inc})
	cancel()
	k.Run(ctx)
	if done != 6 {
		t.Fatalf("queued hold not released on shutdown: done=%d", done)
	}
}

func TestCaptureTranscriptRefusesBadInput(t *testing.T) {
	f := newFakeHome(t, true, true)
	c := f.client()
	if _, err := c.CaptureTranscript(context.Background(), CaptureRequest{Transcript: "relative.jsonl", SessionID: synthThread}); err == nil {
		t.Fatal("relative transcript accepted")
	}
	if _, err := c.CaptureTranscript(context.Background(), CaptureRequest{Transcript: "/nonexistent/x.jsonl", SessionID: "--json"}); err == nil {
		t.Fatal("flag-like session accepted")
	}
	noHive := newFakeHome(t, false, true).client()
	p := writeRollout(t, t.TempDir(), "2026/10/05", synthThread, "{}\n")
	if _, err := noHive.CaptureTranscript(context.Background(), CaptureRequest{Transcript: p, SessionID: synthThread}); err == nil {
		t.Fatal("capture without a Hive CLI must fail")
	}
}

// A Pyre-captured transcript has no Hive session-ledger row naming the
// machine; an attributed hit gets this node's machine.
func TestAttributeAddsLocalMachine(t *testing.T) {
	c := New(EnvFrom(nil), Options{})
	c.SetNativeResolver(func(_ context.Context, id string) (NativeSession, bool) {
		return NativeSession{SessionID: "ses_synth-codex", RunID: "run_synth-codex"}, id == synthThread
	})
	h := contracts.MemoryHit{Kind: "transcript", SessionRef: synthThread, Source: "hive session=" + synthThread + " uid=" + synthThread}
	c.attribute(context.Background(), &h, "synth-node")
	if !strings.HasPrefix(h.Source, "hive machine=synth-node session=") || !strings.HasSuffix(h.Source, "pyre_session=ses_synth-codex pyre_run=run_synth-codex") {
		t.Fatalf("source = %q", h.Source)
	}
	o := contracts.MemoryHit{Kind: "transcript", SessionRef: "other", Source: "hive session=other uid=other"}
	c.attribute(context.Background(), &o, "synth-node")
	if o.Source != "hive session=other uid=other" {
		t.Fatalf("unattributed hit changed: %q", o.Source)
	}
}

// A synthetic codex-0.160-shaped rollout (item_completed items, custom
// exec tool call), as Pyre's app-server threads write them.
const synthRollout160 = `{"timestamp":"2026-10-05T08:00:00.000Z","type":"session_meta","payload":{"id":"` + synthThread + `","cwd":"/synthetic/work","originator":"pyre","cli_version":"0.160.0"}}
{"timestamp":"2026-10-05T08:00:01.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"synthetic prompt"}]}}
{"timestamp":"2026-10-05T08:00:01.000Z","type":"event_msg","payload":{"type":"item_completed","item":{"type":"UserMessage","content":[{"type":"text","text":"synthetic prompt: read notes"}]}}}
{"timestamp":"2026-10-05T08:00:02.000Z","type":"response_item","payload":{"type":"custom_tool_call","name":"exec","input":"const r = await tools.exec_command({cmd:\"cat notes.txt\"});"}}
{"timestamp":"2026-10-05T08:00:03.000Z","type":"event_msg","payload":{"type":"item_completed","item":{"type":"CommandExecution","parsed_cmd":[{"type":"read","cmd":"cat notes.txt"}],"status":"completed"}}}
{"timestamp":"2026-10-05T08:00:04.000Z","type":"event_msg","payload":{"type":"item_completed","item":{"type":"Reasoning","summary_text":[]}}}
{"timestamp":"2026-10-05T08:00:05.000Z","type":"event_msg","payload":{"type":"item_completed","item":{"type":"AgentMessage","content":[{"type":"Text","text":"synthetic answer"}],"phase":"final_answer"}}}
`

func TestNormalizeCodexRollout(t *testing.T) {
	var out strings.Builder
	n, err := NormalizeCodexRollout(strings.NewReader(synthRollout160), &out)
	if err != nil || n != 3 {
		t.Fatalf("added %d lines: %v", n, err)
	}
	got := out.String()
	if !strings.HasPrefix(got, synthRollout160[:strings.Index(synthRollout160, "\n")+1]) {
		t.Fatal("original lines must be kept verbatim and in order")
	}
	for _, want := range []string{`"type":"user_message","message":"synthetic prompt: read notes"`, `"type":"agent_message","message":"synthetic answer"`,
		`"name":"exec_command","arguments":"{\"cmd\":\"cat notes.txt\"}"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("normalized rollout lacks %s:\n%s", want, got)
		}
	}
	p := filepath.Join(t.TempDir(), "r.jsonl")
	must(t, os.WriteFile(p, []byte(synthRollout160), 0o600))
	if need, err := CodexRolloutNeedsShim(p); err != nil || !need {
		t.Fatalf("0.160 rollout needs the shim: %v %v", need, err)
	}
	must(t, os.WriteFile(p, []byte(got), 0o600))
	if need, _ := CodexRolloutNeedsShim(p); need {
		t.Fatal("a rollout with legacy events must not be copied again")
	}
	must(t, os.WriteFile(p, []byte(`{"type":"user","message":{"role":"user"}}`+"\n"), 0o600))
	if need, _ := CodexRolloutNeedsShim(p); need {
		t.Fatal("a Claude transcript is not a codex rollout")
	}
}

// TestCaptureStagesCodex160Rollout: Hive is handed a private, normalized
// copy of a codex-0.160 rollout, removed after the call.
func TestCaptureStagesCodex160Rollout(t *testing.T) {
	f := newFakeHome(t, true, true)
	var seen string
	rr := &capRunner{inner: ExecRunner{}, onCapture: func(c Cmd) {
		b, _ := os.ReadFile(argAfter(c.Args, "--transcript"))
		seen = string(b)
	}}
	env := f.env()
	env.Runner = rr
	staging := t.TempDir()
	c := New(env, Options{StatusTTL: time.Hour, StagingRoot: staging})
	p := writeRollout(t, t.TempDir(), "2026/10/05", synthThread, synthRollout160)
	if _, err := c.CaptureTranscript(context.Background(), CaptureRequest{Transcript: p, SessionID: synthThread, Reason: "pyre-run-end"}); err != nil {
		t.Fatal(err)
	}
	staged := argAfter(rr.calls[0].Args, "--transcript")
	if staged == p || !strings.HasPrefix(staged, filepath.Join(staging, "capture")) || !strings.Contains(seen, `"user_message"`) {
		t.Fatalf("hive got %s (normalized=%v)", staged, strings.Contains(seen, `"user_message"`))
	}
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Fatal("staging copy not removed after the capture")
	}
	if fi, _ := os.Stat(filepath.Join(staging, "capture")); fi.Mode().Perm() != 0o700 {
		t.Fatalf("staging dir mode %v", fi.Mode().Perm())
	}
}
