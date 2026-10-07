package agentguide

import (
	"fmt"
	"strings"
)

const (
	TypedFirstRule = "Prefer the typed Outfit CLI whenever a matching operation is listed. Use `outfit exec` only for a selected CLI-mode capability that has no listed typed operation."
	LinkFirstRule  = "When the task contains an exact supported provider URL, use `outfit get URL` first; do not manually parse it unless Outfit reports that no resolver matched."
	WriteRule      = "Mutations are CLI-only and require explicit human intent plus `--write`."
)

func GeneratedRouting(connection string) string {
	return fmt.Sprintf("Start with `outfit describe %s`. Find a relevant operation with `outfit search '<task>'`, then use `outfit %s <operation> --help`. The universal `outfit run` and `outfit write` forms remain available. Do not guess argument names.", connection, connection)
}

func GeneratedExecutionPolicy() string {
	return TypedFirstRule + " Inputs accept schema flags, `KEY=value`, repeated `--arg`, `--json-args`, or `--input FILE|-`. Captured agent calls return the stable result envelope; use `--json` to force it. If `meta.spill` is present, read the private JSON file at `meta.spill.path`. Use bounded `--all` pagination when the whole collection is necessary. The compact read-only MCP facade is a discovery and dispatch fallback."
}

func ScopedUpstreamNotice(connection string) string {
	return fmt.Sprintf("> **outfit connection:** Use this skill only through `%s`. %s For capabilities with no listed typed operation, translate direct provider-CLI examples to `outfit exec %s -- ...`; Outfit selects the pinned binary and credential. %s", connection, GeneratedRouting(connection), connection, WriteRule)
}

func SessionContext(connections, needsAction []string, installedSkills int) string {
	text := "No active local Outfit connectors are available."
	if len(connections) > 0 {
		text = "Local Outfit connectors available: " + strings.Join(connections, ", ") + "."
	}
	text += " " + LinkFirstRule + " For tasks without a URL, use `outfit ls` or `outfit search`, then `outfit CONNECTION OPERATION --help`. " + GeneratedExecutionPolicy() + " " + WriteRule
	if len(needsAction) > 0 {
		text += " Connections needing human attention: " + strings.Join(needsAction, ", ") + ". Do not claim access through them; run `outfit connector guide NAME` for exact recovery."
	}
	if installedSkills > 0 {
		text += fmt.Sprintf(" Connector guidance is installed as %d host skill(s); use the matching skill when its connector is relevant.", installedSkills)
	}
	return text
}

// Audit rejects the contradictions that have historically caused generated,
// imported, and static host guidance to teach different invocation surfaces.
func Audit(text string) error {
	lower := strings.ToLower(text)
	if strings.Contains(lower, "outfit exec") && !strings.Contains(lower, "no listed typed operation") && !strings.Contains(lower, "without a typed operation") && !strings.Contains(lower, "unlisted capability") && !strings.Contains(lower, "curated surface lacks") {
		return fmt.Errorf("raw CLI guidance must limit outfit exec to capabilities with no listed typed operation")
	}
	if strings.Contains(lower, "prefer mcp") || strings.Contains(lower, "prefer the mcp") {
		return fmt.Errorf("guidance must prefer the typed Outfit CLI, not MCP")
	}
	if strings.Contains(lower, "mutation") && strings.Contains(lower, "outfit") && !strings.Contains(lower, "--write") && !strings.Contains(lower, "outfit write") {
		return fmt.Errorf("mutation guidance must require --write")
	}
	return nil
}
