package linkcontext

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/jhh3/outfit/internal/state"
)

func TestLinkPreferencesDefaultHeavyAndPersistPerConnectionModes(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := state.Open(ctx, state.Paths{Root: root, Database: filepath.Join(root, "outfit.db"), Packages: filepath.Join(root, "packages"), Skills: filepath.Join(root, "skills"), Locks: filepath.Join(root, "locks"), Vault: filepath.Join(root, "vault")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	preferences, err := LoadPreferences(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	if preferences.Mode("notion") != ModeHeavy {
		t.Fatal("new connections should use heavy enrichment by default")
	}
	if err := preferences.SetMode(ctx, store, "notion", ModeLight); err != nil {
		t.Fatal(err)
	}
	if err := preferences.SetMode(ctx, store, "github", ModeOff); err != nil {
		t.Fatal(err)
	}
	reloaded, err := LoadPreferences(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Mode("notion") != ModeLight || reloaded.Mode("github") != ModeOff || reloaded.Mode("slack") != ModeHeavy {
		t.Fatalf("modes were not persisted with a heavy default: notion=%s github=%s slack=%s", reloaded.Mode("notion"), reloaded.Mode("github"), reloaded.Mode("slack"))
	}
}
