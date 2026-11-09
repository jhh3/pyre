package accounts

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

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

// fleetAcctTTL is how long a node reuses the hub's account list for
// own-login matching. Every own-login usage update (one a minute per
// harness while runs take turns) and launch looked the account up with a
// fresh ~150 KB accounts.list; the identities it matches on (rota_ref,
// provider_ref, disabled) change rarely.
const fleetAcctTTL = 30 * time.Second

type fleetAcctCache struct {
	mu sync.Mutex
	by map[contracts.Provider]fleetAcctEntry
}

type fleetAcctEntry struct {
	views []contracts.AccountView
	at    time.Time
}

// fleetAccounts lists the hub's accounts of provider (bounded by
// FleetTimeout; nil without a directory), reusing a list up to
// fleetAcctTTL old (a negative FleetCache always reads). Callers treat the
// views as read-only.
func (s *Service) fleetAccounts(ctx context.Context, provider contracts.Provider) ([]contracts.AccountView, error) {
	if s.cfg.Fleet == nil {
		return nil, nil
	}
	if s.cfg.FleetCache >= 0 {
		s.fleetAccts.mu.Lock()
		e, ok := s.fleetAccts.by[provider]
		s.fleetAccts.mu.Unlock()
		if ok && time.Since(e.at) < fleetAcctTTL {
			return e.views, nil
		}
	}
	out, err := s.listFleetAccounts(ctx, provider)
	if err == nil && s.cfg.FleetCache >= 0 {
		s.fleetAccts.mu.Lock()
		if s.fleetAccts.by == nil {
			s.fleetAccts.by = map[contracts.Provider]fleetAcctEntry{}
		}
		s.fleetAccts.by[provider] = fleetAcctEntry{views: out, at: time.Now()}
		s.fleetAccts.mu.Unlock()
	}
	return out, err
}

func (s *Service) listFleetAccounts(ctx context.Context, provider contracts.Provider) ([]contracts.AccountView, error) {
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
	for _, v := range views {
		a := v.Account
		if a.Disabled || a.Provider != provider || slices.Contains(exclude, a.AccountID) || !MatchesOwnLogin(a, login) || s.hasGrant(ctx, a.AccountID, h) {
			continue
		}
		if _, _, blocked := s.ownLoginBlocked(ctx, h, a.AccountID, v.Latest); blocked {
			continue
		}
		return a, true
	}
	return contracts.Account{}, false
}

// ownLoginBlockedErr is the selection error for an explicit account that
// is this node's own login for h when that login cannot take a run now
// (ownLoginBlocked), nil when it can. It reads like noneEligible's, so a
// non-strict pin falls back with the reason (pinFallbackNote): waiting
// when the limit has a reset, else intervention. Only for harnesses whose
// logins Pyre manages.
func (s *Service) ownLoginBlockedErr(ctx context.Context, h contracts.Harness, a contracts.Account) error {
	if !contracts.ManagedLogins(h) {
		return nil // agy: its own login is all there is (OwnLimit holds placement)
	}
	why, reset, blocked := s.ownLoginBlocked(ctx, h, a.AccountID, s.fleetLatest(ctx, a.Provider, a.AccountID))
	if !blocked {
		return nil
	}
	r := acctRef(accountName(a)) + " (" + a.AccountID + "): own " + string(h) + " login " + why
	outcome, code := contracts.FallbackIntervention, contracts.ErrUnavailable
	if reset != nil {
		outcome, code = contracts.FallbackWaiting, contracts.ErrCapacityExhausted
	}
	e := contracts.Errorf(code, "no eligible %s subscription on this node: %s", a.Provider, r)
	e.Retryable = reset != nil
	e = e.WithDetail("outcome", string(outcome)).WithDetail("reasons", []string{r}).WithRef(contracts.KindAccount, a.AccountID)
	if reset != nil {
		e = e.WithDetail("earliest_reset", reset.UTC().Format(time.RFC3339))
	}
	return e
}

// fleetLatest is the hub's latest usage observation of account id (nil
// when the hub has none or cannot be read).
func (s *Service) fleetLatest(ctx context.Context, provider contracts.Provider, id string) *contracts.AccountObservation {
	views, _ := s.fleetAccounts(ctx, provider)
	for _, v := range views {
		if v.Account.AccountID == id {
			return v.Latest
		}
	}
	return nil
}

// ownLoginRejectWindow bounds how far back a refused login on the node's
// own login counts when the login file's age cannot be read (a keychain).
const ownLoginRejectWindow = time.Hour

// ownLoginBlocked reports why this node's own login for h, which is account
// id, cannot take a run now: by latest (the hub's last-known usage, as
// `pyre accounts` shows it) or this node's own record of it, it is at a
// 5-hour or weekly limit or unusable; it is in a cooldown here; or a run
// on it here was refused authentication since the login was last written
// (ownLoginRejected). reset is when a limit ends (nil: unknown, or a login
// that needs a re-login). Every check reads stored state only.
func (s *Service) ownLoginBlocked(ctx context.Context, h contracts.Harness, id string, latest *contracts.AccountObservation) (why string, reset *time.Time, blocked bool) {
	now := s.now()
	obs := []*contracts.AccountObservation{latest}
	if o, err := getObservation(ctx, s.db, id); err == nil && o != nil {
		obs = append(obs, o)
	}
	for _, o := range obs {
		if o == nil {
			continue
		}
		if o.Eligibility == contracts.EligibleUnusable {
			return "login unusable: " + o.Detail, nil, true
		}
		if (o.Eligibility == contracts.EligibleExhausted || o.Eligibility == contracts.EligibleCooldown) && o.ResetAt != nil && o.ResetAt.After(now) {
			u := *o.ResetAt
			return fmt.Sprintf("%s until %s", o.Eligibility, u.Format(time.RFC3339)), &u, true
		}
		if lk := usage.Judge(o.Windows, now); lk.Limited {
			if lk.Until != nil {
				return "at its limit until " + lk.Until.Format(time.RFC3339), lk.Until, true
			}
			return "at its limit", nil, true
		}
	}
	if cd, err := getCooldown(ctx, s.db, id); err == nil && cd != nil && cd.Until.After(now) {
		u := cd.Until
		return "cooldown until " + u.Format(time.RFC3339) + " (" + cd.Reason + ")", &u, true
	}
	if why, ok := s.ownLoginRejected(ctx, h, id); ok {
		return why, nil, true
	}
	return "", nil, false
}

// ownLoginRejected reports a run on this node's own login for h (account
// id) that ended on a refused login (a limit event of kind auth_invalid)
// after the login file was last written: every turn there fails the same
// way until someone signs in again, which rewrites the file and clears it
// (server's own Codex login answered "workspace routing discovery
// unauthorized (401)" to every turn for nine days).
func (s *Service) ownLoginRejected(ctx context.Context, h contracts.Harness, id string) (string, bool) {
	since := s.now().Add(-ownLoginRejectWindow)
	if mt, ok := ownLoginWritten(s.cfg, h); ok {
		since = mt
	}
	var planned, detail string
	err := s.db.QueryRowContext(ctx, `SELECT f.planned_at, COALESCE(json_extract(f.body,'$.limit.detail'),'') FROM accounts_fallback f
		JOIN accounts_assignment a ON a.run_id=f.from_run_id
		WHERE a.credential_kind=? AND a.account_id=? AND json_extract(f.body,'$.limit.kind')=? AND f.planned_at>=?
		ORDER BY f.planned_at DESC LIMIT 1`, string(contracts.CredAmbient), id, LimitAuthInvalid, ft(since)).Scan(&planned, &detail)
	if err != nil {
		return "", false
	}
	why := "this node's own " + string(h) + " login was refused"
	if detail != "" {
		why += " (" + truncate(detail, 120) + ")"
	}
	return why + " at " + planned + "; sign in again on this machine to use it", true
}

// ownLoginWritten is when the node's own login file for h was last
// written (ok=false when it cannot be read, e.g. a macOS keychain).
func ownLoginWritten(cfg Config, h contracts.Harness) (time.Time, bool) {
	var p string
	switch h {
	case contracts.HarnessCodex:
		if cfg.UserCodexDir != "" {
			p = filepath.Join(cfg.UserCodexDir, "auth.json")
		}
	case contracts.HarnessClaude:
		if cfg.UserClaudeDir != "" {
			p = filepath.Join(cfg.UserClaudeDir, ".credentials.json")
		}
	}
	if p == "" {
		return time.Time{}, false
	}
	fi, err := os.Stat(p)
	if err != nil {
		return time.Time{}, false
	}
	return fi.ModTime().UTC(), true
}
