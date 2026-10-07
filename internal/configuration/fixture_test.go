package configuration

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/extensions"
)

// Synthetic config homes. Everything here is obviously fake.
const (
	synthClaudeSettings = `{
  "model": "default",
  "permissions": {
    "defaultMode": "acceptEdits"
  },
  "env": {
    "ANTHROPIC_API_KEY": "sk-ant-synthetic-0000000000000000"
  },
  "hooks": {
    "Stop": [
      {"hooks": [{"type": "command", "command": "/opt/synth/user-stop-hook"}]}
    ],
    "PreToolUse": [
      {"matcher": "Bash", "hooks": [{"type": "command", "command": "pyre hook claude"}]}
    ]
  },
  "enabledPlugins": {
    "synth-lsp@synth-market": true
  }
}
`
	synthClaudeJSON = `{
  "numStartups": 3,
  "mcpServers": {
    "pyre": {"command": "pyre", "args": ["mcp"]},
    "user-tool": {"command": "user-tool", "env": {"TOOL_TOKEN": "synthetic-secret-value-123"}}
  }
}
`
	synthCodexConfig = `model = "gpt-synth"
approval_policy = "on-request"

[mcp_servers.node_repl]
command = "node"
args = []

[plugins."browser@synth-bundled"]
enabled = true

[hooks.state."/synth/hooks.json:stop:0:0"]
trusted_hash = "synthetic"
`
	synthCodexHooks = `{"hooks": {"Stop": [{"hooks": [{"type": "command", "command": "/opt/synth/codex-stop"}]}]}}`
)

type fakeRuns struct {
	mu   sync.Mutex
	runs []RunRef
}

func (f *fakeRuns) ActiveRuns(ctx context.Context) ([]RunRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]RunRef(nil), f.runs...), nil
}

type fakeVersions struct{ v map[string]string }

func (f fakeVersions) ActiveVersions(ctx context.Context) map[string]string { return f.v }
func (f fakeVersions) Pinned(ctx context.Context, tool string) (Pin, bool)  { return Pin{}, false }

type recReporter struct {
	mu    sync.Mutex
	kinds []contracts.ReportKind
	evs   []contracts.Event
}

func (r *recReporter) Report(ctx context.Context, kind contracts.ReportKind, payload any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.kinds = append(r.kinds, kind)
	if ev, ok := payload.(contracts.Event); ok {
		r.evs = append(r.evs, ev)
	}
	return nil
}

type fixedClock struct{}

func (fixedClock) Now() time.Time { return time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC) }

type env struct {
	t        *testing.T
	home     string
	state    string
	skillSrc string
	project  string
	m        *Manager
	runs     *fakeRuns
	rep      *recReporter
}

func write(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const synthProject = "prj_synth-outfit"

func newEnv(t *testing.T) *env {
	t.Helper()
	root := t.TempDir()
	e := &env{t: t, home: filepath.Join(root, "home"), state: filepath.Join(root, "state"), runs: &fakeRuns{}, rep: &recReporter{}}
	h := e.home
	write(t, filepath.Join(h, ".claude/settings.json"), synthClaudeSettings)
	write(t, filepath.Join(h, ".claude/skills/my-local-skill/SKILL.md"), "---\nname: my-local-skill\n---\nsynthetic\n")
	write(t, filepath.Join(h, ".claude/commands/synth-cmd.md"), "synthetic command\n")
	plugDir := filepath.Join(h, ".claude/plugins/cache/synth-market/synth-lsp/1.0.0")
	write(t, filepath.Join(plugDir, "skills/lsp-help/SKILL.md"), "synthetic plugin skill\n")
	write(t, filepath.Join(h, ".claude/plugins/installed_plugins.json"),
		`{"version":2,"plugins":{"synth-lsp@synth-market":[{"scope":"user","installPath":"`+plugDir+`","version":"1.0.0"}]}}`)
	write(t, filepath.Join(h, ".claude.json"), synthClaudeJSON)
	write(t, filepath.Join(h, ".codex/config.toml"), synthCodexConfig)
	write(t, filepath.Join(h, ".codex/hooks.json"), synthCodexHooks)
	write(t, filepath.Join(h, ".codex/skills/.system/imagegen/SKILL.md"), "synthetic system skill\n")
	write(t, filepath.Join(h, ".codex/skills/codex-user-skill/SKILL.md"), "synthetic codex skill\n")
	e.skillSrc = filepath.Join(root, "outfit-store/synth-review")
	write(t, filepath.Join(e.skillSrc, "SKILL.md"), "---\nname: synth-review\n---\nv1\n")
	write(t, filepath.Join(e.skillSrc, "references/checklist.md"), "synthetic checklist\n")
	e.project = filepath.Join(root, "work/synth-project")
	write(t, filepath.Join(e.project, ".claude/settings.json"), `{"permissions":{"allow":["Bash(make test)"]}}`)

	m, err := New(Options{
		StateDir: e.state,
		NodeID:   "node_synth-mac",
		Homes:    extensions.DefaultHomes(h),
		ProjectDirs: func(ctx context.Context, id string) []string {
			if id == synthProject {
				return []string{e.project}
			}
			return nil
		},
		Runs:     e.runs,
		Versions: fakeVersions{v: map[string]string{"claude": "2.1.289", "codex": "0.160.0"}},
		Reporter: e.rep,
		Clock:    fixedClock{},
	})
	if err != nil {
		t.Fatal(err)
	}
	e.m = m
	return e
}

func (e *env) globalProfile() Profile {
	return Profile{
		Name: "default",
		Settings: map[string]any{
			"claude.permissions.defaultMode": "default",
			"claude.cleanupPeriodDays":       30,
			"codex.model_reasoning_effort":   "high",
		},
		Persist: []string{"claude.cleanupPeriodDays"},
		Skills:  []SkillSpec{{Name: "synth-review", Version: "1.0.0", SourceDir: e.skillSrc}},
		MCPServers: []MCPServerSpec{
			{Name: "pyre", Command: "pyre", Args: []string{"mcp"}},
			{Name: "synth-mcp", Command: "synth-mcp", Args: []string{"--stdio"}, Env: map[string]string{"SYNTH_MODE": "test"}},
		},
		Hooks: []HookSpec{
			{Harness: contracts.HarnessClaude, Event: "PreToolUse", Matcher: "Bash", Command: "pyre hook claude"},
			{Harness: contracts.HarnessClaude, Event: "Stop", Command: "pyre hook claude-stop"},
			{Harness: contracts.HarnessCodex, Event: "Stop", Command: "pyre hook codex-stop"},
		},
		Connections: []string{"conn_synth-github"},
	}
}

func (e *env) saveGlobal(p Profile) {
	e.t.Helper()
	if err := e.m.Profiles().Save("", p); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) planApply(scope string) (contracts.ChangePlan, contracts.EnvironmentGeneration) {
	e.t.Helper()
	ctx := context.Background()
	plan, err := e.m.Plan(ctx, contracts.EnvPlanParams{Scope: scope})
	if err != nil {
		e.t.Fatal(err)
	}
	gen, err := e.m.Apply(ctx, contracts.EnvApplyParams{PlanID: plan.PlanID, ResultDigest: plan.ResultDigest})
	if err != nil {
		e.t.Fatalf("apply: %v (conflicts %+v)", err, plan.Conflicts)
	}
	return plan, gen
}
