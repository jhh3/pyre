package application

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/jhh3/outfit/internal/connection"
	"github.com/jhh3/outfit/internal/kit"
	"github.com/jhh3/outfit/internal/runtime"
	"github.com/jhh3/outfit/internal/skillmanager"
	"github.com/jhh3/outfit/internal/state"
	"github.com/jhh3/outfit/internal/toolchain"
	"github.com/jhh3/outfit/internal/vault"
)

func TestPortableBackupRestoresCredentialAndRuntimeIntoFreshHome(t *testing.T) {
	t.Setenv("OUTFIT_VAULT_BACKEND", "file")
	ctx := context.Background()
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer restore-secret" {
			http.Error(writer, "unauthorized", http.StatusUnauthorized)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"id":"acct_restored","name":"Restored account"}`)
	}))
	defer server.Close()
	pkg := kit.Package{APIVersion: kit.APIVersion, Key: "restore-test", Display: "Restore Test", Version: "1", CredentialMethods: []kit.CredentialMethod{{ID: "token", Label: "Managed token", Description: "Managed token", Recommended: true, Custody: "outfit", Fields: []kit.CredentialField{{Key: "TOKEN", Label: "Token", Secret: true}, {Key: "REGION", Label: "Region"}}, Bindings: []kit.CredentialBinding{{Target: "http-header", Name: "Authorization", Value: "Bearer {{secret.TOKEN}}"}}}}, Modes: []kit.Mode{{ID: "rest", Label: "REST", Description: "REST", Recommendation: kit.Recommendation{Rank: 1, Reason: "Test"}, Executor: kit.Executor{Kind: "http", BaseURL: server.URL + "/"}, CredentialMethods: []string{"token"}, DefaultCredentialMethod: "token", Operations: []kit.Operation{{Name: "account", Title: "Account", Description: "Account", Class: "read", HTTP: &kit.HTTPOperation{Method: "GET", Path: "/account"}}}, Verify: "account"}}, Skill: kit.Skill{Summary: "Restore test"}}

	makeService := func(root string) (Service, func()) {
		paths := state.Paths{Root: root, Database: filepath.Join(root, "outfit.db"), Packages: filepath.Join(root, "packages"), Skills: filepath.Join(root, "skills"), Locks: filepath.Join(root, "locks"), Vault: filepath.Join(root, "vault")}
		store, err := state.Open(ctx, paths)
		if err != nil {
			t.Fatal(err)
		}
		secrets, err := vault.Open(paths)
		if err != nil {
			t.Fatal(err)
		}
		connections := connection.Service{State: store, Vault: secrets, Executor: runtime.Executor{HTTPClient: server.Client()}}
		service := Service{State: store, Vault: secrets, Connections: connections, Tools: toolchain.Manager{Paths: paths}, Skills: skillmanager.Manager{Paths: paths, Roots: map[string]string{"codex": filepath.Join(root, "codex-skills"), "claude": filepath.Join(root, "claude-skills")}}, Host: "both"}
		return service, func() { _ = store.Close() }
	}

	source, closeSource := makeService(t.TempDir())
	raw, _ := json.Marshal(pkg)
	if err := source.State.InstallPackage(ctx, kit.Loaded{Package: pkg, Raw: raw, Digest: "digest", Source: "test"}, "trusted"); err != nil {
		t.Fatal(err)
	}
	if _, err := source.ApplySetup(ctx, SetupRequest{Name: "restored", Connector: pkg.Key, Mode: "rest", CredentialMethod: "token", Fields: map[string]string{"TOKEN": "restore-secret", "REGION": "us-test-1"}}, nil); err != nil {
		t.Fatal(err)
	}
	backupPath := filepath.Join(t.TempDir(), "portable.outfit")
	if err := source.CreateBackup(ctx, backupPath, "correct horse battery staple"); err != nil {
		t.Fatal(err)
	}
	closeSource()

	target, closeTarget := makeService(t.TempDir())
	defer closeTarget()
	if err := target.RestoreBackup(ctx, backupPath, "wrong passphrase", false, nil); err == nil {
		t.Fatal("wrong passphrase restored a backup")
	}
	connectionsBefore, err := target.State.Connections(ctx)
	if err != nil {
		t.Fatal(err)
	}
	credentialsBefore, err := target.State.Credentials(ctx)
	if err != nil {
		t.Fatal(err)
	}
	packagesBefore, err := target.State.Packages(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(connectionsBefore) != 0 || len(credentialsBefore) != 0 || len(packagesBefore) != 0 {
		t.Fatalf("failed restore mutated state: connections=%d credentials=%d packages=%d", len(connectionsBefore), len(credentialsBefore), len(packagesBefore))
	}
	if err := target.RestoreBackup(ctx, backupPath, "correct horse battery staple", false, nil); err != nil {
		t.Fatal(err)
	}
	connection, err := target.State.Connection(ctx, "restored")
	if err != nil || connection.Status != "active" || connection.CredentialID == "" || connection.SafeIdentity["setting.REGION"] != "us-test-1" {
		t.Fatalf("restored connection: %#v %v", connection, err)
	}
	credentials, err := target.State.Credentials(ctx)
	if err != nil || len(credentials) != 1 || credentials[0].Fingerprint == "" {
		t.Fatalf("restored credential record: %#v %v", credentials, err)
	}
	result, err := target.Connections.Run(ctx, "restored", "account", nil, false)
	if err != nil || result.Status != http.StatusOK {
		t.Fatalf("restored runtime: %#v %v", result, err)
	}
}
