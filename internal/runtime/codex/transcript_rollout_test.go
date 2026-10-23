package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// syntheticRollout is a trimmed rollout of a launch prompt answered with
// "ok" (the shape Codex 0.163 wrote on Windows, Oct 2026), CRLF endings.
func syntheticRollout(thread string) string {
	lines := []string{
		`{"timestamp":"2026-10-21T04:48:51.191Z","type":"session_meta","payload":{"id":"` + thread + `"}}`,
		`{"timestamp":"2026-10-21T04:48:51.457Z","type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":"# Pyre\n\nThis session is managed by Pyre."}]}}`,
		`{"timestamp":"2026-10-21T04:48:51.457Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>\n  <cwd>C:\\Users\\synth</cwd>\n</environment_context>"}]}}`,
		`{"timestamp":"2026-10-21T04:48:51.498Z","type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":"Pyre session ses_synth (run run_synth) started."}]}}`,
		`{"timestamp":"2026-10-21T04:48:51.556Z","type":"event_msg","payload":{"type":"task_started"}}`,
		`{"timestamp":"2026-10-21T04:48:51.734Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Reply with just: ok"}]}}`,
		`{"timestamp":"2026-10-21T04:48:52.100Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"[pyre-msg:msg_01SYNTH · sent 9:48 PM PT (now)] from session:parent\nalso say done"}]}}`,
		`{"timestamp":"2026-10-21T04:48:52.200Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"[pyre] background command finished: ` + "`sleep 1`" + ` (exit 0)"}]}}`,
		`{"timestamp":"2026-10-21T04:48:53.164Z","type":"response_item","payload":{"type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"ok","annotations":[]}],"phase":"final_answer"}}`,
		`{"timestamp":"2026-10-21T04:48:53.196Z","type":"event_msg","payload":{"type":"task_complete","last_agent_message":"ok"}}`,
	}
	return strings.Join(lines, "\r\n") + "\r\n"
}

// stoppedRun writes the state of a run Pyre stopped (its app-server gone)
// whose rollout lives under codexHome/sessions.
func stoppedRun(t *testing.T, dir, runID, home, rolloutHome, thread string) contracts.RunHandle {
	t.Helper()
	return stoppedRunOn(t, dir, runID, home, rolloutHome, thread, rolloutDay)
}

// rolloutDay is the synthetic rollout's local day (its folder name).
var rolloutDay = time.Date(2026, 10, 20, 21, 48, 50, 0, time.Local)

func stoppedRunOn(t *testing.T, dir, runID, home, rolloutHome, thread string, launched time.Time) contracts.RunHandle {
	t.Helper()
	state := filepath.Join(dir, "runs", runID)
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	mb, _ := json.Marshal(runMeta{RunID: runID, SessionID: "ses_synth", ThreadID: thread, Home: home, LaunchedAt: launched.UTC()})
	if err := os.WriteFile(filepath.Join(state, "run.json"), mb, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "stopped.json"), []byte(`{"stopped":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	day := filepath.Join(rolloutHome, "sessions", rolloutDay.Format("2006"), rolloutDay.Format("01"), rolloutDay.Format("02"))
	if err := os.MkdirAll(day, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(day, "rollout-2026-10-20T21-48-50-"+thread+".jsonl"), []byte(syntheticRollout(thread)), 0o600); err != nil {
		t.Fatal(err)
	}
	return contracts.RunHandle{RunID: runID, SessionID: "ses_synth", Adapter: AdapterName, Harness: contracts.HarnessCodex, StateDir: state}
}

// A stopped run's transcript (no app-server) comes from its rollout file:
// the child report read right after archive-when-done stopped a Codex run
// found "No assistant message" although it had answered.
func TestCodexReadTranscriptAfterStopFromRollout(t *testing.T) {
	e := newEnv(t, nil)
	thread := "01a15cca-0000-7000-8000-00000000synth"
	h := stoppedRun(t, e.dir, "run_rollout", e.home, e.home, thread)
	resp, err := e.a.ReadTranscript(context.Background(), h, contracts.ReadSessionRequest{SessionID: "ses_synth", RunID: h.RunID, Tail: true, Limit: 40})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Freshness.Stale || !strings.Contains(resp.Freshness.Reason, "rollout") {
		t.Fatalf("freshness %+v", resp.Freshness)
	}
	en := resp.Entries
	if len(en) != 4 || en[0].Kind != "user" || en[0].Text != "Reply with just: ok" ||
		en[1].Kind != "pyre_message" || en[1].MessageID != "msg_01SYNTH" ||
		en[2].Kind != "system" || !strings.HasPrefix(en[2].Text, "[pyre] background command finished") ||
		en[3].Kind != "assistant" || en[3].Text != "ok" || en[3].RunID != h.RunID {
		t.Fatalf("entries %+v", en)
	}
	if b, _ := os.ReadFile(filepath.Join(h.StateDir, rolloutPathFile)); !strings.HasSuffix(string(b), thread+".jsonl") {
		t.Fatalf("rollout path not remembered: %q", b)
	}
	if resp.NextCursor != "4" {
		t.Fatalf("cursor %q", resp.NextCursor)
	}
	if more, _ := e.a.ReadTranscript(context.Background(), h, contracts.ReadSessionRequest{RunID: h.RunID, Cursor: resp.NextCursor}); len(more.Entries) != 0 {
		t.Fatalf("after the cursor: %+v", more.Entries)
	}

	// No rollout: stale and empty, as before.
	gone := stoppedRun(t, e.dir, "run_norollout", filepath.Join(e.dir, "home-b"), filepath.Join(e.dir, "elsewhere"), "thr-missing")
	resp, err = e.a.ReadTranscript(context.Background(), gone, contracts.ReadSessionRequest{RunID: gone.RunID, Tail: true})
	if err != nil || len(resp.Entries) != 0 || !resp.Freshness.Stale || strings.Contains(resp.Freshness.Reason, "rollout") {
		t.Fatalf("%+v %v", resp, err)
	}
}

// A run on the node's own login (no Pyre profile, codex_home "") keeps its
// rollout under CODEX_HOME, else ~/.codex.
func TestCodexAmbientRolloutFiles(t *testing.T) {
	e := newEnv(t, nil)
	ambient := filepath.Join(e.dir, "ambient-codex")
	t.Setenv("CODEX_HOME", ambient)
	thread := "01a15cca-0000-7000-8000-0000ambient"
	h := stoppedRun(t, e.dir, "run_ambient", "", ambient, thread)
	if _, files := e.a.TranscriptFiles(h); len(files) != 1 || !strings.HasPrefix(files[0], ambient) {
		t.Fatalf("ambient rollout not found: %v", files)
	}
	resp, _ := e.a.ReadTranscript(context.Background(), h, contracts.ReadSessionRequest{RunID: h.RunID, Tail: true, Limit: 1})
	if len(resp.Entries) != 1 || resp.Entries[0].Text != "ok" {
		t.Fatalf("entries %+v", resp.Entries)
	}
	t.Setenv("CODEX_HOME", "")
	t.Setenv("HOME", e.dir)
	t.Setenv("USERPROFILE", e.dir)
	if got, want := codexHome(""), filepath.Join(e.dir, ".codex"); got != want {
		t.Fatalf("default home %q, want %q", got, want)
	}
}

// A turn that started and ended between two observations is reported
// working once, so the observer sees it end (a 1.6 s launch turn was
// never seen running and its parent was never told).
func TestCodexObserveTurnBetweenObservations(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	h := e.launch(e.a, "run_fast", e.home, nil, nil)
	e.wait(e.a, h, 5*time.Second, idle)
	if o, _ := e.a.Observe(context.Background(), h, ""); o.State != contracts.RunWaitingInput {
		t.Fatalf("idle launch: %+v", o)
	}
	dlv(t, e.a, h, "msg_fast", "reply ok", contracts.ModeQueue)
	rr, _ := e.a.run(h)
	deadline := time.Now().Add(5 * time.Second)
	for {
		rr.ts.mu.Lock()
		done := rr.ts.status.Type == "idle" && rr.ts.turnsStarted == 1
		rr.ts.mu.Unlock()
		if done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the turn did not end")
		}
		time.Sleep(20 * time.Millisecond)
	}
	o, _ := e.a.Observe(context.Background(), h, "")
	if o.State != contracts.RunRunning || !strings.Contains(o.StateReason, "ended since the last observation") {
		t.Fatalf("a turn that ended unseen is reported working once: %+v", o)
	}
	for i := 0; i < 2; i++ {
		if o, _ := e.a.Observe(context.Background(), h, ""); o.State != contracts.RunWaitingInput {
			t.Fatalf("then idle: %+v", o)
		}
	}
}

// The rollout of a thread started on another day (a resumed run) is found
// by the full glob, remembered, and then read without globbing.
func TestCodexRolloutOtherDayRemembered(t *testing.T) {
	e := newEnv(t, nil)
	thread := "01a15cca-0000-7000-8000-0000otherday"
	h := stoppedRunOn(t, e.dir, "run_otherday", e.home, e.home, thread, rolloutDay.AddDate(0, 0, 9))
	if _, files := e.a.TranscriptFiles(h); len(files) != 1 {
		t.Fatalf("not found by the full glob: %v", files)
	}
	// Remembered: found even where no glob would look.
	moved := filepath.Join(e.home, "sessions", "moved", "rollout-x-"+thread+".jsonl")
	_ = os.MkdirAll(filepath.Dir(moved), 0o700)
	if err := os.WriteFile(moved, []byte(syntheticRollout(thread)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.StateDir, rolloutPathFile), []byte(moved), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, files := e.a.TranscriptFiles(h); len(files) != 1 || files[0] != moved {
		t.Fatalf("remembered path not used: %v", files)
	}
	// A remembered path outside the home is ignored.
	if err := os.WriteFile(filepath.Join(h.StateDir, rolloutPathFile), []byte(filepath.Join(e.dir, "x-"+thread+".jsonl")), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, files := e.a.TranscriptFiles(h); len(files) != 1 || !strings.HasPrefix(files[0], e.home) {
		t.Fatalf("%v", files)
	}
}

// A failed turn no observation saw (an automatic capacity continue failing
// at once) is not reported as a turn.
func TestCodexObserveUnseenFailedTurn(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	h := e.launch(e.a, "run_failfast", e.home, nil, nil)
	e.wait(e.a, h, 5*time.Second, idle)
	e.a.Observe(context.Background(), h, "")
	dlv(t, e.a, h, "msg_fail", "FAKE_LIMIT", contracts.ModeQueue)
	rr, _ := e.a.run(h)
	deadline := time.Now().Add(5 * time.Second)
	for {
		rr.ts.mu.Lock()
		done := rr.ts.lastErr != nil && rr.ts.turnsStarted == 1
		rr.ts.mu.Unlock()
		if done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the turn did not fail")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if o, _ := e.a.Observe(context.Background(), h, ""); o.State == contracts.RunRunning {
		t.Fatalf("a failed unseen turn was reported working: %+v", o)
	}
}
