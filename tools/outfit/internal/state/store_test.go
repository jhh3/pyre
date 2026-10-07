package state

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhh3/outfit/internal/kit"
)

var concurrentFixturePackage = kit.Loaded{
	Package: kit.Package{APIVersion: kit.APIVersion, Key: "concurrent", Display: "Concurrent", Version: "1"},
	Digest:  "fixture-digest",
	Source:  "builtin/concurrent.yaml",
	Raw:     []byte(`{"apiVersion":"outfit.jhh3.dev/connector/v1alpha2","key":"concurrent","display":"Concurrent","version":"1"}`),
}

func TestConcurrentStoreProcessHelper(t *testing.T) {
	if os.Getenv("OUTFIT_STORE_HELPER") != "1" {
		t.Skip("subprocess helper")
	}
	root := os.Getenv("OUTFIT_STORE_ROOT")
	paths := Paths{Root: root, Database: filepath.Join(root, "outfit.db"), Packages: filepath.Join(root, "packages"), Skills: filepath.Join(root, "skills"), Locks: filepath.Join(root, "locks"), Vault: filepath.Join(root, "vault")}
	store, err := Open(context.Background(), paths)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.InstallPackage(context.Background(), concurrentFixturePackage, "builtin"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Connections(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentProcessesCanOpenAndReadOneStore(t *testing.T) {
	root := t.TempDir()
	paths := Paths{Root: root, Database: filepath.Join(root, "outfit.db"), Packages: filepath.Join(root, "packages"), Skills: filepath.Join(root, "skills"), Locks: filepath.Join(root, "locks"), Vault: filepath.Join(root, "vault")}
	store, err := Open(context.Background(), paths)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.InstallPackage(context.Background(), concurrentFixturePackage, "builtin"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	runConcurrentStoreHelpers(t, root, 24)
}

func TestConcurrentProcessesCanInitializeOneStore(t *testing.T) {
	runConcurrentStoreHelpers(t, t.TempDir(), 24)
}

func runConcurrentStoreHelpers(t *testing.T, root string, processes int) {
	t.Helper()
	errors := make(chan error, processes)
	var group sync.WaitGroup
	for range processes {
		group.Add(1)
		go func() {
			defer group.Done()
			command := exec.Command(os.Args[0], "-test.run=^TestConcurrentStoreProcessHelper$")
			command.Env = append(os.Environ(), "OUTFIT_STORE_HELPER=1", "OUTFIT_STORE_ROOT="+root)
			if output, err := command.CombinedOutput(); err != nil {
				errors <- fmt.Errorf("subprocess: %w\n%s", err, output)
			}
		}()
	}
	group.Wait()
	close(errors)
	for err := range errors {
		t.Error(err)
	}
}

func TestIdenticalPackageInstallDoesNotWrite(t *testing.T) {
	root := t.TempDir()
	paths := Paths{Root: root, Database: filepath.Join(root, "outfit.db"), Packages: filepath.Join(root, "packages"), Skills: filepath.Join(root, "skills"), Locks: filepath.Join(root, "locks"), Vault: filepath.Join(root, "vault")}
	store, err := Open(context.Background(), paths)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.InstallPackage(context.Background(), concurrentFixturePackage, "builtin"); err != nil {
		t.Fatal(err)
	}
	var before, after int64
	if err := store.db.QueryRow(`SELECT total_changes()`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := store.InstallPackage(context.Background(), concurrentFixturePackage, "builtin"); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT total_changes()`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("identical install changed sqlite state: before=%d after=%d", before, after)
	}
}

func TestStorePersistsPackageAndConnection(t *testing.T) {
	root := t.TempDir()
	paths := Paths{Root: root, Database: filepath.Join(root, "outfit.db"), Packages: filepath.Join(root, "packages"), Skills: filepath.Join(root, "skills"), Locks: filepath.Join(root, "locks"), Vault: filepath.Join(root, "vault")}
	store, err := Open(context.Background(), paths)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	pkg := kit.Package{APIVersion: kit.APIVersion, Key: "acme", Display: "Acme", Version: "1", CredentialMethods: []kit.CredentialMethod{{ID: "no-auth", Label: "No auth", Description: "Public API", Recommended: true, Custody: "none"}}, Modes: []kit.Mode{{ID: "api", Label: "API", Description: "API", Recommendation: kit.Recommendation{Rank: 1, Reason: "Public API"}, Executor: kit.Executor{Kind: "http", BaseURL: "https://api.example.com/"}, CredentialMethods: []string{"no-auth"}, DefaultCredentialMethod: "no-auth"}}, Skill: kit.Skill{Summary: "Acme"}}
	loaded := kit.Loaded{Package: pkg, Digest: "digest", Source: "builtin", Raw: []byte(`{"apiVersion":"outfit.jhh3.dev/connector/v1alpha2","key":"acme","display":"Acme","version":"1"}`)}
	if err := store.InstallPackage(context.Background(), loaded, "builtin"); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateConnection(context.Background(), Connection{ID: "c1", Name: "acme-work", ConnectorKey: "acme", SelectedMode: "api", CredentialMethod: "no-auth", Status: "active", SafeIdentity: map[string]string{"account": "work"}}); err != nil {
		t.Fatal(err)
	}
	connection, err := store.Connection(context.Background(), "acme-work")
	if err != nil {
		t.Fatal(err)
	}
	if connection.SelectedMode != "api" || connection.CredentialMethod != "no-auth" || connection.SafeIdentity["account"] != "work" {
		t.Fatalf("unexpected connection: %#v", connection)
	}
}

func TestMigrationUpgradesExistingContextRowsIntoFTS(t *testing.T) {
	root := t.TempDir()
	database := filepath.Join(root, "outfit.db")
	db, err := sql.Open("sqlite", database)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE schema_version(version INTEGER NOT NULL);`,
		`INSERT INTO schema_version VALUES(1);`,
		`CREATE TABLE context_items(handle TEXT PRIMARY KEY,url TEXT NOT NULL,connection_name TEXT NOT NULL,resource_type TEXT NOT NULL,title TEXT NOT NULL DEFAULT '',body TEXT NOT NULL,fetched_at TEXT NOT NULL,expires_at TEXT NOT NULL);`,
		`INSERT INTO context_items VALUES('old','https://example.test','old-connection','issue','Historical item','migration searchable body','2026-01-01T00:00:00Z','2030-01-01T00:00:00Z');`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := Open(context.Background(), Paths{Root: root, Database: database, Packages: filepath.Join(root, "packages"), Skills: filepath.Join(root, "skills"), Locks: filepath.Join(root, "locks"), Vault: filepath.Join(root, "vault")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	results, err := store.SearchContexts(context.Background(), "migration", "", time.Now(), false, 10)
	if err != nil || len(results) != 1 || results[0].Handle != "old" {
		t.Fatalf("old row was not migrated into FTS: %#v %v", results, err)
	}
}

func TestMigrationRenamesPersistedCWProtocolIdentifiers(t *testing.T) {
	root := t.TempDir()
	store, err := Open(context.Background(), Paths{Root: root, Database: filepath.Join(root, "outfit.db"), Packages: filepath.Join(root, "packages"), Skills: filepath.Join(root, "skills"), Locks: filepath.Join(root, "locks"), Vault: filepath.Join(root, "vault")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	pkg := kit.Package{APIVersion: kit.APIVersion, Key: "acme", Display: "Acme", Version: "1", CredentialMethods: []kit.CredentialMethod{{ID: "cw-oauth", Label: "OAuth", Custody: "outfit"}}, Modes: []kit.Mode{{ID: "api", Label: "API", CredentialMethods: []string{"cw-oauth"}, DefaultCredentialMethod: "cw-oauth"}}, Skill: kit.Skill{Summary: "Acme"}}
	legacyRaw := []byte(`{"apiVersion":"cw.jhh3.dev/connector/v1alpha2","key":"acme","display":"Acme","version":"1","credentialMethods":[{"id":"cw-oauth","custody":"cw"}]}`)
	if err := store.InstallPackage(context.Background(), kit.Loaded{Package: pkg, Raw: legacyRaw, Digest: "legacy", Source: "test"}, "trusted"); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateConnection(context.Background(), Connection{ID: "legacy", Name: "legacy", ConnectorKey: "acme", SelectedMode: "api", CredentialMethod: "cw-oauth", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE schema_version SET version=4`); err != nil {
		t.Fatal(err)
	}
	if err := store.migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	connection, err := store.Connection(context.Background(), "legacy")
	if err != nil || connection.CredentialMethod != "outfit-oauth" {
		t.Fatalf("connection method = %q, err = %v", connection.CredentialMethod, err)
	}
	credential, err := store.Credential(context.Background(), connection.CredentialID)
	if err != nil || credential.MethodID != "outfit-oauth" {
		t.Fatalf("credential method = %q, err = %v", credential.MethodID, err)
	}
	var manifest string
	if err := store.db.QueryRow(`SELECT manifest_json FROM connector_packages WHERE key='acme'`).Scan(&manifest); err != nil || !strings.Contains(manifest, "outfit.jhh3.dev/") || !strings.Contains(manifest, `"custody":"outfit"`) || !strings.Contains(manifest, `"id":"outfit-oauth"`) {
		t.Fatalf("manifest = %q, err = %v", manifest, err)
	}
}

func TestContextFTSStoresProvenanceAndHonorsTTL(t *testing.T) {
	root := t.TempDir()
	store, err := Open(context.Background(), Paths{Root: root, Database: filepath.Join(root, "outfit.db"), Packages: filepath.Join(root, "packages"), Skills: filepath.Join(root, "skills"), Locks: filepath.Join(root, "locks"), Vault: filepath.Join(root, "vault")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	item := ContextItem{Handle: "res_alpha", URL: "outfit://github/get_issue/alpha", ConnectionName: "github", ConnectorKey: "github", Operation: "get_issue", ResourceType: "operation-result", Title: "Alpha incident", Body: `{"summary":"database timeout"}`, Arguments: map[string]any{"id": "ALPHA-1"}, Provenance: map[string]any{"kind": "operation", "status": 200}, FetchedAt: now, ExpiresAt: now.Add(time.Hour)}
	if err := store.PutContext(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	results, err := store.SearchContexts(context.Background(), "database", "github", now, false, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Operation != "get_issue" || results[0].Arguments["id"] != "ALPHA-1" || results[0].Provenance["kind"] != "operation" {
		t.Fatalf("unexpected search result: %#v", results)
	}
	if results, err = store.SearchContexts(context.Background(), "database", "", now.Add(2*time.Hour), false, 10); err != nil || len(results) != 0 {
		t.Fatalf("expired item was returned: %#v %v", results, err)
	}
	if count, err := store.PurgeExpiredContexts(context.Background(), now.Add(2*time.Hour)); err != nil || count != 1 {
		t.Fatalf("purge: %d %v", count, err)
	}
}

func TestPurgeAllContextsRemovesFreshFTSEntries(t *testing.T) {
	root := t.TempDir()
	store, err := Open(context.Background(), Paths{Root: root, Database: filepath.Join(root, "outfit.db"), Packages: filepath.Join(root, "packages"), Skills: filepath.Join(root, "skills"), Locks: filepath.Join(root, "locks"), Vault: filepath.Join(root, "vault")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	if err := store.PutContext(context.Background(), ContextItem{Handle: "fresh", URL: "https://example.test", ConnectionName: "docs", ResourceType: "page", Title: "Fresh item", Body: "still searchable", FetchedAt: now, ExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if count, err := store.PurgeAllContexts(context.Background()); err != nil || count != 1 {
		t.Fatalf("purge all: %d %v", count, err)
	}
	if results, err := store.SearchContexts(context.Background(), "searchable", "", now, true, 10); err != nil || len(results) != 0 {
		t.Fatalf("purged FTS entry remained: %#v %v", results, err)
	}
}

func TestSearchContextsTreatsUserInputAsPlainText(t *testing.T) {
	root := t.TempDir()
	store, err := Open(context.Background(), Paths{Root: root, Database: filepath.Join(root, "outfit.db"), Packages: filepath.Join(root, "packages"), Skills: filepath.Join(root, "skills"), Locks: filepath.Join(root, "locks"), Vault: filepath.Join(root, "vault")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	if err := store.PutContext(context.Background(), ContextItem{Handle: "special", URL: "https://example.test", ConnectionName: "docs", ResourceType: "page", Title: "C++ setup", Body: "Use foo:bar safely", FetchedAt: now, ExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"C++", "foo:bar"} {
		results, err := store.SearchContexts(context.Background(), query, "", now, false, 10)
		if err != nil || len(results) != 1 {
			t.Fatalf("plain query %q failed: results=%#v err=%v", query, results, err)
		}
	}
}

func TestSwitchAndRemovePersistCleanupPointerAtomically(t *testing.T) {
	root := t.TempDir()
	paths := Paths{Root: root, Database: filepath.Join(root, "outfit.db"), Packages: filepath.Join(root, "packages"), Skills: filepath.Join(root, "skills"), Locks: filepath.Join(root, "locks"), Vault: filepath.Join(root, "vault")}
	store, err := Open(context.Background(), paths)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	pkg := kit.Loaded{Package: kit.Package{Key: "acme", Version: "1"}, Raw: []byte(`{"key":"acme","version":"1"}`)}
	if err := store.InstallPackage(context.Background(), pkg, "trusted"); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateConnection(context.Background(), Connection{ID: "c1", Name: "work", ConnectorKey: "acme", SelectedMode: "one", CredentialMethod: "token", Status: "active", CredentialRef: "old", SafeIdentity: map[string]string{}}); err != nil {
		t.Fatal(err)
	}
	switched, cleanup, err := store.SwitchConnection(context.Background(), "work", "old", "two", "token", "new", "active", "clean_old", map[string]string{}, nil)
	if err != nil || !switched || !cleanup {
		t.Fatalf("switch: %v %v", switched, err)
	}
	cleanups, _ := store.Cleanups(context.Background())
	if len(cleanups) != 1 || cleanups[0].CredentialRef != "old" {
		t.Fatalf("old pointer lost: %#v", cleanups)
	}
	removed, cleanup, err := store.RemoveConnection(context.Background(), "work", "new", "clean_new")
	if err != nil || !removed || !cleanup {
		t.Fatalf("remove: %v %v", removed, err)
	}
	cleanups, _ = store.Cleanups(context.Background())
	if len(cleanups) != 2 {
		t.Fatalf("cleanup not retained: %#v", cleanups)
	}
	credentials, err := store.Credentials(context.Background())
	if err != nil || len(credentials) != 2 {
		t.Fatalf("credential lifecycle was not retained: %#v %v", credentials, err)
	}
	for _, credential := range credentials {
		if credential.Status != "cleanup-pending" || len(credential.Consumers) != 0 {
			t.Fatalf("removed credential has wrong lifecycle state: %#v", credential)
		}
	}
	if err := store.FinishCleanup(context.Background(), "clean_old"); err != nil {
		t.Fatal(err)
	}
	credentials, err = store.Credentials(context.Background())
	if err != nil || len(credentials) != 1 || credentials[0].VaultRef != "new" {
		t.Fatalf("finished cleanup did not remove the unreferenced credential: %#v %v", credentials, err)
	}
}

func TestRotateCredentialSecretMovesConsumersAndJournalsOldRef(t *testing.T) {
	root := t.TempDir()
	store, err := Open(context.Background(), Paths{Root: root, Database: filepath.Join(root, "outfit.db"), Packages: filepath.Join(root, "packages"), Skills: filepath.Join(root, "skills"), Locks: filepath.Join(root, "locks"), Vault: filepath.Join(root, "vault")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	pkg := kit.Loaded{Package: kit.Package{Key: "acme", Version: "1"}, Raw: []byte(`{"key":"acme","version":"1"}`)}
	if err := store.InstallPackage(context.Background(), pkg, "trusted"); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateConnection(context.Background(), Connection{ID: "c1", Name: "work", ConnectorKey: "acme", SelectedMode: "mcp", CredentialMethod: "oauth", Status: "active", CredentialRef: "old", SafeIdentity: map[string]string{"setting.OAUTH_TOKEN_EXPIRY": "old-expiry"}}); err != nil {
		t.Fatal(err)
	}
	rotated, err := store.RotateCredentialSecret(context.Background(), "cred_c1", "old", "new", "cleanup-refresh", map[string]string{"OAUTH_TOKEN_EXPIRY": "new-expiry"})
	if err != nil || !rotated {
		t.Fatalf("rotate: %t %v", rotated, err)
	}
	connection, err := store.Connection(context.Background(), "work")
	if err != nil || connection.CredentialRef != "new" || connection.SafeIdentity["setting.OAUTH_TOKEN_EXPIRY"] != "new-expiry" {
		t.Fatalf("connection = %#v err=%v", connection, err)
	}
	cleanups, err := store.Cleanups(context.Background())
	if err != nil || len(cleanups) != 1 || cleanups[0].CredentialRef != "old" {
		t.Fatalf("cleanups = %#v err=%v", cleanups, err)
	}
	if rotated, err = store.RotateCredentialSecret(context.Background(), "cred_c1", "old", "lost", "cleanup-lost", nil); err != nil || rotated {
		t.Fatalf("stale rotation won: %t %v", rotated, err)
	}
}
