package hub

import (
	"context"
	"database/sql"
	"strings"
	"unicode/utf8"

	"github.com/jhh3/pyre/internal/contracts"
)

var _ contracts.AccountUpdateAPI = (*Hub)(nil)

// UpdateAccount changes an account's label, disabled flag and/or tier
// (owner only) and emits account.changed.
func (h *Hub) UpdateAccount(ctx context.Context, req contracts.UpdateAccountRequest) (contracts.Account, error) {
	p, err := principal(ctx)
	if err != nil {
		return contracts.Account{}, err
	}
	if !isOwner(p) {
		return contracts.Account{}, contracts.Errorf(contracts.ErrForbidden, "only the owner manages accounts")
	}
	if err := requireOp(req.Mutation); err != nil {
		return contracts.Account{}, err
	}
	if err := checkAccountUpdate(&req); err != nil {
		return contracts.Account{}, err
	}
	var out contracts.Account
	err = h.mutate(ctx, req.OperationID, contracts.OpUpdateAccount, p, req, &out, func(tx *sql.Tx) (any, error) {
		a, err := h.getAccount(ctx, tx, req.AccountID)
		if err != nil {
			return nil, err
		}
		before := a
		if req.Label != nil {
			a.Label = *req.Label
		}
		if req.Disabled != nil {
			a.Disabled = *req.Disabled
		}
		if req.Tier != nil {
			a.Tier = *req.Tier
		}
		h.auditAccountEditTx(ctx, tx, actorOf(p), &before, a)
		return a, h.putAccountTx(ctx, tx, a) // emits account.changed
	})
	return out, err
}

// checkAccountUpdate validates and normalizes an update request.
func checkAccountUpdate(req *contracts.UpdateAccountRequest) error {
	if !contracts.ValidID(req.AccountID, contracts.PrefixAccount) {
		return contracts.Errorf(contracts.ErrInvalid, "account_id %q is malformed", req.AccountID)
	}
	if req.Label == nil && req.Disabled == nil && req.Tier == nil {
		return contracts.Errorf(contracts.ErrInvalid, "nothing to update (label, disabled or tier)")
	}
	if req.Tier != nil {
		t, ok := contracts.ParseAccountTier(string(*req.Tier))
		if !ok {
			return contracts.Errorf(contracts.ErrInvalid, "tier %q is not normal or reserve", *req.Tier)
		}
		req.Tier = &t
	}
	if req.Label != nil {
		l := strings.TrimSpace(*req.Label)
		switch {
		case l == "":
			return contracts.Errorf(contracts.ErrInvalid, "label is required")
		case utf8.RuneCountInString(l) > contracts.MaxAccountLabel:
			return contracts.Errorf(contracts.ErrInvalid, "label is longer than %d characters", contracts.MaxAccountLabel)
		case strings.ContainsAny(l, "\n\r\t"):
			return contracts.Errorf(contracts.ErrInvalid, "label must be one line")
		}
		req.Label = &l
	}
	return nil
}
