package codex

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A profile run mirrors the owner's trust of exactly the hook content the
// owner trusted ("Hooks need review", Codex 0.162+): a copied hooks.json
// hook under the profile's path gets the owner's record for the same file
// under the owner's home; a changed plugin hook, a hook the owner never
// trusted and one already trusted are left alone; the owner's enabled=false
// is kept. The owner's own config is never written.
func TestCodexProfileMirrorsOwnerHookTrust(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	owner := e.o.OwnerCodexHome
	if err := os.MkdirAll(owner, 0o700); err != nil {
		t.Fatal(err)
	}
	ownerCfg := `model = "synthetic"

[hooks.state."` + owner + `/hooks.json:stop:0:0"]
trusted_hash = "sha256:aaa"

[hooks.state."` + owner + `/hooks.json:post_tool_use:0:0"]
trusted_hash = "sha256:ddd"
enabled = false

[hooks.state."synthetic@market:hooks/codex-hooks.json:session_start:0:0"]
trusted_hash = "sha256:old"

[hooks.state."synthetic@market:hooks/hooks.json:stop:0:0"]
trusted_hash = "sha256:eee"
trusted_content_hash = "sha256:script-eee"
synthetic_future_field = 7

[hooks.state."synthetic@market:hooks/hooks.json:session_start:0:0"]
trusted_hash = "sha256:fff"
trusted_content_hash = "sha256:script-new"

[hooks.state."synthetic@market:hooks/hooks.json:user_prompt_submit:0:0"]
trusted_hash = "sha256:ggg"
trusted_content_hash = "sha256:script-old"
`
	if err := os.WriteFile(filepath.Join(owner, "config.toml"), []byte(ownerCfg), 0o600); err != nil {
		t.Fatal(err)
	}
	hooks := []map[string]any{
		{"key": e.home + "/hooks.json:stop:0:0", "currentHash": "sha256:aaa", "trustStatus": "untrusted"},
		{"key": e.home + "/hooks.json:post_tool_use:0:0", "currentHash": "sha256:ddd", "trustStatus": "untrusted"},
		{"key": "synthetic@market:hooks/codex-hooks.json:session_start:0:0", "currentHash": "sha256:new", "trustStatus": "modified"},
		{"key": e.home + "/hooks.json:user_prompt_submit:0:0", "currentHash": "sha256:bbb", "trustStatus": "untrusted"},
		{"key": e.home + "/config.toml:stop:0:0", "currentHash": "sha256:ccc", "trustStatus": "trusted"},
		{"key": "synthetic@market:hooks/hooks.json:stop:0:0", "currentHash": "sha256:eee", "trustStatus": "untrusted"},
		// Codex 0.166+: "<trusted_hash>/<trusted_content_hash>". The owner
		// trusted the updated script; the profile still has the old one.
		{"key": "synthetic@market:hooks/hooks.json:session_start:0:0", "currentHash": "sha256:fff/sha256:script-new", "trustStatus": "modified"},
		// The script changed and the owner has not trusted the new one.
		{"key": "synthetic@market:hooks/hooks.json:user_prompt_submit:0:0", "currentHash": "sha256:ggg/sha256:script-new", "trustStatus": "modified"},
	}
	b, _ := json.Marshal(hooks)
	if err := os.WriteFile(filepath.Join(e.home, "fake-hooks.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	e.launch(e.a, "run_ht1", e.home, nil, nil)
	log, _ := os.ReadFile(filepath.Join(e.o.StateRoot, "codex", homeKey(e.home), "fake-config.log"))
	var write string
	for _, l := range strings.Split(string(log), "\n") {
		if strings.HasPrefix(l, "config/value/write ") && strings.Contains(l, `"keyPath":"hooks.state"`) {
			if write != "" {
				t.Fatalf("hook trust written twice:\n%s", log)
			}
			write = l
		}
	}
	var got struct {
		Config struct {
			MergeStrategy string               `json:"mergeStrategy"`
			Value         map[string]hookState `json:"value"`
		}
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(write, "config/value/write ")), &got.Config); err != nil {
		t.Fatalf("no hook trust write (%v):\n%s", err, log)
	}
	if got.Config.MergeStrategy != "upsert" || len(got.Config.Value) != 4 {
		t.Fatalf("mirrored %+v", got.Config)
	}
	if st := got.Config.Value[e.home+"/hooks.json:stop:0:0"]; len(st) != 1 || st["trusted_hash"] != "sha256:aaa" {
		t.Fatalf("stop hook: %+v", st)
	}
	if st := got.Config.Value[e.home+"/hooks.json:post_tool_use:0:0"]; len(st) != 2 || st["trusted_hash"] != "sha256:ddd" || st["enabled"] != false {
		t.Fatalf("disabled hook: %+v", st)
	}
	// Codex 0.166 records the script's hash beside trusted_hash; every
	// field of the owner's record is mirrored, a future one included.
	if st := got.Config.Value["synthetic@market:hooks/hooks.json:stop:0:0"]; len(st) != 3 || st["trusted_hash"] != "sha256:eee" ||
		st["trusted_content_hash"] != "sha256:script-eee" || st["synthetic_future_field"] != float64(7) {
		t.Fatalf("0.166 hook: %+v", st)
	}
	if st := got.Config.Value["synthetic@market:hooks/hooks.json:session_start:0:0"]; st["trusted_hash"] != "sha256:fff" || st["trusted_content_hash"] != "sha256:script-new" {
		t.Fatalf("updated script hook: %+v", st)
	}
	if _, ok := got.Config.Value["synthetic@market:hooks/hooks.json:user_prompt_submit:0:0"]; ok {
		t.Fatal("mirrored a script the owner never trusted")
	}
	if after, _ := os.ReadFile(filepath.Join(owner, "config.toml")); string(after) != ownerCfg {
		t.Fatal("the owner's config changed")
	}

	// An app-server without hooks/list (Codex before 0.162): nothing written.
	e2 := newEnv(t, nil)
	_ = os.MkdirAll(e2.o.OwnerCodexHome, 0o700)
	_ = os.WriteFile(filepath.Join(e2.o.OwnerCodexHome, "config.toml"), []byte(ownerCfg), 0o600)
	e2.launch(e2.a, "run_ht2", e2.home, nil, nil)
	log, _ = os.ReadFile(filepath.Join(e2.o.StateRoot, "codex", homeKey(e2.home), "fake-config.log"))
	if strings.Contains(string(log), `"keyPath":"hooks.state"`) {
		t.Fatalf("hook trust written without hooks/list:\n%s", log)
	}
}

func TestOwnerHookKey(t *testing.T) {
	for _, tc := range []struct{ key, want string }{
		{"/p/codex/hooks.json:stop:0:0", "/o/.codex/hooks.json:stop:0:0"},
		{"/p/codex/config.toml:stop:1:2", "/o/.codex/config.toml:stop:1:2"},
		{"/p/codex-other/hooks.json:stop:0:0", "/p/codex-other/hooks.json:stop:0:0"},
		{"hive@hive:hooks/codex-hooks.json:session_start:0:0", "hive@hive:hooks/codex-hooks.json:session_start:0:0"},
	} {
		if got := ownerHookKey(tc.key, "/p/codex", "/o/.codex"); got != tc.want {
			t.Errorf("%s: %s, want %s", tc.key, got, tc.want)
		}
	}
}

func TestHookStateTrusts(t *testing.T) {
	st := hookState{"trusted_hash": "sha256:a", "trusted_content_hash": "sha256:b"}
	for cur, want := range map[string]bool{
		"sha256:a": true, "sha256:a/sha256:b": true, "sha256:a/sha256:c": false,
		"sha256:x/sha256:b": false, "": false, "/sha256:b": false,
	} {
		if got := st.trusts(cur); got != want {
			t.Errorf("trusts(%q) = %v, want %v", cur, got, want)
		}
	}
	if (hookState{"trusted_hash": "sha256:a"}).trusts("sha256:a/sha256:b") {
		t.Error("a record without trusted_content_hash trusted a script hash")
	}
}
