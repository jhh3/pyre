package accounts

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
	"github.com/jhh3/pyre/internal/usage"
)

// ResolveRequest is the full form of a launch-time account binding.
type ResolveRequest struct {
	Harness   contracts.Harness
	Policy    contracts.AccountPolicy
	RunID     string
	SessionID string
	// LeaseHolder overrides the Codex process-lease holder. Default
	// Config.CodexLeaseHolder: the shared per-home app-server, so runs that
	// go through it share the home's single-process lease. A run that needs
	// its own Codex process passes a distinct holder (e.g. "run:<id>") and
	// conflicts with any other holder.
	LeaseHolder string
	// Exclude lists accounts not to choose (fallback excludes the limited one).
	Exclude []string
}

// Resolution is the node-local launch binding. Env carries only nonsecret
// profile paths; Unset lists inherited variables the launcher must remove so
// a managed run can neither use another login nor fall into API billing.
type Resolution struct {
	ProfileDir     string
	Env            map[string]string
	Unset          []string
	Assignment     *contracts.AccountAssignment
	CredentialKind contracts.CredentialKind
	Grant          *contracts.DeviceGrant
}

// Variables removed from every Claude/Codex launch environment.
var (
	unsetClaude = []string{"CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR", "CLAUDE_SECURESTORAGE_CONFIG_DIR",
		"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX"}
	unsetCodex   = []string{"OPENAI_API_KEY", "CODEX_API_KEY", "CODEX_ACCESS_TOKEN"}
	unsetAmbient = map[contracts.Harness][]string{
		contracts.HarnessClaude: {"CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX"},
		contracts.HarnessCodex:  unsetCodex,
	}
)

// UnsetFor lists the inherited variables a managed launch of harness h
// removes (other logins, API keys); also used for login refreshes.
func UnsetFor(h contracts.Harness) []string {
	switch h {
	case contracts.HarnessClaude:
		return append(append([]string{}, unsetClaude...), unsetCodex...)
	case contracts.HarnessCodex:
		return append(append([]string{}, unsetCodex...), unsetClaude...)
	}
	return nil
}

// Resolve implements the daemon's account hook:
//
//	Resolve(ctx, harness, policy, runID) (profileDir, env, assignment, error)
//
// Harnesses without a subscription provider (shell, mock) resolve to nothing.
// Ambient policy returns no profile and an assignment labelled
// credential_kind=ambient. Errors are *contracts.Error with
// Details["outcome"] = "waiting" | "intervention" when nothing is eligible.
func (s *Service) Resolve(ctx context.Context, harness contracts.Harness, policy contracts.AccountPolicy, runID string) (string, map[string]string, *contracts.AccountAssignment, error) {
	r, err := s.ResolveFor(ctx, ResolveRequest{Harness: harness, Policy: policy, RunID: runID})
	if err != nil {
		return "", nil, nil, err
	}
	return r.ProfileDir, r.Env, r.Assignment, nil
}

// candidate is one account considered for selection.
type candidate struct {
	acct     contracts.Account
	grant    grantRow
	ver      verdict
	class    availClass
	headroom float64
	// room is the ranking key of a fresh candidate (5h used, then weekly
	// used); zero (unknown, ranks last) for every other class.
	room   usage.Headroom
	reset  *time.Time
	fresh  contracts.Freshness
	why    string
	active int
	pref   int
	// verifiable: a checked-in rota grant whose usage cannot be observed
	// while checked in (Codex needs an app-server on the profile) but can be
	// verified from the profile right after checkout. verifyNote says why
	// not when false (e.g. the snapshot's access token expired).
	verifiable bool
	verifyNote string
	// refreshable: the same, but the snapshot's access token expired at
	// rest; after checkout the harness refreshes its own login in the
	// profile (Config.LoginRefresher), then the usage probe decides.
	refreshable bool
	refreshed   bool // the harness refreshed the login at checkout
	// Machine affinity (affinity.go).
	fleet  *fleetUse // hub view of other machines' runs; nil: none configured
	home   bool      // this machine's current/last account
	near   bool      // a window at/above usage.NearPercent (or none reported)
	senior bool      // home and shared: this machine was on it first
	others []string  // other machines with active runs on it (sorted ids)
	tie    uint64    // per-node hash for equal headroom
}

type availClass int

const (
	classFresh availClass = iota
	classUnknown
	classCooldown
	classUnusable
	// classVerify: unknown only because the account is checked in and not
	// observable there; selection may check it out to verify its usage from
	// the profile before the run is admitted (see verifyAtCheckout).
	classVerify
	// classRefresh: "eligible, unverified": checked in with an access token
	// expired at rest. Ranked below fresh and verify. ResolveFor checks it
	// out, lets the harness refresh its login in the profile, and admits the
	// run only on a fresh eligible probe (before the run, and so its first
	// prompt, exists).
	classRefresh
)

func (c availClass) String() string {
	return [...]string{"fresh", "unknown", "cooldown", "unusable", "verify", "refresh"}[c]
}

// classify turns the latest observation and cooldown into an availability
// class. Stale or missing data is unknown, never capacity.
func (s *Service) classify(obs *contracts.AccountObservation, cd *cooldown, now time.Time) (availClass, float64, *time.Time, contracts.Freshness, string) {
	fr := contracts.Freshness{Source: "none", Stale: true, Reason: "no usage observation"}
	if obs != nil {
		fr = contracts.Freshness{ObservedAt: obs.ObservedAt, Source: obs.Source, Stale: obs.Stale}
	}
	if obs != nil && obs.Eligibility == contracts.EligibleUnusable {
		// Needs re-auth (reauth.go) outranks its cooldown: an intervention,
		// never capacity to wait for.
		return classUnusable, 0, nil, fr, "login unusable: " + obs.Detail
	}
	if cd != nil && cd.Until.After(now) {
		u := cd.Until
		return classCooldown, 0, &u, fr, "cooldown until " + u.Format(time.RFC3339) + " (" + cd.Reason + ")"
	}
	if obs == nil {
		return classUnknown, 0, nil, fr, "no usage observation"
	}
	age := now.Sub(obs.ObservedAt)
	switch obs.Eligibility {
	case contracts.EligibleUnusable:
		return classUnusable, 0, nil, fr, "login unusable: " + obs.Detail
	case contracts.EligibleExhausted, contracts.EligibleCooldown:
		if obs.ResetAt != nil && obs.ResetAt.After(now) {
			u := *obs.ResetAt
			return classCooldown, 0, &u, fr, fmt.Sprintf("%s until %s", obs.Eligibility, u.Format(time.RFC3339))
		}
		if obs.ResetAt == nil && age <= s.cfg.FreshFor {
			return classCooldown, 0, nil, fr, string(obs.Eligibility) + " (reset unknown)"
		}
		fr.Stale, fr.Reason = true, "reset passed; awaiting a new observation"
		return classUnknown, 0, nil, fr, fr.Reason
	}
	if obs.Eligibility == contracts.EligibleUnknown {
		fr.Stale, fr.Reason = true, "eligibility unknown"
		why := "availability unknown"
		if obs.Detail != "" {
			why += ": " + obs.Detail
		}
		return classUnknown, 0, nil, fr, why + fmt.Sprintf(" (observed %s ago)", age.Round(time.Second))
	}
	if obs.Stale || age > s.cfg.FreshFor {
		fr.Stale, fr.Reason = true, fmt.Sprintf("observation %s old (fresh window %s)", age.Round(time.Second), s.cfg.FreshFor)
		return classUnknown, 0, nil, fr, "availability unknown: " + fr.Reason
	}
	if obs.Eligibility != contracts.EligibleYes {
		fr.Stale, fr.Reason = true, "eligibility "+string(obs.Eligibility)
		return classUnknown, 0, nil, fr, "availability unknown"
	}
	// The extra-usage budget is not a plan limit (usage.IsLimit).
	maxUsed, reset := usage.MaxUsed(obs.Windows)
	if maxUsed >= s.cfg.ExhaustedPercent {
		return classCooldown, 0, reset, fr, fmt.Sprintf("window at %.0f%%", maxUsed)
	}
	return classFresh, 100 - maxUsed, nil, fr, fmt.Sprintf("fresh: %s (observed %s ago)", usage.HeadroomOf(obs.Windows).Numbers(), age.Round(time.Second))
}

// gather verifies every same-provider grant on this node (filesystem only)
// and returns the candidates. Verification state changes are persisted.
func (s *Service) gather(ctx context.Context, req ResolveRequest) ([]*candidate, []string, error) {
	provider := contracts.ProviderFor(req.Harness)
	accts, err := listAccounts(ctx, s.db, provider)
	if err != nil {
		return nil, nil, err
	}
	excluded := map[string]bool{}
	for _, id := range req.Exclude {
		excluded[id] = true
	}
	var out []*candidate
	var notes []string
	for _, a := range accts {
		if excluded[a.AccountID] {
			continue
		}
		if req.Policy.Mode == contracts.AccountExplicit && a.AccountID != req.Policy.AccountID {
			continue
		}
		if a.Disabled {
			notes = append(notes, a.AccountID+": disabled")
			continue
		}
		if a.Kind != "" && a.Kind != "subscription" {
			notes = append(notes, a.AccountID+": not a subscription (API billing is never selected)")
			continue
		}
		g, err := s.getGrant(ctx, s.db, a.AccountID, req.Harness)
		if contracts.IsCode(err, contracts.ErrNotFound) {
			notes = append(notes, a.AccountID+": no "+string(req.Harness)+" profile on this node")
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		c := &candidate{acct: a, grant: g, pref: len(req.Policy.Prefer)}
		for i, p := range req.Policy.Prefer {
			if p == a.AccountID {
				c.pref = i
				break
			}
		}
		switch {
		case g.Kind == contracts.CredClaudeTokenFD:
			c.ver = verdict{Grant: g, Outcome: contracts.FallbackIntervention,
				Reason: "claude-oauth-token-fd grants need launch FD support (contract request); not launchable yet"}
		case g.Kind == contracts.CredOAuthToken:
			c.ver = s.tokenVerdict(ctx, a, g) // token.go
			c.grant = c.ver.Grant
			if !c.ver.OK {
				// Token removed or rejected: back on the account's own login
				// when it has one (tokenmerge.go), verified as usual.
				if rg, ok := s.maybeRevert(ctx, g.GrantID); ok {
					c = &candidate{acct: a, grant: rg, pref: c.pref}
					c.ver, c.grant, c.verifiable, c.refreshable, c.verifyNote = s.verifyLogin(ctx, a, rg)
				}
			}
		case g.Kind == CredRotaCheckout && !s.cfg.Rota.Enabled:
			c.ver = verdict{Grant: g, Outcome: contracts.FallbackIntervention, Reason: "rota checkouts are disabled on this node"}
		case g.Kind == CredRotaCheckout && !s.checkedOut(ctx, g.GrantID):
			c.ver = s.verifyRotaGrant(a, g)
			s.persistVerdict(ctx, c.ver)
			c.grant = c.ver.Grant
			if c.ver.OK {
				c.verifiable, c.refreshable, c.verifyNote = s.verifiableAtCheckout(a, c.grant)
			}
		default:
			c.ver = s.verifyGrant(ctx, a, g)
			s.persistVerdict(ctx, c.ver)
			c.grant = c.ver.Grant
		}
		out = append(out, c)
	}
	if req.Policy.Mode != contracts.AccountExplicit && len(out) > 1 {
		// Read before the selection transaction (network, short timeout).
		f := s.fleetView(ctx)
		for _, c := range out {
			c.fleet = f
		}
	}
	return out, notes, nil
}

// noneEligible builds the waiting/intervention error.
func noneEligible(provider contracts.Provider, cands []*candidate, notes []string, extra string) *contracts.Error {
	var earliest *time.Time
	waiting := false
	reasons := append([]string{}, notes...)
	for _, c := range cands {
		r := c.acct.AccountID + ": "
		switch {
		case !c.ver.OK:
			r += c.ver.Reason
		default:
			r += c.why
			if c.class == classCooldown || c.class == classUnknown || c.class == classFresh || c.class == classVerify || c.class == classRefresh {
				waiting = true
			}
			if c.reset != nil && (earliest == nil || c.reset.Before(*earliest)) {
				t := *c.reset
				earliest = &t
			}
		}
		reasons = append(reasons, r)
	}
	if extra != "" {
		reasons = append(reasons, extra)
	}
	outcome := contracts.FallbackIntervention
	code := contracts.ErrUnavailable
	if waiting {
		outcome, code = contracts.FallbackWaiting, contracts.ErrCapacityExhausted
	}
	e := contracts.Errorf(code, "no eligible %s subscription on this node: %s", provider, strings.Join(reasons, "; "))
	e.Retryable = waiting
	e = e.WithDetail("outcome", string(outcome)).WithDetail("reasons", reasons)
	if earliest != nil {
		e = e.WithDetail("earliest_reset", earliest.Format(time.RFC3339))
	}
	return e
}

// rank orders candidates: preference order, then the machine-affinity tier
// (affinity.go: home, free, shared, rest), then most fresh headroom (the
// lower 5-hour use, then the lower weekly use; usage.Headroom.Compare),
// then fewest active local assignments, then a per-node hash and the id
// (deterministic).
func rank(cs []*candidate) {
	sort.SliceStable(cs, func(i, j int) bool {
		a, b := cs[i], cs[j]
		if a.pref != b.pref {
			return a.pref < b.pref
		}
		ta, tb := a.tier(), b.tier()
		if ta != tb {
			return ta < tb
		}
		if ta == tierShared {
			if len(a.others) != len(b.others) {
				return len(a.others) < len(b.others)
			}
			if a.home != b.home {
				return a.home
			}
		}
		if d := a.room.Compare(b.room); d != 0 {
			return d < 0
		}
		if len(a.others) != len(b.others) {
			return len(a.others) < len(b.others)
		}
		if a.active != b.active {
			return a.active < b.active
		}
		if a.tie != b.tie {
			return a.tie < b.tie
		}
		return a.acct.AccountID < b.acct.AccountID
	})
}

// choose picks a candidate inside tx (current observations, cooldowns,
// assignments and leases). It returns the chosen candidate or a
// waiting/intervention error. dryRun skips lease/admission side effects.
func (s *Service) choose(ctx context.Context, q querier, req ResolveRequest, cands []*candidate, notes []string, holder string) (*candidate, error) {
	now := s.now()
	provider := contracts.ProviderFor(req.Harness)
	var fresh, verify, refresh, unknown []*candidate
	var blocked []string
	for _, c := range cands {
		if !c.ver.OK {
			continue
		}
		// The grant must still be the one we verified (no concurrent re-login).
		cur, err := s.grantByID(ctx, q, c.grant.GrantID)
		if err != nil {
			return nil, err
		}
		if cur.Generation != c.grant.Generation || cur.State != contracts.GrantValid && cur.State != contracts.GrantExpiring {
			c.ver.OK, c.ver.Reason = false, "grant changed during selection (generation/state)"
			continue
		}
		obs, err := getObservation(ctx, q, c.acct.AccountID)
		if err != nil {
			return nil, err
		}
		cd, err := getCooldown(ctx, q, c.acct.AccountID)
		if err != nil {
			return nil, err
		}
		c.class, c.headroom, c.reset, c.fresh, c.why = s.classify(obs, cd, now)
		c.room = usage.Headroom{}
		if c.class == classFresh {
			c.room = usage.HeadroomOf(obs.Windows)
		}
		if c.class == classUnknown {
			if c.verifiable {
				c.class = classVerify
			} else if c.refreshable {
				c.class = classRefresh
			} else if c.verifyNote != "" {
				c.why += "; " + c.verifyNote
			}
		}
		if (c.class == classVerify || c.class == classRefresh) && obs != nil {
			// Unverified: the last-known windows decide, exactly as the
			// owner sees it (usage.Judge): at a limit whose reset is ahead
			// it waits; otherwise it ranks by its last-known headroom.
			lk := usage.Judge(obs.Windows, now)
			if lk.Limited {
				c.class, c.reset = classCooldown, lk.Until
				c.why = "last known usage at its limit; " + c.why
			} else {
				c.room = lk.Room
			}
		}
		act, err := s.activeAssignments(ctx, q, c.acct.AccountID)
		if err != nil {
			return nil, err
		}
		c.active = len(act)
		if s.cfg.MaxAssignmentsPerAccount > 0 && c.active >= s.cfg.MaxAssignmentsPerAccount {
			blocked = append(blocked, fmt.Sprintf("%s: %d active assignments (limit %d)", c.acct.AccountID, c.active, s.cfg.MaxAssignmentsPerAccount))
			if c.class == classFresh || c.class == classUnknown || c.class == classVerify || c.class == classRefresh {
				c.class, c.why = classCooldown, "admission limit reached"
			}
			continue
		}
		if c.grant.MaxProcs == 1 {
			l, err := getLease(ctx, q, c.grant.GrantID)
			if err != nil {
				return nil, err
			}
			if l != nil && l.Holder != holder {
				msg := fmt.Sprintf("%s: process lease held by %q (one Codex process per home)", c.acct.AccountID, l.Holder)
				if req.Policy.Mode == contracts.AccountExplicit {
					e := contracts.Errorf(contracts.ErrConflict, "%s", msg).WithRef(contracts.KindGrant, c.grant.GrantID).
						WithDetail("outcome", string(contracts.FallbackIntervention))
					return nil, e
				}
				blocked = append(blocked, msg)
				continue
			}
		} else if c.grant.MaxProcs > 1 {
			n := 0
			for _, a := range act {
				if a.GrantID == c.grant.GrantID {
					n++
				}
			}
			if n >= c.grant.MaxProcs {
				blocked = append(blocked, fmt.Sprintf("%s: grant process limit %d reached", c.acct.AccountID, c.grant.MaxProcs))
				continue
			}
		}
		switch c.class {
		case classFresh:
			fresh = append(fresh, c)
		case classVerify:
			verify = append(verify, c)
		case classRefresh:
			refresh = append(refresh, c)
		case classUnknown:
			unknown = append(unknown, c)
		}
	}
	if err := s.markAffinity(ctx, q, provider, cands); err != nil {
		return nil, err
	}
	// A token account outranks the same person's other logins (token.go).
	pt := preferTokens(fresh, fresh, verify, refresh, unknown)
	fresh, verify, refresh, unknown = pt[0], pt[1], pt[2], pt[3]
	rank(fresh)
	rank(verify)
	rank(refresh)
	rank(unknown)
	explicit := req.Policy.Mode == contracts.AccountExplicit
	switch {
	case len(fresh) > 0:
		// The reason the owner reads in the launch review and the
		// assignment: "most headroom (5h 12% used, weekly 40%)".
		c := fresh[0]
		c.why = s.explainAuto(c, fresh, cands)
		if c.pref < len(req.Policy.Prefer) {
			c.why = "preferred account (" + c.room.Numbers() + ")"
		}
		if explicit {
			// Not a choice among candidates: the starter named it (seen live
			// as "only eligible account" for --account anthropic).
			c.why = "explicit account (" + c.room.Numbers() + ")"
		}
		return c, nil
	case len(verify) > 0:
		// Not capacity yet: ResolveFor checks it out and admits the run only
		// if the usage probe from the profile is fresh and eligible.
		c := verify[0]
		c.why = "checked in (usage not observable while checked in); verified from the profile at checkout; last: " + c.why
		return c, nil
	case len(refresh) > 0:
		// Eligible but unverified: the harness refreshes the expired login
		// in the checked-out profile, then the probe decides (ResolveFor).
		c := refresh[0]
		c.why = "eligible, unverified (access token expired at rest); the harness refreshes it in the checked-out profile, then usage is verified; last: " + c.why
		return c, nil
	case len(unknown) > 0 && (explicit || s.cfg.AllowUnknown):
		c := unknown[0]
		if explicit {
			c.why = "explicit account; " + c.why
		} else {
			c.why = "allowed by configuration (AllowUnknown); " + c.why
		}
		return c, nil
	}
	extra := strings.Join(blocked, "; ")
	if len(unknown) > 0 {
		extra = strings.TrimPrefix(extra+"; unknown availability is not treated as capacity (AllowUnknown=false)", "; ")
	}
	return nil, noneEligible(provider, cands, notes, extra)
}

// ResolveFor binds an account to a run (race-safe: selection and the
// assignment insert happen in one immediate transaction).
func (s *Service) ResolveFor(ctx context.Context, req ResolveRequest) (Resolution, error) {
	var res Resolution
	if req.RunID == "" {
		return res, contracts.Errorf(contracts.ErrInvalid, "run_id required")
	}
	provider := contracts.ProviderFor(req.Harness)
	if provider == "" {
		return res, nil // shell/mock: no subscription binding
	}
	if req.Policy.Mode == "" {
		req.Policy.Mode = contracts.AccountEligible
	}
	s.canonicalPolicy(ctx, &req.Policy) // accounts the hub folded (tokenmerge.go)
	for i, id := range req.Exclude {
		req.Exclude[i] = s.canonicalAccount(ctx, id)
	}
	// Idempotent retry: return the existing binding.
	if a, err := s.assignmentForRun(ctx, s.db, req.RunID); err == nil {
		if a.ReleasedAt != nil {
			return res, contracts.Errorf(contracts.ErrConflict, "run's account assignment was already released").WithRef(contracts.KindRun, req.RunID)
		}
		return s.resolutionFor(ctx, req.Harness, a)
	} else if !contracts.IsCode(err, contracts.ErrNotFound) {
		return res, err
	}

	ambientReason := "credential_kind=ambient: node default login; no Pyre isolation; never used for automatic fallback"
	if req.Policy.Mode == contracts.AccountEligible {
		if why, err := s.defaultAmbient(ctx, req.Harness); err != nil {
			return res, err
		} else if why != "" {
			req.Policy.Mode = contracts.AccountAmbient
			ambientReason = "credential_kind=ambient: " + why + "; never used for automatic fallback"
		}
	}
	// ambient binds the node's own login. acct is the account that login is
	// (explicit own-login account); nil looks it up, so the assignment names
	// the account usage and the audit log attribute the run to.
	ambient := func(reason string, acct *contracts.Account) (Resolution, error) {
		if acct == nil {
			if oa, ok := s.ownLoginAccount(ctx, req.Harness); ok {
				acct = &oa
				reason += "; own login is account " + accountName(oa)
			}
		}
		id := ""
		if acct != nil {
			id = acct.AccountID
		}
		a := assignmentRow{AccountAssignment: contracts.AccountAssignment{
			AssignmentID: contracts.NewID(contracts.PrefixAssignment), RunID: req.RunID, SessionID: req.SessionID,
			NodeID: s.cfg.NodeID, AssignedAt: s.now(), AccountID: id,
			Reason:    reason,
			Freshness: contracts.Freshness{ObservedAt: s.now(), Source: "node:" + s.cfg.NodeID, Stale: true, Reason: "ambient login not observed"},
		}, CredentialKind: contracts.CredAmbient}
		if err := insertAssignment(ctx, s.db, a); err != nil {
			return res, err
		}
		s.report(ctx, contracts.ReportAssignment, a.AccountAssignment)
		return s.resolutionFor(ctx, req.Harness, a)
	}
	if req.Policy.Mode == contracts.AccountAmbient {
		return ambient(ambientReason, nil)
	}
	// fallThrough: an eligible launch whose accounts all turned out to need
	// re-auth while selecting (a 401 at checkout verification) runs on the
	// node's own login under the auto default instead of failing.
	fallThrough := func(err error) (Resolution, error) {
		if req.Policy.Mode != contracts.AccountEligible {
			return res, err
		}
		why, derr := s.defaultAmbient(ctx, req.Harness)
		if derr == nil && why == "" && s.cfg.DefaultAccount != DefaultAccountSubscription {
			// The node's own login is a registered account the hub counts as
			// ready (ownaccount.go): run on it rather than wait or fail.
			if oa, ok := s.ownLoginCandidate(ctx, req.Harness, req.Exclude); ok {
				s.log.Info("accounts: no Pyre login eligible; using the node's own login", "run", req.RunID, "account", oa.AccountID)
				return ambient(fmt.Sprintf("credential_kind=ambient: no Pyre login on this node is eligible; %s is this node's own %s login", accountName(oa), req.Harness), &oa)
			}
		}
		if derr != nil || why == "" {
			return res, err
		}
		s.log.Info("accounts: no usable account; using the node's own login", "run", req.RunID, "why", why)
		return ambient("credential_kind=ambient: "+why, nil)
	}
	if req.Policy.Mode == contracts.AccountExplicit {
		a, err := s.Account(ctx, req.Policy.AccountID)
		var local *contracts.Account
		switch {
		case contracts.IsCode(err, contracts.ErrNotFound):
			// Not on this node: perhaps the machines' own login (ownaccount.go).
		case err != nil:
			return res, err
		case a.Provider != provider:
			return res, contracts.Errorf(contracts.ErrInvalid, "account %s is %s; harness %s needs %s (cross-provider use is never allowed)",
				a.AccountID, a.Provider, req.Harness, provider)
		default:
			local = &a
		}
		if local == nil || !s.hasGrant(ctx, a.AccountID, req.Harness) && MatchesOwnLogin(a, s.OwnLogin(req.Harness)) {
			oa, oerr := s.explicitOwnLogin(ctx, req.Harness, req.Policy.AccountID, local)
			if oerr != nil {
				return res, oerr
			}
			return ambient(fmt.Sprintf("explicit account; credential_kind=ambient: %s is this node's own %s login", accountName(oa), req.Harness), &oa)
		}
	} else if req.Policy.Mode != contracts.AccountEligible {
		return res, contracts.Errorf(contracts.ErrInvalid, "unknown account policy mode %q", req.Policy.Mode)
	}
	holder := req.LeaseHolder
	if holder == "" {
		holder = s.cfg.CodexLeaseHolder
	}

	var lastCheckoutErr error
	for round := 0; round < 4; round++ {
		cands, notes, err := s.gather(ctx, req)
		if err != nil {
			return res, err
		}
		var chosen *candidate
		var asg assignmentRow
		attempt := func() error {
			return sqlitex.Tx(ctx, s.db, func(tx *sql.Tx) error {
				c, err := s.choose(ctx, tx, req, cands, notes, holder)
				if err != nil {
					return err
				}
				lease := ""
				if c.grant.MaxProcs == 1 {
					if err := acquireLease(ctx, tx, c.grant.GrantID, holder, s.now()); err != nil {
						return err
					}
					lease = holder
				}
				asg = assignmentRow{AccountAssignment: contracts.AccountAssignment{
					AssignmentID: contracts.NewID(contracts.PrefixAssignment), AccountID: c.acct.AccountID,
					GrantID: c.grant.GrantID, GrantGeneration: c.grant.Generation, Lease: lease,
					RunID: req.RunID, SessionID: req.SessionID, NodeID: s.cfg.NodeID,
					Reason: c.why, AssignedAt: s.now(), Freshness: c.fresh,
				}, CredentialKind: c.grant.Kind}
				chosen = c
				return insertAssignment(ctx, tx, asg)
			})
		}
		err = attempt()
		if err != nil && s.shouldObserveAndRetry(err) {
			// Nothing fresh: take one bounded observation pass (respects each
			// account's poll schedule/back-off) and try once more.
			s.ObserveDue(ctx, provider)
			err = attempt()
		}
		if err != nil {
			if a, aerr := s.assignmentForRun(ctx, s.db, req.RunID); aerr == nil && a.ReleasedAt == nil {
				return s.resolutionFor(ctx, req.Harness, a) // a concurrent duplicate won
			}
			if lastCheckoutErr != nil && contracts.IsCode(err, contracts.ErrCapacityExhausted) || lastCheckoutErr != nil && contracts.IsCode(err, contracts.ErrUnavailable) {
				return fallThrough(lastCheckoutErr)
			}
			return fallThrough(err)
		}
		if chosen.grant.Kind == CredRotaCheckout {
			// Transfer the rota login into the profile now that the run owns
			// the assignment; a refused transfer aborts the (never launched)
			// assignment and, for eligible policy, tries the next account.
			if cerr := s.ensureCheckout(ctx, chosen.acct, chosen.grant, req.RunID); cerr != nil {
				s.abortAssignment(ctx, asg)
				if req.Policy.Mode != contracts.AccountEligible {
					return res, cerr
				}
				lastCheckoutErr = cerr
				req.Exclude = append(req.Exclude, chosen.acct.AccountID)
				continue
			}
			if chosen.class == classVerify || chosen.class == classRefresh {
				explicit := req.Policy.Mode == contracts.AccountExplicit
				verr := error(nil)
				if chosen.class == classRefresh {
					verr = s.refreshAtCheckout(ctx, chosen, explicit)
				}
				if verr == nil && chosen.class == classRefresh && !chosen.refreshed {
					// explicit and the refresh was inconclusive: launch as
					// before (the run's harness refreshes on use).
				} else if verr == nil {
					verr = s.verifyAtCheckout(ctx, chosen, explicit)
				}
				if verr != nil {
					// Never launched: drop the assignment and return ownership
					// to rota at once.
					s.abortAssignment(ctx, asg)
					s.maybeCheckin(ctx, chosen.grant.GrantID)
					if req.Policy.Mode != contracts.AccountEligible {
						return res, verr
					}
					lastCheckoutErr = verr
					req.Exclude = append(req.Exclude, chosen.acct.AccountID)
					continue
				}
				asg.Reason, asg.Freshness = chosen.why, chosen.fresh
				s.updateAssignmentReason(ctx, asg)
			}
		}
		s.log.Info("accounts: assigned", "run", req.RunID, "account", chosen.acct.AccountID, "reason", chosen.why)
		s.report(ctx, contracts.ReportAssignment, asg.AccountAssignment)
		return s.resolutionFor(ctx, req.Harness, asg)
	}
	return fallThrough(lastCheckoutErr)
}

// abortAssignment removes an assignment whose run never launched (failed
// rota checkout) and drops its process-lease reference.
func (s *Service) abortAssignment(ctx context.Context, a assignmentRow) {
	err := sqlitex.Tx(ctx, s.db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM accounts_assignment WHERE assignment_id=?`, a.AssignmentID); err != nil {
			return err
		}
		if a.Lease != "" {
			return releaseLease(ctx, tx, a.GrantID, a.Lease)
		}
		return nil
	})
	if err != nil {
		s.log.Warn("accounts: abort assignment", "run", a.RunID, "err", err)
	}
}

func (s *Service) shouldObserveAndRetry(err error) bool {
	e := contracts.AsError(err)
	if e == nil || e.Details == nil {
		return false
	}
	return e.Details["outcome"] == string(contracts.FallbackWaiting)
}

func (s *Service) resolutionFor(ctx context.Context, h contracts.Harness, a assignmentRow) (Resolution, error) {
	asg := a.AccountAssignment
	res := Resolution{Assignment: &asg, CredentialKind: a.CredentialKind}
	if a.CredentialKind == contracts.CredAmbient {
		res.Unset = append([]string{}, unsetAmbient[h]...)
		return res, nil
	}
	g, err := s.grantByID(ctx, s.db, a.GrantID)
	if err != nil {
		return res, err
	}
	if g.Generation != a.GrantGeneration {
		return res, contracts.Errorf(contracts.ErrConflict, "assignment pins grant generation %d; grant is now %d", a.GrantGeneration, g.Generation).WithRef(contracts.KindGrant, g.GrantID)
	}
	dg := g.DeviceGrant
	res.Grant = &dg
	res.ProfileDir = g.ProfileDir
	res.Env = map[string]string{harnessEnvVar(h): g.ProfileDir}
	s.refreshComposition(g.ProfileDir, h) // copies current; the owner is never written
	switch h {
	case contracts.HarnessClaude:
		res.Unset = append(append([]string{}, unsetClaude...), unsetCodex...)
		for k, v := range ClaudeProfileEnv() {
			res.Env[k] = v
		}
	case contracts.HarnessCodex:
		res.Unset = append(append([]string{}, unsetCodex...), unsetClaude...)
	}
	if g.Kind == contracts.CredOAuthToken {
		if err := s.tokenEnv(ctx, g, &res); err != nil { // token.go
			return Resolution{}, err
		}
	}
	return res, nil
}

// Release ends a run's assignment (idempotent) and drops its lease
// reference. The assignment row is kept for audit.
func (s *Service) Release(runID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := s.ReleaseRun(ctx, runID); err != nil && !contracts.IsCode(err, contracts.ErrNotFound) {
		s.log.Warn("accounts: release", "run", runID, "err", err)
	}
}

// ReleaseRun is Release with a context and the released assignment.
func (s *Service) ReleaseRun(ctx context.Context, runID string) (contracts.AccountAssignment, error) {
	var out assignmentRow
	changed := false
	err := sqlitex.Tx(ctx, s.db, func(tx *sql.Tx) error {
		a, err := s.assignmentForRun(ctx, tx, runID)
		if err != nil {
			return err
		}
		out = a
		if a.ReleasedAt != nil {
			return nil
		}
		now := s.now()
		if _, err := tx.ExecContext(ctx, `UPDATE accounts_assignment SET released_at=? WHERE assignment_id=?`, ft(now), a.AssignmentID); err != nil {
			return err
		}
		if a.Lease != "" {
			if err := releaseLease(ctx, tx, a.GrantID, a.Lease); err != nil {
				return err
			}
		}
		out.ReleasedAt = &now
		changed = true
		return nil
	})
	if err == nil && changed {
		s.report(ctx, contracts.ReportAssignment, out.AccountAssignment)
		s.releaseRotaHook(ctx, out)
	}
	return out.AccountAssignment, err
}

// CheckAssignment re-validates a run's binding (before launch, on recover):
// still active, grant not dead, and the pinned generation is current.
func (s *Service) CheckAssignment(ctx context.Context, runID string) error {
	a, err := s.assignmentForRun(ctx, s.db, runID)
	if err != nil {
		return err
	}
	if a.ReleasedAt != nil {
		return contracts.Errorf(contracts.ErrConflict, "assignment released").WithRef(contracts.KindAssignment, a.AssignmentID)
	}
	if a.CredentialKind == contracts.CredAmbient {
		return nil
	}
	g, err := s.grantByID(ctx, s.db, a.GrantID)
	if err != nil {
		return err
	}
	switch {
	case g.Generation != a.GrantGeneration:
		return contracts.Errorf(contracts.ErrConflict, "stale grant generation %d (current %d)", a.GrantGeneration, g.Generation).WithRef(contracts.KindGrant, g.GrantID)
	case g.State == contracts.GrantDead || g.State == contracts.GrantRevoked || g.State == contracts.GrantExpired:
		return contracts.Errorf(contracts.ErrUnavailable, "grant %s: %s", g.State, g.Detail).WithRef(contracts.KindGrant, g.GrantID)
	}
	return nil
}

// Select is the dry-run form (account.select): it verifies and ranks but
// creates no assignment and takes no lease.
func (s *Service) Select(ctx context.Context, p contracts.AccountSelectParams) (contracts.AccountSelection, error) {
	h := harnessForProvider(p.Provider)
	if h == "" {
		return contracts.AccountSelection{}, contracts.Errorf(contracts.ErrInvalid, "unknown provider %q", p.Provider)
	}
	if p.Policy.Mode == "" {
		p.Policy.Mode = contracts.AccountEligible
	}
	s.canonicalPolicy(ctx, &p.Policy)
	sel := contracts.AccountSelection{Provider: p.Provider}
	if p.Policy.Mode == contracts.AccountEligible {
		why, err := s.defaultAmbient(ctx, h)
		if err != nil {
			return sel, err
		}
		if why != "" {
			if oa, ok := s.ownLoginAccount(ctx, h); ok {
				sel.AccountID = oa.AccountID
				why += "; own login is account " + accountName(oa)
			}
			sel.Reason = why
			sel.Freshness = contracts.Freshness{ObservedAt: s.now(), Source: "node:" + s.cfg.NodeID, Stale: true, Reason: "ambient login not observed"}
			return sel, nil
		}
	}
	if p.Policy.Mode == contracts.AccountAmbient {
		sel.Reason = "credential_kind=ambient: node default login (no Pyre assignment)"
		sel.Freshness = contracts.Freshness{ObservedAt: s.now(), Source: "node:" + s.cfg.NodeID, Stale: true, Reason: "ambient login not observed"}
		return sel, nil
	}
	if p.Policy.Mode == contracts.AccountExplicit {
		a, err := s.Account(ctx, p.Policy.AccountID)
		if contracts.IsCode(err, contracts.ErrNotFound) || err == nil && a.Provider == p.Provider && !s.hasGrant(ctx, a.AccountID, h) && MatchesOwnLogin(a, s.OwnLogin(h)) {
			var local *contracts.Account
			if err == nil {
				local = &a
			}
			oa, oerr := s.explicitOwnLogin(ctx, h, p.Policy.AccountID, local)
			if oerr != nil {
				return sel, oerr
			}
			sel.AccountID = oa.AccountID
			sel.Reason = fmt.Sprintf("explicit account; credential_kind=ambient: %s is this node's own %s login", accountName(oa), h)
			sel.Freshness = contracts.Freshness{ObservedAt: s.now(), Source: "node:" + s.cfg.NodeID, Stale: true, Reason: "ambient login not observed"}
			return sel, nil
		}
		if err != nil {
			return sel, err
		}
		if a.Provider != p.Provider {
			return sel, contracts.Errorf(contracts.ErrInvalid, "account %s is %s, not %s (cross-provider use is never allowed)", a.AccountID, a.Provider, p.Provider)
		}
	}
	req := ResolveRequest{Harness: h, Policy: p.Policy, RunID: "dry-run"}
	cands, notes, err := s.gather(ctx, req)
	if err != nil {
		return sel, err
	}
	c, err := s.choose(ctx, s.db, req, cands, notes, s.cfg.CodexLeaseHolder)
	if err != nil {
		e := contracts.AsError(err)
		if o, ok := e.Details["outcome"].(string); ok {
			sel.Outcome = contracts.FallbackOutcome(o)
			sel.Reason = e.Message
			sel.Freshness = contracts.Freshness{ObservedAt: s.now(), Source: "node:" + s.cfg.NodeID, Stale: true, Reason: "no eligible account"}
			return sel, nil
		}
		return sel, err
	}
	sel.AccountID, sel.GrantID, sel.Reason, sel.Freshness = c.acct.AccountID, c.grant.GrantID, c.why, c.fresh
	return sel, nil
}

// Default account policies (pyred --default-account).
const (
	DefaultAccountAuto         = "auto"
	DefaultAccountSubscription = "subscription"
	DefaultAccountAmbient      = "ambient"
)

// ValidDefaultAccount reports whether v is a known default account policy
// ("" means auto).
func ValidDefaultAccount(v string) bool {
	switch v {
	case "", DefaultAccountAuto, DefaultAccountSubscription, DefaultAccountAmbient:
		return true
	}
	return false
}

// defaultAmbient decides whether a launch without an explicit account
// (mode eligible) runs on the node's own login, and says why ("" = select an
// eligible subscription). auto: only when this node has no Pyre account
// (grant) for the harness at all; a node WITH accounts keeps subscription
// selection and waits when they are exhausted, never falling back to the
// owner's login.
func (s *Service) defaultAmbient(ctx context.Context, h contracts.Harness) (string, error) {
	switch s.cfg.DefaultAccount {
	case DefaultAccountSubscription:
		return "", nil
	case DefaultAccountAmbient:
		return "your own login on this node (pyred --default-account ambient)", nil
	}
	// Only enabled accounts count: an account the owner disabled leaves the
	// node to its own login, like a node that never had one.
	accts, err := listAccounts(ctx, s.db, contracts.ProviderFor(h))
	if err != nil {
		return "", err
	}
	// A login that needs re-auth (reauth.go) counts as none either: the
	// node runs on its own login instead of failing every launch.
	var dead []string
	for _, a := range accts {
		if a.Disabled {
			continue
		}
		var n int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM accounts_grant WHERE harness=? AND account_id=?`, string(h), a.AccountID).Scan(&n); err != nil {
			return "", err
		}
		if n == 0 {
			continue
		}
		bad, err := s.needsReauth(ctx, a.AccountID, h)
		if err != nil {
			return "", err
		}
		if !bad {
			return "", nil
		}
		dead = append(dead, accountName(a))
	}
	if len(dead) > 0 {
		return fmt.Sprintf("your own login on this node (%s needs re-auth: %s)", strings.Join(dead, ", "),
			s.reloginHint(contracts.Account{Provider: contracts.ProviderFor(h)})), nil
	}
	return fmt.Sprintf("your own login on this node (this node has no Pyre %s accounts)", contracts.ProviderFor(h)), nil
}
