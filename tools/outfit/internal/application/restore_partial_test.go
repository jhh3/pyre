package application

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jhh3/outfit/internal/backup"
	"github.com/jhh3/outfit/internal/connection"
	"github.com/jhh3/outfit/internal/kit"
	"github.com/jhh3/outfit/internal/runtime"
	"github.com/jhh3/outfit/internal/skillmanager"
	"github.com/jhh3/outfit/internal/state"
	"github.com/jhh3/outfit/internal/toolchain"
	"github.com/jhh3/outfit/internal/vault"
)

type restoreFixture struct {
	t       *testing.T
	server  *httptest.Server
	rejectB atomic.Bool // the provider refuses token-b (an expired account)
	pkg     kit.Package
}

func newRestoreFixture(t *testing.T) *restoreFixture {
	t.Setenv("OUTFIT_VAULT_BACKEND", "file")
	fixture := &restoreFixture{t: t}
	fixture.server = httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.Header.Get("Authorization") {
		case "Bearer token-a":
		case "Bearer token-b":
			if fixture.rejectB.Load() {
				http.Error(writer, "unauthorized", http.StatusUnauthorized)
				return
			}
		default:
			http.Error(writer, "unauthorized", http.StatusUnauthorized)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"id":"acct_synthetic","name":"Synthetic account"}`)
	}))
	t.Cleanup(fixture.server.Close)
	fixture.pkg = kit.Package{APIVersion: kit.APIVersion, Key: "restore-test", Display: "Restore Test", Version: "1", CredentialMethods: []kit.CredentialMethod{{ID: "token", Label: "Managed token", Description: "Managed token", Recommended: true, Custody: "outfit", Fields: []kit.CredentialField{{Key: "TOKEN", Label: "Token", Secret: true}}, Bindings: []kit.CredentialBinding{{Target: "http-header", Name: "Authorization", Value: "Bearer {{secret.TOKEN}}"}}}}, Modes: []kit.Mode{{ID: "rest", Label: "REST", Description: "REST", Recommendation: kit.Recommendation{Rank: 1, Reason: "Test"}, Executor: kit.Executor{Kind: "http", BaseURL: fixture.server.URL + "/"}, CredentialMethods: []string{"token"}, DefaultCredentialMethod: "token", Operations: []kit.Operation{{Name: "account", Title: "Account", Description: "Account", Class: "read", HTTP: &kit.HTTPOperation{Method: "GET", Path: "/account"}}}, Verify: "account"}}, Skill: kit.Skill{Summary: "Restore test"}}
	return fixture
}

func (fixture *restoreFixture) service(root string) Service {
	t := fixture.t
	ctx := context.Background()
	paths := state.Paths{Root: root, Database: filepath.Join(root, "outfit.db"), Packages: filepath.Join(root, "packages"), Skills: filepath.Join(root, "skills"), Locks: filepath.Join(root, "locks"), Vault: filepath.Join(root, "vault")}
	store, err := state.Open(ctx, paths)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	secrets, err := vault.Open(paths)
	if err != nil {
		t.Fatal(err)
	}
	connections := connection.Service{State: store, Vault: secrets, Executor: runtime.Executor{HTTPClient: fixture.server.Client()}}
	return Service{State: store, Vault: secrets, Connections: connections, Tools: toolchain.Manager{Paths: paths}, Skills: skillmanager.Manager{Paths: paths, Roots: map[string]string{"codex": filepath.Join(root, "codex-skills"), "claude": filepath.Join(root, "claude-skills")}}, Host: "both"}
}

func (fixture *restoreFixture) install(service Service) {
	raw, _ := json.Marshal(fixture.pkg)
	if err := service.State.InstallPackage(context.Background(), kit.Loaded{Package: fixture.pkg, Raw: raw, Digest: "digest", Source: "test"}, "trusted"); err != nil {
		fixture.t.Fatal(err)
	}
}

func (fixture *restoreFixture) connect(service Service, name, token string) {
	if _, err := service.ApplySetup(context.Background(), SetupRequest{Name: name, Connector: fixture.pkg.Key, Mode: "rest", CredentialMethod: "token", Fields: map[string]string{"TOKEN": token}}, nil); err != nil {
		fixture.t.Fatal(err)
	}
}

const restorePassphrase = "synthetic restore passphrase"

// One connection that cannot be restored no longer rolls back the others,
// and leaves no hidden row behind that blocks its name on the next try.
func TestRestoreKeepsGoodConnectionsWhenOneFails(t *testing.T) {
	ctx := context.Background()
	fixture := newRestoreFixture(t)
	source := fixture.service(t.TempDir())
	fixture.install(source)
	fixture.connect(source, "alpha", "token-a")
	fixture.connect(source, "bravo", "token-b")
	backupPath := filepath.Join(t.TempDir(), "fleet.outfit")
	if _, err := source.CreateBackup(ctx, backupPath, restorePassphrase, backup.BuildOptions{}); err != nil {
		t.Fatal(err)
	}

	target := fixture.service(t.TempDir())
	fixture.rejectB.Store(true)
	err := target.RestoreBackup(ctx, backupPath, restorePassphrase, false, nil)
	var incomplete *RestoreIncompleteError
	if !errors.As(err, &incomplete) || len(incomplete.Failures) != 1 || incomplete.Failures[0].Name != "bravo" || len(incomplete.Restored) != 1 || incomplete.Restored[0] != "alpha" {
		t.Fatalf("restore error = %v", err)
	}
	if got, err := target.State.Connection(ctx, "alpha"); err != nil || got.Status != "active" {
		t.Fatalf("alpha after partial restore: %#v %v", got, err)
	}
	if got, err := target.State.Connection(ctx, "bravo"); !state.IsNotFound(err) {
		t.Fatalf("failed connection left a row behind: %#v %v", got, err)
	}

	// The provider recovers: restoring just the failed one works, and its
	// name is free.
	fixture.rejectB.Store(false)
	if err := target.RestoreBackupSelected(ctx, backupPath, restorePassphrase, false, []string{"bravo"}, nil); err != nil {
		t.Fatalf("retry restore: %v", err)
	}
	if got, err := target.State.Connection(ctx, "bravo"); err != nil || got.Status != "active" {
		t.Fatalf("bravo after retry: %#v %v", got, err)
	}
}

// A connection removed earlier (status "removed", as an old rollback left
// github on win-home) must not make restore say it already exists.
func TestRestoreIgnoresRemovedConnectionRows(t *testing.T) {
	ctx := context.Background()
	fixture := newRestoreFixture(t)
	source := fixture.service(t.TempDir())
	fixture.install(source)
	fixture.connect(source, "alpha", "token-a")
	backupPath := filepath.Join(t.TempDir(), "fleet.outfit")
	if _, err := source.CreateBackup(ctx, backupPath, restorePassphrase, backup.BuildOptions{}); err != nil {
		t.Fatal(err)
	}
	target := fixture.service(t.TempDir())
	fixture.install(target)
	fixture.connect(target, "alpha", "token-a")
	if err := target.Connections.Remove(ctx, "alpha"); err != nil {
		t.Fatal(err)
	}
	if got, err := target.State.Connection(ctx, "alpha"); err != nil || got.Status != "removed" {
		t.Fatalf("setup: expected a removed row, got %#v %v", got, err)
	}
	if err := target.RestoreBackup(ctx, backupPath, restorePassphrase, false, nil); err != nil {
		t.Fatalf("restore over a removed row: %v", err)
	}
	if got, err := target.State.Connection(ctx, "alpha"); err != nil || got.Status != "active" {
		t.Fatalf("alpha after restore: %#v %v", got, err)
	}
}

// A replaced connection that fails to restore keeps the local one intact.
func TestRestoreReplaceKeepsPriorConnectionWhenItsRestoreFails(t *testing.T) {
	ctx := context.Background()
	fixture := newRestoreFixture(t)
	source := fixture.service(t.TempDir())
	fixture.install(source)
	fixture.connect(source, "bravo", "token-b")
	backupPath := filepath.Join(t.TempDir(), "fleet.outfit")
	if _, err := source.CreateBackup(ctx, backupPath, restorePassphrase, backup.BuildOptions{}); err != nil {
		t.Fatal(err)
	}
	target := fixture.service(t.TempDir())
	fixture.install(target)
	fixture.connect(target, "bravo", "token-a")
	before, _ := target.State.Connection(ctx, "bravo")
	fixture.rejectB.Store(true)
	var incomplete *RestoreIncompleteError
	if err := target.RestoreBackup(ctx, backupPath, restorePassphrase, true, nil); !errors.As(err, &incomplete) || incomplete.Failures[0].RevertErr != nil {
		t.Fatalf("restore error = %v", err)
	}
	after, err := target.State.Connection(ctx, "bravo")
	if err != nil || after.Status != "active" || after.CredentialRef != before.CredentialRef {
		t.Fatalf("prior connection changed: before %#v after %#v %v", before, after, err)
	}
}

// Built-in packages the target installs itself are skipped before
// validation, so an older or JSON-mangled copy in the backup cannot fail it.
func TestRestoreSkipsBuiltinPackagesTheTargetAlreadyHas(t *testing.T) {
	ctx := context.Background()
	fixture := newRestoreFixture(t)
	target := fixture.service(t.TempDir())
	fixture.install(target)
	invalid := fixture.pkg
	invalid.Modes = nil // would fail kit.Validate
	backupPath := filepath.Join(t.TempDir(), "builtin.outfit")
	if err := backup.Write(backupPath, restorePassphrase, backup.Payload{Packages: []backup.Package{{Manifest: invalid, Trust: "builtin", Source: "builtin"}}}); err != nil {
		t.Fatal(err)
	}
	if err := target.RestoreBackup(ctx, backupPath, restorePassphrase, false, nil); err != nil {
		t.Fatalf("restore with a built-in package copy: %v", err)
	}
	installed, err := target.State.Package(ctx, fixture.pkg.Key)
	if err != nil || len(installed.Package.Modes) != 1 {
		t.Fatalf("target package was replaced: %#v %v", installed.Package.Modes, err)
	}
}

// flakyVault fails Put after allowPuts successful ones (a keychain that
// starts refusing writes).
type flakyVault struct {
	vault.Store
	allowPuts int
	puts      int
}

func (v *flakyVault) Put(ctx context.Context, value map[string]string) (string, error) {
	v.puts++
	if v.puts > v.allowPuts {
		return "", &vault.WriteError{Err: errors.New("synthetic: user interaction is not allowed")}
	}
	return v.Store.Put(ctx, value)
}

func withVault(service Service, store vault.Store) Service {
	service.Vault = store
	service.Connections.Vault = store
	return service
}

func (fixture *restoreFixture) threeConnectionBackup(t *testing.T) string {
	source := fixture.service(t.TempDir())
	fixture.install(source)
	fixture.connect(source, "alpha", "token-a")
	fixture.connect(source, "bravo", "token-a")
	fixture.connect(source, "charlie", "token-a")
	backupPath := filepath.Join(t.TempDir(), "fleet.outfit")
	if _, err := source.CreateBackup(context.Background(), backupPath, restorePassphrase, backup.BuildOptions{}); err != nil {
		t.Fatal(err)
	}
	return backupPath
}

// A vault that cannot store secrets is found before any account is
// verified (or an OAuth grant refreshed at the provider).
func TestRestoreProbesTheVaultBeforeTouchingAnything(t *testing.T) {
	ctx := context.Background()
	fixture := newRestoreFixture(t)
	backupPath := fixture.threeConnectionBackup(t)
	var requests atomic.Int32
	inner := fixture.server.Config.Handler
	fixture.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); inner.ServeHTTP(w, r) })
	target := fixture.service(t.TempDir())
	target = withVault(target, &flakyVault{Store: target.Vault})
	err := target.RestoreBackup(ctx, backupPath, restorePassphrase, false, nil)
	if err == nil || !strings.Contains(err.Error(), "nothing was restored") {
		t.Fatalf("restore with a refusing vault = %v", err)
	}
	if requests.Load() != 0 {
		t.Fatalf("%d provider requests before the vault probe failed", requests.Load())
	}
	if packages, _ := target.State.Packages(ctx); len(packages) != 0 {
		t.Fatalf("packages installed before the vault probe failed: %d", len(packages))
	}
}

// A vault failure in the middle stops the restore: the rest is not
// attempted (not verified, not refreshed).
func TestRestoreStopsOnAVaultWriteFailure(t *testing.T) {
	ctx := context.Background()
	fixture := newRestoreFixture(t)
	backupPath := fixture.threeConnectionBackup(t)
	target := fixture.service(t.TempDir())
	target = withVault(target, &flakyVault{Store: target.Vault, allowPuts: 2}) // the probe and alpha
	err := target.RestoreBackup(ctx, backupPath, restorePassphrase, false, nil)
	var incomplete *RestoreIncompleteError
	if !errors.As(err, &incomplete) || len(incomplete.Restored) != 1 || len(incomplete.Failures) != 1 || incomplete.Failures[0].Name != "bravo" || len(incomplete.NotAttempted) != 1 || incomplete.NotAttempted[0] != "charlie" {
		t.Fatalf("restore error = %v (%+v)", err, incomplete)
	}
	if _, err := target.State.Connection(ctx, "charlie"); !state.IsNotFound(err) {
		t.Fatalf("charlie was attempted: %v", err)
	}
}

// Cancellation stops the loop, and putting back the interrupted connection
// still works (it runs on a fresh context).
func TestRestoreCancellationStopsAndRevertsCleanly(t *testing.T) {
	fixture := newRestoreFixture(t)
	backupPath := fixture.threeConnectionBackup(t)
	target := fixture.service(t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	progress := func(stage, detail string) {
		if stage == "connection" && detail == "Restoring bravo" {
			cancel()
		}
	}
	err := target.RestoreBackup(ctx, backupPath, restorePassphrase, false, progress)
	var incomplete *RestoreIncompleteError
	if !errors.As(err, &incomplete) || len(incomplete.Restored) != 1 || incomplete.Restored[0] != "alpha" || len(incomplete.NotAttempted) == 0 {
		t.Fatalf("cancelled restore = %v (%+v)", err, incomplete)
	}
	for _, failure := range incomplete.Failures {
		if failure.RevertErr != nil {
			t.Fatalf("revert after cancel failed: %v", failure.RevertErr)
		}
	}
	for _, name := range []string{"bravo", "charlie"} {
		if _, err := target.State.Connection(context.Background(), name); !state.IsNotFound(err) {
			t.Fatalf("%s left behind after the cancelled restore: %v", name, err)
		}
	}
}

// --replace over connections that share one credential in the backup.
func TestRestoreReplaceHandlesSharedCredentials(t *testing.T) {
	ctx := context.Background()
	fixture := newRestoreFixture(t)
	source := fixture.service(t.TempDir())
	fixture.install(source)
	fixture.connect(source, "alpha", "token-a")
	shared, _ := source.State.Connection(ctx, "alpha")
	if _, err := source.ReuseCredential(ctx, shared.CredentialID, "alpha-second", "rest", nil); err != nil {
		t.Fatal(err)
	}
	backupPath := filepath.Join(t.TempDir(), "fleet.outfit")
	if _, err := source.CreateBackup(ctx, backupPath, restorePassphrase, backup.BuildOptions{}); err != nil {
		t.Fatal(err)
	}
	target := fixture.service(t.TempDir())
	if err := target.RestoreBackup(ctx, backupPath, restorePassphrase, false, nil); err != nil {
		t.Fatalf("first restore: %v", err)
	}
	if err := target.RestoreBackup(ctx, backupPath, restorePassphrase, true, nil); err != nil {
		t.Fatalf("restore --replace over shared credentials: %v", err)
	}
	for _, name := range []string{"alpha", "alpha-second"} {
		if got, err := target.State.Connection(ctx, name); err != nil || got.Status != "active" {
			t.Fatalf("%s: %#v %v", name, got, err)
		}
	}
}

// After a committed --replace the superseded secret is destroyed; until
// then it is kept so a failure could switch back to it.
func TestRestoreReplaceDestroysTheOldSecretOnlyAfterCommit(t *testing.T) {
	ctx := context.Background()
	fixture := newRestoreFixture(t)
	source := fixture.service(t.TempDir())
	fixture.install(source)
	fixture.connect(source, "alpha", "token-a")
	backupPath := filepath.Join(t.TempDir(), "fleet.outfit")
	if _, err := source.CreateBackup(ctx, backupPath, restorePassphrase, backup.BuildOptions{}); err != nil {
		t.Fatal(err)
	}
	target := fixture.service(t.TempDir())
	fixture.install(target)
	fixture.connect(target, "alpha", "token-a")
	before, _ := target.State.Connection(ctx, "alpha")
	if err := target.RestoreBackup(ctx, backupPath, restorePassphrase, true, nil); err != nil {
		t.Fatal(err)
	}
	after, _ := target.State.Connection(ctx, "alpha")
	if after.CredentialRef == before.CredentialRef {
		t.Fatal("replace did not switch the credential")
	}
	if _, err := target.Vault.Get(ctx, before.CredentialRef); !vault.IsNotFound(err) {
		t.Fatalf("superseded secret still stored after commit: %v", err)
	}
	if cleanups, _ := target.State.Cleanups(ctx); len(cleanups) != 0 {
		t.Fatalf("cleanups left pending: %+v", cleanups)
	}
}
