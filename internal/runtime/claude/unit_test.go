package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// Screens captured from real Claude Code 2.1.289 in an isolated tmux server
// (WP07 calibration; synthetic content only).
const realIdle = ` ▐▛███▛█   Claude Code v2.1.289
▝▜██████▀  Opus 5.5 (1M context) with high effort · Claude Max

                                                                 ● high · /effort
────────────────────────────────────────────────────────────────────────────────
❯ Try "how does <filepath> work?"
────────────────────────────────────────────────────────────────────────────────
  ⏸ manual mode on · ? for shortcuts · ← for agents`

const realIdleEscLine = "\x1b[39m❯  \x1b[2mTry \"refactor <filepath>\"\x1b[0m"
const realDraftEscLine = "\x1b[39m❯  hello draft"

const realDraft = `────────────────────────────────────────────────────────────────────────────────
❯ [pyre-msg:msg_calib1] draft text not submitted
────────────────────────────────────────────────────────────────────────────────
  ⏸ manual mode on`

const realMulti = `                                                ctrl+g to edit in Nvim
────────────────────────────────────────────────────────────────────────────────
❯ line one
  line two [pyre-msg:msg_calib2]
  line three
────────────────────────────────────────────────────────────────────────────────
  ⏸ manual mode on`

const realLongPaste = `────────────────────────────────────────────────────────────────────────────────
❯ [Pasted text #1 +61 lines]
────────────────────────────────────────────────────────────────────────────────
  paste again to expand`

const realTrust = `────────────────────────────────────────────────────────────────────────────────
 Accessing workspace:
 /private/tmp/scratch/pw07/calib/untrusted-work
 Quick safety check: Is this a project you created or one you trust? (Like your own code, a
 well-known open source project, or work from your team). If not, take a moment to review what's in
 this folder first.
 Claude Code'll be able to read, edit, and execute files here.
 Security guide
 ❯ No, exit
   Yes, I trust this folder
 Enter to confirm · Esc to cancel`

const realPermission = ` Bash command
   touch pyre-approval-1.txt
 Do you want to proceed?
 ❯ 1. Yes
   2. Yes, and don't ask again for touch commands in this folder
   3. No
 Esc to cancel`

func TestParseScreenReal(t *testing.T) {
	rule := strings.Repeat("─", 80)
	si := parseScreen(realIdle, rule+"\n"+realIdleEscLine+"\n"+rule)
	if !si.HasInputBox || !si.Placeholder || !si.InputEmpty() || si.Dialog != "" {
		t.Fatalf("idle: %+v", si)
	}
	// Without escapes the Try "…" heuristic applies.
	if si := parseScreen(realIdle, ""); !si.InputEmpty() {
		t.Fatalf("idle no-esc: %+v", si)
	}
	si = parseScreen(realDraft, rule+"\n"+realDraftEscLine+"\n"+rule)
	if !si.HasInputBox || si.InputEmpty() || !si.markerOnInput("[pyre-msg:msg_calib1]", true) {
		t.Fatalf("draft: %+v", si)
	}
	if si.markerOnInput("[pyre-msg:other]", true) {
		t.Fatal("wrong marker matched")
	}
	si = parseScreen(realMulti, "")
	if len(si.InputLines) != 3 || !si.markerOnInput("[pyre-msg:msg_calib2]", false) {
		t.Fatalf("multi: %+v", si.InputLines)
	}
	si = parseScreen(realLongPaste, "")
	if !si.markerOnInput("[pyre-msg:x]", true) || si.markerOnInput("[pyre-msg:x]", false) {
		t.Fatal("long paste placeholder only counts when the box was empty before")
	}
	si = parseScreen(realTrust, "")
	if si.HasInputBox || si.DialogKind != "trust" || si.TrustYes {
		t.Fatalf("trust: %+v", si)
	}
	si = parseScreen(strings.Replace(strings.Replace(realTrust, " ❯ No, exit", "   No, exit", 1), "   Yes, I trust", " ❯ Yes, I trust", 1), "")
	if !si.TrustYes {
		t.Fatal("trust yes selection not detected")
	}
	si = parseScreen(realPermission, "")
	if si.HasInputBox || si.DialogKind != "permission" {
		t.Fatalf("permission: %+v", si)
	}
	si = parseScreen(" Select a theme\n ❯ 1. Dark mode\n   2. Light mode\n Enter to confirm", "")
	if si.DialogKind != "unknown" || si.Dialog != "Select a theme" {
		t.Fatalf("unknown dialog: %+v", si)
	}
	// Regression (real resume): scrollback shows past prompts as "❯ text"
	// while the input box is not yet rendered; that is not a dialog.
	si = parseScreen(" ▐▛███▛█   Claude Code v2.1.289\n\n❯ [pyre-msg:msg_real1] Reply with the single word ok.\n\n⏺ ok\n", "")
	if si.Dialog != "" || si.HasInputBox {
		t.Fatalf("history line taken for a dialog: %+v", si)
	}
}

func TestDetectLimit(t *testing.T) {
	le := detectLimit(`{"error":"Claude AI usage limit reached|1791200000"}`, time.Now())
	if le == nil || le.Kind != "usage_limit" || le.ResetAt == nil || le.ResetAt.Unix() != 1791200000 {
		t.Fatalf("%+v", le)
	}
	if le := detectLimit("API Error: 401 OAuth token has expired. Please run /login", time.Now()); le == nil || le.Kind != "auth_invalid" {
		t.Fatalf("%+v", le)
	}
	if le := detectLimit("rate_limit_error: too many requests", time.Now()); le == nil || le.Kind != "rate_limited" {
		t.Fatalf("%+v", le)
	}
	if detectLimit("all good", time.Now()) != nil {
		t.Fatal("false positive")
	}
	if got := redact("token=abc123 bearer xyz.def sk-ant-oat01-SECRET"); strings.Contains(got, "abc123") || strings.Contains(got, "SECRET") || strings.Contains(got, "xyz") {
		t.Fatalf("redact: %q", got)
	}
}

func TestProjectSlugAndMarkers(t *testing.T) {
	if got := projectSlug("/private/tmp/scratch/pyre-wp01/claude/repro/work"); got != "-private-tmp-scratch-pyre-wp01-claude-repro-work" {
		t.Fatal(got)
	}
	ms := findMarkers("[pyre-msg:msg_01ABC] hi [pyre-msg:msg_2.x:y] [pyre-msg:bad id]")
	if len(ms) != 2 || ms[0] != "msg_01ABC" || ms[1] != "msg_2.x:y" {
		t.Fatalf("%v", ms)
	}
	if renderInput(contracts.DeliveryInput{MessageID: "msg_1", Text: "hi"}) != "[pyre-msg:msg_1] hi" {
		t.Fatal("render prefix")
	}
	if renderInput(contracts.DeliveryInput{MessageID: "msg_1", Text: "[pyre-msg:msg_1] hi"}) != "[pyre-msg:msg_1] hi" {
		t.Fatal("render keeps existing marker")
	}
}

func TestHookSettings(t *testing.T) {
	dir := t.TempDir()
	p, err := writeHookSettings(dir, []string{"/opt/pyre dir/pyred", "hook", "claude"}, 90*time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	var s struct {
		Hooks map[string][]struct {
			Matcher string
			Hooks   []struct {
				Type, Command string
				Timeout       int
			}
		}
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	if len(s.Hooks) != len(HookEvents) {
		t.Fatalf("events %d", len(s.Hooks))
	}
	pr := s.Hooks["PermissionRequest"][0]
	if pr.Matcher != "*" || pr.Hooks[0].Timeout != 105 || !strings.HasPrefix(pr.Hooks[0].Command, "'/opt/pyre dir/pyred' hook claude PermissionRequest ") {
		t.Fatalf("%+v", pr)
	}
}

func TestRunHookEventsAndBroker(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	out, code := RunHook(ctx, "UserPromptSubmit", strings.NewReader(`{"session_id":"s1","prompt":"[pyre-msg:msg_9] hi","transcript_path":"/x"}`), dir)
	if code != 0 || out != nil {
		t.Fatal(code, out)
	}
	recs, _, _ := readEvents(dir, 0)
	if len(recs) != 1 || recs[0].Markers[0] != "msg_9" || recs[0].SessionID != "s1" {
		t.Fatalf("%+v", recs)
	}
	// Missing state dir never fails the harness.
	if _, code := RunHook(ctx, "Stop", strings.NewReader("{}"), filepath.Join(dir, "gone")); code != 0 {
		t.Fatal(code)
	}
	// Broker: PreToolUse then PermissionRequest; answer allow.
	// Generous broker wait and poll deadline: under a loaded full-suite run
	// (go test -p N) the old 2.5s poll could expire before the hook wrote
	// its request. The hook returns as soon as the answer lands.
	_ = writeFileAtomic(filepath.Join(dir, fileBroker), []byte(`{"wait_seconds":30}`))
	RunHook(ctx, "PreToolUse", strings.NewReader(`{"tool_name":"Bash","tool_use_id":"toolu_1","tool_input":{"command":"ls"}}`), dir)
	done := make(chan []byte)
	go func() {
		o, _ := RunHook(ctx, "PermissionRequest", strings.NewReader(`{"tool_name":"Bash","tool_input":{"command":"ls"}}`), dir)
		done <- o
	}()
	var rid string
	for deadline := time.Now().Add(15 * time.Second); rid == "" && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
		ents, _ := os.ReadDir(filepath.Join(dir, dirPending))
		for _, e := range ents {
			// writeFileAtomic stages ".<rid>.json.tmp<pid>" in the same
			// directory before the rename; only the final name is a request.
			if n := e.Name(); !strings.HasPrefix(n, ".") && strings.HasSuffix(n, ".json") {
				rid = strings.TrimSuffix(n, ".json")
			}
		}
	}
	pr := readPending(dir, rid)
	if pr == nil || pr.ToolUseID != "toolu_1" || pr.Summary != "ls" {
		t.Fatalf("pending %+v", pr)
	}
	b, _ := json.Marshal(answerFile{RequestID: rid, OptionID: "deny", Behavior: "deny", Message: "no thanks"})
	_ = writeFileAtomic(filepath.Join(dir, dirAnswers, rid+".json"), b)
	o := <-done
	if !bytes.Contains(o, []byte(`"behavior":"deny"`)) || !bytes.Contains(o, []byte("no thanks")) || !bytes.Contains(o, []byte(`"hookEventName":"PermissionRequest"`)) {
		t.Fatalf("decision %s", o)
	}
	recs, _, _ = readEvents(dir, 0)
	if recs[len(recs)-1].Event != evApprovalAnswered {
		t.Fatalf("last %+v", recs[len(recs)-1])
	}
	// Stop with a queued injection claims it once.
	inj, _ := json.Marshal(injection{MessageID: "msg_s", Text: "[pyre-msg:msg_s] steer"})
	_ = os.MkdirAll(filepath.Join(dir, dirInject), 0o700)
	_ = writeFileAtomic(filepath.Join(dir, dirInject, injectionName(1, "msg_s")+".msg"), inj)
	o, _ = RunHook(ctx, "Stop", strings.NewReader(`{}`), dir)
	if !bytes.Contains(o, []byte(`"decision":"block"`)) {
		t.Fatalf("stop %s", o)
	}
	if o, _ := RunHook(ctx, "Stop", strings.NewReader(`{}`), dir); o != nil {
		t.Fatalf("second stop claimed again: %s", o)
	}
}

func TestBrokerTimeout(t *testing.T) {
	dir := t.TempDir()
	_ = writeFileAtomic(filepath.Join(dir, fileBroker), []byte(`{"wait_seconds":1}`))
	o, _ := RunHook(context.Background(), "PermissionRequest", strings.NewReader(`{"tool_name":"Bash","tool_input":{"command":"ls"}}`), dir)
	if o != nil {
		t.Fatalf("timeout must return no decision, got %s", o)
	}
	recs, _, _ := readEvents(dir, 0)
	if recs[len(recs)-1].Event != evApprovalTimeout {
		t.Fatalf("%+v", recs)
	}
}

// Approval effect rules on a synthetic fold.
func TestEvalApprovalsRules(t *testing.T) {
	a := &Adapter{o: Options{Now: time.Now}}
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, dirAnswers), 0o700)
	h := contracts.RunHandle{RunID: "run_x", SessionID: "ses_x", StateDir: dir}
	t0 := time.Now().Add(-time.Minute)
	mk := func(evs ...hookRecord) (*runState, []contracts.Approval) {
		rs := newRunState()
		rs.applyHook(hookRecord{Event: "SessionStart", At: t0})
		for _, e := range evs {
			rs.applyHook(e)
		}
		s, _ := rs.derive()
		return rs, a.evalApprovals(h, rs, s)
	}
	perm := hookRecord{Event: "PermissionRequest", At: t0.Add(time.Second), RequestID: "r1", ToolUseID: "tu1", ToolName: "Bash", HookPID: os.Getpid()}
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }
	// pending
	if _, aps := mk(perm); aps[0].State != contracts.ApprovalPending {
		t.Fatalf("pending: %+v", aps[0])
	}
	// pyre allow then PostToolUse => effective
	_, aps := mk(perm, hookRecord{Event: evApprovalAnswered, At: at(2), RequestID: "r1", Behavior: "allow"},
		hookRecord{Event: "PostToolUse", At: at(3), ToolUseID: "tu1"})
	if aps[0].State != contracts.ApprovalResolved || aps[0].Resolution.Effect != "effective" || aps[0].Resolution.Source != "pyre" {
		t.Fatalf("effective: %+v", aps[0].Resolution)
	}
	// TUI allowed first (PostToolUse before broker answer) => superseded
	_, aps = mk(perm, hookRecord{Event: "PostToolUse", At: at(2), ToolUseID: "tu1"},
		hookRecord{Event: evApprovalAnswered, At: at(3), RequestID: "r1", Behavior: "deny"})
	if aps[0].Resolution == nil || aps[0].Resolution.Effect != "superseded" || aps[0].Resolution.Source != "native" {
		t.Fatalf("superseded: %+v", aps[0].Resolution)
	}
	// Answered in the TUI with no Pyre answer => resolved native, no effect
	_, aps = mk(perm, hookRecord{Event: "PostToolUse", At: at(2), ToolUseID: "tu1"})
	if aps[0].Resolution.Source != "native" || aps[0].Resolution.Effect != "" {
		t.Fatalf("native: %+v", aps[0].Resolution)
	}
	// Hook process gone (TUI "No") with an unconsumed Pyre answer => superseded
	dead := perm
	dead.HookPID = 999999
	b, _ := json.Marshal(answerFile{RequestID: "r1", OptionID: "allow", Behavior: "allow"})
	_ = os.WriteFile(filepath.Join(dir, dirAnswers, "r1.json"), b, 0o600)
	_, aps = mk(dead)
	if aps[0].State != contracts.ApprovalResolved || aps[0].Resolution.Effect != "superseded" {
		t.Fatalf("dead hook: %+v", aps[0])
	}
	os.Remove(filepath.Join(dir, dirAnswers, "r1.json"))
	// Broker timed out, dialog still up => expired; turn ended => resolved native
	_, aps = mk(perm, hookRecord{Event: evApprovalTimeout, At: at(5), RequestID: "r1"})
	if aps[0].State != contracts.ApprovalExpired {
		t.Fatalf("expired: %+v", aps[0])
	}
	rs, _ := mk(perm, hookRecord{Event: evApprovalTimeout, At: at(5), RequestID: "r1"})
	rs.applyTranscript(transcriptRecord{Type: "system", Subtype: "turn_duration", Timestamp: at(6).Format(time.RFC3339Nano)}, 0)
	s, reason := rs.derive()
	aps = a.evalApprovals(h, rs, s)
	if s != contracts.RunWaitingInput || !strings.Contains(reason, "turn_duration") || aps[0].State != contracts.ApprovalResolved {
		t.Fatalf("turn ended: %s %s %+v", s, reason, aps[0])
	}
	if aps[0].ApprovalID != approvalID("run_x", "r1") || !contracts.ValidID(aps[0].ApprovalID, contracts.PrefixApproval) {
		t.Fatalf("approval id %q", aps[0].ApprovalID)
	}
}

func TestStateFold(t *testing.T) {
	rs := newRunState()
	if s, _ := rs.derive(); s != contracts.RunStarting {
		t.Fatal(s)
	}
	t0 := time.Now()
	steps := []struct {
		rec  hookRecord
		want contracts.RunState
	}{
		{hookRecord{Event: "SessionStart", At: t0, SessionID: "abc"}, contracts.RunWaitingInput},
		{hookRecord{Event: "UserPromptSubmit", At: t0.Add(1), Markers: []string{"msg_1"}}, contracts.RunRunning},
		{hookRecord{Event: "PreToolUse", At: t0.Add(2), ToolName: "AskUserQuestion", ToolUseID: "q"}, contracts.RunWaitingApproval},
		{hookRecord{Event: "PostToolUse", At: t0.Add(3), ToolUseID: "q"}, contracts.RunRunning},
		{hookRecord{Event: "Stop", At: t0.Add(4), Payload: json.RawMessage(`{"background_tasks":[{"id":"b"}]}`)}, contracts.RunWaitingBackground},
		{hookRecord{Event: "UserPromptSubmit", At: t0.Add(5)}, contracts.RunRunning},
		{hookRecord{Event: "Notification", At: t0.Add(6), Payload: json.RawMessage(`{"notification_type":"permission_prompt"}`)}, contracts.RunWaitingApproval},
		{hookRecord{Event: "Stop", At: t0.Add(7), Payload: json.RawMessage(`{"background_tasks":[]}`)}, contracts.RunWaitingInput},
		{hookRecord{Event: "SessionEnd", At: t0.Add(8)}, contracts.RunExiting},
	}
	for i, st := range steps {
		rs.applyHook(st.rec)
		if s, r := rs.derive(); s != st.want {
			t.Fatalf("step %d %s: got %s (%s) want %s", i, st.rec.Event, s, r, st.want)
		}
	}
	if ev, ok := rs.bestEvidence("msg_1"); !ok || ev.Kind != contracts.EvidenceClaudePromptHook {
		t.Fatal("evidence")
	}
	// transcript queue enqueue + later user record (consumed)
	rs.applyTranscript(transcriptRecord{Type: "queue-operation", Operation: "enqueue", Content: json.RawMessage(`"[pyre-msg:msg_q] x"`), Timestamp: t0.Format(time.RFC3339Nano)}, 10)
	rs.applyTranscript(transcriptRecord{Type: "user", UUID: "u1", Message: json.RawMessage(`{"role":"user","content":"[pyre-msg:msg_q] x"}`), Timestamp: t0.Format(time.RFC3339Nano)}, 20)
	if len(rs.accepted["msg_q"]) != 2 || rs.consumed["msg_q"].Ref != "u1" {
		t.Fatalf("%+v %+v", rs.accepted["msg_q"], rs.consumed)
	}
	// tool_result user records are not prompts
	rs.applyTranscript(transcriptRecord{Type: "user", Message: json.RawMessage(`{"role":"user","content":[{"type":"tool_result","content":"[pyre-msg:msg_t]"}]}`)}, 30)
	if _, ok := rs.accepted["msg_t"]; ok {
		t.Fatal("tool result counted as prompt")
	}
}

// ChangeStamp moves when Claude writes a hook event or a transcript record
// (a native-terminal interrupt writes only the transcript's turn_duration),
// so the daemon can wake a backed-off observer.
func TestChangeStampFollowsEventsAndTranscript(t *testing.T) {
	a := &Adapter{o: Options{Now: time.Now}, runs: map[string]*runEntry{}}
	dir := t.TempDir()
	tp := filepath.Join(t.TempDir(), "synthetic-session.jsonl")
	if err := os.WriteFile(tp, []byte(`{"type":"user","uuid":"u0","timestamp":"2026-10-04T12:00:00Z"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := contracts.RunHandle{RunID: "run_stamp", SessionID: "ses_stamp", StateDir: dir}
	s0 := a.ChangeStamp(h)
	if err := appendJSONL(filepath.Join(dir, fileEvents), hookRecord{ID: "ev1", At: time.Now(), Event: "SessionStart", TranscriptPath: tp}); err != nil {
		t.Fatal(err)
	}
	s1 := a.ChangeStamp(h)
	if s1 == s0 {
		t.Fatal("stamp ignores a new hook event")
	}
	e := a.entry(h)
	e.mu.Lock()
	a.refresh(h, e)
	e.mu.Unlock()
	s2 := a.ChangeStamp(h)
	if !strings.Contains(s2, "|") {
		t.Fatalf("stamp does not cover the transcript once located: %q", s2)
	}
	f, _ := os.OpenFile(tp, os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString(`{"type":"system","subtype":"turn_duration","timestamp":"2026-10-04T12:00:05Z"}` + "\n")
	f.Close()
	if s3 := a.ChangeStamp(h); s3 == s2 {
		t.Fatal("stamp ignores a transcript append (turn_duration after an interrupt)")
	}
}
