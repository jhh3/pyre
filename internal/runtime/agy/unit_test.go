package agy

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
)

// Screens captured from agy 1.4.2 on server (tmux capture-pane, trailing
// blanks trimmed, the rule shortened to 60 columns).
var (
	rule        = strings.Repeat("─", 60)
	realIdle    = "  ▄▀▀      ▀▀▄\n" + rule + "\n>\n" + rule + "\n? for shortcuts                                Gemini 4 Argon · medium\n"
	realDraft   = "> earlier\n  pomegranate\n" + rule + "\n> What is the secret word?\n  Second line [pyre-msg:msg_TEST1]\n" + rule + "\n                                Gemini 4 Argon · medium\n"
	realWorking = "⣽  Generating...\n" + rule + "\n>\n" + rule + "\nesc to cancel                                  Gemini 4 Argon · medium\n"
	realQueued  = "⡿  Generating...\n▸ queued second message [pyre-msg:msg_TEST2] (alt+m to send immediately)\n" + rule + "\n>\n" + rule + "\n  Press up to edit queued messages             Gemini 4 Argon · medium\n"
	realSlash   = rule + "\n> /ex\n" + rule + "\n> /exit     Exit the CLI\n  /context  Visualize current context usage\n  ↑/↓ Navigate · enter Select · tab Complete\nesc to cancel        Gemini 4 Argon · medium\n"
	realTrust   = "Accessing workspace:\n/home/synthetic/work\nDo you trust the contents of this project?\nAntigravity CLI requires permission to read, edit, and execute files here.\n> Yes, I trust this folder\n  No, exit\n  ↑/↓ Navigate · enter Confirm\n                                         Gemini 4 Argon · medium\n"
	realPerm    = "● Ran (touch approval-test.txt) (ctrl+o to expand)\nCommand\n" + rule + "\nRequesting permission for:\n   touch approval-test.txt\nRun this command?\n> 1. Yes, run command\n  2. Yes, and always allow in this conversation for commands that start with 'touch'\n  4. No, cancel\n  ↑/↓ Navigate · tab Amend · ctrl+g edit/expand command\nesc to cancel                    Gemini 3.8 Flash · low\n"
	realBgTask  = rule + "\n>\n" + rule + "\n  ● [09:45:52] sleep 30 running\n" + rule + "\n? for shortcuts                     Gemini 4 Argon · medium · 1 task(s) · /tasks\n"
)

func TestParseScreenReal(t *testing.T) {
	cases := []struct {
		name                      string
		screen                    string
		box, empty, working, idle bool
		kind, input               string
	}{
		{"idle", realIdle, true, true, false, true, "", ""},
		{"draft", realDraft, true, false, false, false, "", "What is the secret word?\nSecond line [pyre-msg:msg_TEST1]"},
		{"working", realWorking, true, true, true, false, "", ""},
		{"queued", realQueued, true, true, false, false, "", ""},
		{"slash menu", realSlash, true, false, false, false, "", "/ex"},
		{"trust", realTrust, false, false, false, false, "trust", ""},
		{"permission", realPerm, false, false, false, false, "permission", ""},
		{"background task", realBgTask, true, true, false, true, "", ""},
	}
	for _, c := range cases {
		si := parseScreen(c.screen)
		if si.HasInputBox != c.box || si.InputEmpty() != c.empty || si.Working() != c.working || si.Idle() != c.idle || si.DialogKind != c.kind || si.Input != c.input {
			t.Errorf("%s: box=%v empty=%v working=%v idle=%v kind=%q input=%q", c.name, si.HasInputBox, si.InputEmpty(), si.Working(), si.Idle(), si.DialogKind, si.Input)
		}
	}
	if si := parseScreen(realTrust); !si.TrustYes {
		t.Error("trust: Yes not selected")
	}
	if si := parseScreen(realQueued); !si.Queued {
		t.Error("queued input not seen")
	}
	if si := parseScreen(realDraft); !si.showsText("msg_TEST1", "") || !si.showsMarker("msg_TEST1") || si.showsText("msg_OTHER", "unrelated") || si.showsMarker("msg_TEST") {
		t.Error("draft recognition")
	}
	dated := rule + "\n> [pyre-msg:msg_H1 · sent 3:05 PM PT (just now)] from session:synthetic\n  body\n" + rule + "\n   Model\n"
	if si := parseScreen(dated); !si.showsMarker("msg_H1") {
		t.Error("dated header marker not recognised in the input box")
	}
}

func TestParseModels(t *testing.T) {
	out := "Fetching available models...\ngemini-4-argon-medium\tGemini 4 Argon (Medium)\nclaude-sonnet-5-5-high\tClaude Sonnet 5.5 (High)\n\nnot a model line\n"
	ms := parseModels(out)
	if len(ms) != 2 || ms[0].ID != "gemini-4-argon-medium" || ms[1].Label != "Claude Sonnet 5.5 (High)" {
		t.Fatalf("models %+v", ms)
	}
}

func TestHooksFileAndHookMain(t *testing.T) {
	dir := t.TempDir()
	b, err := hooksFile([]string{"/opt/pyred dir/pyred", "hook", "agy"}, dir)
	if err != nil {
		t.Fatal(err)
	}
	var spec map[string]map[string]json.RawMessage
	if err := json.Unmarshal(b, &spec); err != nil {
		t.Fatal(err)
	}
	if _, ok := spec["pyre"]["PreToolUse"]; ok {
		t.Fatal("PreToolUse must not be registered (an empty answer denies the tool)")
	}
	if !strings.Contains(string(spec["pyre"]["Stop"]), `'/opt/pyred dir/pyred' hook agy Stop `+dir) {
		t.Fatalf("Stop handler %s", spec["pyre"]["Stop"])
	}
	if !strings.Contains(string(spec["pyre"]["PostToolUse"]), `"matcher": "*"`) {
		t.Fatalf("PostToolUse %s", spec["pyre"]["PostToolUse"])
	}
	in := `{"conversationId":"e645b50a-526b-445f-a6ea-ae8e6827e413","transcriptPath":"/x/t.jsonl","modelName":"m","terminationReason":"NO_TOOL_CALL","fullyIdle":true,` +
		`"toolCall":{"name":"run_command","args":{"CommandLine":"echo SYNTHETIC-SECRET"}}}`
	var out bytes.Buffer
	if rc := HookMain([]string{"Stop", dir}, strings.NewReader(in), &out); rc != 0 || strings.TrimSpace(out.String()) != "{}" {
		t.Fatalf("rc %d out %q", rc, out.String())
	}
	raw, _ := os.ReadFile(filepath.Join(dir, fileEvents))
	if strings.Contains(string(raw), "SYNTHETIC-SECRET") {
		t.Fatal("tool arguments reached the state dir")
	}
	recs, _ := readEvents(dir, 0)
	if len(recs) != 1 || recs[0].ConversationID != "e645b50a-526b-445f-a6ea-ae8e6827e413" || recs[0].Termination != "NO_TOOL_CALL" || recs[0].Tool != "run_command" {
		t.Fatalf("records %+v", recs)
	}
	// Garbage in: still "{}" and exit 0.
	out.Reset()
	if rc := HookMain([]string{"Stop", "relative"}, strings.NewReader("nope"), &out); rc != 0 || strings.TrimSpace(out.String()) != "{}" {
		t.Fatalf("bad input: rc %d out %q", rc, out.String())
	}
}

func TestStateFoldAndTranscript(t *testing.T) {
	rs := newRunState()
	if st, _ := rs.hookState(); st != contracts.RunWaitingInput {
		t.Fatalf("fresh: %s", st)
	}
	rs.applyHook(hookRecord{Event: "PreInvocation", ConversationID: "c"})
	if st, _ := rs.hookState(); st != contracts.RunRunning {
		t.Fatalf("turn: %s", st)
	}
	rs.applyHook(hookRecord{Event: "Stop", Termination: "ERROR"})
	if st, why := rs.hookState(); st != contracts.RunWaitingInput || !strings.Contains(why, "ERROR") {
		t.Fatalf("stop: %s %s", st, why)
	}
	line := `{"step_index":4,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","created_at":"2026-10-18T16:45:26Z","content":"<USER_REQUEST>\n[pyre-msg:msg_X] hi\n</USER_REQUEST>\n<ADDITIONAL_METADATA>\nThe current local time is: now.\n</ADDITIONAL_METADATA>"}`
	rs.applyTranscript([]byte(line), 10)
	if ev, ok := rs.accepted["msg_X"]; !ok || ev.Ref != "c#4" || ev.Off != 10 {
		t.Fatalf("evidence %+v", rs.accepted)
	}
	var r transcriptRecord
	_ = json.Unmarshal([]byte(line), &r)
	if e, ok := entryFor(r, 1, "run_x"); !ok || e.Kind != "pyre_message" || e.Text != "[pyre-msg:msg_X] hi" {
		t.Fatalf("entry %+v", e)
	}
	tool := `{"step_index":5,"source":"MODEL","type":"PLANNER_RESPONSE","tool_calls":[{"name":"run_command","args":{"CommandLine":"go test ./...","toolSummary":"Run tests"}}]}`
	var tr transcriptRecord
	_ = json.Unmarshal([]byte(tool), &tr)
	if e, ok := entryFor(tr, 2, "run_x"); !ok || e.Kind != "tool" || e.Text != "run_command: Run tests" {
		t.Fatalf("tool entry %+v", e)
	}
}

func TestRulesAndModelArgs(t *testing.T) {
	r := rulesText("/w/project", "/s/agy/ses_x", "GUIDE")
	if !strings.HasPrefix(r, "---\ntrigger: always_on\n---\n") || !strings.Contains(r, "/w/project") || !strings.Contains(r, "Never run commands in it") || !strings.Contains(r, "GUIDE") {
		t.Fatalf("rules %q", r)
	}
	for _, ok := range []string{"gemini-4-argon-high", "claude-opus-5-5", "models/x@v1"} {
		if err := checkModelArgs(ok, "high"); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range [][2]string{{"--help", ""}, {"a b", ""}, {"x", "HIGH; rm"}} {
		if err := checkModelArgs(bad[0], bad[1]); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}

// The delivery engine renders the dated header "[pyre-msg:<id> · sent …]"
// (internal/messaging/header): it must count as the marker, or no real
// message would ever be accepted.
func TestDatedHeaderIsAcceptanceEvidence(t *testing.T) {
	rs := newRunState()
	rs.conversationID = "c"
	line := `{"step_index":2,"type":"USER_INPUT","created_at":"2026-10-18T16:45:26Z","content":"<USER_REQUEST>\n[pyre-msg:msg_01SYNTH · sent 3:05 PM PT (8h ago)] from session:synthetic-peer\n\nplease rebase\n</USER_REQUEST>"}`
	rs.applyTranscript([]byte(line), 0)
	if ev, ok := rs.accepted["msg_01SYNTH"]; !ok || ev.Ref != "c#2" {
		t.Fatalf("dated header not evidence: %+v", rs.accepted)
	}
	var r transcriptRecord
	_ = json.Unmarshal([]byte(line), &r)
	if e, ok := entryFor(r, 1, "run_x"); !ok || e.Kind != "pyre_message" || e.MessageID != "msg_01SYNTH" {
		t.Fatalf("entry %+v", e)
	}
	if in := renderInput(contracts.DeliveryInput{MessageID: "msg_01SYNTH", Text: "[pyre-msg:msg_01SYNTH · sent 3:05 PM PT (8h ago)] from x"}); strings.Count(in, "[pyre-msg:") != 1 {
		t.Fatalf("marker added twice: %q", in)
	}
}

// Control characters never reach the paste: an embedded bracketed-paste
// end would turn the rest of a message into keystrokes (Enter included).
func TestPasteSafe(t *testing.T) {
	got := pasteSafe("a\x1b[201~\rrm -rf x\r\nline2\tok\x07\u009b")
	if got != "a[201~rm -rf x\nline2\tok" {
		t.Fatalf("%q", got)
	}
}
