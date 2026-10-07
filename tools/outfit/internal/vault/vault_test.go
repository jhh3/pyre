package vault

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

func errorsIsNotExist(err error) bool { return os.IsNotExist(err) }

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
