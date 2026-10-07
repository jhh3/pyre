package claude

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"github.com/jhh3/pyre/internal/runtime/claudeplugin"
)

// DoorbellAlive reports whether the run's doorbell watcher process (pid
// file written by pyred inbox-watch) is running.
func DoorbellAlive(stateDir string) bool {
	b, err := os.ReadFile(filepath.Join(stateDir, claudeplugin.PIDFile))
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	return err == nil && pid > 0 && syscall.Kill(pid, 0) == nil
}

// Doorbell (S-D): a per-run plugin monitor tells the agent about new Pyre
// mail; the agent reads it with the pyre MCP inbox tools. Capability
// contracts.CapInputMonitor; evidence contracts.EvidenceClaudeInboxRead
// (acceptance) and contracts.EvidenceClaudeMonitorNotified (informational,
// never acceptance). Contract v1.6, decision I16.

// Pyre MCP tool names as Claude sees them (server key "pyre").
const (
	toolPyreInbox      = "mcp__pyre__inbox"
	toolPyreGetMessage = "mcp__pyre__get_message"
	toolPyreAck        = "mcp__pyre__ack_message"
)

// DoorbellAllowedTools are the tools the doorbell notification names:
// reading and acknowledging own mail must not open a permission dialog.
var DoorbellAllowedTools = []string{toolPyreInbox, toolPyreGetMessage, toolPyreAck}

// PyreMessagingAllowedTools are pre-approved in every Claude run's
// settings (allow rules for a server the run lacks are inert). They are
// the non-destructive messaging tools for fleet peers and the owner:
// reading, acknowledging and replying to mail, sending a message, waiting
// for a reply — plus the read-only get_digest catch-up. Replying through `pyre send` in Bash instead opens a
// permission dialog in the default permission mode; when nobody answers
// it, the run blocks on its native prompt. Not pre-approved, so they keep
// Claude's normal permission prompt: starting, stopping or interrupting
// sessions, answering approvals, reissuing work (duplicate risk),
// resolving deliveries, publishing files, editing documents, notes and
// memory, creating projects or re-associating sessions, and connection
// invocations. A message to a peer is mail: the
// recipient decides what to do with it, and it is never a typed
// keystroke into another session.
var PyreMessagingAllowedTools = []string{
	toolPyreInbox, toolPyreGetMessage, toolPyreAck,
	"mcp__pyre__send_message", "mcp__pyre__wait_reply",
	"mcp__pyre__get_digest",     // read-only "while you were away" digest
	"mcp__pyre__search",         // read-only global search (W-search): finding past work never prompts
	"mcp__pyre__audit_log",      // read-only audit log (W-audit): no secrets or message text
	"mcp__pyre__rename_session", // W-names: an agent names its own session (hub refuses other sessions and owner-set titles)
	"mcp__pyre__flag_attention", // W-attn: an agent flags its own session "needs you" (hub refuses other sessions); a prompt here would itself need the owner
	"mcp__pyre__clear_attention",
}

// runAllowedTools is the permissions.allow list written to every run's
// settings, with or without the doorbell (whose tools are a subset).
func runAllowedTools() []string {
	return append([]string(nil), PyreMessagingAllowedTools...)
}

// messageIDRe matches "message_id":"msg_…" in raw or JSON-escaped text.
var messageIDRe = regexp.MustCompile(`message_id\\*"\s*:\s*\\*"(msg_[A-Za-z0-9_.:\-]{1,190})`)

// pyreToolMessageIDs extracts the message ids an inbox read returned (or
// an ack named) from a PostToolUse payload.
func pyreToolMessageIDs(tool string, p map[string]any, raw []byte) []string {
	switch tool {
	case toolPyreInbox, toolPyreGetMessage:
		resp := string(raw)
		if i := strings.Index(resp, `"tool_response"`); i >= 0 {
			resp = resp[i:]
		}
		seen := map[string]bool{}
		var out []string
		for _, m := range messageIDRe.FindAllStringSubmatch(resp, -1) {
			if !seen[m[1]] {
				seen[m[1]] = true
				out = append(out, m[1])
			}
		}
		return out
	case toolPyreAck:
		ti, _ := p["tool_input"].(map[string]any)
		if id := str(ti, "message_id"); id != "" && safeID(id) {
			return []string{id}
		}
	}
	return nil
}

// monitorBlockers are environment variables under which Claude Code does
// not run plugin monitors (docs: Monitor tool availability).
var monitorBlockers = []string{
	"DISABLE_TELEMETRY", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC",
	"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY",
}

// doorbellBlocked returns the first variable that disables monitors for a
// run, looking at the launch env (explicit) and then the inherited one.
func doorbellBlocked(env map[string]string, unset []string) string {
	removed := map[string]bool{}
	for _, k := range unset {
		removed[k] = true
	}
	for _, k := range monitorBlockers {
		if v, ok := env[k]; ok {
			if v != "" && v != "0" && v != "false" {
				return k
			}
			continue
		}
		if removed[k] {
			continue
		}
		if v := os.Getenv(k); v != "" && v != "0" && v != "false" {
			return k
		}
	}
	return ""
}
