package contracts

import "context"

// OpUpdateAccount edits an existing account's owner-facing fields
// (UpdateAccountRequest -> Account). Owner only. Unlike accounts.upsert it
// changes only the fields given, so a relabel never resets the rota
// binding, the per-machine flag or the provider identity. Additive: hubs
// that predate it answer 404 (the client reports unsupported).
const OpUpdateAccount = "accounts.update"

// UpdateAccountRequest: nil fields are left unchanged.
type UpdateAccountRequest struct {
	Mutation
	AccountID string  `json:"account_id"`
	Label     *string `json:"label,omitempty"`
	Disabled  *bool   `json:"disabled,omitempty"`
}

// AccountUpdateAPI is implemented by hubs (and the mock backend, the
// client and the node-local proxy) that offer OpUpdateAccount. It is not
// part of API: callers type-assert.
type AccountUpdateAPI interface {
	UpdateAccount(ctx context.Context, req UpdateAccountRequest) (Account, error)
}

// MaxAccountLabel is the longest account label (runes).
const MaxAccountLabel = 64
