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

// The StopFailure payloads real Claude Code 2.1.304 wrote on office-box2 on
// 2026-10-24 when Team seats hit their spend limits (message verbatim, ids
// and paths synthetic). Pyre read them only as "rate_limited" through the
// error code (iss_…).
const (
	realStopFailureIndividualSpend = `{"cwd":"/home/synth/work","error":"rate_limit","hook_event_name":"StopFailure",` +
		`"last_assistant_message":"You've hit your individual spend limit · ask your admin to raise it at https://claude.ai/admin-settings/usage · your session limit resets 1am (UTC)",` +
		`"prompt_id":"00000000-0000-4000-8000-000000000003","session_id":"00000000-0000-4000-8000-000000000004"}`
	realStopFailureOrgSpend = `{"cwd":"/home/synth/work","error":"rate_limit","hook_event_name":"StopFailure",` +
		`"last_assistant_message":"You've hit your org's monthly spend limit · ask your admin to raise it at https://claude.ai/admin-settings/usage · your session limit resets 2:20am (UTC)",` +
		`"prompt_id":"00000000-0000-4000-8000-000000000005","session_id":"00000000-0000-4000-8000-000000000006"}`
)

func TestDetectLimitSpendLimits(t *testing.T) {
	at := time.Date(2026, 10, 24, 23, 31, 26, 0, time.UTC)
	for _, c := range []struct {
		text  string
		reset time.Time
	}{
		{realStopFailureIndividualSpend, time.Date(2026, 10, 25, 1, 0, 0, 0, time.UTC)},
		{realStopFailureOrgSpend, time.Date(2026, 10, 25, 2, 20, 0, 0, time.UTC)},
	} {
		le := detectLimit(c.text, at)
		if le == nil || le.Kind != "usage_limit" {
			t.Fatalf("%q -> %+v, want usage_limit", c.text, le)
		}
		if le.ResetAt == nil || !le.ResetAt.Equal(c.reset) {
			t.Fatalf("reset %v, want %v", le.ResetAt, c.reset)
		}
		if !strings.Contains(le.Detail, "spend limit") {
			t.Fatalf("detail %q", le.Detail)
		}
	}
	// Claude's other limit-message shapes (2.1.304 bundle strings).
	for _, text := range []string{
		"You've hit your channel's monthly spend limit · your session limit resets 9pm (UTC)",
		"You’ve hit your individual usage limit · resets 4am (UTC)",
		"You've hit your monthly spend limit · your session limit resets 1am (UTC)",
		"You've hit your team's shared budget. /model to switch models.",
		"You've reached your Fable limit.",
		"You're out of usage credits. /model to switch models.",
		"Your org is out of usage · add funds to continue",
		"Your group's usage limit is set to $0",
		"Your usage allocation has been disabled by your admin",
		"Your credit balance is too low to access the Anthropic API.",
	} {
		if le := detectLimit(text, at); le == nil || le.Kind != "usage_limit" {
			t.Fatalf("%q -> %+v, want usage_limit", text, le)
		}
	}
	// Not limits.
	for _, text := range []string{
		"Updating spend limit…",
		"The spend limit request goes to the gateway",
		"You set a spend limit of $50 earlier; nothing else changed.",
	} {
		if le := detectLimit(text, at); le != nil {
			t.Fatalf("%q -> %+v, want none", text, le)
		}
	}
}

func TestStopFailureHookReportsSpendLimit(t *testing.T) {
	rs := newRunState()
	at := time.Date(2026, 10, 24, 23, 31, 26, 0, time.UTC)
	rs.applyHook(hookRecord{Event: "StopFailure", At: at, Payload: []byte(realStopFailureIndividualSpend)})
	if rs.limit == nil || rs.limit.Kind != "usage_limit" || rs.limit.ResetAt == nil || !rs.limit.ResetAt.Equal(time.Date(2026, 10, 25, 1, 0, 0, 0, time.UTC)) {
		t.Fatalf("%+v", rs.limit)
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
	h2 := f.launchIn("run_xb", profB, pluginEnv(), ref, prompt)
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
