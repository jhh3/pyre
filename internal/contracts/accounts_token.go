package contracts

import (
	"context"
	"time"
)

// Token accounts (additive). An account backed by a long-lived,
// inference-scoped access token instead of a device login: `claude
// setup-token` (sk-ant-oat01-…, CLAUDE_CODE_OAUTH_TOKEN) for anthropic, a
// Codex personal access token (CODEX_ACCESS_TOKEN) for openai. The token is
// not a refresh family: the same token runs on every node at once, so
// grants of this kind need no fleet lease (Account.PerMachineIndependent).
//
// The hub holds the only copy, encrypted at rest. No read API returns it;
// only a node holding a grant fetches it (OpFetchAccountToken) over its
// authenticated TLS link, at launch and for usage probes, and keeps it in
// memory. It never appears in a command payload, run record, node database,
// journal, event or delivery record.

// CredOAuthToken: a hub-held access token injected only into the run's
// private environment file (anthropic CLAUDE_CODE_OAUTH_TOKEN, openai
// CODEX_ACCESS_TOKEN) with an isolated, credential-free profile. Never
// refreshes; a 401/403 is final until the owner sets a new token.
const CredOAuthToken CredentialKind = "oauth-token"

// Account token operations. Hubs that predate them answer 404 (clients
// report unsupported). They are not part of API: callers type-assert
// AccountTokenAPI.
const (
	// OpSetAccountToken stores (or replaces) an account's token
	// (SetAccountTokenRequest -> AccountTokenStatus). Owner only. The token
	// travels only in this request body.
	OpSetAccountToken = "accounts.set_token"
	// OpRemoveAccountToken deletes an account's token
	// (RemoveAccountTokenRequest -> AccountTokenStatus). Owner only.
	OpRemoveAccountToken = "accounts.remove_token"
	// OpListAccountTokens lists token status, never token bytes
	// (ListAccountTokensRequest -> []AccountTokenStatus).
	OpListAccountTokens = "accounts.tokens"
	// OpFetchAccountToken returns the token to a node allowed to use it
	// (FetchAccountTokenRequest -> FetchedAccountToken). Node principals
	// only; never proxied for local callers.
	OpFetchAccountToken = "accounts.token.fetch"
)

// MaxAccountToken bounds a token's length (bytes).
const MaxAccountToken = 4096

// SetAccountTokenRequest: Nodes empty = every node (current and future).
type SetAccountTokenRequest struct {
	Mutation
	AccountID string   `json:"account_id"`
	Token     string   `json:"token"`
	Nodes     []string `json:"nodes,omitempty"`
}

// String never prints the token.
func (r SetAccountTokenRequest) String() string {
	return "SetAccountTokenRequest{account_id=" + r.AccountID + " token=[redacted]}"
}

// GoString never prints the token.
func (r SetAccountTokenRequest) GoString() string { return r.String() }

type RemoveAccountTokenRequest struct {
	Mutation
	AccountID string `json:"account_id"`
}

type ListAccountTokensRequest struct {
	AccountID string `json:"account_id,omitempty"`
}

type FetchAccountTokenRequest struct {
	AccountID string `json:"account_id"`
}

// AccountTokenStatus is everything a reader may know about a token: that
// it is set, when, where it may be used, and its last probe outcome.
type AccountTokenStatus struct {
	AccountID string    `json:"account_id"`
	Provider  Provider  `json:"provider"`
	Set       bool      `json:"set"`
	AddedAt   time.Time `json:"added_at,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
	// Nodes the token may be fetched by; empty = every node.
	Nodes []string `json:"nodes,omitempty"`
	// VerifiedAt is the newest usage probe that the provider accepted.
	VerifiedAt *time.Time `json:"verified_at,omitempty"`
	// Rejected: the newest probe got 401/403 (revoked or expired).
	Rejected bool   `json:"rejected,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

// FetchedAccountToken carries the token to a node. Holders keep it in
// memory only and never log, persist or report it.
type FetchedAccountToken struct {
	AccountID string    `json:"account_id"`
	Provider  Provider  `json:"provider"`
	Token     string    `json:"token"`
	UpdatedAt time.Time `json:"updated_at"`
}

// String never prints the token.
func (f FetchedAccountToken) String() string {
	return "FetchedAccountToken{account_id=" + f.AccountID + " token=[redacted]}"
}

// GoString never prints the token.
func (f FetchedAccountToken) GoString() string { return f.String() }

// AccountTokenAPI is implemented by hubs, the mock backend and the client.
type AccountTokenAPI interface {
	SetAccountToken(ctx context.Context, req SetAccountTokenRequest) (AccountTokenStatus, error)
	RemoveAccountToken(ctx context.Context, req RemoveAccountTokenRequest) (AccountTokenStatus, error)
	ListAccountTokens(ctx context.Context, req ListAccountTokensRequest) ([]AccountTokenStatus, error)
}

// AccountTokenFetcher is the node-principal half (hub and client).
type AccountTokenFetcher interface {
	FetchAccountToken(ctx context.Context, req FetchAccountTokenRequest) (FetchedAccountToken, error)
}

// SourceToken is the AccountRegisterParams.Source of a token grant: an
// isolated, credential-free profile whose runs get the hub-held token.
const SourceToken = "token"

// UsageSourceMessagesProbe names observations read from the unified
// rate-limit headers of a minimal Messages request (1 output token).
const UsageSourceMessagesProbe = "claude:messages-headers"
