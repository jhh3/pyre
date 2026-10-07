package accounts

import (
	"context"
	"slices"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/usage"
)

// Launching on an "own login" account. The hub lists an account whose
// rota_ref (or provider ref) is a machine's own harness login as ready on
// those machines (usage/ownlogin.go) although no node holds a Pyre login for
// it. Such a run is the node's ambient login (credential_kind=ambient, no
// profile) with the assignment's account_id set to that account, so usage
// and the audit log attribute it correctly. A node whose own login is a
// different identity refuses; it never runs one account's work on another.

// fleetAccounts lists the hub's accounts of provider (bounded by
// FleetTimeout; nil without a directory).
func (s *Service) fleetAccounts(ctx context.Context, provider contracts.Provider) ([]contracts.AccountView, error) {
	if s.cfg.Fleet == nil {
		return nil, nil
	}
	tctx, cancel := context.WithTimeout(ctx, s.cfg.FleetTimeout)
	defer cancel()
	var out []contracts.AccountView
	cur := ""
	for i := 0; i < 20; i++ {
		page, err := s.cfg.Fleet.ListAccounts(tctx, contracts.ListAccountsRequest{Provider: provider, PageRequest: contracts.PageRequest{Cursor: cur, Limit: 200}})
		if err != nil {
			return out, err
		}
		out = append(out, page.Items...)
		if page.NextCursor == "" {
			break
		}
		cur = page.NextCursor
	}
	return out, nil
}

// ownLoginAccount is the enabled account that is this node's own login for
// h: a local account first, else the hub's (one with no Pyre login anywhere,
// the hub's "own login" card, wins). ok=false when the node has no readable
// own login or no account matches it.
func (s *Service) ownLoginAccount(ctx context.Context, h contracts.Harness) (contracts.Account, bool) {
	login := s.OwnLogin(h)
	if login == nil {
		return contracts.Account{}, false
	}
	provider := contracts.ProviderFor(h)
	if accts, err := listAccounts(ctx, s.db, provider); err == nil {
		for _, a := range accts {
			if !a.Disabled && MatchesOwnLogin(a, login) {
				return a, true
			}
		}
	}
	views, _ := s.fleetAccounts(ctx, provider)
	var best *contracts.Account
	for i := range views {
		a := views[i].Account
		if a.Disabled || a.Provider != provider || !MatchesOwnLogin(a, login) {
			continue
		}
		if len(views[i].Grants) == 0 {
			return a, true
		}
		if best == nil {
			best = &a
		}
	}
	if best != nil {
		return *best, true
	}
	return contracts.Account{}, false
}

// hasGrant reports whether this node holds a Pyre login of account id for h.
func (s *Service) hasGrant(ctx context.Context, id string, h contracts.Harness) bool {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM accounts_grant WHERE harness=? AND account_id=?`, string(h), id).Scan(&n); err != nil {
		return true // unknown: let normal selection explain
	}
	return n > 0
}

// loginName is the identity of a harness login for messages.
func loginName(l *contracts.HarnessLogin) string {
	if l.Email != "" {
		return l.Email
	}
	return l.ProviderRef
}

// acctIdentity is an account's login identity for messages.
func acctIdentity(a contracts.Account) string {
	if a.RotaRef != "" {
		return a.RotaRef
	}
	if a.ProviderRef != "" {
		return a.ProviderRef
	}
	return accountName(a)
}

// explicitOwnLogin checks an explicit account this node has no Pyre login
// for: local is the node's record of it (nil: none, the hub is asked). It
// returns the account when it is this node's own login for h, else a clear
// refusal.
func (s *Service) explicitOwnLogin(ctx context.Context, h contracts.Harness, id string, local *contracts.Account) (contracts.Account, error) {
	provider := contracts.ProviderFor(h)
	var a contracts.Account
	if local != nil {
		a = *local
	} else {
		views, ferr := s.fleetAccounts(ctx, provider)
		found := false
		for _, v := range views {
			if v.Account.AccountID == id {
				a, found = v.Account, true
				break
			}
		}
		if !found {
			if ferr != nil {
				return a, contracts.Errorf(contracts.ErrUnavailable, "account %s has no login on this node and the hub's account list is unavailable: %s",
					id, redact(contracts.AsError(ferr).Message)).WithRef(contracts.KindAccount, id).WithDetail("outcome", string(contracts.FallbackIntervention))
			}
			return a, contracts.Errorf(contracts.ErrNotFound, "account %s not found (not on this node or the hub, or not a %s account)", id, provider).
				WithRef(contracts.KindAccount, id)
		}
		if a.Provider != provider {
			return a, contracts.Errorf(contracts.ErrInvalid, "account %s is %s; harness %s needs %s (cross-provider use is never allowed)",
				a.AccountID, a.Provider, h, provider)
		}
		if a.Disabled {
			return a, contracts.Errorf(contracts.ErrConflict, "account %s is disabled", id).WithRef(contracts.KindAccount, id)
		}
	}
	login := s.OwnLogin(h)
	if login == nil {
		return a, contracts.Errorf(contracts.ErrUnavailable, "account %s (%s) has no Pyre login on this node and this node has no own %s login",
			accountName(a), acctIdentity(a), h).WithRef(contracts.KindAccount, id).WithDetail("outcome", string(contracts.FallbackIntervention))
	}
	if !MatchesOwnLogin(a, login) {
		return a, contracts.Errorf(contracts.ErrConflict, "account %s (%s) is not this node's own %s login (this node is logged in as %s); start it on a machine logged in as %s",
			accountName(a), acctIdentity(a), h, loginName(login), acctIdentity(a)).WithRef(contracts.KindAccount, id).
			WithDetail("outcome", string(contracts.FallbackIntervention))
	}
	return a, nil
}

// ownLoginCandidate is the hub account that is this node's own login for h,
// for automatic selection when no Pyre login here is eligible: enabled, not
// excluded, no Pyre login of it on this node (those were candidates
// already), and not at a limit by its last-known usage.
func (s *Service) ownLoginCandidate(ctx context.Context, h contracts.Harness, exclude []string) (contracts.Account, bool) {
	login := s.OwnLogin(h)
	if login == nil {
		return contracts.Account{}, false
	}
	provider := contracts.ProviderFor(h)
	views, _ := s.fleetAccounts(ctx, provider)
	now := s.now()
	for _, v := range views {
		a := v.Account
		if a.Disabled || a.Provider != provider || slices.Contains(exclude, a.AccountID) || !MatchesOwnLogin(a, login) || s.hasGrant(ctx, a.AccountID, h) {
			continue
		}
		if o := v.Latest; o != nil {
			if o.Eligibility == contracts.EligibleUnusable {
				continue
			}
			if (o.Eligibility == contracts.EligibleExhausted || o.Eligibility == contracts.EligibleCooldown) && o.ResetAt != nil && o.ResetAt.After(now) {
				continue
			}
			if usage.Judge(o.Windows, now).Limited {
				continue
			}
		}
		return a, true
	}
	return contracts.Account{}, false
}
