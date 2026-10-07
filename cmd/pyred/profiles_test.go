package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProfileRoot(t *testing.T) {
	home, state := t.TempDir(), filepath.Join(t.TempDir(), "state")
	env := map[string]string{}
	get := func(k string) string { return env[k] }
	if got, want := profileRoot(state, get, home), filepath.Join(home, ".local", "state", "pyre", "profiles"); got != want {
		t.Fatalf("default = %s, want %s", got, want)
	}
	// An existing <state>/profiles keeps working.
	if err := os.MkdirAll(filepath.Join(state, "profiles"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got := profileRoot(state, get, home); got != filepath.Join(state, "profiles") {
		t.Fatalf("legacy = %s", got)
	}
	env[EnvProfileRoot] = "/srv/synthetic/profiles"
	if got := profileRoot(state, get, home); got != "/srv/synthetic/profiles" {
		t.Fatalf("override = %s", got)
	}
}

func TestRunFlagsAccountsTestMode(t *testing.T) {
	var o runOpts
	if err := runFlags(&o).Parse([]string{"--state", t.TempDir()}); err != nil || o.accountsTestMode {
		t.Fatalf("default: %v %v", err, o.accountsTestMode)
	}
	var o2 runOpts
	if err := runFlags(&o2).Parse([]string{"--accounts-test-mode", "--rota-checkout"}); err != nil || !o2.accountsTestMode || !o2.rotaCheckout {
		t.Fatalf("flag: %v %+v", err, o2)
	}
}
