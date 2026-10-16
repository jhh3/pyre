package claude

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// The StopFailure payload real Claude Code wrote on server when a token
// account hit its 5-hour limit (2026-10-08; field values verbatim, ids
// synthetic). Pyre did not recognise it, so no fallback ever ran.
const realStopFailureLimit = `{"cwd":"/home/synth/work","error":"rate_limit","hook_event_name":"StopFailure",` +
	`"last_assistant_message":"You've hit your session limit · resets 3pm (America/Los_Angeles)",` +
	`"prompt_id":"00000000-0000-4000-8000-000000000001","session_id":"00000000-0000-4000-8000-000000000002"}`

func TestDetectLimitRealClaudeTexts(t *testing.T) {
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Skip("no tz database")
	}
	at := time.Date(2026, 10, 8, 13, 43, 0, 0, la) // 1:43pm PDT
	le := detectLimit(realStopFailureLimit, at.UTC())
	if le == nil || le.Kind != "usage_limit" {
		t.Fatalf("%+v", le)
	}
	if want := time.Date(2026, 10, 8, 15, 0, 0, 0, la).UTC(); le.ResetAt == nil || !le.ResetAt.Equal(want) {
		t.Fatalf("reset %v, want %v", le.ResetAt, want)
	}
	if le.Detail != "rate_limit: You've hit your session limit · resets 3pm (America/Los_Angeles)" {
		t.Fatalf("detail %q", le.Detail)
	}
	// After 3pm the reset is tomorrow's.
	if le := detectLimit(realStopFailureLimit, time.Date(2026, 10, 8, 16, 0, 0, 0, la)); le.ResetAt == nil ||
		!le.ResetAt.Equal(time.Date(2026, 10, 9, 15, 0, 0, 0, la).UTC()) {
		t.Fatalf("next-day reset %+v", le.ResetAt)
	}
	for _, c := range []struct{ text, kind string }{
		{"⎿  You’ve hit your weekly limit · resets Oct 10, 9am (Europe/Berlin)", "usage_limit"},
		{"You've hit your Opus limit · resets 5:30pm", "usage_limit"},
		{`{"error":"rate_limit","last_assistant_message":"API Error: Request rejected (429)"}`, "rate_limited"},
		{`{"error":"authentication_failed","last_assistant_message":"Invalid API key · Please run /login"}`, "auth_invalid"},
		{`{"error":"billing_error","last_assistant_message":"Credit balance too low"}`, "usage_limit"},
		{`{"error":"server_error","last_assistant_message":"API Error: 500"}`, ""},
	} {
		le := detectLimit(c.text, at)
		if c.kind == "" && le != nil || c.kind != "" && (le == nil || le.Kind != c.kind) {
			t.Fatalf("%q -> %+v, want %q", c.text, le, c.kind)
		}
	}
	berlin, _ := time.LoadLocation("Europe/Berlin")
	if le := detectLimit("You’ve hit your weekly limit · resets Oct 10, 9am (Europe/Berlin)", at); le.ResetAt == nil ||
		!le.ResetAt.Equal(time.Date(2026, 10, 10, 9, 0, 0, 0, berlin).UTC()) {
		t.Fatalf("dated reset %+v", le.ResetAt)
	}
}

func TestStopFailureHookReportsSessionLimit(t *testing.T) {
	rs := newRunState()
	rs.applyHook(hookRecord{Event: "StopFailure", At: time.Now(), Payload: []byte(realStopFailureLimit)})
	if rs.limit == nil || rs.limit.Kind != "usage_limit" || rs.limit.ResetAt == nil {
		t.Fatalf("%+v", rs.limit)
	}
}

// Cross-account native resume with the fake Claude: two account profiles
// whose projects/ link to one shared store; the run launched under A
// resumes the same session (same uuid, its transcript) under B, with the
// "continue" prompt submitted as the first turn.
func TestFakeCrossAccountResumeSharedStore(t *testing.T) {
	t.Parallel()
	f := newFakeEnv(t, nil)
	if !contracts.HasCap(f.a.Capabilities(), contracts.CapAccountCrossResume) {
		t.Fatal("cross-account resume not advertised with ProfileAsConfigDir")
	}
	shared := filepath.Join(f.dir, "shared", "projects")
	if err := os.MkdirAll(shared, 0o700); err != nil {
		t.Fatal(err)
	}
	profA, profB := filepath.Join(f.dir, "profA"), filepath.Join(f.dir, "profB")
	for _, p := range []string{profA, profB} {
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(shared, filepath.Join(p, "projects")); err != nil {
			t.Fatal(err)
		}
	}
	h := f.launchIn("run_xa", profA, nil, nil, "")
	f.waitState(f.a, h, contracts.RunWaitingInput, 15*time.Second)
	deliver(t, f.a, h, "msg_xa", "remember the synthetic plan", contracts.ModeQueue)
	f.waitState(f.a, h, contracts.RunWaitingInput, 15*time.Second)
	ctx := context.Background()
	if _, err := f.a.Stop(ctx, h); err != nil {
		t.Fatal(err)
	}
	obs, _ := f.a.Observe(ctx, h, "")
	kind, ref, err := f.a.ResumeSpec(ctx, contracts.Run{RunID: h.RunID, NativeRef: obs.NativeRef}, true)
	if err != nil || kind != contracts.ContinuationNativeResume {
		t.Fatalf("resume spec %v %v", kind, err)
	}
	prompt := "[pyre] Continue where you left off (account switched from synth-a to synth-b because of the usage limit)."
	h2 := f.launchIn("run_xb", profB, nil, ref, prompt)
	obs = f.waitObs(f.a, h2, 15*time.Second, func(o contracts.Observation) bool {
		// The continue prompt runs as the first turn and ends (Stop).
		return o.State == contracts.RunWaitingInput && o.NativeRef != nil && strings.Contains(o.StateReason, "Stop")
	})
	if obs.NativeRef.ID != h.NativeRef.ID {
		t.Fatalf("resumed a different session: %s vs %s", obs.NativeRef.ID, h.NativeRef.ID)
	}
	// One transcript (the store's), holding A's turn and B's continue prompt.
	tx := filepath.Join(profB, "projects", projectSlug(mustEval(t, filepath.Join(f.dir, "work"))), h.NativeRef.ID+".jsonl")
	b, err := os.ReadFile(tx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "remember the synthetic plan") || !strings.Contains(string(b), "Continue where you left off") {
		t.Fatalf("shared transcript lacks the turns:\n%s", b)
	}
	// Without the shared store the fake (like Claude) finds no conversation.
	profC := filepath.Join(f.dir, "profC")
	_ = os.MkdirAll(profC, 0o700)
	if _, err := os.Stat(filepath.Join(profC, "projects", projectSlug(mustEval(t, filepath.Join(f.dir, "work"))), h.NativeRef.ID+".jsonl")); err == nil {
		t.Fatal("unshared profile sees the transcript")
	}
}

func mustEval(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
