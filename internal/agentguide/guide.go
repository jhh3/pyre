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
)

// DefaultMaxBriefBytes bounds the appended project brief.
const DefaultMaxBriefBytes = 4096

// Options customise Text. The zero value renders the plain guide.
type Options struct {
	// ProjectName and ProjectBrief, when set, append a bounded brief.
	ProjectName  string
	ProjectBrief string
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
			"message, and it hears when you need input or exit. You do not need to message it unless\n" +
			"asked to; end your turn with a short summary of the result instead.\n")
	} else {
		b.WriteString("Automatic reports to it are off; message it if you were asked to report back.\n")
	}
	b.WriteString("Message the owner only if you are explicitly told to.\n")
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
const cliMessaging = `- Message a session or the owner: ` + "`pyre send <session|owner> \"text\" [--request-ack]`" + `
- Your inbox: ` + "`pyre inbox --unacked [--wait 30]`" + `; await a reply: ` + "`pyre wait-reply <msg_id>`" + `
`

const mcpMessaging = `- Message a session or the owner: MCP tool ` + "`mcp__pyre__send_message`" + ` (to, text)
- Your inbox: ` + "`mcp__pyre__inbox`" + ` (unacked: true); await a reply: ` + "`mcp__pyre__wait_reply`" + `
`

const guideTools = `- Share a file durably: ` + "`pyre publish <file> --title \"...\"`" + ` (a local path is not shared)
- Review: ` + "`pyre comment <version_or_revision_id> \"text\"`" + `, ` + "`pyre comments <id>`" + `
- Notes, handoffs, decisions: ` + "`pyre note \"title\" --body \"...\" [--handoff|--decision]`" + `
- Project brief and docs: ` + "`pyre context`, `pyre doc ls`, `pyre doc show <doc>`" + `
- Shared memory: ` + "`pyre memory search \"query\"`" + `; pending approvals: ` + "`pyre approvals`" + `
- Past work in any session on any machine: ` + "`pyre search \"query\"`" + ` (MCP ` + "`search`" + `)

`

const cliReply = `Input that starts with ` + "`[pyre-msg:<id>]`" + ` is a message from a peer session or the owner,
not your user typing. Reply with ` + "`pyre send --reply-to <id> \"...\"`" + ` (or ` + "`pyre ack <id>`" + ` when no
reply is needed); ` + "`pyre msg show <id>`" + ` shows the sender and thread.
`

const mcpReply = `Input that starts with ` + "`[pyre-msg:<id>]`" + ` is a message from a peer session or the owner,
not your user typing. Reply with the MCP tool ` + "`mcp__pyre__send_message`" + ` (reply_to: "<id>", text) or,
when no reply is needed, ` + "`mcp__pyre__ack_message`" + `; ` + "`mcp__pyre__get_message`" + ` shows the sender and thread.
When only the owner can unblock you (a decision, a credential, a choice between options), call
` + "`mcp__pyre__flag_attention`" + ` (text: one line saying what you need); it marks this session
"needs you" until the owner engages, you call ` + "`mcp__pyre__clear_attention`" + `, or the run ends.
These messaging tools are pre-approved. The ` + "`pyre send`/`pyre ack`" + ` shell commands need a
permission prompt that may sit unanswered while nobody watches this terminal.
`

const guideHabits = `
Working well with others:
- Look for existing related sessions, notes and artifacts before starting new work.
- When sharing a checkout with another session, say what you will change first.
- Publish durable decisions and results; include pointers (version/note IDs,
  branches, paths) in handoffs and replies.
- Delivery "indeterminate" means unknown, not failed. Never blindly resend:
  inspect with ` + "`pyre msg show <id>`" + `; reissue only with ` + "`pyre reissue <id> --accept-duplicate-risk`" + `.
`

const cliAsk = "- If you are blocked or unsure, ask: `pyre send owner \"...\"` or message a peer.\n"
const mcpAsk = "- If you are blocked or unsure, ask: `mcp__pyre__send_message (to: \"owner\", text)` or message a peer.\n"

const mcpNote = "The other operations are available as `pyre` MCP tools too (list_nodes, list_sessions,\nstart_session, read_session, publish, ...); starting, stopping or interrupting sessions and creating projects ask for permission.\n" +
	"When Pyre notifies you of new messages, read them with the MCP tool `mcp__pyre__inbox` (unacked: true; pre-approved), not the shell command, then reply to or `mcp__pyre__ack_message` each one.\n"

// Text renders the guide with an optional bounded project brief.
func Text(opts Options) string {
	var b strings.Builder
	b.WriteString(guideHead)
	if opts.MCP {
		b.WriteString(mcpMessaging)
	} else {
		b.WriteString(cliMessaging)
	}
	b.WriteString(guideTools)
	if opts.MCP {
		b.WriteString(mcpReply)
	} else {
		b.WriteString(cliReply)
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
