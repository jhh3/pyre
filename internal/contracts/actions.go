package contracts

import "time"

// Node action names for NodeActionRequest.Action. Payload types follow.
// Integration packages implement handlers on the node; the hub only routes.
const (
	ActionInventory       = "inventory"        // {} -> NodeInventory
	ActionEnvPlan         = "env.plan"         // EnvPlanParams -> ChangePlan
	ActionEnvApply        = "env.apply"        // EnvApplyParams -> EnvironmentGeneration
	ActionEnvDrift        = "env.drift"        // {} -> []DriftItem
	ActionEnvRollback     = "env.rollback"     // EnvRollbackParams -> EnvironmentGeneration
	ActionEnvResolve      = "env.resolve"      // EnvPlanParams -> EnvironmentGeneration (preview, no apply)
	ActionUpdateStage     = "update.stage"     // UpdateParams -> ToolVersion
	ActionUpdateActivate  = "update.activate"  // UpdateParams -> ToolVersion
	ActionUpdateRollback  = "update.rollback"  // UpdateParams -> ToolVersion
	ActionUpdateList      = "update.list"      // {} -> []ToolVersion
	ActionUpdatePrune     = "update.prune"     // {tool} -> []ToolVersion (keeps a rollback target and in-use versions)
	ActionUpdateSwapPlan  = "update.swap_plan" // {} -> daemon self-update drain plan
	ActionEnvProfileSet   = "env.profile.set"  // desired profile document (hub -> node)
	ActionEnvProfileGet   = "env.profile.get"
	ActionEnvAdoptPreview = "env.adopt.preview" // {} -> adoption preview (nothing moved)
	ActionConnCheck       = "connection.check"  // ConnectionParams -> ConnectionAvailability
	ActionConnInvoke      = "connection.invoke" // ConnectionInvokeParams -> ConnectionInvokeResult
	ActionConnGrant       = "connection.grant"  // ConnectionGrantParams -> Connection
	ActionConnRevoke      = "connection.revoke" // ConnectionGrantParams -> Connection
	ActionConnList        = "connection.list"   // {} -> []Connection
	// Cross-node credential provisioning (sealed transfer; only ciphertext
	// crosses the hub). Payload types live in internal/connections.
	ActionConnProvisionOffer  = "connection.provision_offer"
	ActionConnProvisionSeal   = "connection.provision_seal"
	ActionConnProvisionAccept = "connection.provision_accept"
	ActionConnDeprovision     = "connection.deprovision"
	ActionAccountList         = "account.list"         // {} -> []AccountView (node-local view)
	ActionAccountSelect       = "account.select"       // AccountSelectParams -> AccountSelection (dry run)
	ActionAccountObserve      = "account.observe"      // {provider?} -> []AccountObservation (bounded refresh of usage)
	ActionAccountInjectLimit  = "account.inject_limit" // InjectLimitParams -> FallbackPlan (test mode only)
	ActionAccountCheckouts    = "account.checkouts"    // {} -> []CheckoutView (rota checkouts on the node)
	ActionAccountCheckin      = "account.checkin"      // {account_id} -> CheckoutView (owner; refuses with live runs)
	ActionAccountRegister     = "account.register"     // AccountRegisterParams -> {profile, provider_ref, grant, verified, detail}
	ActionMemorySearch        = "memory.search"        // MemorySearchParams -> MemorySearchResult
	ActionMemoryGet           = "memory.get"           // {id} -> MemoryHit
	ActionMemoryStatus        = "memory.status"        // {} -> MemoryStatus
	ActionDirList             = "dir.list"             // DirListParams -> DirListResult (launch picker)
	ActionDiagnostics         = "diagnostics"          // {} -> Diagnostics
)

type NodeInventory struct {
	NodeID            string                       `json:"node_id"`
	CollectedAt       time.Time                    `json:"collected_at"`
	Harnesses         []HarnessInfo                `json:"harnesses"`
	Tools             []ToolVersion                `json:"tools"`
	Extensions        []ExtensionRef               `json:"extensions"` // skills, plugins, MCP servers found
	ConfigRoots       []ConfigRoot                 `json:"config_roots"`
	Drift             []DriftItem                  `json:"drift"`
	CurrentGeneration string                       `json:"current_generation,omitempty"`
	Integrations      map[string]IntegrationStatus `json:"integrations"` // "hive" | "outfit" | "rota"
}

type ConfigRoot struct {
	Harness Harness `json:"harness"`
	Path    string  `json:"path"`
	Exists  bool    `json:"exists"`
	Managed bool    `json:"managed"`
}

// IntegrationStatus reports a personal tool's presence/version on a node.
type IntegrationStatus struct {
	Present  bool   `json:"present"`
	Version  string `json:"version,omitempty"`
	Path     string `json:"path,omitempty"`
	Revision string `json:"revision,omitempty"`
	Dirty    bool   `json:"dirty,omitempty"` // checkout has local modifications (preserved)
	Health   string `json:"health,omitempty"`
}

type EnvPlanParams struct {
	Scope     string            `json:"scope"` // "global" | "project:<id>" | "session:<id>"
	Profile   string            `json:"profile,omitempty"`
	ProjectID string            `json:"project_id,omitempty"`
	Overrides map[string]string `json:"overrides,omitempty"`
}

type EnvApplyParams struct {
	PlanID       string `json:"plan_id"`
	ResultDigest Digest `json:"result_digest"` // must match the reviewed plan
}

type EnvRollbackParams struct {
	GenerationID string `json:"generation_id"`
}

type UpdateParams struct {
	Tool    string `json:"tool"`
	Version string `json:"version,omitempty"`
	Source  string `json:"source,omitempty"` // URL or package spec
	Digest  Digest `json:"digest,omitempty"`
	// SimulateHealthFailure is honored only in test mode.
	SimulateHealthFailure bool `json:"simulate_health_failure,omitempty"`
}

type ConnectionParams struct {
	ConnectionID string `json:"connection_id"`
}

type ConnectionInvokeParams struct {
	ConnectionID string            `json:"connection_id"`
	SessionID    string            `json:"session_id"`
	Operation    string            `json:"operation"`
	Args         map[string]string `json:"args,omitempty"`
}

type ConnectionInvokeResult struct {
	ConnectionID string  `json:"connection_id"`
	Output       RawJSON `json:"output,omitempty"` // never includes credentials
	Truncated    bool    `json:"truncated,omitempty"`
}

type ConnectionGrantParams struct {
	ConnectionID string `json:"connection_id"`
	NodeID       string `json:"node_id,omitempty"`
	ProjectID    string `json:"project_id,omitempty"`
	SessionID    string `json:"session_id,omitempty"`
}

// AccountRegisterParams mirrors a hub account onto a node and prepares its
// profile (account.register).
type AccountRegisterParams struct {
	Account Account `json:"account"` // account_id from the hub; provider; label; rota_ref
	Harness Harness `json:"harness"`
	// Source: "rota" (rota-checkout grant backed by Account.RotaRef) or
	// "login" (isolated profile for a fresh device login).
	Source string `json:"source"`
	// BindRotaIdentity sets Account.ProviderRef from the rota snapshot's
	// non-secret account id.
	BindRotaIdentity bool `json:"bind_rota_identity,omitempty"`
}

type AccountSelectParams struct {
	Provider Provider      `json:"provider"`
	Policy   AccountPolicy `json:"policy"`
}

type AccountSelection struct {
	AccountID string          `json:"account_id"`
	Provider  Provider        `json:"provider"`
	GrantID   string          `json:"grant_id,omitempty"`
	Reason    string          `json:"reason"`
	Freshness Freshness       `json:"freshness"`
	Outcome   FallbackOutcome `json:"outcome,omitempty"` // set when nothing eligible (waiting/intervention)
}

type InjectLimitParams struct {
	SessionID string `json:"session_id"`
	Kind      string `json:"kind"` // "usage_limit" | "auth_invalid"
}

type MemorySearchParams struct {
	Query     string `json:"query"`
	ProjectID string `json:"project_id,omitempty"`
	Limit     int    `json:"limit,omitempty"`
}

type MemoryHit struct {
	ID            string    `json:"id"`
	Kind          string    `json:"kind"` // "transcript" | "note" | "handoff" | "decision" | "document"
	Title         string    `json:"title"`
	Snippet       string    `json:"snippet"`
	Source        string    `json:"source"` // provenance: machine/session/path
	SessionRef    string    `json:"session_ref,omitempty"`
	ProjectID     string    `json:"project_id,omitempty"`
	At            time.Time `json:"at"`
	Authoritative bool      `json:"authoritative"` // true only for project documents/decisions
}

type MemorySearchResult struct {
	Hits      []MemoryHit `json:"hits"`
	Truncated bool        `json:"truncated"`
	Freshness Freshness   `json:"freshness"`
}

type MemoryStatus struct {
	Backend      string     `json:"backend"` // "hive" | "unavailable"
	Version      string     `json:"version,omitempty"`
	LastSyncAt   *time.Time `json:"last_sync_at,omitempty"`
	LastIndexAt  *time.Time `json:"last_index_at,omitempty"`
	PendingItems int        `json:"pending_items"`
	Detail       string     `json:"detail,omitempty"`
}

type DirListParams struct {
	Path string `json:"path,omitempty"` // default home
}

type DirListResult struct {
	Path  string   `json:"path"`
	Dirs  []string `json:"dirs"`
	IsGit bool     `json:"is_git"`
}

type Diagnostics struct {
	NodeID    string         `json:"node_id"`
	Uptime    int64          `json:"uptime_seconds"`
	Queues    map[string]int `json:"queues"` // outbox, inbox, reports pending
	Limits    map[string]int `json:"limits"`
	Resources NodeResources  `json:"resources"`
	Errors    []string       `json:"recent_errors"` // redacted
}
