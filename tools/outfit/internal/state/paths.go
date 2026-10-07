package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type Paths struct {
	Root     string
	Database string
	Packages string
	Skills   string
	Locks    string
	Vault    string
}

func ResolvePaths() (Paths, error) {
	root := os.Getenv("OUTFIT_HOME")
	legacyRoot := ""
	if root == "" {
		if configured := os.Getenv("CW_HOME"); configured != "" {
			root = configured
		} else {
			config, err := os.UserConfigDir()
			if err != nil {
				return Paths{}, fmt.Errorf("resolve config directory: %w", err)
			}
			root = filepath.Join(config, "outfit")
			legacyRoot = filepath.Join(config, "cw")
		}
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return Paths{}, err
	}
	if legacyRoot != "" {
		legacyRoot, err = filepath.Abs(legacyRoot)
		if err != nil {
			return Paths{}, err
		}
		if err := migrateLegacyRoot(legacyRoot, root); err != nil {
			return Paths{}, err
		}
	}
	if err := migrateLegacyDatabase(root); err != nil {
		return Paths{}, err
	}
	if legacyRoot != "" {
		if err := migrateLegacyReceiptPaths(legacyRoot, root); err != nil {
			return Paths{}, err
		}
	}
	return Paths{
		Root: root, Database: filepath.Join(root, "outfit.db"), Packages: filepath.Join(root, "packages"),
		Skills: filepath.Join(root, "skills"), Locks: filepath.Join(root, "locks"), Vault: filepath.Join(root, "vault"),
	}, nil
}

func migrateLegacyReceiptPaths(legacyRoot, root string) error {
	receipts, err := filepath.Glob(filepath.Join(root, "tools", "*", "*", "*", "receipt.json"))
	if err != nil {
		return fmt.Errorf("find migrated tool receipts: %w", err)
	}
	for _, receiptPath := range receipts {
		bytes, err := os.ReadFile(receiptPath)
		if err != nil {
			return fmt.Errorf("read migrated tool receipt %s: %w", receiptPath, err)
		}
		var receipt map[string]any
		if err := json.Unmarshal(bytes, &receipt); err != nil {
			return fmt.Errorf("decode migrated tool receipt %s: %w", receiptPath, err)
		}
		storedPath, _ := receipt["path"].(string)
		relative, ok := pathWithin(legacyRoot, storedPath)
		if !ok {
			continue
		}
		receipt["path"] = filepath.Join(root, relative)
		encoded, err := json.MarshalIndent(receipt, "", "  ")
		if err != nil {
			return fmt.Errorf("encode migrated tool receipt %s: %w", receiptPath, err)
		}
		temporary, err := os.CreateTemp(filepath.Dir(receiptPath), ".receipt-migrate-*")
		if err != nil {
			return err
		}
		temporaryPath := temporary.Name()
		cleanup := func() { _ = os.Remove(temporaryPath) }
		if err := temporary.Chmod(0o600); err != nil {
			_ = temporary.Close()
			cleanup()
			return err
		}
		if _, err := temporary.Write(append(encoded, '\n')); err != nil {
			_ = temporary.Close()
			cleanup()
			return err
		}
		if err := temporary.Close(); err != nil {
			cleanup()
			return err
		}
		if err := os.Rename(temporaryPath, receiptPath); err != nil {
			cleanup()
			return fmt.Errorf("replace migrated tool receipt %s: %w", receiptPath, err)
		}
	}
	return nil
}

func pathWithin(root, path string) (string, bool) {
	if root == "" || path == "" {
		return "", false
	}
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return "", false
	}
	return relative, true
}

func migrateLegacyRoot(legacyRoot, root string) error {
	if _, err := os.Stat(legacyRoot); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("inspect legacy cw state: %w", err)
	}
	if _, err := os.Stat(root); err == nil {
		return fmt.Errorf("both legacy cw state and Outfit state exist; move or remove one before continuing")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect Outfit state: %w", err)
	}
	if err := os.Rename(legacyRoot, root); err != nil {
		return fmt.Errorf("adopt legacy cw state as Outfit: %w", err)
	}
	return nil
}

func migrateLegacyDatabase(root string) error {
	for _, suffix := range []string{"", "-wal", "-shm"} {
		legacyPath := filepath.Join(root, "cw.db"+suffix)
		outfitPath := filepath.Join(root, "outfit.db"+suffix)
		if _, err := os.Stat(legacyPath); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return fmt.Errorf("inspect legacy cw database: %w", err)
		}
		if _, err := os.Stat(outfitPath); err == nil {
			return fmt.Errorf("both legacy cw database and Outfit database exist in %s", root)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect Outfit database: %w", err)
		}
		if err := os.Rename(legacyPath, outfitPath); err != nil {
			return fmt.Errorf("rename legacy cw database: %w", err)
		}
	}
	return nil
}

func (paths Paths) Ensure() error {
	for _, directory := range []string{paths.Root, paths.Packages, paths.Skills, paths.Locks, paths.Vault} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return err
		}
		if err := os.Chmod(directory, 0o700); err != nil {
			return err
		}
	}
	return nil
}
