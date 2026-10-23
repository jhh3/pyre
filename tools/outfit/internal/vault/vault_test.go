package vault

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jhh3/outfit/internal/state"
	keyring "github.com/zalando/go-keyring"
)

func TestFileVaultRoundTripAndDelete(t *testing.T) {
	root := t.TempDir()
	vault, err := openFile(root)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := vault.Put(context.Background(), map[string]string{"TOKEN": "super-secret"})
	if err != nil {
		t.Fatal(err)
	}
	value, err := vault.Get(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if value["TOKEN"] != "super-secret" {
		t.Fatal("secret did not round trip")
	}
	raw, err := os.ReadFile(filepath.Join(root, "secrets", ref+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) == "super-secret" {
		t.Fatal("secret was stored in plaintext")
	}
	if err := vault.Delete(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	if _, err := vault.Get(context.Background(), ref); !errorsIsNotExist(err) {
		t.Fatalf("expected deleted secret: %v", err)
	}
}

func errorsIsNotExist(err error) bool { return IsNotFound(err) }

func TestKeyringLazilyMigratesLegacySecret(t *testing.T) {
	originalGet, originalSet, originalDelete := keyringGet, keyringSet, keyringDelete
	t.Cleanup(func() { keyringGet, keyringSet, keyringDelete = originalGet, originalSet, originalDelete })
	entries := map[string]string{legacyService + "/cwsec_existing": `{"TOKEN":"secret"}`}
	keyringGet = func(serviceName, user string) (string, error) {
		value, ok := entries[serviceName+"/"+user]
		if !ok {
			return "", keyring.ErrNotFound
		}
		return value, nil
	}
	keyringSet = func(serviceName, user, value string) error {
		entries[serviceName+"/"+user] = value
		return nil
	}
	keyringDelete = func(serviceName, user string) error {
		key := serviceName + "/" + user
		if _, ok := entries[key]; !ok {
			return keyring.ErrNotFound
		}
		delete(entries, key)
		return nil
	}
	value, err := (&Keyring{}).Get(context.Background(), "cwsec_existing")
	if err != nil || value["TOKEN"] != "secret" {
		t.Fatalf("migrate secret: %#v %v", value, err)
	}
	if _, ok := entries[service+"/cwsec_existing"]; !ok {
		t.Fatal("Outfit keychain entry was not created")
	}
	if _, ok := entries[legacyService+"/cwsec_existing"]; ok {
		t.Fatal("legacy keychain entry was not removed")
	}
	if err := (&Keyring{}).Delete(context.Background(), "cwsec_existing"); err != nil {
		t.Fatal(err)
	}
	if _, err := keyringGet(service, "cwsec_existing"); !errors.Is(err, keyring.ErrNotFound) {
		t.Fatal("migrated secret was not deleted")
	}
}

func TestNewSecretReferencesUseOutfitPrefix(t *testing.T) {
	ref, err := randomRef()
	if err != nil || !strings.HasPrefix(ref, "outfitsec_") {
		t.Fatalf("reference = %q, err = %v", ref, err)
	}
}

func TestKeyringReportsMissingSecretAsNotFound(t *testing.T) {
	originalGet := keyringGet
	t.Cleanup(func() { keyringGet = originalGet })
	keyringGet = func(string, string) (string, error) { return "", keyring.ErrNotFound }
	if _, err := (&Keyring{}).Get(context.Background(), "outfitsec_missing"); !IsNotFound(err) {
		t.Fatalf("missing keychain item = %v, want ErrNotFound", err)
	}
	keyringGet = func(string, string) (string, error) { return "", errors.New("keychain locked") }
	if _, err := (&Keyring{}).Get(context.Background(), "outfitsec_locked"); err == nil || IsNotFound(err) {
		t.Fatalf("locked keychain = %v, want a non-not-found error", err)
	}
}

// Nothing records a backend implicitly: running once with
// OUTFIT_VAULT_BACKEND=file must not move a keychain machine to the file
// vault.
func TestFileVaultUseDoesNotRecordTheBackend(t *testing.T) {
	paths := testPaths(t)
	t.Setenv("OUTFIT_VAULT_BACKEND", "file")
	if _, err := mustOpen(t, paths).Put(context.Background(), map[string]string{"TOKEN": "synthetic"}); err != nil {
		t.Fatal(err)
	}
	mustOpen(t, paths)
	t.Setenv("OUTFIT_VAULT_BACKEND", "")
	if backend, source := BackendSource(paths); backend != "keyring" || source != "default" {
		t.Fatalf("backend after an env-selected run = %s (%s)", backend, source)
	}
}

func TestSetBackendIsRecordedAndEnvStillWins(t *testing.T) {
	paths := testPaths(t)
	t.Setenv("OUTFIT_VAULT_BACKEND", "")
	if err := SetBackend(paths, "file"); err != nil {
		t.Fatal(err)
	}
	if backend, source := BackendSource(paths); backend != "file" || source != "recorded" {
		t.Fatalf("recorded backend = %s (%s)", backend, source)
	}
	t.Setenv("OUTFIT_VAULT_BACKEND", "keyring")
	if backend, source := BackendSource(paths); backend != "keyring" || source != "env" {
		t.Fatalf("env backend = %s (%s)", backend, source)
	}
	if err := SetBackend(paths, "bogus"); err == nil {
		t.Fatal("bogus backend accepted")
	}
}

// Whatever backend is selected, a secret is read where it lives.
func TestDualReadsSecretsFromEitherBackend(t *testing.T) {
	originalGet, originalSet, originalDelete := keyringGet, keyringSet, keyringDelete
	t.Cleanup(func() { keyringGet, keyringSet, keyringDelete = originalGet, originalSet, originalDelete })
	entries := map[string]string{}
	keyringGet = func(serviceName, user string) (string, error) {
		if v, ok := entries[serviceName+"/"+user]; ok {
			return v, nil
		}
		return "", keyring.ErrNotFound
	}
	keyringSet = func(serviceName, user, value string) error { entries[serviceName+"/"+user] = value; return nil }
	keyringDelete = func(serviceName, user string) error { delete(entries, serviceName+"/"+user); return nil }
	ctx := context.Background()
	paths := testPaths(t)

	t.Setenv("OUTFIT_VAULT_BACKEND", "keyring")
	keyRef, err := mustOpen(t, paths).Put(ctx, map[string]string{"TOKEN": "synthetic-keychain"})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("OUTFIT_VAULT_BACKEND", "file")
	fileVault := mustOpen(t, paths)
	fileRef, err := fileVault.Put(ctx, map[string]string{"TOKEN": "synthetic-file"})
	if err != nil {
		t.Fatal(err)
	}
	if v, err := fileVault.Get(ctx, keyRef); err != nil || v["TOKEN"] != "synthetic-keychain" {
		t.Fatalf("file backend reading a keychain ref: %v %v", v, err)
	}
	t.Setenv("OUTFIT_VAULT_BACKEND", "keyring")
	keyVault := mustOpen(t, paths)
	if v, err := keyVault.Get(ctx, fileRef); err != nil || v["TOKEN"] != "synthetic-file" {
		t.Fatalf("keyring backend reading a file ref: %v %v", v, err)
	}
	if err := keyVault.Delete(ctx, fileRef); err != nil {
		t.Fatal(err)
	}
	if _, err := keyVault.Get(ctx, fileRef); !IsNotFound(err) {
		t.Fatalf("deleted file ref = %v", err)
	}
	// File backend on a machine without a secret service: a ref in neither
	// place is "not found", not an opaque keychain error.
	keyringGet = func(string, string) (string, error) {
		return "", errors.New("synthetic: no secret service on the session bus")
	}
	if _, err := fileVault.Get(ctx, "outfitsec_absent"); !IsNotFound(err) {
		t.Fatalf("missing ref with no keychain = %v", err)
	}
}

func TestKeyringHomeDoesNotCreateAFileVault(t *testing.T) {
	paths := testPaths(t)
	t.Setenv("OUTFIT_VAULT_BACKEND", "")
	mustOpen(t, paths)
	if _, err := os.Stat(filepath.Join(paths.Vault, "master.key")); !os.IsNotExist(err) {
		t.Fatalf("keyring home got a file-vault key: %v", err)
	}
}

func testPaths(t *testing.T) state.Paths {
	root := t.TempDir()
	return state.Paths{Root: root, Vault: filepath.Join(root, "vault")}
}

func mustOpen(t *testing.T, paths state.Paths) Store {
	t.Helper()
	store, err := Open(paths)
	if err != nil {
		t.Fatal(err)
	}
	return store
}
