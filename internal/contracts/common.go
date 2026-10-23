package contracts

import (
	"encoding/json"
	"time"
)

// ObjectKind names a referencable record type.
type ObjectKind string

const (
	KindNode             ObjectKind = "node"
	KindSession          ObjectKind = "session"
	KindRun              ObjectKind = "run"
	KindProject          ObjectKind = "project"
	KindMessage          ObjectKind = "message"
	KindThread           ObjectKind = "thread"
	KindDocument         ObjectKind = "document"
	KindDocumentRevision ObjectKind = "document_revision"
	KindProposal         ObjectKind = "document_proposal"
	KindArtifact         ObjectKind = "artifact"
	KindArtifactVersion  ObjectKind = "artifact_version"
	KindUpload           ObjectKind = "upload"
	KindComment          ObjectKind = "comment"
	KindAccount          ObjectKind = "account"
	KindGrant            ObjectKind = "device_grant"
	KindAssignment       ObjectKind = "account_assignment"
	KindEnvironment      ObjectKind = "environment_generation"
	KindApproval         ObjectKind = "approval"
	KindLease            ObjectKind = "input_lease"
	KindAttention        ObjectKind = "attention"
	KindConnection       ObjectKind = "connection"
	KindNote             ObjectKind = "note"
	KindOperation        ObjectKind = "operation"
	KindCommand          ObjectKind = "node_command"
	KindClient           ObjectKind = "client"
)

// ObjectRef is a stable reference to any record.
type ObjectRef struct {
	Kind ObjectKind `json:"kind"`
	ID   string     `json:"id"`
}

// PrincipalKind distinguishes authenticated identities.
type PrincipalKind string

const (
	PrincipalOwner   PrincipalKind = "owner"   // the human owner via an enrolled client
	PrincipalNode    PrincipalKind = "node"    // an enrolled machine daemon
	PrincipalSession PrincipalKind = "session" // an agent session's scoped credential
	PrincipalSystem  PrincipalKind = "system"  // hub-internal actions (expiry, migration)
)

// Principal is an actor identity as established by authentication, never by
// a client-supplied claim. OnBehalfOf records authorized delegation.
type Principal struct {
	Kind       PrincipalKind `json:"kind"`
	ID         string        `json:"id"`
	Name       string        `json:"name,omitempty"` // display alias only
	OnBehalfOf *Principal    `json:"on_behalf_of,omitempty"`
}

// Freshness describes how current an observation is. Stale/unknown
// observations must be shown as such; they never imply idle or failed.
type Freshness struct {
	ObservedAt time.Time `json:"observed_at"`
	Source     string    `json:"source"` // e.g. "node:node_x", "cache", "mock"
	Stale      bool      `json:"stale"`
	Reason     string    `json:"reason,omitempty"`
}

// Page is a cursor-paginated list. NextCursor is empty at the end.
// SnapshotAt is the server time at which the page was read.
type Page[T any] struct {
	Items      []T       `json:"items"`
	NextCursor string    `json:"next_cursor,omitempty"`
	SnapshotAt time.Time `json:"snapshot_at"`
}

// PageRequest is embedded in list requests.
type PageRequest struct {
	Cursor string `json:"cursor,omitempty"`
	Limit  int    `json:"limit,omitempty"` // server clamps; 0 = default (50)
}

// Mutation is embedded in every mutating request.
type Mutation struct {
	// OperationID is client-generated and stable across retries of the same
	// logical operation. Required.
	OperationID string `json:"operation_id"`
	// CorrelationID optionally links related operations (e.g. a reissue).
	CorrelationID string `json:"correlation_id,omitempty"`
}

// Harness identifies a runtime kind.
type Harness string

const (
	HarnessClaude Harness = "claude"
	HarnessCodex  Harness = "codex"
	// HarnessAgy is Google Antigravity's CLI (agy). Pyre runs it on the
	// node's own login only: agy has no config-dir switch, so there is no
	// isolated per-account profile (account.isolated_profile unsupported).
	HarnessAgy   Harness = "agy"
	HarnessShell Harness = "shell"
	HarnessMock  Harness = "mock" // deterministic mock runtime; never runtime evidence
)

// Provider identifies a subscription provider. Fallback never crosses providers.
type Provider string

const (
	ProviderAnthropic Provider = "anthropic" // Claude subscriptions
	ProviderOpenAI    Provider = "openai"    // Codex/ChatGPT subscriptions
	ProviderGoogle    Provider = "google"    // Antigravity (agy) Google subscriptions
)

// ProviderFor returns the subscription provider of an agent harness.
func ProviderFor(h Harness) Provider {
	switch h {
	case HarnessClaude:
		return ProviderAnthropic
	case HarnessCodex:
		return ProviderOpenAI
	case HarnessAgy:
		return ProviderGoogle
	}
	return ""
}

// HarnessForProvider is the agent harness whose login a provider's
// accounts are ("" for an unknown provider).
func HarnessForProvider(p Provider) Harness {
	switch p {
	case ProviderAnthropic:
		return HarnessClaude
	case ProviderOpenAI:
		return HarnessCodex
	case ProviderGoogle:
		return HarnessAgy
	}
	return ""
}

// ManagedLogins reports whether Pyre can bind a harness's runs to its own
// per-account logins (isolated profiles: Claude config dirs, Codex homes).
// A harness with a provider but without managed logins (agy) always runs on
// the node's own login: account selection never balances it across
// accounts, and an explicit account must be that own login.
func ManagedLogins(h Harness) bool {
	return h == HarnessClaude || h == HarnessCodex
}

// RawJSON is an opaque, versioned adapter/namespace payload.
type RawJSON = json.RawMessage

// Digest is "sha256:<hex>".
type Digest string
