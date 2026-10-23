package backup

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/outfit/internal/kit"
	"github.com/jhh3/outfit/internal/skillmanager"
	"github.com/jhh3/outfit/internal/state"
	"github.com/jhh3/outfit/internal/toolchain"
	"github.com/jhh3/outfit/internal/vault"
)

func TestEncryptedBackupRoundTripContainsConnectionsWithoutPlaintext(t *testing.T) {
	t.Setenv("OUTFIT_VAULT_BACKEND", "file")
	ctx := context.Background()
	root := t.TempDir()
	paths := state.Paths{Root: root, Database: filepath.Join(root, "outfit.db"), Packages: filepath.Join(root, "packages"), Skills: filepath.Join(root, "skills"), Locks: filepath.Join(root, "locks"), Vault: filepath.Join(root, "vault")}
	store, err := state.Open(ctx, paths)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	pkg := kit.Package{APIVersion: kit.APIVersion, Key: "acme", Display: "Acme", Version: "1", CredentialMethods: []kit.CredentialMethod{{ID: "token", Label: "Token", Description: "Token", Recommended: true, Custody: "outfit", Fields: []kit.CredentialField{{Key: "TOKEN", Label: "Token", Secret: true}}}}, Modes: []kit.Mode{{ID: "cli", Label: "CLI", Description: "CLI", Recommendation: kit.Recommendation{Rank: 1, Reason: "test"}, Executor: kit.Executor{Kind: "cli", Binary: "true"}, CredentialMethods: []string{"token"}, DefaultCredentialMethod: "token", Operations: []kit.Operation{{Name: "status", Description: "Status", Class: "read", CLI: &kit.CLIOperation{Args: []string{"--version"}}}}}}, Skill: kit.Skill{Summary: "Acme"}}
	raw, _ := json.Marshal(pkg)
	if err := store.InstallPackage(ctx, kit.Loaded{Package: pkg, Raw: raw, Digest: "digest", Source: "test"}, "trusted"); err != nil {
		t.Fatal(err)
	}
	secrets, err := vault.Open(paths)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := secrets.Put(ctx, map[string]string{"TOKEN": "super-secret-value"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateConnection(ctx, state.Connection{ID: "con_1", Name: "acme", ConnectorKey: "acme", SelectedMode: "cli", CredentialMethod: "token", Status: "active", CredentialRef: ref, SafeIdentity: map[string]string{"account": "work"}}); err != nil {
		t.Fatal(err)
	}
	payload, skipped, err := Build(ctx, store, secrets, toolchain.Manager{Paths: paths}, skillmanager.Manager{Paths: paths}, BuildOptions{})
	if err != nil || len(skipped) != 0 {
		t.Fatal(skipped, err)
	}
	path := filepath.Join(root, "portable.outfit")
	if err := Write(path, "correct horse battery staple", payload); err != nil {
		t.Fatal(err)
	}
	rawBackup, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rawBackup), "super-secret-value") {
		t.Fatal("backup exposed plaintext secret")
	}
	if _, err := Read(path, "wrong"); err == nil {
		t.Fatal("wrong passphrase decrypted backup")
	}
	restored, err := Read(path, "correct horse battery staple")
	if err != nil || len(restored.Connections) != 1 || len(restored.Credentials) != 1 || restored.Credentials[0].Fields["TOKEN"] != "super-secret-value" || restored.Connections[0].CredentialID != restored.Credentials[0].ID {
		t.Fatalf("restored payload: %#v %v", restored, err)
	}
	if info, err := os.Stat(path); err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o600) {
		t.Fatalf("backup permissions: %#v %v", info, err)
	}
}

func TestReadAcceptsLegacyCWBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "portable.cwb")
	want := Payload{
		CreatedAt:   time.Now().UTC(),
		Connections: []Connection{{Name: "workspace", ConnectorKey: "google-workspace", CredentialMethod: "cw-oauth"}},
		Credentials: []Credential{{ID: "cred_1", ConnectorKey: "google-workspace", MethodID: "cw-oauth"}},
		Packages: []Package{{Manifest: kit.Package{
			APIVersion:        "cw.jhh3.dev/connector/v1alpha2",
			CredentialMethods: []kit.CredentialMethod{{ID: "cw-oauth", Custody: "cw"}},
			Modes:             []kit.Mode{{CredentialMethods: []string{"cw-oauth"}, DefaultCredentialMethod: "cw-oauth"}},
		}}},
	}
	if err := writeWithFormat(path, "passphrase", want, legacyFormat); err != nil {
		t.Fatal(err)
	}
	got, err := Read(path, "passphrase")
	if err != nil || len(got.Connections) != 1 || got.Connections[0].CredentialMethod != "outfit-oauth" || got.Credentials[0].MethodID != "outfit-oauth" || got.Packages[0].Manifest.APIVersion != kit.APIVersion || got.Packages[0].Manifest.CredentialMethods[0].Custody != "outfit" || got.Packages[0].Manifest.Modes[0].DefaultCredentialMethod != "outfit-oauth" {
		t.Fatalf("legacy backup = %#v, err = %v", got, err)
	}
}

// One connection whose vault secret vanished (seen on server: intercom's file
// was gone while doctor said ok) must not block backing up the others.
func TestBuildSkipsConnectionsWhoseSecretIsMissing(t *testing.T) {
	t.Setenv("OUTFIT_VAULT_BACKEND", "file")
	ctx := context.Background()
	root := t.TempDir()
	paths := state.Paths{Root: root, Database: filepath.Join(root, "outfit.db"), Packages: filepath.Join(root, "packages"), Skills: filepath.Join(root, "skills"), Locks: filepath.Join(root, "locks"), Vault: filepath.Join(root, "vault")}
	store, err := state.Open(ctx, paths)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	pkg := kit.Package{APIVersion: kit.APIVersion, Key: "acme", Display: "Acme", Version: "1", CredentialMethods: []kit.CredentialMethod{{ID: "token", Label: "Token", Description: "Token", Recommended: true, Custody: "outfit", Fields: []kit.CredentialField{{Key: "TOKEN", Label: "Token", Secret: true}}}}, Modes: []kit.Mode{{ID: "cli", Label: "CLI", Description: "CLI", Recommendation: kit.Recommendation{Rank: 1, Reason: "test"}, Executor: kit.Executor{Kind: "cli", Binary: "true"}, CredentialMethods: []string{"token"}, DefaultCredentialMethod: "token", Operations: []kit.Operation{{Name: "status", Description: "Status", Class: "read", CLI: &kit.CLIOperation{Args: []string{"--version"}}}}}}, Skill: kit.Skill{Summary: "Acme"}}
	raw, _ := json.Marshal(pkg)
	if err := store.InstallPackage(ctx, kit.Loaded{Package: pkg, Raw: raw, Digest: "digest", Source: "test"}, "trusted"); err != nil {
		t.Fatal(err)
	}
	secrets, err := vault.Open(paths)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"kept", "broken"} {
		ref, err := secrets.Put(ctx, map[string]string{"TOKEN": "synthetic-" + name})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.CreateConnection(ctx, state.Connection{ID: "con_" + name, Name: name, ConnectorKey: "acme", SelectedMode: "cli", CredentialMethod: "token", Status: "active", CredentialRef: ref}); err != nil {
			t.Fatal(err)
		}
		if name == "broken" {
			if err := os.Remove(filepath.Join(paths.Vault, "secrets", ref+".json")); err != nil {
				t.Fatal(err)
			}
		}
	}
	payload, skipped, err := Build(ctx, store, secrets, toolchain.Manager{Paths: paths}, skillmanager.Manager{Paths: paths}, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 1 || skipped[0].Name != "broken" || skipped[0].Kind != SkipMissingSecret || !strings.Contains(skipped[0].Reason, "no secret") {
		t.Fatalf("skipped = %#v", skipped)
	}
	if len(payload.Connections) != 1 || payload.Connections[0].Name != "kept" || len(payload.Credentials) != 1 || payload.Credentials[0].Fields["TOKEN"] != "synthetic-kept" {
		t.Fatalf("payload = %#v", payload)
	}
}

func TestBuildCanExcludeOAuthConnections(t *testing.T) {
	t.Setenv("OUTFIT_VAULT_BACKEND", "file")
	ctx := context.Background()
	root := t.TempDir()
	paths := state.Paths{Root: root, Database: filepath.Join(root, "outfit.db"), Packages: filepath.Join(root, "packages"), Skills: filepath.Join(root, "skills"), Locks: filepath.Join(root, "locks"), Vault: filepath.Join(root, "vault")}
	store, err := state.Open(ctx, paths)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	pkg := kit.Package{APIVersion: kit.APIVersion, Key: "acme", Display: "Acme", Version: "1", CredentialMethods: []kit.CredentialMethod{
		{ID: "token", Label: "Token", Description: "Token", Recommended: true, Custody: "outfit", Fields: []kit.CredentialField{{Key: "TOKEN", Label: "Token", Secret: true}}},
		{ID: "outfit-oauth", Label: "OAuth", Description: "OAuth", Custody: "oauth", OAuth: &kit.OAuthConfig{}},
	}, Modes: []kit.Mode{{ID: "cli", Label: "CLI", Description: "CLI", Recommendation: kit.Recommendation{Rank: 1, Reason: "test"}, Executor: kit.Executor{Kind: "cli", Binary: "true"}, CredentialMethods: []string{"token", "outfit-oauth"}, DefaultCredentialMethod: "token"}}, Skill: kit.Skill{Summary: "Acme"}}
	raw, _ := json.Marshal(pkg)
	if err := store.InstallPackage(ctx, kit.Loaded{Package: pkg, Raw: raw, Digest: "digest", Source: "test"}, "trusted"); err != nil {
		t.Fatal(err)
	}
	secrets, err := vault.Open(paths)
	if err != nil {
		t.Fatal(err)
	}
	for name, method := range map[string]string{"plain": "token", "grant": "outfit-oauth"} {
		ref, _ := secrets.Put(ctx, map[string]string{"TOKEN": "synthetic-" + name})
		if err := store.CreateConnection(ctx, state.Connection{ID: "con_" + name, Name: name, ConnectorKey: "acme", SelectedMode: "cli", CredentialMethod: method, Status: "active", CredentialRef: ref}); err != nil {
			t.Fatal(err)
		}
	}
	payload, skipped, err := Build(ctx, store, secrets, toolchain.Manager{Paths: paths}, skillmanager.Manager{Paths: paths}, BuildOptions{ExcludeOAuth: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(payload.Connections) != 1 || payload.Connections[0].Name != "plain" || len(payload.Credentials) != 1 {
		t.Fatalf("payload = %#v", payload)
	}
	if len(skipped) != 1 || skipped[0].Name != "grant" || skipped[0].Kind != SkipOAuth {
		t.Fatalf("skipped = %#v", skipped)
	}
}
