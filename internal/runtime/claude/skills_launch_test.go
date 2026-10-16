package claude

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/claudeplugin"
)

// TestLaunchMaterializesSkills: launch writes the agent slash commands into
// the session's config dir (bare /<name>) and leaves an owner's same-named
// skill alone; with no doorbell and no plugin delivery there is no plugin.
func TestLaunchMaterializesSkills(t *testing.T) {
	f := newFakeEnv(t, func(o *Options) { o.DisablePluginDelivery = true })
	cfg := filepath.Join(f.dir, "cfg")
	own := filepath.Join(cfg, "skills", "pair", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(own), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(own, []byte("synthetic owner pair"), 0o600); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(f.dir, "work")
	_ = os.MkdirAll(work, 0o755)
	argvFile := filepath.Join(f.dir, "argv")
	h, err := f.a.Launch(context.Background(), contracts.ResolvedLaunch{
		RunID: "run_skills", SessionID: "ses_t", Harness: contracts.HarnessClaude, Cwd: work,
		Env:      map[string]string{"PYRE_TEST_ROLE": "fakeclaude", "PYRE_SESSION_ID": "ses_t", "FAKE_ARGV_FILE": argvFile},
		StateDir: filepath.Join(f.dir, "runs", "run_skills"), ProfileDir: cfg,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range claudeplugin.Skills() {
		b, err := os.ReadFile(filepath.Join(cfg, "skills", s.Name, "SKILL.md"))
		if err != nil {
			t.Fatal(err)
		}
		if s.Name == "pair" {
			if string(b) != "synthetic owner pair" {
				t.Fatalf("owner's /pair overwritten: %q", b)
			}
			continue
		}
		if string(b) != s.Body {
			t.Fatalf("%s not materialized", s.Name)
		}
	}
	var argv []byte
	for i := 0; i < 100 && len(argv) == 0; i++ {
		argv, _ = os.ReadFile(argvFile)
		time.Sleep(50 * time.Millisecond)
	}
	if len(argv) == 0 || strings.Contains(string(argv), "--plugin-dir") {
		t.Fatalf("argv %q: want no plugin without doorbell or delivery", argv)
	}
	if _, err := os.Stat(filepath.Join(h.StateDir, "pyre-plugin")); err == nil {
		t.Fatal("plugin generated without doorbell or delivery")
	}
}
