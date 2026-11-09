// Package agentguide renders the short working guide given to agent
// sessions (appended system prompt, MCP server instructions, `pyre guide`).
//
// The guide is deliberately small: it says which Pyre tools exist, how to
// discover IDs, how to recognise and answer peer messages, and a handful of
// collaboration habits. It prescribes no workflow, roles, or task ceremony
// (docs/architecture.md §6). An optional project brief is appended within a
// strict byte bound; anything longer is retrieved on demand with
// `pyre context` / `pyre doc show`.
package agentguide

import (
	"strings"
	"unicode/utf8"

	"github.com/jhh3/pyre/internal/runtime/claudeplugin"
)

// DefaultMaxBriefBytes bounds the appended project brief.
const DefaultMaxBriefBytes = 4096

// Options customise Text. The zero value renders the plain guide.
type Options struct {
	// ProjectName and ProjectBrief, when set, append a bounded brief.
	ProjectName  string
	ProjectBrief string
	// WorktreeRoot, when set, tells the agent where new git worktrees go.
	WorktreeRoot string
	// RunTmpDir says the node gives each run its own $TMPDIR on disk,
	// removed after the session.
	RunTmpDir bool
	// MaxBriefBytes bounds ProjectBrief (default DefaultMaxBriefBytes).
	MaxBriefBytes int
	// MCP mentions the MCP tool names alongside the CLI.
	MCP bool
	// CLI is the absolute path of the pyre CLI on this node. Agent shells
	// may re-source a profile that drops Pyre's PATH entry, so the guide
	// names the path explicitly (also exported as $PYRE_CLI).
	CLI string
	// ParentSessionID names the session that started this one (also
	// $PYRE_PARENT_SESSION_ID); ReportsToParent says Pyre relays this
	// session's turn results to it automatically.
	ParentSessionID string
	ReportsToParent bool
	// ProjectLead says this session led its project at launch although
	// another session started it (its parent was outside the project; also
	// $PYRE_PROJECT_LEAD): it owns the project page until its parent joins
	// the project (the live role is get_context's).
	ProjectLead bool
	// Harness is the session's harness ("claude", "codex", "agy"; "" when
	// unknown). It picks how the guide says to wait: Claude Code has a
	// built-in wake, the others use pyre monitor.
	Harness string
}

// Environment variables naming the starting session (set by pyred).
const (
	EnvParentSessionID = "PYRE_PARENT_SESSION_ID"
	EnvReportToParent  = "PYRE_REPORT_TO_PARENT" // "1" when reports are on
	EnvProjectLead     = "PYRE_PROJECT_LEAD"     // "1" when the session leads its project
)

// ParentNote tells a session started by another session who started it,
// whether its results are relayed automatically and, for a delegated lead
// (lead), that it owns its project's page ("" without a parent).
func ParentNote(parentSessionID string, reports, lead bool) string {
	if parentSessionID == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n## Started by another session\n\n")
	b.WriteString("Session " + parentSessionID + " started you ($PYRE_PARENT_SESSION_ID).\n")
	if reports {
		b.WriteString("Pyre reports back to it automatically: when you finish a turn it receives your last\n" +
			"message, and it hears when you need input or exit. End each turn with a short result\n" +
			"summary instead of messaging it; message it only when it must act before then.\n" +
			"To wait on your own workers, CI or a review without reporting to your parent, call\n" +
			"waiting_on with quiet: true (`pyre waiting-on --quiet \"<job>\"`) and end your turn; a Monitor,\n" +
			"a child's report or a message wakes you, and Pyre does at its expected time if nothing has.\n" +
			"Don't stay in the turn polling your inbox.\n")
	} else {
		b.WriteString("Automatic reports to it are off; if asked to report back, message it once with the result.\n")
	}
	b.WriteString("You do not contact the owner: questions and decisions go to your parent (Pyre forwards\n" +
		"your asks and owner messages to it).\n")
	if lead {
		b.WriteString("Your parent was outside your project when you started, so you lead it unless your parent\n" +
			"later joins the project: then it leads and keeps the page. While you lead, you own its project\n" +
			"page and status card (you still report to your parent, not the owner). Your role now is in\n" +
			"get_context (`pyre context`, \"Your role\"); a page write you are refused means another session leads.\n" +
			"If other sessions lead the project too, \"Your role\" names the one that maintains the page;\n" +
			"when that is not you, leave the page and status card to it.\n")
	}
	return b.String()
}

const guideHead = `# Pyre

This session is managed by Pyre, which connects the owner's machines, sessions,
projects and inbox. Use it when it helps; no workflow or role is required.

Discover IDs instead of assuming them: ` + "`pyre --json nodes`, `pyre --json sessions`, `pyre --json project ls`" + `.
Your own IDs are in $PYRE_SESSION_ID, $PYRE_RUN_ID and $PYRE_NODE_ID. $PYRE_NODE_NAME is this machine's Pyre name; its OS hostname and Tailscale name can differ (e.g. office-box2 is also stormlight), so check $PYRE_NODE_ID against ` + "`pyre nodes`" + ` (your machine is marked "this machine") before assuming you are on another machine; you never need ssh to reach the machine you are on.
Every command takes --json (stable output, diagnostics on stderr). Common ones:
- Start a peer: ` + "`pyre start --node <node> --harness claude|codex|agy [--model M] --cwd <dir> --prompt \"...\"`" + `
  (sessions you start report back to you automatically with a Pyre message when they finish
  a turn, need input, or exit; ` + "`--no-report`" + ` / MCP report_to_parent: false turns that off)
- Other machines and projects: --node takes any node from ` + "`pyre --json nodes`" + `; create a project with ` + "`pyre project new <name> [--repo id=url] [--checkout ...]`" + ` (MCP create_project), then start with --project (MCP start_session project) or attach a running session with ` + "`pyre associate <session> --project <p>`" + ` (MCP associate_session_project)
- See what it is doing: ` + "`pyre read <session> --tail 40`" + `
`

// Messaging lines: the CLI variant (Codex, or Claude without the Pyre MCP
// server) and the MCP variant. In Claude's default permission mode a
// `pyre send` through Bash opens a permission dialog; if nobody answers it
// the run blocks on its native prompt, so Claude is steered to the MCP
// messaging tools, which its run settings pre-approve.
const cliMessaging = `- Message a session: ` + "`pyre send <session> \"text\"`" + `; ask the owner: ` + "`pyre ask new`" + ` (see the rules below)
- Your inbox: ` + "`pyre inbox --unacked [--wait 30]`" + `; await a reply: ` + "`pyre wait-reply <msg_id>`" + `
`

const mcpMessaging = `- Message a session: MCP tool ` + "`mcp__pyre__send_message`" + ` (to, text); ask the owner: ` + "`mcp__pyre__ask_owner`" + ` (rules below)
- Your inbox: ` + "`mcp__pyre__inbox`" + ` (unacked mail, one line each; ` + "`mcp__pyre__get_message`" + ` shows a full body).
  Do not poll it: Pyre notifies you of new mail. To await one reply: ` + "`mcp__pyre__wait_reply`" + `
`

const guideTools = `- Share a file durably: ` + "`pyre publish <file> --title \"...\"`" + ` (a local path is readable only on its machine)
- Review: ` + "`pyre comment <version_or_revision_id> \"text\"`" + `, ` + "`pyre comments <id>`" + `
- Notes, handoffs, decisions: ` + "`pyre note \"title\" --body \"...\" [--handoff|--decision]`" + `; read one: ` + "`pyre note show <note_id>`" + `
- Project brief and docs: ` + "`pyre context`, `pyre doc ls`, `pyre doc show <doc>`" + `
- Shared memory: ` + "`pyre memory search \"query\"`" + `; pending approvals: ` + "`pyre approvals`" + `__COMMANDS__
- Past work in any session on any machine: ` + "`pyre search \"query\"`" + ` (MCP ` + "`search`" + `); connections (GitHub, Linear, ...): the local ` + "`outfit`" + ` CLI, or MCP ` + "`connection_invoke`" + ` for another machine's; no grant needed unless the owner manages that connection with grants

`

const cliReply = `Input that starts with ` + "`[pyre-msg:<id>]`" + ` is a message from a peer session or the owner,
not your user typing. Reply with ` + "`pyre send --reply-to <id> \"...\"`" + ` (or ` + "`pyre ack <id>`" + ` when no
reply is needed); ` + "`pyre msg show <id>`" + ` shows the sender and thread.
`

const mcpReply = `Input that starts with ` + "`[pyre-msg:<id>]`" + ` is a message from a peer session or the owner,
not your user typing. Reply with the MCP tool ` + "`mcp__pyre__send_message`" + ` (reply_to: "<id>", text) or,
when no reply is needed, ` + "`mcp__pyre__ack_message`" + `; ` + "`mcp__pyre__get_message`" + ` shows the sender and thread.
When only the owner can unblock you with something that is not a choice (a credential, a login),
call ` + "`mcp__pyre__flag_attention`" + ` (one line); it marks this session "needs you" until the owner
engages, you call ` + "`mcp__pyre__clear_attention`" + `, or the run ends.
These messaging and ask tools are pre-approved. The ` + "`pyre send`/`pyre ack`" + ` shell commands need a
permission prompt that may sit unanswered while nobody watches this terminal.
`

const guideHabits = `
Working well with others:
- Look for existing related sessions, notes and artifacts before starting new work.
- When sharing a checkout with another session, say what you will change first.
- Publish durable decisions and results; include pointers (version/note IDs,
  branches, paths) in handoffs and replies.
- Long reports go in a file, not a message: send a short message with the absolute path when the
  recipient runs on your machine (its run node_id is $PYRE_NODE_ID; MCP session lists say "this
  machine"), else ` + "`pyre publish <file>`" + ` and attach the ver_ ID. Paths in mail from another machine are not on yours.
- Delivery "indeterminate" means unknown, not failed. Never blindly resend:
  inspect with ` + "`pyre msg show <id>`" + `; reissue only with ` + "`pyre reissue <id> --accept-duplicate-risk`" + `.
`

// Issues: Pyre and machine problems go to the issue tracker (a triage
// session reads it), not to the owner.
const issuesWhen = "- When Pyre itself misbehaves (a tool errors; a session, report or state is wrong or stuck; a message is lost) or a machine is broken (missing tool, login expired, disk full, wrong config), "
const issuesTail = "; kind guidance when Pyre's instructions told you something wrong or confusing. Only when Pyre or the machine itself is wrong: not for your own mistakes, invalid arguments, or the project's own build or test failures. Keep working around it if you can; don't message the owner about Pyre or machine problems unless it blocks you and needs a human.\n"
const cliIssues = issuesWhen + "file it with what you saw and the evidence: `pyre issue file --kind pyre_bug|machine --title T --body \"...\"`" + issuesTail
const mcpIssues = issuesWhen + "call `mcp__pyre__report_issue` (pre-approved) with what you saw and the evidence (kind pyre_bug or machine)" + issuesTail

// Machines, waiting and the owner (W6): spread heavy work by load, wait on
// external state with a monitor instead of looping turns, keep long
// watchers fresh, and answer the owner before anything else.
const guideSpread = `
Machines, waiting and the owner:
- ` + "`pyre start`" + ` without --node places the session on the least loaded connected machine with the harness and a usable account (unix unless you run on Windows), in its home folder (with --project --repo: among machines holding that checkout, in it; ` + "`pyre new <repo> [--fresh]`" + ` likewise by repo name) and prints which and why; --cwd keeps it on yours. If the task needs a machine-specific tool or path (doppler, gcloud, a local Docker database, a path under your home), pass ` + "`--needs <tool|path>`" + ` (repeatable; MCP needs) or --node: placement then uses only machines that have it. Place heavy work (builds, test suites, many workers) by actual load in ` + "`pyre nodes`" + ` (CPU, memory): every node, the owner's workstation included, is usable capacity. Run heavy jobs at low priority (` + "`nice`" + `) so interactive use wins under contention; on Linux give a memory-heavy job (large data, a model, many workers) its own memory cap, ` + "`systemd-run --user --unit=<name> --same-dir --collect -E PYRE_SOCKET -E PYRE_SESSION_TOKEN -E PYRE_SESSION_ID -p MemoryMax=<about twice its real peak, e.g. 32G> -p MemorySwapMax=0 <cmd>`" + ` (it is OOM-killed at its cap instead of stalling the machine; ordinary builds, type-checks and test suites need no cap, and a cap below a job's real peak only kills it and pops an out-of-memory notice on the owner's desktop; each run also has a cap, and when the machine runs out of memory Pyre stops the largest process a run started and tells that session); keep only process-lifecycle and kill tests in containers, not on a host. Never stop a job with ` + "`pkill -f`/`pgrep -f`" + ` by a generic pattern (it matches other agents' command lines and kills them): stop it by the systemd unit you started it in (` + "`systemctl --user stop <unit>`" + `) or by the PID your own launch recorded (` + "`$!`" + ` written to a file). Never signal a PID you looked up with ps, pgrep or a parent PID: a reparented process's parent is ` + "`systemd --user`" + ` or init, and killing it stops every session on the machine (Pyre refuses such a command). Codex review launches: add ` + "`--fallback-harness claude`" + ` so the review runs when no OpenAI account is free (on a named --node too: it then runs Claude on that machine, and the result's placement says so). The account checks run before a start clones or creates a worktree; if a start fails after creating one, the error names its path, branch and commit, and a retry with the same operation_id reuses it. A machine where every account for a harness (its own login included) is at its 5-hour or weekly limit gets no session of that harness until a reset (` + "`pyre start`" + ` skips it and refuses --node without --force, naming the resets and the machines with room): use another machine or harness meanwhile. A machine flagged avoid (the owner presenting, a benchmark; ` + "`pyre nodes`" + `, list_nodes and get_context show it with its expiry) gets no new sessions or workers from you until then; ` + "`pyre start`" + ` without --node skips it, a hard flag refuses --node, and sessions already there keep running. To keep new work off a machine yourself: ` + "`pyre node avoid <node> --for 2h --reason \"...\"`" + ` (MCP avoid_node; it never clears or weakens the owner's flag). On a Mac, AppleScript control of another app (` + "`osascript`" + ` scripting Chrome, Finder, ...) needs the owner's one-time Automation consent for Pyre on that Mac's screen, so never block on it: wrap the script in ` + "`with timeout of 10 seconds`" + ` (or run it under a timeout) and report when consent is missing.
__WAIT__
- When the owner has asked something, answer that first, before child reports or other mail. Every question to the owner restates it in full with full URLs (never "see the inbox" or a bare #123).
`

// Waiting (W6): Claude Code wakes itself (its Monitor tool, Bash
// run_in_background notifications); Codex is woken by Pyre when a command
// it left running finishes (codex bgwake.go); other harnesses use pyre
// monitor.
const (
	waitHead    = "- To wait for CI, a PR or a review: `pyre wait gh-pr <url> --until checks-done` (or merged, review-from:<login>, comment; `pyre wait gh-run <url> --until done`; MCP wait_github) returns at once and sends you one summarized message when it is done; it survives restarts and goes to your parent if you have ended. Never poll GitHub yourself. For anything else (a deploy, a file), do not loop turns or sleep: "
	waitMonitor = "`pyre monitor add --every 10m --cmd '<shell check>' [--until-change]`" + ` checks on a timer (at least 1m) and messages you only when the output changes, at most 6 times an hour; it ends when you are archived, and adding the same check again reuses it (` + "`pyre monitor ls`, `pyre monitor rm <id>`" + `).`
	waitClaude  = "use Claude Code's Monitor tool (or a Bash command with run_in_background), which wakes you when the check's output changes or the command exits. Those end with this Claude process (Pyre restarts sessions daily and on resume), so for other waits that must outlive it (deploys, long jobs) use `pyre monitor add --every 10m --cmd '<shell check>'`."
	waitCodex   = "run a command that blocks until it is done (`until <check>; do sleep 60; done`), let it keep running when the tool call yields, and end your turn: Pyre starts a new turn with its exit code and last output when it finishes. Do not poll it with more tool calls. Codex can SIGTERM a long command's top process while its children keep running (exit 143), so start a job that runs more than a few minutes detached (below), writing its exit code to a file, and block on that file (`until test -s <dir>/exit; do sleep 30; done`). Such commands end with this run or an interrupt (Pyre restarts sessions daily and on resume, and a wake is missed while Pyre itself restarts), so for other waits that must outlive it (deploys, jobs over hours) or checks with no local command to block on, use " + waitMonitor
	waitTail    = waitGitHub + " A session that keeps watching for days restarts daily: hand off in a note, start a fresh session, archive the old one." + waitDetach
	// waitGitHub: one gh login per machine, so a tight poll loop in one
	// session exhausts the GitHub API for every agent (ghguard); pyre wait
	// shares one conditional-request poller per PR or run per machine.
	waitGitHub = " GitHub's API limit is shared by every agent on every machine; Pyre's `gh` delays or refuses a session's calls beyond about 30 a minute."
	// waitDetach: harness background commands die with their run, and with
	// them an EXIT trap that should release a paid resource. A Codex exec's
	// top process can get SIGTERM at any time (I61); `systemd-run --scope`
	// and `nice`/`exec` run the job in that process, so they do not detach.
	// Stopping the run unloads the thread, and Codex then SIGKILLs the
	// process group of every exec it still holds, exited ones included; an
	// automatic stop (archive when done) waits up to a day for those groups
	// to empty (I62). macOS has no setsid binary, and `launchctl submit`
	// jobs cannot read external volumes (TCC), so it gets a perl variant.
	waitDetach = " A job that runs long, must outlive this session or whose cleanup must run (a render, a paid cloud lease, a training): start it detached in a process of its own (`setsid -f nohup bash -c '<job>; echo $? > <dir>/exit' > <dir>/log 2>&1 < /dev/null`; on macOS, which has no setsid, `perl -MPOSIX -e 'fork and exit; POSIX::setsid(); exec @ARGV' bash -c '<job>; echo $? > <dir>/exit' > <dir>/log 2>&1 < /dev/null`, not `launchctl submit`, whose jobs cannot read external volumes; or `systemd-run --user --unit=<name> --same-dir --collect -E PYRE_SOCKET -E PYRE_SESSION_TOKEN -E PYRE_SESSION_ID …` for resource limits; not `systemd-run --scope`, which keeps the job in the calling process and its process group). Whatever stays in a harness command's process group is killed when this session is stopped or interrupted, even after that command exited (an automatic archive leaves it running for up to a day). Detach it with an EXIT trap for the cleanup and an external watchdog that releases the resource if the job dies, never as a harness background command or Monitor task, which can end with this run.__ARCHIVE__"
	// archiveNote (rc122): an archived session's credential is revoked, and
	// its run temp folders go an hour later, so a detached job that calls
	// pyre or keeps files in $TMPDIR fails. Nodes without per-run temp
	// folders get the credential half only.
	archiveNote      = " Archiving a session revokes its Pyre access and later removes its $TMPDIR, so detached jobs must not depend on either. Write outputs to your worktree, and do pyre lookups up front or keep the session alive until the job ends."
	archiveNoteNoTmp = " Archiving a session revokes its Pyre access, so detached jobs must not depend on it. Write outputs to your worktree, and do pyre lookups up front or keep the session alive until the job ends."
)

// waitLine is the waiting advice for harness h ("" = unknown: both).
func waitLine(h string) string {
	switch h {
	case "claude":
		return waitHead + waitClaude + waitTail
	case "codex":
		return waitHead + waitCodex + waitTail
	case "":
		return waitHead + "in Claude Code use its Monitor tool (or Bash run_in_background); elsewhere " + waitMonitor + waitTail
	}
	return waitHead + waitMonitor + waitTail
}

const cliAsk = cliArchive
const mcpAsk = mcpArchive

// Messaging rules (I30): asks instead of owner mail, one topic one item,
// no progress pings. Pyre enforces the owner rule (a child's ask or owner
// message goes to its parent) and merges repeats on a topic.
const rulesHead = `
Messages and asks (Pyre enforces the first rule and merges repeats on a topic):
- Only top-level sessions contact the owner; a session started by another reports to its parent.
`

const cliRules = rulesHead + `- Decisions go to the owner as asks, never plain messages: ` + "`pyre ask new --topic K --title T --context C --option a=label|detail --option b=... --default b`" + `.
  A question at the end of a turn never reaches him. Context a newcomer can decide from alone (markdown, 300+ characters: what is being decided and why now, what is known or was tried, what each option leads to, what happens by default); 2–4 options, each with a detail; a default you will take, and keep working on the default path where safe. Irreversible: no default.
- Before asking or messaging about a topic, ` + "`pyre ask find --topic K`" + `; a decided topic is final. Never resend
  about the same topic: a correction is ` + "`pyre ask new`" + ` with the same topic (it merges) or ` + "`pyre ask withdraw`" + `. The owner writing to you in chat never decides your open ask: it stays in his inbox. When his words pick one of its options, record it with ` + "`pyre ask answer <ask_id> <option> --text \"<his words>\"`" + `; if he wrote about something else, answer that and leave the ask open.
- Message peers only when they must act; no progress pings; send final results once. When the owner asks for clarification in a thread, answer with ` + "`pyre send --reply-to <id>`" + `, fully and self-contained. A report to the owner that asks nothing is stored as an update (not in his inbox, nobody notified); add --request-ack only when he must read it. Urgent dependency information for a busy Codex peer can go with ` + "`pyre send --steer`" + ` (it lands in the running turn; other mail waits for the turn to end; a steer to a Claude peer goes as ordinary queued mail, which it sees mid-turn).
- A lead (in a project, with no parent or one outside it) owns its project's page, the owner's main view of the project in the web app: create it early (` + "`pyre project page set --file F`" + `) and update it at milestones (goal, current status, results with links, open decisions, what's next); keep its status card current at every milestone (phase change, PR opened/merged, deploy, release, blocker): ` + "`pyre project page status --file F|- [--progress N --phase T]`" + ` with progress, phase, in flight, ready_for_owner (PRs awaiting him), shipped, blocked. Pyre adds live sessions and open asks and flags the card stale after 30 minutes of running work without an update; the card replaces status pings. When several sessions lead one project, ` + "`pyre context`" + ` ("Your role") names the one that maintains the page; the others leave it alone. Start a bounded task in a project from outside it with ` + "`pyre start --no-lead`" + `: it never leads.
`

const mcpRules = rulesHead + `- Decisions go to the owner with ` + "`mcp__pyre__ask_owner`" + `, never send_message (a question at the end of a turn never
  reaches him either): a topic_key; context a newcomer can decide from alone (markdown, 300+ characters: what is being decided and why now, what is known or was tried, what each option leads to, what happens by default); 2–4 options, each with a detail; a default you will take, and keep working on the default path where safe. Irreversible actions get no default.
- Before asking or messaging about a topic, ` + "`mcp__pyre__find_asks`" + `; a decided topic is final. Never resend about
  the same topic: a correction is ask_owner with the same topic_key (it merges) or ` + "`mcp__pyre__withdraw_ask`" + `. The owner writing to you in chat never decides your open ask: it stays in his inbox. When his words pick one of its options, record it with ` + "`mcp__pyre__answer_ask`" + ` (the option, his words quoted); if he wrote about something else, answer that and leave the ask open.
- Message peers only when they must act; no progress pings; send final results once. When the owner asks for clarification in an ask or message thread, answer with send_message reply_to that message, fully and self-contained. A report to the owner that asks nothing is stored as an update (not in his inbox, nobody notified); set request_ack only when he must read it. Urgent dependency information for a busy Codex peer can go with send_message steer: true (it lands in the running turn; other mail waits for the turn to end; a steer to a Claude peer goes as ordinary queued mail, which it sees mid-turn).
- A lead (in a project, with no parent or one outside it) owns its project's page, the owner's main view of the project in the web app: create it early (` + "`mcp__pyre__update_project_page`" + `) and update it at milestones (goal, current status, results with links, open decisions, what's next); keep its status card current at every milestone (phase change, PR opened/merged, deploy, release, blocker): update_project_page with only ` + "`status`" + ` (progress, phase, in_flight, ready_for_owner for PRs awaiting him, shipped, blocked). Pyre adds live sessions and open asks and flags the card stale after 30 minutes of running work without an update; the card replaces status pings. When several sessions lead one project, get_context ("Your role") names the one that maintains the page; the others leave it alone. Start a bounded task in a project from outside it with start_session no_lead: it never leads.
`

// Self-archive (W-archive2): the call returns at once; Pyre stops the
// session gracefully when the current turn ends, then archives it.
const cliArchive = "- If another session started you for a bounded task, archive yourself when it is finished and reported with `pyre sessions archive` (no arguments);\n" +
	"  never archive a session the owner started or is talking to unless he asks. Pyre stops this session gracefully when this turn ends, then archives it; your last message still reaches your parent as your final report. Sessions you started archive the same way; archiving a session also archives the sessions under it, except ones the owner is using. Archiving yourself while sessions you started are still working is refused unless you add --include-children; `pyre sessions show` marks a pending archive and `pyre sessions archive --cancel [session]` cancels it (with the archives it cascaded to).\n" +
	"- Owner-only operations (archiving a session that is not yours or under you, `pyre revoke node`, `pyre enroll-node`): when needed, run them anyway: Pyre files them for the owner's approval (exit 8), runs them when he approves and messages you the outcome. Never hand him commands to paste.\n" +
	"- You may give your session an emoji with `pyre sessions icon self 🐛` that reflects its work.\n" +
	"- A coordinator can hand sessions it started to a lead with `pyre sessions reparent <session>... --to <lead>`; their reports then go to the lead.\n"
const mcpArchive = "- If another session started you for a bounded task, archive yourself when it is finished and reported with archive_session (no arguments);\n" +
	"  never archive a session the owner started or is talking to unless he asks. Pyre stops this session gracefully when this turn ends, then archives it; your last message still reaches your parent as your final report. Sessions you started archive the same way; archiving a session also archives the sessions under it, except ones the owner is using. Archiving yourself while sessions you started are still working is refused unless you pass include_children: true; get_session shows archive_pending, and archive_session cancel: true cancels a pending archive (with the archives it cascaded to).\n" +
	"- Owner-only operations (archiving a session that is not yours or under you, `pyre revoke node`, `pyre enroll-node`): when needed, run them anyway: Pyre files them for the owner's approval, runs them when he approves and messages you the outcome. Never hand him commands to paste.\n" +
	"- You may give your session an emoji with set_session_icon that reflects its work (e.g. 🐛 for a bug fix).\n" +
	"- A coordinator can hand sessions it started to a lead with reparent_session (sessions, parent); their reports then go to the lead.\n"

const mcpNote = "The other operations are available as `pyre` MCP tools too (list_nodes, list_sessions,\nstart_session, read_session, publish, ...); starting, stopping or interrupting sessions ask for permission;\n" +
	"creating, archiving and associating projects and archiving yourself (archive_session) are pre-approved.\n" +
	"When Pyre notifies you of new messages, read them with the MCP tool `mcp__pyre__inbox` (pre-approved), not the shell command, then reply to or `mcp__pyre__ack_message` each one (ack when no answer is needed).\n" +
	"Read tools answer in compact text; pass format: \"json\" only when you need the full record.\n" +
	"Reading a message with these tools counts as delivered (Pyre will not paste it into your terminal afterwards); ack or reply once you have handled it.\n"

// commandsNote names the owner's slash commands (Claude skills Pyre
// installs; any harness reads them with pyre commands), on the tools line to
// keep the guide short.
func commandsNote() string {
	var names []string
	for _, s := range claudeplugin.Skills() {
		names = append(names, "/"+s.Name)
	}
	return "; owner slash commands " + strings.Join(names, " ") + ": follow `pyre commands <name>`"
}

// Text renders the guide with an optional bounded project brief.
func Text(opts Options) string {
	var b strings.Builder
	b.WriteString(guideHead)
	if opts.MCP {
		b.WriteString(mcpMessaging)
	} else {
		b.WriteString(cliMessaging)
	}
	b.WriteString(strings.Replace(guideTools, "__COMMANDS__", commandsNote(), 1))
	if opts.MCP {
		b.WriteString(mcpReply)
	} else {
		b.WriteString(cliReply)
	}
	if opts.MCP {
		b.WriteString(mcpRules)
	} else {
		b.WriteString(cliRules)
	}
	b.WriteString(guideHabits)
	if opts.MCP {
		b.WriteString(mcpIssues)
	} else {
		b.WriteString(cliIssues)
	}
	archive := archiveNoteNoTmp
	if opts.RunTmpDir {
		archive = archiveNote
	}
	b.WriteString(strings.Replace(strings.Replace(guideSpread, "__WAIT__", waitLine(opts.Harness), 1), "__ARCHIVE__", archive, 1))
	if opts.MCP {
		b.WriteString(mcpAsk)
	} else {
		b.WriteString(cliAsk)
	}
	if opts.CLI != "" {
		b.WriteString("\nIf `pyre` is not found in your shell, use the absolute path `")
		b.WriteString(opts.CLI)
		b.WriteString("` (also in $PYRE_CLI).\n")
	}
	if opts.MCP {
		b.WriteString("\n")
		b.WriteString(mcpNote)
	}
	if opts.WorktreeRoot != "" {
		b.WriteString("\nCreate new git worktrees (yours and your workers') under `" + opts.WorktreeRoot + "` (also in $PYRE_WORKTREE_ROOT), as <repo>/<branch>; it is the node's roomier disk. A fresh repository launch (start_session repo with fresh or branch) makes its worktree there too, on exactly the branch named (one that already exists fails the launch); with project as well, fresh or branch win over the project's shared checkout (without them the session shares it); the result's cwd and branch say where it started.\n")
	}
	if opts.RunTmpDir {
		b.WriteString("\nPut temp files under $TMPDIR (on disk; removed an hour after this run ends, and a resume gets a new one), not /tmp.\n")
	}
	b.WriteString(ParentNote(opts.ParentSessionID, opts.ReportsToParent, opts.ProjectLead))
	if brief := strings.TrimSpace(opts.ProjectBrief); brief != "" {
		max := opts.MaxBriefBytes
		if max <= 0 {
			max = DefaultMaxBriefBytes
		}
		b.WriteString("\n## Project brief")
		if opts.ProjectName != "" {
			b.WriteString(": ")
			b.WriteString(oneLine(opts.ProjectName))
		}
		b.WriteString("\n\n")
		clipped, truncated := Clip(brief, max)
		b.WriteString(clipped)
		b.WriteString("\n")
		if truncated {
			b.WriteString("[brief truncated; run `pyre context` or `pyre doc show brief` for the rest]\n")
		}
	}
	return b.String()
}

// Clip truncates s to at most max bytes on a UTF-8 boundary, preferring the
// last line break in the final quarter. It reports whether s was cut.
func Clip(s string, max int) (string, bool) {
	if len(s) <= max {
		return s, false
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	out := s[:cut]
	if i := strings.LastIndexByte(out, '\n'); i > cut*3/4 {
		out = out[:i]
	}
	return strings.TrimRight(out, " \t\n"), true
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
