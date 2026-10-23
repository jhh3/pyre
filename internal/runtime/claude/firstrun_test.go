package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/tmux"
)

// Screens captured from real Claude Code 2.1.304 on server in an isolated tmux
// server with an empty CLAUDE_CONFIG_DIR (no credentials; the security
// notes behind a synthetic API key): the onboarding a Pyre profile shows
// when its .claude.json lacks hasCompletedOnboarding.
func readScreen(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestParseScreenOnboarding(t *testing.T) {
	si := parseScreen(readScreen(t, "screen-theme-picker-2.1.304.txt"), "")
	if si.HasInputBox || si.DialogKind != "onboarding" || si.Onboarding != "theme" || si.Dialog != "Claude first-run theme picker" {
		t.Fatalf("theme picker: %+v", si)
	}
	si = parseScreen(readScreen(t, "screen-security-notes-2.1.304.txt"), "")
	if si.HasInputBox || si.DialogKind != "onboarding" || si.Onboarding != "security" {
		t.Fatalf("security notes: %+v", si)
	}
	// The picker text in the scrollback above a live input box (after /theme
	// or a finished onboarding) is not a startup screen.
	idle := readScreen(t, "screen-theme-picker-2.1.304.txt") + "\n" + realIdle
	if si := parseScreen(idle, ""); !si.HasInputBox || si.DialogKind == "onboarding" {
		t.Fatalf("picker in scrollback: %+v", si)
	}
}

func TestEnsureFirstRunAnswers(t *testing.T) {
	read := func(t *testing.T, p string) map[string]json.RawMessage {
		t.Helper()
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]json.RawMessage{}
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("%s: %v\n%s", p, err, b)
		}
		return m
	}
	t.Run("projects only (win-home 2026-10-20)", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, ".claude.json")
		_ = os.WriteFile(p, []byte(`{"projects":{"C:/src":{"hasTrustDialogAccepted":true}}}`), 0o600)
		ensureFirstRunAnswers(dir)
		m := read(t, p)
		if string(m["hasCompletedOnboarding"]) != "true" || string(m["theme"]) != `"dark"` {
			t.Fatalf("%v", m)
		}
		if !strings.Contains(string(m["projects"]), "hasTrustDialogAccepted") {
			t.Fatalf("projects lost: %s", m["projects"])
		}
	})
	t.Run("missing file", func(t *testing.T) {
		dir := t.TempDir()
		ensureFirstRunAnswers(dir)
		if m := read(t, filepath.Join(dir, ".claude.json")); string(m["hasCompletedOnboarding"]) != "true" {
			t.Fatalf("%v", m)
		}
	})
	t.Run("existing values untouched", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, ".claude.json")
		orig := []byte(`{"hasCompletedOnboarding": false, "theme": "light-ansi", "x": 1}`)
		_ = os.WriteFile(p, orig, 0o600)
		ensureFirstRunAnswers(dir)
		if b, _ := os.ReadFile(p); string(b) != string(orig) {
			t.Fatalf("rewritten: %s", b)
		}
	})
	t.Run("unparseable left alone", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, ".claude.json")
		for _, orig := range []string{`{"projects": {`, `[1,2]`, `null`} {
			_ = os.WriteFile(p, []byte(orig), 0o600)
			ensureFirstRunAnswers(dir)
			if b, _ := os.ReadFile(p); string(b) != orig {
				t.Fatalf("%q rewritten: %s", orig, b)
			}
		}
	})
	t.Run("byte-order mark", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, ".claude.json")
		_ = os.WriteFile(p, append([]byte("\xef\xbb\xbf"), `{"projects":{}}`...), 0o600)
		ensureFirstRunAnswers(dir)
		if m := read(t, p); string(m["hasCompletedOnboarding"]) != "true" {
			t.Fatalf("%v", m)
		}
	})
}

// automations collects OnAutomation reports.
type automations struct {
	mu  sync.Mutex
	got []contracts.NodeAutomation
}

func (r *automations) add(a contracts.NodeAutomation) {
	r.mu.Lock()
	r.got = append(r.got, a)
	r.mu.Unlock()
}

func (r *automations) list() []contracts.NodeAutomation {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]contracts.NodeAutomation(nil), r.got...)
}

// promptRan reports a transcript user record holding prompt under profile.
func promptRan(profile, prompt string) bool {
	m, _ := filepath.Glob(filepath.Join(profile, "projects", "*", "*.jsonl"))
	for _, p := range m {
		if b, err := os.ReadFile(p); err == nil && strings.Contains(string(b), prompt) {
			return true
		}
	}
	return false
}

// The win-home failure: the profile's config lacked hasCompletedOnboarding
// (and the launch seed did not land), so Claude opened its onboarding and
// the run sat on the theme picker. The adapter answers the theme picker and
// the security notes, the session starts and the initial prompt runs.
func TestFakeOnboardingAnswered(t *testing.T) {
	t.Parallel()
	var rec automations
	f := newFakeEnv(t, func(o *Options) { o.StartupWait = 2 * time.Second; o.OnAutomation = rec.add })
	profile := filepath.Join(f.dir, "cfg")
	h := f.launchIn("run_onb1", profile, map[string]string{"FAKE_THEME_PICKER": "always"}, nil, "first-run prompt marker")
	// Launch drives the screens for StartupWait only; after that the node's
	// observation loop does (each Observe may press). On a slow runner the
	// second screen can be drawn after Launch returned, so observe as the
	// node would.
	end := time.Now().Add(30 * time.Second)
	for !promptRan(profile, "first-run prompt marker") {
		obs, _ := f.a.Observe(t.Context(), h, "")
		if time.Now().After(end) {
			t.Fatalf("initial prompt never ran after the onboarding screens: %+v", obs)
		}
		time.Sleep(200 * time.Millisecond)
	}
	f.waitState(f.a, h, contracts.RunWaitingInput, 10*time.Second) // the prompt's turn ended
	end = time.Now().Add(5 * time.Second)
	for len(rec.list()) == 0 && time.Now().Before(end) {
		time.Sleep(50 * time.Millisecond)
	}
	got := rec.list()
	if len(got) != 1 || got[0].Action != contracts.AutomationOnboardingAnswered || got[0].RunID != "run_onb1" ||
		!strings.Contains(got[0].Detail, "theme picker / security notes") {
		t.Fatalf("automation reports: %+v", got)
	}
}

// With the seed in place Claude never shows the onboarding: a profile whose
// .claude.json holds only "projects" gets hasCompletedOnboarding at launch.
func TestFakeOnboardingSeeded(t *testing.T) {
	t.Parallel()
	var rec automations
	f := newFakeEnv(t, func(o *Options) { o.OnAutomation = rec.add })
	profile := filepath.Join(f.dir, "cfg")
	_ = os.MkdirAll(profile, 0o700)
	_ = os.WriteFile(filepath.Join(profile, ".claude.json"), []byte(`{"projects":{}}`), 0o600)
	h := f.launchIn("run_onb2", profile, map[string]string{"FAKE_THEME_PICKER": "gate"}, nil, "")
	f.waitObs(f.a, h, 15*time.Second, func(o contracts.Observation) bool {
		return o.State == contracts.RunWaitingInput && strings.Contains(o.StateReason, "session started")
	})
	if got := rec.list(); len(got) != 0 {
		t.Fatalf("onboarding shown despite the seed: %+v", got)
	}
}

// A blocking screen with no input box and no hook event at all becomes
// "needs human" once FirstRunScreenWait has passed when it carries the
// onboarding banner; otherwise the reason says Claude has not started,
// without claiming a first-run screen.
func TestFakeFirstRunUnknownScreen(t *testing.T) {
	t.Parallel()
	f := newFakeEnv(t, func(o *Options) { o.StartupWait = time.Second; o.FirstRunScreenWait = 2 * time.Second })
	h := f.launch("run_onb3", map[string]string{"FAKE_FIRSTRUN_UNKNOWN": "welcome"}, nil)
	if obs, _ := f.a.Observe(t.Context(), h, ""); obs.State == contracts.RunWaitingApproval {
		t.Fatalf("needs human before FirstRunScreenWait: %+v", obs)
	}
	obs := f.waitObs(f.a, h, 10*time.Second, func(o contracts.Observation) bool { return o.StateReason == firstRunScreenReason })
	if obs.State != contracts.RunWaitingApproval {
		t.Fatalf("%+v", obs)
	}

	g := newFakeEnv(t, func(o *Options) { o.StartupWait = time.Second; o.FirstRunScreenWait = 2 * time.Second })
	h2 := g.launch("run_onb4", map[string]string{"FAKE_FIRSTRUN_UNKNOWN": "blank"}, nil)
	obs = g.waitObs(g.a, h2, 10*time.Second, func(o contracts.Observation) bool {
		return o.StateReason == "Claude has not started (no SessionStart after 2 s)"
	})
	if obs.State == contracts.RunWaitingApproval {
		t.Fatalf("no first-run screen, yet needs human: %+v", obs)
	}
}

// screenShows reports whether the run's pane currently shows text.
func screenShows(t *testing.T, f *fakeEnv, h contracts.RunHandle, text string) bool {
	t.Helper()
	s, _ := f.a.srv.CapturePane(t.Context(), h.TmuxTarget, tmux.CaptureOptions{})
	return strings.Contains(s, text)
}

// Review regression (PR #33): with a slow redraw, repeated Enters on a
// still-shown onboarding screen landed on the next one. Onboarding followed
// by the workspace-trust dialog of a folder the owner never trusted, with
// "Yes, I trust this folder" preselected: the trust stays unanswered.
func TestFakeOnboardingSlowRedrawLeavesTrust(t *testing.T) {
	t.Parallel()
	var rec automations
	f := newFakeEnv(t, func(o *Options) {
		o.StartupWait = 2 * time.Second
		o.OnAutomation = rec.add
		o.TrustWorkspace = func(string) bool { return false }
	})
	h := f.launch("run_onb5", map[string]string{"FAKE_THEME_PICKER": "always", "FAKE_ONBOARDING_REDRAW_MS": "1500",
		"FAKE_AFTER_ONBOARDING": "trust", "FAKE_TRUST_YES": "1"}, nil)
	f.waitObs(f.a, h, 20*time.Second, func(o contracts.Observation) bool {
		return strings.Contains(o.StateReason, "needs human: workspace trust")
	})
	// Keep observing (each Observe may press keys) well past the repress gap.
	end := time.Now().Add(2*onboardingRepress + time.Second)
	for time.Now().Before(end) {
		obs, _ := f.a.Observe(t.Context(), h, "")
		if obs.State != contracts.RunWaitingApproval || !strings.Contains(obs.StateReason, "workspace trust") {
			t.Fatalf("trust dialog answered or left: %+v", obs)
		}
		time.Sleep(200 * time.Millisecond)
	}
	for _, a := range rec.list() {
		if a.Action == contracts.AutomationTrustAccepted {
			t.Fatalf("untrusted folder trusted: %+v", rec.list())
		}
	}
}

// The login step after the picker (a profile without a login) is never
// answered: it is reported as needs human.
func TestFakeOnboardingLeavesLogin(t *testing.T) {
	t.Parallel()
	f := newFakeEnv(t, func(o *Options) { o.StartupWait = 2 * time.Second })
	h := f.launch("run_onb6", map[string]string{"FAKE_THEME_PICKER": "always", "FAKE_AFTER_ONBOARDING": "login"}, nil)
	f.waitObs(f.a, h, 15*time.Second, func(o contracts.Observation) bool {
		return strings.Contains(o.StateReason, "needs human: login required")
	})
	end := time.Now().Add(onboardingRepress + time.Second)
	for time.Now().Before(end) {
		obs, _ := f.a.Observe(t.Context(), h, "")
		if !strings.Contains(obs.StateReason, "needs human: login required") {
			t.Fatalf("%+v", obs)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !screenShows(t, f, h, "Select login method") || screenShows(t, f, h, "Opening browser") {
		t.Fatal("login method chosen by the adapter")
	}
}

func TestWriteFileAtomicUniqueTemp(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, ".claude.json")
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := writeFileAtomic(p, []byte(`{"projects":{},"hasCompletedOnboarding":true}`)); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if b, _ := os.ReadFile(p); string(b) != `{"projects":{},"hasCompletedOnboarding":true}` {
		t.Fatalf("%q", b)
	}
	if m, _ := filepath.Glob(filepath.Join(dir, ".*.tmp*")); len(m) != 0 {
		t.Fatalf("temp files left: %v", m)
	}
}

func TestEnsureFirstRunAnswersSymlink(t *testing.T) {
	dir, other := t.TempDir(), t.TempDir()
	target := filepath.Join(other, "owner.json")
	_ = os.WriteFile(target, []byte(`{"projects":{}}`), 0o600)
	p := filepath.Join(dir, ".claude.json")
	if err := os.Symlink(target, p); err != nil {
		t.Skip("no symlinks:", err)
	}
	ensureFirstRunAnswers(dir)
	if fi, err := os.Lstat(p); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("link replaced: %v %v", fi, err)
	}
	if b, _ := os.ReadFile(target); string(b) != `{"projects":{}}` {
		t.Fatalf("target written: %s", b)
	}
}
