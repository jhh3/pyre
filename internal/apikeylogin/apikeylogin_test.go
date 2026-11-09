package apikeylogin

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func write(t *testing.T, p, s string) {
	t.Helper()
	_ = os.MkdirAll(filepath.Dir(p), 0o700)
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCodex(t *testing.T) {
	d := t.TempDir()
	if Codex(d) != "" {
		t.Fatal("no auth.json is not an API-key login")
	}
	for _, tc := range []struct {
		auth string
		key  bool
	}{
		{`{"auth_mode":"apikey","OPENAI_API_KEY":"synthetic-not-a-key"}`, true},
		{`{"OPENAI_API_KEY":"synthetic-not-a-key"}`, true},
		{`{"auth_mode":"apikey"}`, true},
		{`{"auth_mode":"chatgpt","OPENAI_API_KEY":null,"tokens":{"access_token":"a","refresh_token":"r"}}`, false},
		{`{"OPENAI_API_KEY":"synthetic-not-a-key","tokens":{"refresh_token":"r"}}`, false},
		{`not json`, false},
	} {
		write(t, filepath.Join(d, "auth.json"), tc.auth)
		why := Codex(d)
		if (why != "") != tc.key || strings.Contains(why, "synthetic-not-a-key") {
			t.Errorf("%s: %q", tc.auth, why)
		}
	}
}

func TestClaude(t *testing.T) {
	d := t.TempDir()
	if Claude(d) != "" {
		t.Fatal("empty config dir is not an API-key login")
	}
	write(t, filepath.Join(d, "settings.json"), `{"model":"opus"}`)
	write(t, filepath.Join(d, ".claude.json"), `{"oauthAccount":{"accountUuid":"u"}}`)
	if why := Claude(d); why != "" {
		t.Fatalf("subscription login: %q", why)
	}
	write(t, filepath.Join(d, ".claude.json"), `{"primaryApiKey":"synthetic-not-a-key"}`)
	if why := Claude(d); !strings.Contains(why, "Console login") || strings.Contains(why, "synthetic-not-a-key") {
		t.Fatalf("console key: %q", why)
	}
	write(t, filepath.Join(d, ".claude.json"), `{}`)
	write(t, filepath.Join(d, "settings.local.json"), `{"apiKeyHelper":"/synthetic/helper"}`)
	if why := Claude(d); !strings.Contains(why, "apiKeyHelper") {
		t.Fatalf("helper: %q", why)
	}
	if Check("agy", d) != "" || Check("claude", d) == "" {
		t.Fatal("Check")
	}
}

func TestNoLogin(t *testing.T) {
	none := func(string) string { return "" }
	d := t.TempDir()
	if !strings.Contains(NoLogin("codex", d, none), "no own codex login") {
		t.Fatal("codex without auth.json has no login")
	}
	write(t, filepath.Join(d, "auth.json"), `{"auth_mode":"chatgpt"}`)
	if NoLogin("codex", d, none) != "" {
		t.Fatal("codex with auth.json has a login")
	}
	if NoLogin("agy", d, none) != "" {
		t.Fatal("other harnesses are not judged")
	}
	if runtime.GOOS == "darwin" {
		return
	}
	c := t.TempDir()
	if !strings.Contains(NoLogin("claude", c, none), "no own claude login") {
		t.Fatal("claude without .credentials.json has no login")
	}
	if NoLogin("claude", c, func(k string) string {
		if k == "CLAUDE_CODE_OAUTH_TOKEN" {
			return "synthetic"
		}
		return ""
	}) != "" {
		t.Fatal("CLAUDE_CODE_OAUTH_TOKEN is a login")
	}
	write(t, filepath.Join(c, ".credentials.json"), `{}`)
	if NoLogin("claude", c, none) != "" {
		t.Fatal("claude with .credentials.json has a login")
	}
}
