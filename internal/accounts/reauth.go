package accounts

import (
	"context"
	"os"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// A login the provider rejects (HTTP 401: expired, signed out elsewhere,
// refresh token reused) is "needs re-auth": the account is recorded unusable
// with how to fix it, held in a cooldown so selection does not check it out
// and probe it again at every launch, and selection falls through to the
// next eligible account or, with the auto default, to the node's own login.

// reauthCooldownReason marks a cooldown set because the login was rejected.
const reauthCooldownReason = "needs re-auth"

// authRejected reports whether a probe error says the provider refused the
// login itself (not a limit, not a transport failure).
func authRejected(msg string) bool {
	l := strings.ToLower(msg)
	switch {
	case strings.Contains(l, "401"), strings.Contains(l, "unauthorized"),
		strings.Contains(l, "sign in again"), strings.Contains(l, "log in again"),
		strings.Contains(l, "refresh_token_reused"), strings.Contains(l, "refresh token") && (strings.Contains(l, "expired") || strings.Contains(l, "invalid") || strings.Contains(l, "reused")),
		strings.Contains(l, "invalid_grant"), strings.Contains(l, "token_expired"):
		return true
	}
	return false
}

// reloginHint says how the owner fixes a rejected login of a.
func (s *Service) reloginHint(a contracts.Account) string {
	node := s.cfg.NodeName
	if node == "" {
		node = s.cfg.NodeID
	}
	if a.Provider == contracts.ProviderOpenAI {
		return "log in again: pyre accounts login --node " + node
	}
	return "log in again on " + node + " (pyre accounts add-token for a token account)"
}

// markNeedsReauth records a rejected login: an unusable observation (shown
// as "needs re-auth" with the fix) and a cooldown of cfg.ReauthCooldown.
func (s *Service) markNeedsReauth(ctx context.Context, a contracts.Account, why string) *contracts.AccountObservation {
	until := s.now().Add(s.cfg.ReauthCooldown)
	_ = putCooldown(ctx, s.db, a.AccountID, until, reauthCooldownReason)
	// The fix first: displays shorten long details (usage.Humanize).
	cause := "login rejected"
	if strings.Contains(why, "401") {
		cause = "login rejected: 401"
	}
	detail := s.reloginHint(a) + " (" + cause + ")"
	s.log.Warn("accounts: login needs re-auth", "account", a.AccountID, "retry_after", until.Format(time.RFC3339))
	return s.failObs(ctx, a, contracts.EligibleUnusable, detail, false)
}

// holdNeedsReauth keeps a checked-in account's "needs re-auth" observation
// while its re-auth cooldown runs (a checked-in Codex login cannot be
// probed, and replacing it with "unknown" would make selection check it out
// again). A rota snapshot rewritten after the rejection (the owner logged in
// again) ends the hold at once.
func (s *Service) holdNeedsReauth(ctx context.Context, a contracts.Account, g grantRow) bool {
	prev, _ := getObservation(ctx, s.db, a.AccountID)
	if prev == nil || prev.Eligibility != contracts.EligibleUnusable {
		return false
	}
	cd, _ := getCooldown(ctx, s.db, a.AccountID)
	if cd == nil || cd.Reason != reauthCooldownReason || !cd.Until.After(s.now()) {
		return false
	}
	if snap, _, err := s.snapshotPaths(g.Harness, a.RotaRef); err == nil {
		if fi, err := os.Stat(snap); err == nil && fi.ModTime().After(prev.ObservedAt) {
			_ = clearCooldown(ctx, s.db, a.AccountID)
			return false
		}
	}
	return true
}

func clearCooldown(ctx context.Context, q querier, accountID string) error {
	_, err := q.ExecContext(ctx, `DELETE FROM accounts_cooldown WHERE account_id=?`, accountID)
	return err
}

// needsReauth reports whether this node's login for the account is known to
// be rejected: a dead/expired/revoked grant or an unusable observation.
func (s *Service) needsReauth(ctx context.Context, accountID string, h contracts.Harness) (bool, error) {
	g, err := s.getGrant(ctx, s.db, accountID, h)
	if err != nil {
		if contracts.IsCode(err, contracts.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	switch g.State {
	case contracts.GrantDead, contracts.GrantExpired, contracts.GrantRevoked:
		return true, nil
	}
	obs, err := getObservation(ctx, s.db, accountID)
	if err != nil {
		return false, err
	}
	return obs != nil && obs.Eligibility == contracts.EligibleUnusable, nil
}

// accountName is the account's label, else its id.
func accountName(a contracts.Account) string {
	if a.Label != "" {
		return a.Label
	}
	return a.AccountID
}
