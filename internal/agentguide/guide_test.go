package agentguide

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// W-runtmp: the $TMPDIR line appears only on nodes that give runs their own.
func TestGuideMentionsRunTmpDir(t *testing.T) {
	if g := Text(Options{}); strings.Contains(g, "$TMPDIR") {
		t.Fatal("plain guide mentions $TMPDIR")
	}
	if g := Text(Options{RunTmpDir: true}); !strings.Contains(g, "Put temp files under $TMPDIR") {
		t.Fatalf("guide lacks the $TMPDIR line:\n%s", g)
	}
}

func TestGuideIsShortAndCoversTheEssentials(t *testing.T) {
	g := Text(Options{})
	if n := strings.Count(g, "\n"); n > 56 {
		t.Fatalf("guide is %d lines; keep it short (<= ~56)", n)
	}
	for _, want := range []string{
		"pyre --json nodes", "$PYRE_SESSION_ID", "[pyre-msg:<id>]", "--reply-to",
		"existing related", "sharing a checkout", "Publish durable", "pointers",
		"indeterminate", "Never blindly resend", "--accept-duplicate-risk", "ask",
		"pyre project new", "create_project", "associate_session_project",
		// the owner's slash commands (embedded Claude skills)
		"/kickoff", "/open-items", "pyre commands <name>",
		// I30 messaging rules, CLI variant
		"Only top-level sessions contact the owner", "pyre ask new", "pyre ask find", "pyre ask withdraw",
		"default you will take", "newcomer", "no progress pings", "final results once",
		"question at the end of a turn never",
		// W6: placement, monitors, the owner first
		"least loaded", "pyre monitor add", "restarts daily", "answer that first", "full URLs", "--fallback-harness claude",
	} {
		if !strings.Contains(g, want) {
			t.Errorf("guide lacks %q", want)
		}
	}
	for _, banned := range []string{"must always", "coordinator role", "task board", "MANDATORY"} {
		if strings.Contains(g, banned) {
			t.Errorf("guide prescribes ceremony: %q", banned)
		}
	}
	// Issues: Pyre and machine problems are filed, not sent to the owner.
	for g, tool := range map[string]string{Text(Options{}): "pyre issue file --kind pyre_bug|machine", Text(Options{MCP: true}): "mcp__pyre__report_issue"} {
		for _, want := range []string{tool, "login expired", "kind guidance", "not for your own mistakes, invalid arguments, or the project's own build or test failures", "Keep working around it", "don't message the owner about Pyre or machine problems"} {
			if !strings.Contains(g, want) {
				t.Errorf("guide lacks %q", want)
			}
		}
	}
	if !strings.Contains(Text(Options{MCP: true}), "question at the end of a turn never") {
		t.Error("MCP variant lacks the end-of-turn question rule")
	}
	// W-web-inbox: ask context, clarification replies, the lead's project page.
	for _, g := range []string{Text(Options{}), Text(Options{MCP: true})} {
		for _, want := range []string{"300+ characters", "each with a detail", "what happens by default", "asks for clarification", "self-contained", "project's page", "at milestones", "status card", "ready_for_owner"} {
			if !strings.Contains(g, want) {
				t.Errorf("guide lacks %q", want)
			}
		}
	}
	if g := Text(Options{MCP: true}); !strings.Contains(g, "mcp__pyre__update_project_page") || !strings.Contains(g, "reply_to that message") {
		t.Error("MCP variant lacks the project page tool or the reply_to rule")
	}
	if !strings.Contains(Text(Options{}), "pyre project page set") {
		t.Error("CLI variant lacks pyre project page set")
	}
	if !strings.Contains(Text(Options{MCP: true}), "MCP tools") {
		t.Error("MCP variant should mention the tools")
	}
}

func TestGuideWaitingAdviceFollowsTheHarness(t *testing.T) {
	claude, codex, unknown := Text(Options{MCP: true, Harness: "claude"}), Text(Options{MCP: true, Harness: "codex"}), Text(Options{})
	if !strings.Contains(claude, "Monitor tool") || !strings.Contains(claude, "run_in_background") || !strings.Contains(claude, "outlive it") {
		t.Errorf("Claude should wait with its built-in Monitor tool:\n%s", claude)
	}
	if !strings.Contains(codex, "end your turn: Pyre starts a new turn with its exit code") || !strings.Contains(codex, "gh pr checks <n> --watch") ||
		!strings.Contains(codex, "pyre monitor add --every 10m") || strings.Contains(codex, "Monitor tool") || !strings.Contains(codex, "outlive it") {
		t.Errorf("Codex should wait with a background command Pyre wakes it for, pyre monitor only for long waits:\n%s", codex)
	}
	if agy := Text(Options{Harness: "agy"}); !strings.Contains(agy, "pyre monitor add --every 10m") || strings.Contains(agy, "Pyre starts a new turn") {
		t.Errorf("agy has no wake and should wait with pyre monitor:\n%s", agy)
	}
	if !strings.Contains(unknown, "Monitor tool") || !strings.Contains(unknown, "pyre monitor add --every 10m") {
		t.Errorf("an unknown harness gets both:\n%s", unknown)
	}
	for _, g := range []string{claude, codex, unknown} {
		if !strings.Contains(g, "restarts daily") || strings.Contains(g, "__WAIT__") {
			t.Errorf("waiting line:\n%s", g)
		}
	}
}

func TestBriefIsBounded(t *testing.T) {
	brief := strings.Repeat("Goal: ship the synthetic thing. ünïcödé\n", 500)
	g := Text(Options{ProjectName: "Synthetic\nProject", ProjectBrief: brief, MaxBriefBytes: 1000})
	if !strings.Contains(g, "## Project brief: Synthetic Project") || !strings.Contains(g, "brief truncated") {
		t.Fatalf("brief header/truncation: %q", g[len(g)-200:])
	}
	if len(g) > len(Text(Options{}))+1000+200 {
		t.Fatalf("brief not bounded: %d bytes", len(g))
	}
	if !utf8.ValidString(g) {
		t.Fatal("truncation split a UTF-8 sequence")
	}
	small := Text(Options{ProjectBrief: "Short brief."})
	if !strings.Contains(small, "Short brief.") || strings.Contains(small, "truncated") {
		t.Fatal("short brief must be included verbatim")
	}
	if s, cut := Clip("ééé", 3); cut != true || !utf8.ValidString(s) {
		t.Fatalf("clip %q %v", s, cut)
	}
}

// With the Pyre MCP server, replies go through the pre-approved MCP
// messaging tools: `pyre send` through Bash opens a permission dialog that
// may sit unanswered (seen live: the remote approval expired and the run
// waited on its native prompt).
func TestMCPGuideSteersRepliesToMCPTools(t *testing.T) {
	g := Text(Options{MCP: true, CLI: "/opt/synthetic/pyre"})
	for _, want := range []string{
		"mcp__pyre__send_message", "reply_to", "mcp__pyre__ack_message", "mcp__pyre__inbox",
		"pre-approved", "permission prompt",
	} {
		if !strings.Contains(g, want) {
			t.Errorf("MCP guide lacks %q", want)
		}
	}
	// The reply instruction names the MCP tool, not the shell command.
	i := strings.Index(g, "[pyre-msg:<id>]")
	if i < 0 {
		t.Fatal("no message marker paragraph")
	}
	para := g[i:]
	if j := strings.Index(para, "\n\n"); j > 0 {
		para = para[:j]
	}
	if !strings.Contains(para, "mcp__pyre__send_message") || strings.Contains(para, "pyre send --reply-to") {
		t.Fatalf("reply paragraph should steer to the MCP tool:\n%s", para)
	}
	// Decisions go to the owner as asks (I30), not owner mail.
	for _, want := range []string{"mcp__pyre__ask_owner", "never send_message", "mcp__pyre__find_asks", "mcp__pyre__withdraw_ask",
		"same topic_key", "Only top-level sessions contact the owner", "no progress pings", "Irreversible actions get no default"} {
		if !strings.Contains(g, want) {
			t.Errorf("MCP guide lacks %q", want)
		}
	}
	if strings.Contains(g, `to: "owner"`) || strings.Contains(Text(Options{}), "pyre send owner") {
		t.Error("the guide must not steer decisions to plain owner mail")
	}
	// Without MCP (Codex) the shell commands stay.
	if plain := Text(Options{}); !strings.Contains(plain, "pyre send --reply-to") || strings.Contains(plain, "mcp__pyre__") {
		t.Error("plain guide must keep the CLI instructions")
	}
}

func TestParentNoteAndPeerReports(t *testing.T) {
	g := Text(Options{MCP: true})
	if !strings.Contains(g, "report back to you automatically") || strings.Contains(g, "Started by another session") {
		t.Fatalf("parent-side line missing or child note without a parent:\n%s", g)
	}
	child := Text(Options{MCP: true, ParentSessionID: "ses_synth-parent", ReportsToParent: true})
	for _, want := range []string{"## Started by another session", "ses_synth-parent", "$PYRE_PARENT_SESSION_ID", "reports back to it automatically", "You do not contact the owner", "go to your parent",
		"waiting_on with quiet: true", "end your turn", "Don't stay in the turn polling your inbox"} {
		if !strings.Contains(child, want) {
			t.Errorf("child guide lacks %q", want)
		}
	}
	off := ParentNote("ses_synth-parent", false)
	if !strings.Contains(off, "off") || strings.Contains(off, "reports back to it automatically") || strings.Contains(off, "quiet") {
		t.Errorf("opt-out note: %s", off)
	}
	if ParentNote("", true) != "" {
		t.Error("no parent, no note")
	}
}

// The owner's default: sessions use connections without asking him.
func TestGuideSaysConnectionsNeedNoGrant(t *testing.T) {
	for _, g := range []string{Text(Options{}), Text(Options{MCP: true})} {
		for _, want := range []string{"`outfit`", "connection_invoke", "no grant needed"} {
			if !strings.Contains(g, want) {
				t.Errorf("guide lacks %q", want)
			}
		}
	}
}
