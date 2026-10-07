package accounts

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/usage"
)

// Token accounts (contracts.CredOAuthToken). The hub holds the token; the
// node fetches it over its authenticated link (Config.Tokens) when a run is
// bound to the account or its usage is probed, and keeps it in memory only.
// The run gets it solely through its private environment file (decision
// I18) as the provider's token variable, with an isolated profile that
// holds no credential (I22 composition). A token is not a refresh family,
// so the same token runs on several nodes at once: no fleet lease, no
// process limit.
//
// Usage: a minimal Messages request (1 output token on Haiku) returns the
// unified rate-limit headers (5-hour and weekly utilization, reset and
// status), read into the ordinary observation model. Probes run at most
// every TokenProbeEvery per account while the account is in use on this
// node, at selection when the reading is older than that, and on demand
// (account.observe {"force":true}, `pyre accounts --refresh`), never more
// than once a minute.

// TokenSource fetches a hub-held token (node credential). Implementations
// must not log, persist or report the value.
type TokenSource interface {
	FetchAccountToken(ctx context.Context, accountID string) (token string, updatedAt time.Time, err error)
}

const (
	// TokenProbeEvery bounds token usage probes per account (in use) and is
	// the age at which selection re-probes before choosing.
	TokenProbeEvery = 10 * time.Minute
	// TokenIdleProbeEvery re-reads an account no run uses here.
	TokenIdleProbeEvery = 20 * time.Minute
	// tokenRefreshMin is the floor for on-demand probes.
	tokenRefreshMin = time.Minute
	// tokenCacheFor: a cached token is refetched after this (a replaced or
	// removed token is noticed); on a hub outage the cached copy serves up
	// to tokenCacheMax.
	tokenCacheFor = 10 * time.Minute
	tokenCacheMax = 12 * time.Hour
	// TokenProbeModel is the model of the 1-token usage probe.
	TokenProbeModel = "claude-haiku-4-5-20251001"

	tokenRejected = "token revoked or expired — run `claude setup-token` and `pyre accounts add-token` again"
	tokenRemoved  = "no token on the hub for this account — run `pyre accounts add-token` again"
)

// tokenSpec is what a harness needs for token accounts. A harness without
// launch support registers and shows its account but is never selected.
type tokenSpec struct {
	Env        string // the run's token variable
	Launch     bool
	LaunchWhy  string // why not, when !Launch
	Probe      bool   // usage probe implemented
	ProbeWhy   string
	ObsSource  string
	RejectHint string
}

var tokenSpecs = map[contracts.Harness]tokenSpec{
	contracts.HarnessClaude: {Env: "CLAUDE_CODE_OAUTH_TOKEN", Launch: true, Probe: true,
		ObsSource: contracts.UsageSourceMessagesProbe, RejectHint: tokenRejected},
	// Codex 0.160 reads CODEX_ACCESS_TOKEN (personal access token mode).
	// Launch and usage through the app-server in token mode are not
	// verified yet: the account registers and shows, but is not selected.
	contracts.HarnessCodex: {Env: "CODEX_ACCESS_TOKEN",
		LaunchWhy: "Codex access-token launch is not verified yet (no token-mode app-server check); not selected",
		ProbeWhy:  "usage of Codex access tokens is not observed yet", ObsSource: "token:codex",
		RejectHint: "token revoked or expired — create a new Codex access token and run `pyre accounts add-token` again"},
}

// TokenEnvVar is the variable a token run of harness h gets ("" = none).
func TokenEnvVar(h contracts.Harness) string { return tokenSpecs[h].Env }

type cachedToken struct {
	v       secret
	updated time.Time
	fetched time.Time
}

type tokenCache struct {
	mu     sync.Mutex
	m      map[string]cachedToken
	probed map[string]time.Time
	// rejected: the hub UpdatedAt of a token the provider rejected here; a
	// sync re-attaches the account only for a newer token.
	rejected map[string]time.Time
}

func (c *tokenCache) reject(id string, updated time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.rejected == nil {
		c.rejected = map[string]time.Time{}
	}
	if updated.IsZero() {
		updated = time.Now()
	}
	c.rejected[id] = updated
}

func (c *tokenCache) rejectedAt(id string) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rejected[id]
}

func (c *tokenCache) get(id string) (cachedToken, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.m[id]
	return t, ok
}

func (c *tokenCache) put(id string, t cachedToken) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]cachedToken{}
	}
	c.m[id] = t
}

func (c *tokenCache) drop(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, id)
}

func (c *tokenCache) lastProbe(id string) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.probed[id]
}

func (c *tokenCache) markProbe(id string, t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.probed == nil {
		c.probed = map[string]time.Time{}
	}
	c.probed[id] = t
}

// isTokenGone: the hub has no token for the account, or this node may not
// use it. The node's grant is then revoked until the owner sets a token.
func isTokenGone(err error) bool {
	return contracts.IsCode(err, contracts.ErrNotFound) || contracts.IsCode(err, contracts.ErrForbidden)
}

// token returns the account's token (cached up to tokenCacheFor; fresh
// forces a fetch). A hub outage falls back to a cached copy younger than
// tokenCacheMax.
func (s *Service) token(ctx context.Context, accountID string, fresh bool) (secret, error) {
	now := s.now()
	c, ok := s.tok.get(accountID)
	if ok && !fresh && now.Sub(c.fetched) < tokenCacheFor {
		return c.v, nil
	}
	if s.cfg.Tokens == nil {
		return secret{}, contracts.Errorf(contracts.ErrUnavailable, "this node has no hub link to fetch account tokens")
	}
	fctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	v, upd, err := s.cfg.Tokens.FetchAccountToken(fctx, accountID)
	cancel()
	switch {
	case err == nil && v != "":
		t := cachedToken{v: secret{v: v}, updated: upd, fetched: now}
		s.tok.put(accountID, t)
		return t.v, nil
	case err == nil:
		err = contracts.Errorf(contracts.ErrNotFound, "hub returned an empty token")
	}
	if isTokenGone(err) {
		s.tok.drop(accountID)
		return secret{}, err
	}
	if ok && now.Sub(c.fetched) < tokenCacheMax {
		s.log.Warn("accounts: token fetch failed; using the in-memory copy", "account", accountID, "err", redact(err.Error()))
		return c.v, nil
	}
	ce := contracts.AsError(err)
	return secret{}, contracts.Errorf(contracts.ErrUnavailable, "fetch account token: %s", redact(ce.Message))
}

// registerToken prepares a token grant: an isolated, credential-free
// profile (owner configuration composed as for every profile) and a grant
// that never refreshes. Re-registering after add-token revives a dead or
// revoked grant (new generation) and drops the cached token, then probes.
func (s *Service) registerToken(ctx context.Context, a contracts.Account, h contracts.Harness) (RegisterResult, error) {
	var res RegisterResult
	if _, ok := tokenSpecs[h]; !ok {
		return res, contracts.Errorf(contracts.ErrInvalid, "harness %s has no token accounts", h)
	}
	if a.RotaRef == "" {
		a.PerMachineIndependent = true
	}
	spec := tokenSpecs[h]
	if g, err := s.getGrant(ctx, s.db, a.AccountID, h); err == nil && g.Kind != contracts.CredOAuthToken && !spec.Launch {
		// Never displace a working login with a token this harness cannot
		// launch yet.
		res.Grant, res.Detail = g.DeviceGrant, "token stored on the hub; this node keeps the "+kindWord(g.Kind)+" login for runs ("+spec.LaunchWhy+")"
		return res, nil
	}
	info, err := s.createProfile(ctx, a, h, contracts.CredOAuthToken)
	if err != nil {
		return res, err
	}
	s.tok.drop(a.AccountID)
	s.tok.markProbe(a.AccountID, time.Time{})
	g, err := s.getGrant(ctx, s.db, a.AccountID, h)
	if err != nil {
		return res, err
	}
	if g.State != contracts.GrantValid {
		if g.State != contracts.GrantUnknown {
			g.Generation++ // a revived token is a new login lifecycle
		}
		g.EverValid = true
		g.State, g.Detail, g.ObservedAt = contracts.GrantValid, "token held by the hub; injected at launch", s.now()
		if err := updateGrantState(ctx, s.db, g); err != nil {
			return res, err
		}
	}
	s.report(ctx, contracts.ReportGrant, g.DeviceGrant)
	res.Profile = info
	if _, err := s.token(ctx, a.AccountID, true); err != nil {
		res.Grant, res.Detail = g.DeviceGrant, "token not available to this node: "+contracts.AsError(err).Message
		return res, nil
	}
	if spec.Probe {
		s.obmu.Lock()
		o := s.observeToken(ctx, a, g)
		s.obmu.Unlock()
		if o != nil {
			res.Verified = o.Eligibility != contracts.EligibleUnusable && o.Confidence == "high"
			res.Detail = o.Detail
			if res.Verified && res.Detail == "" {
				res.Detail = "provider accepted the token (" + usage.HeadroomOf(o.Windows).Numbers() + ")"
			}
		}
	} else {
		res.Detail = spec.ProbeWhy
	}
	if cur, err := s.grantByID(ctx, s.db, g.GrantID); err == nil {
		g = cur
	}
	res.Grant = g.DeviceGrant
	return res, nil
}

// tokenVerdict checks a token grant before selection: provider launch
// support, grant state, and that the token is obtainable now (fetched
// into memory, so binding never waits on the hub).
func (s *Service) tokenVerdict(ctx context.Context, a contracts.Account, g grantRow) verdict {
	v := verdict{Grant: g, Outcome: contracts.FallbackIntervention}
	spec := tokenSpecs[g.Harness]
	switch {
	case !spec.Launch:
		v.Reason = spec.LaunchWhy
		return v
	case g.State == contracts.GrantDead || g.State == contracts.GrantRevoked || g.State == contracts.GrantExpired:
		v.Reason = "token " + string(g.State) + ": " + g.Detail
		return v
	}
	if _, err := s.token(ctx, a.AccountID, false); err != nil {
		if isTokenGone(err) {
			s.setGrantState(ctx, g, contracts.GrantRevoked, tokenRemoved)
			v.Reason = tokenRemoved
			return v
		}
		v.Reason = "token unavailable: " + contracts.AsError(err).Message
		v.Outcome = contracts.FallbackWaiting
		return v
	}
	if spec.Probe {
		if o, _ := getObservation(ctx, s.db, a.AccountID); o == nil || s.now().Sub(o.ObservedAt) >= TokenProbeEvery {
			s.obmu.Lock()
			s.observeToken(ctx, a, g)
			s.obmu.Unlock()
			if cur, err := s.grantByID(ctx, s.db, g.GrantID); err == nil {
				g = cur
				v.Grant = cur
			}
			if g.State != contracts.GrantValid {
				v.Reason = "token " + string(g.State) + ": " + g.Detail
				return v
			}
		}
	}
	v.OK = true
	return v
}

// tokenProbeDue decides whether the observer probes a token account now.
func (s *Service) tokenProbeDue(ctx context.Context, a contracts.Account, g grantRow, force bool) bool {
	if !tokenSpecs[g.Harness].Probe || g.State == contracts.GrantDead || g.State == contracts.GrantRevoked {
		return false
	}
	since := s.now().Sub(s.tok.lastProbe(a.AccountID))
	if force {
		return since >= tokenRefreshMin
	}
	if since < TokenProbeEvery {
		return false
	}
	o, _ := getObservation(ctx, s.db, a.AccountID)
	if o == nil {
		return true // never observed here: one reading so the account shows
	}
	if act, err := s.activeAssignments(ctx, s.db, a.AccountID); err == nil && len(act) > 0 {
		return true
	}
	// Idle accounts are re-read now and then (one output token each) so
	// they show as ready instead of "unknown" once a reading goes stale.
	return s.now().Sub(o.ObservedAt) >= TokenIdleProbeEvery
}

// observeToken probes, stores and reports one token observation (caller
// holds obmu). It returns the stored observation (nil when kept).
func (s *Service) observeToken(ctx context.Context, a contracts.Account, g grantRow) *contracts.AccountObservation {
	s.tok.markProbe(a.AccountID, s.now())
	r := s.probeToken(ctx, a, g)
	if r.obs == nil {
		return nil
	}
	r.obs.Source = tokenSpecs[g.Harness].ObsSource
	if err := putObservation(ctx, s.db, *r.obs); err == nil {
		s.report(ctx, contracts.ReportAccountObs, *r.obs)
	}
	return r.obs
}

// probeToken sends the minimal Messages request and reads the unified
// rate-limit headers.
func (s *Service) probeToken(ctx context.Context, a contracts.Account, g grantRow) probeResult {
	spec := tokenSpecs[g.Harness]
	if !spec.Probe {
		return probeResult{obs: s.failObs(ctx, a, contracts.EligibleUnknown, spec.ProbeWhy, true)}
	}
	tok, err := s.token(ctx, a.AccountID, false)
	if err != nil {
		if isTokenGone(err) {
			s.setGrantState(ctx, g, contracts.GrantRevoked, tokenRemoved)
			return probeResult{obs: s.failObs(ctx, a, contracts.EligibleUnusable, tokenRemoved, true)}
		}
		msg := "token unavailable: " + contracts.AsError(err).Message
		return probeResult{obs: s.failObs(ctx, a, contracts.EligibleUnknown, msg, false), failed: true, errText: msg}
	}
	url := s.cfg.ClaudeMessagesURL
	if url == "" {
		url = "https://api.anthropic.com/v1/messages"
	}
	body := []byte(`{"model":"` + TokenProbeModel + `","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return probeResult{failed: true, errText: "bad messages URL"}
	}
	req.Header.Set("Authorization", "Bearer "+tok.reveal())
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	req.Header.Set("content-type", "application/json")
	req.Header.Set("User-Agent", s.cfg.ClaudeUserAgent)
	resp, err := s.cfg.HTTPClient.Do(req)
	if err != nil {
		msg := "usage probe failed: " + redact(err.Error())
		return probeResult{obs: s.failObs(ctx, a, contracts.EligibleUnknown, msg, false), failed: true, errText: msg}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	u := ParseUnifiedHeaders(resp.Header)
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		// A token never refreshes: the rejection is final until a new one.
		if c, ok := s.tok.get(a.AccountID); ok {
			s.tok.reject(a.AccountID, c.updated)
		} else {
			s.tok.reject(a.AccountID, time.Time{})
		}
		s.tok.drop(a.AccountID)
		s.markDead(ctx, g, fmt.Sprintf("%s (HTTP %d)", spec.RejectHint, resp.StatusCode))
		return probeResult{obs: s.failObs(ctx, a, contracts.EligibleUnusable, spec.RejectHint, true)}
	case resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusTooManyRequests && len(u.Windows) > 0:
		o := s.goodObs(a, u.Windows, "")
		if resp.StatusCode == http.StatusTooManyRequests || u.Rejected {
			o.Eligibility, o.Confidence = contracts.EligibleExhausted, "high"
			o.Detail = "limit reached (" + orNone(u.Claim) + ")"
			if r := u.Reset; r != nil && (o.ResetAt == nil || r.After(*o.ResetAt)) {
				o.ResetAt = r
			}
			if o.ResetAt == nil {
				t := s.now().Add(retryAfter(resp.Header.Get("Retry-After"), s.now()))
				o.ResetAt = &t
			}
		}
		return probeResult{obs: o}
	case resp.StatusCode == http.StatusTooManyRequests:
		ra := retryAfter(resp.Header.Get("Retry-After"), s.now())
		t := s.now().Add(ra)
		o := s.failObs(ctx, a, contracts.EligibleExhausted, "rate limited (HTTP 429); at limit until reset", true)
		o.ResetAt, o.Confidence = &t, "medium"
		return probeResult{obs: o, retryAfter: ra}
	default:
		msg := fmt.Sprintf("usage probe HTTP %d", resp.StatusCode)
		return probeResult{obs: s.failObs(ctx, a, contracts.EligibleUnknown, msg, false), failed: true, errText: msg}
	}
}

func orNone(s string) string {
	if s == "" {
		return "unified limit"
	}
	return s
}

// UnifiedLimits is what the anthropic-ratelimit-unified-* headers say.
type UnifiedLimits struct {
	Windows []contracts.UsageWindow
	// Status is anthropic-ratelimit-unified-status (allowed,
	// allowed_warning, rejected); Rejected when it or any window says
	// rejected.
	Status   string
	Rejected bool
	// Reset is anthropic-ratelimit-unified-reset (the binding window's).
	Reset *time.Time
	// Claim is the representative claim (e.g. five_hour).
	Claim string
}

const unifiedPrefix = "anthropic-ratelimit-unified-"

// unifiedName maps a header claim to the observation window name.
func unifiedName(claim string) string {
	switch claim {
	case "5h":
		return usage.FiveHour
	case "7d":
		return usage.Weekly
	case "overage":
		return usage.Extra
	}
	if rest, ok := strings.CutPrefix(claim, "7d_"); ok {
		return usage.Weekly + "_" + slug(rest)
	}
	return slug(claim)
}

// ParseUnifiedHeaders reads the unified rate-limit headers: every
// <claim>-utilization (0..1) becomes a window (5h → five_hour, 7d →
// seven_day, overage → extra_usage, shown only when used) with its
// <claim>-reset (unix seconds).
func ParseUnifiedHeaders(h http.Header) UnifiedLimits {
	var out UnifiedLimits
	vals := map[string]string{}
	for k, v := range h {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, unifiedPrefix) && len(v) > 0 {
			vals[strings.TrimPrefix(lk, unifiedPrefix)] = strings.TrimSpace(v[0])
		}
	}
	unix := func(s string) *time.Time {
		n, err := strconv.ParseFloat(s, 64)
		if err != nil || n <= 0 {
			return nil
		}
		t := time.Unix(int64(n), 0).UTC()
		return &t
	}
	out.Status, out.Claim = vals["status"], vals["representative-claim"]
	out.Rejected = out.Status == "rejected"
	out.Reset = unix(vals["reset"])
	var claims []string
	for k := range vals {
		if c, ok := strings.CutSuffix(k, "-utilization"); ok {
			claims = append(claims, c)
		}
	}
	sort.Strings(claims)
	for _, c := range claims {
		f, err := strconv.ParseFloat(vals[c+"-utilization"], 64)
		if err != nil {
			continue
		}
		name := unifiedName(c)
		if name == usage.Extra && f <= 0 {
			continue // no extra usage spent: nothing to show
		}
		out.Windows = append(out.Windows, contracts.UsageWindow{Name: name, UsedPercent: clampPct(f * 100), ResetAt: unix(vals[c+"-reset"])})
		if vals[c+"-status"] == "rejected" && name != usage.Extra {
			out.Rejected = true
		}
	}
	sort.SliceStable(out.Windows, func(i, j int) bool { return windowOrder(out.Windows[i].Name) < windowOrder(out.Windows[j].Name) })
	return out
}

// tokenEnv adds the token variable to a token run's environment and keeps
// it out of the unset list.
func (s *Service) tokenEnv(ctx context.Context, g grantRow, res *Resolution) error {
	spec := tokenSpecs[g.Harness]
	if spec.Env == "" {
		return contracts.Errorf(contracts.ErrUnsupported, "harness %s has no token accounts", g.Harness)
	}
	tok, err := s.token(ctx, g.AccountID, false)
	if err != nil {
		return err
	}
	res.Env[spec.Env] = tok.reveal()
	out := res.Unset[:0:0]
	for _, k := range res.Unset {
		if k != spec.Env {
			out = append(out, k)
		}
	}
	res.Unset = out
	return nil
}

// personKeys are the names an account goes by, for "prefer the token
// account of the same person": its label, the local part of its rota
// name, and its provider identity.
func personKeys(a contracts.Account) []string {
	var ks []string
	add := func(k string) {
		if k = strings.ToLower(strings.TrimSpace(k)); k != "" {
			ks = append(ks, k)
		}
	}
	add(a.Label)
	if a.RotaRef != "" {
		local, _, _ := strings.Cut(a.RotaRef, "@")
		add(local)
	}
	if a.ProviderRef != "" {
		add("ref:" + a.ProviderRef)
	}
	return ks
}

// preferTokens drops non-token candidates that belong to the same person
// as an eligible token candidate (same subscription, two credentials):
// the token runs everywhere at once, the rota login does not.
func preferTokens(eligible []*candidate, lists ...[]*candidate) [][]*candidate {
	people := map[string]bool{}
	for _, c := range eligible {
		if c.grant.Kind == contracts.CredOAuthToken {
			for _, k := range personKeys(c.acct) {
				people[k] = true
			}
		}
	}
	out := make([][]*candidate, len(lists))
	for i, l := range lists {
		if len(people) == 0 {
			out[i] = l
			continue
		}
		for _, c := range l {
			same := false
			if c.grant.Kind != contracts.CredOAuthToken {
				for _, k := range personKeys(c.acct) {
					same = same || people[k]
				}
			}
			if !same {
				out[i] = append(out[i], c)
			}
		}
	}
	return out
}
