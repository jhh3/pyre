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
}

// Environment variables naming the starting session (set by pyred).
const (
	EnvParentSessionID = "PYRE_PARENT_SESSION_ID"
	EnvReportToParent  = "PYRE_REPORT_TO_PARENT" // "1" when reports are on
)

// ParentNote tells a session started by another session who started it and
// whether its results are relayed automatically ("" without a parent).
func ParentNote(parentSessionID string, reports bool) string {
	if parentSessionID == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n## Started by another session\n\n")
	b.WriteString("Session " + parentSessionID + " started you ($PYRE_PARENT_SESSION_ID).\n")
	if reports {
		b.WriteString("Pyre reports back to it automatically: when you finish a turn it receives your last\n" +
			"message, and it hears when you need input or exit. End each turn with a short result\n" +
			"summary instead of messaging it; message it only when it must act before then.\n")
	} else {
		b.WriteString("Automatic reports to it are off; if asked to report back, message it once with the result.\n")
	}
	b.WriteString("You do not contact the owner: questions and decisions go to your parent (Pyre forwards\n" +
		"your asks and owner messages to it).\n")
	return b.String()
}

const guideHead = `# Pyre

This session is managed by Pyre, which connects the owner's machines, sessions,
projects and inbox. Use it when it helps; no workflow or role is required.

Discover IDs instead of assuming them: ` + "`pyre --json nodes`, `pyre --json sessions`, `pyre --json project ls`" + `.
Your own IDs are in $PYRE_SESSION_ID, $PYRE_RUN_ID and $PYRE_NODE_ID.
Every command takes --json (stable output, diagnostics on stderr). Common ones:
- Start a peer: ` + "`pyre start --node <node> --harness claude|codex --cwd <dir> --prompt \"...\"`" + `
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
- Notes, handoffs, decisions: ` + "`pyre note \"title\" --body \"...\" [--handoff|--decision]`" + `
- Project brief and docs: ` + "`pyre context`, `pyre doc ls`, `pyre doc show <doc>`" + `
- Shared memory: ` + "`pyre memory search \"query\"`" + `; pending approvals: ` + "`pyre approvals`" + `__COMMANDS__
- Past work in any session on any machine: ` + "`pyre search \"query\"`" + ` (MCP ` + "`search`" + `)

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
  about the same topic: a correction is ` + "`pyre ask new`" + ` with the same topic (it merges) or ` + "`pyre ask withdraw`" + `.
- Message peers only when they must act; no progress pings; send final results once. When the owner asks for clarification in a thread, answer with ` + "`pyre send --reply-to <id>`" + `, fully and self-contained.
- A lead (top-level session in a project) owns its project's page, the owner's main view of the project in the web app: create it early (` + "`pyre project page set --file F`" + `) and update it at milestones (goal, current status, results with links, open decisions, what's next).
`

const mcpRules = rulesHead + `- Decisions go to the owner with ` + "`mcp__pyre__ask_owner`" + `, never send_message (a question at the end of a turn never
  reaches him either): a topic_key; context a newcomer can decide from alone (markdown, 300+ characters: what is being decided and why now, what is known or was tried, what each option leads to, what happens by default); 2–4 options, each with a detail; a default you will take, and keep working on the default path where safe. Irreversible actions get no default.
- Before asking or messaging about a topic, ` + "`mcp__pyre__find_asks`" + `; a decided topic is final. Never resend about
  the same topic: a correction is ask_owner with the same topic_key (it merges) or ` + "`mcp__pyre__withdraw_ask`" + `.
- Message peers only when they must act; no progress pings; send final results once. When the owner asks for clarification in an ask or message thread, answer with send_message reply_to that message, fully and self-contained.
- A lead (top-level session in a project) owns its project's page, the owner's main view of the project in the web app: create it early (` + "`mcp__pyre__update_project_page`" + `) and update it at milestones (goal, current status, results with links, open decisions, what's next).
`

// Self-archive (W-archive2): the call returns at once; Pyre stops the
// session gracefully when the current turn ends, then archives it.
const cliArchive = "- If another session started you for a bounded task, archive yourself when it is finished and reported with `pyre sessions archive` (no arguments);\n" +
	"  never archive a session the owner started or is talking to (Pyre refuses it).\n" +
	"  Pyre stops this session gracefully when this turn ends, then archives it. Sessions you started archive the same way.\n" +
	"- You may give your session an emoji with `pyre sessions icon self 🐛` that reflects its work.\n" +
	"- A coordinator can hand sessions it started to a lead with `pyre sessions reparent <session>... --to <lead>`; their reports then go to the lead.\n"
const mcpArchive = "- If another session started you for a bounded task, archive yourself when it is finished and reported with archive_session (no arguments);\n" +
	"  never archive a session the owner started or is talking to (Pyre refuses it).\n" +
	"  Pyre stops this session gracefully when this turn ends, then archives it. Sessions you started archive the same way.\n" +
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
		b.WriteString("\nCreate new git worktrees (yours and your workers') under `" + opts.WorktreeRoot + "` (also in $PYRE_WORKTREE_ROOT), as <repo>/<branch>; it is the node's roomier disk.\n")
	}
	b.WriteString(ParentNote(opts.ParentSessionID, opts.ReportsToParent))
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
