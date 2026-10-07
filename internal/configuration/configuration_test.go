package configuration

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/extensions"
)

func findValue(g contracts.EnvironmentGeneration, key string) (contracts.ResolvedValue, bool) {
	for _, v := range g.Values {
		if v.Key == key {
			return v, true
		}
	}
	return contracts.ResolvedValue{}, false
}

func hasChange(p contracts.ChangePlan, pathSub, action string) *contracts.PlanChange {
	for i, c := range p.Changes {
		if strings.Contains(c.Path, pathSub) && c.Action == action {
			return &p.Changes[i]
		}
	}
	return nil
}

func argValue(args []string, key string) (string, bool) {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-c" && strings.HasPrefix(args[i+1], key+"=") {
			return strings.TrimPrefix(args[i+1], key+"="), true
		}
	}
	return "", false
}

func flagValue(args []string, flag string) string {
	for i := 0; i < len(args); i++ {
		if strings.HasPrefix(args[i], flag+"=") {
			return strings.TrimPrefix(args[i], flag+"=")
		}
		if args[i] == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// A15: adopt an existing setup, preview, apply global; unmanaged items and
// user files are preserved; managed items carry ownership markers.
func TestAdoptApplyPreservesUnmanaged(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	codexBefore := read(t, filepath.Join(e.home, ".codex/config.toml"))
	claudeJSONBefore := read(t, filepath.Join(e.home, ".claude.json"))
	hooksBefore := read(t, filepath.Join(e.home, ".codex/hooks.json"))

	prev, err := e.m.AdoptionPreview(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prev.Summary, "will be referenced, nothing moved") || prev.Unmanaged == 0 {
		t.Fatalf("preview: %+v", prev.Summary)
	}

	e.saveGlobal(e.globalProfile())
	plan, gen := e.planApply("global")
	if len(plan.Conflicts) != 0 {
		t.Fatalf("unexpected conflicts: %+v", plan.Conflicts)
	}
	if hasChange(plan, ".claude/skills/synth-review", "create") == nil || hasChange(plan, ".codex/skills/synth-review", "create") == nil {
		t.Fatalf("skill creates missing: %+v", plan.Changes)
	}
	pc := hasChange(plan, ".claude/settings.json#pyre-managed", "update")
	if pc == nil || pc.Backup == "" || !strings.Contains(pc.Diff, "+ cleanupPeriodDays: 30") {
		t.Fatalf("managed key change: %+v", pc)
	}
	reused := false
	for _, c := range plan.Changes {
		if c.Path == "claude:mcp:pyre" && strings.Contains(c.Diff, "reused, not registered twice") {
			reused = true
		}
	}
	if !reused {
		t.Fatalf("pyre MCP not deduplicated against ~/.claude.json: %+v", plan.Changes)
	}

	// User files outside Pyre-owned keys are untouched.
	if read(t, filepath.Join(e.home, ".codex/config.toml")) != codexBefore {
		t.Fatal("codex config.toml was modified")
	}
	if read(t, filepath.Join(e.home, ".claude.json")) != claudeJSONBefore {
		t.Fatal("~/.claude.json was modified")
	}
	if read(t, filepath.Join(e.home, ".codex/hooks.json")) != hooksBefore {
		t.Fatal("codex hooks.json was modified")
	}
	settings := read(t, filepath.Join(e.home, ".claude/settings.json"))
	var sm map[string]any
	if err := json.Unmarshal([]byte(settings), &sm); err != nil {
		t.Fatal(err)
	}
	if sm["model"] != "default" || sm["cleanupPeriodDays"] != float64(30) {
		t.Fatalf("settings after apply: %v", sm)
	}
	if hooks := sm["hooks"].(map[string]any); len(hooks) != 2 {
		t.Fatalf("user hooks changed: %v", hooks)
	}
	// Key order preserved: model stays first, the managed key is appended.
	if !strings.HasPrefix(strings.TrimSpace(settings), "{\n  \"model\"") || strings.Index(settings, "cleanupPeriodDays") < strings.Index(settings, "enabledPlugins") {
		t.Fatalf("key order not preserved:\n%s", settings)
	}
	if _, ok := extensions.ReadMarker(filepath.Join(e.home, ".claude/skills/synth-review")); !ok {
		t.Fatal("managed skill has no ownership marker")
	}
	if _, ok := extensions.ReadMarker(filepath.Join(e.home, ".claude/skills/my-local-skill")); ok {
		t.Fatal("user skill got a marker")
	}
	// Generation content.
	if gen.NodeID != "node_synth-mac" || gen.Digest == "" || !contracts.ValidID(gen.GenerationID, contracts.PrefixEnvironment) {
		t.Fatalf("gen: %+v", gen)
	}
	if !reflect.DeepEqual(gen.ConnectionGrants, []string{"conn_synth-github"}) {
		t.Fatalf("grants: %v", gen.ConnectionGrants)
	}
	if gen.RuntimeVersions["claude"] != "2.1.289" {
		t.Fatalf("versions: %v", gen.RuntimeVersions)
	}
	var managedSkill, localSkill, systemSkill bool
	for _, s := range gen.Skills {
		switch {
		case s.Name == "synth-review" && s.Managed && s.Layer == contracts.LayerManagedGlobal:
			managedSkill = true
		case s.Name == "my-local-skill" && !s.Managed && s.Layer == contracts.LayerAdoptedUser:
			localSkill = true
		case s.Name == "imagegen" && s.Layer == contracts.LayerProviderDefault:
			systemSkill = true
		}
	}
	if !managedSkill || !localSkill || !systemSkill {
		t.Fatalf("skills: %+v", gen.Skills)
	}
	// Event + report emitted.
	if len(e.rep.evs) == 0 || e.rep.evs[len(e.rep.evs)-1].Kind != contracts.EvEnvironmentApplied {
		t.Fatalf("no environment.applied event: %+v", e.rep.kinds)
	}
	// Re-planning is a no-op: no duplicate registrations or writes.
	again, err := e.m.Plan(ctx, contracts.EnvPlanParams{Scope: "global"})
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Changes) != 0 || len(again.RestartRequired) != 0 {
		t.Fatalf("second plan not a no-op: %+v", again.Changes)
	}
	g2, err := e.m.Apply(ctx, contracts.EnvApplyParams{PlanID: again.PlanID, ResultDigest: again.ResultDigest})
	if err != nil || g2.GenerationID != gen.GenerationID {
		t.Fatalf("noop apply: %v %s", err, g2.GenerationID)
	}
	if strings.Count(read(t, filepath.Join(e.home, ".claude/settings.json")), "cleanupPeriodDays") != 1 {
		t.Fatal("managed key duplicated")
	}
}

// A15: overlays for both harnesses, with no duplicate hook/MCP registration.
func TestLaunchOverlaysBothHarnessesNoDuplicates(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.saveGlobal(e.globalProfile())
	_, applied := e.planApply("global")

	gen, lo, err := e.m.Resolve(ctx, Scope{Harness: contracts.HarnessClaude})
	if err != nil {
		t.Fatal(err)
	}
	if gen.GenerationID != applied.GenerationID {
		t.Fatalf("identical resolution should reuse the applied generation: %s vs %s", gen.GenerationID, applied.GenerationID)
	}
	if lo.Env["PYRE_ENV_GENERATION"] != gen.GenerationID {
		t.Fatalf("env: %v", lo.Env)
	}
	var settings map[string]any
	if err := json.Unmarshal([]byte(read(t, flagValue(lo.ExtraArgs, "--settings"))), &settings); err != nil {
		t.Fatal(err)
	}
	if settings["permissions"].(map[string]any)["defaultMode"] != "default" {
		t.Fatalf("overlay settings: %v", settings)
	}
	hooks := settings["hooks"].(map[string]any)
	if _, dup := hooks["PreToolUse"]; dup {
		t.Fatalf("PreToolUse hook duplicated (already in user settings): %v", hooks)
	}
	if stop := hooks["Stop"].([]any); len(stop) != 1 || !strings.Contains(mustJSON(stop), "pyre hook claude-stop") {
		t.Fatalf("Stop overlay: %v", stop)
	}
	if strings.Contains(mustJSON(settings), "/opt/synth/user-stop-hook") {
		t.Fatal("user hook copied into overlay (would run twice)")
	}
	var mcp struct {
		MCPServers map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(read(t, flagValue(lo.ExtraArgs, "--mcp-config"))), &mcp); err != nil {
		t.Fatal(err)
	}
	if _, dup := mcp.MCPServers["pyre"]; dup {
		t.Fatal("pyre MCP registered twice for claude")
	}
	if _, ok := mcp.MCPServers["synth-mcp"]; !ok {
		t.Fatalf("synth-mcp missing: %v", mcp.MCPServers)
	}

	_, clo, err := e.m.Resolve(ctx, Scope{Harness: contracts.HarnessCodex})
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := argValue(clo.ExtraArgs, "model_reasoning_effort"); v != `"high"` {
		t.Fatalf("codex args: %v", clo.ExtraArgs)
	}
	if v, _ := argValue(clo.ExtraArgs, "mcp_servers.pyre.command"); v != `"pyre"` {
		t.Fatalf("codex pyre MCP: %v", clo.ExtraArgs)
	}
	if v, _ := argValue(clo.ExtraArgs, "mcp_servers.synth-mcp.env"); v != `{SYNTH_MODE = "test"}` {
		t.Fatalf("codex env: %q", v)
	}
	hv, _ := argValue(clo.ExtraArgs, "hooks.Stop")
	if !strings.Contains(hv, "pyre hook codex-stop") || strings.Contains(hv, "/opt/synth/codex-stop") {
		t.Fatalf("codex hooks overlay: %q", hv)
	}
	if _, ok := argValue(clo.ExtraArgs, "model"); ok {
		t.Fatal("adopted user values must not be re-sent as overlays")
	}

	// Adapter (Pyre runtime) hooks merge into the same overlay and dedupe.
	g3, lo3, err := e.m.Resolve(ctx, Scope{Harness: contracts.HarnessClaude, AdapterHooks: []HookSpec{
		{Harness: contracts.HarnessClaude, Event: "PreToolUse", Matcher: "Bash", Command: "pyre hook claude"},
		{Harness: contracts.HarnessClaude, Event: "PermissionRequest", Command: "pyre hook claude-permission"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if g3.GenerationID == applied.GenerationID || g3.PreviousID != applied.GenerationID {
		t.Fatalf("adapter hooks should derive a new generation from the applied one: %+v", g3)
	}
	s3 := read(t, flagValue(lo3.ExtraArgs, "--settings"))
	if strings.Count(s3, "pyre hook claude\"") != 0 || !strings.Contains(s3, "claude-permission") {
		t.Fatalf("adapter overlay: %s", s3)
	}
	g4, _, _ := e.m.Resolve(ctx, Scope{Harness: contracts.HarnessClaude, AdapterHooks: []HookSpec{
		{Harness: contracts.HarnessClaude, Event: "PreToolUse", Matcher: "Bash", Command: "pyre hook claude"},
		{Harness: contracts.HarnessClaude, Event: "PermissionRequest", Command: "pyre hook claude-permission"},
	}})
	if g4.GenerationID != g3.GenerationID {
		t.Fatal("identical launch resolution minted a second generation")
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// Precedence and provenance across all five layers.
func TestPrecedenceProvenance(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.saveGlobal(e.globalProfile())
	if err := e.m.Profiles().Save(synthProject, Profile{Name: "default", Settings: map[string]any{"claude.permissions.defaultMode": "plan"}}); err != nil {
		t.Fatal(err)
	}
	gen, err := e.m.Preview(ctx, contracts.EnvPlanParams{Scope: "session:ses_synth-1", ProjectID: synthProject,
		Overrides: map[string]string{"claude.permissions.defaultMode": "acceptEdits", "claude.model": "opus"}})
	if err != nil {
		t.Fatal(err)
	}
	v, _ := findValue(gen, "claude.permissions.defaultMode")
	want := []contracts.ConfigLayer{contracts.LayerProviderDefault, contracts.LayerAdoptedUser, contracts.LayerManagedGlobal, contracts.LayerManagedProject}
	if v.Layer != contracts.LayerSession || v.Value != "acceptEdits" || !reflect.DeepEqual(v.Overrode, want) {
		t.Fatalf("defaultMode: %+v", v)
	}
	if v, _ := findValue(gen, "claude.model"); v.Layer != contracts.LayerSession || !reflect.DeepEqual(v.Overrode, []contracts.ConfigLayer{contracts.LayerAdoptedUser}) {
		t.Fatalf("model: %+v", v)
	}
	if v, _ := findValue(gen, "codex.approval_policy"); v.Layer != contracts.LayerAdoptedUser || !reflect.DeepEqual(v.Overrode, []contracts.ConfigLayer{contracts.LayerProviderDefault}) {
		t.Fatalf("approval_policy: %+v", v)
	}
	if v, ok := findValue(gen, "claude.permissions.allow"); !ok || v.Layer != contracts.LayerAdoptedUser {
		t.Fatalf("project-level user settings not adopted: %+v", v)
	}
	if v, _ := findValue(gen, "codex.model_reasoning_effort"); v.Layer != contracts.LayerManagedGlobal {
		t.Fatalf("effort: %+v", v)
	}
	if gen.ProjectID == nil || *gen.ProjectID != synthProject {
		t.Fatal("project id")
	}
	if _, ok := findValue(gen, "codex.hooks.state"); ok {
		t.Fatal("codex internal hook trust state adopted as configuration")
	}
}

// A15 drift: local edits are reported; managed conflicts block apply.
func TestDriftAfterLocalEdit(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.saveGlobal(e.globalProfile())
	e.planApply("global")
	if d, err := e.m.Drift(ctx); err != nil || len(d) != 0 {
		t.Fatalf("drift right after apply: %v %+v", err, d)
	}

	sp := filepath.Join(e.home, ".claude/settings.json")
	s := read(t, sp)
	s = strings.Replace(s, `"cleanupPeriodDays": 30`, `"cleanupPeriodDays": 7`, 1)
	s = strings.Replace(s, `"Stop": [`, `"UserPromptSubmit": [{"hooks": [{"type": "command", "command": "/opt/synth/new-hook"}]}], "Stop": [`, 1)
	write(t, sp, s)
	write(t, filepath.Join(e.home, ".claude/skills/new-user-skill/SKILL.md"), "synthetic new\n")
	cp := filepath.Join(e.home, ".codex/config.toml")
	write(t, cp, strings.Replace(read(t, cp), `model = "gpt-synth"`, `model = "gpt-synth-mini"`, 1))
	write(t, filepath.Join(e.home, ".codex/skills/synth-review/SKILL.md"), "locally edited\n")

	d, err := e.m.Drift(ctx)
	if err != nil {
		t.Fatal(err)
	}
	expect := map[string]bool{
		"modified|cleanupPeriodDays":                false,
		"unmanaged_addition|hooks.UserPromptSubmit": false,
		"unmanaged_addition|new-user-skill":         false,
		"modified|codex.model":                      false,
		"modified|skill:synth-review":               false,
	}
	for _, it := range d {
		k := it.Kind + "|" + it.Key
		if _, ok := expect[k]; ok {
			expect[k] = true
		}
		if it.Kind == "modified" && it.Key == "cleanupPeriodDays" && (it.Expected != "30" || it.Actual != "7") {
			t.Fatalf("modified values: %+v", it)
		}
		if it.Kind == "modified" && it.Key == "codex.model" && (it.Expected != "gpt-synth" || it.Actual != "gpt-synth-mini") {
			t.Fatalf("codex modified: %+v", it)
		}
		if it.Kind == "unmanaged_addition" && it.Key == "hooks.UserPromptSubmit" && it.Path != "~/.claude/settings.json" {
			t.Fatalf("hook path: %+v", it)
		}
	}
	for k, seen := range expect {
		if !seen {
			t.Fatalf("missing drift %s in %+v", k, d)
		}
	}
	if e.rep.evs[len(e.rep.evs)-1].Kind != contracts.EvEnvironmentDrift {
		t.Fatal("no environment.drift event")
	}
	// Unmanaged additions survive; nothing was rewritten by Drift.
	if !strings.Contains(read(t, sp), "/opt/synth/new-hook") {
		t.Fatal("unmanaged hook lost")
	}

	// Changing the managed key now conflicts with the local edit.
	p := e.globalProfile()
	p.Settings["claude.cleanupPeriodDays"] = 14
	e.saveGlobal(p)
	plan, err := e.m.Plan(ctx, contracts.EnvPlanParams{Scope: "global"})
	if err != nil {
		t.Fatal(err)
	}
	var keyConflict, skillConflict bool
	for _, c := range plan.Conflicts {
		if c.Kind == "conflict" && c.Key == "cleanupPeriodDays" && c.Expected == "30" && c.Actual == "7" {
			keyConflict = true
		}
		if c.Kind == "conflict" && c.Key == "skill:synth-review" {
			skillConflict = true
		}
	}
	if !keyConflict || !skillConflict {
		t.Fatalf("conflicts: %+v", plan.Conflicts)
	}
	before := read(t, sp)
	_, err = e.m.Apply(ctx, contracts.EnvApplyParams{PlanID: plan.PlanID, ResultDigest: plan.ResultDigest})
	if !contracts.IsCode(err, contracts.ErrConflict) {
		t.Fatalf("apply with conflicts: %v", err)
	}
	if read(t, sp) != before {
		t.Fatal("conflicting apply modified user file")
	}
}

func TestApplyRequiresReviewedDigestAndUnchangedFiles(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.saveGlobal(e.globalProfile())
	plan, err := e.m.Plan(ctx, contracts.EnvPlanParams{Scope: "global"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.Apply(ctx, contracts.EnvApplyParams{PlanID: plan.PlanID, ResultDigest: "sha256:wrong"}); !contracts.IsCode(err, contracts.ErrConflict) {
		t.Fatalf("wrong digest: %v", err)
	}
	sp := filepath.Join(e.home, ".claude/settings.json")
	write(t, sp, strings.Replace(read(t, sp), `"model": "default"`, `"model": "sonnet"`, 1))
	if _, err := e.m.Apply(ctx, contracts.EnvApplyParams{PlanID: plan.PlanID, ResultDigest: plan.ResultDigest}); !contracts.IsCode(err, contracts.ErrConflict) {
		t.Fatalf("changed since plan: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.home, ".claude/skills/synth-review")); !os.IsNotExist(err) {
		t.Fatal("refused apply wrote files")
	}
	if e.m.CurrentGenerationID(ctx) != "" {
		t.Fatal("refused apply set a current generation")
	}
}

// Partial failure: the second config-home write fails; earlier writes are
// undone and no generation becomes current.
func TestPartialFailureRollsBack(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.saveGlobal(e.globalProfile())
	_, gen1 := e.planApply("global")
	settings1 := read(t, filepath.Join(e.home, ".claude/settings.json"))
	skill1 := read(t, filepath.Join(e.home, ".claude/skills/synth-review/SKILL.md"))

	write(t, filepath.Join(e.skillSrc, "SKILL.md"), "---\nname: synth-review\n---\nv2\n")
	p := e.globalProfile()
	p.Settings["claude.cleanupPeriodDays"] = 45
	e.saveGlobal(p)
	plan, err := e.m.Plan(ctx, contracts.EnvPlanParams{Scope: "global"})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	e.m.failBefore = func(op fileOp) error {
		calls++
		if op.Kind == "json_write" {
			return errors.New("injected disk failure")
		}
		return nil
	}
	_, err = e.m.Apply(ctx, contracts.EnvApplyParams{PlanID: plan.PlanID, ResultDigest: plan.ResultDigest})
	e.m.failBefore = nil
	if err == nil || !strings.Contains(err.Error(), "rolled back") || calls < 3 {
		t.Fatalf("expected rolled-back failure after earlier ops, got %v (calls %d)", err, calls)
	}
	if got := read(t, filepath.Join(e.home, ".claude/skills/synth-review/SKILL.md")); got != skill1 {
		t.Fatalf("skill not restored: %q", got)
	}
	if read(t, filepath.Join(e.home, ".claude/settings.json")) != settings1 {
		t.Fatal("settings changed")
	}
	if e.m.CurrentGenerationID(ctx) != gen1.GenerationID {
		t.Fatal("current generation moved after failed apply")
	}
	ents, _ := os.ReadDir(filepath.Join(e.home, ".claude/skills"))
	for _, en := range ents {
		if strings.HasPrefix(en.Name(), ".pyre-") {
			t.Fatalf("leftover temp dir %s", en.Name())
		}
	}
	if d, _ := e.m.Drift(ctx); len(d) != 0 {
		t.Fatalf("drift after rolled-back apply: %+v", d)
	}
	// The same plan applies cleanly once the fault is gone.
	if _, err := e.m.Apply(ctx, contracts.EnvApplyParams{PlanID: plan.PlanID, ResultDigest: plan.ResultDigest}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(read(t, filepath.Join(e.home, ".claude/skills/synth-review/SKILL.md")), "v2") {
		t.Fatal("v2 not applied")
	}
}

func TestRollbackToPreviousGeneration(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.saveGlobal(e.globalProfile())
	_, gen1 := e.planApply("global")
	write(t, filepath.Join(e.skillSrc, "SKILL.md"), "---\nname: synth-review\n---\nv2\n")
	p := e.globalProfile()
	p.Settings["claude.cleanupPeriodDays"] = 45
	p.Skills = append(p.Skills, SkillSpec{Name: "synth-extra", Harness: contracts.HarnessClaude, SourceDir: e.skillSrc})
	e.saveGlobal(p)
	_, gen2 := e.planApply("global")
	if gen2.PreviousID != gen1.GenerationID {
		t.Fatalf("previous id: %s", gen2.PreviousID)
	}
	if _, err := os.Stat(filepath.Join(e.home, ".claude/skills/synth-extra")); err != nil {
		t.Fatal(err)
	}
	g, err := e.m.Rollback(ctx, contracts.EnvRollbackParams{GenerationID: gen1.GenerationID})
	if err != nil {
		t.Fatal(err)
	}
	if g.GenerationID != gen1.GenerationID || e.m.CurrentGenerationID(ctx) != gen1.GenerationID {
		t.Fatal("rollback did not make gen1 current")
	}
	if !strings.Contains(read(t, filepath.Join(e.home, ".claude/skills/synth-review/SKILL.md")), "v1") {
		t.Fatal("skill not rolled back")
	}
	if _, err := os.Stat(filepath.Join(e.home, ".claude/skills/synth-extra")); !os.IsNotExist(err) {
		t.Fatal("skill added by gen2 not removed")
	}
	if !strings.Contains(read(t, filepath.Join(e.home, ".claude/settings.json")), `"cleanupPeriodDays": 30`) {
		t.Fatal("managed key not rolled back")
	}
	if d, _ := e.m.Drift(ctx); len(d) != 0 {
		t.Fatalf("drift after rollback: %+v", d)
	}
	// gen2's overlay files survive for runs still pinned to it.
	if _, err := os.Stat(filepath.Join(e.state, "env/generations", gen2.GenerationID, "claude/settings.json")); err != nil {
		t.Fatal("gen2 overlay removed")
	}
}

// Restart-required: running sessions keep their generation; a plan reports
// them and flags changed values.
func TestRestartRequiredAndPinnedRuns(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.saveGlobal(e.globalProfile())
	_, gen1 := e.planApply("global")
	sgen, lo, err := e.m.Resolve(ctx, Scope{Harness: contracts.HarnessClaude, SessionID: "ses_synth-2", Overrides: map[string]string{"claude.model": "opus"}})
	if err != nil {
		t.Fatal(err)
	}
	pinned := flagValue(lo.ExtraArgs, "--settings")
	pinnedContent := read(t, pinned)
	if !strings.Contains(pinnedContent, `"model": "opus"`) {
		t.Fatalf("session overlay: %s", pinnedContent)
	}
	e.runs.runs = []RunRef{
		{RunID: "run_synth-1", SessionID: "ses_synth-1", Harness: contracts.HarnessClaude, GenerationID: gen1.GenerationID},
		{RunID: "run_synth-2", SessionID: "ses_synth-2", Harness: contracts.HarnessClaude, GenerationID: sgen.GenerationID},
		{RunID: "run_synth-3", SessionID: "ses_synth-3", ProjectID: "prj_synth-other", Harness: contracts.HarnessCodex, GenerationID: gen1.GenerationID},
		// Shell runs take no environment generation: never restart-required.
		{RunID: "run_synth-4", SessionID: "ses_synth-4", Harness: contracts.HarnessShell},
	}
	p := e.globalProfile()
	p.Settings["claude.permissions.defaultMode"] = "plan"
	e.saveGlobal(p)
	plan, err := e.m.Plan(ctx, contracts.EnvPlanParams{Scope: "global"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plan.RestartRequired, []string{"ses_synth-1", "ses_synth-2", "ses_synth-3"}) {
		t.Fatalf("restart required: %v", plan.RestartRequired)
	}
	pg, _ := e.m.Preview(ctx, contracts.EnvPlanParams{Scope: "global"})
	if v, _ := findValue(pg, "claude.permissions.defaultMode"); !v.RestartRequired {
		t.Fatalf("changed value not flagged: %+v", v)
	}
	if v, _ := findValue(pg, "codex.model_reasoning_effort"); v.RestartRequired {
		t.Fatalf("unchanged value flagged: %+v", v)
	}
	pp, err := e.m.Plan(ctx, contracts.EnvPlanParams{Scope: "project:prj_synth-other"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(pp.RestartRequired, []string{"ses_synth-3"}) {
		t.Fatalf("project-scoped restart: %v", pp.RestartRequired)
	}
	e.planApply("global")
	// The running session's pinned overlay is immutable.
	if read(t, pinned) != pinnedContent {
		t.Fatal("pinned session overlay changed after a new apply")
	}
	if removed, _ := e.m.Prune(ctx, 1); len(removed) != 0 {
		for _, id := range removed {
			if id == sgen.GenerationID || id == gen1.GenerationID {
				t.Fatalf("pruned a generation referenced by a live run: %v", removed)
			}
		}
	}
	if _, err := os.Stat(pinned); err != nil {
		t.Fatal("pinned overlay pruned")
	}
}

func TestSecretsRedactedAndRejected(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	bad := e.globalProfile()
	bad.Settings["claude.env.GITHUB_TOKEN"] = "ghp_synthetic000000000000000000"
	if err := e.m.Profiles().Save("", bad); !contracts.IsCode(err, contracts.ErrInvalid) {
		t.Fatalf("secret in profile accepted: %v", err)
	}
	bad2 := e.globalProfile()
	bad2.MCPServers[1].Env = map[string]string{"API_KEY": "synthetic"}
	if err := e.m.Profiles().Save("", bad2); !contracts.IsCode(err, contracts.ErrInvalid) {
		t.Fatalf("secret MCP env accepted: %v", err)
	}
	e.saveGlobal(e.globalProfile())
	if _, err := e.m.Preview(ctx, contracts.EnvPlanParams{Scope: "session:ses_synth-1", Overrides: map[string]string{"claude.model": "sk-ant-synthetic-abcdefghijkl"}}); !contracts.IsCode(err, contracts.ErrInvalid) {
		t.Fatalf("secret override accepted: %v", err)
	}
	plan, gen := e.planApply("global")
	v, ok := findValue(gen, "claude.env.ANTHROPIC_API_KEY")
	if !ok || v.Value != Redacted {
		t.Fatalf("adopted secret not redacted: %+v", v)
	}
	for _, blob := range []string{mustJSON(gen), mustJSON(plan)} {
		if strings.Contains(blob, "sk-ant-synthetic") || strings.Contains(blob, "synthetic-secret-value") {
			t.Fatal("secret leaked into generation/plan")
		}
	}
	// No secret anywhere in Pyre's env state (records, overlays, plans).
	// Backups are faithful private copies of user files (0600 in 0700 dirs).
	backups := filepath.Join(e.state, "env/backups")
	_ = filepath.Walk(e.state, func(p string, info os.FileInfo, err error) error {
		if err == nil && strings.HasPrefix(p, backups) {
			if info.IsDir() && info.Mode().Perm() != 0o700 || info.Mode().IsRegular() && info.Mode().Perm() != 0o600 {
				t.Errorf("backup %s has mode %v", p, info.Mode().Perm())
			}
			return nil
		}
		if err == nil && info.Mode().IsRegular() {
			if b, _ := os.ReadFile(p); strings.Contains(string(b), "sk-ant-synthetic") || strings.Contains(string(b), "synthetic-secret-value") {
				t.Errorf("secret persisted in %s", p)
			}
		}
		return nil
	})
	// Isolated Claude profile: user MCP servers are composed per run, except
	// ones carrying secret env values.
	prof := filepath.Join(e.state, "profiles/acct_synth/claude")
	write(t, filepath.Join(prof, ".claude.json"), `{"oauthAccount":{}}`)
	_, lo, err := e.m.Resolve(ctx, Scope{Harness: contracts.HarnessClaude, ConfigHome: prof})
	if err != nil {
		t.Fatal(err)
	}
	mcp := read(t, flagValue(lo.ExtraArgs, "--mcp-config"))
	if !strings.Contains(mcp, `"pyre"`) || strings.Contains(mcp, "synthetic-secret-value") || strings.Contains(mcp, "user-tool") {
		t.Fatalf("isolated profile MCP composition: %s", mcp)
	}
}

func TestCodexUnsupportedKeyIsConflict(t *testing.T) {
	e := newEnv(t)
	p := e.globalProfile()
	p.Settings[`codex.profiles."a.b".model`] = "x"
	e.saveGlobal(p)
	plan, err := e.m.Plan(context.Background(), contracts.EnvPlanParams{Scope: "global"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range plan.Conflicts {
		if c.Kind == "conflict" && strings.Contains(c.Key, `"a.b"`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected conflict, got %+v", plan.Conflicts)
	}
}

func TestUserOwnedSameNameSkillConflicts(t *testing.T) {
	e := newEnv(t)
	write(t, filepath.Join(e.home, ".claude/skills/synth-review/SKILL.md"), "user's own different skill\n")
	e.saveGlobal(e.globalProfile())
	plan, err := e.m.Plan(context.Background(), contracts.EnvPlanParams{Scope: "global"})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Conflicts) != 1 || plan.Conflicts[0].Key != "skill:synth-review" {
		t.Fatalf("conflicts: %+v", plan.Conflicts)
	}
	_, err = e.m.Apply(context.Background(), contracts.EnvApplyParams{PlanID: plan.PlanID, ResultDigest: plan.ResultDigest})
	if !contracts.IsCode(err, contracts.ErrConflict) {
		t.Fatal(err)
	}
	if read(t, filepath.Join(e.home, ".claude/skills/synth-review/SKILL.md")) != "user's own different skill\n" {
		t.Fatal("user skill overwritten")
	}
}

func TestProjectSkillUsesPinnedPluginOverlay(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if err := e.m.Profiles().Save(synthProject, Profile{Name: "default", Skills: []SkillSpec{{Name: "proj-skill", Harness: contracts.HarnessClaude, SourceDir: e.skillSrc}}}); err != nil {
		t.Fatal(err)
	}
	gen, lo, err := e.m.Resolve(ctx, Scope{Harness: contracts.HarnessClaude, ProjectID: synthProject})
	if err != nil {
		t.Fatal(err)
	}
	dir := flagValue(lo.ExtraArgs, "--plugin-dir")
	if !strings.HasPrefix(dir, filepath.Join(e.state, "env/generations", gen.GenerationID)) {
		t.Fatalf("plugin dir: %q", dir)
	}
	if _, err := os.Stat(filepath.Join(dir, "skills/proj-skill/SKILL.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".claude-plugin/plugin.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(e.home, ".claude/skills/proj-skill")); !os.IsNotExist(err) {
		t.Fatal("project skill written into the user's home")
	}
}

func TestHandlers(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.saveGlobal(e.globalProfile())
	h := e.m.Handlers()
	for _, name := range []string{contracts.ActionEnvPlan, contracts.ActionEnvApply, contracts.ActionEnvDrift, contracts.ActionEnvRollback, contracts.ActionEnvResolve} {
		if h[name] == nil {
			t.Fatalf("missing handler %s", name)
		}
	}
	out, err := h[contracts.ActionEnvPlan](ctx, json.RawMessage(`{"scope":"global"}`))
	if err != nil {
		t.Fatal(err)
	}
	plan := out.(contracts.ChangePlan)
	if _, err := h[contracts.ActionEnvApply](ctx, json.RawMessage(`{"plan_id":"`+plan.PlanID+`","result_digest":"sha256:x"}`)); !contracts.IsCode(err, contracts.ErrConflict) {
		t.Fatalf("bad digest: %v", err)
	}
	out, err = h[contracts.ActionEnvApply](ctx, json.RawMessage(`{"plan_id":"`+plan.PlanID+`","result_digest":"`+string(plan.ResultDigest)+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := out.(contracts.EnvironmentGeneration); !ok {
		t.Fatalf("apply result %T", out)
	}
	if _, err := h[contracts.ActionEnvPlan](ctx, json.RawMessage(`{"scope":"global","bogus":1}`)); !contracts.IsCode(err, contracts.ErrInvalid) {
		t.Fatalf("unknown field: %v", err)
	}
	if _, err := h[contracts.ActionEnvPlan](ctx, json.RawMessage(`{"scope":"global","overrides":{"claude.model":"x"}}`)); !contracts.IsCode(err, contracts.ErrInvalid) {
		t.Fatalf("overrides outside session: %v", err)
	}
	d, err := h[contracts.ActionEnvDrift](ctx, nil)
	if err != nil || len(d.([]contracts.DriftItem)) != 0 {
		t.Fatalf("drift: %v %v", err, d)
	}
	if _, err := h[contracts.ActionEnvRollback](ctx, json.RawMessage(`{"generation_id":"env_nope"}`)); !contracts.IsCode(err, contracts.ErrNotFound) {
		t.Fatalf("rollback unknown: %v", err)
	}
}

func TestKeyParsing(t *testing.T) {
	segs, err := SplitKey(`claude.enabledPlugins."a.b@m".x`)
	if err != nil || !reflect.DeepEqual(segs, []string{"claude", "enabledPlugins", "a.b@m", "x"}) {
		t.Fatalf("%v %v", segs, err)
	}
	if JoinKey(segs...) != `claude.enabledPlugins."a.b@m".x` {
		t.Fatal(JoinKey(segs...))
	}
	for _, bad := range []string{"", "a..b", `a."b`, "a.", `a."b"c`} {
		if _, err := SplitKey(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
	if v := ParseValue("true"); v != true {
		t.Fatal(v)
	}
	if v := ParseValue("opus"); v != "opus" {
		t.Fatal(v)
	}
	if v, _ := tomlValue(map[string]any{"a b": "x\n", "c": []any{int64(1)}}); v != `{"a b" = "x\n", c = [1]}` {
		t.Fatal(v)
	}
	if !SensitiveValue("ghp_abcdefghijklmnopqrstuvwxyz") || SensitiveValue("default") || !SensitiveKey("claude.env.MY_API_KEY") {
		t.Fatal("secret detection")
	}
}
