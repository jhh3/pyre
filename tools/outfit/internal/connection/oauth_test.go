package connection

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/outfit/internal/kit"
	"github.com/jhh3/outfit/internal/runtime"
	"github.com/jhh3/outfit/internal/state"
	"github.com/jhh3/outfit/internal/vault"
	"golang.org/x/oauth2"
)

func TestRefreshOAuthFieldsMintsShortLivedAccessToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if err := request.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if request.Form.Get("grant_type") != "refresh_token" || request.Form.Get("refresh_token") != "refresh-1" {
			t.Fatalf("refresh form = %v", request.Form)
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(response, `{"access_token":"fresh-access","expires_in":3600,"token_type":"Bearer"}`)
	}))
	defer server.Close()
	method := kit.CredentialMethod{OAuth: &kit.OAuthConfig{AuthorizationURL: server.URL + "/auth", TokenURL: server.URL}}
	fields, err := RefreshOAuthFields(context.Background(), method, map[string]string{
		oauthClientIDField: "client", oauthClientSecretField: "secret", oauthAccessTokenField: "expired",
		oauthRefreshTokenField: "refresh-1", oauthTokenExpiryField: time.Now().Add(-time.Hour).Format(time.RFC3339Nano),
	})
	if err != nil {
		t.Fatal(err)
	}
	if fields[oauthAccessTokenField] != "fresh-access" || fields[oauthRefreshTokenField] != "refresh-1" {
		t.Fatalf("refreshed fields = %#v", fields)
	}
	expiry, err := time.Parse(time.RFC3339Nano, fields[oauthTokenExpiryField])
	if err != nil || expiry.Before(time.Now().Add(50*time.Minute)) {
		t.Fatalf("expiry = %v, %v", expiry, err)
	}
}

func TestSecretPersistsRotatedOAuthRefreshToken(t *testing.T) {
	t.Setenv("OUTFIT_VAULT_BACKEND", "file")
	ctx := context.Background()
	tokenServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if err := request.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if request.Form.Get("refresh_token") != "refresh-old" {
			t.Fatalf("refresh token = %q", request.Form.Get("refresh_token"))
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(response, `{"access_token":"access-new","refresh_token":"refresh-new","expires_in":3600,"token_type":"Bearer"}`)
	}))
	defer tokenServer.Close()
	root := t.TempDir()
	paths := state.Paths{Root: root, Database: root + "/outfit.db", Packages: root + "/packages", Skills: root + "/skills", Locks: root + "/locks", Vault: root + "/vault"}
	store, err := state.Open(ctx, paths)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	method := kit.CredentialMethod{ID: "oauth", Custody: "oauth", Fields: []kit.CredentialField{
		{Key: oauthClientIDField, Generated: true}, {Key: oauthAccessTokenField, Secret: true, Generated: true},
		{Key: oauthRefreshTokenField, Secret: true, Generated: true}, {Key: oauthTokenExpiryField, Generated: true},
	}, OAuth: &kit.OAuthConfig{AuthorizationURL: tokenServer.URL + "/auth", TokenURL: tokenServer.URL}}
	pkg := kit.Package{Key: "acme", Version: "1", CredentialMethods: []kit.CredentialMethod{method}, Modes: []kit.Mode{{ID: "mcp", CredentialMethods: []string{"oauth"}, DefaultCredentialMethod: "oauth"}}}
	raw, _ := json.Marshal(pkg)
	if err := store.InstallPackage(ctx, kit.Loaded{Package: pkg, Raw: raw}, "trusted"); err != nil {
		t.Fatal(err)
	}
	secret, err := vault.Open(paths)
	if err != nil {
		t.Fatal(err)
	}
	oldRef, err := secret.Put(ctx, map[string]string{oauthAccessTokenField: "access-old", oauthRefreshTokenField: "refresh-old"})
	if err != nil {
		t.Fatal(err)
	}
	connection := state.Connection{ID: "c1", Name: "work", ConnectorKey: "acme", SelectedMode: "mcp", CredentialMethod: "oauth", Status: "active", CredentialRef: oldRef, SafeIdentity: map[string]string{
		"setting." + oauthClientIDField: "client", "setting." + oauthTokenExpiryField: time.Now().Add(-time.Hour).Format(time.RFC3339Nano),
	}}
	if err := store.CreateConnection(ctx, connection); err != nil {
		t.Fatal(err)
	}
	connection, err = store.Connection(ctx, "work")
	if err != nil {
		t.Fatal(err)
	}
	service := Service{State: store, Vault: secret}
	fields, err := service.secret(ctx, connection)
	if err != nil || fields[oauthRefreshTokenField] != "refresh-new" {
		t.Fatalf("refreshed fields = %#v err=%v", fields, err)
	}
	current, err := store.Connection(ctx, "work")
	if err != nil || current.CredentialRef == oldRef {
		t.Fatalf("credential ref did not rotate: %#v err=%v", current, err)
	}
	stored, err := secret.Get(ctx, current.CredentialRef)
	if err != nil || stored[oauthRefreshTokenField] != "refresh-new" {
		t.Fatalf("persisted grant = %#v err=%v", stored, err)
	}
	if _, err := secret.Get(ctx, oldRef); err == nil {
		t.Fatal("superseded OAuth grant remained in the vault")
	}
}

func TestOAuthReconnectRequiredRecognizesInvalidGrant(t *testing.T) {
	err := &oauth2.RetrieveError{ErrorCode: "invalid_grant"}
	if !OAuthReconnectRequired(err) || OAuthReconnectRequired(errors.New("network down")) {
		t.Fatal("OAuth refresh failure classification is wrong")
	}
}

func TestOAuthCallbackPreservesTransferredLoopbackRedirect(t *testing.T) {
	method := kit.CredentialMethod{OAuth: &kit.OAuthConfig{TokenURL: "https://slack.com/api/oauth.v2.user.access"}}
	listen, redirect, path, err := oauthCallback(map[string]string{oauthRedirectURIField: "http://localhost:3118/callback"}, method)
	if err != nil || listen != "127.0.0.1:3118" || redirect != "http://localhost:3118/callback" || path != "/callback" {
		t.Fatalf("callback = %q %q %q err=%v", listen, redirect, path, err)
	}
	if _, _, _, err := oauthCallback(map[string]string{oauthRedirectURIField: "https://evil.example/callback"}, method); err == nil {
		t.Fatal("non-loopback transferred callback was accepted")
	}
}

func TestGeneratedOAuthScopeEvidenceMayBeOptional(t *testing.T) {
	method := kit.CredentialMethod{Fields: []kit.CredentialField{
		{Key: oauthAccessTokenField, Generated: true},
		{Key: oauthGrantedScopesField, Generated: true, Optional: true},
	}}
	if err := validateGeneratedFields(method, map[string]string{oauthAccessTokenField: "access"}); err != nil {
		t.Fatalf("provider-omitted scope evidence blocked a valid OAuth grant: %v", err)
	}
	method.Fields[1].Optional = false
	if err := validateGeneratedFields(method, map[string]string{oauthAccessTokenField: "access"}); err == nil {
		t.Fatal("required generated field was accepted when absent")
	}
}

func TestConfigureOAuthStoresGrantAndRunsWithSelectedToken(t *testing.T) {
	t.Setenv("OUTFIT_VAULT_BACKEND", "file")
	ctx := context.Background()
	root := t.TempDir()
	paths := state.Paths{Root: root, Database: root + "/outfit.db", Packages: root + "/packages", Skills: root + "/skills", Locks: root + "/locks", Vault: root + "/vault"}
	store, err := state.Open(ctx, paths)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer access-1" {
			t.Fatalf("authorization = %q", request.Header.Get("Authorization"))
		}
		_, _ = io.WriteString(response, `{"id":"acct_1","name":"Workspace"}`)
	}))
	defer server.Close()
	method := kit.CredentialMethod{
		ID: "oauth", Label: "OAuth", Description: "OAuth", Custody: "oauth", CapabilityField: "SERVICES",
		Fields: []kit.CredentialField{
			{Key: oauthClientIDField, Label: "Client ID"}, {Key: oauthClientSecretField, Label: "Client secret", Secret: true},
			{Key: "SERVICES", Label: "Services"},
			{Key: oauthAccessTokenField, Label: "Access", Secret: true, Generated: true},
			{Key: oauthRefreshTokenField, Label: "Refresh", Secret: true, Generated: true},
			{Key: oauthTokenExpiryField, Label: "Expiry", Generated: true},
			{Key: oauthGrantedScopesField, Label: "Scopes", Generated: true},
		},
		Bindings: []kit.CredentialBinding{{Target: "http-header", Name: "Authorization", Value: "Bearer {{secret.OAUTH_ACCESS_TOKEN}}"}},
		OAuth:    &kit.OAuthConfig{AuthorizationURL: "https://accounts.example/auth", TokenURL: "https://accounts.example/token", Scopes: []string{"openid"}, CapabilityScopes: map[string][]string{"gmail": {"gmail.scope"}}},
	}
	pkg := kit.Package{Key: "workspace", Version: "1", CredentialMethods: []kit.CredentialMethod{method}, Modes: []kit.Mode{{ID: "rest", Executor: kit.Executor{Kind: "http", BaseURL: server.URL + "/", PrivateNet: true}, CredentialMethods: []string{"oauth"}, DefaultCredentialMethod: "oauth", Capabilities: []string{"gmail"}, Operations: []kit.Operation{{Name: "account", Description: "Account", Class: "read", HTTP: &kit.HTTPOperation{Method: "GET", Path: "account"}}}, Verify: "account"}}, Skill: kit.Skill{Summary: "Workspace"}}
	raw, _ := json.Marshal(pkg)
	if err := store.InstallPackage(ctx, kit.Loaded{Package: pkg, Raw: raw}, "trusted"); err != nil {
		t.Fatal(err)
	}
	secret, err := vault.Open(paths)
	if err != nil {
		t.Fatal(err)
	}
	service := Service{
		State: store, Vault: secret, Executor: runtime.Executor{HTTPClient: server.Client()},
		OAuthAuthorize: func(_ context.Context, _ kit.CredentialMethod, _ map[string]string, capabilities []string) (OAuthGrant, error) {
			if strings.Join(capabilities, ",") != "gmail" {
				t.Fatalf("capabilities = %v", capabilities)
			}
			return OAuthGrant{Fields: map[string]string{
				oauthAccessTokenField: "access-1", oauthRefreshTokenField: "refresh-1",
				oauthTokenExpiryField: time.Now().Add(time.Hour).Format(time.RFC3339Nano), oauthGrantedScopesField: "gmail.scope,openid",
			}, Identity: map[string]string{"principal": "person@example.com", "scopes": "gmail.scope,openid"}}, nil
		},
	}
	configured, err := service.ConfigureWithMethod(ctx, "work", "workspace", "rest", "oauth", map[string]string{oauthClientIDField: "client", oauthClientSecretField: "client-secret", "SERVICES": "gmail"})
	if err != nil {
		t.Fatal(err)
	}
	if configured.SafeIdentity["principal"] != "person@example.com" || configured.SafeIdentity["capabilities"] != "gmail" {
		t.Fatalf("safe identity = %#v", configured.SafeIdentity)
	}
	if strings.Contains(strings.Join(mapValues(configured.SafeIdentity), " "), "access-1") {
		t.Fatal("access token entered safe identity")
	}
	stored, err := secret.Get(ctx, configured.CredentialRef)
	if err != nil {
		t.Fatal(err)
	}
	if stored[oauthRefreshTokenField] != "refresh-1" || stored[oauthClientSecretField] != "client-secret" {
		t.Fatalf("stored grant = %#v", stored)
	}
	if _, err := service.Run(ctx, "work", "account", nil, false); err != nil {
		t.Fatal(err)
	}
}

func mapValues(values map[string]string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		result = append(result, value)
	}
	return result
}
