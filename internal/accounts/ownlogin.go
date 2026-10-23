package accounts

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// The machine's own (ambient) login: Pyre identifies it read-only so the
// owner sees "alice · own login · linux-box, mac" counted as ready instead
// of "openai 0 ready" when Codex runs fine on each machine's own login.
// Nothing here writes the harness files, refreshes, or probes: usage comes
// from the rate-limit updates Pyre's own runs on that login already receive.

// OwnLogin returns the nonsecret identity of this machine's own login for
// h (nil when there is none or it is unreadable). Read-only; tokens are
// decoded locally for their identity claims and never kept or logged.
func (s *Service) OwnLogin(h contracts.Harness) *contracts.HarnessLogin {
	switch h {
	case contracts.HarnessCodex:
		return codexOwnLogin(s.cfg.UserCodexDir)
	case contracts.HarnessClaude:
		p := s.cfg.Rota.LiveClaudeJSON
		if p == "" && s.cfg.UserClaudeDir != "" {
			p = filepath.Join(filepath.Dir(s.cfg.UserClaudeDir), ".claude.json")
		}
		return claudeOwnLogin(p)
	case contracts.HarnessAgy:
		return agyOwnLogin(s.cfg.UserAgyDir)
	}
	return nil
}

// agyOwnLogin reads the identity of agy's sign-in: the email claim of the
// id_token in <dir>/antigravity-oauth-token (decoded locally; the access and
// refresh tokens are never used, kept or logged).
func agyOwnLogin(dir string) *contracts.HarnessLogin {
	if dir == "" {
		return nil
	}
	b, err := os.ReadFile(filepath.Join(dir, "antigravity-oauth-token"))
	if err != nil {
		return nil
	}
	var t struct {
		IDToken string `json:"id_token"`
	}
	if json.Unmarshal(b, &t) != nil || t.IDToken == "" {
		return nil
	}
	c := jwtClaims(t.IDToken)
	if c == nil {
		return nil
	}
	l := &contracts.HarnessLogin{}
	l.Email, _ = c["email"].(string)
	l.ProviderRef, _ = c["sub"].(string)
	if l.Email == "" && l.ProviderRef == "" {
		return nil
	}
	return l
}

func codexOwnLogin(home string) *contracts.HarnessLogin {
	if home == "" {
		return nil
	}
	b, err := os.ReadFile(filepath.Join(home, "auth.json"))
	if err != nil {
		return nil
	}
	var a codexAuth
	if json.Unmarshal(b, &a) != nil || a.Tokens == nil {
		return nil
	}
	l := &contracts.HarnessLogin{ProviderRef: a.Tokens.AccountID}
	if c := jwtClaims(a.Tokens.IDToken); c != nil {
		l.Email, _ = c["email"].(string)
		if auth, ok := c["https://api.openai.com/auth"].(map[string]any); ok {
			if l.ProviderRef == "" {
				l.ProviderRef, _ = auth["chatgpt_account_id"].(string)
			}
			l.Plan, _ = auth["chatgpt_plan_type"].(string)
		}
	}
	if l.Email == "" && l.ProviderRef == "" {
		return nil
	}
	return l
}

func claudeOwnLogin(path string) *contracts.HarnessLogin {
	if path == "" {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var c struct {
		OAuthAccount *struct {
			AccountUUID  string `json:"accountUuid"`
			EmailAddress string `json:"emailAddress"`
		} `json:"oauthAccount"`
	}
	if json.Unmarshal(b, &c) != nil || c.OAuthAccount == nil {
		return nil
	}
	if c.OAuthAccount.EmailAddress == "" && c.OAuthAccount.AccountUUID == "" {
		return nil
	}
	return &contracts.HarnessLogin{Email: c.OAuthAccount.EmailAddress, ProviderRef: c.OAuthAccount.AccountUUID}
}

// MatchesOwnLogin reports whether account a is the identity l (same
// provider assumed): rota_ref equal to the email, or the provider ref.
func MatchesOwnLogin(a contracts.Account, l *contracts.HarnessLogin) bool {
	if l == nil {
		return false
	}
	if l.Email != "" && strings.EqualFold(a.RotaRef, l.Email) {
		return true
	}
	return l.ProviderRef != "" && a.ProviderRef == l.ProviderRef
}

// ownObsInterval bounds own-login observations (a rate-limit update arrives
// with every turn).
const ownObsInterval = time.Minute

type ownObsState struct {
	mu   sync.Mutex
	last map[contracts.Harness]time.Time
}

// ObserveOwnRateLimits records a rate-limit update that a Pyre run on the
// machine's own Codex login received (codex adapter, account/rateLimits/
// updated on the ambient app-server) as an observation of the hub account
// that login matches, reported to the hub for this machine. At most once a
// minute; no probe, nothing written to the harness's files.
func (s *Service) ObserveOwnRateLimits(ctx context.Context, h contracts.Harness, raw json.RawMessage) {
	if s.cfg.Fleet == nil {
		return
	}
	now := s.now()
	s.own.mu.Lock()
	if s.own.last == nil {
		s.own.last = map[contracts.Harness]time.Time{}
	}
	if now.Sub(s.own.last[h]) < ownObsInterval {
		s.own.mu.Unlock()
		return
	}
	s.own.last[h] = now
	s.own.mu.Unlock()
	login := s.OwnLogin(h)
	if login == nil {
		return
	}
	rl, err := ParseCodexRateLimits(raw)
	if err != nil || len(rl.Windows) == 0 {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, defaultFleetTimeout)
	defer cancel()
	page, err := s.cfg.Fleet.ListAccounts(cctx, contracts.ListAccountsRequest{Provider: contracts.ProviderFor(h)})
	if err != nil {
		return
	}
	for _, v := range page.Items {
		if v.Account.Disabled || !MatchesOwnLogin(v.Account, login) {
			continue
		}
		o := s.goodObs(v.Account, rl.Windows, "")
		o.Source = "own-login (Pyre runs)"
		if rl.Reached {
			o.Eligibility, o.Confidence = contracts.EligibleExhausted, "high"
			o.Detail = "codex limit reached: " + truncate(rl.Reason, 200)
		}
		s.report(ctx, contracts.ReportAccountObs, *o)
		return
	}
}
