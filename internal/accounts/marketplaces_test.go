package accounts

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// TestUnreadableMarketplaceLeftOutOfProfile: an owner "directory"
// marketplace that the daemon cannot read in time (laptop: under
// ~/Documents, blocked by a macOS privacy prompt for the pyred LaunchAgent,
// which hung Claude at startup with a blank pane) is dropped from the
// profile's settings.json and known_marketplaces.json copies together with
// the plugins enabled from it; readable marketplaces, other settings and
// the owner's own files are untouched, and the gap is reported.
func TestUnreadableMarketplaceLeftOutOfProfile(t *testing.T) {
	e := newEnv(t)
	blockedDir := filepath.Join(t.TempDir(), "Documents", "synthetic-mkt")
	okDir := filepath.Join(t.TempDir(), "synthetic-ok-mkt")
	writeIn(t, okDir, ".claude-plugin/marketplace.json", `{"name":"okmkt"}`)

	release := make(chan struct{})
	defer close(release)
	oldProbe, oldTimeout := readDirProbe, marketplaceProbeTimeout
	readDirProbe = func(dir string) error {
		if dir == blockedDir {
			<-release // a privacy prompt nobody answers
			return nil
		}
		return oldProbe(dir)
	}
	marketplaceProbeTimeout = 100 * time.Millisecond
	defer func() { readDirProbe, marketplaceProbeTimeout = oldProbe, oldTimeout }()

	settings := `{"model":"synthetic","enabledPlugins":{"synth-bridge@blocked":true,"good@okmkt":true,"gh@github-mkt":true},` +
		`"extraKnownMarketplaces":{"blocked":{"source":{"source":"directory","path":"` + blockedDir + `"}},` +
		`"okmkt":{"source":{"source":"directory","path":"` + okDir + `"}}},"allowedChannelPlugins":[{"marketplace":"blocked","plugin":"synth-bridge"}]}`
	known := `{"blocked":{"source":{"source":"directory","path":"` + blockedDir + `"},"installLocation":"` + blockedDir + `"},` +
		`"github-mkt":{"source":{"source":"github","repo":"synthetic/mkt"},"installLocation":"/nonexistent/synthetic"}}`
	writeIn(t, e.userClaude, "settings.json", settings)
	writeIn(t, e.userClaude, "plugins/known_marketplaces.json", known)

	acct := contracts.Account{AccountID: "acct_synth-mkt", Provider: contracts.ProviderAnthropic}
	start := time.Now()
	info, err := e.svc.CreateProfile(context.Background(), acct, contracts.HarnessClaude)
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("composition took %s; a blocked marketplace must not block it", d)
	}
	if len(info.Unavailable) != 1 || !strings.Contains(info.Unavailable[0], `"blocked"`) || !strings.Contains(info.Unavailable[0], blockedDir) {
		t.Fatalf("unavailable = %q", info.Unavailable)
	}

	var got struct {
		Model          string                     `json:"model"`
		Enabled        map[string]bool            `json:"enabledPlugins"`
		Extra          map[string]json.RawMessage `json:"extraKnownMarketplaces"`
		AllowedChannel []json.RawMessage          `json:"allowedChannelPlugins"`
	}
	b, err := os.ReadFile(filepath.Join(info.Dir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.Model != "synthetic" || got.Enabled["synth-bridge@blocked"] || !got.Enabled["good@okmkt"] || !got.Enabled["gh@github-mkt"] {
		t.Fatalf("settings copy: %s", b)
	}
	if _, ok := got.Extra["blocked"]; ok || got.Extra["okmkt"] == nil || len(got.AllowedChannel) != 1 {
		t.Fatalf("settings copy marketplaces: %s", b)
	}
	kb, err := os.ReadFile(filepath.Join(info.Dir, "plugins", "known_marketplaces.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(kb), `"blocked"`) || !strings.Contains(string(kb), `"github-mkt"`) {
		t.Fatalf("known_marketplaces copy: %s", kb)
	}
	// The owner's files are unchanged.
	if ob, _ := os.ReadFile(filepath.Join(e.userClaude, "settings.json")); string(ob) != settings {
		t.Fatalf("owner settings changed: %s", ob)
	}
	if ob, _ := os.ReadFile(filepath.Join(e.userClaude, "plugins", "known_marketplaces.json")); string(ob) != known {
		t.Fatalf("owner known_marketplaces changed: %s", ob)
	}

	// A launch refresh while the first probe is still blocked answers at once
	// (no second goroutine) and keeps the marketplace out.
	start = time.Now()
	e.svc.refreshComposition(info.Dir, contracts.HarnessClaude)
	if d := time.Since(start); d > marketplaceProbeTimeout {
		t.Fatalf("refresh with a probe in flight took %s", d)
	}
	if b, _ := os.ReadFile(filepath.Join(info.Dir, "settings.json")); strings.Contains(string(b), "synth-bridge@blocked") {
		t.Fatalf("refresh restored the blocked plugin: %s", b)
	}
}

// TestReadableMarketplacesUntouched: with every local marketplace readable
// the settings copy is byte-identical to the owner's.
func TestReadableMarketplacesUntouched(t *testing.T) {
	e := newEnv(t)
	okDir := filepath.Join(t.TempDir(), "synthetic-ok-mkt")
	writeIn(t, okDir, ".claude-plugin/marketplace.json", `{"name":"okmkt"}`)
	settings := `{"enabledPlugins":{"good@okmkt":true},"extraKnownMarketplaces":{"okmkt":{"source":{"source":"directory","path":"` + okDir + `"}}}}`
	writeIn(t, e.userClaude, "settings.json", settings)
	info, err := e.svc.CreateProfile(context.Background(), contracts.Account{AccountID: "acct_synth-ok", Provider: contracts.ProviderAnthropic}, contracts.HarnessClaude)
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Unavailable) != 0 {
		t.Fatalf("unavailable = %q", info.Unavailable)
	}
	if b, _ := os.ReadFile(filepath.Join(info.Dir, "settings.json")); string(b) != settings {
		t.Fatalf("settings copy changed: %s", b)
	}
}
