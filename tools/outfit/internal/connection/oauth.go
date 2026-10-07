package connection

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	goruntime "runtime"
	"sort"
	"strings"
	"time"

	"github.com/jhh3/outfit/internal/kit"
	"golang.org/x/oauth2"
)

const (
	oauthClientIDField      = "OAUTH_CLIENT_ID"
	oauthClientSecretField  = "OAUTH_CLIENT_SECRET"
	oauthAccessTokenField   = "OAUTH_ACCESS_TOKEN"
	oauthRefreshTokenField  = "OAUTH_REFRESH_TOKEN"
	oauthTokenExpiryField   = "OAUTH_TOKEN_EXPIRY"
	oauthGrantedScopesField = "OAUTH_GRANTED_SCOPES"
	oauthRedirectURIField   = "OAUTH_REDIRECT_URI"
)

type OAuthGrant struct {
	Fields   map[string]string
	Identity map[string]string
}

type OAuthAuthorizer func(context.Context, kit.CredentialMethod, map[string]string, []string) (OAuthGrant, error)

func AuthorizeBrowser(ctx context.Context, method kit.CredentialMethod, fields map[string]string, capabilities []string) (OAuthGrant, error) {
	if method.OAuth == nil {
		return OAuthGrant{}, errors.New("credential method has no OAuth configuration")
	}
	clientID := strings.TrimSpace(fields[oauthClientIDField])
	if clientID == "" {
		return OAuthGrant{}, errors.New("OAuth client ID is required")
	}
	listenAddress, redirectURL, callbackPath, err := oauthCallback(fields, method)
	if err != nil {
		return OAuthGrant{}, err
	}
	listener, err := net.Listen("tcp", listenAddress)
	if err != nil {
		return OAuthGrant{}, fmt.Errorf("start OAuth callback: %w", err)
	}
	defer listener.Close()
	if redirectURL == "" {
		redirectURL = "http://" + listener.Addr().String() + callbackPath
	}
	config := oauthConfig(method, fields, capabilities, redirectURL)
	if len(config.Scopes) == 0 {
		config.Scopes = splitOAuthScopes(fields[oauthGrantedScopesField])
	}
	state, err := randomURLToken(32)
	if err != nil {
		return OAuthGrant{}, err
	}
	verifier := oauth2.GenerateVerifier()
	type callback struct {
		code string
		err  error
	}
	result := make(chan callback, 1)
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second}
	mux := http.NewServeMux()
	mux.HandleFunc(callbackPath, func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Get("state") != state {
			http.Error(response, "OAuth state did not match. Return to outfit and retry.", http.StatusBadRequest)
			select {
			case result <- callback{err: errors.New("OAuth state did not match")}:
			default:
			}
			return
		}
		if upstreamError := request.URL.Query().Get("error"); upstreamError != "" {
			http.Error(response, "Google authorization was not completed. Return to outfit and retry.", http.StatusBadRequest)
			select {
			case result <- callback{err: fmt.Errorf("OAuth authorization: %s", upstreamError)}:
			default:
			}
			return
		}
		code := request.URL.Query().Get("code")
		if code == "" {
			http.Error(response, "Authorization code is missing. Return to outfit and retry.", http.StatusBadRequest)
			select {
			case result <- callback{err: errors.New("OAuth callback omitted code")}:
			default:
			}
			return
		}
		response.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = response.Write([]byte("<!doctype html><title>Connected to outfit</title><h1>Connected</h1><p>You can close this window and return to outfit.</p>"))
		select {
		case result <- callback{code: code}:
		default:
		}
	})
	server.Handler = mux
	go func() { _ = server.Serve(listener) }()
	defer server.Shutdown(context.Background())
	authorizationURL := config.AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.SetAuthURLParam("prompt", "consent"), oauth2.S256ChallengeOption(verifier))
	if err := openBrowser(authorizationURL); err != nil {
		return OAuthGrant{}, fmt.Errorf("open this URL in a browser and retry if needed: %s (%w)", authorizationURL, err)
	}
	var returned callback
	select {
	case returned = <-result:
	case <-ctx.Done():
		return OAuthGrant{}, ctx.Err()
	case <-time.After(5 * time.Minute):
		return OAuthGrant{}, errors.New("OAuth authorization timed out after 5 minutes")
	}
	if returned.err != nil {
		return OAuthGrant{}, returned.err
	}
	token, err := config.Exchange(ctx, returned.code, oauth2.VerifierOption(verifier))
	if err != nil {
		return OAuthGrant{}, fmt.Errorf("exchange OAuth code: %w", err)
	}
	if token.RefreshToken == "" {
		return OAuthGrant{}, errors.New("provider returned no refresh token; revoke the existing app grant and retry")
	}
	scopes := resolvedOAuthScopes(method, capabilities)
	grant := OAuthGrant{Fields: map[string]string{
		oauthAccessTokenField: token.AccessToken, oauthRefreshTokenField: token.RefreshToken,
		oauthTokenExpiryField: token.Expiry.UTC().Format(time.RFC3339Nano), oauthGrantedScopesField: strings.Join(scopes, ","),
	}, Identity: map[string]string{"scopes": strings.Join(scopes, ",")}}
	if method.OAuth.UserInfoURL != "" {
		identity, err := oauthUserInfo(ctx, config, token, method.OAuth.UserInfoURL)
		if err != nil {
			return OAuthGrant{}, err
		}
		for key, value := range identity {
			grant.Identity[key] = value
		}
	}
	return grant, nil
}

func oauthCallback(fields map[string]string, method kit.CredentialMethod) (listenAddress, redirectURL, callbackPath string, err error) {
	configured := strings.TrimSpace(fields[oauthRedirectURIField])
	if configured == "" {
		return "127.0.0.1:0", "", "/oauth/callback", nil
	}
	parsed, parseErr := url.Parse(configured)
	if parseErr != nil || parsed.Scheme != "http" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Hostname() != "localhost" && parsed.Hostname() != "127.0.0.1") || parsed.Port() == "" || parsed.EscapedPath() == "" {
		return "", "", "", errors.New("OAuth redirect URI must be an exact loopback HTTP URL with a port and callback path")
	}
	return net.JoinHostPort("127.0.0.1", parsed.Port()), configured, parsed.EscapedPath(), nil
}

func splitOAuthScopes(raw string) []string {
	seen := map[string]bool{}
	var result []string
	for _, scope := range strings.FieldsFunc(raw, func(value rune) bool { return value == ',' || value == ' ' }) {
		if scope = strings.TrimSpace(scope); scope != "" && !seen[scope] {
			seen[scope] = true
			result = append(result, scope)
		}
	}
	sort.Strings(result)
	return result
}

func RefreshOAuthFields(ctx context.Context, method kit.CredentialMethod, fields map[string]string) (map[string]string, error) {
	if method.OAuth == nil {
		return fields, nil
	}
	result := cloneStrings(fields)
	expiry, _ := time.Parse(time.RFC3339Nano, fields[oauthTokenExpiryField])
	if fields[oauthAccessTokenField] != "" && expiry.After(time.Now().Add(time.Minute)) {
		return result, nil
	}
	if fields[oauthRefreshTokenField] == "" {
		return nil, errors.New("OAuth refresh token is missing; reconnect")
	}
	config := oauthConfig(method, fields, nil, "")
	token, err := config.TokenSource(ctx, &oauth2.Token{AccessToken: fields[oauthAccessTokenField], RefreshToken: fields[oauthRefreshTokenField], Expiry: expiry}).Token()
	if err != nil {
		return nil, fmt.Errorf("refresh OAuth token: %w", err)
	}
	result[oauthAccessTokenField] = token.AccessToken
	result[oauthTokenExpiryField] = token.Expiry.UTC().Format(time.RFC3339Nano)
	if token.RefreshToken != "" {
		result[oauthRefreshTokenField] = token.RefreshToken
	}
	return result, nil
}

func OAuthReconnectRequired(err error) bool {
	var retrieve *oauth2.RetrieveError
	return errors.As(err, &retrieve) && (retrieve.ErrorCode == "invalid_grant" || retrieve.ErrorCode == "invalid_token")
}

func OAuthIdentity(ctx context.Context, method kit.CredentialMethod, fields map[string]string) (map[string]string, error) {
	if method.OAuth == nil || method.OAuth.UserInfoURL == "" {
		return nil, nil
	}
	expiry, _ := time.Parse(time.RFC3339Nano, fields[oauthTokenExpiryField])
	token := &oauth2.Token{AccessToken: fields[oauthAccessTokenField], RefreshToken: fields[oauthRefreshTokenField], Expiry: expiry}
	return oauthUserInfo(ctx, oauthConfig(method, fields, nil, ""), token, method.OAuth.UserInfoURL)
}

func oauthConfig(method kit.CredentialMethod, fields map[string]string, capabilities []string, redirectURL string) *oauth2.Config {
	return &oauth2.Config{
		ClientID: fields[oauthClientIDField], ClientSecret: fields[oauthClientSecretField], RedirectURL: redirectURL,
		Endpoint: oauth2.Endpoint{AuthURL: method.OAuth.AuthorizationURL, TokenURL: method.OAuth.TokenURL},
		Scopes:   resolvedOAuthScopes(method, capabilities),
	}
}

func resolvedOAuthScopes(method kit.CredentialMethod, capabilities []string) []string {
	seen := map[string]bool{}
	result := make([]string, 0, len(method.OAuth.Scopes))
	appendScope := func(scope string) {
		if scope = strings.TrimSpace(scope); scope != "" && !seen[scope] {
			seen[scope] = true
			result = append(result, scope)
		}
	}
	for _, scope := range method.OAuth.Scopes {
		appendScope(scope)
	}
	for _, capability := range capabilities {
		for _, scope := range method.OAuth.CapabilityScopes[capability] {
			appendScope(scope)
		}
	}
	sort.Strings(result)
	return result
}

func oauthUserInfo(ctx context.Context, config *oauth2.Config, token *oauth2.Token, endpoint string) (map[string]string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	response, err := config.Client(ctx, token).Do(request)
	if err != nil {
		return nil, fmt.Errorf("read OAuth identity: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("read OAuth identity: %s", response.Status)
	}
	var payload struct {
		Sub   string `json:"sub"`
		Email string `json:"email"`
		Name  string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode OAuth identity: %w", err)
	}
	return map[string]string{"account_id": payload.Sub, "principal": payload.Email, "account_name": payload.Name}, nil
}

func randomURLToken(size int) (string, error) {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func openBrowser(target string) error {
	parsed, err := url.Parse(target)
	if err != nil || parsed.Scheme != "https" {
		return errors.New("OAuth authorization URL is not HTTPS")
	}
	var command *exec.Cmd
	switch goruntime.GOOS {
	case "darwin":
		command = exec.Command("open", target)
	case "windows":
		command = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	default:
		command = exec.Command("xdg-open", target)
	}
	return command.Start()
}

func cloneStrings(values map[string]string) map[string]string {
	result := make(map[string]string, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}
