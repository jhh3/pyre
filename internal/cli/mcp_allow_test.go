package cli

import (
	"strings"
	"testing"

	"github.com/jhh3/pyre/internal/runtime/claude"
)

// The tools the Claude adapter pre-approves exist, are the messaging tools
// for fleet peers plus projects and self/child archiving (W-archive2), and
// include nothing that launches, stops, interrupts, answers approvals,
// duplicates work or publishes.
func TestPreapprovedPyreToolsAreMessagingOnly(t *testing.T) {
	byName := map[string]MCPTool{}
	for _, tl := range MCPTools() {
		byName["mcp__pyre__"+tl.Name] = tl
	}
	want := map[string]bool{"mcp__pyre__send_message": true, "mcp__pyre__inbox": true, "mcp__pyre__get_message": true,
		"mcp__pyre__ack_message": true, "mcp__pyre__wait_reply": true,
		"mcp__pyre__get_digest":     true,                                     // read-only catch-up (W-digest)
		"mcp__pyre__search":         true,                                     // read-only global search (W-search)
		"mcp__pyre__audit_log":      true,                                     // read-only audit log (W-audit)
		"mcp__pyre__rename_session": true,                                     // self-only rename (W-names; hub-enforced)
		"mcp__pyre__flag_attention": true, "mcp__pyre__clear_attention": true, // self-only flag (W-attn; hub-enforced)
		// W-archive2: projects and self/child archiving (hub-enforced)
		"mcp__pyre__create_project": true, "mcp__pyre__associate_session_project": true,
		"mcp__pyre__archive_session": true, "mcp__pyre__archive_project": true,
		"mcp__pyre__set_session_icon": true, // W-archive2: a session's emoji (like rename)
		"mcp__pyre__reparent_session": true, // W-reparent: hub-enforced permissions
		// W-asks: owner questions (hub-enforced: children redirected, own asks only)
		"mcp__pyre__ask_owner": true, "mcp__pyre__find_asks": true, "mcp__pyre__withdraw_ask": true,
		// W-grants: proposals only file an owner ask; list own project/tree; revoke narrows
		"mcp__pyre__propose_grant": true, "mcp__pyre__propose_fact": true, "mcp__pyre__list_grants": true, "mcp__pyre__revoke_grant": true,
		// W-web-inbox: the lead's project page (hub-enforced)
		"mcp__pyre__update_project_page": true, "mcp__pyre__get_project_page": true,
		// setup review: own-turn waiting marker (self only)
		"mcp__pyre__waiting_on": true,
		// issues: reporting never prompts; updates are hub-limited to owner/triage
		"mcp__pyre__report_issue": true, "mcp__pyre__list_issues": true, "mcp__pyre__get_issue": true, "mcp__pyre__update_issue": true}
	got := map[string]bool{}
	for _, name := range claude.PyreMessagingAllowedTools {
		got[name] = true
		tl, ok := byName[name]
		if !ok {
			t.Errorf("pre-approved %s is not a pyre MCP tool", name)
			continue
		}
		if tl.Annotations == nil || tl.Annotations.DestructiveHint {
			t.Errorf("pre-approved %s is destructive", name)
		}
	}
	for name := range want {
		if !got[name] {
			t.Errorf("%s is not pre-approved", name)
		}
	}
	for name := range got {
		if !want[name] {
			t.Errorf("unexpected pre-approved tool %s", name)
		}
	}
	for _, name := range claude.DoorbellAllowedTools {
		if !got[name] {
			t.Errorf("doorbell tool %s missing from the messaging set", name)
		}
	}
	// Nothing that records the owner's words is pre-approved (setup review
	// PR #12: a session may only propose).
	for _, banned := range []string{"start_session", "stop_session", "interrupt_session", "answer_approval", "reissue_message", "resolve_delivery", "publish", "record_grant", "add_fact", "create_grant"} {
		if got["mcp__pyre__"+banned] || strings.Contains(strings.Join(claude.PyreMessagingAllowedTools, ","), banned) {
			t.Errorf("%s must not be pre-approved", banned)
		}
	}
}
