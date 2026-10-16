package cloud

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// AuthKey is a Tailscale auth key for one node boot. Key is a secret.
type AuthKey struct {
	ID      string
	Key     string
	Expires time.Time
}

// KeyMinter provides one auth key per create/start: single-use,
// ephemeral (the device leaves the tailnet when it logs out or stays
// offline), pre-authorized and tagged, so a boot never waits for approval
// and the node gets only the tag's ACL.
type KeyMinter interface {
	Mint(ctx context.Context, description string) (AuthKey, error)
	// Revoke invalidates a key that may not have been used (best effort).
	Revoke(ctx context.Context, id string) error
}

// StaticKey hands out a pre-made key once (it is single use).
type StaticKey struct {
	Key  string
	used bool
}

func (s *StaticKey) Mint(context.Context, string) (AuthKey, error) {
	if s.used {
		return AuthKey{}, errors.New("the configured Tailscale auth key was already used in this invocation")
	}
	s.used = true
	return AuthKey{Key: s.Key}, nil
}

func (s *StaticKey) Revoke(context.Context, string) error { return nil }

// OAuthMinter mints keys through the Tailscale API with an OAuth client
// (scope auth_keys, allowed to assign Tag).
type OAuthMinter struct {
	ClientID, ClientSecret string
	Tag                    string
	TTL                    time.Duration
	BaseURL                string // default https://api.tailscale.com
	HTTP                   *http.Client
	token                  string
}

func (o *OAuthMinter) base() string {
	if o.BaseURL != "" {
		return strings.TrimRight(o.BaseURL, "/")
	}
	return "https://api.tailscale.com"
}

func (o *OAuthMinter) client() *http.Client {
	if o.HTTP != nil {
		return o.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (o *OAuthMinter) accessToken(ctx context.Context) (string, error) {
	if o.token != "" {
		return o.token, nil
	}
	form := url.Values{"client_id": {o.ClientID}, "client_secret": {o.ClientSecret}, "grant_type": {"client_credentials"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.base()+"/api/v2/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if err := o.do(req, &tok); err != nil {
		return "", fmt.Errorf("tailscale oauth token: %w", err)
	}
	if tok.AccessToken == "" {
		return "", errors.New("tailscale oauth token: empty access token")
	}
	o.token = tok.AccessToken
	return o.token, nil
}

func (o *OAuthMinter) do(req *http.Request, out any) error {
	resp, err := o.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		// The error body never echoes our secrets; trim it anyway.
		msg := strings.TrimSpace(string(body))
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, msg)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(body, out)
}

func (o *OAuthMinter) Mint(ctx context.Context, description string) (AuthKey, error) {
	tok, err := o.accessToken(ctx)
	if err != nil {
		return AuthKey{}, err
	}
	ttl := o.TTL
	if ttl <= 0 {
		ttl = 15 * time.Minute
	}
	tag := o.Tag
	if tag == "" {
		tag = DefaultTailscaleTag
	}
	body := map[string]any{
		"capabilities": map[string]any{"devices": map[string]any{"create": map[string]any{
			"reusable": false, "ephemeral": true, "preauthorized": true, "tags": []string{tag},
		}}},
		"expirySeconds": int(ttl.Seconds()),
		"description":   clipDesc(description),
	}
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.base()+"/api/v2/tailnet/-/keys", bytes.NewReader(b))
	if err != nil {
		return AuthKey{}, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	var res struct {
		ID      string    `json:"id"`
		Key     string    `json:"key"`
		Expires time.Time `json:"expires"`
	}
	if err := o.do(req, &res); err != nil {
		return AuthKey{}, fmt.Errorf("tailscale: create auth key: %w", err)
	}
	if res.Key == "" {
		return AuthKey{}, errors.New("tailscale: create auth key: no key in the response")
	}
	return AuthKey{ID: res.ID, Key: res.Key, Expires: res.Expires}, nil
}

func (o *OAuthMinter) Revoke(ctx context.Context, id string) error {
	if id == "" {
		return nil
	}
	tok, err := o.accessToken(ctx)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, o.base()+"/api/v2/tailnet/-/keys/"+url.PathEscape(id), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	return o.do(req, nil)
}

// Tailscale descriptions allow letters, digits and dashes (max 50).
func clipDesc(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		case r == ' ' || r == '_' || r == '.':
			b.WriteRune('-')
		}
		if b.Len() >= 50 {
			break
		}
	}
	return b.String()
}

// MinterFromConfig picks the key source: an OAuth client (env or config)
// first, then a static key (env PYRE_TAILSCALE_AUTHKEY or the configured
// file). nil means none is configured.
func MinterFromConfig(t TailscaleConfig, getenv func(string) string) (KeyMinter, error) {
	id := or(getenv(EnvTailscaleClientID), t.OAuthClientID)
	secret := getenv(EnvTailscaleClientSecret)
	if secret == "" && t.OAuthClientSecretFile != "" {
		s, err := readSecretFile(t.OAuthClientSecretFile)
		if err != nil {
			return nil, fmt.Errorf("tailscale oauth secret: %w", err)
		}
		secret = s
	}
	if id != "" && secret != "" {
		ttl := time.Duration(t.KeyTTLSeconds) * time.Second
		return &OAuthMinter{ClientID: id, ClientSecret: secret, Tag: t.tag(), TTL: ttl}, nil
	}
	if k := strings.TrimSpace(getenv(EnvTailscaleAuthKey)); k != "" {
		return &StaticKey{Key: k}, nil
	}
	if t.AuthKeyFile != "" {
		k, err := readSecretFile(t.AuthKeyFile)
		if err != nil {
			return nil, fmt.Errorf("tailscale auth key: %w", err)
		}
		return &StaticKey{Key: k}, nil
	}
	return nil, nil
}
