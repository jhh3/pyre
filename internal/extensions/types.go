package extensions

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jhh3/pyre/internal/contracts"
)

// Kind classifies an inventory item.
type Kind string

const (
	KindSkill     Kind = "skill"
	KindPlugin    Kind = "plugin"
	KindMCPServer Kind = "mcp_server"
	KindHook      Kind = "hook"
	KindCommand   Kind = "command"
	KindAgent     Kind = "agent"
	KindPrompt    Kind = "prompt"
)

// MarkerFile is the ownership marker Pyre writes into every directory it
// manages inside a harness config home (e.g. ~/.claude/skills/<name>/).
// Directories without it are user-owned and are never modified.
const MarkerFile = ".pyre-managed.json"

// Marker is the content of MarkerFile.
type Marker struct {
	Owner        string                `json:"owner"` // always "pyre"
	Name         string                `json:"name"`
	Kind         Kind                  `json:"kind"`
	Layer        contracts.ConfigLayer `json:"layer"`
	GenerationID string                `json:"generation_id"`
	Digest       contracts.Digest      `json:"digest"` // DigestDir of the directory without the marker
	Version      string                `json:"version,omitempty"`
}

// ReadMarker returns the marker in dir, or ok=false if absent/invalid.
func ReadMarker(dir string) (Marker, bool) {
	b, err := os.ReadFile(filepath.Join(dir, MarkerFile))
	if err != nil {
		return Marker{}, false
	}
	var m Marker
	if json.Unmarshal(b, &m) != nil || m.Owner != "pyre" {
		return Marker{}, false
	}
	return m, true
}

// HookDef is a secret-free view of a hook registration. Command is kept for
// local overlay generation and dedupe but is never serialized.
type HookDef struct {
	Event         string           `json:"event"`
	Matcher       string           `json:"matcher,omitempty"`
	Type          string           `json:"type"`
	Command       string           `json:"-"`
	CommandDigest contracts.Digest `json:"command_digest"`
	Timeout       int              `json:"timeout,omitempty"`
}

// HookIdentity is the dedupe identity of a hook registration.
func HookIdentity(h contracts.Harness, event, matcher, command string) string {
	return "hook:" + string(h) + ":" + event + ":" + matcher + ":" + string(DigestStrings(strings.TrimSpace(command)))[7:19]
}

// MCPDef is a secret-free view of an MCP server registration. Arguments,
// environment and header values are local-only.
type MCPDef struct {
	Name       string            `json:"name"`
	Transport  string            `json:"transport"` // "stdio" | "http" | "sse"
	Command    string            `json:"-"`
	Args       []string          `json:"-"`
	URL        string            `json:"-"`
	Env        map[string]string `json:"-"`
	Headers    map[string]string `json:"-"`
	EnvKeys    []string          `json:"env_keys,omitempty"`
	HeaderKeys []string          `json:"header_keys,omitempty"`
	DefDigest  contracts.Digest  `json:"def_digest"`
}

// HasSecretMaterial reports whether the definition carries env/header values
// that must not be copied into another file.
func (d MCPDef) HasSecretMaterial() bool {
	for _, v := range d.Env {
		if v != "" {
			return true
		}
	}
	for _, v := range d.Headers {
		if v != "" {
			return true
		}
	}
	return false
}

// Normalize fills Transport, EnvKeys, HeaderKeys and DefDigest. Two
// definitions with equal DefDigest are the same registration.
func (d *MCPDef) Normalize() {
	if d.Transport == "" {
		if d.URL != "" {
			d.Transport = "http"
		} else {
			d.Transport = "stdio"
		}
	}
	d.EnvKeys = sortedKeys(d.Env)
	d.HeaderKeys = sortedKeys(d.Headers)
	parts := []string{d.Transport, d.Command, d.URL}
	parts = append(parts, "args")
	parts = append(parts, d.Args...)
	parts = append(parts, "env")
	for _, k := range d.EnvKeys {
		parts = append(parts, k, string(DigestStrings(d.Env[k])))
	}
	parts = append(parts, "headers")
	for _, k := range d.HeaderKeys {
		parts = append(parts, k, string(DigestStrings(d.Headers[k])))
	}
	d.DefDigest = DigestStrings(parts...)
}

func sortedKeys(m map[string]string) []string {
	if len(m) == 0 {
		return nil
	}
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// MCPIdentity is the dedupe identity of an MCP server name for a harness.
func MCPIdentity(h contracts.Harness, name string) string {
	return "mcp:" + string(h) + ":" + name
}

// Item is one inventoried extension.
type Item struct {
	Kind     Kind                  `json:"kind"`
	Name     string                `json:"name"`
	Harness  contracts.Harness     `json:"harness"`
	Version  string                `json:"version,omitempty"`
	Digest   contracts.Digest      `json:"digest,omitempty"`
	Layer    contracts.ConfigLayer `json:"layer"`
	Managed  bool                  `json:"managed"`
	Scope    string                `json:"scope"`    // "user" | "project" | "local" | "plugin" | "system"
	Location string                `json:"location"` // display path (home shown as ~)
	Path     string                `json:"-"`        // absolute local path
	Enabled  *bool                 `json:"enabled,omitempty"`
	Identity string                `json:"identity"`
	Plugin   string                `json:"plugin,omitempty"` // providing plugin, if any
	Hook     *HookDef              `json:"hook,omitempty"`
	MCP      *MCPDef               `json:"mcp,omitempty"`
	Detail   string                `json:"detail,omitempty"`
}

// Ref converts the item to the shared contract shape. Source is
// "<kind>@<location>" because contracts.ExtensionRef has no kind field (a
// Kind field is requested from WP00).
func (it Item) Ref() contracts.ExtensionRef {
	return contracts.ExtensionRef{
		Name:    it.Name,
		Harness: it.Harness,
		Version: it.Version,
		Digest:  it.Digest,
		Layer:   it.Layer,
		Managed: it.Managed,
		Source:  string(it.Kind) + "@" + it.Location,
	}
}

// Homes names the config roots to scan. Empty fields are skipped.
type Homes struct {
	Home        string   // user's home directory, used for "~" display
	ClaudeHome  string   // ~/.claude or CLAUDE_CONFIG_DIR
	ClaudeJSON  string   // ~/.claude.json (user-scope MCP servers)
	CodexHome   string   // ~/.codex or CODEX_HOME
	ProjectDirs []string // project roots on this node
}

// DefaultHomes returns the standard locations under home.
func DefaultHomes(home string) Homes {
	return Homes{
		Home:       home,
		ClaudeHome: filepath.Join(home, ".claude"),
		ClaudeJSON: filepath.Join(home, ".claude.json"),
		CodexHome:  filepath.Join(home, ".codex"),
	}
}

// Display renders an absolute path with the home directory shown as "~".
func (h Homes) Display(p string) string {
	if h.Home != "" && (p == h.Home || strings.HasPrefix(p, h.Home+string(filepath.Separator))) {
		return "~" + p[len(h.Home):]
	}
	return p
}

// ManagedSkill is a skill package the Outfit integration (WP12) manages.
type ManagedSkill struct {
	Name    string
	Version string
	Digest  contracts.Digest
	Harness contracts.Harness // empty = both
	Dir     string            // local source directory
	Layer   contracts.ConfigLayer
}

// ManagedSource is the WP12 integration point for Outfit-managed skills.
type ManagedSource interface {
	ManagedSkills(ctx context.Context) ([]ManagedSkill, error)
}

// Inventory is a scan result.
type Inventory struct {
	Items    []Item                 `json:"items"`
	Roots    []contracts.ConfigRoot `json:"roots"`
	Warnings []string               `json:"warnings,omitempty"`
	// Duplicates lists identities registered more than once across sources
	// (kind "conflict"): the harness would run/register them twice.
	Duplicates []contracts.DriftItem `json:"duplicates,omitempty"`
}

// Refs flattens the inventory into contract refs.
func (inv Inventory) Refs() []contracts.ExtensionRef {
	out := make([]contracts.ExtensionRef, 0, len(inv.Items))
	for _, it := range inv.Items {
		out = append(out, it.Ref())
	}
	return out
}

// OfKind returns the items of one kind, optionally restricted to a harness.
func (inv Inventory) OfKind(k Kind, h contracts.Harness) []Item {
	var out []Item
	for _, it := range inv.Items {
		if it.Kind == k && (h == "" || it.Harness == h || it.Harness == "") {
			out = append(out, it)
		}
	}
	return out
}

// Identities returns the set of item identities.
func (inv Inventory) Identities() map[string]Item {
	m := make(map[string]Item, len(inv.Items))
	for _, it := range inv.Items {
		if _, ok := m[it.Identity]; !ok {
			m[it.Identity] = it
		}
	}
	return m
}
