package skillgen

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jhh3/outfit/internal/agentguide"
	"github.com/jhh3/outfit/internal/invoke"
	"github.com/jhh3/outfit/internal/kit"
	"github.com/jhh3/outfit/internal/state"
)

func Render(connection state.Connection, pkg kit.Package, mode kit.Mode) string {
	var body strings.Builder
	fmt.Fprintf(&body, "---\nname: outfit-%s\ndescription: %s Use when this task needs the locally configured %s connection.\n---\n\n", connection.Name, yamlSafe(pkg.Skill.Summary), pkg.Display)
	fmt.Fprintf(&body, "# %s through outfit\n\nThis machine has `%s` selected in **%s** mode. Use only that mode; never infer or activate another one.\n\n", pkg.Display, connection.Name, mode.Label)
	body.WriteString(agentguide.GeneratedRouting(connection.Name) + "\n\n")
	body.WriteString(agentguide.GeneratedExecutionPolicy() + "\n\n")
	body.WriteString("Reads run directly. " + agentguide.WriteRule + " Outfit supplies the selected credential and executor; do not call or authenticate the underlying provider transport separately.\n\n")
	linkTypes := selectedLinkTypes(pkg, mode.ID)
	if len(linkTypes) > 0 {
		body.WriteString("## Pasted links\n\n")
		body.WriteString("When the task contains an exact supported provider URL, run `outfit get 'URL'` first. This is the preferred selected-mode route: it validates the URL, runs the connector lookup, and applies the configured heavy/light context policy. Do not manually parse a supported URL into lower-level operation arguments unless `outfit get` reports that no resolver matched.\n\n")
		fmt.Fprintf(&body, "Supported resource types in this mode: %s.\n", strings.Join(linkTypes, ", "))
	}
	operations := append([]kit.Operation{}, mode.Operations...)
	sort.Slice(operations, func(i, j int) bool { return operations[i].Name < operations[j].Name })
	if len(operations) > 0 {
		fmt.Fprintf(&body, "## Available operations\n\nThis selected mode currently exposes %d operation(s). The names below are a routing index, not an argument schema; use `outfit describe` for the authoritative details.\n", len(operations))
		classes := []string{"read", "write", "destructive"}
		seen := map[string]bool{}
		for _, class := range classes {
			renderOperationClass(&body, operations, class)
			seen[class] = true
		}
		for _, operation := range operations {
			if !seen[operation.Class] {
				renderOperationClass(&body, operations, operation.Class)
				seen[operation.Class] = true
			}
		}
		body.WriteString("\n## Typed CLI entry points\n\n")
		for _, operation := range operations {
			friendly := invoke.FriendlyOperationName(pkg.Key, operation.Name)
			suffix := ""
			if operation.Class != "read" {
				suffix = " --write"
			}
			fmt.Fprintf(&body, "- `%s`: `outfit %s %s --help%s`\n", operation.Name, connection.Name, friendly, suffix)
		}
	}
	if len(pkg.Skill.Guidance) > 0 {
		body.WriteString("\n## Guidance\n\n")
		for _, item := range pkg.Skill.Guidance {
			fmt.Fprintf(&body, "- %s\n", item)
		}
	}
	if len(pkg.Skill.Workflows) > 0 {
		body.WriteString("\n## Common workflows\n")
		for _, workflow := range pkg.Skill.Workflows {
			fmt.Fprintf(&body, "\n### %s\n\n", workflow.Title)
			for index, step := range workflow.Steps {
				fmt.Fprintf(&body, "%d. %s\n", index+1, step)
			}
			fmt.Fprintf(&body, "\nRender the live recipe and its exact selected-mode entry points with `outfit workflow show %s %s`.\n", connection.Name, workflowSlug(workflow.Title))
		}
	}
	if len(pkg.Skill.Examples) > 0 {
		body.WriteString("\n## Examples\n\n")
		for _, item := range pkg.Skill.Examples {
			fmt.Fprintf(&body, "```bash\n%s\n```\n", strings.ReplaceAll(item, "outfit run "+pkg.Key, "outfit run "+connection.Name))
		}
	}
	return body.String()
}

func selectedLinkTypes(pkg kit.Package, mode string) []string {
	seen := map[string]bool{}
	var result []string
	for _, resolver := range pkg.Links {
		for _, lookup := range resolver.LookupOptions() {
			if lookup.Mode != "" && lookup.Mode != mode {
				continue
			}
			if !seen[resolver.ResourceType] {
				seen[resolver.ResourceType] = true
				result = append(result, resolver.ResourceType)
			}
			break
		}
	}
	sort.Strings(result)
	return result
}

func workflowSlug(title string) string {
	var output strings.Builder
	dash := false
	for _, char := range strings.ToLower(title) {
		if char >= 'a' && char <= 'z' || char >= '0' && char <= '9' {
			output.WriteRune(char)
			dash = false
		} else if output.Len() > 0 && !dash {
			output.WriteByte('-')
			dash = true
		}
	}
	return strings.Trim(output.String(), "-")
}

func renderOperationClass(body *strings.Builder, operations []kit.Operation, class string) {
	var names []string
	for _, operation := range operations {
		if operation.Class == class {
			names = append(names, "`"+operation.Name+"`")
		}
	}
	if len(names) == 0 {
		return
	}
	label := class
	if label == "" {
		label = "other"
	}
	fmt.Fprintf(body, "\n### %s (%d)\n\n%s\n", strings.ToUpper(label[:1])+label[1:], len(names), strings.Join(names, ", "))
}

func Sync(host string, connections []state.Connection, packages map[string]kit.Package, tools map[string][]state.Tool, allowed func(string, string) bool) ([]string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	roots := []string{}
	if host == "both" || host == "codex" {
		codexRoot := os.Getenv("CODEX_HOME")
		if codexRoot == "" {
			codexRoot = filepath.Join(home, ".codex")
		}
		roots = append(roots, filepath.Join(codexRoot, "skills"))
	}
	if host == "both" || host == "claude" {
		claudeRoot := os.Getenv("CLAUDE_CONFIG_DIR")
		if claudeRoot == "" {
			claudeRoot = filepath.Join(home, ".claude")
		}
		roots = append(roots, filepath.Join(claudeRoot, "skills"))
	}
	if len(roots) == 0 {
		return nil, fmt.Errorf("host must be codex, claude, or both")
	}
	return SyncRoots(roots, connections, packages, tools, allowed)
}

func SyncRoots(roots []string, connections []state.Connection, packages map[string]kit.Package, tools map[string][]state.Tool, allowed func(string, string) bool) ([]string, error) {
	var written []string
	for _, root := range roots {
		if err := os.MkdirAll(root, 0o700); err != nil {
			return nil, err
		}
		legacyEntries, _ := filepath.Glob(filepath.Join(root, "cw-*"))
		for _, entry := range legacyEntries {
			if _, err := os.Stat(filepath.Join(entry, ".cw-generated")); err == nil {
				if err := os.RemoveAll(entry); err != nil {
					return nil, err
				}
			}
		}
		entries, _ := filepath.Glob(filepath.Join(root, "outfit-*"))
		for _, entry := range entries {
			if _, err := os.Stat(filepath.Join(entry, ".outfit-generated")); err == nil {
				_ = os.RemoveAll(entry)
			}
		}
		for _, connection := range connections {
			if connection.Status != "active" {
				continue
			}
			pkg, ok := packages[connection.ConnectorKey]
			if !ok {
				continue
			}
			var mode kit.Mode
			for _, candidate := range pkg.Modes {
				if candidate.ID == connection.SelectedMode {
					mode = candidate
				}
			}
			if allowed != nil {
				operations := mode.Operations[:0]
				for _, operation := range mode.Operations {
					if allowed(connection.Name, operation.Name) {
						operations = append(operations, operation)
					}
				}
				mode.Operations = operations
				pkg.Skill.Examples = nil
				pkg.Skill.Workflows = nil
			}
			known := map[string]bool{}
			for _, operation := range mode.Operations {
				known[operation.Name] = true
			}
			for _, tool := range tools[connection.Name] {
				if !known[tool.Name] && (allowed == nil || allowed(connection.Name, tool.Name)) {
					mode.Operations = append(mode.Operations, kit.Operation{Name: tool.Name, Title: tool.Title, Description: tool.Description, Class: tool.Class, Input: tool.InputSchema, MCPTool: tool.SourceName})
				}
			}
			directory := filepath.Join(root, "outfit-"+connection.Name)
			if err := os.MkdirAll(directory, 0o700); err != nil {
				return nil, err
			}
			path := filepath.Join(directory, "SKILL.md")
			if err := os.WriteFile(path, []byte(Render(connection, pkg, mode)), 0o600); err != nil {
				return nil, err
			}
			if err := os.WriteFile(filepath.Join(directory, ".outfit-generated"), []byte("generated by outfit; safe to replace\n"), 0o600); err != nil {
				return nil, err
			}
			written = append(written, path)
		}
	}
	return written, nil
}

func yamlSafe(value string) string { return strings.ReplaceAll(strings.TrimSpace(value), "\n", " ") }
