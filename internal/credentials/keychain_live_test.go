package credentials

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestKeychainLiveThrowaway exercises the real macOS `security` CLI against a
// throwaway keychain created inside $PYRE_TEST_KEYCHAIN_DIR (never the login
// keychain). It is skipped unless that variable is set.
func TestKeychainLiveThrowaway(t *testing.T) {
	dir := os.Getenv("PYRE_TEST_KEYCHAIN_DIR")
	if runtime.GOOS != "darwin" || dir == "" {
		t.Skip("set PYRE_TEST_KEYCHAIN_DIR on macOS to run against a throwaway keychain")
	}
	kc := filepath.Join(dir, "pyre-wp12-test.keychain-db")
	_ = os.Remove(kc)
	mustRun(t, "/usr/bin/security", "create-keychain", "-p", "synthetic-throwaway", kc)
	t.Cleanup(func() {
		_ = exec.Command("/usr/bin/security", "delete-keychain", kc).Run()
		out, _ := exec.Command("/usr/bin/security", "list-keychains", "-d", "user").Output()
		if strings.Contains(string(out), "pyre-wp12-test") {
			t.Errorf("throwaway keychain left in search list")
		}
	})
	mustRun(t, "/usr/bin/security", "set-keychain-settings", kc) // no auto-lock
	mustRun(t, "/usr/bin/security", "unlock-keychain", "-p", "synthetic-throwaway", kc)

	k := &KeychainStore{Security: "/usr/bin/security", Keychain: kc}
	ctx := context.Background()
	ref := NewRef(BackendKeychain, "conn_live-test", "SYNTH_TOKEN")
	// Include newline, quotes, spaces, unicode, and a long JWT-sized body.
	value := synthA + " \"quoted\" 'single' \n ünïcode " + strings.Repeat("x", 1400)
	if err := k.Put(ctx, ref, NewSecret(value)); err != nil {
		t.Fatal(err)
	}
	got, err := k.Get(ctx, ref)
	if err != nil || got.Reveal() != value {
		t.Fatalf("round trip failed: %v (len %d vs %d)", err, got.Len(), len(value))
	}
	if err := k.Put(ctx, NewRef(BackendKeychain, "conn_live-test", "TOO_BIG"), NewSecret(strings.Repeat("z", MaxKeychainValue+1000))); kindOf(err) != KindInvalid {
		t.Fatalf("oversized value must be refused: %v", err)
	}
	// -U updates in place.
	if err := k.Put(ctx, ref, NewSecret(synthB)); err != nil {
		t.Fatal(err)
	}
	if got, _ := k.Get(ctx, ref); got.Reveal() != synthB {
		t.Fatal("update did not replace value")
	}
	if err := k.Delete(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if _, err := k.Get(ctx, ref); !IsNotFound(err) {
		t.Fatalf("expected not found after delete: %v", err)
	}
	if err := k.Delete(ctx, ref); err != nil {
		t.Fatalf("delete must be idempotent: %v", err)
	}
}

func mustRun(t *testing.T, name string, args ...string) {
	t.Helper()
	if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v: %s", name, args, err, out)
	}
}
