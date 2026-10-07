package connection

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/outfit/internal/kit"
	cwruntime "github.com/jhh3/outfit/internal/runtime"
	"github.com/jhh3/outfit/internal/state"
	"github.com/jhh3/outfit/internal/vault"
)

func TestNativeCredentialDetectionImportsAuthenticatedCLIWithoutLogin(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell helper is Unix-only")
	}
	ctx := context.Background()
	root := t.TempDir()
	authenticated := filepath.Join(root, "authenticated")
	setupCalled := filepath.Join(root, "setup-called")
	if err := os.WriteFile(authenticated, []byte("yes"), 0o600); err != nil {
		t.Fatal(err)
	}
	binary := writeNativeLoginHelper(t, authenticated, setupCalled)
	pkg := kit.Package{
		Key: "native", Version: "1",
		CredentialMethods: []kit.CredentialMethod{{
			ID: "native-login", Label: "Existing CLI login", Description: "Use existing auth", Recommended: true, Custody: "executor",
			Setup: &kit.CommandTemplate{Program: binary, Args: []string{"setup"}}, Verify: &kit.CommandTemplate{Program: binary, Args: []string{"verify"}},
		}},
		Modes: []kit.Mode{{ID: "cli", Executor: kit.Executor{Kind: "cli", Binary: binary}, CredentialMethods: []string{"native-login"}, DefaultCredentialMethod: "native-login"}},
	}
	paths := state.Paths{Root: root, Database: root + "/outfit.db", Packages: root + "/packages", Skills: root + "/skills", Locks: root + "/locks", Vault: root + "/vault"}
	store, err := state.Open(ctx, paths)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	raw, _ := json.Marshal(pkg)
	if err := store.InstallPackage(ctx, kit.Loaded{Package: pkg, Raw: raw}, "trusted"); err != nil {
		t.Fatal(err)
	}
	secret, err := vault.Open(paths)
	if err != nil {
		t.Fatal(err)
	}
	service := Service{State: store, Vault: secret, Executor: cwruntime.Executor{}}
	method, identity, ok := service.DetectExecutorCredential(ctx, pkg, pkg.Modes[0])
	if !ok || method != "native-login" || identity["principal"] != "john@example.com" {
		t.Fatalf("detected = %q %#v %t", method, identity, ok)
	}
	configured, err := service.ConfigureWithMethod(ctx, "native", "native", "cli", "native-login", nil)
	if err != nil {
		t.Fatal(err)
	}
	if configured.CredentialMethod != "native-login" || configured.SafeIdentity["principal"] != "john@example.com" {
		t.Fatalf("configured = %#v", configured)
	}
	if _, err := os.Stat(setupCalled); !os.IsNotExist(err) {
		t.Fatal("interactive setup ran despite an authenticated CLI")
	}
}

func TestNativeCredentialRunsLoginOnlyWhenExistingVerificationFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell helper is Unix-only")
	}
	ctx := context.Background()
	root := t.TempDir()
	authenticated := filepath.Join(root, "authenticated")
	setupCalled := filepath.Join(root, "setup-called")
	binary := writeNativeLoginHelper(t, authenticated, setupCalled)
	pkg := kit.Package{
		Key: "native", Version: "1",
		CredentialMethods: []kit.CredentialMethod{{
			ID: "native-login", Label: "Existing CLI login", Description: "Use existing auth", Recommended: true, Custody: "executor",
			Setup: &kit.CommandTemplate{Program: binary, Args: []string{"setup"}}, Verify: &kit.CommandTemplate{Program: binary, Args: []string{"verify"}},
		}},
		Modes: []kit.Mode{{ID: "cli", Executor: kit.Executor{Kind: "cli", Binary: binary}, CredentialMethods: []string{"native-login"}, DefaultCredentialMethod: "native-login"}},
	}
	paths := state.Paths{Root: root, Database: root + "/outfit.db", Packages: root + "/packages", Skills: root + "/skills", Locks: root + "/locks", Vault: root + "/vault"}
	store, err := state.Open(ctx, paths)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	raw, _ := json.Marshal(pkg)
	if err := store.InstallPackage(ctx, kit.Loaded{Package: pkg, Raw: raw}, "trusted"); err != nil {
		t.Fatal(err)
	}
	secret, err := vault.Open(paths)
	if err != nil {
		t.Fatal(err)
	}
	service := Service{State: store, Vault: secret, Executor: cwruntime.Executor{}}
	if _, _, ok := service.DetectExecutorCredential(ctx, pkg, pkg.Modes[0]); ok {
		t.Fatal("unauthenticated CLI was detected")
	}
	if _, err := service.ConfigureWithMethod(ctx, "native", "native", "cli", "native-login", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(setupCalled); err != nil {
		t.Fatalf("interactive setup did not run: %v", err)
	}
}

func TestNativeCredentialCompletesBrowserContinuationBeforeVerification(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell helper is Unix-only")
	}
	ctx := context.Background()
	root := t.TempDir()
	authenticated := filepath.Join(root, "authenticated")
	setupCalled := filepath.Join(root, "setup-called")
	binary := writeBrowserLoginHelper(t, authenticated, setupCalled)
	pkg := kit.Package{
		Key: "native", Version: "1",
		CredentialMethods: []kit.CredentialMethod{{
			ID: "native-login", Label: "Existing CLI login", Description: "Use existing auth", Recommended: true, Custody: "executor",
			Setup: &kit.CommandTemplate{
				Program: binary, Args: []string{"setup"},
				BrowserContinuation: &kit.BrowserContinuation{URLField: "browser_url", VerificationCodeField: "verification_code", NextStepField: "next_step", TimeoutSeconds: 5},
			},
			Verify: &kit.CommandTemplate{Program: binary, Args: []string{"verify"}},
		}},
		Modes: []kit.Mode{{ID: "cli", Executor: kit.Executor{Kind: "cli", Binary: binary}, CredentialMethods: []string{"native-login"}, DefaultCredentialMethod: "native-login"}},
	}
	paths := state.Paths{Root: root, Database: root + "/outfit.db", Packages: root + "/packages", Skills: root + "/skills", Locks: root + "/locks", Vault: root + "/vault"}
	store, err := state.Open(ctx, paths)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	raw, _ := json.Marshal(pkg)
	if err := store.InstallPackage(ctx, kit.Loaded{Package: pkg, Raw: raw}, "trusted"); err != nil {
		t.Fatal(err)
	}
	secret, err := vault.Open(paths)
	if err != nil {
		t.Fatal(err)
	}
	opened := ""
	service := Service{State: store, Vault: secret, Executor: cwruntime.Executor{OpenBrowser: func(target string) error {
		opened = target
		return nil
	}}}
	configured, err := service.ConfigureWithMethod(ctx, "native", "native", "cli", "native-login", nil)
	if err != nil {
		t.Fatal(err)
	}
	if opened != "https://connect.example.test/authorize" || configured.SafeIdentity["principal"] != "john@example.com" {
		t.Fatalf("browser login did not finish before verification: opened=%q configured=%#v", opened, configured)
	}
	if _, err := os.Stat(setupCalled); err != nil {
		t.Fatalf("setup was not attempted: %v", err)
	}
}

func writeNativeLoginHelper(t *testing.T, authenticated, setupCalled string) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "native-login")
	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'" }
	body := fmt.Sprintf("#!/bin/sh\ncase \"$1\" in\nsetup) touch %s %s; printf '{}';;\nverify) if [ -f %s ]; then printf '{\"user\":\"john@example.com\"}'; else echo 'not logged in' >&2; exit 2; fi;;\nesac\n", quote(authenticated), quote(setupCalled), quote(authenticated))
	if err := os.WriteFile(binary, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return binary
}

func writeBrowserLoginHelper(t *testing.T, authenticated, setupCalled string) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "native-login")
	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'" }
	continuation, err := json.Marshal(map[string]string{
		"browser_url": "https://connect.example.test/authorize", "verification_code": "blue-cat", "next_step": "native-login complete",
	})
	if err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf("#!/bin/sh\ncase \"$1\" in\nsetup) touch %s; printf %%s %s;;\ncomplete) touch %s; printf '{}';;\nverify) if [ -f %s ]; then printf '{\"user\":\"john@example.com\"}'; else printf '{\"authenticated\":false}'; exit 2; fi;;\nesac\n", quote(setupCalled), quote(string(continuation)), quote(authenticated), quote(authenticated))
	if err := os.WriteFile(binary, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return binary
}

func TestConfigureSelectsOneModeAndRemovesSupersededSecret(t *testing.T) {
	t.Setenv("OUTFIT_VAULT_BACKEND", "file")
	ctx := context.Background()
	paths := state.Paths{Root: t.TempDir()}
	paths.Database = paths.Root + "/outfit.db"
	paths.Packages, paths.Skills, paths.Locks, paths.Vault = paths.Root+"/packages", paths.Root+"/skills", paths.Root+"/locks", paths.Root+"/vault"
	store, err := state.Open(ctx, paths)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	loaded := kit.Loaded{Package: kit.Package{
		Key: "sample-api", Version: "1",
		CredentialMethods: []kit.CredentialMethod{{ID: "managed-token", Label: "Managed token", Description: "Test token", Recommended: true, Custody: "outfit", Fields: []kit.CredentialField{{Key: "TOKEN", Label: "Token", Secret: true}}}},
		Modes:             []kit.Mode{{ID: "rest", CredentialMethods: []string{"managed-token"}, DefaultCredentialMethod: "managed-token"}},
	}}
	loaded.Raw, _ = json.Marshal(loaded.Package)
	if err := store.InstallPackage(ctx, loaded, "trusted"); err != nil {
		t.Fatal(err)
	}
	secret, err := vault.Open(paths)
	if err != nil {
		t.Fatal(err)
	}
	service := Service{State: store, Vault: secret, Executor: cwruntime.Executor{}}
	first, err := service.Configure(ctx, "work", "sample-api", "rest", map[string]string{"TOKEN": "first"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Configure(ctx, "work", "sample-api", "rest", map[string]string{"TOKEN": "second"})
	if err != nil {
		t.Fatal(err)
	}
	if first.CredentialRef == second.CredentialRef {
		t.Fatal("credential was not rotated")
	}
	if _, err := secret.Get(ctx, first.CredentialRef); err == nil {
		t.Fatal("superseded secret remains readable")
	}
}

func TestReusedCredentialSurvivesUntilItsLastConnectionIsRemoved(t *testing.T) {
	t.Setenv("OUTFIT_VAULT_BACKEND", "file")
	ctx := context.Background()
	root := t.TempDir()
	paths := state.Paths{Root: root, Database: root + "/outfit.db", Packages: root + "/packages", Skills: root + "/skills", Locks: root + "/locks", Vault: root + "/vault"}
	store, err := state.Open(ctx, paths)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer shared-secret" {
			t.Fatalf("wrong shared credential: %q", request.Header.Get("Authorization"))
		}
		_, _ = io.WriteString(w, `{"id":"acct_shared","name":"Shared account"}`)
	}))
	defer server.Close()
	pkg := kit.Package{
		Key: "shared-api", Version: "1",
		CredentialMethods: []kit.CredentialMethod{{ID: "token", Label: "Token", Description: "Shared token", Recommended: true, Custody: "outfit", Fields: []kit.CredentialField{{Key: "TOKEN", Label: "Token", Secret: true}}, Bindings: []kit.CredentialBinding{{Target: "http-header", Name: "Authorization", Value: "Bearer {{secret.TOKEN}}"}}}},
		Modes:             []kit.Mode{{ID: "rest", Executor: kit.Executor{Kind: "http", BaseURL: server.URL + "/", PrivateNet: true}, CredentialMethods: []string{"token"}, DefaultCredentialMethod: "token", Operations: []kit.Operation{{Name: "account", Class: "read", HTTP: &kit.HTTPOperation{Method: "GET", Path: "/account"}}}, Verify: "account"}},
	}
	raw, _ := json.Marshal(pkg)
	if err := store.InstallPackage(ctx, kit.Loaded{Package: pkg, Raw: raw}, "trusted"); err != nil {
		t.Fatal(err)
	}
	secret, _ := vault.Open(paths)
	service := Service{State: store, Vault: secret, Executor: cwruntime.Executor{HTTPClient: server.Client()}}
	first, err := service.ConfigureWithMethod(ctx, "first", "shared-api", "rest", "token", map[string]string{"TOKEN": "shared-secret"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.ReuseCredential(ctx, first.CredentialID, "second", "rest", nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.CredentialID != second.CredentialID || first.CredentialRef != second.CredentialRef {
		t.Fatalf("credential was duplicated: %#v %#v", first, second)
	}
	if err := service.Remove(ctx, "first"); err != nil {
		t.Fatal(err)
	}
	if _, err := secret.Get(ctx, second.CredentialRef); err != nil {
		t.Fatalf("shared credential was destroyed with its first consumer: %v", err)
	}
	if _, err := service.Run(ctx, "second", "account", nil, false); err != nil {
		t.Fatalf("remaining consumer stopped working: %v", err)
	}
	if err := service.Remove(ctx, "second"); err != nil {
		t.Fatal(err)
	}
	if _, err := secret.Get(ctx, second.CredentialRef); err == nil {
		t.Fatal("last-consumer removal left the credential readable")
	}
}

func TestSuccessfulReadEntersOptInCacheWithProvenance(t *testing.T) {
	t.Setenv("OUTFIT_VAULT_BACKEND", "file")
	ctx := context.Background()
	root := t.TempDir()
	paths := state.Paths{Root: root, Database: root + "/outfit.db", Packages: root + "/packages", Skills: root + "/skills", Locks: root + "/locks", Vault: root + "/vault"}
	store, err := state.Open(ctx, paths)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{"title":"cached alpha"}`) }))
	defer server.Close()
	pkg := kit.Package{
		Key: "sample-api", Version: "1",
		CredentialMethods: []kit.CredentialMethod{{ID: "no-auth", Label: "No auth", Description: "Public test API", Recommended: true, Custody: "none"}},
		Modes: []kit.Mode{{
			ID: "rest", Executor: kit.Executor{Kind: "http", BaseURL: server.URL + "/", PrivateNet: true},
			CredentialMethods: []string{"no-auth"}, DefaultCredentialMethod: "no-auth",
			Operations: []kit.Operation{{
				Name: "read_item", Title: "Read item", Class: "read",
				Input: map[string]any{"type": "object", "properties": map[string]any{"id": map[string]any{"type": "string"}}, "required": []string{"id"}, "additionalProperties": false},
				HTTP:  &kit.HTTPOperation{Method: "GET", Path: "/items/:id"},
			}},
		}},
	}
	rawPackage, _ := json.Marshal(pkg)
	if err := store.InstallPackage(ctx, kit.Loaded{Package: pkg, Raw: rawPackage}, "trusted"); err != nil {
		t.Fatal(err)
	}
	secret, _ := vault.Open(paths)
	ref, err := secret.Put(ctx, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateConnection(ctx, state.Connection{ID: "c1", Name: "work", ConnectorKey: "sample-api", SelectedMode: "rest", CredentialMethod: "no-auth", Status: "active", CredentialRef: ref, SafeIdentity: map[string]string{}}); err != nil {
		t.Fatal(err)
	}
	if err := store.Setting(ctx, "cache.config", state.CacheConfig{Enabled: true, TTLSeconds: 3600}); err != nil {
		t.Fatal(err)
	}
	service := Service{State: store, Vault: secret, Executor: cwruntime.Executor{HTTPClient: server.Client()}}
	if _, err := service.Run(ctx, "work", "read_item", map[string]string{"id": "alpha"}, false); err != nil {
		t.Fatal(err)
	}
	items, err := store.SearchContexts(ctx, "alpha", "work", time.Now(), false, 10)
	if err != nil || len(items) != 1 || items[0].Arguments["id"] != "alpha" || items[0].Provenance["kind"] != "operation" {
		t.Fatalf("cache result: %#v %v", items, err)
	}
}

func TestConfigureRejectsPathLikeConnectionName(t *testing.T) {
	service := Service{}
	if _, err := service.Configure(context.Background(), "../escape", "sample-api", "rest", nil); err == nil {
		t.Fatal("path-like connection name accepted")
	}
}

func TestWorkspacePolicyBlocksAdministrativeAccessToOtherConnections(t *testing.T) {
	ctx := context.Background()
	service := Service{Allowed: func(name string) bool { return name == "allowed" }}
	if err := service.Remove(ctx, "other"); err == nil || !strings.Contains(err.Error(), "not enabled") {
		t.Fatalf("remove escaped workspace policy: %v", err)
	}
	if err := service.Verify(ctx, "other"); err == nil || !strings.Contains(err.Error(), "not enabled") {
		t.Fatalf("verify escaped workspace policy: %v", err)
	}
}

func TestConfigureHonorsWorkspaceConnectionAndModeDeclarationBeforeStorage(t *testing.T) {
	service := Service{AllowedConfiguration: func(name, packageKey, mode string) bool {
		return name == "work" && packageKey == "github" && mode == "cli"
	}}
	if _, err := service.Configure(context.Background(), "work", "github", "rest", nil); err == nil {
		t.Fatal("undeclared workspace mode reached configuration")
	}
}

func TestConfigureRefusesUntrustedPackageBeforeSecretStorage(t *testing.T) {
	t.Setenv("OUTFIT_VAULT_BACKEND", "file")
	ctx := context.Background()
	root := t.TempDir()
	paths := state.Paths{Root: root, Database: root + "/outfit.db", Packages: root + "/packages", Skills: root + "/skills", Locks: root + "/locks", Vault: root + "/vault"}
	store, err := state.Open(ctx, paths)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	loaded := kit.Loaded{Package: kit.Package{
		Key: "sample-api", Version: "1",
		CredentialMethods: []kit.CredentialMethod{{ID: "managed-token", Label: "Managed token", Description: "Test token", Recommended: true, Custody: "outfit", Fields: []kit.CredentialField{{Key: "TOKEN", Label: "Token", Secret: true}}}},
		Modes:             []kit.Mode{{ID: "rest", CredentialMethods: []string{"managed-token"}, DefaultCredentialMethod: "managed-token"}},
	}}
	loaded.Raw, _ = json.Marshal(loaded.Package)
	if err := store.InstallPackage(ctx, loaded, "untrusted"); err != nil {
		t.Fatal(err)
	}
	secret, _ := vault.Open(paths)
	service := Service{State: store, Vault: secret}
	if _, err := service.Configure(ctx, "work", "sample-api", "rest", map[string]string{"TOKEN": "secret"}); err == nil {
		t.Fatal("untrusted package configured")
	}
}

type refusingVault struct{ reads int }

func (vault *refusingVault) Put(context.Context, map[string]string) (string, error) { return "", nil }
func (vault *refusingVault) Get(context.Context, string) (map[string]string, error) {
	vault.reads++
	return nil, context.Canceled
}
func (vault *refusingVault) Delete(context.Context, string) error { return nil }

func TestPlanDoesNotReadVaultOrExecute(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := state.Open(ctx, state.Paths{Root: root, Database: root + "/outfit.db", Packages: root + "/packages", Skills: root + "/skills", Locks: root + "/locks", Vault: root + "/vault"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	pkg := kit.Package{
		Key: "sample-api", Version: "1",
		CredentialMethods: []kit.CredentialMethod{{ID: "no-auth", Label: "No auth", Description: "Public test API", Recommended: true, Custody: "none"}},
		Modes: []kit.Mode{{
			ID: "rest", Executor: kit.Executor{Kind: "http", BaseURL: "https://api.example.test/"},
			CredentialMethods: []string{"no-auth"}, DefaultCredentialMethod: "no-auth",
			Operations: []kit.Operation{{Name: "get_item", Class: "read", HTTP: &kit.HTTPOperation{Method: "GET", Path: "/items/:id"}}},
		}},
	}
	raw, _ := json.Marshal(pkg)
	if err := store.InstallPackage(ctx, kit.Loaded{Package: pkg, Digest: "digest", Raw: raw}, "trusted"); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateConnection(ctx, state.Connection{ID: "c1", Name: "work", ConnectorKey: "sample-api", SelectedMode: "rest", CredentialMethod: "no-auth", Status: "active", CredentialRef: "must-not-read", SafeIdentity: map[string]string{}}); err != nil {
		t.Fatal(err)
	}
	secrets := &refusingVault{}
	plan, err := (Service{State: store, Vault: secrets}).Plan(ctx, "work", "get_item", map[string]string{"id": "1"}, false)
	if err != nil || plan.Runtime.Path != "/items/1" || secrets.reads != 0 {
		t.Fatalf("dry plan touched vault or diverged: %#v reads=%d err=%v", plan, secrets.reads, err)
	}
	explained, err := (Service{State: store, Vault: secrets, AllowedOperation: func(string, string) bool { return false }}).Explain(ctx, "work", "get_item", map[string]string{"id": "1"}, false)
	if err != nil || explained.Policy != "denied: operation is not enabled by this project" || secrets.reads != 0 {
		t.Fatalf("explain did not expose denial safely: %#v reads=%d err=%v", explained, secrets.reads, err)
	}
}

func TestOperationCapabilitiesFollowSelectedConnectionServices(t *testing.T) {
	mode := kit.Mode{Capabilities: []string{"gmail", "drive"}}
	connection := state.Connection{SafeIdentity: map[string]string{"capabilities": "gmail"}}
	if !operationCapabilitiesAllowed(connection, mode, kit.Operation{Capabilities: []string{"gmail"}}) {
		t.Fatal("selected Gmail operation was refused")
	}
	if operationCapabilitiesAllowed(connection, mode, kit.Operation{Capabilities: []string{"drive"}}) {
		t.Fatal("unselected Drive operation was allowed")
	}
	if !operationCapabilitiesAllowed(state.Connection{SafeIdentity: map[string]string{}}, mode, kit.Operation{Capabilities: []string{"drive"}}) {
		t.Fatal("mode defaults did not apply when no capability selection exists")
	}
}
