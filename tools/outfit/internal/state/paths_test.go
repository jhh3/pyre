package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestMigrateLegacyRootAndDatabase(t *testing.T) {
	config := t.TempDir()
	legacyRoot := filepath.Join(config, "cw")
	outfitRoot := filepath.Join(config, "outfit")
	if err := os.MkdirAll(filepath.Join(legacyRoot, "vault"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"cw.db", "cw.db-wal", "cw.db-shm"} {
		if err := os.WriteFile(filepath.Join(legacyRoot, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrateLegacyRoot(legacyRoot, outfitRoot); err != nil {
		t.Fatal(err)
	}
	if err := migrateLegacyDatabase(outfitRoot); err != nil {
		t.Fatal(err)
	}
	if err := migrateLegacyReceiptPaths(legacyRoot, outfitRoot); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(legacyRoot); !os.IsNotExist(err) {
		t.Fatalf("legacy root still exists: %v", err)
	}
	for _, name := range []string{"outfit.db", "outfit.db-wal", "outfit.db-shm"} {
		if _, err := os.Stat(filepath.Join(outfitRoot, name)); err != nil {
			t.Fatalf("missing migrated %s: %v", name, err)
		}
	}
	if err := migrateLegacyRoot(legacyRoot, outfitRoot); err != nil {
		t.Fatalf("migration is not idempotent: %v", err)
	}
}

func TestMigrateLegacyReceiptPathsRepairsMovedManagedTools(t *testing.T) {
	config := t.TempDir()
	legacyRoot := filepath.Join(config, "cw")
	outfitRoot := filepath.Join(config, "outfit")
	receiptPath := filepath.Join(outfitRoot, "tools", "gws", "0.22.5", "linux-amd64", "receipt.json")
	if err := os.MkdirAll(filepath.Dir(receiptPath), 0o700); err != nil {
		t.Fatal(err)
	}
	legacyBinary := filepath.Join(legacyRoot, "tools", "gws", "0.22.5", "linux-amd64", "gws")
	if err := os.WriteFile(receiptPath, []byte(`{"id":"gws","path":"`+filepath.ToSlash(legacyBinary)+`","source":"release"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := migrateLegacyReceiptPaths(legacyRoot, outfitRoot); err != nil {
		t.Fatal(err)
	}
	assertReceiptPath(t, receiptPath, filepath.Join(outfitRoot, "tools", "gws", "0.22.5", "linux-amd64", "gws"))
	if err := migrateLegacyReceiptPaths(legacyRoot, outfitRoot); err != nil {
		t.Fatalf("receipt repair is not idempotent: %v", err)
	}
	assertReceiptPath(t, receiptPath, filepath.Join(outfitRoot, "tools", "gws", "0.22.5", "linux-amd64", "gws"))
}

func TestMigrateLegacyReceiptPathsLeavesAdoptedExternalToolsAlone(t *testing.T) {
	config := t.TempDir()
	receiptPath := filepath.Join(config, "outfit", "tools", "gh", "system", "linux-amd64", "receipt.json")
	if err := os.MkdirAll(filepath.Dir(receiptPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(receiptPath, []byte(`{"id":"gh","path":"/usr/local/bin/gh","adopted":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := migrateLegacyReceiptPaths(filepath.Join(config, "cw"), filepath.Join(config, "outfit")); err != nil {
		t.Fatal(err)
	}
	assertReceiptPath(t, receiptPath, "/usr/local/bin/gh")
}

func assertReceiptPath(t *testing.T, receiptPath, expected string) {
	t.Helper()
	bytes, err := os.ReadFile(receiptPath)
	if err != nil {
		t.Fatal(err)
	}
	var receipt struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(bytes, &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.Path != expected {
		t.Fatalf("receipt path = %q, want %q", receipt.Path, expected)
	}
}

func TestMigrateLegacyRootRefusesSplitState(t *testing.T) {
	config := t.TempDir()
	legacyRoot := filepath.Join(config, "cw")
	outfitRoot := filepath.Join(config, "outfit")
	if err := os.MkdirAll(legacyRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outfitRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := migrateLegacyRoot(legacyRoot, outfitRoot); err == nil {
		t.Fatal("expected split state to fail closed")
	}
}
