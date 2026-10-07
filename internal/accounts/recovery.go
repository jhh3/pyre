package accounts

import (
	"context"
	"encoding/json"
	"path/filepath"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// Recovery of checked-in rota accounts (A12).
//
// A checked-in Claude rota account is observed with the snapshot's access
// token (read-only GET, never a refresh). A checked-in Codex rota account
// cannot be observed at all: reading its rate limits needs a Codex
// app-server on a home holding the login, and starting one on rota's
// snapshot home would make Codex a second owner of the refresh family. Its
// availability is therefore unknown whenever it is checked in, including
// after a limit's reset, and unknown is never capacity.
//
// Instead of treating unknown as capacity, selection verifies such an
// account at checkout: the login is MOVED into the profile (lock + fleet
// lease, exactly as for a run), the usage probe runs there (the same probe
// as for any checked-out account: it never refreshes and skips an expired
// token), and the run is admitted only if the result is fresh and eligible.
// Otherwise the never-launched assignment is dropped and the account is
// checked straight back in. A limited result keeps its reset, so the account
// is not checked out again before it.

// Expired at rest (the common case for idle accounts: a Claude access token
// lives about 8 h after its last refresh). A probe never refreshes, so such
// an account cannot be observed while checked in, and Pyre must never
// refresh rota's snapshot. Once checked out, however, the profile is the
// single owner of the login (rota lock + hub lease), and the harness
// refreshing it there is exactly what a launch does, with write-back to rota.
// So selection treats it as "eligible, unverified" (classRefresh, below
// verified-fresh accounts): ResolveFor checks it out, asks the harness to
// refresh its own login in the profile (LoginRefresher: Claude's TUI started
// without a prompt and with hooks off; Codex's app-server account/read with
// refreshToken), then runs the never-refreshing probe there. The run, and so
// its first prompt, exists only after a fresh eligible result: an account
// that turns out to be at its limit is checked back in (its refreshed login
// written back) and selection moves on, so no prompt is lost or replayed.

// LoginRefresher makes a harness refresh its own login inside a checked-out
// profile. fresh reports whether the profile's access token is valid again;
// implementations return once it is (or on ctx/timeout). It never sends a
// prompt to the model.
type LoginRefresher interface {
	RefreshLogin(ctx context.Context, h contracts.Harness, profileDir string, fresh func() bool) error
}

// refreshTimeout bounds one login refresh at checkout; refreshFailBackoff
// keeps an account whose harness could not refresh from being retried on
// every selection.
var (
	refreshTimeout     = 45 * time.Second
	refreshFailBackoff = 30 * time.Minute
)

// verifiableAtCheckout reports whether a checked-in rota grant whose usage is
// unknown can be verified from its profile right after checkout (verifiable),
// or after the harness refreshes an access token that expired at rest
// (refreshable). When neither, the note says why (empty when verification is
// simply not needed).
func (s *Service) verifiableAtCheckout(a contracts.Account, g grantRow) (verifiable, refreshable bool, note string) {
	snap, _, err := s.snapshotPaths(g.Harness, a.RotaRef)
	if err != nil {
		return false, false, "not verifiable at checkout: invalid rota_ref"
	}
	var si storeInfo
	switch g.Harness {
	case contracts.HarnessCodex:
		if s.cfg.CodexReader == nil {
			return false, false, "not verifiable at checkout: codex rate-limit reader unavailable"
		}
		si, _ = readCodexStore(filepath.Dir(snap))
		if !si.Present {
			return false, false, "not verifiable at checkout: no ChatGPT login in the rota snapshot"
		}
	case contracts.HarnessClaude:
		sn, err := readClaudeSnapshot(snap)
		if err != nil || sn.accessToken.empty() {
			return false, false, ""
		}
		if sn.expiresAt > 0 {
			si.ExpiresAt = time.UnixMilli(sn.expiresAt).UTC()
		}
	default:
		return false, false, ""
	}
	expired := !si.ExpiresAt.IsZero() && !si.ExpiresAt.After(s.now().Add(probeExpiryMargin))
	s.rfmu.Lock()
	backoff, failed := s.refreshFailed[a.AccountID]
	s.rfmu.Unlock()
	switch {
	case expired && failed && s.now().Before(backoff):
		return false, false, "unverified: the last login refresh at checkout failed; automatic selection retries it after " + backoff.Format(time.RFC3339)
	case expired && s.cfg.LoginRefresher != nil:
		return false, true, ""
	case expired:
		return false, false, "not verifiable at checkout: rota snapshot access token expired and a usage probe never refreshes " +
			"(no harness login refresher on this node; an explicit --account launch lets the harness refresh it inside the profile)"
	case g.Harness == contracts.HarnessClaude:
		// A valid Claude snapshot token is probed directly; a profile probe
		// would use the same token, so a checkout cannot learn more.
		return false, false, ""
	}
	return true, false, ""
}

// profileTokenFresh reports whether the checked-out profile's access token is
// valid (beyond the probe margin). Read-only.
func (s *Service) profileTokenFresh(ctx context.Context, g grantRow) bool {
	var si storeInfo
	switch g.Harness {
	case contracts.HarnessClaude:
		_, si, _ = s.claudeProfileOAuth(ctx, g.ProfileDir)
	case contracts.HarnessCodex:
		si, _ = readCodexStore(g.ProfileDir)
	}
	return si.Present && si.ExpiresAt.After(s.now().Add(probeExpiryMargin))
}

// refreshAtCheckout asks the harness to refresh the just-checked-out login in
// the profile. On success c.refreshed is set and the caller verifies usage.
// A failed refresh backs the account off (cooldown) and refuses it, except
// for an explicit choice, which launches as before (c.refreshed false).
func (s *Service) refreshAtCheckout(ctx context.Context, c *candidate, explicit bool) error {
	if !s.checkedOut(ctx, c.grant.GrantID) {
		// Never refresh a login that rota still holds.
		return contracts.Errorf(contracts.ErrConflict, "refusing to refresh %s: not checked out on this node", c.acct.AccountID)
	}
	fresh := func() bool { return s.profileTokenFresh(ctx, c.grant) }
	if !fresh() {
		s.refreshComposition(c.grant.ProfileDir, c.grant.Harness) // the warm-up TUI is a launch too
		rctx, cancel := context.WithTimeout(ctx, refreshTimeout)
		err := s.cfg.LoginRefresher.RefreshLogin(rctx, c.grant.Harness, c.grant.ProfileDir, fresh)
		cancel()
		if !fresh() {
			why := "the harness did not refresh the login in the profile"
			if err != nil {
				why += ": " + truncate(redact(err.Error()), 200)
			}
			s.log.Warn("accounts: login refresh at checkout failed", "account", c.acct.AccountID, "err", why)
			if explicit {
				c.why = "explicit account; " + why + "; launching (the harness refreshes on use)"
				return nil
			}
			until := s.now().Add(refreshFailBackoff)
			s.rfmu.Lock()
			if s.refreshFailed == nil {
				s.refreshFailed = map[string]time.Time{}
			}
			s.refreshFailed[c.acct.AccountID] = until
			s.rfmu.Unlock()
			provider := contracts.ProviderFor(c.grant.Harness)
			reason := c.acct.AccountID + ": unverified (access token expired at rest): " + why + "; retried after " + until.Format(time.RFC3339)
			e := contracts.Errorf(contracts.ErrCapacityExhausted, "no eligible %s subscription on this node: %s", provider, reason)
			e.Retryable = true
			return e.WithDetail("outcome", string(contracts.FallbackWaiting)).WithDetail("reasons", []string{reason}).
				WithDetail("earliest_reset", until.Format(time.RFC3339))
		}
	}
	c.refreshed = true
	s.log.Info("accounts: login refreshed by the harness in the checked-out profile", "account", c.acct.AccountID)
	// The rotated login reaches rota by the sync loop / check-in write-back.
	return nil
}

// verifyAtCheckout probes a just-checked-out account from its profile and
// admits the run only on a fresh eligible result. explicit additionally
// admits an inconclusive (unknown) result, as explicit selection always has.
// On success c's class, headroom, freshness and reason are updated.
func (s *Service) verifyAtCheckout(ctx context.Context, c *candidate, explicit bool) error {
	s.obmu.Lock()
	ps, err := getPoll(ctx, s.db, c.acct.AccountID)
	if err == nil {
		s.pollAccount(ctx, c.acct, ps)
	}
	s.obmu.Unlock()
	if err != nil {
		return err
	}
	obs, err := getObservation(ctx, s.db, c.acct.AccountID)
	if err != nil {
		return err
	}
	cd, err := getCooldown(ctx, s.db, c.acct.AccountID)
	if err != nil {
		return err
	}
	class, headroom, reset, fresh, why := s.classify(obs, cd, s.now())
	switch {
	case class == classFresh:
		c.class, c.headroom, c.reset, c.fresh = class, headroom, reset, fresh
		c.why = "verified at checkout: " + why
		if c.refreshed {
			c.why = "login refreshed by the harness in the checked-out profile; " + c.why
		}
		return nil
	case class == classUnknown && explicit:
		c.fresh = fresh
		c.why = "explicit account; verification at checkout inconclusive: " + why
		return nil
	}
	provider := contracts.ProviderFor(c.grant.Harness)
	reason := c.acct.AccountID + ": verified at checkout: " + why
	code, outcome := contracts.ErrCapacityExhausted, contracts.FallbackWaiting
	if class == classUnusable {
		code, outcome = contracts.ErrUnavailable, contracts.FallbackIntervention
	}
	e := contracts.Errorf(code, "no eligible %s subscription on this node: %s", provider, reason)
	e.Retryable = outcome == contracts.FallbackWaiting
	e = e.WithDetail("outcome", string(outcome)).WithDetail("reasons", []string{reason})
	if reset != nil {
		e = e.WithDetail("earliest_reset", reset.Format(time.RFC3339))
	}
	s.log.Info("accounts: checkout verification refused", "account", c.acct.AccountID, "class", class.String())
	return e
}

// updateAssignmentReason records the verified reason and freshness.
func (s *Service) updateAssignmentReason(ctx context.Context, a assignmentRow) {
	fb, _ := json.Marshal(a.Freshness)
	if _, err := s.db.ExecContext(ctx, `UPDATE accounts_assignment SET reason=?, freshness=? WHERE assignment_id=?`,
		a.Reason, string(fb), a.AssignmentID); err != nil {
		s.log.Warn("accounts: assignment reason", "run", a.RunID, "err", err)
	}
}

// resetPassedSinceObserved reports whether a limit's reset (the latest
// observation's reset or the cooldown's end) has passed since the account was
// last observed: the account is then due for a probe regardless of its poll
// schedule, so recovery is not delayed by up to a poll interval.
func (s *Service) resetPassedSinceObserved(ctx context.Context, accountID string) bool {
	now := s.now()
	obs, _ := getObservation(ctx, s.db, accountID)
	cd, _ := getCooldown(ctx, s.db, accountID)
	var ts []time.Time
	if obs != nil && obs.ResetAt != nil {
		ts = append(ts, *obs.ResetAt)
	}
	if cd != nil {
		ts = append(ts, cd.Until)
	}
	for _, t := range ts {
		if !t.After(now) && (obs == nil || obs.ObservedAt.Before(t)) {
			return true
		}
	}
	return false
}
