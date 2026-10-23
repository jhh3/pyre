package accounts

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/codex/codexschema"
	"github.com/jhh3/pyre/internal/usage"
)

// Usage observation is bounded: one probe at a time (Service.obmu), each
// account at most once per PollInterval (≥5 min outside tests) plus jitter,
// exponential back-off on errors, and Retry-After honored. Tokens are read
// from the PROFILE store only and are never refreshed: an expired access
// token yields an unknown observation and no request.

const probeExpiryMargin = 60 * time.Second

// RunObserver polls due accounts (and agy's own quota) until ctx ends.
func (s *Service) RunObserver(ctx context.Context) {
	tick := time.NewTicker(30 * time.Second)
	if s.cfg.TestMode && s.cfg.PollInterval < time.Minute {
		tick.Reset(s.cfg.PollInterval / 2)
	}
	defer tick.Stop()
	go s.RunRotaSync(ctx) // no-op unless rota checkouts are enabled
	for {
		s.ObserveDue(ctx, "")
		s.kickAgyUsage(ctx) // agy's own quota in the background, at most every 5 min (agyusage.go)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// ObserveDue probes every account of provider ("" = all) whose schedule is
// due, sequentially, and returns the latest observation of each account.
func (s *Service) ObserveDue(ctx context.Context, provider contracts.Provider) []contracts.AccountObservation {
	return s.observeDue(ctx, provider, false)
}

// ObserveNow is ObserveDue on demand (`pyre accounts --refresh`): token
// accounts are probed even when idle (at most once a minute).
func (s *Service) ObserveNow(ctx context.Context, provider contracts.Provider) []contracts.AccountObservation {
	return s.observeDue(ctx, provider, true)
}

func (s *Service) observeDue(ctx context.Context, provider contracts.Provider, force bool) []contracts.AccountObservation {
	s.obmu.Lock()
	defer s.obmu.Unlock()
	accts, err := listAccounts(ctx, s.db, provider)
	if err != nil {
		s.log.Warn("accounts: observe list", "err", err)
		return nil
	}
	var out []contracts.AccountObservation
	for _, a := range accts {
		if ctx.Err() != nil {
			break
		}
		if a.Disabled {
			continue
		}
		if g, err := s.getGrant(ctx, s.db, a.AccountID, harnessForProvider(a.Provider)); err == nil && g.Kind == contracts.CredOAuthToken {
			// Token accounts: probed while in use, never on a fixed poll (token.go).
			if s.tokenProbeDue(ctx, a, g, force) {
				s.observeToken(ctx, a, g)
				if _, ok := s.maybeRevert(ctx, g.GrantID); ok { // tokenmerge.go
					s.pollAccount(ctx, a, pollState{}) // observe the fallback login now
				}
			}
			if o, _ := getObservation(ctx, s.db, a.AccountID); o != nil {
				out = append(out, *o)
			}
			continue
		}
		ps, err := getPoll(ctx, s.db, a.AccountID)
		if err != nil {
			continue
		}
		// A passed reset makes the account due at once (once per reset),
		// unless the last probe failed and is backing off.
		if !ps.NextAt.IsZero() && s.now().Before(ps.NextAt) && (ps.Failures > 0 || !s.resetPassedSinceObserved(ctx, a.AccountID)) {
			if o, _ := getObservation(ctx, s.db, a.AccountID); o != nil {
				out = append(out, *o)
			}
			continue
		}
		s.pollAccount(ctx, a, ps)
		if o, _ := getObservation(ctx, s.db, a.AccountID); o != nil {
			out = append(out, *o)
		}
	}
	return out
}

// IngestObservation stores an observation relayed from another node (usage
// is provider-wide; keep the newest sample, never sum them).
func (s *Service) IngestObservation(ctx context.Context, o contracts.AccountObservation) error {
	if o.AccountID == "" {
		return contracts.Errorf(contracts.ErrInvalid, "account_id required")
	}
	return putObservation(ctx, s.db, o)
}

// Observations returns the latest stored observation for each account.
func (s *Service) Observations(ctx context.Context, provider contracts.Provider) ([]contracts.AccountObservation, error) {
	accts, err := listAccounts(ctx, s.db, provider)
	if err != nil {
		return nil, err
	}
	var out []contracts.AccountObservation
	for _, a := range accts {
		o, err := getObservation(ctx, s.db, a.AccountID)
		if err != nil {
			return nil, err
		}
		if o != nil {
			out = append(out, *o)
		}
	}
	return out, nil
}

// probeResult is the outcome of one probe.
type probeResult struct {
	obs        *contracts.AccountObservation // nil = keep the previous observation
	retryAfter time.Duration
	failed     bool
	errText    string
}

func (s *Service) pollAccount(ctx context.Context, a contracts.Account, ps pollState) {
	h := harnessForProvider(a.Provider)
	g, err := s.getGrant(ctx, s.db, a.AccountID, h)
	next := s.cfg.PollInterval + s.jitter(s.cfg.PollInterval/5)
	if err != nil {
		// No profile on this node: nothing to observe here.
		_ = putPoll(ctx, s.db, a.AccountID, pollState{NextAt: s.now().Add(next)})
		return
	}
	if g.Kind == contracts.CredOAuthToken {
		s.observeToken(ctx, a, g)     // token.go: its own schedule
		s.maybeRevert(ctx, g.GrantID) // rejected/removed: back on the fallback login (tokenmerge.go)
		_ = putPoll(ctx, s.db, a.AccountID, pollState{NextAt: s.now().Add(next)})
		return
	}
	var r probeResult
	switch g.State {
	case contracts.GrantDead, contracts.GrantRevoked, contracts.GrantExpired:
		// No retries against a dead login; an intervention is already open.
		r = probeResult{obs: s.failObs(ctx, a, contracts.EligibleUnusable, "grant "+string(g.State)+"; re-login required; not probing", true)}
	default:
		switch h {
		case contracts.HarnessClaude:
			r = s.probeClaude(ctx, a, g)
		case contracts.HarnessCodex:
			r = s.probeCodex(ctx, a, g)
		}
	}
	if r.obs != nil {
		if err := putObservation(ctx, s.db, *r.obs); err == nil {
			s.report(ctx, contracts.ReportAccountObs, *r.obs)
		}
	}
	np := pollState{}
	if r.failed {
		np.Failures = ps.Failures + 1
		np.LastError = r.errText
		back := time.Duration(float64(s.cfg.PollInterval) * math.Pow(2, float64(np.Failures)))
		if back > s.cfg.MaxBackoff || back <= 0 {
			back = s.cfg.MaxBackoff
		}
		if r.retryAfter > back {
			back = r.retryAfter
		}
		next = back + s.jitter(s.cfg.PollInterval/5)
	}
	np.NextAt = s.now().Add(next)
	_ = putPoll(ctx, s.db, a.AccountID, np)
}

// failObs builds an unknown/unusable observation, keeping the last good
// windows for display (marked stale).
func (s *Service) failObs(ctx context.Context, a contracts.Account, el contracts.Eligibility, detail string, replace bool) *contracts.AccountObservation {
	prev, _ := getObservation(ctx, s.db, a.AccountID)
	o := contracts.AccountObservation{
		AccountID: a.AccountID, Provider: a.Provider, ReportingNode: s.cfg.NodeID, ObservedAt: s.now(),
		Source: sourceFor(a.Provider), Confidence: "low", Eligibility: el, Stale: true, Detail: redact(detail),
	}
	if prev != nil {
		if !replace && prev.Eligibility == contracts.EligibleYes && !prev.Stale && s.now().Sub(prev.ObservedAt) <= s.cfg.FreshFor {
			return nil // a recent good sample stays until it ages out
		}
		o.Windows = prev.Windows
	}
	return &o
}

func sourceFor(p contracts.Provider) string {
	if p == contracts.ProviderOpenAI {
		return "codex:app-server"
	}
	return "claude:usage-endpoint"
}

func (s *Service) goodObs(a contracts.Account, ws []contracts.UsageWindow, detail string) *contracts.AccountObservation {
	o := contracts.AccountObservation{
		AccountID: a.AccountID, Provider: a.Provider, ReportingNode: s.cfg.NodeID, ObservedAt: s.now(),
		Source: sourceFor(a.Provider), Confidence: "high", Eligibility: contracts.EligibleYes, Windows: ws, Detail: detail,
	}
	var reset *time.Time
	for _, w := range ws {
		// The extra-usage budget at 100% leaves the plan windows usable.
		if usage.IsLimit(w.Name) && w.UsedPercent >= s.cfg.ExhaustedPercent {
			o.Eligibility = contracts.EligibleExhausted
			if w.ResetAt != nil && (reset == nil || w.ResetAt.After(*reset)) {
				t := *w.ResetAt
				reset = &t
			}
		}
	}
	o.ResetAt = reset
	if len(ws) == 0 {
		o.Eligibility, o.Confidence, o.Detail = contracts.EligibleUnknown, "low", "usage response had no windows"
	}
	return &o
}

// probeClaude calls GET <ClaudeUsageURL> with the profile's access token.
func (s *Service) probeClaude(ctx context.Context, a contracts.Account, g grantRow) probeResult {
	var si storeInfo
	if g.Kind == CredRotaCheckout && !s.checkedOut(ctx, g.GrantID) {
		var why string
		if si, why = s.rotaSnapshotToken(g, a); why != "" {
			return probeResult{obs: s.failObs(ctx, a, contracts.EligibleUnknown, why, true)}
		}
	} else {
		si = s.readClaudeStore(ctx, g)
	}
	switch {
	case si.Locked != "":
		return probeResult{obs: s.failObs(ctx, a, contracts.EligibleUnknown, si.Locked, true), failed: true, errText: "keychain unavailable"}
	case !si.Present || si.AccessToken.empty():
		return probeResult{obs: s.failObs(ctx, a, contracts.EligibleUnknown, "no login in profile store", true)}
	case !si.ExpiresAt.IsZero() && !si.ExpiresAt.After(s.now().Add(probeExpiryMargin)):
		return probeResult{obs: s.failObs(ctx, a, contracts.EligibleUnknown,
			"access token expired; usage probe never refreshes (the harness refreshes on its next use)", true)}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.cfg.ClaudeUsageURL, nil)
	if err != nil {
		return probeResult{failed: true, errText: "bad usage URL"}
	}
	req.Header.Set("Authorization", "Bearer "+si.AccessToken.reveal())
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", s.cfg.ClaudeUserAgent)
	resp, err := s.cfg.HTTPClient.Do(req)
	if err != nil {
		msg := "usage request failed: " + redact(err.Error())
		return probeResult{obs: s.failObs(ctx, a, contracts.EligibleUnknown, msg, false), failed: true, errText: msg}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == http.StatusOK:
		ws, err := ParseClaudeUsage(body)
		if err != nil {
			return probeResult{obs: s.failObs(ctx, a, contracts.EligibleUnknown, "unparseable usage response", false), failed: true, errText: "unparseable usage response"}
		}
		return probeResult{obs: s.goodObs(a, ws, "")}
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		if g.Kind == contracts.CredClaudeTokenFD {
			// Setup tokens never refresh: a rejection is final.
			s.markDead(ctx, g, fmt.Sprintf("setup token rejected (HTTP %d); re-login required", resp.StatusCode))
			return probeResult{obs: s.failObs(ctx, a, contracts.EligibleUnusable, "setup token rejected; re-login required", true)}
		}
		return probeResult{obs: s.failObs(ctx, a, contracts.EligibleUnknown,
			fmt.Sprintf("access token rejected (HTTP %d); not refreshing (harness-owned)", resp.StatusCode), true),
			failed: true, errText: fmt.Sprintf("HTTP %d", resp.StatusCode)}
	case resp.StatusCode == http.StatusTooManyRequests:
		ra := retryAfter(resp.Header.Get("Retry-After"), s.now())
		msg := fmt.Sprintf("usage endpoint rate-limited (retry after %s)", ra.Round(time.Second))
		return probeResult{obs: s.failObs(ctx, a, contracts.EligibleUnknown, msg, false), failed: true, retryAfter: ra, errText: msg}
	default:
		msg := fmt.Sprintf("usage endpoint HTTP %d", resp.StatusCode)
		return probeResult{obs: s.failObs(ctx, a, contracts.EligibleUnknown, msg, false), failed: true, errText: msg}
	}
}

// probeCodex reads rate limits through the Codex adapter's app-server for
// the profile home, holding the shared app-server lease for the call.
func (s *Service) probeCodex(ctx context.Context, a contracts.Account, g grantRow) probeResult {
	if g.Kind == CredRotaCheckout && !s.checkedOut(ctx, g.GrantID) {
		if s.holdNeedsReauth(ctx, a, g) {
			return probeResult{} // reauth.go
		}
		_, why := s.rotaSnapshotToken(g, a)
		if prev, _ := getObservation(ctx, s.db, a.AccountID); prev != nil && prev.ResetAt != nil && prev.ResetAt.After(s.now()) &&
			(prev.Eligibility == contracts.EligibleExhausted || prev.Eligibility == contracts.EligibleCooldown) {
			// Not observable while checked in: keep the last limit and its
			// reset (from a limit event or a checkout verification) so the
			// account is not checked out again before that reset.
			return probeResult{}
		}
		return probeResult{obs: s.failObs(ctx, a, contracts.EligibleUnknown, why, true)}
	}
	si, _ := readCodexStore(g.ProfileDir)
	switch {
	case !si.Present:
		return probeResult{obs: s.failObs(ctx, a, contracts.EligibleUnknown, "no ChatGPT login in profile auth.json", true)}
	case !si.ExpiresAt.IsZero() && !si.ExpiresAt.After(s.now().Add(probeExpiryMargin)):
		return probeResult{obs: s.failObs(ctx, a, contracts.EligibleUnknown,
			"access token expired; usage probe never refreshes (the harness refreshes on its next use)", true)}
	case s.cfg.CodexReader == nil:
		return probeResult{obs: s.failObs(ctx, a, contracts.EligibleUnknown, "codex adapter rate-limit reader unavailable", true)}
	}
	holder := s.cfg.CodexLeaseHolder
	if err := acquireLease(ctx, s.db, g.GrantID, holder, s.now()); err != nil {
		msg := "codex home in use by another process holder; skipped (one process per home)"
		return probeResult{obs: s.failObs(ctx, a, contracts.EligibleUnknown, msg, false), failed: true, errText: msg}
	}
	defer func() { _ = releaseLease(context.WithoutCancel(ctx), s.db, g.GrantID, holder) }()
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	raw, err := s.cfg.CodexReader.ReadRateLimits(cctx, g.ProfileDir)
	if err != nil {
		msg := "account/rateLimits/read failed: " + truncate(redact(err.Error()), 200)
		if authRejected(msg) {
			return probeResult{obs: s.markNeedsReauth(ctx, a, msg), failed: true, errText: msg}
		}
		return probeResult{obs: s.failObs(ctx, a, contracts.EligibleUnknown, msg, false), failed: true, errText: msg}
	}
	rl, err := ParseCodexRateLimits(raw)
	if err != nil {
		return probeResult{obs: s.failObs(ctx, a, contracts.EligibleUnknown, "unparseable rate-limit response", false), failed: true, errText: "unparseable"}
	}
	o := s.goodObs(a, rl.Windows, "")
	if rl.Reached {
		// The backend says usage is blocked even if no window shows 100%.
		o.Eligibility, o.Confidence = contracts.EligibleExhausted, "high"
		o.Detail = "codex limit reached: " + truncate(rl.Reason, 200)
		if rl.ResetAt != nil && (o.ResetAt == nil || rl.ResetAt.After(*o.ResetAt)) {
			t := *rl.ResetAt
			o.ResetAt = &t
		}
	}
	return probeResult{obs: o}
}

func retryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Minute
	}
	if n, err := strconv.ParseFloat(v, 64); err == nil && n >= 0 {
		return time.Duration(n * float64(time.Second))
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(now); d > 0 {
			return d
		}
	}
	return time.Minute
}

func parseTimeLoose(v any) *time.Time {
	switch x := v.(type) {
	case string:
		if x == "" {
			return nil
		}
		if t, err := time.Parse(time.RFC3339Nano, x); err == nil {
			t = t.UTC()
			return &t
		}
		if n, err := strconv.ParseFloat(x, 64); err == nil {
			return parseTimeLoose(n)
		}
	case float64:
		if x <= 0 {
			return nil
		}
		if x > 1e12 { // milliseconds
			x /= 1000
		}
		t := time.Unix(int64(x), 0).UTC()
		return &t
	case json.Number:
		f, err := x.Float64()
		if err == nil {
			return parseTimeLoose(f)
		}
	}
	return nil
}

// ParseClaudeUsage parses a /api/oauth/usage response (or a rota cache
// "data" object, same format) into windows: every top-level object carrying
// "utilization" (five_hour, seven_day, seven_day_<model>...) and the newer
// "limits" array (session→five_hour, weekly_all→seven_day,
// weekly_scoped→seven_day_<model>). Unknown fields are ignored.
func ParseClaudeUsage(b []byte) ([]contracts.UsageWindow, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(b, &top); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var ws []contracts.UsageWindow
	if raw, ok := top["limits"]; ok {
		var limits []struct {
			Kind     string   `json:"kind"`
			Percent  *float64 `json:"percent"`
			ResetsAt any      `json:"resets_at"`
			Scope    struct {
				Model struct {
					DisplayName string `json:"display_name"`
				} `json:"model"`
			} `json:"scope"`
		}
		if json.Unmarshal(raw, &limits) == nil {
			for _, l := range limits {
				if l.Percent == nil {
					continue
				}
				name := l.Kind
				switch l.Kind {
				case "session":
					name = "five_hour"
				case "weekly_all":
					name = "seven_day"
				case "weekly_scoped":
					name = "seven_day_" + slug(l.Scope.Model.DisplayName)
				}
				if name == "" || seen[name] {
					continue
				}
				seen[name] = true
				ws = append(ws, contracts.UsageWindow{Name: name, UsedPercent: clampPct(*l.Percent), ResetAt: parseTimeLoose(l.ResetsAt)})
			}
		}
	}
	keys := make([]string, 0, len(top))
	for k := range top {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if seen[k] || k == "limits" {
			continue
		}
		var blk struct {
			Utilization *float64 `json:"utilization"`
			ResetsAt    any      `json:"resets_at"`
		}
		if json.Unmarshal(top[k], &blk) != nil || blk.Utilization == nil {
			continue
		}
		seen[k] = true
		ws = append(ws, contracts.UsageWindow{Name: k, UsedPercent: clampPct(*blk.Utilization), ResetAt: parseTimeLoose(blk.ResetsAt)})
	}
	sort.SliceStable(ws, func(i, j int) bool { return windowOrder(ws[i].Name) < windowOrder(ws[j].Name) })
	return ws, nil
}

func windowOrder(n string) string {
	switch n {
	case "five_hour":
		return "0"
	case "seven_day":
		return "1"
	}
	return "2" + n
}

func slug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
		case b.Len() > 0 && !strings.HasSuffix(b.String(), "_"):
			b.WriteByte('_')
		}
	}
	out := strings.TrimSuffix(b.String(), "_")
	if out == "" {
		return "scoped"
	}
	return out
}

func clampPct(f float64) float64 {
	if f < 0 {
		return 0
	}
	if f > 100 {
		return 100
	}
	return f
}

// The structs below decode `account/rateLimits/read` (GetAccountRateLimitsResponse
// / RateLimitSnapshot / RateLimitWindow in the Codex app-server schema). Every
// JSON field they name is checked against the installed Codex's generated
// schema by the drift guard (CodexSchemaRequirements; tools/codexschema and
// internal/runtime/codex TestCodexSchemaDrift), so a renamed field fails
// loudly instead of silently decoding as zero.

type codexWindow struct {
	UsedPercent        *float64 `json:"usedPercent"`
	WindowDurationMins *float64 `json:"windowDurationMins"`
	ResetsAt           any      `json:"resetsAt"`
}

type codexSnapshot struct {
	LimitID   string       `json:"limitId"`
	LimitName string       `json:"limitName"`
	Primary   *codexWindow `json:"primary"`
	Secondary *codexWindow `json:"secondary"`
	// RateLimitReachedType is non-null while this bucket is blocked
	// (Codex 0.160.0: rate_limit_reached, workspace_*_credits_depleted,
	// workspace_*_usage_limit_reached). Earlier Pyre code decoded a
	// non-existent "limitReached" bool, so the limit was never seen.
	RateLimitReachedType *string `json:"rateLimitReachedType"`
	// SpendControlReached: backend spend control blocks usage (null = unknown).
	SpendControlReached *bool `json:"spendControlReached"`
}

type codexRateLimitsResponse struct {
	RateLimits *codexSnapshot           `json:"rateLimits"`
	ByID       map[string]codexSnapshot `json:"rateLimitsByLimitId"`
	// OrdinaryUsageAllowed false means the backend denies ordinary included
	// usage for the account; null means unavailable (never inferred).
	OrdinaryUsageAllowed *bool `json:"ordinaryUsageAllowed"`
}

// CodexRateLimits is the parsed form of an account/rateLimits/read result.
type CodexRateLimits struct {
	Windows []contracts.UsageWindow
	// Reached reports that the backend says usage is currently blocked;
	// Reason names the signal(s) (bucket and reached type), never a token.
	Reached bool
	Reason  string
	// ResetAt is the latest reset among the windows of blocked buckets.
	ResetAt *time.Time
}

// CodexSchemaBindings maps each decode struct above to the generated Codex
// app-server definition it decodes. Used by the schema drift guard.
func CodexSchemaBindings() map[string][]string {
	return map[string][]string{
		"GetAccountRateLimitsResponse": jsonFieldNames(codexRateLimitsResponse{}),
		"RateLimitSnapshot":            jsonFieldNames(codexSnapshot{}),
		"RateLimitWindow":              jsonFieldNames(codexWindow{}),
	}
}

// CodexSchemaRequirements is CodexSchemaBindings in drift-guard form.
func CodexSchemaRequirements() []codexschema.Req {
	b := CodexSchemaBindings()
	defs := make([]string, 0, len(b))
	for d := range b {
		defs = append(defs, d)
	}
	sort.Strings(defs)
	reqs := make([]codexschema.Req, 0, len(defs))
	for _, d := range defs {
		reqs = append(reqs, codexschema.Req{Type: d, Fields: b[d], Why: "internal/accounts ParseCodexRateLimits"})
	}
	return reqs
}

func jsonFieldNames(v any) []string {
	t := reflect.TypeOf(v)
	var out []string
	for i := 0; i < t.NumField(); i++ {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		if name != "" && name != "-" {
			out = append(out, name)
		}
	}
	return out
}

// ParseCodexRateLimits parses an app-server account/rateLimits/read result
// ({rateLimits: {...}, rateLimitsByLimitId: {...}}) into windows. A 300-min
// window is five_hour, 10080-min is seven_day; other buckets are suffixed by
// their limit name. Reached is set when any bucket carries a
// rateLimitReachedType or spendControlReached=true, or the response says
// ordinaryUsageAllowed=false.
func ParseCodexRateLimits(raw []byte) (CodexRateLimits, error) {
	var r codexRateLimitsResponse
	if err := json.Unmarshal(raw, &r); err != nil {
		return CodexRateLimits{}, err
	}
	type kv struct {
		id string
		s  codexSnapshot
	}
	var snaps []kv
	if len(r.ByID) > 0 {
		for id, sn := range r.ByID {
			snaps = append(snaps, kv{id, sn})
		}
		sort.Slice(snaps, func(i, j int) bool {
			if (snaps[i].id == "codex") != (snaps[j].id == "codex") {
				return snaps[i].id == "codex"
			}
			return snaps[i].id < snaps[j].id
		})
	} else if r.RateLimits != nil {
		id := r.RateLimits.LimitID
		if id == "" {
			id = "codex"
		}
		snaps = append(snaps, kv{id, *r.RateLimits})
	}
	var out CodexRateLimits
	var reasons []string
	if r.OrdinaryUsageAllowed != nil && !*r.OrdinaryUsageAllowed {
		out.Reached = true
		reasons = append(reasons, "ordinaryUsageAllowed=false")
	}
	for _, sn := range snaps {
		blocked := false
		if t := sn.s.RateLimitReachedType; t != nil && *t != "" {
			blocked = true
			reasons = append(reasons, sn.id+":"+*t)
		}
		if sn.s.SpendControlReached != nil && *sn.s.SpendControlReached {
			blocked = true
			reasons = append(reasons, sn.id+":spend_control_reached")
		}
		out.Reached = out.Reached || blocked
		scope := ""
		if sn.id != "codex" {
			scope = sn.s.LimitName
			if scope == "" {
				scope = strings.TrimPrefix(sn.id, "codex_")
			}
		}
		for i, w := range []*codexWindow{sn.s.Primary, sn.s.Secondary} {
			if w == nil || w.UsedPercent == nil {
				continue
			}
			name := []string{"primary", "secondary"}[i]
			if w.WindowDurationMins != nil {
				switch m := int(*w.WindowDurationMins); m {
				case 300:
					name = "five_hour"
				case 10080:
					name = "seven_day"
				default:
					name = fmt.Sprintf("window_%dm", m)
				}
			}
			if scope != "" {
				name += "_" + slug(scope)
			}
			uw := contracts.UsageWindow{Name: name, UsedPercent: clampPct(*w.UsedPercent), ResetAt: parseTimeLoose(w.ResetsAt)}
			if blocked && uw.ResetAt != nil && (out.ResetAt == nil || uw.ResetAt.After(*out.ResetAt)) {
				t := *uw.ResetAt
				out.ResetAt = &t
			}
			out.Windows = append(out.Windows, uw)
		}
	}
	out.Reason = strings.Join(reasons, ", ")
	return out, nil
}
