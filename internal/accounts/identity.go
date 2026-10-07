package accounts

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// storeInfo is what Pyre learns from a profile's credential store. The token
// is held as a secret and used only by the usage probe; everything else is
// nonsecret metadata.
type storeInfo struct {
	Exists      bool   // a store (file or keychain item) exists
	Present     bool   // it holds a usable-looking login
	Source      string // "keychain:<service>" | "file" | "token-file"
	Locked      string // keychain unavailable/locked (actionable error)
	APIKeyOnly  bool   // Codex: API-key login (API billing; never a subscription)
	AccessToken secret
	ExpiresAt   time.Time // zero = unknown
}

type claudeOAuth struct {
	ClaudeAiOauth *struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresAt    int64  `json:"expiresAt"` // unix ms
	} `json:"claudeAiOauth"`
}

func parseClaudeStore(b []byte, si *storeInfo) {
	var c claudeOAuth
	if json.Unmarshal(b, &c) != nil || c.ClaudeAiOauth == nil {
		return
	}
	o := c.ClaudeAiOauth
	si.Present = o.AccessToken != "" || o.RefreshToken != ""
	si.AccessToken = secret{o.AccessToken}
	if o.ExpiresAt > 0 {
		si.ExpiresAt = time.UnixMilli(o.ExpiresAt).UTC()
	}
}

// readClaudeStore inspects a Claude profile's credential store: the macOS
// keychain item for the canonical profile dir (through `security`, as Claude
// itself does), then <dir>/.credentials.json. Never refreshes, never writes.
func (s *Service) readClaudeStore(ctx context.Context, g grantRow) storeInfo {
	var si storeInfo
	dir := g.ProfileDir
	if g.Kind == contracts.CredClaudeTokenFD {
		b, err := os.ReadFile(filepath.Join(dir, "oauth-token"))
		if err == nil {
			si.Exists, si.Source = true, "token-file"
			tok := strings.TrimSpace(string(b))
			si.Present = tok != ""
			si.AccessToken = secret{tok}
		}
		return si
	}
	// File store (Linux, and the macOS fallback when the keychain refuses).
	var file storeInfo
	if b, err := os.ReadFile(filepath.Join(dir, ".credentials.json")); err == nil {
		file.Exists, file.Source = true, "file"
		parseClaudeStore(b, &file)
	}
	if s.cfg.GOOS != "darwin" {
		return file
	}
	svc := ClaudeKeychainService(dir)
	out, code, err := s.runSecurity(ctx, "find-generic-password", "-s", svc, "-w")
	switch {
	case err == nil && code == 0:
		si.Exists, si.Source = true, "keychain:"+svc
		b := bytes.TrimSpace(out)
		if len(b) > 0 && b[0] != '{' {
			if dec, derr := hex.DecodeString(string(b)); derr == nil {
				b = dec
			}
		}
		parseClaudeStore(b, &si)
		if si.Present || !file.Present {
			return si // keychain wins; a blank item with no file is a blanked store
		}
		return file
	case code == 44:
		// errSecItemNotFound: Claude uses (or fell back to) the file.
		return file
	default:
		if file.Present {
			return file
		}
		// Never mistake an unreadable keychain for a blanked login.
		return storeInfo{Locked: fmt.Sprintf("keychain item %q unavailable (security exit %d); unlock the login keychain", svc, code)}
	}
}

var execCommand = exec.CommandContext

// runSecurity runs the configured `security` binary with a short timeout and
// a minimal environment. Output is returned to the caller only; never logged.
func (s *Service) runSecurity(ctx context.Context, args ...string) ([]byte, int, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := execCommand(ctx, s.cfg.SecurityPath, args...)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + os.Getenv("HOME"), "USER=" + os.Getenv("USER")}
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return nil, ee.ExitCode(), nil
		}
		return nil, -1, err
	}
	return out.Bytes(), 0, nil
}

// claudeIdentity reads oauthAccount.accountUuid from the profile's
// .claude.json (local; no network).
func claudeIdentity(dir string) (string, error) {
	b, err := os.ReadFile(filepath.Join(dir, ".claude.json"))
	if err != nil {
		return "", err
	}
	var c struct {
		OAuthAccount *struct {
			AccountUUID string `json:"accountUuid"`
		} `json:"oauthAccount"`
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return "", fmt.Errorf(".claude.json unparseable")
	}
	if c.OAuthAccount == nil || c.OAuthAccount.AccountUUID == "" {
		return "", fmt.Errorf("no oauthAccount in profile .claude.json")
	}
	return c.OAuthAccount.AccountUUID, nil
}

type codexAuth struct {
	AuthMode string  `json:"auth_mode"`
	APIKey   *string `json:"OPENAI_API_KEY"`
	Tokens   *struct {
		IDToken      string `json:"id_token"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		AccountID    string `json:"account_id"`
	} `json:"tokens"`
}

func jwtClaims(tok string) map[string]any {
	parts := strings.Split(tok, ".")
	if len(parts) < 2 {
		return nil
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return nil
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return nil
	}
	return m
}

func jwtExpiry(tok string) time.Time {
	if c := jwtClaims(tok); c != nil {
		if exp, ok := c["exp"].(float64); ok && exp > 0 {
			return time.Unix(int64(exp), 0).UTC()
		}
	}
	return time.Time{}
}

// readCodexStore inspects <home>/auth.json locally.
func readCodexStore(dir string) (storeInfo, string) {
	var si storeInfo
	b, err := os.ReadFile(filepath.Join(dir, "auth.json"))
	if err != nil {
		return si, ""
	}
	si.Exists, si.Source = true, "file"
	var a codexAuth
	if json.Unmarshal(b, &a) != nil {
		return si, ""
	}
	if a.Tokens == nil || (a.Tokens.AccessToken == "" && a.Tokens.RefreshToken == "") {
		si.APIKeyOnly = a.APIKey != nil && *a.APIKey != ""
		return si, ""
	}
	si.Present = true
	si.AccessToken = secret{a.Tokens.AccessToken}
	si.ExpiresAt = jwtExpiry(a.Tokens.AccessToken)
	ident := a.Tokens.AccountID
	if ident == "" {
		if c := jwtClaims(a.Tokens.IDToken); c != nil {
			if auth, ok := c["https://api.openai.com/auth"].(map[string]any); ok {
				ident, _ = auth["chatgpt_account_id"].(string)
			}
		}
	}
	return si, ident
}

// verdict is the result of a pre-launch grant check.
type verdict struct {
	OK      bool
	Outcome contracts.FallbackOutcome // intervention when not OK and human action is needed
	Reason  string
	Grant   grantRow
	Changed bool // grant row needs persisting
	Store   storeInfo
}

// verifyGrant checks a grant before launch using local signals only:
// credential presence (dead/blanked detection), API-key logins, and the
// profile's identity against Account.ProviderRef. It never refreshes.
func (s *Service) verifyGrant(ctx context.Context, a contracts.Account, g grantRow) verdict {
	v := verdict{Grant: g, Outcome: contracts.FallbackIntervention}
	now := s.now()
	set := func(state contracts.GrantState, detail string) {
		if v.Grant.State != state || v.Grant.Detail != detail {
			v.Grant.State, v.Grant.Detail, v.Grant.ObservedAt, v.Changed = state, detail, now, true
		}
	}
	switch g.State {
	case contracts.GrantDead, contracts.GrantRevoked, contracts.GrantExpired:
		v.Reason = fmt.Sprintf("grant %s; re-login required (%s) then reverify", g.State, g.Detail)
		return v
	}
	if _, err := os.Stat(g.ProfileDir); err != nil {
		set(contracts.GrantUnknown, "profile directory missing")
		v.Reason = "profile directory missing: " + g.ProfileDir
		return v
	}
	var si storeInfo
	var ident string
	var identErr error
	switch g.Harness {
	case contracts.HarnessClaude:
		si = s.readClaudeStore(ctx, g)
		ident, identErr = claudeIdentity(g.ProfileDir)
	case contracts.HarnessCodex:
		si, ident = readCodexStore(g.ProfileDir)
		if ident == "" {
			identErr = fmt.Errorf("no account_id in auth.json")
		}
	default:
		v.Reason = "unsupported harness"
		return v
	}
	v.Store = si
	switch {
	case si.Locked != "":
		set(contracts.GrantUnknown, si.Locked)
		v.Reason = si.Locked
		return v
	case si.APIKeyOnly:
		set(contracts.GrantUnknown, "API-key login in profile; not a subscription (API billing is never used)")
		v.Reason = v.Grant.Detail
		return v
	case si.Exists && !si.Present && g.EverValid:
		// Claude blanks a profile's store when its refresh token is rejected.
		set(contracts.GrantDead, "credential store blanked (login rejected by provider); re-login required")
		v.Reason = v.Grant.Detail
		s.report(ctx, contracts.ReportGrant, v.Grant.DeviceGrant)
		return v
	case !si.Present:
		set(contracts.GrantUnknown, "login required")
		v.Reason = "no login in profile; run the profile login command"
		return v
	}
	if g.Harness == contracts.HarnessClaude {
		if st, ok := s.probeClaudeAuth(ctx, g.ProfileDir); ok {
			if why := checkClaudeAuth(st, g.ProfileDir); why != "" {
				set(contracts.GrantUnknown, why)
				v.Reason = why
				return v
			}
		}
	}
	if identErr != nil {
		v.Reason = "identity unavailable: " + identErr.Error()
		return v
	}
	if a.ProviderRef == "" {
		v.Reason = fmt.Sprintf("account has no provider_ref; confirm the profile identity %q (BindIdentity) before use", ident)
		return v
	}
	if ident != a.ProviderRef {
		set(v.Grant.State, fmt.Sprintf("identity mismatch: expected %s, profile has %s", a.ProviderRef, ident))
		v.Reason = v.Grant.Detail
		return v
	}
	if g.State != contracts.GrantValid && g.State != contracts.GrantExpiring {
		if !g.EverValid {
			// First verification of a new login lifecycle (fresh profile, or
			// after Reverify): bump so older assignments are rejected. A
			// transient unknown (e.g. locked keychain) keeps its generation.
			v.Grant.Generation++
		}
		v.Grant.EverValid = true
		v.Grant.IdentityRef = ident
		set(contracts.GrantValid, "identity verified locally")
	} else if g.IdentityRef != ident || !g.EverValid {
		v.Grant.IdentityRef, v.Grant.EverValid, v.Changed = ident, true, true
	}
	v.OK = true
	return v
}

func (s *Service) persistVerdict(ctx context.Context, v verdict) {
	if !v.Changed {
		return
	}
	if err := updateGrantState(ctx, s.db, v.Grant); err != nil {
		s.log.Warn("accounts: persist grant", "grant", v.Grant.GrantID, "err", err)
		return
	}
	s.report(ctx, contracts.ReportGrant, v.Grant.DeviceGrant)
}

// VerifyGrant checks (account, harness) locally and persists any state
// change. It is what the owner runs after logging in to a profile.
func (s *Service) VerifyGrant(ctx context.Context, accountID string, h contracts.Harness) (contracts.DeviceGrant, error) {
	a, err := s.Account(ctx, accountID)
	if err != nil {
		return contracts.DeviceGrant{}, err
	}
	g, err := s.getGrant(ctx, s.db, accountID, h)
	if err != nil {
		return contracts.DeviceGrant{}, err
	}
	v := s.verifyGrant(ctx, a, g)
	s.persistVerdict(ctx, v)
	if !v.OK {
		return v.Grant.DeviceGrant, contracts.Errorf(contracts.ErrUnavailable, "%s", v.Reason).WithRef(contracts.KindGrant, g.GrantID)
	}
	return v.Grant.DeviceGrant, nil
}

// Reverify clears a dead/expired/revoked grant after the owner performed a
// fresh login in the profile, bumping the generation so assignments pinned
// to the old login are rejected. The identity must still match.
func (s *Service) Reverify(ctx context.Context, accountID string, h contracts.Harness) (contracts.DeviceGrant, error) {
	g, err := s.getGrant(ctx, s.db, accountID, h)
	if err != nil {
		return contracts.DeviceGrant{}, err
	}
	switch g.State {
	case contracts.GrantDead, contracts.GrantExpired, contracts.GrantRevoked:
		// New login lifecycle: the next successful verification bumps the
		// generation.
		g.State, g.Detail, g.ObservedAt, g.EverValid = contracts.GrantUnknown, "reverify after re-login", s.now(), false
		if err := updateGrantState(ctx, s.db, g); err != nil {
			return g.DeviceGrant, err
		}
	}
	return s.VerifyGrant(ctx, accountID, h)
}

// BindIdentity records the profile's locally observed provider account id
// as Account.ProviderRef after the owner confirmed it. It refuses to
// overwrite a different existing ProviderRef.
func (s *Service) BindIdentity(ctx context.Context, accountID string, h contracts.Harness) (string, error) {
	a, err := s.Account(ctx, accountID)
	if err != nil {
		return "", err
	}
	g, err := s.getGrant(ctx, s.db, accountID, h)
	if err != nil {
		return "", err
	}
	var ident string
	switch h {
	case contracts.HarnessClaude:
		ident, err = claudeIdentity(g.ProfileDir)
	case contracts.HarnessCodex:
		_, ident = readCodexStore(g.ProfileDir)
		if ident == "" {
			err = fmt.Errorf("no account_id in auth.json")
		}
	}
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", contracts.Errorf(contracts.ErrUnavailable, "no login in profile yet")
		}
		return "", contracts.Errorf(contracts.ErrUnavailable, "identity unavailable: %v", err)
	}
	if a.ProviderRef != "" && a.ProviderRef != ident {
		return ident, contracts.Errorf(contracts.ErrConflict, "account already bound to %s; profile has %s", a.ProviderRef, ident)
	}
	a.ProviderRef = ident
	return ident, s.UpsertAccount(ctx, a)
}
