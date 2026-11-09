package contracts

import "time"

// ConnectionState is the hub's view of a node's control connection.
type ConnectionState string

const (
	ConnConnected    ConnectionState = "connected"
	ConnDisconnected ConnectionState = "disconnected" // last_seen_at says when
	ConnNever        ConnectionState = "never"        // enrolled but never connected
	ConnRevoked      ConnectionState = "revoked"
)

// NodeLabelEnrollment is a hub-set node label. Value "pending" marks a node
// whose enrollment ticket has not been redeemed yet (connection state
// "never"); it cannot run sessions until the node completes enrollment.
const NodeLabelEnrollment = "pyre.enrollment"

// Node is an enrolled Mac/Linux machine. Offline never means idle.
type Node struct {
	NodeID       string            `json:"node_id"`
	Name         string            `json:"name"` // mutable alias, e.g. "server"
	Hostname     string            `json:"hostname,omitempty"`
	SSHAlias     string            `json:"ssh_alias,omitempty"`
	Platform     string            `json:"platform"` // "darwin" | "linux"
	Arch         string            `json:"arch"`     // "arm64" | "amd64"
	Harnesses    []HarnessInfo     `json:"harnesses"`
	Versions     map[string]string `json:"versions"` // pyred, protocol, tmux, ...
	Capabilities []string          `json:"capabilities"`
	Connection   NodeConnection    `json:"connection"`
	Resources    NodeResources     `json:"resources"`
	EnrolledAt   time.Time         `json:"enrolled_at"`
	Labels       map[string]string `json:"labels,omitempty"`
	// Avoid (additive) lists the node's avoid flags in force when read:
	// new work should stay off the node until each expires (see
	// node_avoid.go). Empty when none or from a hub without them.
	Avoid []NodeAvoid `json:"avoid,omitempty"`
}

// HarnessInfo reports an installed harness as probed by the node.
type HarnessInfo struct {
	Harness      Harness   `json:"harness"`
	Version      string    `json:"version"`
	Executable   string    `json:"executable,omitempty"`
	Available    bool      `json:"available"`
	Reason       string    `json:"reason,omitempty"` // why unavailable
	Capabilities []string  `json:"capabilities"`     // adapter capability set (see capabilities.go)
	ProbedAt     time.Time `json:"probed_at"`
	// Login identifies the machine's own (ambient) login for this harness,
	// read from the harness's files without writing them: identity only,
	// never a credential. Nil when there is none or it is unreadable.
	// Clients match it to an account (rota_ref = email, or provider_ref).
	Login *HarnessLogin `json:"login,omitempty"`
	// Models and Efforts (additive) list the models and reasoning efforts
	// the harness offers on this node, for start forms (pyre start --model
	// / --effort). Empty when unknown; free text is always allowed.
	Models  []ModelChoice `json:"models,omitempty"`
	Efforts []string      `json:"efforts,omitempty"`
	// Usage (additive) is the own login's subscription usage as the
	// harness itself reports it, for a harness whose login Pyre does not
	// manage (agy: `agy -p /usage`). Percentages and resets only; nil when
	// not read.
	Usage *HarnessUsage `json:"usage,omitempty"`
	// Limit (additive) is the usage or rate limit the own login of a
	// harness whose login Pyre does not manage (agy) reported on a run on
	// this node, while its reset is ahead: the kind, a short excerpt of
	// the harness's message and the reset. Clients do not place new runs
	// of the harness here until the reset (decision I72). Nil when none.
	Limit *LimitEvent `json:"limit,omitempty"`
}

// HarnessUsage is a reading of a machine's own harness login: the usage
// windows ("five_hour", "seven_day") and when they were read.
type HarnessUsage struct {
	Windows    []UsageWindow `json:"windows"`
	ObservedAt time.Time     `json:"observed_at"`
	Source     string        `json:"source,omitempty"` // e.g. "agy:/usage"
}

// HarnessLogin is the nonsecret identity of a machine's own harness login.
type HarnessLogin struct {
	Email       string `json:"email,omitempty"`
	ProviderRef string `json:"provider_ref,omitempty"` // ChatGPT account_id / Claude accountUuid
	Plan        string `json:"plan,omitempty"`
}

// NodeConnection is the hub-side connection observation.
type NodeConnection struct {
	State          ConnectionState `json:"state"`
	LastSeenAt     *time.Time      `json:"last_seen_at,omitempty"`
	ConnectedSince *time.Time      `json:"connected_since,omitempty"`
	RemoteAddr     string          `json:"remote_addr,omitempty"`
	Protocol       int             `json:"protocol,omitempty"`
}

// NodeResources is a replaceable resource summary.
type NodeResources struct {
	CPUCount   int     `json:"cpu_count"`
	Load1      float64 `json:"load1"`
	MemTotalMB int64   `json:"mem_total_mb"`
	MemAvailMB int64   `json:"mem_avail_mb"`
	ActiveRuns int     `json:"active_runs"`
	MaxRuns    int     `json:"max_runs"`     // advertised launch capacity; 0 = unlimited
	PyreRSSMB  float64 `json:"pyre_rss_mb"`  // daemon's own overhead
	PyreCPUPct float64 `json:"pyre_cpu_pct"` // daemon's own overhead
	// Supervised harness processes (agent/model work, not Pyre overhead).
	AgentRSSMB  float64   `json:"agent_rss_mb"`
	AgentCPUPct float64   `json:"agent_cpu_pct"`
	ObservedAt  time.Time `json:"observed_at"`

	// Machine stats (contract v1.8, additive; zero/absent = not sampled on
	// this platform or by an older pyred). Sampled once per heartbeat.

	// CPUPct is machine-wide CPU busy time over the last sample interval,
	// 0-100 across all cores (Linux /proc/stat deltas; macOS a 1 s iostat
	// sample).
	CPUPct      float64 `json:"cpu_pct,omitempty"`
	Load5       float64 `json:"load5,omitempty"`
	Load15      float64 `json:"load15,omitempty"`
	SwapTotalMB int64   `json:"swap_total_mb,omitempty"`
	SwapUsedMB  int64   `json:"swap_used_mb,omitempty"`
	// MemPressure is "normal", "warn" or "critical" ("" unknown): macOS
	// kern.memorystatus_vm_pressure_level; Linux derived from PSI
	// (MemPressureSome10 >= 10 warn, >= 40 critical).
	MemPressure string `json:"mem_pressure,omitempty"`
	// MemPressureSome10 is Linux PSI /proc/pressure/memory "some avg10":
	// the percentage of the last 10 s some task stalled on memory.
	MemPressureSome10 float64 `json:"mem_pressure_some10,omitempty"`
	// Disks covers the home filesystem and the Pyre data directory's
	// filesystem when it differs.
	Disks []DiskUsage `json:"disks,omitempty"`
	// Runs is per-run process-tree usage for this node's live runs.
	Runs []RunUsage `json:"runs,omitempty"`
	// MemoryProtection (additive, decision I66) describes the node's
	// memory protection in one line: the per-run scope caps and the memory
	// guard's trigger, e.g. "per-run scopes MemoryMax=19.5G
	// MemorySwapMax=2.0G OOMPolicy=continue; guard on (available < 5% and
	// swap > 80% used)". Empty from older nodes.
	MemoryProtection string `json:"memory_protection,omitempty"`
	// GitHub (additive) is the GitHub API budget of this node's gh login and
	// the sessions here that called gh most in the last hour (counted by
	// Pyre's gh shim, internal/ghguard). Nil when gh is absent and no run
	// called it.
	GitHub *GitHubBudget `json:"github,omitempty"`
	// Tools (additive) says which common command-line tools (ProbedTools)
	// pyred finds on its PATH (plus the usual user bin folders): name ->
	// present. Presence only: pyred never runs them. A tool missing from
	// the map is unknown (nil from an older pyred). Placement uses it for
	// pyre start --needs.
	Tools map[string]bool `json:"tools,omitempty"`
}

// ProbedTools are the command-line tools every node reports in
// NodeResources.Tools (machine-specific setups that decide where a task
// can run).
var ProbedTools = []string{"aws", "bun", "doppler", "docker", "ffmpeg", "gcloud", "gh", "git", "go",
	"jq", "kubectl", "node", "npm", "pnpm", "psql", "python3", "rg", "terraform", "uv", "xvfb-run"}

// GitHubBudget is a node's view of the GitHub API rate limit, which every
// agent on every machine using the same login shares. pyred samples
// `gh api rate_limit` (free: it does not count against the limit) at most
// once a minute.
type GitHubBudget struct {
	Core       GitHubLimit `json:"core"`
	GraphQL    GitHubLimit `json:"graphql"`
	ObservedAt time.Time   `json:"observed_at,omitzero"` // zero: never sampled
	// CallsLastHour counts gh calls by this node's runs in the last hour;
	// TopSessions lists the heaviest callers, most calls first.
	CallsLastHour int            `json:"calls_last_hour"`
	TopSessions   []GitHubCaller `json:"top_sessions,omitempty"`
}

// GitHubLimit is one rate-limit bucket ("core" REST, "graphql").
type GitHubLimit struct {
	Limit     int       `json:"limit"`
	Remaining int       `json:"remaining"`
	Reset     time.Time `json:"reset,omitzero"`
}

// GitHubLowFraction is the remaining share of a bucket below which Pyre
// warns sessions that call gh.
const GitHubLowFraction = 0.2

// Low reports a sampled bucket with less than GitHubLowFraction left.
func (l GitHubLimit) Low() bool {
	return l.Limit > 0 && float64(l.Remaining) < GitHubLowFraction*float64(l.Limit)
}

// Low reports a sampled budget with either bucket low.
func (b *GitHubBudget) Low() bool {
	return b != nil && !b.ObservedAt.IsZero() && (b.Core.Low() || b.GraphQL.Low())
}

// GitHubCaller is one session's gh use in the last hour: calls let through,
// calls delayed by the shim's throttle, and calls it refused.
type GitHubCaller struct {
	SessionID string `json:"session_id"`
	RunID     string `json:"run_id,omitempty"`
	Calls     int    `json:"calls"`
	Throttled int    `json:"throttled,omitempty"`
	Refused   int    `json:"refused,omitempty"`
}

// Memory pressure levels (NodeResources.MemPressure).
const (
	MemPressureNormal   = "normal"
	MemPressureWarn     = "warn"
	MemPressureCritical = "critical"
)

// DiskUsage is free/total space of one filesystem.
type DiskUsage struct {
	Path    string   `json:"path"`
	Roles   []string `json:"roles"` // "home", "data"
	TotalMB int64    `json:"total_mb"`
	FreeMB  int64    `json:"free_mb"` // available to the (unprivileged) owner
}

// RunUsage is the resource use of one run's process tree: the tmux pane
// process and every descendant (harness, node/bun, MCP servers,
// subagents, tool commands). RSS is summed (shared pages count once per
// process, so it overstates); CPU is the delta of cumulative user+system
// time, including reaped children, over the sample interval.
type RunUsage struct {
	RunID     string  `json:"run_id"`
	SessionID string  `json:"session_id,omitempty"`
	Procs     int     `json:"procs"`
	RSSMB     float64 `json:"rss_mb"`
	// CPUPct is a percentage of one core (200 = two cores busy); absent
	// until two samples of the same tree exist.
	CPUPct     float64    `json:"cpu_pct,omitempty"`
	Top        *ProcUsage `json:"top,omitempty"` // largest process by RSS
	ObservedAt time.Time  `json:"observed_at"`
}

// ProcUsage is one process of a run's tree.
type ProcUsage struct {
	PID   int     `json:"pid"`
	Name  string  `json:"name"`
	RSSMB float64 `json:"rss_mb"`
}

// EnrollmentToken is a one-time secret shown once to the owner, never stored
// in plaintext by the hub (only its hash). Not part of any record.
type EnrollmentRequest struct {
	Mutation
	Name     string `json:"name"`
	SSHAlias string `json:"ssh_alias,omitempty"`
	TTL      int    `json:"ttl_seconds,omitempty"`
}

// EnrollmentTicket is returned once to the owner. Secret is shown once.
type EnrollmentTicket struct {
	NodeID        string    `json:"node_id"`
	Secret        string    `json:"secret"` // one-time; exchanged by the node for a node credential
	ExpiresAt     time.Time `json:"expires_at"`
	HubURL        string    `json:"hub_url"`
	HubCertSHA256 string    `json:"hub_cert_sha256,omitempty"` // TLS pin for self-signed hubs
}

// NodeEnrollExchange is posted by a node to redeem a ticket.
type NodeEnrollExchange struct {
	NodeID   string `json:"node_id"`
	Secret   string `json:"secret"`
	Hostname string `json:"hostname"`
	Platform string `json:"platform"`
	Arch     string `json:"arch"`
}

// NodeCredential is returned once to the node; stored only in its local
// credential file (0600) and never logged.
type NodeCredential struct {
	NodeID string `json:"node_id"`
	Token  string `json:"token"`
}

// ClientPairing pairs an owner client (CLI on a new machine, Android).
type PairingRequest struct {
	Mutation
	ClientName string `json:"client_name"`
	Kind       string `json:"kind"` // "cli" | "android" | "tui"
	TTL        int    `json:"ttl_seconds,omitempty"`
}

type PairingTicket struct {
	PairingID     string    `json:"pairing_id"`
	Code          string    `json:"code"` // one-time
	HubURL        string    `json:"hub_url"`
	HubCertSHA256 string    `json:"hub_cert_sha256,omitempty"`
	ExpiresAt     time.Time `json:"expires_at"`
	// URI is a pyre://pair?... deep link/QR payload embedding the above.
	URI string `json:"uri"`
}

type PairingExchange struct {
	PairingID  string `json:"pairing_id"`
	Code       string `json:"code"`
	ClientName string `json:"client_name"`
	Kind       string `json:"kind"`
}

type ClientCredential struct {
	ClientID string `json:"client_id"`
	Token    string `json:"token"`
}

// ClientInfo describes a paired owner client (never includes its token).
type ClientInfo struct {
	ClientID   string     `json:"client_id"`
	Name       string     `json:"name"`
	Kind       string     `json:"kind"`
	CreatedAt  time.Time  `json:"created_at"`
	LastSeenAt *time.Time `json:"last_seen_at,omitempty"`
	Revoked    bool       `json:"revoked"`
}
