package hostmcp

import (
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

type OAuthMetadata struct {
	AuthorizationURL string
	TokenURL         string
	Scopes           []string
}

func DiscoverOAuthMetadata(ctx context.Context, candidate Candidate) (OAuthMetadata, error) {
	if known, ok := knownOAuthMetadata(candidate.Endpoint); ok {
		return known, nil
	}
	endpoint, err := url.Parse(candidate.Endpoint)
	if err != nil {
		return OAuthMetadata{}, err
	}
	origin := endpoint.Scheme + "://" + endpoint.Host
	resourceURLs := []string{origin + "/.well-known/oauth-protected-resource" + endpoint.EscapedPath(), origin + "/.well-known/oauth-protected-resource"}
	var resource struct {
		AuthorizationServers []string `json:"authorization_servers"`
		Scopes               []string `json:"scopes_supported"`
	}
	var resourceErr error
	for _, target := range resourceURLs {
		if err := readMetadata(ctx, target, &resource); err == nil && len(resource.AuthorizationServers) > 0 {
			resourceErr = nil
			break
		} else {
			resourceErr = err
		}
	}
	if resourceErr != nil || len(resource.AuthorizationServers) == 0 {
		return OAuthMetadata{}, fmt.Errorf("discover protected-resource metadata: %w", resourceErr)
	}
	authorizationServer, err := fixedHTTPS(resource.AuthorizationServers[0])
	if err != nil {
		return OAuthMetadata{}, fmt.Errorf("authorization server: %w", err)
	}
	issuer, _ := url.Parse(authorizationServer)
	metadataURLs := []string{issuer.Scheme + "://" + issuer.Host + "/.well-known/oauth-authorization-server" + strings.TrimSuffix(issuer.EscapedPath(), "/"), issuer.Scheme + "://" + issuer.Host + "/.well-known/openid-configuration" + strings.TrimSuffix(issuer.EscapedPath(), "/")}
	var metadata struct {
		AuthorizationURL string   `json:"authorization_endpoint"`
		TokenURL         string   `json:"token_endpoint"`
		Scopes           []string `json:"scopes_supported"`
	}
	var metadataErr error
	for _, target := range metadataURLs {
		if err := readMetadata(ctx, target, &metadata); err == nil && metadata.AuthorizationURL != "" && metadata.TokenURL != "" {
			metadataErr = nil
			break
		} else {
			metadataErr = err
		}
	}
	if metadataErr != nil {
		return OAuthMetadata{}, fmt.Errorf("discover authorization-server metadata: %w", metadataErr)
	}
	metadata.AuthorizationURL, err = fixedHTTPS(metadata.AuthorizationURL)
	if err != nil {
		return OAuthMetadata{}, fmt.Errorf("authorization endpoint: %w", err)
	}
	metadata.TokenURL, err = fixedHTTPS(metadata.TokenURL)
	if err != nil {
		return OAuthMetadata{}, fmt.Errorf("token endpoint: %w", err)
	}
	scopes := resource.Scopes
	if len(scopes) == 0 {
		scopes = metadata.Scopes
	}
	return OAuthMetadata{AuthorizationURL: metadata.AuthorizationURL, TokenURL: metadata.TokenURL, Scopes: normalizeScopes(scopes)}, nil
}

func knownOAuthMetadata(endpoint string) (OAuthMetadata, bool) {
	values := map[string]OAuthMetadata{
		"https://mcp.linear.app/mcp": {AuthorizationURL: "https://mcp.linear.app/authorize", TokenURL: "https://mcp.linear.app/token", Scopes: []string{"read", "write"}},
		"https://mcp.notion.com/mcp": {AuthorizationURL: "https://mcp.notion.com/authorize", TokenURL: "https://mcp.notion.com/token", Scopes: []string{"default"}},
		"https://mcp.sentry.dev/mcp": {AuthorizationURL: "https://mcp.sentry.dev/oauth/authorize", TokenURL: "https://mcp.sentry.dev/oauth/token", Scopes: []string{"org:read", "project:write", "team:write", "event:write"}},
		"https://mcp.slack.com/mcp":  {AuthorizationURL: "https://slack.com/oauth/v2_user/authorize", TokenURL: "https://slack.com/api/oauth.v2.user.access"},
	}
	value, ok := values[endpoint]
	return value, ok
}

func readMetadata(ctx context.Context, target string, destination any) error {
	target, err := fixedHTTPS(target)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(request *http.Request, via []*http.Request) error {
		if len(via) >= 3 {
			return errors.New("too many metadata redirects")
		}
		_, err := fixedHTTPS(request.URL.String())
		return err
	}}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("%s returned %s", target, response.Status)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(destination); err != nil {
		return err
	}
	return nil
}

func fixedHTTPS(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("URL must be fixed HTTPS without credentials, query, or fragment")
	}
	return parsed.String(), nil
}
