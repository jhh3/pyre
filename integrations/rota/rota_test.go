package rota

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// fakeRota writes a stand-in rota script. Any invocation other than
// `--help` leaves a marker file, so tests prove Pyre never runs it.
func fakeRota(t *testing.T, dir string) (path, marker string) {
	t.Helper()
	path = filepath.Join(dir, "rota")
	marker = filepath.Join(dir, "FORBIDDEN-INVOCATION")
	script := `#!/bin/sh
# rota — manage Claude accounts (synthetic stand-in)
if [ "$1" = "--help" ] && [ $# -eq 1 ]; then
  echo "rota — manage Claude accounts and show Claude + Codex subscription usage."
  exit 0
fi
echo "$@" >> '` + marker + `'
exit 3
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path, marker
}

func setup(t *testing.T) (Options, string, string) {
	t.Helper()
	base := t.TempDir()
	path, marker := fakeRota(t, base)
	claudeAcc := filepath.Join(base, "home", ".claude", "accounts")
	codexHome := filepath.Join(base, "home", ".codex")
	os.MkdirAll(claudeAcc, 0o700)
	for _, n := range []string{"alice@synthetic.invalid", "work-b"} {
		p := filepath.Join(claudeAcc, n+".json")
		os.WriteFile(p, []byte(`{"claudeAiOauth":{"accessToken":"pyre-synthetic-R-access-initial","refreshToken":"pyre-synthetic-R-refresh-initial"}}`), 0o600)
		// Unreadable: proves names come from the directory listing only.
		os.Chmod(p, 0)
	}
	os.WriteFile(filepath.Join(claudeAcc, ".usage-cache.json"), []byte(`{
	  "alice@synthetic.invalid": {"fetchedAt": 1791150000000, "data": {
	    "five_hour": {"utilization": 3.0, "resets_at": "2026-10-04T20:10:00+00:00"},
	    "seven_day": {"utilization": 42.0, "resets_at": "2026-10-06T02:00:00+00:00"},
	    "extra_usage": {"is_enabled": true, "used_credits": 340142, "decimal_places": 2}}},
	  "(current)": {"fetchedAt": 1791150000000, "data": {"five_hour": {"utilization": 9}}},
	  "work-b": {"fetchedAt": 1791140000000, "data": {"limits": [{"kind": "session", "percent": 50}]}}
	}`), 0o600)
	for _, n := range []string{"personal", "team"} {
		d := filepath.Join(codexHome, "accounts", n)
		os.MkdirAll(d, 0o700)
		os.WriteFile(filepath.Join(d, "auth.json"), []byte(`{"tokens":{"access_token":"pyre-synthetic-S-access-initial"}}`), 0o600)
		os.Chmod(filepath.Join(d, "auth.json"), 0)
	}
	os.MkdirAll(filepath.Join(codexHome, "accounts", "empty"), 0o700) // no auth.json: not an account
	t.Cleanup(func() {
		filepath.Walk(base, func(p string, _ os.FileInfo, _ error) error { os.Chmod(p, 0o700); return nil })
	})
	return Options{RotaPath: path, ClaudeAccountsDir: claudeAcc, CodexHome: codexHome}, marker, base
}

func TestScanReadOnly(t *testing.T) {
	opts, marker, _ := setup(t)
	opts.RunHelp = true
	inv, err := Scan(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if !inv.Status.Present || inv.Status.Health != "ok" || !strings.HasPrefix(inv.Status.Revision, "sha256:") {
		t.Fatalf("status %+v", inv.Status)
	}
	if strings.Join(inv.ClaudeAccounts, ",") != "alice@synthetic.invalid,work-b" {
		t.Fatalf("claude names %v", inv.ClaudeAccounts)
	}
	if strings.Join(inv.CodexAccounts, ",") != "personal,team" {
		t.Fatalf("codex names %v", inv.CodexAccounts)
	}
	if len(inv.UsageHints) != 2 || inv.UsageHints[0].Name != "alice@synthetic.invalid" || len(inv.UsageHints[0].Windows) != 2 ||
		inv.UsageHints[0].Windows[1].UsedPercent != 42 || inv.UsageHints[1].Windows[0].Name != "five_hour" {
		t.Fatalf("hints %+v", inv.UsageHints)
	}
	if !inv.UsageHints[0].FetchedAt.Equal(time.UnixMilli(1791150000000).UTC()) {
		t.Fatal("fetchedAt")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		b, _ := os.ReadFile(marker)
		t.Fatalf("a rota command other than --help was executed: %s", b)
	}
	if len(inv.Notes) != 0 {
		t.Fatalf("notes %v", inv.Notes)
	}
}

func TestDetectWithoutRunningAnything(t *testing.T) {
	opts, marker, _ := setup(t)
	st := Detect(context.Background(), opts)
	if !st.Present || st.Health != "ok (file)" {
		t.Fatalf("%+v", st)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("rota executed")
	}
	st = Detect(context.Background(), Options{RotaPath: filepath.Join(t.TempDir(), "missing")})
	if st.Present {
		t.Fatal("missing rota reported present")
	}
}

func TestHintsAreDisplayOnly(t *testing.T) {
	opts, _, _ := setup(t)
	inv, _ := Scan(context.Background(), opts)
	o := inv.UsageHints[0].Observation("acct_synth-a", "node_synth")
	if !o.Stale || o.Eligibility != contracts.EligibleUnknown || o.Confidence != "low" || o.Source != "rota:usage-cache" {
		t.Fatalf("%+v", o)
	}
}

func TestMappings(t *testing.T) {
	inv := Inventory{ClaudeAccounts: []string{"alice@synthetic.invalid", "work-b", "orphan"}, CodexAccounts: []string{"personal"}}
	accts := []contracts.Account{
		{AccountID: "acct_a", Provider: contracts.ProviderAnthropic, Label: "alice", RotaRef: ""},
		{AccountID: "acct_b", Provider: contracts.ProviderAnthropic, Label: "B", RotaRef: "work-b"},
		{AccountID: "acct_p", Provider: contracts.ProviderOpenAI, Label: "personal"},
		{AccountID: "acct_wrong", Provider: contracts.ProviderOpenAI, Label: "alice"},
	}
	ms := SuggestMappings(inv, accts)
	got := map[string]Mapping{}
	for _, m := range ms {
		got[m.RotaName] = m
	}
	if m := got["alice@synthetic.invalid"]; m.AccountID != "acct_a" || m.Confirmed {
		t.Fatalf("alice %+v", m)
	}
	if m := got["work-b"]; m.AccountID != "acct_b" || !m.Confirmed {
		t.Fatalf("work-b %+v", m)
	}
	if m := got["orphan"]; m.AccountID != "" {
		t.Fatalf("orphan %+v", m)
	}
	if m := got["personal"]; m.AccountID != "acct_p" || m.Provider != contracts.ProviderOpenAI {
		t.Fatalf("personal %+v", m)
	}
	if a := ApplyMapping(accts[0], "alice@synthetic.invalid"); a.RotaRef != "alice@synthetic.invalid" {
		t.Fatal("apply")
	}
}

func TestOversizedOrBadCache(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, ".usage-cache.json")
	os.WriteFile(p, []byte(strings.Repeat(" ", 2048)+"{}"), 0o600)
	if _, err := ReadUsageCache(p, 1024); err == nil {
		t.Fatal("oversized cache accepted")
	}
	os.WriteFile(p, []byte("{not json"), 0o600)
	if _, err := ReadUsageCache(p, 0); err == nil || strings.Contains(err.Error(), "not json") {
		t.Fatalf("bad cache: %v", err)
	}
}

// TestRealNodeReadOnly inventories the real rota on this machine when
// PYRE_ROTA_REAL=1. It is read-only (directory listings, the rota script
// file, the usage cache) and logs counts only: no names, no numbers.
func TestRealNodeReadOnly(t *testing.T) {
	if os.Getenv("PYRE_ROTA_REAL") != "1" {
		t.Skip("set PYRE_ROTA_REAL=1 to inventory the real rota (read-only)")
	}
	inv, err := Scan(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("rota present=%v health=%q revision=%s claude_snapshots=%d codex_snapshots=%d usage_hints=%d notes=%d",
		inv.Status.Present, inv.Status.Health, inv.Status.Revision, len(inv.ClaudeAccounts), len(inv.CodexAccounts), len(inv.UsageHints), len(inv.Notes))
}
