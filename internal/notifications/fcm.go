package notifications

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

const (
	defaultFCMEndpoint = "https://fcm.googleapis.com"
	defaultTokenURI    = "https://oauth2.googleapis.com/token"
	fcmScope           = "https://www.googleapis.com/auth/firebase.messaging"
	jwtBearerGrant     = "urn:ietf:params:oauth:grant-type:jwt-bearer"
)

// FCMConfig configures the FCM HTTP v1 provider.
type FCMConfig struct {
	// ServiceAccountFile is the path to a Google service-account JSON key
	// with the Firebase Cloud Messaging permission. It lives outside the
	// repository (never committed) and should be readable only by the hub.
	ServiceAccountFile string
	// ServiceAccountJSON may be used instead of ServiceAccountFile.
	ServiceAccountJSON []byte
	// Endpoint overrides https://fcm.googleapis.com (tests).
	Endpoint string
	// TokenURL overrides the service account's token_uri (tests).
	TokenURL string
	// HTTPClient defaults to a client with a 15s timeout.
	HTTPClient *http.Client
	Clock      contracts.Clock
	// TTL is the Android message lifetime (default 1h); stale alerts are
	// not useful because the Inbox is authoritative.
	TTL time.Duration
}

type serviceAccount struct {
	Type         string `json:"type"`
	ProjectID    string `json:"project_id"`
	PrivateKeyID string `json:"private_key_id"`
	PrivateKey   string `json:"private_key"`
	ClientEmail  string `json:"client_email"`
	TokenURI     string `json:"token_uri"`
}

// FCM delivers data-only messages through the FCM HTTP v1 API using the
// OAuth2 JWT-bearer flow (RS256, standard library only) with token caching.
type FCM struct {
	projectID string
	email     string
	keyID     string
	key       *rsa.PrivateKey
	tokenURL  string
	sendURL   string
	client    *http.Client
	clock     contracts.Clock
	ttl       time.Duration

	mu        sync.Mutex
	token     string
	tokenExp  time.Time
	tokenGets int
}

var _ Provider = (*FCM)(nil)

// NewFCM loads and validates the service account. It does not contact
// Google; the first access token is fetched on first delivery.
func NewFCM(cfg FCMConfig) (*FCM, error) {
	raw := cfg.ServiceAccountJSON
	if len(raw) == 0 {
		if cfg.ServiceAccountFile == "" {
			return nil, errors.New("fcm: no service account configured")
		}
		b, err := os.ReadFile(cfg.ServiceAccountFile)
		if err != nil {
			return nil, fmt.Errorf("fcm: read service account: %w", err)
		}
		raw = b
	}
	var sa serviceAccount
	if err := json.Unmarshal(raw, &sa); err != nil {
		return nil, errors.New("fcm: service account file is not valid JSON")
	}
	if sa.Type != "service_account" || sa.ProjectID == "" || sa.ClientEmail == "" || sa.PrivateKey == "" {
		return nil, errors.New("fcm: service account must have type service_account, project_id, client_email, and private_key")
	}
	key, err := parseRSAKey(sa.PrivateKey)
	if err != nil {
		return nil, err
	}
	tokenURL := firstSet(cfg.TokenURL, sa.TokenURI, defaultTokenURI)
	endpoint := strings.TrimRight(firstSet(cfg.Endpoint, defaultFCMEndpoint), "/")
	for _, u := range []string{tokenURL, endpoint} {
		if err := requireSecureURL(u); err != nil {
			return nil, err
		}
	}
	f := &FCM{
		projectID: sa.ProjectID,
		email:     sa.ClientEmail,
		keyID:     sa.PrivateKeyID,
		key:       key,
		tokenURL:  tokenURL,
		sendURL:   endpoint + "/v1/projects/" + url.PathEscape(sa.ProjectID) + "/messages:send",
		client:    cfg.HTTPClient,
		clock:     cfg.Clock,
		ttl:       cfg.TTL,
	}
	if f.client == nil {
		f.client = &http.Client{Timeout: 15 * time.Second}
	}
	if f.clock == nil {
		f.clock = contracts.SystemClock{}
	}
	if f.ttl <= 0 {
		f.ttl = time.Hour
	}
	return f, nil
}

func firstSet(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// requireSecureURL allows https, or plain http only to a loopback host
// (local fakes), so the signed assertion and bearer token never cross a
// network in clear text.
func requireSecureURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("fcm: bad URL %q", raw)
	}
	if u.Scheme == "https" {
		return nil
	}
	if u.Scheme == "http" {
		host := u.Hostname()
		if host == "localhost" {
			return nil
		}
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
			return nil
		}
	}
	return fmt.Errorf("fcm: URL %q must use https", raw)
}

func parseRSAKey(p string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(p))
	if block == nil {
		return nil, errors.New("fcm: private_key is not PEM")
	}
	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		rk, ok := k.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("fcm: private_key is not an RSA key")
		}
		return rk, nil
	}
	k, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("fcm: cannot parse private_key")
	}
	return k, nil
}

// Name returns "fcm".
func (f *FCM) Name() string { return ProviderFCM }

// ProjectID returns the Firebase project of the service account.
func (f *FCM) ProjectID() string { return f.projectID }

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// signAssertion builds the RS256 JWT for the OAuth2 JWT-bearer grant.
func (f *FCM) signAssertion(now time.Time) (string, error) {
	hdr := map[string]string{"alg": "RS256", "typ": "JWT"}
	if f.keyID != "" {
		hdr["kid"] = f.keyID
	}
	claims := map[string]any{
		"iss":   f.email,
		"scope": fcmScope,
		"aud":   f.tokenURL,
		"iat":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
	}
	hb, err := json.Marshal(hdr)
	if err != nil {
		return "", err
	}
	cb, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	signing := b64(hb) + "." + b64(cb)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, f.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signing + "." + b64(sig), nil
}

func (f *FCM) accessToken(ctx context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.clock.Now()
	if f.token != "" && now.Before(f.tokenExp.Add(-time.Minute)) {
		return f.token, nil
	}
	assertion, err := f.signAssertion(now)
	if err != nil {
		return "", err
	}
	form := url.Values{"grant_type": {jwtBearerGrant}, "assertion": {assertion}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := f.client.Do(req)
	if err != nil {
		return "", &fcmError{msg: "token request failed", retryable: true}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		var oe struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(body, &oe)
		return "", &fcmError{msg: fmt.Sprintf("token endpoint returned %d %s", resp.StatusCode, clip(oe.Error, 60)),
			retryable: resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500}
	}
	var tr struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
		TokenType   string `json:"token_type"`
	}
	if err := json.Unmarshal(body, &tr); err != nil || tr.AccessToken == "" {
		return "", &fcmError{msg: "token endpoint returned no access_token", retryable: true}
	}
	if tr.ExpiresIn <= 0 {
		tr.ExpiresIn = 3600
	}
	f.token = tr.AccessToken
	f.tokenExp = now.Add(time.Duration(tr.ExpiresIn) * time.Second)
	f.tokenGets++
	return f.token, nil
}

func (f *FCM) invalidateToken() {
	f.mu.Lock()
	f.token = ""
	f.mu.Unlock()
}

// TokenFetches reports how many access tokens were obtained (cache metric).
func (f *FCM) TokenFetches() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tokenGets
}

type fcmError struct {
	msg          string
	retryable    bool
	unregistered bool
	auth         bool
	retryAfter   time.Duration
}

func (e *fcmError) Error() string { return "fcm: " + e.msg }

// Deliver sends m to each target. A 401 refreshes the access token once.
func (f *FCM) Deliver(ctx context.Context, m Message, targets []Target) []Result {
	out := make([]Result, 0, len(targets))
	for _, t := range targets {
		err := f.send(ctx, m, t.DeviceToken)
		var fe *fcmError
		if errors.As(err, &fe) && fe.auth {
			f.invalidateToken()
			err = f.send(ctx, m, t.DeviceToken)
		}
		r := Result{Target: t, Err: err}
		if errors.As(err, &fe) {
			r.Retryable, r.Unregistered, r.RetryAfter = fe.retryable, fe.unregistered, fe.retryAfter
		} else if err != nil {
			r.Retryable = true
		}
		out = append(out, r)
	}
	return out
}

type fcmAndroid struct {
	Priority    string `json:"priority"`
	CollapseKey string `json:"collapse_key,omitempty"`
	TTL         string `json:"ttl"`
}

type fcmMessage struct {
	Token   string            `json:"token"`
	Data    map[string]string `json:"data"`
	Android fcmAndroid        `json:"android"`
}

// payload builds the HTTP v1 request body: data-only (no "notification"
// block), so the app renders the alert and can withdraw it later.
func (f *FCM) payload(m Message, token string) ([]byte, error) {
	prio := "HIGH"
	if m.Action == ActionDismiss || m.Severity == "info" {
		prio = "NORMAL"
	}
	return json.Marshal(map[string]fcmMessage{"message": {
		Token: token,
		Data:  m.Data(),
		Android: fcmAndroid{
			Priority:    prio,
			CollapseKey: clip(m.GroupKey, 64),
			TTL:         strconv.FormatInt(int64(f.ttl/time.Second), 10) + "s",
		},
	}})
}

func (f *FCM) send(ctx context.Context, m Message, token string) error {
	if token == "" {
		return &fcmError{msg: "empty device token", unregistered: true}
	}
	at, err := f.accessToken(ctx)
	if err != nil {
		return err
	}
	body, err := f.payload(m, token)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.sendURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+at)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := f.client.Do(req)
	if err != nil {
		return &fcmError{msg: "send request failed", retryable: true}
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	return classifyFCMError(resp, rb)
}

func classifyFCMError(resp *http.Response, body []byte) error {
	var er struct {
		Error struct {
			Code    int    `json:"code"`
			Status  string `json:"status"`
			Details []struct {
				Type      string `json:"@type"`
				ErrorCode string `json:"errorCode"`
			} `json:"details"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &er)
	code := ""
	for _, d := range er.Error.Details {
		if d.ErrorCode != "" {
			code = d.ErrorCode
			break
		}
	}
	e := &fcmError{msg: fmt.Sprintf("send returned %d %s %s", resp.StatusCode, er.Error.Status, code)}
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		if n, err := strconv.Atoi(ra); err == nil && n > 0 {
			e.retryAfter = time.Duration(n) * time.Second
		}
	}
	switch {
	case code == "UNREGISTERED" || resp.StatusCode == http.StatusNotFound:
		e.unregistered = true
	case code == "SENDER_ID_MISMATCH":
		e.unregistered = true // token belongs to another Firebase project
	case resp.StatusCode == http.StatusUnauthorized:
		e.auth = true
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 || code == "QUOTA_EXCEEDED" || code == "UNAVAILABLE" || code == "INTERNAL":
		e.retryable = true
	}
	return e
}
