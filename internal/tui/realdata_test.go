package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/colorprofile"

	"github.com/jhh3/pyre/internal/contracts"
)

// Regression tests for defects found running the TUI against a real fleet
// (real Claude Code panes, real hub text). All data here is synthetic.

func testModel(profile colorprofile.Profile) *Model {
	return &Model{th: newTheme(profile, true), loads: map[string]*loadState{}, lists: map[string]*listState{},
		opts: Options{Clock: contracts.SystemClock{}}, width: 200, height: 55}
}

// claudePane mimics a real Claude Code pane: content at the top, a tall
// run of blank rows, then the permission prompt and status line at the
// bottom, with trailing spaces as tmux reports them.
func claudePane() string {
	var b strings.Builder
	b.WriteString("> synthetic task: write the report    \n")
	b.WriteString("● Write(synthetic.txt)                \n")
	for i := 0; i < 30; i++ {
		b.WriteString("                                                            \n")
	}
	b.WriteString(" Do you want to create synthetic.txt?\n")
	b.WriteString(" ❯ 1. Yes\n")
	b.WriteString("   2. No\n")
	b.WriteString("\n\n")
	b.WriteString(strings.Repeat("─", 140) + "\n")
	b.WriteString(" ? for shortcuts                                    \n")
	return b.String()
}

func TestCompactScreenKeepsContentAndPrompt(t *testing.T) {
	got := compactScreen(claudePane())
	want := []string{
		"> synthetic task: write the report",
		"● Write(synthetic.txt)",
		"",
		" Do you want to create synthetic.txt?",
		" ❯ 1. Yes",
		"   2. No",
		"",
		strings.Repeat("─", 140),
		" ? for shortcuts",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("compactScreen:\n%q\nwant\n%q", got, want)
	}
	if compactScreen("") != nil || len(compactScreen("\n   \n")) != 0 {
		t.Fatal("blank screens should compact to nothing")
	}
}

func TestActivityShowsApprovalQuestionFromTallPane(t *testing.T) {
	m := testModel(colorprofile.NoTTY)
	sv := contracts.SessionView{Session: contracts.Session{SessionID: "ses_synth-pane", Title: "synthetic", Harness: contracts.HarnessClaude},
		Run: &contracts.Run{RunID: "run_synth-pane", State: contracts.RunWaitingApproval,
			Capabilities: []string{contracts.CapScreen, contracts.CapTranscript}}}
	m.loads["preview:ses_synth-pane"] = &loadState{have: true, val: contracts.ReadSessionResponse{Screen: claudePane(),
		Entries: []contracts.TranscriptEntry{{Kind: "user", Text: "synthetic task"}}}}
	out := strings.Join(m.activityLines(sv, 80, 22), "\n")
	for _, want := range []string{"Do you want to create synthetic.txt?", "1. Yes", "? for shortcuts"} {
		if !strings.Contains(out, want) {
			t.Errorf("activity lost %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "─…") || strings.Contains(out, " …│") {
		t.Errorf("terminal lines should clip like a viewport, not end in an ellipsis:\n%s", out)
	}
	// The frame is sized to its content, not padded to the pane height.
	if n := strings.Count(out, "\n"); n > 16 {
		t.Errorf("activity uses %d lines for a 9-line screen:\n%s", n, out)
	}
}

func TestActivityNeverDrawsHalfAFrame(t *testing.T) {
	m := testModel(colorprofile.NoTTY)
	sv := contracts.SessionView{Session: contracts.Session{SessionID: "ses_synth-pane", Title: "synthetic"},
		Run: &contracts.Run{RunID: "run_synth-pane", State: contracts.RunRunning, Capabilities: []string{contracts.CapScreen, contracts.CapTranscript}}}
	m.loads["preview:ses_synth-pane"] = &loadState{have: true, val: contracts.ReadSessionResponse{Screen: "a\nb\nc",
		Entries: []contracts.TranscriptEntry{{Kind: "user", Text: "one"}, {Kind: "assistant", Text: "two"}}}}
	for h := 2; h <= 12; h++ {
		out := m.activityLines(sv, 60, h)
		if len(out) > h {
			t.Fatalf("h=%d: %d lines", h, len(out))
		}
		joined := strings.Join(out, "\n")
		if strings.Contains(joined, "╭") != strings.Contains(joined, "╰") {
			t.Fatalf("h=%d: half a frame:\n%s", h, joined)
		}
	}
}

func TestPyreMessageEnvelopeIsReadable(t *testing.T) {
	m := testModel(colorprofile.NoTTY)
	text := "[pyre-msg:msg_synth-1] from owner:cli_synth-laptop (owner (init)) (reply to msg_synth-0):\nPlease rerun the synthetic suite."
	got := strings.Join(m.transcriptEntry(contracts.TranscriptEntry{Kind: "pyre_message", Text: text}, 100), "\n")
	if strings.Contains(got, "pyre-msg") || strings.Contains(got, "cli_synth") {
		t.Fatalf("raw envelope shown: %s", got)
	}
	for _, want := range []string{"message", "from you · reply:", "Please rerun the synthetic suite."} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	if _, ok := m.parsePyreMessage("plain user text"); ok {
		t.Fatal("plain text parsed as a message")
	}
}

func TestToolResultPlaceholdersHidden(t *testing.T) {
	es := shownEntries([]contracts.TranscriptEntry{{Kind: "tool", Text: "Bash: echo synthetic"}, {Kind: "tool", Text: "tool result"}, {Kind: "assistant", Text: "done"}})
	if len(es) != 2 || es[1].Kind != "assistant" {
		t.Fatalf("entries = %+v", es)
	}
}

func TestHumanizedPathsTimesAndIDs(t *testing.T) {
	if got := tildePath("/Users/synth/work/a"); got != "~/work/a" {
		t.Errorf("tildePath = %q", got)
	}
	if got := tildePath("/home/synth"); got != "~" {
		t.Errorf("tildePath home = %q", got)
	}
	if got := tildePath("/tmp/x"); got != "/tmp/x" {
		t.Errorf("tildePath other = %q", got)
	}
	now := time.Date(2026, 10, 5, 4, 32, 0, 0, time.UTC)
	got := humanTimes("shell alive; screen unchanged since 2026-10-05T04:20:55Z", now)
	if got != "shell alive; screen unchanged since 04:20 (11m ago)" {
		t.Errorf("humanTimes = %q", got)
	}
	m := testModel(colorprofile.NoTTY)
	m.loads["sessions"] = &loadState{have: true, val: []contracts.SessionView{{Session: contracts.Session{SessionID: "ses_01SYNTH", Title: "Rate limiter review"}}}}
	m.loads["nodes"] = &loadState{have: true, val: []contracts.Node{{NodeID: "node_01SYNTH", Name: "lab-linux"}}}
	if got := m.humanIDs("Message from ses_01SYNTH"); got != "Message from Rate limiter review" {
		t.Errorf("humanIDs = %q", got)
	}
	if got := m.humanIDs("remote attach needs an ssh alias for node node_01SYNTH (not configured)"); !strings.Contains(got, "node lab-linux") {
		t.Errorf("humanIDs node = %q", got)
	}
	if got := m.humanIDs("unknown ses_other stays"); got != "unknown ses_other stays" {
		t.Errorf("unknown IDs must stay: %q", got)
	}
	if got := m.who(contracts.Principal{Kind: contracts.PrincipalOwner, ID: "cli_x", Name: "init"}); got != "you" {
		t.Errorf("owner = %q", got)
	}
}

func TestSearchHighlightKeepsLineStyling(t *testing.T) {
	m := testModel(colorprofile.TrueColor)
	line := m.th.accent.Render("user") + "    ready to go " + m.th.box.Render("│")
	got := m.markQuery(line, "ready", false)
	if !strings.Contains(got, m.th.box.Render("│")) {
		t.Errorf("frame border lost its style: %q", got)
	}
	if !strings.Contains(got, m.th.accent.Render("user")) {
		t.Errorf("role colour lost: %q", got)
	}
	if ansiStrip(got) != ansiStrip(line) {
		t.Errorf("text changed: %q", ansiStrip(got))
	}
	cur := m.markQuery(line, "ready", true)
	if cur == got {
		t.Error("the current match should look different from other matches")
	}
}

func TestListStatusFoldsAttentionBadge(t *testing.T) {
	sv := contracts.SessionView{Attention: 1, Run: &contracts.Run{State: contracts.RunWaitingInput}}
	if st := listStatus(sv, nil); st.label != "needs you" {
		t.Errorf("idle with an inbox item = %q", st.label)
	}
	sv.Run.State = contracts.RunWaitingApproval
	if st := listStatus(sv, nil); st.label != "approval" {
		t.Errorf("approval keeps its own word, got %q", st.label)
	}
	sv.Attention = 0
	sv.Run.State = contracts.RunRunning
	if st := listStatus(sv, nil); st.label != "working" {
		t.Errorf("no attention = %q", st.label)
	}
}

func TestPackColumnsNeverSplitsAGroup(t *testing.T) {
	blocks := [][]string{{"A", "a1", "a2", ""}, {"B", "b1", ""}, {"C", "c1", "c2", "c3", ""}, {"D", "d1", ""}}
	out := packColumns(blocks, 15, 3, 10)
	cols := map[string]int{}
	for _, l := range out {
		for ci := 0; ci < 3; ci++ {
			start := ci * 13
			if start >= len(l) {
				continue
			}
			cell := strings.TrimSpace(l[start:min(len(l), start+10)])
			if len(cell) == 2 && cell[1] >= '0' && cell[1] <= '9' {
				g := strings.ToUpper(cell[:1])
				if c, ok := cols[g]; ok && c != ci {
					t.Fatalf("group %s split across columns:\n%s", g, strings.Join(out, "\n"))
				}
				cols[g] = ci
			}
		}
	}
}

func TestNativePromptFallbackExplained(t *testing.T) {
	m := testModel(colorprofile.NoTTY)
	sv := contracts.SessionView{Session: contracts.Session{SessionID: "ses_synth-native", Title: "synthetic", Harness: contracts.HarnessClaude},
		Run: &contracts.Run{RunID: "run_synth-native", State: contracts.RunWaitingApproval, Adapter: "claude-tmux"}}
	out := strings.Join(m.sessionDetail(sv, 90, 30), "\n")
	if !strings.Contains(out, "Waiting on a prompt in its terminal") {
		t.Fatalf("expired remote approval not explained:\n%s", out)
	}
}

func TestOfflineDetailIsOneHonestStatement(t *testing.T) {
	m := testModel(colorprofile.NoTTY)
	seen := time.Now().Add(-25 * time.Second)
	m.loads["nodes"] = &loadState{have: true, val: []contracts.Node{{NodeID: "node_synth-off", Name: "synth-box",
		Connection: contracts.NodeConnection{State: contracts.ConnDisconnected, LastSeenAt: &seen}}}}
	sv := contracts.SessionView{NodeName: "synth-box", Session: contracts.Session{SessionID: "ses_synth-off", Title: "synthetic", Harness: contracts.HarnessClaude},
		Run: &contracts.Run{RunID: "run_synth-off", NodeID: "node_synth-off", State: contracts.RunUnknown,
			StateReason: "node disconnected; last observed waiting_input", Capabilities: []string{contracts.CapScreen}}}
	out := strings.Join(m.sessionDetail(sv, 100, 30), "\n")
	for _, want := range []string{"synth-box is offline (seen 25s ago); last observed idle", "The run may still be alive.", "No live preview while synth-box is offline."} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "last observed waiting_input") || strings.Contains(out, "unknown") {
		t.Errorf("raw reason or 'unknown' repeated:\n%s", out)
	}
}

// A04: the message detail names the artifact a message references, like
// `pyre msg show` and the Android message card (it showed thread and reply
// but no attachment).
func TestMessageDetailShowsAttachments(t *testing.T) {
	m := testModel(colorprofile.Ascii)
	mv := contracts.MessageView{Envelope: contracts.MessageEnvelope{
		MessageID: "msg_synth-reply", Mode: contracts.ModeQueue,
		Sender:    contracts.Principal{Kind: contracts.PrincipalSession, ID: "ses_synth-mock"},
		Recipient: contracts.Address{Kind: contracts.AddrSession, ID: "ses_synth-shell"},
		ThreadID:  "thr-synth", InReplyTo: "msg_synth-request",
		Body:        contracts.MessageBody{Text: "synthetic reply"},
		Attachments: []contracts.Attachment{{ArtifactVersionID: "ver_synth-notes-v2"}, {DocumentRevisionID: "rev_synth"}},
		CreatedAt:   time.Now(),
	}}
	out := strings.Join(m.messageDetail(mv, 120), "\n")
	for _, want := range []string{"thr-synth", "msg_synth-request", "artifact version ver_synth-notes-v2", "document revision rev_synth", "session:ses_synth-mock"} {
		if !strings.Contains(out, want) {
			t.Fatalf("message detail lacks %q:\n%s", want, out)
		}
	}
}
