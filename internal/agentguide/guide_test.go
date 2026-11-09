package agentguide

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// Node avoid flags: every variant tells agents to keep new work off an
// avoided machine, where flags show, and how to set one.
func TestGuideMentionsAvoidedMachines(t *testing.T) {
	for _, g := range []string{Text(Options{}), Text(Options{MCP: true})} {
		for _, want := range []string{"flagged avoid", "with its expiry", "gets no new sessions or workers from you", "skips it", "a hard flag refuses --node",
			"sessions already there keep running", "pyre node avoid <node> --for 2h", "avoid_node", "never clears or weakens the owner's flag"} {
			if !strings.Contains(g, want) {
				t.Errorf("guide lacks %q", want)
			}
		}
	}
}

// Every variant names the machine's Pyre name and warns that its OS
// hostname and Tailscale name can differ, so an agent does not ssh to the
// machine it is on or report its own node unreachable.
func TestGuideNamesThisMachine(t *testing.T) {
	for _, g := range []string{Text(Options{}), Text(Options{MCP: true})} {
		for _, want := range []string{"$PYRE_NODE_NAME is this machine's Pyre name", "OS hostname and Tailscale name can differ",
			"check $PYRE_NODE_ID against `pyre nodes`", "before assuming you are on another machine", "you never need ssh to reach the machine you are on"} {
			if !strings.Contains(g, want) {
				t.Errorf("guide lacks %q", want)
			}
		}
	}
}

// W-runtmp: the $TMPDIR line appears only on nodes that give runs their own.
func TestGuideMentionsRunTmpDir(t *testing.T) {
	if g := Text(Options{}); strings.Contains(g, "$TMPDIR") {
		t.Fatal("plain guide mentions $TMPDIR")
	}
	if g := Text(Options{RunTmpDir: true}); !strings.Contains(g, "Put temp files under $TMPDIR") {
		t.Fatalf("guide lacks the $TMPDIR line:\n%s", g)
	}
}

// rc122: a detached job loses the session's Pyre access when the session
// is archived, and its $TMPDIR an hour later; the guide says so on the
// detached-job line (no new line), for every harness.
func TestGuideArchiveRevokesDetachedJobs(t *testing.T) {
	const full = "Archiving a session revokes its Pyre access and later removes its $TMPDIR, so detached jobs must not depend on either. Write outputs to your worktree, and do pyre lookups up front or keep the session alive until the job ends."
	const noTmp = "Archiving a session revokes its Pyre access, so detached jobs must not depend on it. Write outputs to your worktree, and do pyre lookups up front or keep the session alive until the job ends."
	for _, h := range []string{"", "claude", "codex", "agy"} {
		for _, mcp := range []bool{false, true} {
			plain, tmp := Text(Options{Harness: h, MCP: mcp}), Text(Options{Harness: h, MCP: mcp, RunTmpDir: true})
			if !strings.Contains(tmp, full) || !strings.Contains(plain, noTmp) || strings.Contains(plain, full) {
				t.Fatalf("harness %q mcp %v: archive note missing or wrong", h, mcp)
			}
			if strings.Contains(tmp, "__ARCHIVE__") || strings.Contains(plain, "__ARCHIVE__") {
				t.Fatal("placeholder left in the guide")
			}
			for _, l := range strings.Split(tmp, "\n") {
				if strings.Contains(l, full) && !strings.Contains(l, "external watchdog") {
					t.Fatalf("archive note is not on the detached-job line: %q", l)
				}
			}
		}
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
		// jobs whose cleanup must run are detached, not harness background commands
		"start it detached", "setsid -f nohup", "echo $? > <dir>/exit", "systemd-run --user --unit=", "EXIT trap", "external watchdog",
		// I61: --scope runs the job in the calling (SIGTERMed) process
		"not `systemd-run --scope`",
		// I61/I62: an explicit stop kills what stays in an exec's process
		// group; an automatic archive spares it for up to a day
		"process group is killed when this session is stopped or interrupted", "automatic archive leaves it running",
		// macOS: no setsid binary; launchd jobs cannot read external volumes
		"POSIX::setsid()", "not `launchctl submit`",
		// I66: memory-heavy jobs carry their own cap; the guard stops runaways
		"-p MemoryMax=<about twice its real peak, e.g. 32G> -p MemorySwapMax=0", "ordinary builds, type-checks and test suites need no cap", "Pyre stops the largest process a run started",
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
	if g := Text(Options{}); !strings.Contains(g, "--needs <tool|path>") || !strings.Contains(g, "MCP needs") {
		t.Fatal("guide lacks the --needs placement hint")
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
	if !strings.Contains(codex, "end your turn: Pyre starts a new turn with its exit code") || !strings.Contains(codex, "until <check>; do sleep 60; done") ||
		!strings.Contains(codex, "pyre monitor add --every 10m") || strings.Contains(codex, "Monitor tool") || !strings.Contains(codex, "outlive it") {
		t.Errorf("Codex should wait with a background command Pyre wakes it for, pyre monitor only for long waits:\n%s", codex)
	}
	// I61: Codex's parent-death SIGTERM; long jobs run detached and are waited on by their exit file.
	if !strings.Contains(codex, "exit 143") || !strings.Contains(codex, "until test -s <dir>/exit") || strings.Contains(claude, "exit 143") {
		t.Errorf("Codex should run long jobs detached and block on their exit file:\n%s", codex)
	}
	if agy := Text(Options{Harness: "agy"}); !strings.Contains(agy, "pyre monitor add --every 10m") || strings.Contains(agy, "Pyre starts a new turn") {
		t.Errorf("agy has no wake and should wait with pyre monitor:\n%s", agy)
	}
	if !strings.Contains(unknown, "Monitor tool") || !strings.Contains(unknown, "pyre monitor add --every 10m") {
		t.Errorf("an unknown harness gets both:\n%s", unknown)
	}
	for _, g := range []string{claude, codex, unknown, Text(Options{Harness: "agy"})} {
		// Every harness waits on GitHub with pyre wait, not its own poll loop.
		if !strings.Contains(g, "pyre wait gh-pr <url> --until checks-done") || !strings.Contains(g, "Never poll GitHub yourself") || strings.Contains(g, "gh pr checks <n> --watch") {
			t.Errorf("GitHub waiting advice:\n%s", g)
		}
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
	off := ParentNote("ses_synth-parent", false, false)
	if !strings.Contains(off, "off") || strings.Contains(off, "reports back to it automatically") || strings.Contains(off, "quiet") {
		t.Errorf("opt-out note: %s", off)
	}
	if ParentNote("", true, true) != "" {
		t.Error("no parent, no note")
	}
	if strings.Contains(child, "you lead it") {
		t.Error("a child in its parent's project is not told it leads")
	}
	// A delegated lead (parent outside its project at launch) owns the page
	// until its parent joins the project, is pointed at its live role, and
	// still reports to its parent.
	lead := Text(Options{MCP: true, ParentSessionID: "ses_synth-parent", ReportsToParent: true, ProjectLead: true})
	for _, want := range []string{"so you lead it unless your parent\nlater joins the project", "you own its project\npage and status card",
		"Your role now is in\nget_context", "You do not contact the owner", "with no parent or one outside it",
		"names the one that maintains the page;\nwhen that is not you, leave the page"} {
		if !strings.Contains(lead, want) {
			t.Errorf("delegated lead guide lacks %q", want)
		}
	}
	// Several leads and --no-lead (I63), in both guide variants.
	for g, want := range map[string]string{Text(Options{}): "`pyre start --no-lead`: it never leads", Text(Options{MCP: true}): "start_session no_lead: it never leads"} {
		if !strings.Contains(g, "When several sessions lead one project") || !strings.Contains(g, want) {
			t.Errorf("guide lacks the co-lead and no-lead lines (%q)", want)
		}
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

// Stopping jobs: by the unit or the PID the agent's own launch recorded,
// never a PID looked up with ps/pgrep/ppid (iss_…:
// a reparented loop's parent was systemd --user).
func TestGuideStopsJobsByUnitOrRecordedPID(t *testing.T) {
	for _, g := range []string{Text(Options{}), Text(Options{MCP: true})} {
		for _, want := range []string{"stop it by the systemd unit you started it in", "`systemctl --user stop <unit>`",
			"the PID your own launch recorded (`$!` written to a file)", "Never signal a PID you looked up with ps, pgrep or a parent PID",
			"`systemd --user` or init", "Pyre refuses such a command"} {
			if !strings.Contains(g, want) {
				t.Errorf("guide lacks %q", want)
			}
		}
		if strings.Contains(g, "exact PID") {
			t.Error("guide still says to stop a job by its exact PID")
		}
	}
}
