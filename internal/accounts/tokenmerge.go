package accounts

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// One account per identity (W-tokmerge). A person's account can hold both a
// hub token and a per-node login (rota checkout or a device login in the
// profile). The node keeps one grant per (account, harness):
//
//   - Registering a token on an account whose grant is a rota/login grant
//     converts that grant (same grant ID and generation) to oauth-token
//     with its own credential-free profile (<harness>-token), and keeps
//     the previous grant as the fallback (accounts_grant_fallback). A rota
//     run in flight keeps its checkout: check-in uses the checkout's own
//     profile, and token runs never hold it.
//   - When the token goes away (removed on the hub, this node no longer
//     allowed) or the provider rejects it, the grant reverts to the
//     fallback login, which selection verifies as before.
//   - A token-only account the hub folded into another one (hub
//     accounts_merge.go) is folded here too: an alias keeps explicit
//     policies naming the old ID working, its grant stops being selected
//     at once and is deleted when no run uses it any more.
//
// SyncTokenAccounts (run by pyred on a timer) registers every hub token
// account this node may use (nodes enrolled later, a harness installed
// later) and applies the hub's folds, so nothing needs `add-token` again.

func tokenMergeMigration() string {
	return `
CREATE TABLE accounts_grant_fallback (
	grant_id      TEXT PRIMARY KEY,
	kind          TEXT NOT NULL,
	profile_dir   TEXT NOT NULL,
	profile_ref   TEXT NOT NULL,
	refresh_owner TEXT NOT NULL,
	max_procs     INTEGER NOT NULL DEFAULT 0,
	detail        TEXT NOT NULL DEFAULT '',
	saved_at      TEXT NOT NULL
);
CREATE TABLE accounts_alias (
	old_id TEXT PRIMARY KEY,
	new_id TEXT NOT NULL,
	at     TEXT NOT NULL
);`
}

type grantFallback struct {
	GrantID, ProfileDir, ProfileRef, RefreshOwner, Detail string
	Kind                                                  contracts.CredentialKind
	MaxProcs                                              int
}

func getGrantFallback(ctx context.Context, q querier, grantID string) (*grantFallback, error) {
	var f grantFallback
	var kind string
	err := q.QueryRowContext(ctx, `SELECT grant_id,kind,profile_dir,profile_ref,refresh_owner,max_procs,detail FROM accounts_grant_fallback WHERE grant_id=?`, grantID).
		Scan(&f.GrantID, &kind, &f.ProfileDir, &f.ProfileRef, &f.RefreshOwner, &f.MaxProcs, &f.Detail)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	f.Kind = contracts.CredentialKind(kind)
	return &f, nil
}

func (s *Service) putFallback(ctx context.Context, f grantFallback) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO accounts_grant_fallback(grant_id,kind,profile_dir,profile_ref,refresh_owner,max_procs,detail,saved_at) VALUES(?,?,?,?,?,?,?,?)
		ON CONFLICT(grant_id) DO UPDATE SET kind=excluded.kind, profile_dir=excluded.profile_dir, profile_ref=excluded.profile_ref,
		refresh_owner=excluded.refresh_owner, max_procs=excluded.max_procs, detail=excluded.detail, saved_at=excluded.saved_at`,
		f.GrantID, string(f.Kind), f.ProfileDir, f.ProfileRef, f.RefreshOwner, f.MaxProcs, f.Detail, ft(s.now()))
	return err
}

// tokenProfileSub is the profile subdirectory a token grant of (account,
// harness) uses: the existing token grant's, "<harness>-token" when the
// account's grant on this node is another kind (converted), else
// "<harness>".
func (s *Service) tokenProfileSub(ctx context.Context, accountID string, h contracts.Harness) string {
	g, err := s.getGrant(ctx, s.db, accountID, h)
	switch {
	case err != nil:
		return string(h)
	case g.Kind == contracts.CredOAuthToken:
		if b := filepath.Base(g.ProfileDir); b == string(h) || b == string(h)+"-token" {
			return b
		}
		return string(h)
	default:
		return string(h) + "-token"
	}
}

// convertToToken turns an existing rota/login grant into a token grant
// (same ID and generation, so a run in flight is untouched) and saves the
// previous grant as the fallback. Caller: ensureGrant.
func (s *Service) convertToToken(ctx context.Context, g grantRow, dir string) (grantRow, error) {
	if err := s.putFallback(ctx, grantFallback{GrantID: g.GrantID, Kind: g.Kind, ProfileDir: g.ProfileDir, ProfileRef: g.ProfileRef,
		RefreshOwner: g.RefreshOwner, MaxProcs: g.MaxProcs, Detail: g.Detail}); err != nil {
		return g, err
	}
	g.Kind, g.ProfileDir, g.ProfileRef, g.RefreshOwner = contracts.CredOAuthToken, dir, g.AccountID+"/"+filepath.Base(dir), "none"
	g.State, g.Detail, g.ObservedAt = contracts.GrantUnknown, "token held by the hub; injected at launch (the "+kindWord(g.Kind)+" login is the fallback)", s.now()
	if _, err := s.db.ExecContext(ctx, `UPDATE accounts_grant SET kind=?, profile_dir=?, profile_ref=?, refresh_owner=?, state=?, detail=?, observed_at=? WHERE grant_id=?`,
		string(g.Kind), g.ProfileDir, g.ProfileRef, g.RefreshOwner, string(g.State), g.Detail, ft(g.ObservedAt), g.GrantID); err != nil {
		return g, err
	}
	g.Mechanism = mechanismFor(g.Kind)
	s.log.Info("accounts: token attached to an existing login (kept as fallback)", "account", g.AccountID, "grant", g.GrantID)
	return g, nil
}

func kindWord(k contracts.CredentialKind) string {
	if k == CredRotaCheckout {
		return "rota"
	}
	return "device"
}

// revertToken turns a token grant whose token is gone or rejected back
// into its fallback login. ok=false when the grant has no fallback.
func (s *Service) revertToken(ctx context.Context, g grantRow, why string) (grantRow, bool) {
	if g.Kind != contracts.CredOAuthToken {
		return g, false
	}
	f, err := getGrantFallback(ctx, s.db, g.GrantID)
	if err != nil || f == nil {
		return g, false
	}
	g.Kind, g.ProfileDir, g.ProfileRef, g.RefreshOwner, g.MaxProcs = f.Kind, f.ProfileDir, f.ProfileRef, f.RefreshOwner, f.MaxProcs
	// Unknown: selection re-verifies the login (rota snapshot / profile).
	g.State, g.Detail, g.ObservedAt = contracts.GrantUnknown, "token unavailable ("+why+"); using the "+kindWord(f.Kind)+" login", s.now()
	if _, err := s.db.ExecContext(ctx, `UPDATE accounts_grant SET kind=?, profile_dir=?, profile_ref=?, refresh_owner=?, max_procs=?, state=?, detail=?, observed_at=? WHERE grant_id=?`,
		string(g.Kind), g.ProfileDir, g.ProfileRef, g.RefreshOwner, g.MaxProcs, string(g.State), g.Detail, ft(g.ObservedAt), g.GrantID); err != nil {
		return g, false
	}
	_, _ = s.db.ExecContext(ctx, `DELETE FROM accounts_grant_fallback WHERE grant_id=?`, g.GrantID)
	// A rejected token says nothing about the subscription: drop that
	// reading so the login is judged on its own, and observe it soon.
	if o, _ := getObservation(ctx, s.db, g.AccountID); o != nil && o.Eligibility == contracts.EligibleUnusable &&
		(o.Source == contracts.UsageSourceMessagesProbe || strings.HasPrefix(o.Source, "token:")) {
		_, _ = s.db.ExecContext(ctx, `DELETE FROM accounts_observation WHERE account_id=?`, g.AccountID)
	}
	_ = putPoll(ctx, s.db, g.AccountID, pollState{NextAt: s.now()})
	g.Mechanism = mechanismFor(g.Kind)
	s.log.Info("accounts: token unavailable; account back on its fallback login", "account", g.AccountID, "kind", string(g.Kind), "why", why)
	s.report(ctx, contracts.ReportGrant, g.DeviceGrant)
	return g, true
}

// tokenDown reports whether a token grant can no longer be used until the
// owner acts (token removed, node not allowed, provider rejection).
func tokenDown(g grantRow) bool {
	return g.Kind == contracts.CredOAuthToken && (g.State == contracts.GrantDead || g.State == contracts.GrantRevoked || g.State == contracts.GrantExpired)
}

// maybeRevert reverts a token grant that went down; it returns the current
// grant.
func (s *Service) maybeRevert(ctx context.Context, grantID string) (grantRow, bool) {
	g, err := s.grantByID(ctx, s.db, grantID)
	if err != nil || !tokenDown(g) {
		return g, false
	}
	return s.revertToken(ctx, g, g.Detail)
}

// rotaProfileDir is where a rota grant's checkout lives (the fallback's
// profile while the grant is converted to a token).
func (s *Service) rotaProfileDir(ctx context.Context, g grantRow) string {
	if g.Kind == contracts.CredOAuthToken {
		if f, _ := getGrantFallback(ctx, s.db, g.GrantID); f != nil {
			return f.ProfileDir
		}
	}
	return g.ProfileDir
}

// ---------- aliases (accounts the hub folded) ----------

// canonicalAccount maps a folded account ID to its survivor.
func (s *Service) canonicalAccount(ctx context.Context, id string) string {
	if id == "" {
		return id
	}
	var to string
	if err := s.db.QueryRowContext(ctx, `SELECT new_id FROM accounts_alias WHERE old_id=?`, id).Scan(&to); err == nil && to != "" {
		return to
	}
	return id
}

func (s *Service) canonicalPolicy(ctx context.Context, p *contracts.AccountPolicy) {
	p.AccountID = s.canonicalAccount(ctx, p.AccountID)
	for i, id := range p.Prefer {
		p.Prefer[i] = s.canonicalAccount(ctx, id)
	}
}

// FoldAccount records that the hub folded oldID into newID: the old
// account is no longer selected, and is removed once no run uses it.
func (s *Service) FoldAccount(ctx context.Context, oldID, newID string) error {
	if oldID == "" || newID == "" || oldID == newID {
		return contracts.Errorf(contracts.ErrInvalid, "fold needs two different accounts")
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO accounts_alias(old_id,new_id,at) VALUES(?,?,?)
		ON CONFLICT(old_id) DO UPDATE SET new_id=excluded.new_id, at=excluded.at`, oldID, newID, ft(s.now())); err != nil {
		return err
	}
	_, _ = s.db.ExecContext(ctx, `UPDATE accounts_alias SET new_id=? WHERE new_id=?`, newID, oldID)
	s.log.Info("accounts: account folded into another (hub)", "from", oldID, "into", newID)
	s.cleanupFolded(ctx)
	return nil
}

// cleanupFolded retires folded accounts: their token grants stop being
// selected at once (revoked), and the account is deleted locally when no
// active assignment uses it.
func (s *Service) cleanupFolded(ctx context.Context) {
	rows, err := s.db.QueryContext(ctx, `SELECT old_id,new_id FROM accounts_alias`)
	if err != nil {
		return
	}
	type pair struct{ old, new string }
	var ps []pair
	for rows.Next() {
		var p pair
		if rows.Scan(&p.old, &p.new) == nil {
			ps = append(ps, p)
		}
	}
	rows.Close()
	for _, p := range ps {
		gs, err := s.listGrants(ctx, s.db, p.old)
		if err != nil {
			continue
		}
		act, err := s.activeAssignments(ctx, s.db, p.old)
		if err != nil {
			continue
		}
		keep := false
		for _, g := range gs {
			if g.Kind != contracts.CredOAuthToken {
				keep = true // not a token-only account here: leave it to the owner
				continue
			}
			if len(act) > 0 {
				if g.State != contracts.GrantRevoked {
					g.State, g.Detail, g.ObservedAt = contracts.GrantRevoked, "merged into "+p.new+"; running sessions finish here", s.now()
					_ = updateGrantState(ctx, s.db, g)
				}
				keep = true
				continue
			}
			_, _ = s.db.ExecContext(ctx, `DELETE FROM accounts_grant WHERE grant_id=?`, g.GrantID)
			_, _ = s.db.ExecContext(ctx, `DELETE FROM accounts_grant_fallback WHERE grant_id=?`, g.GrantID)
			_, _ = s.db.ExecContext(ctx, `DELETE FROM accounts_lease WHERE grant_id=?`, g.GrantID)
		}
		if keep {
			continue
		}
		for _, q := range []string{`DELETE FROM accounts_observation WHERE account_id=?`, `DELETE FROM accounts_cooldown WHERE account_id=?`,
			`DELETE FROM accounts_poll WHERE account_id=?`, `DELETE FROM accounts_account WHERE account_id=?`} {
			_, _ = s.db.ExecContext(ctx, q, p.old)
		}
		s.tok.drop(p.old)
	}
}

// ---------- sync with the hub ----------

// TokenDirectory is what a node reads from the hub to keep its token
// accounts current (node credential; never token bytes).
type TokenDirectory interface {
	ListAccounts(ctx context.Context, req contracts.ListAccountsRequest) (contracts.Page[contracts.AccountView], error)
	ListAccountTokens(ctx context.Context, req contracts.ListAccountTokensRequest) ([]contracts.AccountTokenStatus, error)
}

// TokenSyncResult says what one sync pass did (IDs only).
type TokenSyncResult struct {
	Registered []string `json:"registered,omitempty"`
	Folded     []string `json:"folded,omitempty"` // "old->new"
	Errors     []string `json:"errors,omitempty"`
}

// SyncTokenAccounts registers every hub token account this node may use
// for a harness available here (available may be nil: all), refreshes the
// local copy of hub accounts, and applies the hub's folds.
func (s *Service) SyncTokenAccounts(ctx context.Context, dir TokenDirectory, available func(contracts.Harness) bool) TokenSyncResult {
	var res TokenSyncResult
	hubAccts := map[string]contracts.Account{}
	cur := ""
	for i := 0; i < 50; i++ {
		page, err := dir.ListAccounts(ctx, contracts.ListAccountsRequest{PageRequest: contracts.PageRequest{Cursor: cur}})
		if err != nil {
			res.Errors = append(res.Errors, "list accounts: "+contracts.AsError(err).Message)
			return res
		}
		for _, v := range page.Items {
			hubAccts[v.Account.AccountID] = v.Account
		}
		if page.NextCursor == "" {
			break
		}
		cur = page.NextCursor
	}
	statuses, err := dir.ListAccountTokens(ctx, contracts.ListAccountTokensRequest{})
	if err != nil {
		res.Errors = append(res.Errors, "list tokens: "+contracts.AsError(err).Message)
		return res
	}
	// 1. Local accounts the hub no longer lists: folded into another?
	locals, err := listAccounts(ctx, s.db, "")
	if err != nil {
		res.Errors = append(res.Errors, err.Error())
		return res
	}
	for _, l := range locals {
		if a, ok := hubAccts[l.AccountID]; ok {
			// The owner's label, disabled flag and tier follow the hub;
			// local bindings are only filled in, never cleared.
			m := l
			m.Label, m.Disabled, m.Tier = a.Label, a.Disabled, a.Tier
			if m.RotaRef == "" {
				m.RotaRef = a.RotaRef
			}
			if a.Provider == l.Provider && (m.Label != l.Label || m.Disabled != l.Disabled || m.Tier != l.Tier || m.RotaRef != l.RotaRef) {
				_ = s.UpsertAccount(ctx, m)
			}
			continue
		}
		if s.canonicalAccount(ctx, l.AccountID) != l.AccountID {
			continue // already folded; cleanup below
		}
		hasToken := false
		if gs, err := s.listGrants(ctx, s.db, l.AccountID); err == nil {
			for _, g := range gs {
				hasToken = hasToken || g.Kind == contracts.CredOAuthToken
			}
		}
		if !hasToken {
			continue
		}
		st, err := dir.ListAccountTokens(ctx, contracts.ListAccountTokensRequest{AccountID: l.AccountID})
		if err != nil || len(st) != 1 || st[0].AccountID == l.AccountID {
			continue
		}
		if _, ok := hubAccts[st[0].AccountID]; !ok {
			continue
		}
		if err := s.FoldAccount(ctx, l.AccountID, st[0].AccountID); err != nil {
			res.Errors = append(res.Errors, err.Error())
			continue
		}
		res.Folded = append(res.Folded, l.AccountID+"->"+st[0].AccountID)
	}
	s.cleanupFolded(ctx)
	// 2. Every token account this node may use: registered here.
	for _, st := range statuses {
		a, ok := hubAccts[st.AccountID]
		if !ok || !st.Set || a.Disabled || st.Rejected {
			continue
		}
		if len(st.Nodes) > 0 && !contains(st.Nodes, s.cfg.NodeID) {
			continue
		}
		h := harnessForProvider(a.Provider)
		spec, ok := tokenSpecs[h]
		if !ok || available != nil && !available(h) {
			continue
		}
		g, gerr := s.getGrant(ctx, s.db, a.AccountID, h)
		switch {
		case gerr != nil && !contracts.IsCode(gerr, contracts.ErrNotFound):
			continue
		case gerr == nil && g.Kind == contracts.CredOAuthToken:
			if !tokenDown(g) || !st.UpdatedAt.After(g.ObservedAt) {
				continue // in place (or down with no newer token)
			}
		case gerr == nil && !spec.Launch:
			continue // a token this harness cannot launch never displaces a working login
		case gerr == nil:
			if r := s.tok.rejectedAt(a.AccountID); !r.IsZero() && !st.UpdatedAt.After(r) {
				continue // this token was rejected here; wait for a new one
			}
		}
		if _, err := s.Register(ctx, RegisterParams{Account: a, Harness: h, Source: contracts.SourceToken}); err != nil {
			res.Errors = append(res.Errors, a.AccountID+": "+contracts.AsError(err).Message)
			continue
		}
		res.Registered = append(res.Registered, a.AccountID)
	}
	if len(res.Registered) > 0 || len(res.Folded) > 0 {
		s.log.Info("accounts: token accounts synced from the hub", "registered", strings.Join(res.Registered, ","), "folded", strings.Join(res.Folded, ","))
	}
	return res
}

// RunTokenSync syncs at start (after first) and then every interval until
// ctx ends.
func (s *Service) RunTokenSync(ctx context.Context, dir TokenDirectory, available func(contracts.Harness) bool, first, interval time.Duration) {
	t := time.NewTimer(first)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		pctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		s.SyncTokenAccounts(pctx, dir, available)
		cancel()
		t.Reset(interval + s.jitter(interval/10))
	}
}

// verifyLogin verifies a non-token grant exactly as gather does (rota
// snapshot when checked in, the profile store otherwise).
func (s *Service) verifyLogin(ctx context.Context, a contracts.Account, g grantRow) (v verdict, cur grantRow, verifiable, refreshable bool, note string) {
	switch {
	case g.Kind == CredRotaCheckout && !s.cfg.Rota.Enabled:
		v = verdict{Grant: g, Outcome: contracts.FallbackIntervention, Reason: "rota checkouts are disabled on this node"}
		return v, g, false, false, ""
	case g.Kind == CredRotaCheckout && !s.checkedOut(ctx, g.GrantID):
		v = s.verifyRotaGrant(a, g)
		s.persistVerdict(ctx, v)
		if v.OK {
			verifiable, refreshable, note = s.verifiableAtCheckout(a, v.Grant)
		}
		return v, v.Grant, verifiable, refreshable, note
	}
	v = s.verifyGrant(ctx, a, g)
	s.persistVerdict(ctx, v)
	return v, v.Grant, false, false, ""
}
