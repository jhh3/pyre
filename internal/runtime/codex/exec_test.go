//go:build !windows

package codex

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

func newCodexExecTest(t *testing.T, homes ...string) (*ExecAdapter, *Adapter) {
	t.Helper()
	d, err := os.MkdirTemp("", "px")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	a, err := New(Options{NodeID: "node_t", TmuxSocket: filepath.Join(d, "t.sock"), StateRoot: filepath.Join(d, "state"), TranscriptHomes: homes})
	if err != nil {
		t.Fatal(err)
	}
	return NewExec(a), a
}

// writeExecRollout writes a rollout of thread for cwd under home's day
// folder of at, as codex exec does.
func writeExecRollout(t *testing.T, home, cwd, thread string, at time.Time) string {
	t.Helper()
	day := filepath.Join(home, "sessions", at.Format("2006"), at.Format("01"), at.Format("02"))
	if err := os.MkdirAll(day, 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"timestamp":"` + at.UTC().Format(time.RFC3339Nano) + `","type":"session_meta","payload":{"id":"` + thread + `","cwd":"` + cwd + `","originator":"codex_exec"}}` + "\n" +
		`{"timestamp":"2026-10-21T04:48:51.734Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Review the diff"}]}}` + "\n" +
		`{"timestamp":"2026-10-21T04:48:53.164Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"LGTM (synthetic)"}]}}` + "\n"
	p := filepath.Join(day, "rollout-"+at.Format("2006-01-02T15-04-05")+"-"+thread+".jsonl")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// The headless Codex run: pending -> claimed -> running -> exited on the
// profile's CODEX_HOME; without a thread id from the output the run's
// rollout (the only new one for its cwd) names it, and read/search use the
// rollout file (no app-server).
func TestCodexExecAdapterLifecycle(t *testing.T) {
	x, _ := newCodexExecTest(t)
	home, cwd := t.TempDir(), t.TempDir()
	ctx := context.Background()
	h, err := x.Launch(ctx, contracts.ResolvedLaunch{RunID: "run_c1", SessionID: "ses_c", Cwd: cwd, StateDir: filepath.Join(t.TempDir(), "r1"), ProfileDir: home})
	if err != nil {
		t.Fatal(err)
	}
	if h.Adapter != ExecAdapterName || h.Harness != contracts.HarnessCodex {
		t.Fatalf("handle %+v", h)
	}
	if obs, _ := x.Observe(ctx, h, ""); obs.State != contracts.RunStarting {
		t.Fatalf("pending = %+v", obs)
	}
	if err := x.Claimed(h, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	child := exec.Command("sleep", "30")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer child.Process.Kill()
	if err := x.Started(h, child.Process.Pid, false); err != nil {
		t.Fatal(err)
	}
	if obs, _ := x.Observe(ctx, h, ""); obs.State != contracts.RunRunning {
		t.Fatalf("running = %+v", obs)
	}
	thread := "0199a213-81c0-7800-8aa1-bbab2a035a53"
	writeExecRollout(t, home, filepath.Join(t.TempDir(), "other"), "0199a213-0000-7000-8000-000000000001", time.Now()) // another cwd
	// The caller's own older thread in the same cwd, appended to just now
	// (its rollout sits in today's folder, mtime now): never the run's.
	writeExecRollout(t, home, cwd, "0199a213-0000-7000-8000-0000000000aa", time.Now().Add(-time.Hour))
	path := writeExecRollout(t, home, cwd, thread, time.Now())
	code := 0
	if err := x.Exited(h, &code, "", ""); err != nil {
		t.Fatal(err)
	}
	obs, _ := x.Observe(ctx, h, "")
	if obs.State != contracts.RunExited || obs.ExitCode == nil || *obs.ExitCode != 0 || obs.NativeRef == nil ||
		obs.NativeRef.Kind != "codex_thread" || obs.NativeRef.ID != thread {
		t.Fatalf("exited = %+v", obs)
	}
	if _, paths := x.TranscriptFiles(h); len(paths) != 1 || paths[0] != path {
		t.Fatalf("transcript files %q (want %s)", paths, path)
	}
	resp, err := x.ReadTranscript(ctx, h, contracts.ReadSessionRequest{RunID: h.RunID, Tail: true, Limit: 10})
	if err != nil || len(resp.Entries) != 2 || resp.Entries[1].Text != "LGTM (synthetic)" || resp.Screen != "" {
		t.Fatalf("read %+v %v", resp, err)
	}

	// The thread id the CLI saw in codex exec --json output wins.
	h2, _ := x.Launch(ctx, contracts.ResolvedLaunch{RunID: "run_c2", Cwd: cwd, StateDir: filepath.Join(t.TempDir(), "r2"), ProfileDir: home})
	_ = x.Claimed(h2, os.Getpid())
	seen := "0199a213-2222-7000-8000-000000000002"
	if err := x.Exited(h2, &code, "", seen); err != nil {
		t.Fatal(err)
	}
	if obs, _ := x.Observe(ctx, h2, ""); obs.NativeRef == nil || obs.NativeRef.ID != seen {
		t.Fatalf("reported thread = %+v", obs)
	}

	// A resumed thread (exec resume ID) is the run's thread, whatever new
	// rollout the cwd has; nothing is searched.
	old := "0199a213-3333-7000-8000-000000000003"
	h4, _ := x.Launch(ctx, contracts.ResolvedLaunch{RunID: "run_c4", Cwd: cwd, StateDir: filepath.Join(t.TempDir(), "r4"), ProfileDir: home,
		Resume: &contracts.NativeRef{Kind: "codex_thread", ID: old}})
	if h4.NativeRef == nil || h4.NativeRef.ID != old {
		t.Fatalf("resume handle %+v", h4)
	}
	_ = x.Claimed(h4, os.Getpid())
	writeExecRollout(t, home, cwd, "0199a213-4444-7000-8000-000000000004", time.Now())
	if err := x.Exited(h4, &code, "", ""); err != nil {
		t.Fatal(err)
	}
	if obs, _ := x.Observe(ctx, h4, ""); obs.NativeRef == nil || obs.NativeRef.ID != old {
		t.Fatalf("resumed thread = %+v", obs)
	}
	// Two new threads for the cwd: none is recorded.
	h5, _ := x.Launch(ctx, contracts.ResolvedLaunch{RunID: "run_c5", Cwd: cwd, StateDir: filepath.Join(t.TempDir(), "r5"), ProfileDir: home})
	_ = x.Claimed(h5, os.Getpid())
	writeExecRollout(t, home, cwd, "0199a213-5555-7000-8000-000000000005", time.Now())
	writeExecRollout(t, home, cwd, "0199a213-6666-7000-8000-000000000006", time.Now())
	_ = x.Exited(h5, &code, "", "")
	if obs, _ := x.Observe(ctx, h5, ""); obs.NativeRef != nil {
		t.Fatalf("ambiguous new threads recorded %+v", obs.NativeRef)
	}

	// The machine's own login: CODEX_HOME from the launch env.
	h3, _ := x.Launch(ctx, contracts.ResolvedLaunch{RunID: "run_c3", Cwd: cwd, StateDir: filepath.Join(t.TempDir(), "r3"), Env: map[string]string{"CODEX_HOME": home}})
	_ = x.Claimed(h3, os.Getpid())
	if _, err := x.Stop(ctx, h3); err != nil {
		t.Fatal(err)
	}
	if obs, _ := x.Observe(ctx, h3, ""); obs.State != contracts.RunExited || !strings.Contains(obs.StateReason, "stopped before") {
		t.Fatalf("stopped = %+v", obs)
	}
}

// Which account a headless codex exec resume must run on: any when the
// profiles share the sessions store, the profile's account when only it
// has the rollout, the machine's own login when only ~/.codex has it.
func TestCodexResumeOwner(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	shared := filepath.Join(root, "state", "shared", "codex")
	profiles := filepath.Join(root, "state", "profiles")
	pa := filepath.Join(profiles, "acct_synthA", "codex")
	pb := filepath.Join(profiles, "acct_synthB", "codex")
	_ = os.MkdirAll(filepath.Join(shared, "sessions"), 0o700)
	_ = os.MkdirAll(pa, 0o700)
	if err := os.Symlink(filepath.Join(shared, "sessions"), filepath.Join(pa, "sessions")); err != nil {
		t.Fatal(err)
	}
	_ = os.MkdirAll(filepath.Join(pb, "sessions"), 0o700) // own store
	_, a := newCodexExecTest(t, shared, filepath.Join(profiles, "*", "codex*"))
	cwd := t.TempDir()
	tShared, tOwn, tDef := "11111111-1111-7111-8111-111111111111", "22222222-2222-7222-8222-222222222222", "33333333-3333-7333-8333-333333333333"
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.Local)
	writeExecRollout(t, shared, cwd, tShared, at)
	writeExecRollout(t, pb, cwd, tOwn, at)
	writeExecRollout(t, filepath.Join(home, ".codex"), cwd, tDef, at)
	if shared, acct, def := a.ResumeOwner(cwd, tShared); !shared || acct != "" || def {
		t.Fatalf("shared: %v %q %v", shared, acct, def)
	}
	if shared, acct, def := a.ResumeOwner(cwd, tOwn); shared || acct != "acct_synthB" || def {
		t.Fatalf("own store: %v %q %v", shared, acct, def)
	}
	if shared, acct, def := a.ResumeOwner(cwd, tDef); shared || acct != "" || !def {
		t.Fatalf("own login: %v %q %v", shared, acct, def)
	}
	if shared, acct, def := a.ResumeOwner(cwd, "44444444-4444-7444-8444-444444444444"); shared || acct != "" || def {
		t.Fatal("missing thread found")
	}
	if shared, acct, def := a.ResumeOwner(cwd, "../*"); shared || acct != "" || def {
		t.Fatal("a glob pattern matched")
	}
}
