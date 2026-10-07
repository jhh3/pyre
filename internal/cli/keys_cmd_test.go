package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestKeysCommand(t *testing.T) {
	e := newEnv(t, nil)
	r := run(t, e, nil, "keys")
	expectCode(t, r, ExitOK)
	want := filepath.Join(e.get("XDG_CONFIG_HOME"), "pyre", "keys.toml")
	if !strings.Contains(r.stdout, want) || !strings.Contains(r.stdout, "leader") {
		t.Fatalf("keys output lacks path or defaults:\n%s", r)
	}
	if _, err := os.Stat(want); !os.IsNotExist(err) {
		t.Fatalf("pyre keys must never create %s (stat err %v)", want, err)
	}

	bad := filepath.Join(t.TempDir(), "keys.toml")
	if err := os.WriteFile(bad, []byte("leader = [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	expectCode(t, run(t, e, nil, "keys", "--file", bad), ExitUsage)
	expectCode(t, run(t, e, nil, "keys", "--json"), ExitOK)
}
