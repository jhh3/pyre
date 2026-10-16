package cli

import (
	"path/filepath"
	"testing"

	"github.com/jhh3/pyre/internal/clientconn"
)

func TestTUIWebURLPrecedence(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "client.json")
	env := map[string]string{}
	getenv := func(k string) string { return env[k] }

	// No config, no env: the TUI's default.
	if got := tuiWebURL(getenv, cfgPath); got != "" {
		t.Fatalf("no config: %q, want empty (tui default)", got)
	}
	// The client config's web_url.
	if err := clientconn.SaveConfig(cfgPath, clientconn.ClientConfig{HubURL: "https://hub.example.test", Token: "synthetic-token",
		WebURL: "https://web.example.test"}); err != nil {
		t.Fatal(err)
	}
	if got := tuiWebURL(getenv, cfgPath); got != "https://web.example.test" {
		t.Fatalf("config web_url: %q", got)
	}
	// PYRE_WEB_URL overrides it.
	env[clientconn.EnvWebURL] = " https://override.example.test "
	if got := tuiWebURL(getenv, cfgPath); got != "https://override.example.test" {
		t.Fatalf("env override: %q", got)
	}
}
