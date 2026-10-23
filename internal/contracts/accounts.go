package contracts

import (
	"strings"
	"time"
)

// Account is a provider subscription identity. It is separate from each
// device's login/grant and from a run's assignment.
type Account struct {
	AccountID string    `json:"account_id"`
	Provider  Provider  `json:"provider"`
	Label     string    `json:"label"` // friendly name; never an email unless the owner chose it
	Kind      string    `json:"kind"`  // "subscription" (API billing accounts are never fallback targets)
	Disabled  bool      `json:"disabled"`
	CreatedAt time.Time `json:"created_at"`
	// RotaRef links to the Rota account record on nodes (nonsecret name).
	RotaRef string `json:"rota_ref,omitempty"`
	// ProviderRef is the provider's nonsecret account identifier used to
	// verify runtime identity (Claude accountUuid, ChatGPT account_id).
	ProviderRef string `json:"provider_ref,omitempty"`
	Plan        string `json:"plan,omitempty"`
	// PerMachineIndependent marks an account whose login on each machine is
	// a separate refresh family (never copied with rota export/import), so
	// nodes may check it out without an exclusive fleet lease.
	PerMachineIndependent bool `json:"per_machine_independent,omitempty"`
	// Tier orders automatic selection among a provider's accounts: a
	// reserve account takes new work only when every normal account is at,
	// near (usage.NearPercent) or past its limit or ineligible. Empty is
	// normal (readers that predate tiers see every account as normal).
	Tier AccountTier `json:"tier,omitempty"`
}

// AccountTier is an account's selection tier (Account.Tier).
type AccountTier string

const (
	// TierNormal is the default; stored as "" (TierNormal on input).
	TierNormal AccountTier = "normal"
	// TierReserve: used only after every normal account is near or at its
	// limit, or ineligible (accounts/select.go).
	TierReserve AccountTier = "reserve"
)

// Reserve reports whether the account is in the reserve tier.
func (a Account) Reserve() bool { return a.Tier == TierReserve }

// TierName is the tier in words ("normal" for the empty default).
func (a Account) TierName() AccountTier {
	if a.Tier == "" {
		return TierNormal
	}
	return a.Tier
}

// ParseAccountTier accepts "normal" or "reserve" (case-insensitive) and
// returns the stored form ("" for normal).
func ParseAccountTier(s string) (AccountTier, bool) {
	switch AccountTier(strings.ToLower(strings.TrimSpace(s))) {
	case TierNormal, "":
		return "", true
	case TierReserve:
		return TierReserve, true
	}
	return "", false
}

// CredentialKind is the verified mechanism by which a device grant binds an
// account to a run (docs/evidence/accounts/REPORT.md §5–§6).
type CredentialKind string

const (
	// Claude profile store: CLAUDE_CONFIG_DIR=<canonical dir>; macOS keychain
	// item "Claude Code-credentials-<sha256(dir)[:8]>", Linux
	// <dir>/.credentials.json. Harness refreshes; N processes per host OK.
	CredClaudeProfileStore CredentialKind = "claude-profile-store"
	// Claude setup-token passed via CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR;
	// never refreshes; no identity signal.
	CredClaudeTokenFD CredentialKind = "claude-oauth-token-fd"
	// Codex home: CODEX_HOME=<dir>, auth.json with
	// cli_auth_credentials_store="file". Harness refreshes WITHOUT a
	// cross-process lock => at most one Codex process per home (lease).
	CredCodexHomeFile CredentialKind = "codex-home-file"
	// The node's existing default login (no Pyre isolation); reported as
	// ambient and never used for automatic fallback.
	CredAmbient CredentialKind = "ambient"
	// Rota snapshot checked out into an isolated profile: single owner per
	// node (lock next to the snapshot) and per fleet (hub AccountLease);
	// rotated tokens are written back to the snapshot; never the live
	// default login.
	CredRotaCheckout CredentialKind = "rota-checkout"
)

// AccountLease is the hub-side refresh-family lease: at most one node holds
// an account's login at a time (unless the account is
// PerMachineIndependent). Leases expire on their own; holders renew at
// half-life.
type AccountLease struct {
	LeaseID    string     `json:"lease_id"`
	AccountID  string     `json:"account_id"`
	NodeID     string     `json:"node_id"`
	Holder     string     `json:"holder"` // e.g. "rota-checkout:<grant_id>"
	AcquiredAt time.Time  `json:"acquired_at"`
	ExpiresAt  time.Time  `json:"expires_at"`
	ReleasedAt *time.Time `json:"released_at,omitempty"`
}

// AcquireAccountLeaseRequest (node principal). The hub returns ErrConflict
// naming the holder node while another holder has an unexpired lease; the
// same node and holder get the existing lease back (extended). TTLSeconds
// <= 0 selects 30 minutes; the hub clamps to 60..3600.
type AcquireAccountLeaseRequest struct {
	Mutation
	AccountID  string `json:"account_id"`
	Holder     string `json:"holder"`
	TTLSeconds int    `json:"ttl_seconds,omitempty"`
}

// RenewAccountLeaseRequest extends a lease held by the calling node.
type RenewAccountLeaseRequest struct {
	Mutation
	LeaseID    string `json:"lease_id"`
	TTLSeconds int    `json:"ttl_seconds,omitempty"`
}

// ReleaseAccountLeaseRequest ends a lease held by the calling node
// (releasing a released lease returns it unchanged).
type ReleaseAccountLeaseRequest struct {
	Mutation
	LeaseID string `json:"lease_id"`
}

// GrantState describes a device login.
type GrantState string

const (
	GrantValid    GrantState = "valid"
	GrantExpiring GrantState = "expiring" // refresh token near its fixed end of life
	GrantExpired  GrantState = "expired"  // refresh needed/failed; reauth required
	GrantDead     GrantState = "dead"     // provider rejected the login; no retries; intervention
	GrantRevoked  GrantState = "revoked"
	GrantUnknown  GrantState = "unknown"
)

// DeviceGrant is a particular node's login for an account in an isolated
// profile. Credential bytes stay in the node's store; ProfileRef is opaque.
type DeviceGrant struct {
	GrantID    string         `json:"grant_id"`
	AccountID  string         `json:"account_id"`
	NodeID     string         `json:"node_id"`
	Harness    Harness        `json:"harness"`
	ProfileRef string         `json:"profile_ref"` // opaque local profile name
	Mechanism  string         `json:"mechanism"`   // human-readable mechanism description
	Kind       CredentialKind `json:"kind"`
	// ProfileDir is the canonical absolute profile path on NodeID (the
	// Claude keychain item name depends on the exact string).
	ProfileDir string `json:"profile_dir,omitempty"`
	// MaxProcs bounds concurrent harness processes on this grant
	// (codex-home-file: 1). 0 = unlimited.
	MaxProcs int        `json:"max_procs"`
	State    GrantState `json:"state"`
	// RefreshOwner: "harness" (only the harness process bound to this profile
	// refreshes) | "none" (non-refreshing token). Pyre, Rota and usage probes
	// never refresh a harness-owned grant; grants are never shared across nodes.
	RefreshOwner string    `json:"refresh_owner"`
	Generation   int64     `json:"generation"` // bumps on reauth; stale owners rejected
	ObservedAt   time.Time `json:"observed_at"`
	Detail       string    `json:"detail,omitempty"`
}

// AccountAssignment binds an account (via a grant) to exactly one run.
// It is a local execution binding, not a provider quota reservation.
type AccountAssignment struct {
	AssignmentID string `json:"assignment_id"`
	AccountID    string `json:"account_id"`
	GrantID      string `json:"grant_id,omitempty"`
	// CredentialKind records the binding mechanism (CredAmbient when the
	// node's default login was used without Pyre isolation).
	CredentialKind CredentialKind `json:"credential_kind,omitempty"`
	// GrantGeneration pins the grant generation; a stale generation is rejected.
	GrantGeneration int64 `json:"grant_generation"`
	// Lease is the per-grant process lease held by this run (codex homes).
	Lease      string     `json:"lease,omitempty"`
	RunID      string     `json:"run_id"`
	SessionID  string     `json:"session_id"`
	NodeID     string     `json:"node_id"`
	Reason     string     `json:"reason"` // selection explanation
	AssignedAt time.Time  `json:"assigned_at"`
	ReleasedAt *time.Time `json:"released_at,omitempty"`
	Freshness  Freshness  `json:"freshness"` // of the availability data used to decide
}

// Eligibility of an account for new work.
type Eligibility string

const (
	EligibleYes       Eligibility = "eligible"
	EligibleCooldown  Eligibility = "cooldown" // limited until ResetAt
	EligibleExhausted Eligibility = "exhausted"
	EligibleUnusable  Eligibility = "unusable" // login invalid/expired
	EligibleUnknown   Eligibility = "unknown"  // stale or unavailable data; never treated as unlimited
)

// AccountObservation is a sampled usage/health observation. Never contains
// a credential.
type AccountObservation struct {
	AccountID     string        `json:"account_id"`
	Provider      Provider      `json:"provider"`
	ReportingNode string        `json:"reporting_node"`
	ObservedAt    time.Time     `json:"observed_at"`
	Source        string        `json:"source"`     // e.g. "rota:usage-endpoint", "codex:app-server", "limit-event", "mock"
	Confidence    string        `json:"confidence"` // "high" | "medium" | "low"
	Eligibility   Eligibility   `json:"eligibility"`
	Windows       []UsageWindow `json:"windows,omitempty"`
	ResetAt       *time.Time    `json:"reset_at,omitempty"`
	Stale         bool          `json:"stale"`
	Detail        string        `json:"detail,omitempty"`
}

// UsageWindow is a provider-reported utilization window (e.g. 5h, weekly).
type UsageWindow struct {
	Name        string     `json:"name"`         // "five_hour" | "seven_day" | ...
	UsedPercent float64    `json:"used_percent"` // 0..100
	ResetAt     *time.Time `json:"reset_at,omitempty"`
}

// AccountView aggregates for display.
type AccountView struct {
	Account Account             `json:"account"`
	Latest  *AccountObservation `json:"latest,omitempty"`
	// ByNode is the newest observation from each reporting node, ordered by
	// node ID. An account with grants on several nodes (each with its own
	// login) can be eligible on one node and unknown or limited on another;
	// Latest alone is whichever node reported last.
	ByNode      []AccountObservation `json:"by_node,omitempty"`
	Grants      []DeviceGrant        `json:"grants"`
	Assignments []AccountAssignment  `json:"active_assignments"`
}

type ListAccountsRequest struct {
	PageRequest
	Provider Provider `json:"provider,omitempty"`
}

// FallbackOutcome is the result of planning recovery from a limit/failure.
type FallbackOutcome string

const (
	FallbackNativeResume FallbackOutcome = "native_resume" // continuation with another eligible same-provider account
	FallbackHandoff      FallbackOutcome = "handoff"       // checkpoint/handoff available; native resume unsupported
	FallbackWaiting      FallbackOutcome = "waiting"       // waiting for account/reset
	FallbackIntervention FallbackOutcome = "intervention"  // human action required
)

// FallbackPlan is reported (and persisted as an event) when a run hits a
// limit or unusable login.
type FallbackPlan struct {
	SessionID     string          `json:"session_id"`
	FromRunID     string          `json:"from_run_id"`
	FromAccountID string          `json:"from_account_id"`
	Outcome       FallbackOutcome `json:"outcome"`
	ToAccountID   string          `json:"to_account_id,omitempty"`
	NewRunID      string          `json:"new_run_id,omitempty"`
	Reason        string          `json:"reason"`
	// UncertainWork is true if the interrupted run may have had in-flight
	// external effects; its last instruction is NOT replayed automatically.
	UncertainWork bool      `json:"uncertain_work"`
	Provider      Provider  `json:"provider,omitempty"`
	PlannedAt     time.Time `json:"planned_at"`
}
