package contracts

import "time"

// ConfigLayer names a configuration precedence layer. The frozen precedence
// (lowest to highest) is:
//
//	provider defaults -> adopted user configuration -> managed profile
//	(global, then project) -> explicit session overrides
//
// Secret resolution is separate and never part of a layer's values. The
// single merge implementation lives in internal/configuration (WP14); no
// other package may implement its own merge.
type ConfigLayer string

const (
	LayerProviderDefault ConfigLayer = "provider_default"
	LayerAdoptedUser     ConfigLayer = "adopted_user"
	LayerManagedGlobal   ConfigLayer = "managed_global"
	LayerManagedProject  ConfigLayer = "managed_project"
	LayerSession         ConfigLayer = "session"
)

// LayerOrder is the authoritative precedence order (index = precedence).
var LayerOrder = []ConfigLayer{LayerProviderDefault, LayerAdoptedUser, LayerManagedGlobal, LayerManagedProject, LayerSession}

// EnvironmentGeneration is an immutable resolved environment revision.
type EnvironmentGeneration struct {
	GenerationID     string            `json:"generation_id"`
	NodeID           string            `json:"node_id"`
	Profile          string            `json:"profile"`
	ProjectID        *string           `json:"project_id"`
	CreatedAt        time.Time         `json:"created_at"`
	RuntimeVersions  map[string]string `json:"runtime_versions"` // harness/tool -> version
	ConfigDigests    map[string]Digest `json:"config_digests"`   // managed file -> digest
	Skills           []ExtensionRef    `json:"skills"`
	Plugins          []ExtensionRef    `json:"plugins"`
	MCPServers       []ExtensionRef    `json:"mcp_servers"`
	ConnectionGrants []string          `json:"connection_grants"` // connection IDs; never secrets
	Values           []ResolvedValue   `json:"values,omitempty"`  // explain which layer supplied each value
	Digest           Digest            `json:"digest"`
	PreviousID       string            `json:"previous_id,omitempty"`
}

// ExtensionRef identifies a skill/plugin/MCP server and where it came from.
type ExtensionRef struct {
	Name    string      `json:"name"`
	Kind    string      `json:"kind,omitempty"`    // "skill" | "plugin" | "mcp_server" | "hook" | "command" | "agent" | "prompt"
	Harness Harness     `json:"harness,omitempty"` // empty = both
	Version string      `json:"version,omitempty"`
	Digest  Digest      `json:"digest,omitempty"`
	Layer   ConfigLayer `json:"layer"`
	Managed bool        `json:"managed"` // false = adopted/unmanaged user customization
	Source  string      `json:"source,omitempty"`
}

// ResolvedValue explains one effective setting.
type ResolvedValue struct {
	Key             string        `json:"key"`
	Value           string        `json:"value"` // redacted if sensitive
	Layer           ConfigLayer   `json:"layer"`
	Overrode        []ConfigLayer `json:"overrode,omitempty"`
	RestartRequired bool          `json:"restart_required,omitempty"`
}

// DriftItem reports a managed item that differs from the applied generation,
// or an unmanaged change in a managed file.
type DriftItem struct {
	Path     string `json:"path"`
	Key      string `json:"key,omitempty"`
	Kind     string `json:"kind"` // "modified" | "missing" | "unmanaged_addition" | "conflict" | "version_mismatch"
	Expected string `json:"expected,omitempty"`
	Actual   string `json:"actual,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

// ChangePlan is a reviewable diff produced before any apply.
type ChangePlan struct {
	PlanID          string       `json:"plan_id"`
	NodeID          string       `json:"node_id"`
	Scope           string       `json:"scope"` // "global" | "project:<id>" | "session:<id>"
	Changes         []PlanChange `json:"changes"`
	Conflicts       []DriftItem  `json:"conflicts"`
	RestartRequired []string     `json:"restart_required"` // session IDs affected only after restart
	ResultDigest    Digest       `json:"result_digest"`
}

type PlanChange struct {
	Path   string `json:"path"`
	Action string `json:"action"` // "create" | "update" | "delete" | "register" | "unregister"
	Diff   string `json:"diff,omitempty"`
	Backup string `json:"backup,omitempty"`
}

// ToolVersion tracks an installed or staged tool version on a node.
type ToolVersion struct {
	Tool    string `json:"tool"` // "claude" | "codex" | "pyred" | plugin name
	Version string `json:"version"`
	Digest  Digest `json:"digest,omitempty"`
	Path    string `json:"path,omitempty"`
	State   string `json:"state"` // "active" | "staged" | "previous" | "failed"
	// Managed is true for Pyre-staged versions; false for external installs
	// (the user's own claude/codex) which Pyre reports but never changes.
	Managed     bool      `json:"managed"`
	InstalledAt time.Time `json:"installed_at"`
	Health      string    `json:"health,omitempty"` // "ok" | "failed: ..." | "unchecked"
	InUseByRuns []string  `json:"in_use_by_runs,omitempty"`
}

// Connection is a named Outfit connection catalog entry (metadata only).
type Connection struct {
	ConnectionID string   `json:"connection_id"`
	Name         string   `json:"name"`    // e.g. "github-personal"
	Service      string   `json:"service"` // e.g. "github", "linear"
	Modes        []string `json:"modes"`   // "local_credential" | "remote_invoke"
	// Availability per node; credentials themselves stay with Outfit/OS stores.
	Availability []ConnectionAvailability `json:"availability"`
	Scope        ConnectionScope          `json:"scope"`
	Generation   int64                    `json:"generation"` // bumps on grant/revoke
}

type ConnectionAvailability struct {
	NodeID     string    `json:"node_id"`
	State      string    `json:"state"` // "verified" | "missing" | "expired" | "locked_store" | "unknown"
	Mode       string    `json:"mode"`
	VerifiedAt time.Time `json:"verified_at"`
	Detail     string    `json:"detail,omitempty"`
}

type ConnectionScope struct {
	// Access disambiguates empty lists: "broad" (explicit owner-broad grant),
	// "scoped" (only the listed nodes/projects/sessions), "none" (no grants).
	Access   string   `json:"access,omitempty"`
	Nodes    []string `json:"nodes,omitempty"`
	Projects []string `json:"projects,omitempty"` // empty = all projects
	Sessions []string `json:"sessions,omitempty"`
}
